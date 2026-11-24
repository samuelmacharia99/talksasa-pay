package adminapi

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/admin"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
	"github.com/talksasa/talksasa-pay/internal/product"
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

type productJSON struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	AccountPrefix string    `json:"account_prefix"`
	WebhookURL    string    `json:"webhook_url"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

func toProduct(p product.Product) productJSON {
	return productJSON{ID: p.ID, Name: p.Name, AccountPrefix: p.AccountPrefix, WebhookURL: p.WebhookURL,
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
}

func toAPIKey(k product.APIKey) apiKeyJSON {
	return apiKeyJSON{KeyID: k.KeyID, Label: k.Label, CreatedBy: k.CreatedBy, CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt, RevokedAt: k.RevokedAt, LastUsedAt: k.LastUsedAt}
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

type payerJSON struct {
	ID            uuid.UUID `json:"id"`
	ProductID     uuid.UUID `json:"product_id"`
	AccountPrefix string    `json:"account_prefix"`
	ExternalRef   string    `json:"external_ref"`
	AccountNumber string    `json:"account_number"`
	DisplayName   string    `json:"display_name"`
	Email         *string   `json:"email"`
	Status        string    `json:"status"`
	CreditBalance int64     `json:"credit_balance"`
	CreatedAt     time.Time `json:"created_at"`
}

func toPayerRow(r store.ListPayersAdminRow) payerJSON {
	return payerJSON{ID: r.ID, ProductID: r.ProductID, AccountPrefix: r.AccountPrefix, ExternalRef: r.ExternalRef,
		AccountNumber: r.AccountNumber, DisplayName: r.DisplayName, Email: r.Email, Status: string(r.Status),
		CreditBalance: r.CreditBalance, CreatedAt: r.CreatedAt}
}

type phoneJSON struct {
	Phone     string `json:"phone"` // masked
	IsPrimary bool   `json:"is_primary"`
}

type creditMovementJSON struct {
	ID               int64      `json:"id"`
	BatchKind        string     `json:"batch_kind"`
	TransactionID    *uuid.UUID `json:"transaction_id"`
	PaymentRequestID *uuid.UUID `json:"payment_request_id"`
	Amount           int64      `json:"amount"`
	CreatedAt        time.Time  `json:"created_at"`
}

type payerDetailJSON struct {
	payerJSON
	ProductName string               `json:"product_name"`
	UpdatedAt   time.Time            `json:"updated_at"`
	Phones      []phoneJSON          `json:"phones"`
	Credit      []creditMovementJSON `json:"credit_movements"`
}

func toPayerDetail(d reporting.PayerDetail) payerDetailJSON {
	p := d.Payer
	out := payerDetailJSON{
		payerJSON: payerJSON{ID: p.ID, ProductID: p.ProductID, AccountPrefix: p.AccountPrefix, ExternalRef: p.ExternalRef,
			AccountNumber: p.AccountNumber, DisplayName: p.DisplayName, Email: p.Email, Status: string(p.Status),
			CreditBalance: p.CreditBalance, CreatedAt: p.CreatedAt},
		ProductName: p.ProductName, UpdatedAt: p.UpdatedAt, Phones: []phoneJSON{}, Credit: []creditMovementJSON{},
	}
	for _, ph := range d.Phones {
		out.Phones = append(out.Phones, phoneJSON{Phone: tlog.MaskPhone(ph.Phone), IsPrimary: ph.IsPrimary})
	}
	for _, c := range d.Credit {
		out.Credit = append(out.Credit, creditMovementJSON{ID: c.ID, BatchKind: string(c.BatchKind), TransactionID: c.TransactionID,
			PaymentRequestID: c.PaymentRequestID, Amount: c.Amount, CreatedAt: c.CreatedAt})
	}
	return out
}

type requestJSON struct {
	ID               uuid.UUID  `json:"id"`
	ProductID        uuid.UUID  `json:"product_id"`
	AccountPrefix    string     `json:"account_prefix"`
	PayerAccountID   uuid.UUID  `json:"payer_account_id"`
	AccountNumber    string     `json:"account_number"`
	PayerName        string     `json:"payer_name"`
	ProductReference string     `json:"product_reference"`
	Description      string     `json:"description"`
	Amount           int64      `json:"amount"`
	AmountPaid       int64      `json:"amount_paid"`
	Outstanding      int64      `json:"outstanding"`
	Status           string     `json:"status"`
	ExpiresAt        *time.Time `json:"expires_at"`
	CreatedAt        time.Time  `json:"created_at"`
	PaidAt           *time.Time `json:"paid_at"`
	ClosedAt         *time.Time `json:"closed_at"`
}

func toRequestRow(r store.ListPaymentRequestsAdminRow) requestJSON {
	return requestJSON{ID: r.ID, ProductID: r.ProductID, AccountPrefix: r.AccountPrefix, PayerAccountID: r.PayerAccountID,
		AccountNumber: r.AccountNumber, PayerName: r.DisplayName, ProductReference: r.ProductReference, Description: r.Description,
		Amount: r.Amount, AmountPaid: r.AmountPaid, Outstanding: r.Amount - r.AmountPaid, Status: string(r.Status),
		ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt, PaidAt: r.PaidAt, ClosedAt: r.ClosedAt}
}

type requestAllocationJSON struct {
	ID            int64      `json:"id"`
	BatchKind     string     `json:"batch_kind"`
	TransactionID *uuid.UUID `json:"transaction_id"`
	Receipt       *string    `json:"receipt"`
	Amount        int64      `json:"amount"`
	CreatedAt     time.Time  `json:"created_at"`
}

type requestDetailJSON struct {
	requestJSON
	ProductName string                  `json:"product_name"`
	UpdatedAt   time.Time               `json:"updated_at"`
	Allocations []requestAllocationJSON `json:"allocations"`
}

func toRequestDetail(d reporting.RequestDetail) requestDetailJSON {
	r := d.Request
	out := requestDetailJSON{
		requestJSON: requestJSON{ID: r.ID, ProductID: r.ProductID, AccountPrefix: r.AccountPrefix, PayerAccountID: r.PayerAccountID,
			AccountNumber: r.AccountNumber, PayerName: r.DisplayName, ProductReference: r.ProductReference, Description: r.Description,
			Amount: r.Amount, AmountPaid: r.AmountPaid, Outstanding: r.Amount - r.AmountPaid, Status: string(r.Status),
			ExpiresAt: r.ExpiresAt, CreatedAt: r.CreatedAt, PaidAt: r.PaidAt, ClosedAt: r.ClosedAt},
		ProductName: r.ProductName, UpdatedAt: r.UpdatedAt, Allocations: []requestAllocationJSON{},
	}
	for _, a := range d.Allocations {
		out.Allocations = append(out.Allocations, requestAllocationJSON{ID: a.ID, BatchKind: string(a.BatchKind),
			TransactionID: a.TransactionID, Receipt: a.ProviderTransactionID, Amount: a.Amount, CreatedAt: a.CreatedAt})
	}
	return out
}

type txnJSON struct {
	ID               uuid.UUID  `json:"id"`
	ProductID        *uuid.UUID `json:"product_id"`
	AccountPrefix    *string    `json:"account_prefix"`
	Direction        string     `json:"direction"`
	Kind             string     `json:"kind"`
	Channel          string     `json:"channel"`
	Status           string     `json:"status"`
	Amount           int64      `json:"amount"`
	Receipt          *string    `json:"receipt"`
	Phone            *string    `json:"phone"` // masked
	BillRef          *string    `json:"bill_ref"`
	PayerAccountID   *uuid.UUID `json:"payer_account_id"`
	AccountNumber    *string    `json:"account_number"`
	PaymentRequestID *uuid.UUID `json:"payment_request_id"`
	FailureReason    *string    `json:"failure_reason"`
	CreatedAt        time.Time  `json:"created_at"`
	CompletedAt      *time.Time `json:"completed_at"`
}

func maskPtr(p *string) *string {
	if p == nil {
		return nil
	}
	m := tlog.MaskPhone(*p)
	return &m
}

func toTxnRow(r store.ListTransactionsAdminRow) txnJSON {
	return txnJSON{ID: r.ID, ProductID: r.ProductID, AccountPrefix: r.AccountPrefix, Direction: string(r.Direction),
		Kind: string(r.Kind), Channel: string(r.Channel), Status: string(r.Status), Amount: r.Amount,
		Receipt: r.ProviderTransactionID, Phone: maskPtr(r.Phone), BillRef: r.BillRef, PayerAccountID: r.PayerAccountID,
		AccountNumber: r.AccountNumber, PaymentRequestID: r.PaymentRequestID, FailureReason: r.FailureReason,
		CreatedAt: r.CreatedAt, CompletedAt: r.CompletedAt}
}

type txnEventJSON struct {
	FromStatus *string   `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Actor      string    `json:"actor"`
	OccurredAt time.Time `json:"occurred_at"`
}

