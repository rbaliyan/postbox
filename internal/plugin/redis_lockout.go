package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const defaultLockoutKeyPrefix = "postbox:lockout"

// RedisLockoutConfig configures the Redis-backed lockout store.
type RedisLockoutConfig struct {
	// Client is the Redis client to use. Required.
	Client redis.Cmdable
	// KeyPrefix is the Redis key namespace. Defaults to "postbox:lockout".
	KeyPrefix string
}

// RedisLockoutStore implements LockoutStore using Redis.
// Failure counters use INCR/EXPIRE; lockout markers use SET with TTL.
// This implementation shares state across all nodes reading the same Redis
// instance, making it suitable for clustered Postbox deployments.
type RedisLockoutStore struct {
	client    redis.Cmdable
	keyPrefix string
}

var _ LockoutStore = (*RedisLockoutStore)(nil)

// NewRedisLockoutStore creates a RedisLockoutStore.
func NewRedisLockoutStore(cfg RedisLockoutConfig) *RedisLockoutStore {
	prefix := cfg.KeyPrefix
	if prefix == "" {
		prefix = defaultLockoutKeyPrefix
	}
	return &RedisLockoutStore{client: cfg.Client, keyPrefix: prefix}
}

// RecordFailure increments the per-IP failure counter. When the counter reaches
// max, it marks the IP as blocked for lockout duration and returns (true, nil).
// Returns (false, error) if any Redis operation fails; the caller should log
// the error and fail-open rather than silently treating failures as safe.
func (s *RedisLockoutStore) RecordFailure(ctx context.Context, ip string, max int, window, lockout time.Duration) (bool, error) {
	cntKey := fmt.Sprintf("%s:cnt:%s", s.keyPrefix, ip)
	blockedKey := fmt.Sprintf("%s:blocked:%s", s.keyPrefix, ip)

	n, err := s.client.Incr(ctx, cntKey).Result()
	if err != nil {
		return false, fmt.Errorf("redis lockout: incr %s: %w", cntKey, err)
	}
	// Set expiry on first increment only (INCR returns 1).
	if n == 1 {
		if err := s.client.Expire(ctx, cntKey, window).Err(); err != nil {
			return false, fmt.Errorf("redis lockout: expire %s: %w", cntKey, err)
		}
	}
	if int(n) >= max {
		if err := s.client.Set(ctx, blockedKey, 1, lockout).Err(); err != nil {
			return false, fmt.Errorf("redis lockout: set blocked %s: %w", blockedKey, err)
		}
		return true, nil
	}
	return false, nil
}

// IsLockedOut returns (true, nil) if ip has an active lockout marker in Redis.
// Returns (false, error) if the Redis lookup fails.
func (s *RedisLockoutStore) IsLockedOut(ctx context.Context, ip string) (bool, error) {
	key := fmt.Sprintf("%s:blocked:%s", s.keyPrefix, ip)
	n, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return false, fmt.Errorf("redis lockout: exists %s: %w", key, err)
	}
	return n > 0, nil
}
