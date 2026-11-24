// Package talksasapay is the Go client for the Talksasa Pay product API
// (/v1, api/product.openapi.yaml). It depends only on the standard library.
//
//	c, err := talksasapay.New("https://pay-api.talksasa.co.ke", key)
//	payer, created, err := c.UpsertPayer(ctx, "cust-42", talksasapay.PayerInput{DisplayName: "Jane"})
//	pr, _, err := c.CreatePaymentRequest(ctx, talksasapay.PaymentRequestInput{
//		Payer: "cust-42", Reference: "INV-1001", Description: "Hosting", Amount: 1500,
//		IdempotencyKey: "INV-1001-v1",
//	})
//
// Retries: requests that are safe to repeat (GET, PUT, POST with an
// Idempotency-Key, and cancel, which is idempotent by contract) are retried
// on network errors and on 429/502/503/504 with exponential backoff and
// jitter, honouring Retry-After. Other POSTs are never retried.
//
// Errors returned by the API are *APIError; use IsCode / AsAPIError.
// Webhooks: VerifyWebhook and WebhookHandler.
package talksasapay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Version is the client version reported in the User-Agent.
const Version = "0.1.0"

const (
	defaultTimeout     = 30 * time.Second
	defaultMaxRetries  = 3
	defaultBackoffBase = 250 * time.Millisecond
	defaultBackoffMax  = 8 * time.Second
	// maxRetryAfter caps how long a single Retry-After is honoured; a longer
	// one ends the retries and returns the error to the caller.
	maxRetryAfter = 60 * time.Second
	// maxResponseBytes bounds how much of a response body is read.
	maxResponseBytes = 8 << 20
)

// LogEvent describes one HTTP attempt. It never contains the API key or
// request/response bodies.
type LogEvent struct {
	Method    string
	Path      string // escaped path, without query string
	Attempt   int    // 1-based
	Status    int    // 0 on network error
	Duration  time.Duration
	Err       error         // network error or decoded *APIError, nil on success
	RetryIn   time.Duration // >0 when another attempt will follow
	RequestID string        // X-Request-Id of the response, if any
}

// Logger receives a LogEvent after every HTTP attempt.
type Logger func(LogEvent)

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client (transport, proxies, TLS).
// The client is not modified.
func WithHTTPClient(hc *http.Client) Option { return func(c *Client) { c.hc = hc } }

// WithTimeout bounds each attempt (not the total including retries; use the
// context for that). Default 30 s; <=0 disables.
func WithTimeout(d time.Duration) Option { return func(c *Client) { c.timeout = d } }

// WithMaxRetries sets how many times a retryable request is retried after the
// first attempt. Default 3; 0 disables retries.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n < 0 {
			n = 0
		}
		c.maxRetries = n
	}
}

// WithRetryBackoff sets the exponential backoff base and cap (defaults 250 ms
// and 8 s). Each wait is drawn uniformly from [d/2, d] where
// d = min(max, base*2^retry).
func WithRetryBackoff(base, maxWait time.Duration) Option {
	return func(c *Client) {
		if base > 0 {
			c.backoffBase = base
		}
		if maxWait > 0 {
			c.backoffMax = maxWait
		}
	}
}

// WithUserAgent appends suffix (e.g. "cloud-hosting/3.2") to the User-Agent.
func WithUserAgent(suffix string) Option { return func(c *Client) { c.uaSuffix = suffix } }

// WithLogger installs a hook called after every HTTP attempt.
func WithLogger(l Logger) Option { return func(c *Client) { c.logger = l } }

// Client calls the Talksasa Pay product API. It is safe for concurrent use.
type Client struct {
	base        *url.URL // ends in /v1, no trailing slash
	apiKey      string
	hc          *http.Client
	timeout     time.Duration
	maxRetries  int
	backoffBase time.Duration
	backoffMax  time.Duration
	uaSuffix    string
	logger      Logger
	sleep       func(context.Context, time.Duration) error // replaced in tests
}

// New returns a client for baseURL, the API origin such as
// "https://pay-api.talksasa.co.ke" ("/v1" is appended unless baseURL
// already ends with it).
func New(baseURL, apiKey string, opts ...Option) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("talksasapay: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("talksasapay: base URL must be an absolute http(s) URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("talksasapay: base URL must not have a query or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	if !strings.HasSuffix(u.Path, "/v1") {
		u.Path += "/v1"
	}
	if apiKey == "" || strings.ContainsAny(apiKey, " \t\r\n") {
		return nil, errors.New("talksasapay: API key is empty or contains whitespace")
	}
	c := &Client{
		base: u, apiKey: apiKey, hc: http.DefaultClient, timeout: defaultTimeout,
		maxRetries: defaultMaxRetries, backoffBase: defaultBackoffBase, backoffMax: defaultBackoffMax,
		sleep: sleepCtx,
	}
	for _, o := range opts {
		o(c)
	}
	if c.hc == nil {
		c.hc = http.DefaultClient
	}
	return c, nil
}

// String never reveals the API key.
func (c *Client) String() string { return "talksasapay.Client(" + c.base.String() + ")" }

func (c *Client) userAgent() string {
	ua := "talksasa-pay-go/" + Version
	if c.uaSuffix != "" {
		ua += " " + c.uaSuffix
	}
	return ua
}

