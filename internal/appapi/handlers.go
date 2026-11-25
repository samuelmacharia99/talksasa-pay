package appapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/store"
)

func (h *handlers) q() *store.Queries { return store.New(h.d.Pool) }

// ---------------------------------------------------------------------------
// whoami
// ---------------------------------------------------------------------------

type whoAmIJSON struct {
	AppID         uuid.UUID `json:"app_id"`
	AppName       string    `json:"app_name"`
	AccountPrefix string    `json:"account_prefix"`
	KeyID         string    `json:"key_id"`
}

func (h *handlers) whoami(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, whoAmIJSON{
		AppID: p.AppID, AppName: p.AppName, AccountPrefix: p.AccountPrefix, KeyID: p.KeyID,
	})
}

// ---------------------------------------------------------------------------
// Charges
// ---------------------------------------------------------------------------

func validIdempotencyKey(k string) bool {
	return len(k) <= 255 && utf8Printable(k)
}

func utf8Printable(s string) bool {
	return cleanText(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// notImplemented answers routes whose behaviour has not landed yet.
// TODO(gateway-core): remove once every route is implemented.
func notImplemented(w http.ResponseWriter, r *http.Request) {
	problem(w, r, http.StatusNotImplemented, "not_implemented", "This endpoint is not implemented yet.")
}

// createCharge: POST /charges (Idempotency-Key required; 201, or 200 with
// Idempotent-Replayed on a replay). With a phone, the first prompt is sent
// and embedded as latest_stk_push.
func (h *handlers) createCharge(w http.ResponseWriter, r *http.Request) {
	// TODO(gateway-core): validate body, h.d.Charges.Create, render
	// h.d.View.Charge(created.Charge, created.Push).
	notImplemented(w, r)
}

// listCharges: GET /charges?status=&reference=&limit=&cursor= (cursorCharges).
func (h *handlers) listCharges(w http.ResponseWriter, r *http.Request) {
	// TODO(gateway-core): h.d.Charges.List + store.ListLatestSTKPushesForCharges.
	notImplemented(w, r)
}

// getCharge: GET /charges/{id}.
func (h *handlers) getCharge(w http.ResponseWriter, r *http.Request) {
	// TODO(gateway-core): h.d.Charges.Get + GetLatestSTKPushForCharge.
	notImplemented(w, r)
}

// cancelCharge: POST /charges/{id}/cancel (an idempotent retry is also 200).
func (h *handlers) cancelCharge(w http.ResponseWriter, r *http.Request) {
	// TODO(gateway-core): h.d.Charges.Cancel.
	notImplemented(w, r)
}

type listJSON[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// timeCursor parses ?cursor= for created_at-ordered lists.
func timeCursor(kind cursorKind, raw string) (at *time.Time, id *uuid.UUID, fe *httpx.FieldError) {
	if raw == "" {
		return nil, nil, nil
	}
	n, cid, err := decodeCursor(kind, raw)
	if err != nil {
		e := fieldErr("cursor", "is not a cursor returned by this endpoint")
		return nil, nil, &e
	}
	t := time.UnixMicro(n).UTC()
	return &t, &cid, nil
}

// ---------------------------------------------------------------------------
// Transactions
// ---------------------------------------------------------------------------

func (h *handlers) listTransactions(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	var errs []httpx.FieldError
	limit, fe := queryLimit(qs)
	if fe != nil {
		errs = append(errs, *fe)
	}
	cursorAt, cursorID, fe := timeCursor(cursorTransactions, qs.Get("cursor"))
	if fe != nil {
		errs = append(errs, *fe)
	}
	var since *time.Time
	if s := qs.Get("since"); s != "" {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			errs = append(errs, fieldErr("since", "must be an RFC 3339 date-time, e.g. 2026-11-01T00:00:00Z"))
		}
		since = &t
	}
	var chargeID *uuid.UUID
	if c := qs.Get("charge_id"); c != "" {
		id, err := uuid.Parse(c)
		if err != nil {
			errs = append(errs, fieldErr("charge_id", "must be a charge id"))
		}
		chargeID = &id
	}
	if len(errs) > 0 {
		invalid(w, r, http.StatusBadRequest, errs...)
		return
	}

	ctx := r.Context()
	appID := principalFrom(ctx).AppID
	q := h.q()
	out := listJSON[apiview.Transaction]{Data: []apiview.Transaction{}}
	rows, err := q.ListTransactionsForApp(ctx, store.ListTransactionsForAppParams{
		AppID: &appID, ChargeID: chargeID, Since: since, CursorAt: cursorAt, CursorID: cursorID,
		Lim: int32(limit + 1),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		c := encodeCursor(cursorTransactions, last.CreatedAt.UnixMicro(), last.ID)
		out.NextCursor = &c
	}
	out.Data = make([]apiview.Transaction, len(rows))
	for i, t := range rows {
		out.Data[i] = h.d.View.Transaction(apiview.FromTxnListRow(t))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *handlers) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	appID := principalFrom(ctx).AppID
	q := h.q()
	t, err := q.GetTransactionViewForApp(ctx, store.GetTransactionViewForAppParams{AppID: &appID, ID: id})
	if db.IsNotFound(err) {
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.d.View.Transaction(t))
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

type eventsJSON struct {
	Data       []apiview.Event `json:"data"`
	NextCursor *string         `json:"next_cursor"`
	HasMore    bool            `json:"has_more"`
}

func (h *handlers) listEvents(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	var errs []httpx.FieldError
	limit, fe := queryLimit(qs)
	if fe != nil {
		errs = append(errs, *fe)
	}
	var afterTxid int64
	afterID := uuid.Nil
	if a := qs.Get("after"); a != "" {
		n, id, err := decodeCursor(cursorEvents, a)
		if err != nil {
			errs = append(errs, fieldErr("after", "is not a cursor returned by this endpoint"))
		}
		afterTxid, afterID = n, id
	}
	if len(errs) > 0 {
		invalid(w, r, http.StatusBadRequest, errs...)
		return
	}
	rows, err := h.q().ListEventsFeed(r.Context(), store.ListEventsFeedParams{
		AppID: principalFrom(r.Context()).AppID, AfterTxid: afterTxid, AfterID: afterID,
		Lim: int32(limit + 1),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := eventsJSON{Data: make([]apiview.Event, 0, min(len(rows), limit))}
	if len(rows) > limit {
		rows, out.HasMore = rows[:limit], true
	}
	for _, ev := range rows {
		out.Data = append(out.Data, apiview.Event{
			Object: "event", ID: ev.ID, Type: ev.Type, APIVersion: apiview.APIVersion,
			CreatedAt: ev.CreatedAt.UTC(), Data: json.RawMessage(ev.Data),
		})
	}
	if len(rows) > 0 {
		last := rows[len(rows)-1]
		c := encodeCursor(cursorEvents, last.Txid, last.ID)
		out.NextCursor = &c
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
