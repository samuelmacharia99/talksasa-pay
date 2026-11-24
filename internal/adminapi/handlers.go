package adminapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/admin"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/reporting"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

// decode reads exactly one JSON object with no unknown fields.
func (h *handlers) decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusUnsupportedMediaType, "unsupported_media_type", "Send application/json."))
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusRequestEntityTooLarge, "body_too_large", "Request body too large."))
			return false
		}
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "invalid_json", "Request body must be a single JSON object with the documented fields."))
		return false
	}
	return true
}

func client(r *http.Request) admin.Client {
	c := admin.Client{UserAgent: httpx.ClientUAFrom(r.Context())}
	if ip := httpx.ClientIPFrom(r.Context()); ip.IsValid() {
		c.IP = &ip
	}
	return c
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusNotFound, "not_found", "No such resource."))
		return uuid.UUID{}, false
	}
	return id, true
}

func queryUUID(q url.Values, name string) (*uuid.UUID, error) {
	v := strings.TrimSpace(q.Get(name))
	if v == "" {
		return nil, nil
	}
	id, err := uuid.Parse(v)
	if err != nil {
		return nil, filterError(name + " must be a UUID")
	}
	return &id, nil
}

func queryLimit(q url.Values) int {
	n, _ := strconv.Atoi(q.Get("limit"))
	return n
}

// queryTime accepts RFC 3339 or a calendar date (Africa/Nairobi). A date in
// an upper bound means "through the end of that day".
func queryTime(q url.Values, name string, endOfDay bool) (*time.Time, error) {
	v := strings.TrimSpace(q.Get(name))
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	d, err := time.ParseInLocation(time.DateOnly, v, reporting.Nairobi)
	if err != nil {
		return nil, filterError(name + " must be a date (YYYY-MM-DD) or RFC 3339 time")
	}
	if endOfDay {
		d = d.AddDate(0, 0, 1)
	}
	return &d, nil
}

type filterErr string

func (e filterErr) Error() string  { return string(e) }
func filterError(msg string) error { return filterErr(msg) }

