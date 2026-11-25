package adminapi

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/app"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// JSON shapes of the admin API (documented in api/admin.openapi.yaml).
// Amounts are integer KES. Phone numbers are always masked: the admin panel
// never needs a full MSISDN until refunds (Phase 4), which will reveal it
// behind its own permission.

type userJSON struct {
	ID          uuid.UUID  `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	LastLoginAt *time.Time `json:"last_login_at"`
	CreatedBy   string     `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	DisabledAt  *time.Time `json:"disabled_at"`
}

func toUser(u admin.User) userJSON {
	return userJSON{ID: u.ID, Email: u.Email, Name: u.Name, Role: string(u.Role), Status: string(u.Status),
		LastLoginAt: u.LastLoginAt, CreatedBy: u.CreatedBy, CreatedAt: u.CreatedAt, DisabledAt: u.DisabledAt}
}

type meJSON struct {
	ID            uuid.UUID `json:"id"`
	Email         string    `json:"email"`
	Name          string    `json:"name"`
	Role          string    `json:"role"`
	Permissions   []string  `json:"permissions"`
	ExpiresAt     time.Time `json:"expires_at"`
	IdleExpiresAt time.Time `json:"idle_expires_at"`
}

func toMe(p admin.Principal) meJSON {
	perms := []string{}
	for _, x := range admin.Permissions(p.Role) {
		perms = append(perms, string(x))
	}
	return meJSON{ID: p.UserID, Email: p.Email, Name: p.Name, Role: string(p.Role), Permissions: perms,
		ExpiresAt: p.ExpiresAt, IdleExpiresAt: p.IdleExpiresAt}
}

type appJSON struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	AccountPrefix string    `json:"account_prefix"`
	WebhookURL    string    `json:"webhook_url"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

func toApp(p app.App) appJSON {
	return appJSON{ID: p.ID, Name: p.Name, AccountPrefix: p.AccountPrefix, WebhookURL: p.WebhookURL,
		Status: string(p.Status), CreatedAt: p.CreatedAt}
}

type apiKeyJSON struct {
	KeyID      string     `json:"key_id"`
	Label      string     `json:"label"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	AllowedIPs []string   `json:"allowed_ips"` // null = any address
}

func toAPIKey(k app.APIKey) apiKeyJSON {
	out := apiKeyJSON{KeyID: k.KeyID, Label: k.Label, CreatedBy: k.CreatedBy, CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, LastUsedAt: k.LastUsedAt}
	if len(k.AllowedCIDRs) > 0 {
		out.AllowedIPs = make([]string, len(k.AllowedCIDRs))
		for i, c := range k.AllowedCIDRs {
			if c.IsSingleIP() {
				out.AllowedIPs[i] = c.Addr().String()
			} else {
				out.AllowedIPs[i] = c.String()
			}
		}
	}
	return out
}

