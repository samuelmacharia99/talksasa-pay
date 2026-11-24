package talksasapay

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidArgument is returned (wrapped) for arguments rejected before any
// request is sent.
var ErrInvalidArgument = errors.New("talksasapay: invalid argument")

func invalid(msg string) error {
	return fmt.Errorf("%w: %s", ErrInvalidArgument, msg)
}

// pathSegment percent-encodes s for use as one path segment. Everything but
// RFC 3986 unreserved characters is encoded, and "." / ".." are encoded too
// so no proxy or router can treat them as relative path steps.
func pathSegment(s string) string {
	if s == "." || s == ".." {
		return strings.Repeat("%2E", len(s))
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		const hexd = "0123456789ABCDEF"
		b.WriteByte('%')
		b.WriteByte(hexd[c>>4])
		b.WriteByte(hexd[c&15])
	}
	return b.String()
}

func requireID(what, v string) error {
	if v == "" {
		return invalid(what + " is required")
	}
	return nil
}

// Whoami returns the product the API key belongs to.
func (c *Client) Whoami(ctx context.Context) (*WhoAmI, error) {
	var out WhoAmI
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/whoami"}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpsertPayer creates or updates the payer keyed by your externalRef.
// created is true when the payer was new (201). Safe to retry.
func (c *Client) UpsertPayer(ctx context.Context, externalRef string, in PayerInput) (payer *Payer, created bool, err error) {
	if err := requireID("external_ref", externalRef); err != nil {
		return nil, false, err
	}
	if in.Phones == nil {
		in.Phones = []string{}
	}
	var out Payer
	resp, err := c.do(ctx, request{method: http.MethodPut, path: "/payers/" + pathSegment(externalRef), body: in}, &out)
	if err != nil {
		return nil, false, err
	}
	return &out, resp.status == http.StatusCreated, nil
}

// GetPayer returns a payer, including its credit balance.
func (c *Client) GetPayer(ctx context.Context, externalRef string) (*Payer, error) {
	if err := requireID("external_ref", externalRef); err != nil {
		return nil, err
	}
	var out Payer
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/payers/" + pathSegment(externalRef)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// NewIdempotencyKey returns a random key suitable for Idempotency-Key.
func NewIdempotencyKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return "tspgo_" + hex.EncodeToString(b[:])
}

// CreatePaymentRequest asks a payer to pay. replayed is true when the server
// returned an earlier identical request for the same idempotency key (200).
// Any credit the payer holds is applied at once, so the result may already
// be partially_paid or paid.
func (c *Client) CreatePaymentRequest(ctx context.Context, in PaymentRequestInput) (pr *PaymentRequest, replayed bool, err error) {
	key := in.IdempotencyKey
	if key == "" {
		key = NewIdempotencyKey()
	}
	if len(key) > 255 {
		return nil, false, invalid("idempotency key longer than 255 characters")
	}
	var out PaymentRequest
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/payment-requests", body: in, idemKey: key}, &out)
	if err != nil {
		return nil, false, err
	}
	return &out, resp.status == http.StatusOK || resp.header.Get("Idempotent-Replayed") == "true", nil
}

// GetPaymentRequest returns one payment request.
func (c *Client) GetPaymentRequest(ctx context.Context, id string) (*PaymentRequest, error) {
	if err := requireID("id", id); err != nil {
		return nil, err
	}
	var out PaymentRequest
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/payment-requests/" + pathSegment(id)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelPaymentRequest cancels an open request with nothing paid on it.
// Idempotent (cancelling a cancelled request returns it), so it is retried
// like a GET. A request with money on it fails with not_cancellable.
func (c *Client) CancelPaymentRequest(ctx context.Context, id string) (*PaymentRequest, error) {
	if err := requireID("id", id); err != nil {
		return nil, err
	}
	var out PaymentRequest
	r := request{method: http.MethodPost, path: "/payment-requests/" + pathSegment(id) + "/cancel", idempotent: true}
	if _, err := c.do(ctx, r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPaymentRequestsParams filters ListPaymentRequests. Zero values are omitted.
type ListPaymentRequestsParams struct {
	Status    string // open | partially_paid | paid | cancelled | expired
	Reference string
	Payer     string // payer external_ref
	Cursor    string // start after this page cursor
	Limit     int    // page size 1..200 (server default 50)
}

func (p ListPaymentRequestsParams) query() url.Values {
	q := url.Values{}
	setNonEmpty(q, "status", p.Status)
	setNonEmpty(q, "reference", p.Reference)
	setNonEmpty(q, "payer", p.Payer)
	setNonEmpty(q, "cursor", p.Cursor)
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q
}

// ListPaymentRequestsPage returns one page, newest first.
func (c *Client) ListPaymentRequestsPage(ctx context.Context, p ListPaymentRequestsParams) (*Page[PaymentRequest], error) {
	var out Page[PaymentRequest]
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/payment-requests", query: p.query()}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListPaymentRequests iterates over every matching request, newest first,
// fetching pages as needed. Iteration stops after the first error.
//
//	for pr, err := range c.ListPaymentRequests(ctx, talksasapay.ListPaymentRequestsParams{Status: "open"}) {
//		if err != nil { return err }
//		...
//	}
func (c *Client) ListPaymentRequests(ctx context.Context, p ListPaymentRequestsParams) iter.Seq2[PaymentRequest, error] {
	return paginate(p.Cursor, func(cursor string) (*Page[PaymentRequest], error) {
		p.Cursor = cursor
		return c.ListPaymentRequestsPage(ctx, p)
	})
}

// GetTransaction returns one transaction and where its money went.
func (c *Client) GetTransaction(ctx context.Context, id string) (*Transaction, error) {
	if err := requireID("id", id); err != nil {
		return nil, err
	}
	var out Transaction
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/transactions/" + pathSegment(id)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTransactionsParams filters ListTransactions. Zero values are omitted.
type ListTransactionsParams struct {
	Payer  string // payer external_ref
	Since  time.Time
	Cursor string
	Limit  int
}

func (p ListTransactionsParams) query() url.Values {
	q := url.Values{}
	setNonEmpty(q, "payer", p.Payer)
	if !p.Since.IsZero() {
		q.Set("since", p.Since.UTC().Format(time.RFC3339Nano))
	}
	setNonEmpty(q, "cursor", p.Cursor)
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q
}

// ListTransactionsPage returns one page, newest first.
func (c *Client) ListTransactionsPage(ctx context.Context, p ListTransactionsParams) (*Page[Transaction], error) {
	var out Page[Transaction]
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/transactions", query: p.query()}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTransactions iterates over every matching transaction, newest first.
func (c *Client) ListTransactions(ctx context.Context, p ListTransactionsParams) iter.Seq2[Transaction, error] {
	return paginate(p.Cursor, func(cursor string) (*Page[Transaction], error) {
		p.Cursor = cursor
		return c.ListTransactionsPage(ctx, p)
	})
}

// ListEventsParams selects a page of the events feed.
type ListEventsParams struct {
	After string // cursor from the previous page's NextCursor; "" = from the beginning
	Limit int
}

// ListEvents returns one page of the gap-free events feed, oldest first.
// See EventStream for a ready-made poller.
func (c *Client) ListEvents(ctx context.Context, p ListEventsParams) (*EventPage, error) {
	q := url.Values{}
	setNonEmpty(q, "after", p.After)
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	var out EventPage
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/events", query: q}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func setNonEmpty(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

// paginate yields every item of every page starting at cursor.
func paginate[T any](cursor string, fetch func(cursor string) (*Page[T], error)) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		seen := map[string]bool{}
		for {
			page, err := fetch(cursor)
			if err != nil {
				var zero T
				yield(zero, err)
				return
			}
			for _, item := range page.Data {
				if !yield(item, nil) {
					return
				}
			}
			if page.NextCursor == nil || *page.NextCursor == "" || len(page.Data) == 0 {
				return
			}
			if seen[*page.NextCursor] { // a misbehaving server must not loop us forever
				var zero T
				yield(zero, errors.New("talksasapay: server repeated a page cursor"))
				return
			}
			seen[*page.NextCursor] = true
			cursor = *page.NextCursor
		}
	}
}
