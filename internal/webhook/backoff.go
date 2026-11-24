package webhook

import (
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Retry schedule (queue.WebhookMaxAttempts = 25 attempts = 24 retries).
//
// Retry n (after failed attempt n) waits a random time in
// [1 s, ceiling(n)] ("full jitter"), where
//
//	ceiling(n) = min(RetryCap, RetryBase * 2^(n-1)) = min(10 h, 30 s * 2^(n-1))
//
//	retry:    1    2   3   4   5    6    7    8    9     10     11     12-24
//	ceiling:  30s  1m  2m  4m  8m   16m  32m  64m  128m  4.3h   8.5h   10h
//
// Sum of ceilings is about 147 h, so the expected span from the first to the
// last attempt is about 74 h (~3 days), with a hard upper bound of ~6 days.
// Full jitter spreads retries of many failed events so a receiver coming back
// up is not hit by a thundering herd.
const (
	RetryBase = 30 * time.Second
	RetryCap  = 10 * time.Hour

	// Retry-After from a 429/503 is honoured up to this much; a larger value
	// (or garbage) is clamped so one response can't park an event for days.
	MaxRetryAfter = 6 * time.Hour
)

// Backoff returns the full-jitter delay before retry n (n = failed attempts so far, >= 1).
func Backoff(n int) time.Duration {
	c := backoffCeiling(n)
	return time.Second + rand.N(c) //nolint:gosec // jitter, not security
}

func backoffCeiling(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	if n > 30 { // 30 s * 2^29 overflows nothing but is far past the cap
		return RetryCap
	}
	c := RetryBase << (n - 1)
	if c > RetryCap || c <= 0 {
		return RetryCap
	}
	return c
}

// parseRetryAfter reads Retry-After (seconds or an HTTP date) on 429/503.
// ok is false when absent, invalid or not one of those statuses.
func parseRetryAfter(status int, h http.Header, now time.Time) (time.Duration, bool) {
	if status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable {
		return 0, false
	}
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	var d time.Duration
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		if secs > int64(MaxRetryAfter/time.Second) {
			secs = int64(MaxRetryAfter / time.Second)
		}
		d = time.Duration(secs) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	} else {
		return 0, false
	}
	if d < 0 {
		d = 0
	}
	if d > MaxRetryAfter {
		d = MaxRetryAfter
	}
	return d, true
}
