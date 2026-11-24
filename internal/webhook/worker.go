package webhook

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/queue"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// workerActor is recorded on the worker's write transactions.
const workerActor = "system:webhook-worker"

// Defaults for Options.
const (
	DefaultBreakerThreshold      = 5
	DefaultBreakerCooldown       = time.Minute
	DefaultMaxInFlightPerProduct = 10

	maxStoredBody  = 1024 // characters of response body kept per attempt
	maxStoredError = 500
)

// SecretSource returns a product's current and (within the rotation grace)
// previous webhook secret. *product.Service implements it.
type SecretSource interface {
	WebhookSecrets(ctx context.Context, productID uuid.UUID) (current, previous []byte, err error)
}

var _ SecretSource = (*product.Service)(nil)

// Options configures the Worker.
type Options struct {
	Pool    *db.Pool
	Secrets SecretSource // the product service
	Logger  *slog.Logger
	// Client must come from NewHTTPClient (SSRF protection lives there).
	Client *http.Client
	// Env: plain http:// webhook URLs are only delivered in development.
	Env config.Environment
	// Timeout is the per-attempt HTTP timeout (cfg.Worker.WebhookTimeout);
	// the River job timeout is this plus a margin for database work.
	Timeout time.Duration

	BreakerThreshold      int           // consecutive failures that open the breaker (default 5)
	BreakerCooldown       time.Duration // how long it stays open (default 1 min)
	MaxInFlightPerProduct int           // concurrent deliveries per product (default 10)

	// Backoff overrides the retry delay (tests); nil uses Backoff.
	Backoff func(attempt int) time.Duration
	// Now overrides the clock (tests); nil uses time.Now.
	Now func() time.Time
}

// Worker delivers one event per River job. Register it with
// river.AddWorker(workers, w) on the webhooks queue.
type Worker struct {
	river.WorkerDefaults[queue.WebhookArgs]

	pool       *db.Pool
	secrets    SecretSource
	logger     *slog.Logger
	client     *http.Client
	allowHTTP  bool
	timeout    time.Duration
	backoff    func(int) time.Duration
	now        func() time.Time
	endpoints  *endpoints
	retryAfter sync.Map // River job id -> earliest next attempt (from Retry-After)
}

