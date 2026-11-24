package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func decodeProblem(t *testing.T, w *httptest.ResponseRecorder) Problem {
	t.Helper()
	var p Problem
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
	return p
}

func TestLoadShedRejectsOverCapacityWithoutBlocking(t *testing.T) {
	const max = 3
	release := make(chan struct{})
	entered := make(chan struct{}, max)
	h := LoadShed(max)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	var wg sync.WaitGroup
	codes := make(chan int, max)
	for i := 0; i < max; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
			codes <- w.Code
		}()
	}
	for i := 0; i < max; i++ {
		<-entered
	}
	// At capacity: the next request is shed immediately.
	start := time.Now()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" || decodeProblem(t, w).Code != "overloaded" {
		t.Fatalf("got %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if time.Since(start) > time.Second {
		t.Fatal("shedding blocked")
	}
	close(release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusNoContent {
			t.Fatalf("admitted request got %d", c)
		}
	}
	// Slots are released: a new request is admitted.
	w = httptest.NewRecorder()
	h2 := LoadShed(1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for i := 0; i < 3; i++ {
		h2.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
}

func TestLoadShedReleasesSlotOnPanic(t *testing.T) {
	n := 0
	h := LoadShed(1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n++
		if n == 1 {
			panic("boom")
		}
		w.WriteHeader(204)
	}))
	func() {
		defer func() { _ = recover() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 204 {
		t.Fatalf("slot leaked after panic: %d", w.Code)
	}
}

func TestLoadShedDisabled(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if h := LoadShed(0)(next); h == nil {
		t.Fatal("nil handler")
	}
}

// TestLoadShedNeverExceedsLimit races many goroutines (run with -race).
func TestLoadShedNeverExceedsLimit(t *testing.T) {
	const max = 8
	var cur, peak, shed, served atomic.Int64
	h := LoadShed(max)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		c := cur.Add(1)
		for {
			p := peak.Load()
			if c <= p || peak.CompareAndSwap(p, c) {
				break
			}
		}
		time.Sleep(50 * time.Microsecond)
		cur.Add(-1)
		served.Add(1)
	}))
	var wg sync.WaitGroup
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
				if w.Code == http.StatusServiceUnavailable {
					shed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if p := peak.Load(); p > max {
		t.Fatalf("peak in-flight %d > %d", p, max)
	}
	if served.Load()+shed.Load() != 6400 {
		t.Fatalf("served %d + shed %d != 6400", served.Load(), shed.Load())
	}
}

func TestBusyAndOverloadedProblems(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	Busy(w, r, 1500*time.Millisecond)
	if w.Code != 503 || w.Header().Get("Retry-After") != "2" || decodeProblem(t, w).Code != "busy" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
	w = httptest.NewRecorder()
	Overloaded(w, r)
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || decodeProblem(t, w).Code != "overloaded" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}

func TestTimeoutPassesFastResponseThrough(t *testing.T) {
	h := Timeout(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Context().Deadline(); !ok {
			t.Error("no deadline on handler context")
		}
		w.Header().Set("X-A", "1")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "hello")
		_, _ = io.WriteString(w, " world")
	}))
	w := httptest.NewRecorder()
	w.Header().Set("X-Request-Id", "outer")
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 201 || w.Body.String() != "hello world" || w.Header().Get("X-A") != "1" || w.Header().Get("X-Request-Id") != "outer" {
		t.Fatalf("%d %q %v", w.Code, w.Body.String(), w.Header())
	}
}

func TestTimeoutImplicit200AndEmpty(t *testing.T) {
	w := httptest.NewRecorder()
	Timeout(time.Second)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal(w.Code)
	}
}

func TestTimeoutRespondsWithProblemAndCancelsHandler(t *testing.T) {
	handlerErr := make(chan error, 1)
	h := RequestID(Timeout(30 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // a well-behaved handler (DB call) aborts on ctx
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("X-Late", "1")
		_, err := w.Write([]byte("late"))
		handlerErr <- err
	})))
	w := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if el := time.Since(start); el > time.Second {
		t.Fatalf("timeout took %v", el)
	}
	p := decodeProblem(t, w)
	if w.Code != 503 || p.Code != "timeout" || w.Header().Get("Retry-After") != "1" || p.RequestID == "" {
		t.Fatalf("%d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if err := <-handlerErr; !errors.Is(err, http.ErrHandlerTimeout) {
		t.Fatalf("late write err = %v", err)
	}
	if strings.Contains(w.Body.String(), "late") || w.Header().Get("X-Late") != "" {
		t.Fatal("late handler output leaked into the response")
	}
}

func TestTimeoutClientGone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := Timeout(time.Minute)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		time.Sleep(50 * time.Millisecond) // still unwinding when the middleware gives up
	}))
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	w := httptest.NewRecorder()
	go cancel()
	h.ServeHTTP(w, r)
	if w.Code != statusClientClosedRequest {
		t.Fatal(w.Code)
	}
}

func TestTimeoutRepanicsForRecover(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := Recover(logger)(Timeout(time.Second)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 500 || decodeProblem(t, w).Code != "internal_error" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestTimeoutInvalidStatusPanics(t *testing.T) {
	tw := &timeoutWriter{h: make(http.Header)}
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	tw.WriteHeader(42)
}

func TestTimeoutWriterIgnoresSecondWriteHeader(t *testing.T) {
	tw := &timeoutWriter{h: make(http.Header)}
	tw.WriteHeader(201)
	tw.WriteHeader(500)
	w := httptest.NewRecorder()
	tw.flushTo(w)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
}

func TestTimeoutDisabled(t *testing.T) {
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	w := httptest.NewRecorder()
	Timeout(0)(next).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
}

// TestTimeoutRaceAtDeadline makes handlers finish right around the deadline
// many times; with -race this proves there is no data race between the
// handler's writes and the timeout response, and every response is one of
// the two complete, well-formed outcomes.
func TestTimeoutRaceAtDeadline(t *testing.T) {
	h := Timeout(2 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Millisecond)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("ok"))
	}))
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
				switch {
				case w.Code == 200 && w.Body.String() == "ok":
				case w.Code == 503 && strings.Contains(w.Body.String(), `"timeout"`):
				default:
					t.Errorf("mixed response %d %q", w.Code, w.Body.String())
				}
			}
		}()
	}
	wg.Wait()
}

func BenchmarkLoadShedParallel(b *testing.B) {
	h := LoadShed(64)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		w := httptest.NewRecorder()
		for pb.Next() {
			h.ServeHTTP(w, r)
		}
	})
}

func BenchmarkTimeoutParallel(b *testing.B) {
	h := Timeout(time.Second)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) }))
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.ServeHTTP(httptest.NewRecorder(), r)
		}
	})
}
