package productapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// MaxAmount is the largest single payment request (KES).
const MaxAmount = 1_000_000_000

func (h *handlers) q() *store.Queries { return store.New(h.d.Pool) }

// ---------------------------------------------------------------------------
// whoami
// ---------------------------------------------------------------------------

type whoAmIJSON struct {
	ProductID     uuid.UUID `json:"product_id"`
	ProductName   string    `json:"product_name"`
	AccountPrefix string    `json:"account_prefix"`
	KeyID         string    `json:"key_id"`
}

func (h *handlers) whoami(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	httpx.WriteJSON(w, http.StatusOK, whoAmIJSON{
		ProductID: p.ProductID, ProductName: p.ProductName, AccountPrefix: p.AccountPrefix, KeyID: p.KeyID,
	})
}

// ---------------------------------------------------------------------------
// Payers
// ---------------------------------------------------------------------------

// externalRef reads and validates {external_ref}. It must be exactly what
// the product stores: 1-128 bytes of clean UTF-8 with no surrounding spaces
// (the service would otherwise trim it into a different key).
func externalRef(w http.ResponseWriter, r *http.Request) (string, bool) {
	ref, ok := pathParam(r, "external_ref")
	if ok {
		ok = validExternalRef(ref)
	}
	if !ok {
		invalid(w, r, http.StatusUnprocessableEntity,
			fieldErr("external_ref", "must be 1-128 characters with no control characters or surrounding spaces"))
		return "", false
	}
	return ref, true
}

func validExternalRef(ref string) bool {
	return ref != "" && len(ref) <= 128 && strings.TrimSpace(ref) == ref && cleanText(ref)
}

type payerInput struct {
	DisplayName *string  `json:"display_name"`
	Email       *string  `json:"email"`
	Phones      []string `json:"phones"`
}

