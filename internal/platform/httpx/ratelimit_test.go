package httpx

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixedClock(l *RateLimiter, start time.Time) *time.Time {
	now := start
	l.now = func() time.Time { return now }
	return &now
}

func TestRateLimiterRetryAfterIsExact(t *testing.T) {
	l := NewRateLimiterPerSecond(2, 2) // one token every 500ms
	now := fixedClock(l, time.Unix(1_800_000_000, 0))
	l.Allow("k")
	l.Allow("k")
	ok, retry := l.Allow("k")
	if ok || retry != 500*time.Millisecond {
		t.Fatalf("ok=%v retry=%v want 500ms", ok, retry)
	}
	*now = now.Add(200 * time.Millisecond)
	if ok, retry = l.Allow("k"); ok || retry != 300*time.Millisecond {
		t.Fatalf("after 200ms: ok=%v retry=%v want 300ms", ok, retry)
	}
	// Waiting exactly the advertised time must succeed.
	*now = now.Add(retry)
	if ok, _ = l.Allow("k"); !ok {
		t.Fatal("denied after waiting the advertised Retry-After")
	}
}

func TestRateLimiterRefillCapsAtBurst(t *testing.T) {
	l := NewRateLimiterPerSecond(1, 3)
	now := fixedClock(l, time.Unix(1_800_000_000, 0))
	l.Allow("k")
	*now = now.Add(time.Hour)
	n := 0
	for {
		if ok, _ := l.Allow("k"); !ok {
			break
		}
		n++
	}
	if n != 3 {
		t.Fatalf("allowed %d after long idle, want burst 3", n)
	}
}

func TestRateLimiterSweepsIdleKeys(t *testing.T) {
	l := NewRateLimiterPerSecond(1, 1)
	now := fixedClock(l, time.Unix(1_800_000_000, 0))
	for i := 0; i < 500; i++ {
		l.Allow(fmt.Sprint("idle", i))
	}
	*now = now.Add(l.idleTTL + time.Second)
	// Sweeps run every rateSweepEvery calls per shard: drive enough traffic
	// through fresh keys that every shard sweeps at least once.
	hot := make([]string, 4096)
	for i := range hot {
		hot[i] = "hot" + strconv.Itoa(i)
	}
	for i := 0; i < 4*rateShards*rateSweepEvery; i++ {
		l.Allow(hot[i%len(hot)])
	}
	idleLeft := 0
	for i := range l.shards {
		for k := range l.shards[i].buckets {
			if len(k) > 4 && k[:4] == "idle" {
				idleLeft++
			}
		}
	}
	if idleLeft != 0 {
		t.Fatalf("%d idle keys survived the sweep", idleLeft)
	}
}

func TestRateLimiterOverflowSharesBucket(t *testing.T) {
	l := NewRateLimiterPerSecond(1, 2)
	l.maxKeys = rateShards // one key per shard
	fixedClock(l, time.Unix(1_800_000_000, 0))
	// Fill every shard, then new keys must share their shard's overflow
	// bucket: in total at most burst(2) per shard succeed for new keys.
	for i := 0; i < 10_000; i++ {
		l.Allow(fmt.Sprint("fill", i))
	}
	if n := l.Len(); n > rateShards {
		t.Fatalf("tracked %d keys > cap", n)
	}
	allowed := 0
	for i := 0; i < 100_000; i++ {
		if ok, _ := l.Allow(fmt.Sprint("new", i)); ok {
			allowed++
		}
	}
	if allowed > 0 {
		t.Fatalf("overflow buckets should be exhausted by the fill loop, allowed %d", allowed)
	}
}

func TestRateLimiterDegenerateParams(t *testing.T) {
	l := NewRateLimiterPerSecond(0, 0) // clamps: burst 1, ~no refill
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("first request denied")
	}
	ok, retry := l.Allow("k")
	if ok || retry != maxRetryAfter {
		t.Fatalf("ok=%v retry=%v", ok, retry)
	}
	if NewRateLimiter(60, 5).rate != 1 {
		t.Fatal("perMinute conversion")
	}
}

func TestRetryAfterSeconds(t *testing.T) {
	cases := map[time.Duration]int{
		0: 1, -time.Second: 1, time.Nanosecond: 1, time.Second: 1, time.Second + 1: 2,
		1500 * time.Millisecond: 2, 59 * time.Second: 59, 1000 * time.Hour: 86400,
		time.Duration(math.MaxInt64): 86400,
	}
	for d, want := range cases {
		if got := RetryAfterSeconds(d); got != want {
			t.Errorf("RetryAfterSeconds(%v) = %d, want %d", d, got, want)
		}
	}
}

func TestTooManyRequestsHeaderMatchesBody(t *testing.T) {
	w := httptest.NewRecorder()
	TooManyRequests(w, httptest.NewRequest(http.MethodGet, "/", nil), 2300*time.Millisecond)
	if w.Code != 429 || w.Header().Get("Retry-After") != "3" || w.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}

func TestLimitByUsesKeyFunc(t *testing.T) {
	l := NewRateLimiterPerSecond(1, 1)
	fixedClock(l, time.Unix(1_800_000_000, 0))
	h := LimitBy(l, func(r *http.Request) string { return r.Header.Get("K") })(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	do := func(k string) int {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("K", k)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	if do("a") != 200 || do("a") != 429 || do("b") != 200 {
		t.Fatal("per-key isolation broken")
	}
}

// TestRateLimiterConcurrentExactCount: with no refill, exactly burst
// requests per key succeed no matter how many goroutines race (run -race).
func TestRateLimiterConcurrentExactCount(t *testing.T) {
	const keys, burst, goroutines, per = 50, 20, 32, 200
	l := NewRateLimiterPerSecond(1e-9, burst)
	var allowed [keys]atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				k := (g + i) % keys
				if ok, _ := l.Allow("key" + strconv.Itoa(k)); ok {
					allowed[k].Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
	for k := range allowed {
		if n := allowed[k].Load(); n != burst {
			t.Fatalf("key %d: %d allowed, want %d", k, n, burst)
		}
	}
}

func BenchmarkRateLimiterParallelManyKeys(b *testing.B) {
	l := NewRateLimiterPerSecond(1e6, 1_000_000)
	keys := make([]string, 10_000)
	for i := range keys {
		keys[i] = "v1|key_" + strconv.Itoa(i)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.Allow(keys[i%len(keys)])
			i += 7
		}
	})
}

func BenchmarkRateLimiterParallelOneKey(b *testing.B) {
	l := NewRateLimiterPerSecond(1e9, 1_000_000_000)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.Allow("v1|hot")
		}
	})
}
