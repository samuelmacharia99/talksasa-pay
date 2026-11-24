package httpx

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimiter is an in-process token-bucket limiter keyed by an arbitrary
// string (client IP, account). It is sufficient for a single API instance;
// with several instances it moves to Postgres (see docs).
//
// Memory is bounded: idle buckets are swept, and when the table is full new
// keys share one overflow bucket, so a flood of distinct keys degrades to a
// global limit instead of exhausting memory.
type RateLimiter struct {
	rate     float64 // tokens per second
	burst    float64
	maxKeys  int
	idleTTL  time.Duration
	now      func() time.Time
	mu       sync.Mutex
	buckets  map[string]*bucket
	overflow bucket
	ops      int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows `perMinute` requests per minute per key with bursts
// of up to `burst`.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return &RateLimiter{
		rate: float64(perMinute) / 60, burst: float64(burst), maxKeys: 100_000, idleTTL: 10 * time.Minute,
		now: time.Now, buckets: map[string]*bucket{},
	}
}

// Allow takes one token for key. When it returns false, retryAfter says
// when the next token is due.
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.ops++
	if l.ops%1024 == 0 {
		l.sweep(now)
	}
	b, found := l.buckets[key]
	if !found {
		if len(l.buckets) >= l.maxKeys {
			b = &l.overflow
		} else {
			b = &bucket{tokens: l.burst, last: now}
			l.buckets[key] = b
		}
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
}

func (l *RateLimiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.idleTTL {
			delete(l.buckets, k)
		}
	}
}

// LimitByIP rejects requests over the limit with 429 and Retry-After. It
// must run after ClientIP.
func LimitByIP(l *RateLimiter, scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := scope + "|unknown"
			if ip := ClientIPFrom(r.Context()); ip.IsValid() {
				key = scope + "|" + ip.String()
			}
			if ok, retry := l.Allow(key); !ok {
				TooManyRequests(w, r, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// TooManyRequests writes a 429 problem with Retry-After.
func TooManyRequests(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	secs := int(math.Ceil(retry.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	WriteProblem(w, r, NewProblem(http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("Too many requests. Try again in %d seconds.", secs)))
}
