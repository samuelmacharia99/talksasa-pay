package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesasettings"
	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
)

// SettingsService is the Settings page's backend
// (*mpesasettings.Service implements it; tests use a fake).
type SettingsService interface {
	Status(ctx context.Context) (mpesasettings.Status, error)
	Save(ctx context.Context, actor audit.Actor, p mpesasettings.SaveParams) (mpesasettings.Status, error)
	Test(ctx context.Context, actor audit.Actor) (mpesasettings.TestResult, error)
	RegisterURLs(ctx context.Context, actor audit.Actor, confirm string) (mpesasettings.RegisterResult, error)
}

// The settings JSON never carries a credential value: only whether it is
// set and, for long values, its last 4 characters.

type credentialJSON struct {
	Set   bool    `json:"set"`
	Last4 *string `json:"last4"`
}

type callbackURLsJSON struct {
	STK          string `json:"stk"`
	Confirmation string `json:"confirmation"`
	Validation   string `json:"validation"`
}

type registrationJSON struct {
	RegisteredAt     time.Time `json:"registered_at"`
	RegisteredBy     string    `json:"registered_by"`
	RegisteredByName *string   `json:"registered_by_name"`
	ConfirmationURL  string    `json:"confirmation_url"`
	Shortcode        string    `json:"shortcode"`
	Current          bool      `json:"current"`
}

type settingsTestJSON struct {
	OK       bool      `json:"ok"`
	TestedAt time.Time `json:"tested_at"`
	Message  *string   `json:"message"`
}