type txnAllocationJSON struct {
	ID               int64      `json:"id"`
	BatchID          uuid.UUID  `json:"batch_id"`
	BatchKind        string     `json:"batch_kind"`
	LineNo           int32      `json:"line_no"`
	PaymentRequestID *uuid.UUID `json:"payment_request_id"` // null = payer credit
	ProductReference *string    `json:"product_reference"`
	Amount           int64      `json:"amount"`
	CreatedAt        time.Time  `json:"created_at"`
}

type txnDetailJSON struct {
	txnJSON
	ProviderRequestID     *string             `json:"provider_request_id"`
	ReversesTransactionID *uuid.UUID          `json:"reverses_transaction_id"`
	FailureCode           *string             `json:"failure_code"`
	Reason                *string             `json:"reason"`
	CreatedBy             *string             `json:"created_by"`
	OccurredAt            *time.Time          `json:"occurred_at"`
	UpdatedAt             time.Time           `json:"updated_at"`
	PayerName             *string             `json:"payer_name"`
	ProductReference      *string             `json:"product_reference"`
	RawPayload            json.RawMessage     `json:"raw_payload"`
	Events                []txnEventJSON      `json:"events"`
	Allocations           []txnAllocationJSON `json:"allocations"`
}

func toTxnDetail(d reporting.TxnDetail) txnDetailJSON {
	t := d.Txn
	out := txnDetailJSON{
		txnJSON: txnJSON{ID: t.ID, ProductID: t.ProductID, AccountPrefix: t.AccountPrefix, Direction: string(t.Direction),
			Kind: string(t.Kind), Channel: string(t.Channel), Status: string(t.Status), Amount: t.Amount,
			Receipt: t.ProviderTransactionID, Phone: maskPtr(t.Phone), BillRef: t.BillRef, PayerAccountID: t.PayerAccountID,
			AccountNumber: t.AccountNumber, PaymentRequestID: t.PaymentRequestID, FailureReason: t.FailureReason,
			CreatedAt: t.CreatedAt, CompletedAt: t.CompletedAt},
		ProviderRequestID: t.ProviderRequestID, ReversesTransactionID: t.ReversesTransactionID, FailureCode: t.FailureCode,
		Reason: t.Reason, CreatedBy: t.CreatedBy, OccurredAt: t.OccurredAt, UpdatedAt: t.UpdatedAt, PayerName: t.PayerName,
		ProductReference: t.ProductReference, RawPayload: redactPayload(t.RawPayload),
		Events: []txnEventJSON{}, Allocations: []txnAllocationJSON{},
	}
	for _, e := range d.Events {
		var from *string
		if e.FromStatus.Valid {
			s := string(e.FromStatus.TxnStatus)
			from = &s
		}
		out.Events = append(out.Events, txnEventJSON{FromStatus: from, ToStatus: string(e.ToStatus), Actor: e.Actor, OccurredAt: e.OccurredAt})
	}
	for _, a := range d.Allocations {
		out.Allocations = append(out.Allocations, txnAllocationJSON{ID: a.ID, BatchID: a.BatchID, BatchKind: string(a.BatchKind),
			LineNo: a.LineNo, PaymentRequestID: a.PaymentRequestID, ProductReference: a.ProductReference, Amount: a.Amount, CreatedAt: a.CreatedAt})
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
	ProductID     *uuid.UUID      `json:"product_id"`
	AccountPrefix *string         `json:"account_prefix"`
	Details       json.RawMessage `json:"details"`
}

type dashboardWindowJSON struct {
	Total     int64            `json:"total"`
	Payments  int64            `json:"payments"`
	ByProduct map[string]int64 `json:"by_product"` // product id -> KES; nil UUID = unassigned (suspense)
}

func toWindow(w reporting.Window) dashboardWindowJSON {
	out := dashboardWindowJSON{Total: w.Total, Payments: w.Payments, ByProduct: map[string]int64{}}
	for k, v := range w.ByProduct {
		out.ByProduct[k.String()] = v
	}
	return out
}

type dashboardDayJSON struct {
	Day       string           `json:"day"`
	Total     int64            `json:"total"`
	Payments  int64            `json:"payments"`
	ByProduct map[string]int64 `json:"by_product"`
}

type dashboardJSON struct {
	Today       dashboardWindowJSON `json:"today"`
	Last7Days   dashboardWindowJSON `json:"last_7_days"`
	Last30Days  dashboardWindowJSON `json:"last_30_days"`
	Daily       []dashboardDayJSON  `json:"daily"`
	Products    []productJSON       `json:"products"`
	Counts      map[string]int64    `json:"counts"`
	Timezone    string              `json:"timezone"`
	GeneratedAt time.Time           `json:"generated_at"`
}

func toDashboard(d reporting.Dashboard, products []product.Product) dashboardJSON {
	out := dashboardJSON{
		Today: toWindow(d.Today), Last7Days: toWindow(d.Last7Days), Last30Days: toWindow(d.Last30Days),
		Daily: make([]dashboardDayJSON, len(d.Daily)), Products: []productJSON{}, Timezone: "Africa/Nairobi", GeneratedAt: d.GeneratedAt,
		Counts: map[string]int64{
			"open_requests": d.Counts.OpenRequests, "outstanding": d.Counts.Outstanding,
			"suspense_count": d.Counts.SuspenseCount, "suspense_amount": d.Counts.SuspenseAmount,
			"needs_review": d.Counts.NeedsReview, "pending": d.Counts.Pending,
			"credit_held": d.Counts.CreditHeld, "payers": d.Counts.Payers,
		},
	}
	for i, day := range d.Daily {
		bp := map[string]int64{}
		for k, v := range day.ByProduct {
			bp[k.String()] = v
		}
		out.Daily[i] = dashboardDayJSON{Day: day.Day, Total: day.Total, Payments: day.Payments, ByProduct: bp}
	}
	for _, p := range products {
		out.Products = append(out.Products, toProduct(p))
	}
	return out
}

type driftJSON struct {
	Kind   string    `json:"kind"`
	ID     uuid.UUID `json:"id"`
	Cached int64     `json:"cached"`
	Actual int64     `json:"actual"`
}

type integrityJSON struct {
	AuditChainOK bool        `json:"audit_chain_ok"`
	FirstBadID   int64       `json:"first_bad_audit_id,omitempty"`
	LedgerOK     bool        `json:"ledger_ok"`
	Drift        []driftJSON `json:"drift"`
	CheckedAt    time.Time   `json:"checked_at"`
}

func toIntegrity(i reporting.Integrity) integrityJSON {
	out := integrityJSON{AuditChainOK: i.AuditChainOK, FirstBadID: i.FirstBadID, LedgerOK: len(i.Drift) == 0,
		Drift: []driftJSON{}, CheckedAt: i.CheckedAt}
	for _, d := range i.Drift {
		out.Drift = append(out.Drift, driftJSON{Kind: d.Kind, ID: d.ID, Cached: d.Cached, Actual: d.Actual})
	}
	return out
}
