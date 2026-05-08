package relay

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// QueueConfig configures the relay delivery queue.
type QueueConfig struct {
	// Workers is the number of concurrent delivery goroutines. Default 4.
	Workers int
	// MaxRetries is the maximum delivery attempts before discarding. Default 7.
	MaxRetries int
	// BaseDelay is the initial backoff duration. Default 5s.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff. Default 4h.
	MaxDelay time.Duration
}

// ErrQueueFull is returned by Enqueue when the internal channel buffer is full.
var ErrQueueFull = errors.New("relay: queue full")

type queuedJob struct {
	from, subject, body string
	to                  []string
	headers             map[string]string
	attempts            int
	nextRetry           time.Time
}

// RelayQueue delivers messages asynchronously via a backend, retrying with
// exponential backoff on failure. In-flight messages are lost on process
// shutdown; for durability wire a persistent store in a future iteration.
type RelayQueue struct {
	backend Backend
	cfg     QueueConfig
	// jobs is never closed. Workers watch stopCh for the drain signal.
	jobs   chan queuedJob
	stopCh chan struct{} // closed by Stop; watched by workers and re-enqueue
	logger *slog.Logger
	wg     sync.WaitGroup
	cancel context.CancelFunc
}

// NewRelayQueue creates and starts a RelayQueue. Call Stop to drain workers.
func NewRelayQueue(backend Backend, cfg QueueConfig, logger *slog.Logger) *RelayQueue {
	if cfg.Workers <= 0 {
		cfg.Workers = 4
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 7
	}
	if cfg.BaseDelay <= 0 {
		cfg.BaseDelay = 5 * time.Second
	}
	if cfg.MaxDelay <= 0 {
		cfg.MaxDelay = 4 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	bufSize := cfg.Workers * 8
	if bufSize < 64 {
		bufSize = 64
	}
	ctx, cancel := context.WithCancel(context.Background())
	q := &RelayQueue{
		backend: backend,
		cfg:     cfg,
		jobs:    make(chan queuedJob, bufSize),
		stopCh:  make(chan struct{}),
		logger:  logger,
		cancel:  cancel,
	}
	for i := 0; i < cfg.Workers; i++ {
		q.wg.Add(1)
		go q.worker(ctx)
	}
	return q
}

// Enqueue submits a message for async delivery. Returns ErrQueueFull if the
// buffer is at capacity, or ctx.Err() if the context is cancelled before a
// slot is available. Never blocks beyond ctx expiry.
func (q *RelayQueue) Enqueue(ctx context.Context, from string, to []string, subject, body string, headers map[string]string) error {
	job := queuedJob{
		from:    from,
		to:      to,
		subject: subject,
		body:    body,
		headers: headers,
	}
	select {
	case q.jobs <- job:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		return ErrQueueFull
	}
}

// Stop signals workers to drain all buffered jobs and waits for completion.
// If ctx expires before the drain completes, the internal context is cancelled
// to abort any in-progress backend.Send calls and the function returns.
func (q *RelayQueue) Stop(ctx context.Context) {
	// Close stopCh first. Workers' main select and re-enqueue selects both
	// watch stopCh, so they exit cleanly after draining buffered items.
	// q.jobs is NOT closed here — closing a channel that a sender may still
	// reach causes a panic; the stopCh signal is the safe shutdown mechanism.
	close(q.stopCh)

	done := make(chan struct{})
	go func() { q.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		q.cancel() // abort in-progress backend.Send calls
		q.logger.Warn("relay queue: stop timed out, some jobs may be lost")
	}
}

// worker processes jobs until stopCh is closed, then drains any remaining
// buffered items before returning.
func (q *RelayQueue) worker(ctx context.Context) {
	defer q.wg.Done()
	for {
		select {
		case job := <-q.jobs:
			q.process(ctx, job)
		case <-q.stopCh:
			// Drain all remaining buffered jobs before exiting.
			for {
				select {
				case job := <-q.jobs:
					q.process(ctx, job)
				default:
					return
				}
			}
		}
	}
}

func (q *RelayQueue) process(ctx context.Context, job queuedJob) {
	// Wait until the scheduled retry time.
	if delay := time.Until(job.nextRetry); delay > 0 {
		t := time.NewTimer(delay)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}

	err := q.backend.Send(ctx, job.from, job.to, job.subject, job.body, job.headers)
	if err == nil {
		q.logger.Info("relay queue: delivered", "backend", q.backend.Name(),
			"from", job.from, "recipients", job.to, "attempts", job.attempts+1)
		return
	}

	job.attempts++
	if job.attempts >= q.cfg.MaxRetries {
		q.logger.Error("relay queue: permanently failed after max retries",
			"backend", q.backend.Name(), "from", job.from, "recipients", job.to,
			"attempts", job.attempts, "error", err)
		return
	}

	backoff := q.cfg.BaseDelay << (job.attempts - 1)
	if backoff > q.cfg.MaxDelay || backoff <= 0 {
		backoff = q.cfg.MaxDelay
	}
	job.nextRetry = time.Now().Add(backoff)
	q.logger.Warn("relay queue: delivery failed, will retry",
		"backend", q.backend.Name(), "from", job.from, "recipients", job.to,
		"attempts", job.attempts, "retry_in", backoff, "error", err)

	// Re-enqueue for retry. q.jobs is never closed so the send case cannot
	// panic. The stopCh case drops the retry cleanly when shutting down.
	select {
	case q.jobs <- job:
	case <-q.stopCh:
		q.logger.Warn("relay queue: dropped retry on shutdown",
			"from", job.from, "recipients", job.to, "attempts", job.attempts)
	default:
		q.logger.Error("relay queue: dropped retry (queue full)",
			"from", job.from, "recipients", job.to)
	}
}
