package appapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/accountref"
	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/charge"
	"github.com/talksasa/talksasa-pay/internal/phone"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// ChargeService is the part of *charge.Service the app API uses (an
// interface so handler tests can use a fake). Leave Deps.Charges a nil
// interface (not a typed nil *charge.Service) when there is none.
type ChargeService interface {
	Create(ctx context.Context, actor audit.Actor, appID uuid.UUID, p charge.CreateParams) (charge.Created, error)
	Get(ctx context.Context, appID, id uuid.UUID) (charge.Charge, error)
	List(ctx context.Context, appID uuid.UUID, p charge.ListParams) ([]charge.Charge, error)
	Cancel(ctx context.Context, actor audit.Actor, appID, id uuid.UUID) (charge.Cancelled, error)
}

var _ ChargeService = (*charge.Service)(nil)

// PushReader reads the latest prompt of charges (*store.Queries). Callers
// pass only charge ids already scoped to the key's app.
type PushReader interface {
	GetLatestSTKPushForCharge(ctx context.Context, chargeID uuid.UUID) (store.StkPush, error)
	ListLatestSTKPushesForCharges(ctx context.Context, arg store.ListLatestSTKPushesForChargesParams) ([]store.StkPush, error)
}

var _ PushReader = (*store.Queries)(nil)

func validIdempotencyKey(k string) bool {
	return len(k) <= 255 && utf8Printable(k)
}

func utf8Printable(s string) bool {
	return cleanText(s) && strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) }) < 0
}

// idempotencyKey reads the required Idempotency-Key header. It writes the
// problem and returns false when it is missing or malformed.
func idempotencyKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		problem(w, r, http.StatusBadRequest, "idempotency_key_required",
			"Send an Idempotency-Key header (unique per logical request) so retries are safe.")
		return "", false
	}
	if !validIdempotencyKey(key) {
		invalid(w, r, http.StatusBadRequest, fieldErr("Idempotency-Key", "must be 1-255 printable characters"))
		return "", false
	}
	return key, true
}

// notImplemented answers charge routes when no charge service is wired.
func notImplemented(w http.ResponseWriter, r *http.Request) {
	problem(w, r, http.StatusNotImplemented, "not_implemented", "This endpoint is not implemented yet.")
}

// chargeInput is the body of POST /charges (ChargeInput in the spec).
// Pointers tell "absent" from "zero".
type chargeInput struct {
	Reference     *string         `json:"reference"`
	Amount        *int64          `json:"amount"`
	Description   *string         `json:"description"`
	Phone         *string         `json:"phone"`
	AccountNumber *string         `json:"account_number"`
	Metadata      json.RawMessage `json:"metadata"`
	ExpiresIn     *int64          `json:"expires_in"`
}

// validate checks the body against the contract and returns every field
// error at once. The service re-validates (it is the authority); this
// gives callers a complete errors[] list in one round trip.
func (in chargeInput) validate(prefix string) (charge.CreateParams, []httpx.FieldError) {
	var p charge.CreateParams
	var errs []httpx.FieldError
	add := func(field, msg string) { errs = append(errs, fieldErr(field, msg)) }

	switch {
	case in.Reference == nil:
		add("reference", "is required")
	case strings.TrimSpace(*in.Reference) == "" || utf8.RuneCountInString(*in.Reference) > charge.MaxReferenceLen || !cleanText(*in.Reference):
		add("reference", "must be 1-128 characters, not blank, without control characters")
	default:
		p.Reference = *in.Reference
	}

	switch {
	case in.Amount == nil:
		add("amount", "is required")
	case *in.Amount < charge.MinAmount || *in.Amount > charge.MaxAmount:
		add("amount", "must be a whole number of KES from 1 to 250000")
	default:
		p.Amount = *in.Amount
	}

	if in.Description != nil {
		d := *in.Description
		if strings.TrimSpace(d) == "" || utf8.RuneCountInString(d) > charge.MaxDescription || !cleanMultiline(d) {
			add("description", "must be 1-500 characters, not blank, without control characters")
		} else {
			p.Description = d
		}
	}

	if in.Phone != nil {
		if len(*in.Phone) > maxPhoneInput {
			add("phone", phoneMessage)
		} else if _, err := phone.Normalize(*in.Phone); err != nil {
			add("phone", phoneMessage)
		} else {
			p.Phone = *in.Phone
		}
	}

	if in.AccountNumber != nil {
		if err := accountref.Validate(prefix, *in.AccountNumber); err != nil {
			add("account_number", "must be your prefix "+prefix+", a hyphen, a digit, then A-Z or 0-9, at most 12 characters")
		} else {
			p.AccountNumber = *in.AccountNumber
		}
	}

	if m := bytes.TrimSpace(in.Metadata); len(m) > 0 && !bytes.Equal(m, []byte("null")) {
		var buf bytes.Buffer
		switch {
		case m[0] != '{':
			add("metadata", "must be a JSON object")
		case json.Compact(&buf, m) != nil:
			add("metadata", "must be a JSON object")
		case buf.Len() > charge.MaxMetadataBytes:
			add("metadata", "must be at most 4096 bytes")
		default:
			p.Metadata = json.RawMessage(buf.Bytes())
		}
	}

	if in.ExpiresIn != nil {
		d := time.Duration(*in.ExpiresIn) * time.Second
		if *in.ExpiresIn < int64(charge.MinExpiresIn/time.Second) || *in.ExpiresIn > int64(charge.MaxExpiresIn/time.Second) {
			add("expires_in", "must be from 900 to 604800 seconds")
		} else {
			p.ExpiresIn = d
		}
	}
	return p, errs
}

