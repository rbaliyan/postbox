package webhook

import (
	"math"
	"math/rand/v2"
	"time"
)

const (
	retryBase     = 10 * time.Second
	retryMax      = 1 * time.Hour
	retryJitter   = 0.2 // ±20%
	defaultMaxTry = 5
)

// NextRetryAt computes when attempt number n (0-indexed, so n=1 means the
// first retry after an initial failure) should be retried.
// The schedule is: base * 2^n, capped at retryMax, with ±20% jitter.
func NextRetryAt(n int) time.Time {
	delay := float64(retryBase) * math.Pow(2, float64(n))
	if delay > float64(retryMax) {
		delay = float64(retryMax)
	}
	jitter := delay * retryJitter * (rand.Float64()*2 - 1) // uniform in [-jitter, +jitter]
	return time.Now().UTC().Add(time.Duration(delay + jitter))
}

// isPermanentFailure reports whether an HTTP status code should not be retried.
// 4xx client errors (except 429 Too Many Requests) are treated as permanent.
func isPermanentFailure(httpStatus int) bool {
	return httpStatus >= 400 && httpStatus < 500 && httpStatus != 429
}