// request is one logical API call.
type request struct {
	method     string
	path       string // already escaped, relative to /v1, starting with "/"
	query      url.Values
	body       any
	idemKey    string
	idempotent bool // safe to retry (in addition to GET/PUT)
}

// response is what a successful call returns besides the decoded body.
type response struct {
	status int
	header http.Header
}

func (r *request) retryable() bool {
	switch r.method {
	case http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions:
		return true
	}
	return r.idempotent || r.idemKey != ""
}

func retryableStatus(s int) bool {
	return s == http.StatusTooManyRequests || s == http.StatusBadGateway ||
		s == http.StatusServiceUnavailable || s == http.StatusGatewayTimeout
}

// do performs r, retrying when safe, and decodes a 2xx JSON body into out.
func (c *Client) do(ctx context.Context, r request, out any) (*response, error) {
	var payload []byte
	if r.body != nil {
		var err error
		if payload, err = json.Marshal(r.body); err != nil {
			return nil, fmt.Errorf("talksasapay: encode request: %w", err)
		}
	}
	path := c.base.EscapedPath() + r.path
	full := c.base.Scheme + "://" + c.base.Host + path
	if len(r.query) > 0 {
		full += "?" + r.query.Encode()
	}
	for attempt := 1; ; attempt++ {
		start := time.Now()
		resp, ae := c.attempt(ctx, r, full, payload, out)
		ev := LogEvent{Method: r.method, Path: path, Attempt: attempt, Duration: time.Since(start)}
		if resp != nil {
			ev.Status, ev.RequestID = resp.status, resp.header.Get("X-Request-Id")
		}
		if ae == nil {
			c.log(ev)
			return resp, nil
		}
		ev.Status, ev.Err = ae.status, ae.err
		if ae.header != nil {
			ev.RequestID = ae.header.Get("X-Request-Id")
		}
		wait, retry := c.shouldRetry(ctx, r, attempt, ae)
		if retry {
			ev.RetryIn = wait
		}
		c.log(ev)
		if !retry {
			return nil, ae.err
		}
		if c.sleep(ctx, wait) != nil {
			return nil, ae.err // context ended while waiting: report the last real error
		}
	}
}

func (c *Client) log(ev LogEvent) {
	if c.logger != nil {
		c.logger(ev)
	}
}

// attemptError is a failed attempt plus what the retry decision needs.
type attemptError struct {
	err       error
	status    int  // 0 when no response was received
	transient bool // network/transport failure (request may or may not have arrived)
	header    http.Header
}

func (c *Client) attempt(ctx context.Context, r request, full string, payload []byte, out any) (*response, *attemptError) {
	actx := ctx
	if c.timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(actx, r.method, full, body)
	if err != nil {
		return nil, &attemptError{err: fmt.Errorf("talksasapay: build request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent())
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.idemKey != "" {
		req.Header.Set("Idempotency-Key", r.idemKey)
	}
	label := r.method + " " + req.URL.EscapedPath()
	hr, err := c.hc.Do(req)
	if err != nil {
		return nil, &attemptError{err: fmt.Errorf("talksasapay: %s: %w", label, stripURL(err)), transient: true}
	}
	defer func() { _ = hr.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(hr.Body, maxResponseBytes))
	if err != nil {
		return nil, &attemptError{err: fmt.Errorf("talksasapay: %s: read response: %w", label, err),
			status: hr.StatusCode, transient: true, header: hr.Header}
	}
	if hr.StatusCode < 200 || hr.StatusCode > 299 {
		return nil, &attemptError{err: decodeAPIError(hr, raw), status: hr.StatusCode, header: hr.Header}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, &attemptError{err: fmt.Errorf("talksasapay: %s: decode %d response: %w", label, hr.StatusCode, err),
				status: hr.StatusCode, header: hr.Header}
		}
	}
	return &response{status: hr.StatusCode, header: hr.Header}, nil
}

// stripURL drops the *url.Error wrapper (its message repeats the full URL,
// query included) while keeping the underlying cause.
func stripURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// shouldRetry decides whether attempt (1-based) is followed by another and
// how long to wait first.
func (c *Client) shouldRetry(ctx context.Context, r request, attempt int, ae *attemptError) (time.Duration, bool) {
	if attempt > c.maxRetries || !r.retryable() || ctx.Err() != nil {
		return 0, false
	}
	if !ae.transient && !retryableStatus(ae.status) {
		return 0, false
	}
	wait := c.backoff(attempt)
	var apiErr *APIError
	if errors.As(ae.err, &apiErr) && apiErr.RetryAfter > 0 {
		if apiErr.RetryAfter > maxRetryAfter {
			return 0, false
		}
		wait = apiErr.RetryAfter
	}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
		return 0, false // would wake up after the caller gave up
	}
	return wait, true
}

// backoff returns the wait after the given failed attempt (1-based): equal
// jitter over an exponentially growing, capped window.
func (c *Client) backoff(attempt int) time.Duration {
	d := c.backoffBase
	for i := 1; i < attempt && d < c.backoffMax; i++ {
		d *= 2
	}
	if d > c.backoffMax {
		d = c.backoffMax
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1)) //nolint:gosec // jitter, not a secret
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseRetryAfter reads a Retry-After header (seconds or HTTP date).
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}
