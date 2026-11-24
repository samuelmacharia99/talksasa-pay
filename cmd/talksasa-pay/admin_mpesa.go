package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// c2bRegistrar is the part of the Daraja client the register-urls command
// uses (*mpesa.HTTPClient implements it).
type c2bRegistrar interface {
	RegisterC2BURLs(ctx context.Context, req mpesa.RegisterURLsRequest) error
	CallbackURLs() (stk, c2bConfirmation, c2bValidation string)
}

// registerURLsTimeout bounds the whole register-urls command's Daraja call.
const registerURLsTimeout = 60 * time.Second

// redactedToken replaces the callback token wherever it appears in a URL.
const redactedToken = "[TOKEN]"

// redactToken removes the secret callback token from s (raw and
// path-escaped forms). The callback URLs carry the token in their path, so
// they are never printed or audited as they are.
func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	s = strings.ReplaceAll(s, token, redactedToken)
	if esc := url.PathEscape(token); esc != token {
		s = strings.ReplaceAll(s, esc, redactedToken)
	}
	return s
}

// registerC2BURLs registers the paybill's C2B confirmation (and validation)
// URLs with Safaricom and audits it. Done once per paybill (again only if
// the callback base URL or token changes).
func registerC2BURLs(ctx context.Context, c c2bRegistrar, pool db.Beginner, actor audit.Actor, paybill, token string, w io.Writer) error {
	if paybill == "" {
		return errors.New("TSP_MPESA_PAYBILL is not set: there is no paybill to register URLs for")
	}
	_, confirmation, validation := c.CallbackURLs()
	req := mpesa.RegisterURLsRequest{
		ShortCode: paybill, ConfirmationURL: confirmation, ValidationURL: validation,
		// If Safaricom cannot reach the validation URL, complete the
		// payment: we never reject money.
		ResponseType: "Completed",
	}
	cctx, cancel := context.WithTimeout(ctx, registerURLsTimeout)
	defer cancel()
	if err := c.RegisterC2BURLs(cctx, req); err != nil {
		// Daraja may echo the URL (with the token) back in its message.
		return fmt.Errorf("safaricom did not register the URLs: %s", redactToken(err.Error(), token))
	}
	conf, val := redactToken(confirmation, token), redactToken(validation, token)
	// Safaricom has accepted: the record must not be lost to a cancelled
	// command context.
	actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer acancel()
	if err := db.WithTx(actx, pool, actor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		return audit.Record(ctx, q, audit.Entry{
			Actor: actor, Action: "mpesa.paybill_urls_registered", EntityType: "mpesa_shortcode", EntityID: paybill,
			Details: map[string]any{"shortcode": paybill, "confirmation_url": conf, "validation_url": val, "response_type": req.ResponseType},
		})
	}); err != nil {
		return fmt.Errorf("URLs were registered with Safaricom but the audit record failed: %w", err)
	}
	fmt.Fprintf(w, "C2B URLs registered for paybill %s\n  confirmation: %s\n  validation:   %s\n  response type: %s\n(%s is the secret callback token.)\n",
		paybill, conf, val, req.ResponseType, redactedToken)
	return nil
}

// printMpesaStatus prints the M-Pesa configuration (no secrets) and the
// counts staff should watch.
func printMpesaStatus(ctx context.Context, cfg *config.Config, q *store.Queries, w io.Writer) error {
	m := cfg.Mpesa
	if !m.Enabled() {
		fmt.Fprintln(w, "M-Pesa:          off (TSP_MPESA_ENV not set)")
	} else {
		fmt.Fprintf(w, "M-Pesa:          %s\n", m.Env)
		fmt.Fprintf(w, "STK shortcode:   %s\n", orNone(m.ShortCode))
		fmt.Fprintf(w, "C2B paybill:     %s\n", orNone(cfg.Public.Paybill))
		fmt.Fprintf(w, "Callback base:   %s\n", orNone(redactToken(m.CallbackBaseURL, m.CallbackToken.Reveal())))
		if len(m.CallbackCIDRs) == 0 {
			fmt.Fprintln(w, "Callback IPs:    any (no TSP_MPESA_CALLBACK_CIDRS)")
		} else {
			fmt.Fprintf(w, "Callback IPs:    %d allowed network(s)\n", len(m.CallbackCIDRs))
		}
	}
	c, err := q.MpesaStatusCounts(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "\nLive STK pushes (initiating/sent):     %d\n", c.LivePushes)
	fmt.Fprintf(w, "Unknown STK pushes (staff review):     %d\n", c.UnknownPushes)
	fmt.Fprintf(w, "Unprocessed callbacks:                 %d\n", c.UnprocessedCallbacks)
	fmt.Fprintf(w, "Callbacks with errors (last 7 days):   %d\n", c.FailedCallbacks7d)
	return nil
}