func (h *handlers) upsertPayer(w http.ResponseWriter, r *http.Request) {
	ref, ok := externalRef(w, r)
	if !ok {
		return
	}
	var in payerInput
	if !decode(w, r, &in) {
		return
	}
	var errs []httpx.FieldError
	switch {
	case in.DisplayName == nil:
		errs = append(errs, fieldErr("display_name", "is required"))
	case strings.TrimSpace(*in.DisplayName) == "" || len(*in.DisplayName) > 200 || !cleanText(*in.DisplayName):
		errs = append(errs, fieldErr("display_name", "must be 1-200 characters with no control characters"))
	}
	email := ""
	if in.Email != nil {
		email = *in.Email
		if len(email) > 254 {
			errs = append(errs, fieldErr("email", "must be at most 254 characters"))
		}
	}
	if len(in.Phones) > 5 {
		errs = append(errs, fieldErr("phones", "at most 5 phone numbers"))
	}
	if len(errs) > 0 {
		invalid(w, r, http.StatusUnprocessableEntity, errs...)
		return
	}
	productID := principalFrom(r.Context()).ProductID
	p, created, err := h.d.Payers.Upsert(r.Context(), audit.Product(productID), productID, payer.UpsertParams{
		ExternalRef: ref, DisplayName: *in.DisplayName, Email: email, Phones: in.Phones,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	var emailPtr *string
	if p.Email != "" {
		emailPtr = &p.Email
	}
	httpx.WriteJSON(w, status, h.d.View.Payer(store.PayerAccount{
		ID: p.ID, ProductID: p.ProductID, ExternalRef: p.ExternalRef, AccountNumber: p.AccountNumber,
		DisplayName: p.DisplayName, Email: emailPtr, Status: p.Status, CreditBalance: int64(p.CreditBalance),
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}, p.Phones))
}

func (h *handlers) getPayer(w http.ResponseWriter, r *http.Request) {
	ref, ok := externalRef(w, r)
	if !ok {
		return
	}
	productID := principalFrom(r.Context()).ProductID
	q := h.q()
	row, err := q.GetPayerByExternalRef(r.Context(), store.GetPayerByExternalRefParams{ProductID: productID, ExternalRef: ref})
	if db.IsNotFound(err) {
		h.fail(w, r, payer.ErrNotFound)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	phones, err := q.ListPayerPhones(r.Context(), store.ListPayerPhonesParams{ProductID: productID, PayerAccountID: row.ID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	nums := make([]string, len(phones))
	for i, ph := range phones {
		nums[i] = ph.Phone
	}
	httpx.WriteJSON(w, http.StatusOK, h.d.View.Payer(row, nums))
}

// ---------------------------------------------------------------------------
// Payment requests
// ---------------------------------------------------------------------------

type paymentRequestInput struct {
	Payer       *string `json:"payer"`
	Reference   *string `json:"reference"`
	Description *string `json:"description"`
	Amount      *int64  `json:"amount"`
	ExpiresAt   *string `json:"expires_at"`
}

func validIdempotencyKey(k string) bool {
	return len(k) <= 255 && utf8Printable(k)
}

func utf8Printable(s string) bool {
	return cleanText(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

func (h *handlers) createPaymentRequest(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		problem(w, r, http.StatusBadRequest, "idempotency_key_required",
			"Send an Idempotency-Key header (unique per logical request) so retries are safe.")
		return
	}
	if !validIdempotencyKey(key) {
		invalid(w, r, http.StatusBadRequest, fieldErr("Idempotency-Key", "must be 1-255 printable characters"))
		return
	}
	var in paymentRequestInput
	if !decode(w, r, &in) {
		return
	}

	var errs []httpx.FieldError
	text := func(field string, v *string, maxLen int, clean func(string) bool) {
		switch {
		case v == nil:
			errs = append(errs, fieldErr(field, "is required"))
		case strings.TrimSpace(*v) == "" || len(*v) > maxLen || !clean(*v):
			errs = append(errs, fieldErr(field, "must be 1-"+strconv.Itoa(maxLen)+" characters with no control characters"))
		}
	}
	if in.Payer == nil {
		errs = append(errs, fieldErr("payer", "is required"))
	} else if !validExternalRef(*in.Payer) {
		errs = append(errs, fieldErr("payer", "must be a payer external_ref (1-128 characters)"))
	}
	text("reference", in.Reference, 128, cleanText)
	text("description", in.Description, 500, cleanMultiline)
	switch {
	case in.Amount == nil:
		errs = append(errs, fieldErr("amount", "is required"))
	case *in.Amount < 1 || *in.Amount > MaxAmount:
		errs = append(errs, fieldErr("amount", "must be a whole number of KES from 1 to 1000000000"))
	}
	var expiresAt *time.Time
	if in.ExpiresAt != nil {
		t, err := time.Parse(time.RFC3339Nano, *in.ExpiresAt)
		if err != nil {
			errs = append(errs, fieldErr("expires_at", "must be an RFC 3339 date-time, e.g. 2026-12-31T23:59:59Z"))
		} else {
			expiresAt = &t
		}
	}
	if len(errs) > 0 {
		invalid(w, r, http.StatusUnprocessableEntity, errs...)
		return
	}

	ctx := r.Context()
	productID := principalFrom(ctx).ProductID
	q := h.q()
	pa, err := q.GetPayerByExternalRef(ctx, store.GetPayerByExternalRefParams{ProductID: productID, ExternalRef: *in.Payer})
	if db.IsNotFound(err) {
		h.fail(w, r, paymentrequest.ErrPayerNotFound)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	created, err := h.d.Requests.Create(ctx, audit.Product(productID), productID, paymentrequest.CreateParams{
		PayerAccountID: pa.ID, ProductReference: *in.Reference, Description: *in.Description,
		Amount: *in.Amount, ExpiresAt: expiresAt, IdempotencyKey: key,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	row, err := q.GetPaymentRequestForProduct(ctx, store.GetPaymentRequestForProductParams{ProductID: productID, ID: created.Request.ID})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if created.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	httpx.WriteJSON(w, status, h.d.View.PaymentRequest(row))
}

type listJSON[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

// resolvePayerFilter turns ?payer=<external_ref> into a payer id. found is
// false when no such payer exists (the list is then empty, not an error).
func (h *handlers) resolvePayerFilter(r *http.Request, q *store.Queries, ref string) (id *uuid.UUID, found bool, err error) {
	if ref == "" {
		return nil, true, nil
	}
	pa, err := q.GetPayerByExternalRef(r.Context(), store.GetPayerByExternalRefParams{
		ProductID: principalFrom(r.Context()).ProductID, ExternalRef: ref,
	})
	if db.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return &pa.ID, true, nil
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

func (h *handlers) listPaymentRequests(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	var errs []httpx.FieldError
	limit, fe := queryLimit(qs)
	if fe != nil {
		errs = append(errs, *fe)
	}
	cursorAt, cursorID, fe := timeCursor(cursorRequests, qs.Get("cursor"))
	if fe != nil {
		errs = append(errs, *fe)
	}
	var status store.NullPaymentRequestStatus
	if s := qs.Get("status"); s != "" {
		st := store.PaymentRequestStatus(s)
		if !st.Valid() {
			errs = append(errs, fieldErr("status", "must be one of open, partially_paid, paid, cancelled, expired"))
		}
		status = store.NullPaymentRequestStatus{PaymentRequestStatus: st, Valid: true}
	}
	var reference *string
	if ref := qs.Get("reference"); ref != "" {
		if len(ref) > 128 || !cleanText(ref) {
			errs = append(errs, fieldErr("reference", "must be at most 128 characters"))
		}
		reference = &ref
	}
	payerRef := qs.Get("payer")
	if payerRef != "" && !validExternalRef(payerRef) {
		errs = append(errs, fieldErr("payer", "must be a payer external_ref (1-128 characters)"))
	}
	if len(errs) > 0 {
		invalid(w, r, http.StatusBadRequest, errs...)
		return
	}

	q := h.q()
	payerID, found, err := h.resolvePayerFilter(r, q, payerRef)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := listJSON[apiview.PaymentRequest]{Data: []apiview.PaymentRequest{}}
	if !found {
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	rows, err := q.ListPaymentRequestsForProduct(r.Context(), store.ListPaymentRequestsForProductParams{
		ProductID: principalFrom(r.Context()).ProductID, Status: status, Reference: reference, PayerID: payerID,
		CursorAt: cursorAt, CursorID: cursorID, Lim: int32(limit + 1),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		c := encodeCursor(cursorRequests, last.CreatedAt.UnixMicro(), last.ID)
		out.NextCursor = &c
	}
	out.Data = make([]apiview.PaymentRequest, len(rows))
	for i, row := range rows {
		out.Data[i] = h.d.View.PaymentRequest(apiview.FromListRow(row))
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *handlers) getPaymentRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	h.writePaymentRequest(w, r, id)
}

func (h *handlers) writePaymentRequest(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	row, err := h.q().GetPaymentRequestForProduct(r.Context(), store.GetPaymentRequestForProductParams{
		ProductID: principalFrom(r.Context()).ProductID, ID: id,
	})
	if db.IsNotFound(err) {
		h.fail(w, r, paymentrequest.ErrNotFound)
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.d.View.PaymentRequest(row))
}

func (h *handlers) cancelPaymentRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	productID := principalFrom(r.Context()).ProductID
	// AlreadyCancelled (an idempotent retry) is also 200.
	if _, err := h.d.Requests.Cancel(r.Context(), audit.Product(productID), productID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	h.writePaymentRequest(w, r, id)
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
	payerRef := qs.Get("payer")
	if payerRef != "" && !validExternalRef(payerRef) {
		errs = append(errs, fieldErr("payer", "must be a payer external_ref (1-128 characters)"))
	}
	if len(errs) > 0 {
		invalid(w, r, http.StatusBadRequest, errs...)
		return
	}

	ctx := r.Context()
	productID := principalFrom(ctx).ProductID
	q := h.q()
	payerID, found, err := h.resolvePayerFilter(r, q, payerRef)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := listJSON[apiview.Transaction]{Data: []apiview.Transaction{}}
	if !found {
		httpx.WriteJSON(w, http.StatusOK, out)
		return
	}
	rows, err := q.ListTransactionsForProduct(ctx, store.ListTransactionsForProductParams{
		ProductID: &productID, PayerID: payerID, Since: since, CursorAt: cursorAt, CursorID: cursorID,
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
	// All allocation lines for the page in one query (no N+1).
	allocs := map[uuid.UUID][]store.ListAllocationsForTransactionProductRow{}
	if len(rows) > 0 {
		ids := make([]uuid.UUID, len(rows))
		for i, t := range rows {
			ids[i] = t.ID
		}
		lines, err := q.ListAllocationsForTransactionsProduct(ctx, store.ListAllocationsForTransactionsProductParams{
			ProductID: productID, TransactionIds: ids,
		})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for _, l := range lines {
			if l.TransactionID == nil {
				continue
			}
			allocs[*l.TransactionID] = append(allocs[*l.TransactionID], store.ListAllocationsForTransactionProductRow{
				BatchID: l.BatchID, LineNo: l.LineNo, PaymentRequestID: l.PaymentRequestID, Amount: l.Amount,
				BatchKind: l.BatchKind, CreatedAt: l.CreatedAt,
			})
		}
	}
	out.Data = make([]apiview.Transaction, len(rows))
	for i, t := range rows {
		out.Data[i] = h.d.View.Transaction(apiview.FromTxnListRow(t), allocs[t.ID])
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *handlers) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	productID := principalFrom(ctx).ProductID
	q := h.q()
	t, err := q.GetTransactionViewForProduct(ctx, store.GetTransactionViewForProductParams{ProductID: &productID, ID: id})
	if db.IsNotFound(err) {
		problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	allocs, err := q.ListAllocationsForTransactionProduct(ctx, store.ListAllocationsForTransactionProductParams{ProductID: productID, TransactionID: &id})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.d.View.Transaction(t, allocs))
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
		ProductID: principalFrom(r.Context()).ProductID, AfterTxid: afterTxid, AfterID: afterID,
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
