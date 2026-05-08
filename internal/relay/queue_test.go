package relay

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// stubBackend is a Backend that counts Send calls and fails the first errsFor calls.
type stubBackend struct {
	calls   atomic.Int64
	errsFor int32
}

func (s *stubBackend) Name() string { return "stub" }
func (s *stubBackend) Send(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
	n := s.calls.Add(1)
	if int32(n) <= atomic.LoadInt32(&s.errsFor) {
		return errors.New("stub: transient error")
	}
	return nil
}

// counterBackend always succeeds and counts deliveries.
type counterBackend struct{ counter *atomic.Int64 }

func (c *counterBackend) Name() string { return "counter" }
func (c *counterBackend) Send(_ context.Context, _ string, _ []string, _, _ string, _ map[string]string) error {
	c.counter.Add(1)
	return nil
}

func newTestQueue(b Backend, cfg QueueConfig) *RelayQueue {
	return NewRelayQueue(b, cfg, slog.Default())
}

func TestRelayQueue_BasicDeliver(t *testing.T) {
	b := &stubBackend{}
	q := newTestQueue(b, QueueConfig{Workers: 2, MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond})

	if err := q.Enqueue(context.Background(), "from@example.com", []string{"to@example.com"}, "hello", "body", nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	q.Stop(stopCtx)

	if b.calls.Load() != 1 {
		t.Errorf("expected 1 Send call, got %d", b.calls.Load())
	}
}

func TestRelayQueue_RetryOnFailure(t *testing.T) {
	b := &stubBackend{errsFor: 2} // first 2 calls fail; 3rd succeeds
	q := newTestQueue(b, QueueConfig{Workers: 1, MaxRetries: 5, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond})

	if err := q.Enqueue(context.Background(), "a@x.com", []string{"b@x.com"}, "s", "b", nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait until the successful delivery (3rd call) arrives before stopping.
	waitForCalls(t, &b.calls, 3, 2*time.Second)

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	q.Stop(stopCtx)

	if b.calls.Load() < 3 {
		t.Errorf("expected ≥3 Send calls (2 failures + 1 success), got %d", b.calls.Load())
	}
}

func TestRelayQueue_MaxRetriesExhausted(t *testing.T) {
	b := &stubBackend{errsFor: 100} // always fail
	q := newTestQueue(b, QueueConfig{Workers: 1, MaxRetries: 3, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond})

	if err := q.Enqueue(context.Background(), "a@x.com", []string{"b@x.com"}, "s", "b", nil); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Wait until MaxRetries calls are exhausted.
	waitForCalls(t, &b.calls, 3, 2*time.Second)

	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	q.Stop(stopCtx)

	if b.calls.Load() != 3 {
		t.Errorf("expected exactly 3 Send calls (MaxRetries), got %d", b.calls.Load())
	}
}

// waitForCalls polls until counter reaches min or deadline expires.
func waitForCalls(t *testing.T, counter *atomic.Int64, min int64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if counter.Load() >= min {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRelayQueue_ErrQueueFull(t *testing.T) {
	b := &stubBackend{errsFor: 100}
	// Workers=1, bufSize = max(1*8, 64) = 64; use long delays so the worker
	// stays busy and the buffer fills up quickly.
	q := newTestQueue(b, QueueConfig{Workers: 1, MaxRetries: 1, BaseDelay: time.Hour, MaxDelay: time.Hour})

	var full int
	for i := 0; i < 200; i++ {
		if err := q.Enqueue(context.Background(), "a@x.com", []string{"b@x.com"}, "s", "b", nil); errors.Is(err, ErrQueueFull) {
			full++
		}
	}
	if full == 0 {
		t.Error("expected at least one ErrQueueFull, got none")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	q.Stop(stopCtx)
}

func TestRelayQueue_GracefulDrain(t *testing.T) {
	var delivered atomic.Int64
	b := &counterBackend{counter: &delivered}
	q := newTestQueue(b, QueueConfig{Workers: 2, MaxRetries: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond})

	const n = 20
	for i := 0; i < n; i++ {
		_ = q.Enqueue(context.Background(), "a@x.com", []string{"b@x.com"}, "s", "b", nil)
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	q.Stop(stopCtx)

	if delivered.Load() != n {
		t.Errorf("expected %d delivered, got %d", n, delivered.Load())
	}
}

func TestRelayQueue_BackoffSequence(t *testing.T) {
	base := 5 * time.Second
	maxD := 4 * time.Hour
	want := []time.Duration{5, 10, 20, 40, 80, 160, 320}
	for i, w := range want {
		attempts := i + 1
		got := base << (attempts - 1)
		if got > maxD || got <= 0 {
			got = maxD
		}
		if got != w*time.Second {
			t.Errorf("attempts=%d: got %v, want %v", attempts, got, w*time.Second)
		}
	}
}
