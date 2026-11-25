package mpesasettings

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// DefaultRecheck is how long the Provider trusts its settings before it
// reads the saved version again. A save in the same process applies at
// once; other processes (the worker, other api replicas) follow within
// this period.
const DefaultRecheck = 5 * time.Second

// lookupTimeout bounds one version read / reload.
const lookupTimeout = 2 * time.Second

// Provider is the M-Pesa client the api and worker use. It implements
// mpesa.Client by delegating to a Daraja client built from the effective
// settings, and rebuilds that client when the saved settings version
// changes (read on use, cached for DefaultRecheck). With no usable
// settings, Configured is false and calls fail as "not configured".
// Safe for concurrent use.
type Provider struct {
	svc     *Service
	log     *slog.Logger
	recheck time.Duration
	now     func() time.Time

	mu        sync.Mutex
	cur       *snapshot
	checkedAt time.Time
}

type snapshot struct {
	source  Source
	version int64
	paybill string
	client  *mpesa.HTTPClient // nil: not usable
}

var _ mpesa.Client = (*Provider)(nil)

// NewProvider loads the settings once (logging which are in effect) and
// subscribes to saves made through svc. recheck <= 0 uses DefaultRecheck.
func NewProvider(ctx context.Context, svc *Service, recheck time.Duration) *Provider {
	if recheck <= 0 {
		recheck = DefaultRecheck
	}
	p := &Provider{svc: svc, log: svc.o.Logger, recheck: recheck, now: svc.o.Now}
	p.mu.Lock()
	p.refreshLocked(ctx)
	p.mu.Unlock()
	svc.OnChange(p.Invalidate)
	return p
}

// Invalidate makes the next use re-read the settings.
func (p *Provider) Invalidate() {
	p.mu.Lock()
	p.checkedAt = time.Time{}
	p.mu.Unlock()
}

func (p *Provider) current() *snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cur == nil || p.now().Sub(p.checkedAt) >= p.recheck {
		ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
		defer cancel()
		p.refreshLocked(ctx)
	}
	if p.cur == nil {
		return &snapshot{source: SourceNone}
	}
	return p.cur
}

// refreshLocked re-reads the saved version and rebuilds the client when it
// changed. On a database error the previous settings stay in use.
func (p *Provider) refreshLocked(ctx context.Context) {
	p.checkedAt = p.now()
	v, err := store.New(p.svc.o.Pool).GetMpesaSettingsVersion(ctx)
	if db.IsNotFound(err) {
		v, err = 0, nil
	}
	if err != nil {
		p.log.WarnContext(ctx, "mpesa settings: could not read the saved version; keeping the current settings", slog.String("error", err.Error()))
		return
	}
	if p.cur != nil && p.cur.version == v {
		return
	}
	st, err := p.svc.Get(ctx)
	if err != nil {
		p.log.ErrorContext(ctx, "mpesa settings: could not load the saved settings; keeping the current settings", slog.String("error", err.Error()))
		return
	}
	snap := &snapshot{source: st.Source, version: st.Version, paybill: st.Paybill}
	attrs := []any{slog.String("source", string(st.Source)), slog.Int64("version", st.Version)}
	c, cerr := p.svc.client(st)
	switch {
	case cerr == nil:
		snap.client = c
		p.log.InfoContext(ctx, "mpesa: enabled", append(attrs, slog.String("env", st.Environment), slog.String("paybill", st.Paybill),
			slog.String("stk_shortcode", st.STKShortCode), slog.String("callback_base", st.CallbackBaseURL),
			slog.Int("callback_cidrs", len(p.svc.o.Mpesa.CallbackCIDRs)))...)
	case st.Source == SourceNone:
		p.log.InfoContext(ctx, "mpesa: off", attrs...)
	default:
		p.log.ErrorContext(ctx, "mpesa: settings cannot be used; M-Pesa is off", append(attrs, slog.String("problem", problem(cerr)))...)
	}
	p.cur = snap
}

// Configured reports whether prompts can be sent with the current settings.
func (p *Provider) Configured() bool { return p.current().client != nil }

// PaybillShortCode is the paybill in effect ("" when none): shown on the
// pay page, and the only shortcode whose C2B confirmations are recorded.
func (p *Provider) PaybillShortCode() string { return p.current().paybill }

// Source says where the settings in effect come from.
func (p *Provider) Source() Source { return p.current().source }

func errUnavailable() error {
	// Rejected: certainly nothing was sent to Safaricom.
	return &mpesa.Error{Kind: mpesa.Rejected, Code: "unavailable", Message: "M-Pesa is not configured", Err: mpesa.ErrUnavailable}
}

// STKPush implements mpesa.Client.
func (p *Provider) STKPush(ctx context.Context, req mpesa.STKPushRequest) (mpesa.STKPushResponse, error) {
	c := p.current().client
	if c == nil {
		return mpesa.STKPushResponse{}, errUnavailable()
	}
	return c.STKPush(ctx, req)
}

// STKQuery implements mpesa.Client.
func (p *Provider) STKQuery(ctx context.Context, checkoutRequestID string) (mpesa.STKQueryResult, error) {
	c := p.current().client
	if c == nil {
		return mpesa.STKQueryResult{}, mpesa.ErrUnavailable
	}
	return c.STKQuery(ctx, checkoutRequestID)
}

// RegisterC2BURLs implements mpesa.Client. Prefer Service.RegisterURLs,
// which records and audits the registration.
func (p *Provider) RegisterC2BURLs(ctx context.Context, req mpesa.RegisterURLsRequest) error {
	c := p.current().client
	if c == nil {
		return errUnavailable()
	}
	return c.RegisterC2BURLs(ctx, req)
}
