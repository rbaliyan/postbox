package webhook_test

import (
	"testing"
	"time"

	"github.com/rbaliyan/postbox/internal/webhook"
)

func TestNextRetryAt_AlwaysFuture(t *testing.T) {
	for n := 0; n <= 20; n++ {
		before := time.Now()
		got := webhook.NextRetryAt(n)
		if !got.After(before) {
			t.Errorf("NextRetryAt(%d) = %v is not after %v", n, got, before)
		}
	}
}

func TestNextRetryAt_N0_AroundTenSeconds(t *testing.T) {
	// n=0: base=10s, jitter ±20% → range [8s, 12s]
	now := time.Now()
	got := webhook.NextRetryAt(0)
	diff := got.Sub(now)

	lo := 7 * time.Second  // a bit under 8s to tolerate timing imprecision
	hi := 13 * time.Second // a bit over 12s
	if diff < lo || diff > hi {
		t.Errorf("NextRetryAt(0) diff=%v; want in [%v, %v]", diff, lo, hi)
	}
}

func TestNextRetryAt_N1_AroundTwentySeconds(t *testing.T) {
	// n=1: base=10s*2^1=20s, jitter ±20% → range [16s, 24s]
	now := time.Now()
	got := webhook.NextRetryAt(1)
	diff := got.Sub(now)

	lo := 14 * time.Second
	hi := 26 * time.Second
	if diff < lo || diff > hi {
		t.Errorf("NextRetryAt(1) diff=%v; want in [%v, %v]", diff, lo, hi)
	}
}

func TestNextRetryAt_LargeN_CappedAtOneHour(t *testing.T) {
	// Large n should be capped at retryMax (1 hour), then jitter of ±20%
	// → max ~72 minutes, min ~48 minutes
	now := time.Now()
	got := webhook.NextRetryAt(100)
	diff := got.Sub(now)

	lo := 47 * time.Minute
	hi := 73 * time.Minute
	if diff < lo || diff > hi {
		t.Errorf("NextRetryAt(100) diff=%v; want in [%v, %v]", diff, lo, hi)
	}
}

func TestIsPermanentFailure(t *testing.T) {
	cases := []struct {
		status    int
		permanent bool
	}{
		// Permanent failures (4xx, except 429)
		{400, true},
		{401, true},
		{403, true},
		{404, true},
		{422, true},
		// Not permanent — retry
		{429, false}, // Too Many Requests
		{500, false},
		{502, false},
		{503, false},
		{200, false},
		{201, false},
		{301, false},
	}

	for _, tc := range cases {
		got := webhook.IsPermanentFailure(tc.status)
		if got != tc.permanent {
			t.Errorf("IsPermanentFailure(%d) = %v; want %v", tc.status, got, tc.permanent)
		}
	}
}
