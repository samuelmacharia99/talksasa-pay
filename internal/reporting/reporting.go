// Package reporting serves the read-only views behind the admin panel and the
// hosted pay page. It never writes: every money-changing action lives in its
// owning domain package (charge, ledger, stk...).
package reporting

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const (
	DefaultLimit = 50
	MaxLimit     = 200
	maxQueryLen  = 100
)

var (
	ErrNotFound = errors.New("reporting: not found")
	// ErrInvalidFilter wraps a message safe to show to the admin user.
	ErrInvalidFilter = errors.New("reporting: invalid filter")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidFilter, fmt.Sprintf(format, args...))
}

// Nairobi is the reporting timezone: "today" means the Kenyan calendar day.
var Nairobi = func() *time.Location {
	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		return time.FixedZone("EAT", 3*60*60) // Kenya has no DST
	}
	return loc
}()

// Service runs the read queries.
type Service struct {
	pool *db.Pool
	now  func() time.Time
}

// NewService builds the reporting service.
func NewService(pool *db.Pool) *Service { return &Service{pool: pool, now: time.Now} }

// Page is one page of a keyset-paginated list.
type Page[T any] struct {
	Items      []T
	NextCursor string // empty on the last page
}

// cursor encodes the sort key of the last row: "<unix micros>.<id>".
func encodeCursor(at time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(at.UnixMicro(), 10) + "." + id))
}

func decodeCursor(c string) (*time.Time, string, error) {
	if c == "" {
		return nil, "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(raw) > 100 {
		return nil, "", invalid("malformed cursor")
	}
	us, id, ok := strings.Cut(string(raw), ".")
	n, err := strconv.ParseInt(us, 10, 64)
	if !ok || err != nil || id == "" {
		return nil, "", invalid("malformed cursor")
	}
	t := time.UnixMicro(n).UTC()
	return &t, id, nil
}

func uuidCursor(c string) (*time.Time, *uuid.UUID, error) {
	at, id, err := decodeCursor(c)
	if err != nil || at == nil {
		return nil, nil, err
	}
	u, err := uuid.Parse(id)
	if err != nil {
		return nil, nil, invalid("malformed cursor")
	}
	return at, &u, nil
}

func limit(n int) int32 {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return int32(n)
	}
}

// search is a free-text query expanded into the forms the SQL matches on.
type search struct {
	pattern     *string // lower-cased, LIKE-escaped, wrapped in %
	accountNorm *string // account number without hyphen, upper-case
	phone       *string // normalised MSISDN, if q looks like a phone
}

func parseSearch(q string) (search, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return search{}, nil
	}
	if len(q) > maxQueryLen {
		return search{}, invalid("search text must be at most %d characters", maxQueryLen)
	}
	pattern := "%" + likeEscape(strings.ToLower(q)) + "%"
	norm := strings.ToUpper(strings.NewReplacer("-", "", " ", "", "_", "", ".", "").Replace(q))
	s := search{pattern: &pattern, accountNorm: &norm}
	if p, err := phone.Normalize(q); err == nil {
		s.phone = &p
	}
	return s, nil
}

