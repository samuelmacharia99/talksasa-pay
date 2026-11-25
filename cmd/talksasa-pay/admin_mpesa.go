package main

import (
	"context"
	"fmt"
	"io"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/mpesasettings"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// urlRegistrar registers the paybill's C2B URLs (*mpesasettings.Service;
// the admin panel's "Register URLs with Safaricom" button calls the same).
type urlRegistrar interface {
	RegisterURLs(ctx context.Context, actor audit.Actor, confirm string) (mpesasettings.RegisterResult, error)
}

// registerC2BURLs registers the paybill's C2B confirmation (and validation)
// URLs with Safaricom; the service records and audits it. Done once per
// paybill (again if the paybill, callback base URL or token changes).
func registerC2BURLs(ctx context.Context, s urlRegistrar, actor audit.Actor, confirm string, w io.Writer) error {
	res, err := s.RegisterURLs(ctx, actor, confirm)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "C2B URLs registered for paybill %s\n  confirmation: %s\n  validation:   %s\n  response type: %s\n(%s is the secret callback token.)\n",
		res.ShortCode, res.ConfirmationURL, res.ValidationURL, res.ResponseType, mpesasettings.RedactedToken)
	return nil
}

// printMpesaStatus prints the M-Pesa settings in effect (no secrets) and the
// counts staff should watch.
func printMpesaStatus(ctx context.Context, st mpesasettings.Status, q *store.Queries, w io.Writer) error {
	switch st.Source {
	case mpesasettings.SourceNone:
		fmt.Fprintln(w, "M-Pesa:          off (nothing saved in Settings, TSP_MPESA_ENV not set)")
	default:
		src := "saved in the admin panel (Settings)"
		if st.Source == mpesasettings.SourceEnvironment {
			src = "TSP_MPESA_* configuration (nothing saved in Settings)"
		}
		fmt.Fprintf(w, "M-Pesa:          %s, from %s\n", st.Environment, src)
		fmt.Fprintf(w, "C2B paybill:     %s\n", orNone(st.Paybill))
		stk := st.STKShortCode
		if stk == "" {
			stk = st.Paybill
		}
		fmt.Fprintf(w, "STK shortcode:   %s\n", orNone(stk))
		fmt.Fprintf(w, "Callback base:   %s\n", orNone(st.CallbackBaseURL))
		if st.CallbackCIDRs == 0 {
			fmt.Fprintln(w, "Callback IPs:    any (no TSP_MPESA_CALLBACK_CIDRS)")
		} else {
			fmt.Fprintf(w, "Callback IPs:    %d allowed network(s)\n", st.CallbackCIDRs)
		}
		if r := st.Registration; r != nil {
			fmt.Fprintf(w, "URLs registered: %s by %s\n", r.At.UTC().Format("2006-01-02 15:04 MST"), r.By)
		}
	}
	if st.Problem != "" && st.Source != mpesasettings.SourceNone {
		fmt.Fprintf(w, "PROBLEM:         %s\n", st.Problem)
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
