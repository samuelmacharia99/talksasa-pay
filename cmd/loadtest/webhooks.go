package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/talksasa/talksasa-pay/pkg/talksasapay"
)

// receiver counts verified webhook deliveries per event id.
type receiver struct {
	mu        sync.Mutex
	counts    map[string]int
	types     map[string]string
	rejected  map[string]int // reason -> count
	delivered int
}

func newReceiver() *receiver {
	return &receiver{counts: map[string]int{}, types: map[string]string{}, rejected: map[string]int{}}
}

func (rc *receiver) handler(secret []byte) http.Handler {
	return talksasapay.WebhookHandler(secret, func(_ context.Context, ev talksasapay.Event) error {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.counts[ev.ID]++
		rc.types[ev.ID] = ev.Type
		rc.delivered++
		return nil
	}, talksasapay.WithWebhookErrorHook(func(_ *http.Request, err error) {
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.rejected[rejectReason(err)]++
	}))
}

func rejectReason(err error) string {
	for _, c := range []struct {
		err  error
		name string
	}{
		{talksasapay.ErrWebhookNoSignature, "no_signature"},
		{talksasapay.ErrWebhookMalformedSignature, "malformed_signature"},
		{talksasapay.ErrWebhookTimestamp, "timestamp"},
		{talksasapay.ErrWebhookSignatureMismatch, "signature_mismatch"},
		{talksasapay.ErrWebhookBadPayload, "bad_payload"},
	} {
		if errors.Is(err, c.err) {
			return c.name
		}
	}
	return "other"
}

// feedTail returns the cursor at the end of the events feed (start of
// this run's events).
func feedTail(ctx context.Context, c *talksasapay.Client) (string, error) {
	cursor := ""
	for {
		page, err := c.ListEvents(ctx, talksasapay.ListEventsParams{After: cursor, Limit: 200})
		if err != nil {
			return "", err
		}
		if page.NextCursor != nil {
			cursor = *page.NextCursor
		}
		if !page.HasMore || len(page.Data) == 0 {
			return cursor, nil
		}
	}
}

// feedSince returns the ids of all events after cursor.
func feedSince(ctx context.Context, c *talksasapay.Client, cursor string) ([]string, error) {
	var ids []string
	for {
		page, err := c.ListEvents(ctx, talksasapay.ListEventsParams{After: cursor, Limit: 200})
		if err != nil {
			return nil, err
		}
		for _, ev := range page.Data {
			ids = append(ids, ev.ID)
		}
		if page.NextCursor != nil {
			cursor = *page.NextCursor
		}
		if !page.HasMore || len(page.Data) == 0 {
			return ids, nil
		}
	}
}

// webhookResult compares deliveries with the feed.
type webhookResult struct {
	feed, delivered, unique, duplicates, missing, unexpected, rejected int
}

// reconcile waits up to grace for every feed event to be delivered, then
// reports. feedIDs is re-read on each check so late events are included.
func (rc *receiver) reconcile(ctx context.Context, w io.Writer, readFeed func(context.Context) ([]string, error), grace time.Duration) (webhookResult, error) {
	deadline := time.Now().Add(grace)
	var ids []string
	for {
		var err error
		if ids, err = readFeed(ctx); err != nil {
			return webhookResult{}, err
		}
		rc.mu.Lock()
		missing := 0
		for _, id := range ids {
			if rc.counts[id] == 0 {
				missing++
			}
		}
		rc.mu.Unlock()
		if missing == 0 || time.Now().After(deadline) {
			break
		}
		t := time.NewTimer(min(time.Second, time.Until(deadline)+time.Millisecond))
		select {
		case <-ctx.Done():
			t.Stop()
			return webhookResult{}, ctx.Err()
		case <-t.C:
		}
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	res := webhookResult{feed: len(ids), delivered: rc.delivered, unique: len(rc.counts)}
	inFeed := map[string]bool{}
	var missingIDs, dupLines []string
	for _, id := range ids {
		inFeed[id] = true
		if rc.counts[id] == 0 {
			res.missing++
			missingIDs = append(missingIDs, id)
		}
	}
	for id, n := range rc.counts {
		if n > 1 {
			res.duplicates += n - 1
			dupLines = append(dupLines, fmt.Sprintf("  %s %s x%d", id, rc.types[id], n))
		}
		if !inFeed[id] {
			res.unexpected++
		}
	}
	for _, n := range rc.rejected {
		res.rejected += n
	}
	fmt.Fprintf(w, "\nwebhooks: feed events %d, deliveries %d (unique %d), duplicate deliveries %d, missing %d, not in this run's feed %d, rejected %d\n",
		res.feed, res.delivered, res.unique, res.duplicates, res.missing, res.unexpected, res.rejected)
	for reason, n := range rc.rejected {
		fmt.Fprintf(w, "  rejected %s: %d\n", reason, n)
	}
	sort.Strings(dupLines)
	for i, l := range dupLines {
		if i == 20 {
			fmt.Fprintf(w, "  ... %d more duplicated ids\n", len(dupLines)-20)
			break
		}
		fmt.Fprintln(w, l)
	}
	for i, id := range missingIDs {
		if i == 20 {
			fmt.Fprintf(w, "  ... %d more missing ids\n", len(missingIDs)-20)
			break
		}
		fmt.Fprintf(w, "  missing %s\n", id)
	}
	return res, nil
}
