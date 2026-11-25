package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/webhook"
)

// ---------------------------------------------------------------------------
// Webhook deliveries and API key IP allow-lists
// ---------------------------------------------------------------------------

type webhookDeliveryJSON struct {
	EventID        uuid.UUID  `json:"event_id"`
	AppID          uuid.UUID  `json:"app_id"`
	AccountPrefix  string     `json:"account_prefix"`
	EventType      string     `json:"event_type"`
	SubjectType    string     `json:"subject_type"`
	SubjectID      uuid.UUID  `json:"subject_id"`
	Status         string     `json:"status"`
	Attempts       int32      `json:"attempts"`
	LastAttemptAt  *time.Time `json:"last_attempt_at"`
	LastStatusCode *int32     `json:"last_status_code"`
	LastError      *string    `json:"last_error"`
	DeliveredAt    *time.Time `json:"delivered_at"`
	CreatedAt      time.Time  `json:"created_at"`
}

func toWebhookDelivery(d webhook.Delivery) webhookDeliveryJSON {
	return webhookDeliveryJSON{EventID: d.EventID, AppID: d.AppID, AccountPrefix: d.AccountPrefix,
		EventType: d.EventType, SubjectType: d.SubjectType, SubjectID: d.SubjectID, Status: string(d.Status),
		Attempts: d.Attempts, LastAttemptAt: d.LastAttemptAt, LastStatusCode: d.LastStatusCode, LastError: d.LastError,
		DeliveredAt: d.DeliveredAt, CreatedAt: d.CreatedAt}
}

type webhookAttemptJSON struct {
	Attempt      int32     `json:"attempt"`
	AttemptedAt  time.Time `json:"attempted_at"`
	URLHost      string    `json:"url_host"`
	StatusCode   *int32    `json:"status_code"`
	LatencyMs    int32     `json:"latency_ms"`
	Error        *string   `json:"error"`
	ResponseBody *string   `json:"response_body"`
}

type webhookEventJSON struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	CreatedAt time.Time       `json:"created_at"`
	Data      json.RawMessage `json:"data"`
}

type webhookDetailJSON struct {
	Delivery webhookDeliveryJSON  `json:"delivery"`
	Attempts []webhookAttemptJSON `json:"attempts"`
	Event    webhookEventJSON     `json:"event"`
}

func toWebhookDetail(d webhook.Detail) webhookDetailJSON {
	out := webhookDetailJSON{
		Delivery: toWebhookDelivery(d.Delivery), Attempts: make([]webhookAttemptJSON, 0, len(d.Attempts)),
		Event: webhookEventJSON{ID: d.Delivery.EventID, Type: d.Delivery.EventType, CreatedAt: d.EventCreatedAt, Data: d.EventData},
	}
	for _, a := range d.Attempts {
		out.Attempts = append(out.Attempts, toWebhookAttempt(a))
	}
	return out
}

func toWebhookAttempt(a store.WebhookAttempt) webhookAttemptJSON {
	return webhookAttemptJSON{Attempt: a.Attempt, AttemptedAt: a.AttemptedAt, URLHost: a.UrlHost, StatusCode: a.StatusCode,
		LatencyMs: a.LatencyMs, Error: a.Error, ResponseBody: a.ResponseBody}
}

// failWebhook maps webhook-package errors, then defers to fail.
func (h *handlers) failWebhook(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, webhook.ErrNotFound):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusNotFound, "not_found", "No such resource."))
	case errors.Is(err, webhook.ErrNotRedeliverable):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "not_redeliverable",
			"Only failed or skipped deliveries can be redelivered; this one is pending or already delivered."))
	case errors.Is(err, webhook.ErrInvalidFilter):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "invalid_filter",
			strings.TrimPrefix(err.Error(), webhook.ErrInvalidFilter.Error()+": ")))
	default:
		h.fail(w, r, err)
	}
}

func (h *handlers) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	appID, err := queryUUID(q, "app_id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	page, err := h.d.Webhooks.List(r.Context(), webhook.Filter{
		AppID: appID, Status: q.Get("status"), Cursor: q.Get("cursor"), Limit: queryLimit(q),
	})
	if err != nil {
		h.failWebhook(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, mapPage(page, toWebhookDelivery))
}

func (h *handlers) getWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "eventID")
	if !ok {
		return
	}
	d, err := h.d.Webhooks.Get(r.Context(), id)
	if err != nil {
		h.failWebhook(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toWebhookDetail(d))
}

func (h *handlers) redeliverWebhook(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "eventID")
	if !ok {
		return
	}
	d, err := h.d.Webhooks.Redeliver(r.Context(), principalFrom(r.Context()).Actor(), id)
	if err != nil {
		h.failWebhook(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toWebhookDelivery(d))
}

func (h *handlers) setAllowedIPs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	var in struct {
		AllowedIPs *[]string `json:"allowed_ips"`
	}
	if !h.decode(w, r, &in) {
		return
	}
	var list []string
	if in.AllowedIPs != nil {
		list = *in.AllowedIPs
	}
	cidrs, err := app.ParseCIDRs(list)
	if err != nil {
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "invalid_allowed_ips",
			"allowed_ips must be up to 50 IP addresses or CIDR ranges (e.g. 203.0.113.4 or 203.0.113.0/24), or null/[] for any address."))
		return
	}
	if _, err := h.d.Apps.Get(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	k, err := h.d.Apps.SetAllowedCIDRs(r.Context(), principalFrom(r.Context()).Actor(), id, chi.URLParam(r, "keyID"), cidrs)
	if err != nil {
		if errors.Is(err, app.ErrInvalidCIDRs) {
			httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusBadRequest, "invalid_allowed_ips", "Too many allowed IP entries (max 50)."))
			return
		}
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, toAPIKey(k))
}
