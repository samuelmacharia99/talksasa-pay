package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// apiClient is a small /v1 client for the charges API. The load test keeps
// its own client (rather than the Go SDK) so that it measures raw server
// behaviour and stays independent of SDK changes. It never logs the key.
type apiClient struct {
	base      string // origin, e.g. http://127.0.0.1:8088 (no trailing slash)
	key       string
	hc        *http.Client
	timeout   time.Duration // per attempt
	retries   int           // extra attempts for retryable failures
	onAttempt func(status int, err error)
}

// APIError is a problem response.
type APIError struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("talksasa pay: HTTP %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("talksasa pay: HTTP %d", e.Status)
}

func asAPIError(err error) (*APIError, bool) {
	var ae *APIError
	ok := errors.As(err, &ae)
	return ae, ok
}

func isCode(err error, code string) bool {
	ae, ok := asAPIError(err)
	return ok && ae.Code == code
}

// Resources (only the fields the load test reads).
type (
	WhoAmI struct {
		AppID         string `json:"app_id"`
		AppName       string `json:"app_name"`
		AccountPrefix string `json:"account_prefix"`
		KeyID         string `json:"key_id"`
	}
	ChargeInput struct {
		Reference   string `json:"reference"`
		Amount      int64  `json:"amount"`
		Description string `json:"description,omitempty"`
		Phone       string `json:"phone,omitempty"`
	}
	Charge struct {
		ID            string   `json:"id"`
		Reference     string   `json:"reference"`
		Amount        int64    `json:"amount"`
		AccountNumber string   `json:"account_number"`
		Status        string   `json:"status"`
		TransactionID *string  `json:"transaction_id"`
		LatestSTKPush *STKPush `json:"latest_stk_push"`
	}
	STKPush struct {
		ID         string  `json:"id"`
		ChargeID   string  `json:"charge_id"`
		Status     string  `json:"status"`
		ResultCode *string `json:"result_code"`
	}
	Transaction struct {
		ID                string    `json:"id"`
		Kind              string    `json:"kind"`
		Direction         string    `json:"direction"`
		Channel           string    `json:"channel"`
		Status            string    `json:"status"`
		Amount            int64     `json:"amount"`
		Receipt           *string   `json:"receipt"`
		AccountNumber     *string   `json:"account_number"`
		ChargeID          *string   `json:"charge_id"`
		DuplicateOfCharge *string   `json:"duplicate_of_charge"`
		CreatedAt         time.Time `json:"created_at"`
	}
	Page[T any] struct {
		Data       []T     `json:"data"`
		NextCursor *string `json:"next_cursor"`
	}
	Event struct {
		ID   string          `json:"id"`
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	EventPage struct {
		Data       []Event `json:"data"`
		NextCursor *string `json:"next_cursor"`
		HasMore    bool    `json:"has_more"`
	}
)

func (c *apiClient) whoami(ctx context.Context) (*WhoAmI, error) {
	var w WhoAmI
	return &w, c.do(ctx, http.MethodGet, "/v1/whoami", "", nil, &w)
}

func (c *apiClient) createCharge(ctx context.Context, idemKey string, in ChargeInput) (*Charge, error) {
	var ch Charge
	return &ch, c.do(ctx, http.MethodPost, "/v1/charges", idemKey, in, &ch)
}

func (c *apiClient) getCharge(ctx context.Context, id string) (*Charge, error) {
	var ch Charge
	return &ch, c.do(ctx, http.MethodGet, "/v1/charges/"+url.PathEscape(id), "", nil, &ch)
}

func (c *apiClient) cancelCharge(ctx context.Context, id string) (*Charge, error) {
	var ch Charge
	return &ch, c.do(ctx, http.MethodPost, "/v1/charges/"+url.PathEscape(id)+"/cancel", "", nil, &ch)
}

func (c *apiClient) listCharges(ctx context.Context, status string, limit int) (*Page[Charge], error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if status != "" {
		q.Set("status", status)
	}
	var p Page[Charge]
	return &p, c.do(ctx, http.MethodGet, "/v1/charges?"+q.Encode(), "", nil, &p)
}

func (c *apiClient) getTransaction(ctx context.Context, id string) (*Transaction, error) {
	var t Transaction
	return &t, c.do(ctx, http.MethodGet, "/v1/transactions/"+url.PathEscape(id), "", nil, &t)
}

func (c *apiClient) listTransactions(ctx context.Context, since time.Time, cursor string, limit int) (*Page[Transaction], error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if !since.IsZero() {
		q.Set("since", since.UTC().Format(time.RFC3339))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var p Page[Transaction]
	return &p, c.do(ctx, http.MethodGet, "/v1/transactions?"+q.Encode(), "", nil, &p)
}

func (c *apiClient) listEvents(ctx context.Context, after string, limit int) (*EventPage, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if after != "" {
		q.Set("after", after)
	}
	var p EventPage
	return &p, c.do(ctx, http.MethodGet, "/v1/events?"+q.Encode(), "", nil, &p)
}

// do sends one logical request, retrying retryable failures with the same
// body and Idempotency-Key. Every write in /v1 is idempotent (create and
// stk-push by key, cancel by nature), so retrying is safe.
func (c *apiClient) do(ctx context.Context, method, path, idemKey string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		status, retryAfter, err := c.once(ctx, method, path, idemKey, body, out)
		if c.onAttempt != nil {
			c.onAttempt(status, err)
		}
		if err == nil || attempt >= c.retries || !retryable(status, err) || ctx.Err() != nil {
			return err
		}
		wait := retryAfter
		if wait <= 0 {
			step := min(2*time.Second, 100*time.Millisecond<<attempt)
			wait = time.Duration(mrand.Int64N(int64(step) + 1)) //nolint:gosec // jitter, not security
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

func retryable(status int, err error) bool {
	if status == 0 {
		return !errors.Is(err, context.Canceled)
	}
	if isCode(err, "mpesa_unavailable") || isCode(err, "mpesa_error") {
		return false
	}
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// once performs one attempt and returns the status (0 = no response).
func (c *apiClient) once(ctx context.Context, method, path, idemKey string, body []byte, out any) (int, time.Duration, error) {
	actx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(actx, method, c.base+path, rd)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "talksasa-pay-loadtest")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if actx.Err() != nil && ctx.Err() == nil {
			return 0, 0, fmt.Errorf("%s %s: %w", method, pathOnly(path), context.DeadlineExceeded)
		}
		return 0, 0, fmt.Errorf("%s %s: %w", method, pathOnly(path), stripURL(err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, 0, fmt.Errorf("%s %s: read body: %w", method, pathOnly(path), err)
	}
	if resp.StatusCode >= 300 {
		ae := &APIError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, ae)
		ae.Status = resp.StatusCode
		var ra time.Duration
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			ra = min(time.Duration(s)*time.Second, 30*time.Second)
		}
		return resp.StatusCode, ra, ae
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, 0, fmt.Errorf("%s %s: decode: %w", method, pathOnly(path), err)
		}
	}
	return resp.StatusCode, 0, nil
}

func pathOnly(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// stripURL drops the URL from a *url.Error so messages stay short.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
