package talksasapay

import (
	"context"
	"errors"
	"time"
)

// EventStream polls the gap-free events feed and hands each event to
// Handle, persisting the cursor through SaveCursor after every fully
// processed page. Delivery is at least once: if Handle or SaveCursor fails,
// the page is fetched again next time, so Handle must be idempotent
// (de-duplicate by Event.ID).
type EventStream struct {
	Client *Client
	// LoadCursor returns the stored cursor ("" = start of the feed).
	LoadCursor func(ctx context.Context) (string, error)
	// SaveCursor stores cursor after a page has been handled.
	SaveCursor func(ctx context.Context, cursor string) error
	// Handle processes one event. Returning an error stops Run.
	Handle func(ctx context.Context, ev Event) error
	// PageSize is the limit per request (default 100).
	PageSize int
	// PollInterval is the wait when the feed is drained (default 5 s).
	PollInterval time.Duration
	// ErrorBackoff is the wait after an API error (default 10 s). API errors
	// do not stop Run; they are reported to OnError.
	ErrorBackoff time.Duration
	// OnError is told about API errors Run recovers from (optional).
	OnError func(err error)
}

// errStop wraps handler/cursor failures, which end Run.
type errStop struct{ err error }

func (e errStop) Error() string { return e.err.Error() }
func (e errStop) Unwrap() error { return e.err }

// Poll drains the feed once: it fetches pages until has_more is false and
// returns the number of events handled.
func (s *EventStream) Poll(ctx context.Context) (int, error) {
	n, err := s.poll(ctx)
	var st errStop
	if errors.As(err, &st) {
		return n, st.err
	}
	return n, err
}

func (s *EventStream) poll(ctx context.Context) (int, error) {
	if s.Client == nil || s.Handle == nil || s.LoadCursor == nil || s.SaveCursor == nil {
		return 0, errStop{invalid("EventStream needs Client, LoadCursor, SaveCursor and Handle")}
	}
	cursor, err := s.LoadCursor(ctx)
	if err != nil {
		return 0, errStop{err}
	}
	limit := s.PageSize
	if limit <= 0 {
		limit = 100
	}
	handled := 0
	for {
		page, err := s.Client.ListEvents(ctx, ListEventsParams{After: cursor, Limit: limit})
		if err != nil {
			return handled, err
		}
		for _, ev := range page.Data {
			if err := s.Handle(ctx, ev); err != nil {
				return handled, errStop{err}
			}
			handled++
		}
		if page.NextCursor != nil && *page.NextCursor != "" && *page.NextCursor != cursor {
			cursor = *page.NextCursor
			if err := s.SaveCursor(ctx, cursor); err != nil {
				return handled, errStop{err}
			}
		}
		if !page.HasMore || len(page.Data) == 0 {
			return handled, nil
		}
	}
}

// Run polls until ctx ends (returning ctx.Err()) or Handle, LoadCursor or
// SaveCursor fails (returning that error). API errors are retried after
// ErrorBackoff.
func (s *EventStream) Run(ctx context.Context) error {
	interval := s.PollInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	backoff := s.ErrorBackoff
	if backoff <= 0 {
		backoff = 10 * time.Second
	}
	for {
		_, err := s.poll(ctx)
		wait := interval
		if err != nil {
			var st errStop
			if errors.As(err, &st) {
				return st.err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if s.OnError != nil {
				s.OnError(err)
			}
			wait = backoff
		}
		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}