// fail maps service errors to problem responses. Unknown errors are logged
// and returned as a generic 500 so no internal detail leaks.
func (h *handlers) fail(w http.ResponseWriter, r *http.Request, err error) {
	p := func(status int, code, detail string) {
		httpx.WriteProblem(w, r, httpx.NewProblem(status, code, detail))
	}
	var fe filterErr
	switch {
	case errors.As(err, &fe):
		p(http.StatusBadRequest, "invalid_filter", string(fe))
	case errors.Is(err, reporting.ErrInvalidFilter):
		p(http.StatusBadRequest, "invalid_filter", strings.TrimPrefix(err.Error(), reporting.ErrInvalidFilter.Error()+": "))
	case errors.Is(err, admin.ErrInvalidCredentials):
		p(http.StatusUnauthorized, "invalid_credentials", "Sign-in failed. Check your email and password. After several failed attempts the account is locked for a few minutes.")
	case errors.Is(err, admin.ErrInvalidCode):
		p(http.StatusUnauthorized, "invalid_code", "That code is not valid. Use the current code from your authenticator app, or an unused recovery code.")
	case errors.Is(err, admin.ErrSessionInvalid):
		w.Header().Set("WWW-Authenticate", `Bearer realm="talksasa-pay-admin"`)
		p(http.StatusUnauthorized, "session_invalid", "Your session has ended. Please sign in again.")
	case errors.Is(err, admin.ErrInviteInvalid):
		p(http.StatusNotFound, "invite_invalid", "This setup link is invalid, has been used, or has expired. Ask an admin for a new one.")
	case errors.Is(err, admin.ErrEnrolmentInvalid):
		p(http.StatusBadRequest, "enrolment_invalid", "Two-factor enrolment expired or was altered. Reload the page and scan the new QR code.")
	case errors.Is(err, admin.ErrWeakPassword):
		msg, _ := admin.PasswordProblem(err)
		p(http.StatusUnprocessableEntity, "weak_password", msg)
	case errors.Is(err, admin.ErrEmailTaken):
		p(http.StatusConflict, "email_taken", "A staff account with this email already exists.")
	case errors.Is(err, admin.ErrInvalidInput):
		p(http.StatusUnprocessableEntity, "invalid_input", strings.TrimPrefix(err.Error(), admin.ErrInvalidInput.Error()+": "))
	case errors.Is(err, admin.ErrSelfAction):
		p(http.StatusConflict, "self_action", "You cannot do this to your own account.")
	case errors.Is(err, admin.ErrLastAdmin):
		p(http.StatusConflict, "last_admin", "This would leave no active admin. Promote someone else first.")
	case errors.Is(err, admin.ErrAlreadyDisabled):
		p(http.StatusConflict, "already_disabled", "This account is already disabled.")
	case errors.Is(err, admin.ErrNotFound), errors.Is(err, reporting.ErrNotFound), errors.Is(err, product.ErrNotFound):
		p(http.StatusNotFound, "not_found", "No such resource.")
	case errors.Is(err, product.ErrKeyNotFound):
		p(http.StatusNotFound, "key_not_found", "API key not found or already revoked.")
	case errors.Is(err, product.ErrPrefixTaken):
		p(http.StatusConflict, "prefix_taken", "That account prefix is already used by another product.")
	case errors.Is(err, payer.ErrInvalidPrefix):
		p(http.StatusUnprocessableEntity, "invalid_prefix", "Account prefix must be 2-6 capital letters.")
	case errors.Is(err, product.ErrInvalidName):
		p(http.StatusUnprocessableEntity, "invalid_name", "Product name must be 1-100 characters.")
	case errors.Is(err, product.ErrInvalidWebhook):
		p(http.StatusUnprocessableEntity, "invalid_webhook_url", "Webhook URL must be an absolute https:// URL.")
	case errors.Is(err, product.ErrInvalidLabel):
		p(http.StatusUnprocessableEntity, "invalid_label", "Key label must be 1-100 characters.")
	case errors.Is(err, product.ErrInvalidExpiry):
		p(http.StatusUnprocessableEntity, "invalid_expiry", "Key expiry must be in the future.")
	default:
		httpx.InternalError(w, r, h.d.Logger, err)
	}
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

func (h *handlers) login(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	ch, err := h.d.Admin.Login(r.Context(), in.Email, in.Password, client(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"challenge": ch.Token, "expires_at": ch.ExpiresAt})
}

func (h *handlers) verifyMFA(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Challenge string `json:"challenge"`
		Code      string `json:"code"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	s, err := h.d.Admin.VerifyMFA(r.Context(), in.Challenge, in.Code, client(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"session_token": s.Token, "expires_at": s.ExpiresAt, "user": toMe(s.Principal)})
}

func (h *handlers) beginSetup(w http.ResponseWriter, r *http.Request) {
	info, err := h.d.Admin.BeginSetup(r.Context(), chi.URLParam(r, "token"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"email": info.Email, "name": info.Name, "expires_at": info.ExpiresAt,
		"enrolment": info.Enrolment, "totp_secret": info.Secret, "otpauth_uri": info.OTPAuthURI,
	})
}

func (h *handlers) completeSetup(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enrolment string `json:"enrolment"`
		Password  string `json:"password"`
		Code      string `json:"code"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	codes, err := h.d.Admin.CompleteSetup(r.Context(), chi.URLParam(r, "token"), in.Enrolment, in.Password, in.Code, client(r))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

func (h *handlers) me(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, toMe(principalFrom(r.Context())))
}

func (h *handlers) logout(w http.ResponseWriter, r *http.Request) {
	if err := h.d.Admin.Logout(r.Context(), bearer(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) changePassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	if err := h.d.Admin.ChangePassword(r.Context(), principalFrom(r.Context()), in.CurrentPassword, in.NewPassword, client(r)); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

func (h *handlers) dashboard(w http.ResponseWriter, r *http.Request) {
	d, err := h.d.Reporting.Dashboard(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	products, err := h.d.Products.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toDashboard(d, products))
}

func (h *handlers) integrity(w http.ResponseWriter, r *http.Request) {
	i, err := h.d.Reporting.CheckIntegrity(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if !i.AuditChainOK || len(i.Drift) > 0 {
		h.d.Logger.ErrorContext(r.Context(), "INTEGRITY CHECK FAILED",
			slog.Bool("audit_chain_ok", i.AuditChainOK), slog.Int64("first_bad_audit_id", i.FirstBadID), slog.Int("ledger_drift_rows", len(i.Drift)))
	}
	httpx.WriteJSON(w, http.StatusOK, toIntegrity(i))
}

// ---------------------------------------------------------------------------
// Products and API keys
// ---------------------------------------------------------------------------

func (h *handlers) listProducts(w http.ResponseWriter, r *http.Request) {
	ps, err := h.d.Products.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]productJSON, len(ps))
	for i, p := range ps {
		out[i] = toProduct(p)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handlers) createProduct(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string `json:"name"`
		AccountPrefix string `json:"account_prefix"`
		WebhookURL    string `json:"webhook_url"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	c, err := h.d.Products.Create(r.Context(), principalFrom(r.Context()).Actor(), product.CreateParams{
		Name: in.Name, AccountPrefix: in.AccountPrefix, WebhookURL: in.WebhookURL,
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"product": toProduct(c.Product), "webhook_secret": c.WebhookSecret})
}

func (h *handlers) getProduct(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	p, err := h.d.Products.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toProduct(p))
}

func (h *handlers) setWebhookURL(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		WebhookURL string `json:"webhook_url"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	p, err := h.d.Products.SetWebhookURL(r.Context(), principalFrom(r.Context()).Actor(), id, in.WebhookURL)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toProduct(p))
}

func (h *handlers) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	secret, err := h.d.Products.RotateWebhookSecret(r.Context(), principalFrom(r.Context()).Actor(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"webhook_secret": secret})
}

func (h *handlers) setProductStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Status string `json:"status"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	st := store.ProductStatus(in.Status)
	if !st.Valid() {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusUnprocessableEntity, "invalid_status", "Status must be active or suspended."))
		return
	}
	p, err := h.d.Products.SetStatus(r.Context(), principalFrom(r.Context()).Actor(), id, st)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toProduct(p))
}

func (h *handlers) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if _, err := h.d.Products.Get(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	keys, err := h.d.Products.ListAPIKeys(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]apiKeyJSON, len(keys))
	for i, k := range keys {
		out[i] = toAPIKey(k)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handlers) issueAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Label     string     `json:"label"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	if _, err := h.d.Products.Get(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	k, err := h.d.Products.IssueAPIKey(r.Context(), principalFrom(r.Context()).Actor(), id, in.Label, in.ExpiresAt)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"key": toAPIKey(k.Key), "api_key": k.Plaintext})
}

