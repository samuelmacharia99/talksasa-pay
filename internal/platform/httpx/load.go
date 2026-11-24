package httpx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// LoadShed caps the number of requests in flight through the wrapped
// handler. Beyond maxInFlight it answers 503 "overloaded" with
// Retry-After: 1 immediately: it never queues or blocks, so a burst cannot
// pile up goroutines and DB connection waits until everything times out.
// maxInFlight < 1 disables shedding.
func LoadShed(maxInFlight int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if maxInFlight < 1 {
			return next
		}
		limit := int64(maxInFlight)
		var inFlight atomic.Int64
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if inFlight.Add(1) > limit {
				inFlight.Add(-1)
				Overloaded(w, r)
				return
			}
			defer inFlight.Add(-1)
			next.ServeHTTP(w, r)
		})
	}
}

// Overloaded writes 503 "overloaded" with Retry-After: 1.
func Overloaded(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "1")
	WriteProblem(w, r, NewProblem(http.StatusServiceUnavailable, "overloaded", "The service is at capacity. Retry shortly."))
}

// Busy writes 503 "busy" with Retry-After for transient database contention
// (see db.IsBusy): a lock wait or statement timed out, or a transaction kept
// conflicting. The request had no effect and is safe to retry.
func Busy(w http.ResponseWriter, r *http.Request, retry time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(RetryAfterSeconds(retry)))
	WriteProblem(w, r, NewProblem(http.StatusServiceUnavailable, "busy", "The resource is busy with another request. Retry shortly."))
}

// Timeout gives the wrapped handler a context deadline of d. If the handler
// has not finished by then, the client gets 503 "timeout" (with
// Retry-After: 1) and the handler's later writes fail with
// http.ErrHandlerTimeout; its context is cancelled, so DB calls abort.
//
// Like http.TimeoutHandler, the handler runs in its own goroutine and its
// response is buffered until it returns, so there is never a race between a
// late handler write and the timeout response. Consequences: responses are
// held in memory (fine for JSON APIs, not for large downloads) and
// http.Flusher / Hijacker are not available. A panic in the handler is
// re-raised in the calling goroutine so an outer Recover still sees it.
// d <= 0 disables the timeout.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)

			tw := &timeoutWriter{h: make(http.Header)}
			done := make(chan struct{})
			panicked := make(chan any, 1)
			go func() {
				defer func() {
					if p := recover(); p != nil {
						panicked <- p
					}
				}()
				next.ServeHTTP(tw, r)
				close(done)
			}()

			select {
			case p := <-panicked:
				panic(p)
			case <-done:
				tw.flushTo(w)
				return
			case <-ctx.Done():
			}
			tw.mu.Lock()
			select {
			case <-done: // finished at the deadline: prefer the real response
				tw.mu.Unlock()
				tw.flushTo(w)
				return
			default:
			}
			tw.timedOut = true // late handler writes now fail
			tw.mu.Unlock()
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				w.Header().Set("Retry-After", "1")
				WriteProblem(w, r, NewProblem(http.StatusServiceUnavailable, "timeout",
					fmt.Sprintf("The request did not complete within %s. It may or may not have taken effect; retry with the same Idempotency-Key.", d)))
				return
			}
			// The client went away: nobody is listening.
			w.WriteHeader(statusClientClosedRequest)
		})
	}
}

// statusClientClosedRequest is nginx's de-facto code for a client that
// disconnected before the response; it only shows up in logs and metrics.
const statusClientClosedRequest = 499

// timeoutWriter buffers the handler's response until it returns.
type timeoutWriter struct {
	mu       sync.Mutex
	h        http.Header
	buf      bytes.Buffer
	code     int
	timedOut bool
}

// Header returns the handler's private header map. It is only copied to the
// real response after the handler has returned, so it needs no lock.
func (tw *timeoutWriter) Header() http.Header { return tw.h }

// flushTo copies the finished handler's response to w. Only called after
// the handler goroutine has returned.
func (tw *timeoutWriter) flushTo(w http.ResponseWriter) {
	dst := w.Header()
	for k, vv := range tw.h {
		dst[k] = vv
	}
	code := tw.code
	if code == 0 {
		code = http.StatusOK
	}
	w.WriteHeader(code)
	_, _ = w.Write(tw.buf.Bytes())
}

func (tw *timeoutWriter) Write(p []byte) (int, error) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut {
		return 0, http.ErrHandlerTimeout
	}
	if tw.code == 0 {
		tw.code = http.StatusOK
	}
	return tw.buf.Write(p)
}

func (tw *timeoutWriter) WriteHeader(code int) {
	if code < 100 || code > 999 {
		panic(fmt.Sprintf("invalid WriteHeader code %v", code))
	}
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if tw.timedOut || tw.code != 0 {
		return
	}
	tw.code = code
}