type mpesaSettingsJSON struct {
	Source          string                    `json:"source"`
	Environment     *string                   `json:"environment"`
	Shortcode       *string                   `json:"shortcode"`
	STKShortcode    *string                   `json:"stk_shortcode"`
	CallbackBaseURL *string                   `json:"callback_base_url"`
	Credentials     map[string]credentialJSON `json:"credentials"`
	// CallbackTokenSet / CallbackCIDRs come from the deployment
	// configuration (SOPS), not from this page.
	CallbackTokenSet bool              `json:"callback_token_set"`
	CallbackCIDRs    int               `json:"callback_cidrs"`
	CallbackURLs     *callbackURLsJSON `json:"callback_urls"`
	Version          int64             `json:"version"`
	UpdatedBy        *string           `json:"updated_by"`
	UpdatedByName    *string           `json:"updated_by_name"`
	UpdatedAt        *time.Time        `json:"updated_at"`
	Registration     *registrationJSON `json:"registration"`
	LastTest         *settingsTestJSON `json:"last_test"`
	Ready            bool              `json:"ready"`
	Problem          *string           `json:"problem"`
	Environments     []string          `json:"environments"`
	HTTPSOnly        bool              `json:"https_only"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func toCredential(c mpesasettings.Credential) credentialJSON {
	return credentialJSON{Set: c.Set, Last4: strPtr(c.Last4)}
}

// staffName resolves "admin:<uuid>" to the staff member's name (nil when it
// is another kind of actor or unknown).
func (h *handlers) staffName(ctx context.Context, actor string) *string {
	id, ok := strings.CutPrefix(actor, "admin:")
	if !ok || h.d.Admin == nil {
		return nil
	}
	uid, err := uuid.Parse(id)
	if err != nil {
		return nil
	}
	u, err := h.d.Admin.GetUser(ctx, uid)
	if err != nil {
		return nil
	}
	return &u.Name
}

func (h *handlers) toMpesaSettings(ctx context.Context, s mpesasettings.Status) mpesaSettingsJSON {
	out := mpesaSettingsJSON{
		Source: string(s.Source), Environment: strPtr(s.Environment), Shortcode: strPtr(s.Paybill),
		STKShortcode: strPtr(s.STKShortCode), CallbackBaseURL: strPtr(s.CallbackBaseURL),
		Credentials: map[string]credentialJSON{
			"consumer_key": toCredential(s.ConsumerKey), "consumer_secret": toCredential(s.ConsumerSecret), "passkey": toCredential(s.Passkey),
		},
		CallbackTokenSet: s.CallbackTokenSet, CallbackCIDRs: s.CallbackCIDRs, Version: s.Version,
		UpdatedBy: strPtr(s.UpdatedBy), UpdatedAt: s.UpdatedAt, Ready: s.Ready, Problem: strPtr(s.Problem),
		Environments: s.Environments, HTTPSOnly: s.HTTPSOnly,
	}
	if out.Environments == nil {
		out.Environments = []string{}
	}
	if s.CallbackURLs.Confirmation != "" {
		out.CallbackURLs = &callbackURLsJSON{STK: s.CallbackURLs.STK, Confirmation: s.CallbackURLs.Confirmation, Validation: s.CallbackURLs.Validation}
	}
	if s.UpdatedBy != "" {
		out.UpdatedByName = h.staffName(ctx, s.UpdatedBy)
	}
	if r := s.Registration; r != nil {
		out.Registration = &registrationJSON{RegisteredAt: r.At, RegisteredBy: r.By, RegisteredByName: h.staffName(ctx, r.By),
			ConfirmationURL: r.ConfirmationURL, Shortcode: r.ShortCode, Current: r.Current}
	}
	if t := s.LastTest; t != nil {
		out.LastTest = &settingsTestJSON{OK: t.OK, TestedAt: t.At}
	}
	return out
}

func settingsUnavailable(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "settings_unavailable", "Settings are not available in this deployment."))
}

func (h *handlers) failSettings(w http.ResponseWriter, r *http.Request, err error) {
	var ve *mpesasettings.ValidationError
	var de *mpesasettings.DarajaError
	switch {
	case errors.As(err, &ve):
		validationFailed(w, r, ve.Field, ve.Message)
	case errors.Is(err, mpesasettings.ErrConfirm):
		validationFailed(w, r, "confirm", `These are production settings: type "production" to confirm.`)
	case errors.Is(err, mpesasettings.ErrNotConfigured):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "mpesa_not_configured", "Save the paybill settings first."))
	case errors.Is(err, mpesasettings.ErrNoCallbackToken):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusConflict, "callback_token_missing",
			"TSP_MPESA_CALLBACK_TOKEN is not set in the deployment configuration. Ask the operator to add it (docs/runbooks/settings.md)."))
	case errors.As(err, &de):
		httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusBadGateway, "mpesa_error", "Safaricom did not accept the request: "+de.Message))
	default:
		h.fail(w, r, err)
	}
}

func (h *handlers) getMpesaSettings(w http.ResponseWriter, r *http.Request) {
	if h.d.MpesaSettings == nil {
		settingsUnavailable(w, r)
		return
	}
	s, err := h.d.MpesaSettings.Status(r.Context())
	if err != nil {
		h.failSettings(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.toMpesaSettings(r.Context(), s))
}

type putMpesaSettingsBody struct {
	Environment     string  `json:"environment"`
	Shortcode       string  `json:"shortcode"`
	STKShortcode    *string `json:"stk_shortcode"`
	CallbackBaseURL string  `json:"callback_base_url"`
	// Write-only; empty or absent keeps the stored value.
	ConsumerKey    *string `json:"consumer_key"`
	ConsumerSecret *string `json:"consumer_secret"`
	Passkey        *string `json:"passkey"`
	Confirm        *string `json:"confirm"`
}

func secretOf(p *string) config.Secret {
	if p == nil {
		return config.Secret{}
	}
	return config.NewSecret(*p)
}

func (h *handlers) putMpesaSettings(w http.ResponseWriter, r *http.Request) {
	if h.d.MpesaSettings == nil {
		settingsUnavailable(w, r)
		return
	}
	var b putMpesaSettingsBody
	if !h.decode(w, r, &b) {
		return
	}
	p := mpesasettings.SaveParams{
		Environment: b.Environment, Paybill: b.Shortcode, CallbackBaseURL: b.CallbackBaseURL,
		ConsumerKey: secretOf(b.ConsumerKey), ConsumerSecret: secretOf(b.ConsumerSecret), Passkey: secretOf(b.Passkey),
	}
	if b.STKShortcode != nil {
		p.STKShortCode = *b.STKShortcode
	}
	if b.Confirm != nil {
		p.Confirm = strings.TrimSpace(*b.Confirm)
	}
	s, err := h.d.MpesaSettings.Save(r.Context(), principalFrom(r.Context()).Actor(), p)
	if err != nil {
		h.failSettings(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.toMpesaSettings(r.Context(), s))
}

func (h *handlers) testMpesaSettings(w http.ResponseWriter, r *http.Request) {
	if h.d.MpesaSettings == nil {
		settingsUnavailable(w, r)
		return
	}
	res, err := h.d.MpesaSettings.Test(r.Context(), principalFrom(r.Context()).Actor())
	if err != nil {
		h.failSettings(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, settingsTestJSON{OK: res.OK, TestedAt: res.At, Message: strPtr(res.Message)})
}

type registerURLsBody struct {
	Confirm *string `json:"confirm"`
}

type registerURLsJSON struct {
	RegisteredAt    time.Time `json:"registered_at"`
	Shortcode       string    `json:"shortcode"`
	ConfirmationURL string    `json:"confirmation_url"`
	ValidationURL   string    `json:"validation_url"`
	ResponseType    string    `json:"response_type"`
}

func (h *handlers) registerMpesaURLs(w http.ResponseWriter, r *http.Request) {
	if h.d.MpesaSettings == nil {
		settingsUnavailable(w, r)
		return
	}
	var b registerURLsBody
	if r.ContentLength != 0 && !h.decode(w, r, &b) {
		return
	}
	confirm := ""
	if b.Confirm != nil {
		confirm = strings.TrimSpace(*b.Confirm)
	}
	res, err := h.d.MpesaSettings.RegisterURLs(r.Context(), principalFrom(r.Context()).Actor(), confirm)
	if err != nil {
		h.failSettings(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, registerURLsJSON{RegisteredAt: res.At, Shortcode: res.ShortCode,
		ConfirmationURL: res.ConfirmationURL, ValidationURL: res.ValidationURL, ResponseType: res.ResponseType})
}