func (h *handlers) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	if err := h.d.Products.RevokeAPIKey(r.Context(), principalFrom(r.Context()).Actor(), id, chi.URLParam(r, "keyID")); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Read-only screens
// ---------------------------------------------------------------------------

func (h *handlers) listPayers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	productID, err := queryUUID(q, "product_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.d.Reporting.ListPayers(r.Context(), reporting.PayerFilter{
		ProductID: productID, Q: q.Get("q"), Cursor: q.Get("cursor"), Limit: queryLimit(q),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mapPage(page, toPayerRow))
}

func (h *handlers) getPayer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	d, err := h.d.Reporting.GetPayer(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toPayerDetail(d))
}

func (h *handlers) listRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	productID, err1 := queryUUID(q, "product_id")
	payerID, err2 := queryUUID(q, "payer_id")
	if err := firstErr(err1, err2); err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.d.Reporting.ListPaymentRequests(r.Context(), reporting.RequestFilter{
		ProductID: productID, PayerID: payerID, Status: q.Get("status"), Q: q.Get("q"), Cursor: q.Get("cursor"), Limit: queryLimit(q),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mapPage(page, toRequestRow))
}

func (h *handlers) getRequest(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	d, err := h.d.Reporting.GetPaymentRequest(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toRequestDetail(d))
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (h *handlers) listTransactions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	productID, e1 := queryUUID(q, "product_id")
	payerID, e2 := queryUUID(q, "payer_id")
	requestID, e3 := queryUUID(q, "request_id")
	from, e4 := queryTime(q, "from", false)
	to, e5 := queryTime(q, "to", true)
	if err := firstErr(e1, e2, e3, e4, e5); err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.d.Reporting.ListTransactions(r.Context(), reporting.TxnFilter{
		ProductID: productID, PayerID: payerID, RequestID: requestID, Channel: q.Get("channel"), Status: q.Get("status"),
		Direction: q.Get("direction"), Receipt: q.Get("receipt"), Phone: q.Get("phone"), From: from, To: to,
		SuspenseOnly: q.Get("suspense") == "true", Cursor: q.Get("cursor"), Limit: queryLimit(q),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mapPage(page, toTxnRow))
}

func (h *handlers) getTransaction(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	d, err := h.d.Reporting.GetTransaction(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toTxnDetail(d))
}

func (h *handlers) listAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	productID, err := queryUUID(q, "product_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.d.Reporting.ListAudit(r.Context(), reporting.AuditFilter{
		ProductID: productID, Action: q.Get("action"), ActorID: q.Get("actor_id"), EntityID: q.Get("entity_id"),
		Cursor: q.Get("cursor"), Limit: queryLimit(q),
	})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	users, err := h.d.Admin.ListUsers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	names := make(map[string]string, len(users))
	for _, u := range users {
		names[u.ID.String()] = u.Name
	}
	httpx.WriteJSON(w, http.StatusOK, mapPage(page, func(a store.ListAuditAdminRow) auditJSON {
		out := auditJSON{ID: a.ID, OccurredAt: a.OccurredAt, ActorType: string(a.ActorType), ActorID: a.ActorID,
			Action: a.Action, EntityType: a.EntityType, EntityID: a.EntityID, ProductID: a.ProductID,
			AccountPrefix: a.AccountPrefix, Details: a.Details}
		if a.ActorType == store.AuditActorTypeAdmin {
			out.ActorName = names[a.ActorID]
		}
		return out
	}))
}

// ---------------------------------------------------------------------------
// Staff users
// ---------------------------------------------------------------------------

func (h *handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	users, err := h.d.Admin.ListUsers(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := make([]userJSON, len(users))
	for i, u := range users {
		out[i] = toUser(u)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *handlers) inviteUser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		Role  string `json:"role"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	inv, err := h.d.Admin.Invite(r.Context(), principalFrom(r.Context()).Actor(), in.Email, in.Name, store.AdminRole(in.Role))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"user": toUser(inv.User), "setup_url": inv.URL, "expires_at": inv.ExpiresAt})
}

func (h *handlers) setUserRole(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		Role string `json:"role"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	p := principalFrom(r.Context())
	u, err := h.d.Admin.SetRole(r.Context(), p.Actor(), &p.UserID, id, store.AdminRole(in.Role))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toUser(u))
}

func (h *handlers) disableUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	if err := h.d.Admin.Disable(r.Context(), p.Actor(), &p.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) resetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	p := principalFrom(r.Context())
	inv, err := h.d.Admin.Reset(r.Context(), p.Actor(), &p.UserID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": toUser(inv.User), "setup_url": inv.URL, "expires_at": inv.ExpiresAt})
}
