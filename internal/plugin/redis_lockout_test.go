package plugin

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func newTestRedisStore(t *testing.T) (*RedisLockoutStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := NewRedisLockoutStore(RedisLockoutConfig{Client: client})
	return store, mr
}

func TestRedisLockoutStore_NotLockedInitially(t *testing.T) {
	store, _ := newTestRedisStore(t)
	locked, err := store.IsLockedOut(context.Background(), "1.2.3.4")
	if err != nil {
		t.Fatalf("IsLockedOut: %v", err)
	}
	if locked {
		t.Error("expected not locked, got locked")
	}
}

func TestRedisLockoutStore_LockoutAfterMax(t *testing.T) {
	store, _ := newTestRedisStore(t)
	ctx := context.Background()
	const max = 3
	window := time.Minute
	lockout := 15 * time.Minute

	for i := 0; i < max-1; i++ {
		locked, err := store.RecordFailure(ctx, "10.0.0.1", max, window, lockout)
		if err != nil {
			t.Fatalf("RecordFailure %d: %v", i+1, err)
		}
		if locked {
			t.Errorf("should not be locked after %d failures (max=%d)", i+1, max)
		}
	}

	// Reaching threshold.
	locked, err := store.RecordFailure(ctx, "10.0.0.1", max, window, lockout)
	if err != nil {
		t.Fatalf("RecordFailure at threshold: %v", err)
	}
	if !locked {
		t.Error("expected locked after reaching max failures")
	}

	// IsLockedOut should confirm.
	isLocked, err := store.IsLockedOut(ctx, "10.0.0.1")
	if err != nil {
		t.Fatalf("IsLockedOut: %v", err)
	}
	if !isLocked {
		t.Error("expected IP to be locked out")
	}
}

func TestRedisLockoutStore_LockoutExpires(t *testing.T) {
	store, mr := newTestRedisStore(t)
	ctx := context.Background()

	// Lock out immediately with a very short TTL.
	_, err := store.RecordFailure(ctx, "192.168.1.1", 1, time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	isLocked, _ := store.IsLockedOut(ctx, "192.168.1.1")
	if !isLocked {
		t.Fatal("expected locked immediately after threshold")
	}

	// Fast-forward miniredis time past the lockout TTL.
	mr.FastForward(200 * time.Millisecond)

	isLocked, err = store.IsLockedOut(ctx, "192.168.1.1")
	if err != nil {
		t.Fatalf("IsLockedOut after expiry: %v", err)
	}
	if isLocked {
		t.Error("expected lockout to have expired")
	}
}

func TestRedisLockoutStore_DifferentIPsIndependent(t *testing.T) {
	store, _ := newTestRedisStore(t)
	ctx := context.Background()
	const max = 2

	store.RecordFailure(ctx, "1.1.1.1", max, time.Minute, time.Minute) //nolint:errcheck
	store.RecordFailure(ctx, "1.1.1.1", max, time.Minute, time.Minute) //nolint:errcheck

	locked1, _ := store.IsLockedOut(ctx, "1.1.1.1")
	locked2, _ := store.IsLockedOut(ctx, "2.2.2.2")

	if !locked1 {
		t.Error("1.1.1.1 should be locked")
	}
	if locked2 {
		t.Error("2.2.2.2 should not be locked")
	}
}

func TestRedisLockoutStore_KeyPrefix(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	store := NewRedisLockoutStore(RedisLockoutConfig{Client: client, KeyPrefix: "myapp:locks"})

	ctx := context.Background()
	store.RecordFailure(ctx, "5.5.5.5", 1, time.Minute, time.Minute) //nolint:errcheck

	// Key should exist under the custom prefix.
	keys := mr.Keys()
	found := false
	for _, k := range keys {
		if len(k) > 9 && k[:9] == "myapp:loc" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected key with custom prefix 'myapp:locks', got keys: %v", keys)
	}
}

func TestRedisLockoutStore_RedisError(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close() // close before the client is used so all calls fail immediately

	client := redis.NewClient(&redis.Options{
		Addr:         addr,
		DialTimeout:  10 * time.Millisecond,
		ReadTimeout:  10 * time.Millisecond,
		WriteTimeout: 10 * time.Millisecond,
		MaxRetries:   0,
	})
	store := NewRedisLockoutStore(RedisLockoutConfig{Client: client})
	ctx := context.Background()

	_, err := store.RecordFailure(ctx, "9.9.9.9", 3, time.Minute, time.Minute)
	if err == nil {
		t.Error("expected error when Redis is down, got nil")
	}

	_, err = store.IsLockedOut(ctx, "9.9.9.9")
	if err == nil {
		t.Error("expected error from IsLockedOut when Redis is down, got nil")
	}
}