// likeEscape neutralises LIKE wildcards in user input (default escape \).
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func optString(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// Charges
// ---------------------------------------------------------------------------

// ChargeFilter filters the charge list. From/To bound created_at.
type ChargeFilter struct {
	AppID    *uuid.UUID
	Status   string
	Q        string // reference, description or account number
	From, To *time.Time
	Cursor   string
	Limit    int
}

// ListCharges lists charges, newest first.
func (s *Service) ListCharges(ctx context.Context, f ChargeFilter) (Page[store.ListChargesAdminRow], error) {
	var out Page[store.ListChargesAdminRow]
	srch, err := parseSearch(f.Q)
	if err != nil {
		return out, err
	}
	status := store.NullChargeStatus{}
	if f.Status != "" {
		st := store.ChargeStatus(f.Status)
		if !st.Valid() {
			return out, invalid("unknown status %q", f.Status)
		}
		status = store.NullChargeStatus{ChargeStatus: st, Valid: true}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return out, invalid("'from' must be before 'to'")
	}
	at, id, err := uuidCursor(f.Cursor)
	if err != nil {
		return out, err
	}
	lim := limit(f.Limit)
	rows, err := store.New(s.pool).ListChargesAdmin(ctx, store.ListChargesAdminParams{
		AppID: f.AppID, Status: status, Pattern: srch.pattern, AccountNorm: srch.accountNorm,
		CreatedFrom: f.From, CreatedTo: f.To, CursorAt: at, CursorID: id, Lim: lim + 1,
	})
	if err != nil {
		return out, err
	}
	if len(rows) > int(lim) {
		rows = rows[:lim]
		last := rows[len(rows)-1]
		out.NextCursor = encodeCursor(last.CreatedAt, last.ID.String())
	}
	out.Items = rows
	return out, nil
}

// ChargeDetail is a charge with its prompts and every transaction naming it.
type ChargeDetail struct {
	Charge       store.GetChargeAdminRow
	Pushes       []store.StkPush
	Transactions []store.Transaction
}

// GetCharge returns one charge.
func (s *Service) GetCharge(ctx context.Context, id uuid.UUID) (ChargeDetail, error) {
	q := store.New(s.pool)
	c, err := q.GetChargeAdmin(ctx, id)
	if err != nil {
		if db.IsNotFound(err) {
			return ChargeDetail{}, ErrNotFound
		}
		return ChargeDetail{}, err
	}
	pushes, err := q.ListSTKPushesForCharge(ctx, store.ListSTKPushesForChargeParams{ChargeID: id, Lim: MaxLimit})
	if err != nil {
		return ChargeDetail{}, err
	}
	txns, err := q.ListTransactionsForChargeAdmin(ctx, &id)
	if err != nil {
		return ChargeDetail{}, err
	}
	return ChargeDetail{Charge: c, Pushes: pushes, Transactions: txns}, nil
}

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

// TxnFilter filters the transaction list. From/To bound created_at.
type TxnFilter struct {
	AppID     *uuid.UUID
	ChargeID  *uuid.UUID
	Channel   string
	Status    string
	Direction string
	Receipt   string
	Phone     string
	From, To  *time.Time
	// UnmatchedOnly keeps succeeded incoming money with no app (unknown
	// account prefix) that staff must assign.
	UnmatchedOnly bool
	Cursor        string
	Limit         int
}

// ListTransactions lists ledger rows, newest first.
func (s *Service) ListTransactions(ctx context.Context, f TxnFilter) (Page[store.ListTransactionsAdminRow], error) {
	var out Page[store.ListTransactionsAdminRow]
	p := store.ListTransactionsAdminParams{
		AppID: f.AppID, ChargeID: f.ChargeID, CreatedFrom: f.From, CreatedTo: f.To,
		UnmatchedOnly: f.UnmatchedOnly,
	}
	if f.Channel != "" {
		c := store.TxnChannel(f.Channel)
		if !c.Valid() {
			return out, invalid("unknown channel %q", f.Channel)
		}
		p.Channel = store.NullTxnChannel{TxnChannel: c, Valid: true}
	}
	if f.Status != "" {
		st := store.TxnStatus(f.Status)
		if !st.Valid() {
			return out, invalid("unknown status %q", f.Status)
		}
		p.Status = store.NullTxnStatus{TxnStatus: st, Valid: true}
	}
	if f.Direction != "" {
		d := store.TxnDirection(f.Direction)
		if !d.Valid() {
			return out, invalid("unknown direction %q", f.Direction)
		}
		p.Direction = store.NullTxnDirection{TxnDirection: d, Valid: true}
	}
	if r := strings.ToUpper(strings.TrimSpace(f.Receipt)); r != "" {
		if len(r) > 64 {
			return out, invalid("receipt too long")
		}
		p.Receipt = &r
	}
	if f.Phone != "" {
		n, err := phone.Normalize(f.Phone)
		if err != nil {
			return out, invalid("phone must be a Kenyan mobile number")
		}
		p.Phone = &n
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		return out, invalid("'from' must be before 'to'")
	}
	at, id, err := uuidCursor(f.Cursor)
	if err != nil {
		return out, err
	}
	lim := limit(f.Limit)
	p.CursorAt, p.CursorID, p.Lim = at, id, lim+1
	rows, err := store.New(s.pool).ListTransactionsAdmin(ctx, p)
	if err != nil {
		return out, err
	}
	if len(rows) > int(lim) {
		rows = rows[:lim]
		last := rows[len(rows)-1]
		out.NextCursor = encodeCursor(last.CreatedAt, last.ID.String())
	}
	out.Items = rows
	return out, nil
}

// TxnDetail is one transaction with its status history.
type TxnDetail struct {
	Txn    store.GetTransactionAdminRow
	Events []store.TransactionEvent
}

// GetTransaction returns one transaction.
func (s *Service) GetTransaction(ctx context.Context, id uuid.UUID) (TxnDetail, error) {
	q := store.New(s.pool)
	t, err := q.GetTransactionAdmin(ctx, id)
	if err != nil {
		if db.IsNotFound(err) {
			return TxnDetail{}, ErrNotFound
		}
		return TxnDetail{}, err
	}
	events, err := q.ListTransactionEvents(ctx, id)
	if err != nil {
		return TxnDetail{}, err
	}
	return TxnDetail{Txn: t, Events: events}, nil
}

// ---------------------------------------------------------------------------
// Audit log
// ---------------------------------------------------------------------------

// AuditFilter filters the audit log. Action matches a prefix ("app.").
type AuditFilter struct {
	AppID    *uuid.UUID
	Action   string
	ActorID  string
	EntityID string
	Cursor   string
	Limit    int
}

// ListAudit lists audit entries, newest first.
func (s *Service) ListAudit(ctx context.Context, f AuditFilter) (Page[store.ListAuditAdminRow], error) {
	var out Page[store.ListAuditAdminRow]
	var action *string
	if a := strings.TrimSpace(f.Action); a != "" {
		if len(a) > 64 {
			return out, invalid("action filter too long")
		}
		p := likeEscape(a) + "%"
		action = &p
	}
	at, idStr, err := decodeCursor(f.Cursor)
	if err != nil {
		return out, err
	}
	var cursorID *int64
	if at != nil {
		n, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return out, invalid("malformed cursor")
		}
		cursorID = &n
	}
	lim := limit(f.Limit)
	rows, err := store.New(s.pool).ListAuditAdmin(ctx, store.ListAuditAdminParams{
		AppID: f.AppID, ActionPrefix: action, ActorID: optString(f.ActorID), EntityID: optString(f.EntityID),
		CursorAt: at, CursorID: cursorID, Lim: lim + 1,
	})
	if err != nil {
		return out, err
	}
	if len(rows) > int(lim) {
		rows = rows[:lim]
		last := rows[len(rows)-1]
		out.NextCursor = encodeCursor(last.OccurredAt, strconv.FormatInt(last.ID, 10))
	}
	out.Items = rows
	return out, nil
}