// createCharge: POST /charges (Idempotency-Key required; 201, or 200 with
// Idempotent-Replayed on a replay). With a phone, the first prompt is sent
// and embedded as latest_stk_push; a prompt that could not be sent does not
// fail the request (the charge exists and can be prompted again).
func (h *handlers) createCharge(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(w, r)
	if !ok {
		return
	}
	var in chargeInput
	if !decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	pr := principalFrom(ctx)
	p, errs := in.validate(pr.AccountPrefix)
	if len(errs) > 0 {
		invalid(w, r, http.StatusUnprocessableEntity, errs...)
		return
	}
	if h.d.Charges == nil {
		notImplemented(w, r)
		return
	}
	p.IdempotencyKey = key
	created, err := h.d.Charges.Create(ctx, audit.App(pr.AppID), pr.AppID, p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if created.Charge.AppID != pr.AppID { // defence in depth: never render another app's charge
		h.fail(w, r, errCrossApp)
		return
	}
	if created.PushErr != nil {
		// The charge exists; the app sees the failed (or absent) push.
		h.d.Logger.WarnContext(ctx, "charge created but its first prompt was not sent",
			slog.String("charge_id", created.Charge.ID.String()), slog.String("error", created.PushErr.Error()))
	}
	push := created.Push
	if push == nil {
		push = h.latestPush(ctx, created.Charge.ID)
	}
	status := http.StatusCreated
	if created.Replayed {
		status = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	httpx.WriteJSON(w, status, h.d.View.Charge(created.Charge, push))
}

// latestPush is the charge's newest prompt, or nil. A lookup failure is
// logged and rendered as no prompt: the charge itself is what matters.
func (h *handlers) latestPush(ctx context.Context, chargeID uuid.UUID) *store.StkPush {
	if h.d.Pushes == nil {
		return nil
	}
	p, err := h.d.Pushes.GetLatestSTKPushForCharge(ctx, chargeID)
	if err != nil {
		if !db.IsNotFound(err) {
			h.d.Logger.ErrorContext(ctx, "latest stk push lookup failed",
				slog.String("charge_id", chargeID.String()), slog.String("error", err.Error()))
		}
		return nil
	}
	return &p
}

// listCharges: GET /charges?status=&reference=&limit=&cursor=, newest first.
func (h *handlers) listCharges(w http.ResponseWriter, r *http.Request) {
	qs := r.URL.Query()
	var errs []httpx.FieldError
	limit, fe := queryLimit(qs)
	if fe != nil {
		errs = append(errs, *fe)
	}
	cursorAt, cursorID, fe := timeCursor(cursorCharges, qs.Get("cursor"))
	if fe != nil {
		errs = append(errs, *fe)
	}
	lp := charge.ListParams{CursorAt: cursorAt, CursorID: cursorID, Limit: limit + 1}
	if s := qs.Get("status"); s != "" {
		st := store.ChargeStatus(s)
		if !st.Valid() {
			errs = append(errs, fieldErr("status", "must be one of pending, needs_review, succeeded, expired, cancelled, reversed"))
		}
		lp.Status = &st
	}
	if ref := qs.Get("reference"); ref != "" {
		if utf8.RuneCountInString(ref) > charge.MaxReferenceLen || !cleanText(ref) {
			errs = append(errs, fieldErr("reference", "must be at most 128 characters"))
		}
		lp.Reference = ref
	}
	if len(errs) > 0 {
		invalid(w, r, http.StatusBadRequest, errs...)
		return
	}
	if h.d.Charges == nil {
		notImplemented(w, r)
		return
	}
	ctx := r.Context()
	appID := principalFrom(ctx).AppID
	rows, err := h.d.Charges.List(ctx, appID, lp)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := listJSON[apiview.Charge]{Data: []apiview.Charge{}}
	if len(rows) > limit {
		rows = rows[:limit]
		last := rows[len(rows)-1]
		c := encodeCursor(cursorCharges, last.CreatedAt.UnixMicro(), last.ID)
		out.NextCursor = &c
	}
	ids := make([]uuid.UUID, 0, len(rows))
	for _, c := range rows {
		if c.AppID != appID { // defence in depth
			h.fail(w, r, errCrossApp)
			return
		}
		ids = append(ids, c.ID)
	}
	latest := map[uuid.UUID]*store.StkPush{}
	if len(ids) > 0 && h.d.Pushes != nil {
		pushes, err := h.d.Pushes.ListLatestSTKPushesForCharges(ctx, store.ListLatestSTKPushesForChargesParams{AppID: appID, ChargeIds: ids})
		if err != nil {
			h.fail(w, r, err)
			return
		}
		for i := range pushes {
			latest[pushes[i].ChargeID] = &pushes[i]
		}
	}
	out.Data = make([]apiview.Charge, len(rows))
	for i, c := range rows {
		out.Data[i] = h.d.View.Charge(c, latest[c.ID])
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// getCharge: GET /charges/{id} (another app's charge is 404).
func (h *handlers) getCharge(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if h.d.Charges == nil {
		notImplemented(w, r)
		return
	}
	ctx := r.Context()
	appID := principalFrom(ctx).AppID
	c, err := h.d.Charges.Get(ctx, appID, id)
	if err == nil && c.AppID != appID {
		err = charge.ErrNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.d.View.Charge(c, h.latestPush(ctx, c.ID)))
}

// cancelCharge: POST /charges/{id}/cancel (an idempotent retry is also 200).
func (h *handlers) cancelCharge(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	if h.d.Charges == nil {
		notImplemented(w, r)
		return
	}
	ctx := r.Context()
	appID := principalFrom(ctx).AppID
	res, err := h.d.Charges.Cancel(ctx, audit.App(appID), appID, id)
	if err == nil && res.Charge.AppID != appID {
		err = charge.ErrNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.d.View.Charge(res.Charge, h.latestPush(ctx, res.Charge.ID)))
}