// NewWorker builds the webhook worker.
func NewWorker(o Options) (*Worker, error) {
	if o.Pool == nil || o.Secrets == nil || o.Client == nil || o.Logger == nil {
		return nil, errors.New("webhook: Pool, Secrets, Client and Logger are required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.BreakerThreshold <= 0 {
		o.BreakerThreshold = DefaultBreakerThreshold
	}
	if o.BreakerCooldown <= 0 {
		o.BreakerCooldown = DefaultBreakerCooldown
	}
	if o.MaxInFlightPerProduct <= 0 {
		o.MaxInFlightPerProduct = DefaultMaxInFlightPerProduct
	}
	if o.Backoff == nil {
		o.Backoff = Backoff
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Worker{
		pool: o.Pool, secrets: o.Secrets, logger: o.Logger, client: o.Client,
		allowHTTP: o.Env == config.EnvDevelopment, timeout: o.Timeout, backoff: o.Backoff, now: o.Now,
		endpoints: newEndpoints(o.BreakerThreshold, o.BreakerCooldown, o.MaxInFlightPerProduct, o.Now),
	}, nil
}

// Timeout implements river.Worker: the HTTP timeout plus room for the
// database reads and the attempt record.
func (w *Worker) Timeout(*river.Job[queue.WebhookArgs]) time.Duration {
	return w.timeout + 30*time.Second
}

// NextRetry implements river.Worker: full-jitter exponential backoff (see
// Backoff), but never earlier than a receiver's Retry-After.
func (w *Worker) NextRetry(job *river.Job[queue.WebhookArgs]) time.Time {
	next := w.now().Add(w.backoff(job.Attempt))
	if v, ok := w.retryAfter.LoadAndDelete(job.ID); ok {
		if t, _ := v.(time.Time); t.After(next) {
			next = t
		}
	}
	return next
}

// Work implements river.Worker.
func (w *Worker) Work(ctx context.Context, job *river.Job[queue.WebhookArgs]) error {
	args := job.Args
	q := store.New(w.pool)
	log := w.logger.With(slog.String("event_id", args.EventID.String()), slog.String("product_id", args.ProductID.String()),
		slog.Int("job_attempt", job.Attempt))

	ev, err := q.GetEventInternal(ctx, args.EventID)
	if db.IsNotFound(err) {
		return river.JobCancel(errors.New("webhook: event not found"))
	}
	if err != nil {
		return fmt.Errorf("webhook: load event: %w", err)
	}
	if ev.ProductID != args.ProductID {
		return river.JobCancel(errors.New("webhook: job product does not match the event"))
	}
	d, err := q.GetWebhookDelivery(ctx, ev.ID)
	if db.IsNotFound(err) {
		return river.JobCancel(errors.New("webhook: delivery row not found"))
	}
	if err != nil {
		return fmt.Errorf("webhook: load delivery: %w", err)
	}
	if d.Status != store.WebhookDeliveryStatusPending {
		// Already delivered (or finished some other way): idempotent no-op.
		log.DebugContext(ctx, "webhook already finished", slog.String("status", string(d.Status)))
		return nil
	}

	p, err := q.GetProductWebhookTarget(ctx, ev.ProductID)
	if err != nil {
		return fmt.Errorf("webhook: load product: %w", err)
	}
	switch {
	case p.WebhookUrl == nil || strings.TrimSpace(*p.WebhookUrl) == "":
		return w.skip(ctx, ev.ID, "no webhook URL configured")
	case p.Status != store.ProductStatusActive:
		return w.skip(ctx, ev.ID, "product suspended")
	}
	target, err := url.Parse(strings.TrimSpace(*p.WebhookUrl))
	if err != nil || target.Host == "" || (target.Scheme != "https" && (target.Scheme != "http" || !w.allowHTTP)) {
		// Can't succeed until an admin fixes the URL; then they redeliver.
		res := result{host: hostOf(target), errMsg: "webhook URL must be an absolute https:// URL"}
		if err := w.record(ctx, ev.ID, res, store.WebhookDeliveryStatusFailed); err != nil {
			return err
		}
		log.WarnContext(ctx, "webhook URL unusable; delivery failed")
		return river.JobCancel(errors.New("webhook: unusable webhook URL"))
	}

	adm, wait, tk := w.endpoints.acquire(ev.ProductID)
	switch adm {
	case rejectedOpen:
		// Snoozing does not consume an attempt, so the event keeps all its retries.
		log.DebugContext(ctx, "webhook circuit open; snoozing", slog.Duration("wait", wait))
		return river.JobSnooze(wait + jitter(wait/10+time.Second))
	case rejectedBusy:
		return river.JobSnooze(time.Second + jitter(2*time.Second))
	}
	v := noVerdict
	defer func() { w.endpoints.release(tk, v) }()

	cur, prev, err := w.secrets.WebhookSecrets(ctx, ev.ProductID)
	if err != nil {
		return fmt.Errorf("webhook: load signing secret: %w", err)
	}
	body, err := json.Marshal(apiview.Event{
		Object: "event", ID: ev.ID, Type: ev.Type, APIVersion: apiview.APIVersion,
		CreatedAt: ev.CreatedAt.UTC(), Data: json.RawMessage(ev.Data),
	})
	if err != nil {
		return river.JobCancel(fmt.Errorf("webhook: encode event: %w", err))
	}

	res := w.send(ctx, target, ev, body, cur, prev)
	if res.ok {
		v = succeeded
	} else {
		v = failed
	}
	final := !res.ok && job.Attempt >= job.MaxAttempts
	status := store.WebhookDeliveryStatusPending
	switch {
	case res.ok:
		status = store.WebhookDeliveryStatusDelivered
	case final:
		status = store.WebhookDeliveryStatusFailed
	}
	if err := w.record(ctx, ev.ID, res, status); err != nil {
		// The receiver may have the event already; the retry re-sends it
		// (at least once; receivers de-duplicate on the event id).
		return err
	}
	log = log.With(slog.String("url_host", res.host), slog.Int("http_status", res.status), slog.Int64("latency_ms", res.latency.Milliseconds()))
	if res.ok {
		log.InfoContext(ctx, "webhook delivered")
		return nil
	}
	if res.retryAfter > 0 && !final {
		w.retryAfter.Store(job.ID, w.now().Add(res.retryAfter))
	}
	if final {
		log.WarnContext(ctx, "webhook delivery failed permanently", slog.String("error", res.errMsg))
	} else {
		log.InfoContext(ctx, "webhook attempt failed; will retry", slog.String("error", res.errMsg))
	}
	return fmt.Errorf("webhook: delivery attempt failed: %s", res.errMsg)
}

func (w *Worker) skip(ctx context.Context, eventID uuid.UUID, reason string) error {
	err := db.WithTx(ctx, w.pool, workerActor, func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		return q.SkipWebhookDelivery(ctx, store.SkipWebhookDeliveryParams{EventID: eventID, Reason: &reason})
	})
	if err != nil {
		return fmt.Errorf("webhook: mark skipped: %w", err)
	}
	w.logger.InfoContext(ctx, "webhook skipped", slog.String("event_id", eventID.String()), slog.String("reason", reason))
	return nil
}

// record logs the attempt and updates the delivery summary under the
// delivery row lock. If another job already finished the delivery it
// changes nothing.
func (w *Worker) record(ctx context.Context, eventID uuid.UUID, res result, status store.WebhookDeliveryStatus) error {
	// Record even if the job's context has just expired.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	err := db.WithTx(ctx, w.pool, workerActor, func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		d, err := q.LockWebhookDelivery(ctx, eventID)
		if err != nil {
			return err
		}
		if d.Status != store.WebhookDeliveryStatusPending {
			return nil
		}
		var code *int32
		if res.status > 0 {
			c := int32(res.status)
			code = &c
		}
		var errMsg *string
		if res.errMsg != "" {
			m := truncate(res.errMsg, maxStoredError)
			errMsg = &m
		}
		latency := res.latency.Milliseconds()
		if latency > 1<<31-1 {
			latency = 1<<31 - 1
		}
		host := res.host
		if host == "" {
			host = "-"
		}
		if err := q.InsertWebhookAttempt(ctx, store.InsertWebhookAttemptParams{
			EventID: eventID, Attempt: d.Attempts + 1, UrlHost: truncate(host, 255), StatusCode: code,
			LatencyMs: int32(latency), Error: errMsg, ResponseBody: res.body,
		}); err != nil {
			return err
		}
		_, err = q.RecordWebhookAttempt(ctx, store.RecordWebhookAttemptParams{
			EventID: eventID, Status: status, LastStatusCode: code, LastError: errMsg,
		})
		return err
	})
	if err != nil {
		return fmt.Errorf("webhook: record attempt: %w", err)
	}
	return nil
}