// ---------------------------------------------------------------------------
// Dashboard and integrity
// ---------------------------------------------------------------------------

// DayTotal is the collections of one Nairobi calendar day.
type DayTotal struct {
	Day      string // YYYY-MM-DD
	Total    int64
	Payments int64
	ByApp    map[uuid.UUID]int64
}

// Window is collections over a period, overall and per app.
type Window struct {
	Total    int64
	Payments int64
	ByApp    map[uuid.UUID]int64
}

// Dashboard is the admin home page.
type Dashboard struct {
	Today, Last7Days, Last30Days Window
	Daily                        []DayTotal // 30 days, oldest first, gaps filled with zeros
	Counts                       store.DashboardCountsRow
	GeneratedAt                  time.Time
}

// Dashboard aggregates collections (successful incoming payments) by
// Nairobi day for the last 30 days, plus queue counts.
func (s *Service) Dashboard(ctx context.Context) (Dashboard, error) {
	now := s.now().In(Nairobi)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, Nairobi)
	since := today.AddDate(0, 0, -29)
	q := store.New(s.pool)
	rows, err := q.CollectionsByDay(ctx, since.UTC())
	if err != nil {
		return Dashboard{}, err
	}
	counts, err := q.DashboardCounts(ctx)
	if err != nil {
		return Dashboard{}, err
	}
	days := map[string]*DayTotal{}
	var daily []DayTotal
	for d := since; !d.After(today); d = d.AddDate(0, 0, 1) {
		daily = append(daily, DayTotal{Day: d.Format(time.DateOnly), ByApp: map[uuid.UUID]int64{}})
	}
	for i := range daily {
		days[daily[i].Day] = &daily[i]
	}
	d := Dashboard{Daily: daily, Counts: counts, GeneratedAt: s.now().UTC(),
		Today: newWindow(), Last7Days: newWindow(), Last30Days: newWindow()}
	todayStr, weekStart := today.Format(time.DateOnly), today.AddDate(0, 0, -6).Format(time.DateOnly)
	for _, r := range rows {
		day, ok := days[r.Day]
		if !ok {
			continue
		}
		day.Total += r.Total
		day.Payments += r.Payments
		day.ByApp[r.AppID] += r.Total
		add(&d.Last30Days, r)
		if r.Day >= weekStart {
			add(&d.Last7Days, r)
		}
		if r.Day == todayStr {
			add(&d.Today, r)
		}
	}
	sort.Slice(d.Daily, func(i, j int) bool { return d.Daily[i].Day < d.Daily[j].Day })
	return d, nil
}