type pageJSON[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func mapPage[S, T any](p reporting.Page[S], f func(S) T) pageJSON[T] {
	out := pageJSON[T]{Items: make([]T, len(p.Items)), NextCursor: p.NextCursor}
	for i, s := range p.Items {
		out.Items[i] = f(s)
	}
	return out
}

type chargeJSON struct {
	ID            uuid.UUID  `json:"id"`
	AppID         uuid.UUID  `json:"app_id"`
	AccountPrefix string     `json:"account_prefix"`
	AppName       string     `json:"app_name"`
	Reference     string     `json:"reference"`
	Description   *string    `json:"description"`
	Amount        int64      `json:"amount"`
	Currency      string     `json:"currency"`
	AccountNumber string     `json:"account_number"`
	Phone         *string    `json:"phone"` // masked
	Status        string     `json:"status"`
	ExpiresAt     time.Time  `json:"expires_at"`
	SucceededAt   *time.Time `json:"succeeded_at"`
	TransactionID *uuid.UUID `json:"transaction_id"`
	ClosedAt      *time.Time `json:"closed_at"`
	CreatedAt     time.Time  `json:"created_at"`
}

func toChargeRow(r store.ListChargesAdminRow) chargeJSON {
	return chargeJSON{ID: r.ID, AppID: r.AppID, AccountPrefix: r.AccountPrefix, AppName: r.AppName, Reference: r.Reference,
		Description: r.Description, Amount: r.Amount, Currency: r.Currency, AccountNumber: r.AccountNumber,
		Phone: maskPtr(r.Phone), Status: string(r.Status), ExpiresAt: r.ExpiresAt, SucceededAt: r.SucceededAt,
		TransactionID: r.TransactionID, ClosedAt: r.ClosedAt, CreatedAt: r.CreatedAt}
}

type chargeDetailJSON struct {
	chargeJSON
	AppStatus        string          `json:"app_status"`
	Metadata         json.RawMessage `json:"metadata"`
	AccountGenerated bool            `json:"account_generated"`
	UpdatedAt        time.Time       `json:"updated_at"`
	STKPushes        []stkPushJSON   `json:"stk_pushes"`
	Transactions     []txnJSON       `json:"transactions"`
}

func toChargeDetail(d reporting.ChargeDetail) chargeDetailJSON {
	c := d.Charge
	out := chargeDetailJSON{
		chargeJSON: chargeJSON{ID: c.ID, AppID: c.AppID, AccountPrefix: c.AccountPrefix, AppName: c.AppName, Reference: c.Reference,
			Description: c.Description, Amount: c.Amount, Currency: c.Currency, AccountNumber: c.AccountNumber,
			Phone: maskPtr(c.Phone), Status: string(c.Status), ExpiresAt: c.ExpiresAt, SucceededAt: c.SucceededAt,
			TransactionID: c.TransactionID, ClosedAt: c.ClosedAt, CreatedAt: c.CreatedAt},
		AppStatus: string(c.AppStatus), Metadata: json.RawMessage(c.Metadata), AccountGenerated: c.AccountGenerated,
		UpdatedAt: c.UpdatedAt, STKPushes: []stkPushJSON{}, Transactions: []txnJSON{},
	}
	if len(c.Metadata) == 0 {
		out.Metadata = json.RawMessage("null")
	}
	for _, p := range d.Pushes {
		out.STKPushes = append(out.STKPushes, toSTKPush(p))
	}
	for _, t := range d.Transactions {
		out.Transactions = append(out.Transactions, txnJSON{ID: t.ID, AppID: t.AppID, Direction: string(t.Direction),
			Kind: string(t.Kind), Channel: string(t.Channel), Status: string(t.Status), Amount: t.Amount,
			Receipt: t.ProviderTransactionID, Phone: maskPtr(t.Phone), AccountNumber: t.AccountNumber,
			ChargeID: t.ChargeID, ChargeReference: &c.Reference, FailureReason: t.FailureReason,
			CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt})
	}
	return out
}

type txnJSON struct {
	ID              uuid.UUID  `json:"id"`
	AppID           *uuid.UUID `json:"app_id"` // null = unmatched
	AccountPrefix   *string    `json:"account_prefix"`
	Direction       string     `json:"direction"`
	Kind            string     `json:"kind"`
	Channel         string     `json:"channel"`
	Status          string     `json:"status"`
	Amount          int64      `json:"amount"`
	Receipt         *string    `json:"receipt"`
	Phone           *string    `json:"phone"` // masked
	AccountNumber   *string    `json:"account_number"`
	ChargeID        *uuid.UUID `json:"charge_id"`
	ChargeReference *string    `json:"charge_reference"`
	FailureReason   *string    `json:"failure_reason"`
	CreatedAt       time.Time  `json:"created_at"`
	CompletedAt     *time.Time `json:"completed_at"`
}

func maskPtr(p *string) *string {
	if p == nil {
		return nil
	}
	m := tlog.MaskPhone(*p)
	return &m
}

func toTxnRow(r store.ListTransactionsAdminRow) txnJSON {
	return txnJSON{ID: r.ID, AppID: r.AppID, AccountPrefix: r.AccountPrefix, Direction: string(r.Direction),
		Kind: string(r.Kind), Channel: string(r.Channel), Status: string(r.Status), Amount: r.Amount,
		Receipt: r.ProviderTransactionID, Phone: maskPtr(r.Phone), AccountNumber: r.AccountNumber,
		ChargeID: r.ChargeID, ChargeReference: r.ChargeReference, FailureReason: r.FailureReason,
		CreatedAt: r.CreatedAt, CompletedAt: r.CompletedAt}
}

type txnEventJSON struct {
	FromStatus *string   `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Actor      string    `json:"actor"`
	OccurredAt time.Time `json:"occurred_at"`
}

type txnDetailJSON struct {
	txnJSON
	ProviderRequestID     *string         `json:"provider_request_id"`
	ReversesTransactionID *uuid.UUID      `json:"reverses_transaction_id"`
	FailureCode           *string         `json:"failure_code"`
	Reason                *string         `json:"reason"`
	CreatedBy             *string         `json:"created_by"`
	OccurredAt            *time.Time      `json:"occurred_at"`
	UpdatedAt             time.Time       `json:"updated_at"`
	AppName               *string         `json:"app_name"`
	RawPayload            json.RawMessage `json:"raw_payload"`
	Events                []txnEventJSON  `json:"events"`
}

func toTxnDetail(d reporting.TxnDetail) txnDetailJSON {
	t := d.Txn
	out := txnDetailJSON{
		txnJSON: txnJSON{ID: t.ID, AppID: t.AppID, AccountPrefix: t.AccountPrefix, Direction: string(t.Direction),
			Kind: string(t.Kind), Channel: string(t.Channel), Status: string(t.Status), Amount: t.Amount,
			Receipt: t.ProviderTransactionID, Phone: maskPtr(t.Phone), AccountNumber: t.AccountNumber,
			ChargeID: t.ChargeID, ChargeReference: t.ChargeReference, FailureReason: t.FailureReason,
			CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt},
		ProviderRequestID: t.ProviderRequestID, ReversesTransactionID: t.ReversesTransactionID, FailureCode: t.FailureCode,
		Reason: t.Reason, CreatedBy: t.CreatedBy, OccurredAt: t.OccurredAt, UpdatedAt: t.UpdatedAt, AppName: t.AppName,
		RawPayload: redactPayload(t.RawPayload), Events: []txnEventJSON{},
	}
	for _, e := range d.Events {
		var from *string
		if e.FromStatus.Valid {
			s := string(e.FromStatus.TxnStatus)
			from = &s
		}
		out.Events = append(out.Events, txnEventJSON{FromStatus: from, ToStatus: string(e.ToStatus), Actor: e.Actor, OccurredAt: e.OccurredAt})
	}
	return out
}

var msisdnRe = regexp.MustCompile(`^(?:\+?254|0)[17][0-9]{8}$`)

// redactPayload masks phone numbers and sensitive-looking keys inside a
// stored provider payload before it is shown in the admin panel.
func redactPayload(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`"[unreadable payload]"`)
	}
	b, err := json.Marshal(redactValue("", v))
	if err != nil {
		return json.RawMessage(`"[unreadable payload]"`)
	}
	return b
}

func redactValue(key string, v any) any {
	if key != "" && tlog.IsSensitiveKey(key) {
		return "[REDACTED]"
	}
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			t[k] = redactValue(k, child)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = redactValue("", child)
		}
		return t
	case string:
		if msisdnRe.MatchString(strings.TrimSpace(t)) {
			return tlog.MaskPhone(strings.TrimPrefix(strings.TrimSpace(t), "+"))
		}
		return t
	case float64:
		// M-Pesa sends PhoneNumber as a JSON number.
		if s := strings.TrimSuffix(strings.TrimSuffix(jsonNumber(t), ".0"), ".00"); msisdnRe.MatchString(s) {
			return tlog.MaskPhone(s)
		}
		return t
	default:
		return v
	}
}

func jsonNumber(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

type auditJSON struct {
	ID            int64           `json:"id"`
	OccurredAt    time.Time       `json:"occurred_at"`
	ActorType     string          `json:"actor_type"`
	ActorID       string          `json:"actor_id"`
	ActorName     string          `json:"actor_name,omitempty"`
	Action        string          `json:"action"`
	EntityType    string          `json:"entity_type"`
	EntityID      *string         `json:"entity_id"`
	AppID         *uuid.UUID      `json:"app_id"`
	AccountPrefix *string         `json:"account_prefix"`
	Details       json.RawMessage `json:"details"`
}

type dashboardWindowJSON struct {
	Total    int64            `json:"total"`
	Payments int64            `json:"payments"`
	ByApp    map[string]int64 `json:"by_app"` // app id -> KES; nil UUID = unassigned (unmatched)
}

func toWindow(w reporting.Window) dashboardWindowJSON {
	out := dashboardWindowJSON{Total: w.Total, Payments: w.Payments, ByApp: map[string]int64{}}
	for k, v := range w.ByApp {
		out.ByApp[k.String()] = v
	}
	return out
}

type dashboardDayJSON struct {
	Day      string           `json:"day"`
	Total    int64            `json:"total"`
	Payments int64            `json:"payments"`
	ByApp    map[string]int64 `json:"by_app"`
}

type dashboardJSON struct {
	Today       dashboardWindowJSON `json:"today"`
	Last7Days   dashboardWindowJSON `json:"last_7_days"`
	Last30Days  dashboardWindowJSON `json:"last_30_days"`
	Daily       []dashboardDayJSON  `json:"daily"`
	Apps        []appJSON           `json:"apps"`
	Counts      map[string]int64    `json:"counts"`
	Timezone    string              `json:"timezone"`
	GeneratedAt time.Time           `json:"generated_at"`
}

func toDashboard(d reporting.Dashboard, apps []app.App) dashboardJSON {
	out := dashboardJSON{
		Today: toWindow(d.Today), Last7Days: toWindow(d.Last7Days), Last30Days: toWindow(d.Last30Days),
		Daily: make([]dashboardDayJSON, len(d.Daily)), Apps: []appJSON{}, Timezone: "Africa/Nairobi", GeneratedAt: d.GeneratedAt,
		Counts: map[string]int64{
			"charges_pending":                d.Counts.ChargesPending,
			"charges_succeeded_today":        d.Counts.ChargesSucceededToday,
			"charges_succeeded_today_amount": d.Counts.ChargesSucceededTodayAmount,
			"charges_needs_review":           d.Counts.ChargesNeedsReview,
			"transactions_needs_review":      d.Counts.TransactionsNeedsReview,
			"unmatched_count":                d.Counts.UnmatchedCount,
			"unmatched_amount":               d.Counts.UnmatchedAmount,
			"pending_transactions":           d.Counts.PendingTransactions,
			"unknown_stk_pushes":             d.Counts.UnknownStkPushes,
			"unprocessed_callbacks":          d.Counts.UnprocessedCallbacks,
			"inactive_app_payments":          d.Counts.InactiveAppPayments,
		},
	}
	for i, day := range d.Daily {
		bp := map[string]int64{}
		for k, v := range day.ByApp {
			bp[k.String()] = v
		}
		out.Daily[i] = dashboardDayJSON{Day: day.Day, Total: day.Total, Payments: day.Payments, ByApp: bp}
	}
	for _, p := range apps {
		out.Apps = append(out.Apps, toApp(p))
	}
	return out
}

type integrityJSON struct {
	AuditChainOK bool      `json:"audit_chain_ok"`
	FirstBadID   int64     `json:"first_bad_audit_id,omitempty"`
	CheckedAt    time.Time `json:"checked_at"`
}

func toIntegrity(i reporting.Integrity) integrityJSON {
	return integrityJSON{AuditChainOK: i.AuditChainOK, FirstBadID: i.FirstBadID, CheckedAt: i.CheckedAt}
}