// result is one HTTP attempt.
type result struct {
	ok         bool
	host       string
	status     int // 0 when no response
	latency    time.Duration
	errMsg     string  // short, never contains the URL path/query or secrets
	body       *string // sanitized first 1 KB of the response
	retryAfter time.Duration
}

func (w *Worker) send(ctx context.Context, target *url.URL, ev store.Event, body, cur, prev []byte) result {
	res := result{host: hostOf(target)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		res.errMsg = "could not build request"
		return res
	}
	sentAt := w.now()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set(HeaderEventID, ev.ID.String())
	req.Header.Set(HeaderEventType, ev.Type)
	req.Header.Set(HeaderSignature, SignatureHeader(sentAt.Unix(), body, cur, prev))

	start := time.Now()
	resp, err := w.client.Do(req)
	if resp != nil {
		res.status = resp.StatusCode
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
		_ = resp.Body.Close()
		res.body = sanitizeBody(raw)
		if d, ok := parseRetryAfter(resp.StatusCode, resp.Header, w.now()); ok {
			res.retryAfter = d
		}
	}
	res.latency = time.Since(start)
	switch {
	case err != nil:
		res.errMsg = describeErr(err)
	case res.status < 200 || res.status > 299:
		res.errMsg = "HTTP " + strconv.Itoa(res.status)
	default:
		res.ok = true
	}
	return res
}

// hostOf is the only part of the URL ever stored or logged: paths and
// query strings may carry receiver-side tokens.
func hostOf(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Host
}

// describeErr turns a client error into a short, safe message. url.Error is
// unwrapped first because its text includes the full URL.
func describeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	var netErr net.Error
	switch {
	case errors.Is(err, ErrRedirect):
		return "redirect not followed"
	case errors.Is(err, ErrBlockedAddress):
		return "destination address not allowed (private, loopback or reserved network)"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled (worker shutting down)"
	case errors.As(err, &dnsErr):
		if dnsErr.IsNotFound {
			return "dns: no such host"
		}
		return "dns lookup failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "host unreachable"
	case errors.As(err, &certErr):
		return "tls: certificate verification failed"
	case errors.As(err, &recErr):
		return "tls: server did not speak TLS"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed before a response"
	}
	msg := err.Error()
	if strings.Contains(msg, "tls:") {
		return truncate("tls handshake failed: "+msg, maxStoredError)
	}
	return truncate("network error: "+msg, maxStoredError)
}

// sanitizeBody keeps the first 1 KB characters of a response for the
// attempt log: valid UTF-8, no control characters except tab/newline.
func sanitizeBody(raw []byte) *string {
	if len(raw) == 0 {
		return nil
	}
	var b strings.Builder
	n := 0
	for len(raw) > 0 && n < maxStoredBody {
		r, size := utf8.DecodeRune(raw)
		raw = raw[size:]
		switch {
		case r == utf8.RuneError && size <= 1:
			r = '�'
		case r == '\n' || r == '\t':
		case r == '\r':
			continue
		case unicode.IsControl(r) || r == 0:
			continue
		}
		b.WriteRune(r)
		n++
	}
	s := b.String()
	if s == "" {
		return nil
	}
	return &s
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}

func jitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return rand.N(max) //nolint:gosec // jitter, not security
}