func newWindow() Window { return Window{ByApp: map[uuid.UUID]int64{}} }

func add(w *Window, r store.CollectionsByDayRow) {
	w.Total += r.Total
	w.Payments += r.Payments
	w.ByApp[r.AppID] += r.Total
}

// Integrity reports the audit-chain check.
type Integrity struct {
	AuditChainOK bool
	FirstBadID   int64
	CheckedAt    time.Time
}

// CheckIntegrity runs the same check as `admin audit verify`.
func (s *Service) CheckIntegrity(ctx context.Context) (Integrity, error) {
	q := store.New(s.pool)
	bad, err := q.VerifyAuditChain(ctx)
	if err != nil {
		return Integrity{}, err
	}
	return Integrity{AuditChainOK: bad == 0, FirstBadID: bad, CheckedAt: s.now().UTC()}, nil
}

// ---------------------------------------------------------------------------
// Hosted pay page
// ---------------------------------------------------------------------------

// PayPage is everything the public pay page may show. ChargeID and AppID
// are for the server only (sending an STK push, finding the latest one):
// they must never be rendered to the visitor.
type PayPage struct {
	ChargeID      uuid.UUID
	AppID         uuid.UUID
	AppName       string
	AccountNumber string
	Reference     string
	Description   *string
	Amount        int64
	Currency      string
	Status        store.ChargeStatus
	ExpiresAt     time.Time
	SucceededAt   *time.Time
	// Payable: a prompt may be sent (pending and not past expires_at).
	Payable bool
}

// PayPageByToken resolves a hosted-page token. Unknown tokens and charges
// of suspended apps are both ErrNotFound.
func (s *Service) PayPageByToken(ctx context.Context, token string) (PayPage, error) {
	r, err := store.New(s.pool).GetChargeByHostedToken(ctx, token)
	if err != nil {
		if db.IsNotFound(err) {
			return PayPage{}, ErrNotFound
		}
		return PayPage{}, err
	}
	if r.AppStatus != store.AppStatusActive {
		return PayPage{}, ErrNotFound
	}
	status := r.Status
	expired := status == store.ChargeStatusPending && !s.now().Before(r.ExpiresAt)
	if expired {
		// The expiry job may not have run yet; present it as expired.
		status = store.ChargeStatusExpired
	}
	return PayPage{
		ChargeID: r.ID, AppID: r.AppID, AppName: r.AppName, AccountNumber: r.AccountNumber,
		Reference: r.Reference, Description: r.Description, Amount: r.Amount, Currency: r.Currency,
		Status: status, ExpiresAt: r.ExpiresAt, SucceededAt: r.SucceededAt,
		Payable: status == store.ChargeStatusPending,
	}, nil
}
