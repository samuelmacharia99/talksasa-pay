package talksasapay

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// feedServer serves a fixed list of events with cursors "c<n>" = after n.
func feedServer(t *testing.T, events []map[string]any, failFirst int) *fake {
	var mu sync.Mutex
	fails := failFirst
	return newFake(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if fails > 0 {
			fails--
			mu.Unlock()
			problem(500, CodeInternalError, nil)(w, r)
			return
		}
		mu.Unlock()
		start := 0
		if a := r.URL.Query().Get("after"); a != "" {
			_, _ = fmt.Sscanf(a, "c%d", &start)
		}
		limit := 2
		end := min(start+limit, len(events))
		data := events[start:end]
		var next any
		if len(data) > 0 {
			next = fmt.Sprintf("c%d", end)
		}
		jsonResp(200, map[string]any{"data": data, "next_cursor": next, "has_more": end < len(events)})(w, r)
	})
}

func mkEvents(n int) []map[string]any {
	var out []map[string]any
	for i := range n {
		out = append(out, map[string]any{
			"object": "event", "id": fmt.Sprintf("ev-%d", i), "type": EventChargeSucceeded, "api_version": "2026-11-25",
			"created_at": "2026-11-24T10:00:00Z", "data": sampleCharge,
		})
	}
	return out
}

func TestEventStreamPoll(t *testing.T) {
	f := feedServer(t, mkEvents(5), 0)
	c, _ := newTestClient(t, f)
	cursor := ""
	var saves []string
	var got []string
	s := &EventStream{
		Client:     c,
		LoadCursor: func(context.Context) (string, error) { return cursor, nil },
		SaveCursor: func(_ context.Context, cur string) error { cursor = cur; saves = append(saves, cur); return nil },
		Handle: func(_ context.Context, ev Event) error {
			ch, err := ev.Charge()
			if err != nil || ch.Amount != 1500 {
				return fmt.Errorf("decode: %w", err)
			}
			if _, err := ev.Transaction(); err == nil {
				return errors.New("decoded the wrong type")
			}
			got = append(got, ev.ID)
			return nil
		},
		PageSize: 2,
	}
	n, err := s.Poll(context.Background())
	if err != nil || n != 5 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if strings.Join(got, ",") != "ev-0,ev-1,ev-2,ev-3,ev-4" || strings.Join(saves, ",") != "c2,c4,c5" {
		t.Fatalf("got %v saves %v", got, saves)
	}
	if q := f.requests()[0].RawQuery; q != "limit=2" {
		t.Fatalf("first query %s", q)
	}
	if q := f.requests()[1].RawQuery; q != "after=c2&limit=2" {
		t.Fatalf("second query %s", q)
	}
	// Drained: the next poll sees an empty page (null cursor) and keeps the cursor.
	n, err = s.Poll(context.Background())
	if err != nil || n != 0 || cursor != "c5" || len(saves) != 3 {
		t.Fatalf("n=%d err=%v cursor=%s", n, err, cursor)
	}
}

func TestEventStreamHandlerErrorKeepsCursor(t *testing.T) {
	f := feedServer(t, mkEvents(4), 0)
	c, _ := newTestClient(t, f)
	cursor := ""
	boom := errors.New("db down")
	calls := 0
	s := &EventStream{
		Client:     c,
		LoadCursor: func(context.Context) (string, error) { return cursor, nil },
		SaveCursor: func(_ context.Context, cur string) error { cursor = cur; return nil },
		Handle: func(_ context.Context, ev Event) error {
			calls++
			if ev.ID == "ev-3" {
				return boom
			}
			return nil
		},
		PageSize: 2,
	}
	if err := s.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("Run err=%v", err)
	}
	if cursor != "c2" || calls != 4 {
		t.Fatalf("cursor=%s calls=%d (page with the failed event must be redelivered)", cursor, calls)
	}
}

func TestEventStreamRunRecoversFromAPIErrors(t *testing.T) {
	f := feedServer(t, mkEvents(3), 1)
	c, _ := newTestClient(t, f, WithMaxRetries(0))
	var mu sync.Mutex
	cursor := ""
	var apiErrs int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	s := &EventStream{
		Client:     c,
		LoadCursor: func(context.Context) (string, error) { mu.Lock(); defer mu.Unlock(); return cursor, nil },
		SaveCursor: func(_ context.Context, cur string) error { mu.Lock(); defer mu.Unlock(); cursor = cur; return nil },
		Handle: func(context.Context, Event) error {
			seen++
			if seen == 3 {
				cancel()
			}
			return nil
		},
		PollInterval: time.Millisecond, ErrorBackoff: time.Millisecond,
		OnError: func(err error) {
			if !IsCode(err, CodeInternalError) {
				t.Errorf("unexpected %v", err)
			}
			apiErrs++
		},
	}
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err=%v", err)
	}
	if apiErrs != 1 || seen != 3 {
		t.Fatalf("apiErrs=%d seen=%d", apiErrs, seen)
	}
	if err := (&EventStream{}).Run(ctx); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("incomplete stream: %v", err)
	}
}
