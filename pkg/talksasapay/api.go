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

// Whoami returns the app the API key belongs to.
func (c *Client) Whoami(ctx context.Context) (*WhoAmI, error) {
	var out WhoAmI
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/whoami"}, &out); err != nil {
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

func idemKey(given string) (string, error) {
	if given == "" {
		return NewIdempotencyKey(), nil
	}
	if len(given) > 255 {
		return "", invalid("idempotency key longer than 255 characters")
	}
	for _, r := range given {
		if r < 0x20 || r == 0x7f {
			return "", invalid("idempotency key contains control characters")
		}
	}
	return given, nil
}

func checkPhone(phone string) (string, error) {
	phone = strings.TrimSpace(phone)
	if phone == "" || len(phone) > 20 {
		return "", invalid("phone must be 1-20 characters")
	}
	return phone, nil
}

// CreateCharge creates a pending charge for exactly in.Amount KES. The
// result carries CheckoutURL (a hosted pay page) and Paybill (manual M-Pesa
// instructions). With in.Phone the first prompt is sent at once and
// returned as LatestSTKPush; the charge is created even if that prompt
// fails (LatestSTKPush is then the failed push, or nil when M-Pesa is
// unavailable).
//
// The call always carries an Idempotency-Key (in.IdempotencyKey, or a
// random one per call) and its automatic retries reuse it, so one call
// creates at most one charge. replayed is true when the server returned the
// charge of an earlier call with the same key. A charge with the same
// reference fails with duplicate_reference.
func (c *Client) CreateCharge(ctx context.Context, in ChargeInput) (charge *Charge, replayed bool, err error) {
	key, err := idemKey(in.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	if in.Phone != "" {
		if in.Phone, err = checkPhone(in.Phone); err != nil {
			return nil, false, err
		}
	}
	if in.ExpiresIn%time.Second != 0 {
		return nil, false, invalid("ExpiresIn must be whole seconds")
	}
	var out Charge
	resp, err := c.do(ctx, request{method: http.MethodPost, path: "/charges", body: in, idemKey: key}, &out)
	if err != nil {
		return nil, false, err
	}
	return &out, resp.status == http.StatusOK || resp.header.Get("Idempotent-Replayed") == "true", nil
}

// GetCharge returns one charge with its latest prompt.
func (c *Client) GetCharge(ctx context.Context, id string) (*Charge, error) {
	if err := requireID("id", id); err != nil {
		return nil, err
	}
	var out Charge
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/charges/" + pathSegment(id)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CancelCharge cancels a pending charge (charge.cancelled). Idempotent
// (cancelling a cancelled charge returns it), so it is retried like a GET.
// A charge that is not pending fails with not_cancellable. A prompt already
// on the phone is not withdrawn: if the customer still pays, the charge
// succeeds (late money is never dropped).
func (c *Client) CancelCharge(ctx context.Context, id string) (*Charge, error) {
	if err := requireID("id", id); err != nil {
		return nil, err
	}
	var out Charge
	r := request{method: http.MethodPost, path: "/charges/" + pathSegment(id) + "/cancel", idempotent: true}
	if _, err := c.do(ctx, r, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// STKPush sends an M-Pesa payment prompt to phone (07XXXXXXXX, 01XXXXXXXX,
// 2547XXXXXXXX or +2547XXXXXXXX) for the charge's exact amount. replayed is
// true when the server returned the push of an earlier call with the same
// idempotency key.
//
// The call always carries an Idempotency-Key, and its automatic retries
// (network errors, 429, 503 overloaded/busy/timeout) reuse it, so one call
// sends at most one prompt. 409 stk_in_progress / not_payable /
// amount_too_large, 503 mpesa_unavailable and 502 mpesa_error are returned
// at once (check with IsCode). The outcome arrives as stk_push.* events; the
// charge is paid only when charge.succeeded arrives. A failed prompt leaves
// the charge pending: prompt again with a new key.
func (c *Client) STKPush(ctx context.Context, chargeID, phone string, opts STKPushOptions) (push *STKPush, replayed bool, err error) {
	if err := requireID("charge id", chargeID); err != nil {
		return nil, false, err
	}
	phone, err = checkPhone(phone)
	if err != nil {
		return nil, false, err
	}
	key, err := idemKey(opts.IdempotencyKey)
	if err != nil {
		return nil, false, err
	}
	var out STKPush
	r := request{
		method:  http.MethodPost,
		path:    "/charges/" + pathSegment(chargeID) + "/stk-push",
		body:    map[string]string{"phone": phone},
		idemKey: key,
	}
	resp, err := c.do(ctx, r, &out)
	if err != nil {
		return nil, false, err
	}
	return &out, resp.status == http.StatusOK || resp.header.Get("Idempotent-Replayed") == "true", nil
}

// GetSTKPush returns one payment prompt.
func (c *Client) GetSTKPush(ctx context.Context, id string) (*STKPush, error) {
	if err := requireID("id", id); err != nil {
		return nil, err
	}
	var out STKPush
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/stk-pushes/" + pathSegment(id)}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListChargesParams filters ListCharges. Zero values are omitted.
type ListChargesParams struct {
	Status    string // ChargePending ... ChargeReversed
	Reference string // exact reference
	Cursor    string // start after this page cursor
	Limit     int    // page size 1..200 (server default 50)
}

func (p ListChargesParams) query() url.Values {
	q := url.Values{}
	setNonEmpty(q, "status", p.Status)
	setNonEmpty(q, "reference", p.Reference)
	setNonEmpty(q, "cursor", p.Cursor)
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	return q
}

// ListChargesPage returns one page of charges, newest first.
func (c *Client) ListChargesPage(ctx context.Context, p ListChargesParams) (*Page[Charge], error) {
	var out Page[Charge]
	if _, err := c.do(ctx, request{method: http.MethodGet, path: "/charges", query: p.query()}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListCharges iterates over every matching charge, newest first, fetching
// pages as needed. Iteration stops after the first error.
//
//	for ch, err := range c.ListCharges(ctx, talksasapay.ListChargesParams{Status: talksasapay.ChargePending}) {
//		if err != nil { return err }
//		...
//	}
func (c *Client) ListCharges(ctx context.Context, p ListChargesParams) iter.Seq2[Charge, error] {
	return paginate(p.Cursor, func(cursor string) (*Page[Charge], error) {
		p.Cursor = cursor
		return c.ListChargesPage(ctx, p)
	})
}

// GetTransaction returns one transaction.
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
	ChargeID string // only the money for this charge
	Since    time.Time
	Cursor   string
	Limit    int
}

func (p ListTransactionsParams) query() url.Values {
	q := url.Values{}
	setNonEmpty(q, "charge_id", p.ChargeID)
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
