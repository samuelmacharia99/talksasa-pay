package webhook

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// endpoints tracks, per app (= per webhook endpoint), a circuit breaker
// and the number of in-flight deliveries. State is in memory, per worker
// process: it only decides when to *try*, never whether an event is
// delivered, so losing it on restart is harmless.
//
// Breaker: after Threshold consecutive failures the endpoint is open for
// Cooldown; jobs that arrive meanwhile are snoozed (River does not count a
// snooze as an attempt, so no event loses retries). After the cool-down one
// probe is let through (half-open); success closes the breaker, failure
// re-opens it for another Cooldown.
type endpoints struct {
	mu          sync.Mutex
	m           map[uuid.UUID]*endpoint
	threshold   int
	cooldown    time.Duration
	maxInFlight int
	now         func() time.Time
}

type endpoint struct {
	inFlight  int
	failures  int       // consecutive
	openUntil time.Time // zero = closed
	probing   bool      // half-open probe in flight
}

func newEndpoints(threshold int, cooldown time.Duration, maxInFlight int, now func() time.Time) *endpoints {
	return &endpoints{m: map[uuid.UUID]*endpoint{}, threshold: threshold, cooldown: cooldown, maxInFlight: maxInFlight, now: now}
}

// admission is the result of acquire.
type admission int

const (
	admitted admission = iota
	rejectedOpen
	rejectedBusy
)

// ticket is held by an admitted delivery and handed back to release.
type ticket struct {
	id    uuid.UUID
	probe bool // the single half-open probe
}

// acquire asks to start a delivery. When admitted the caller must call
// release exactly once with the ticket. wait is how long the breaker stays
// open when rejectedOpen.
func (e *endpoints) acquire(id uuid.UUID) (adm admission, wait time.Duration, t ticket) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ep := e.m[id]
	if ep == nil {
		ep = &endpoint{}
		e.m[id] = ep
	}
	halfOpen := false
	if !ep.openUntil.IsZero() {
		now := e.now()
		if now.Before(ep.openUntil) {
			return rejectedOpen, ep.openUntil.Sub(now), ticket{}
		}
		if ep.probing { // one probe at a time
			return rejectedOpen, e.cooldown / 4, ticket{}
		}
		halfOpen = true
	}
	if ep.inFlight >= e.maxInFlight {
		return rejectedBusy, 0, ticket{}
	}
	ep.inFlight++
	if halfOpen {
		ep.probing = true
	}
	return admitted, 0, ticket{id: id, probe: halfOpen}
}

// verdict is what a finished delivery tells the breaker.
type verdict int

const (
	noVerdict verdict = iota // nothing was sent (e.g. skipped, error before the request)
	succeeded
	failed
)

// release ends a delivery admitted by acquire.
func (e *endpoints) release(t ticket, v verdict) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ep := e.m[t.id]
	if ep == nil {
		return
	}
	if ep.inFlight > 0 {
		ep.inFlight--
	}
	if t.probe {
		ep.probing = false
	}
	switch v {
	case succeeded:
		ep.failures, ep.openUntil = 0, time.Time{}
	case failed:
		ep.failures++
		if t.probe || ep.failures >= e.threshold {
			ep.openUntil = e.now().Add(e.cooldown)
		}
	}
	if ep.inFlight == 0 && ep.failures == 0 && ep.openUntil.IsZero() {
		delete(e.m, t.id) // keep the map small
	}
}

// open reports whether the breaker for id is currently open (tests, metrics).
func (e *endpoints) open(id uuid.UUID) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	ep := e.m[id]
	return ep != nil && !ep.openUntil.IsZero() && e.now().Before(ep.openUntil)
}
