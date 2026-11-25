package httpx

import (
	"fmt"
	"hash/maphash"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RateLimiter is an in-process token-bucket limiter keyed by an arbitrary
// string (client IP, API key id). It is sufficient for a single API
// instance; with several instances it moves to Postgres (see docs).
//
// It is safe for concurrent use and low-contention: keys are spread over
// independently locked shards, so requests for different keys rarely touch
// the same mutex.
//
// Memory is bounded: idle buckets are swept (incrementally, by the callers
// themselves — no background goroutine), and when a shard is full new keys
// share that shard's overflow bucket, so a flood of distinct keys degrades
// to a shared limit instead of exhausting memory.
type RateLimiter struct {
	rate    float64 // tokens per second
	burst   float64
	maxKeys int // total, split evenly across shards
	idleTTL time.Duration
	now     func() time.Time
	seed    maphash.Seed
	shards  [rateShards]rateShard
}

const (
	rateShards     = 64 // power of two
	rateSweepEvery = 1024
)

type rateShard struct {
	mu       sync.Mutex
	buckets  map[string]bucket
	overflow bucket
	ops      uint32
	_        [64]byte // keep neighbouring shard locks on separate cache lines
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimiter allows `perMinute` requests per minute per key with bursts
// of up to `burst`.
func NewRateLimiter(perMinute, burst int) *RateLimiter {
	return NewRateLimiterPerSecond(float64(perMinute)/60, burst)
}

// NewRateLimiterPerSecond allows `perSecond` requests per second per key
// (fractions allowed) with bursts of up to `burst` (at least 1).
func NewRateLimiterPerSecond(perSecond float64, burst int) *RateLimiter {
	if perSecond <= 0 {
		perSecond = math.SmallestNonzeroFloat64
	}
	if burst < 1 {
		burst = 1
	}
	l := &RateLimiter{
		rate: perSecond, burst: float64(burst), maxKeys: 100_000, idleTTL: 10 * time.Minute,
		now: time.Now, seed: maphash.MakeSeed(),
	}
	for i := range l.shards {
		l.shards[i].buckets = make(map[string]bucket)
	}
	return l
}

// Allow takes one token for key. When it returns false, retryAfter is the
// exact time until the next token is due (always > 0).
func (l *RateLimiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	s := &l.shards[maphash.String(l.seed, key)&(rateShards-1)]
	perShard := l.maxKeys / rateShards
	if perShard < 1 {
		perShard = 1
	}
	s.mu.Lock()
	now := l.now()
	s.ops++
	if s.ops%rateSweepEvery == 0 {
		s.sweep(now, l.idleTTL)
	}
	b, found := s.buckets[key]
	overflow := false
	switch {
	case found:
	case len(s.buckets) >= perShard:
		b, overflow = s.overflow, true
		if b.last.IsZero() {
			b = bucket{tokens: l.burst, last: now}
		}
	default:
		b = bucket{tokens: l.burst, last: now}
	}
	if el := now.Sub(b.last); el > 0 {
		b.tokens = math.Min(l.burst, b.tokens+el.Seconds()*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		ok = true
	} else {
		wait := math.Ceil((1 - b.tokens) / l.rate * float64(time.Second))
		switch {
		case wait >= float64(maxRetryAfter): // also catches +Inf for tiny rates
			retryAfter = maxRetryAfter
		case wait < 1:
			retryAfter = 1
		default:
			retryAfter = time.Duration(wait)
		}
	}
	if overflow {
		s.overflow = b
	} else {
		s.buckets[key] = b
	}
	s.mu.Unlock()
	return ok, retryAfter
}

// Len reports how many keys are tracked (for tests and debugging).
func (l *RateLimiter) Len() int {
	n := 0
	for i := range l.shards {
		s := &l.shards[i]
		s.mu.Lock()
		n += len(s.buckets)
		s.mu.Unlock()
	}
	return n
}

// sweep drops buckets idle for longer than ttl. A bucket idle that long has
// refilled completely, so forgetting it changes nothing for the client.
func (s *rateShard) sweep(now time.Time, ttl time.Duration) {
	for k, b := range s.buckets {
		if now.Sub(b.last) > ttl {
			delete(s.buckets, k)
		}
	}
}

// LimitByIP rejects requests over the limit with 429 and Retry-After. It
// must run after ClientIP.
func LimitByIP(l *RateLimiter, scope string) func(http.Handler) http.Handler {
	return LimitBy(l, func(r *http.Request) string {
		if ip := ClientIPFrom(r.Context()); ip.IsValid() {
			return scope + "|" + ip.String()
		}
		return scope + "|unknown"
	})
}

// LimitBy rejects requests over the limit for key(r) with 429 and an exact
// Retry-After. Use it after authentication with the API key id (or app
// id) as the key, e.g. for /v1:
//
//	LimitBy(l, func(r *http.Request) string { return "v1|" + principal(r).KeyID })
func LimitBy(l *RateLimiter, key func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retry := l.Allow(key(r)); !ok {
				TooManyRequests(w, r, retry)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// maxRetryAfter caps advertised Retry-After values.
const maxRetryAfter = 24 * time.Hour

// RetryAfterSeconds converts a wait into a Retry-After value: whole seconds
// rounded UP (a client retrying after the advertised delay must not be
// rejected again), at least 1, at most one day.
func RetryAfterSeconds(d time.Duration) int {
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	secs := int((d + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	return secs
}

// TooManyRequests writes a 429 problem with Retry-After.
func TooManyRequests(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	secs := RetryAfterSeconds(retry)
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	WriteProblem(w, r, NewProblem(http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("Too many requests. Try again in %d seconds.", secs)))
}
