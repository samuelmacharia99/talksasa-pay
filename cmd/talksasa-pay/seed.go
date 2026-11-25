package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
)

// seedDemo fills a DEVELOPMENT database with demo data so the admin panel
// and pay page have something to show. It goes through the normal services
// (so every rule, trigger and audit entry applies) and is safe to re-run.
//
// TODO(gateway-core): also create demo charges (charge.Service.Create with
// seed-* idempotency keys), a few paid ones, a paybill payment forwarded as
// payment.received and one unmatched payment (unknown prefix).
func seedDemo(ctx context.Context, a *services, actor audit.Actor, w io.Writer) error {
	catalogue := []struct{ name, prefix string }{
		{"ISP Billing", "BILL"},
		{"Cloud Hosting", "HOST"},
	}
	for _, pc := range catalogue {
		p, err := a.apps.Resolve(ctx, pc.prefix)
		if errors.Is(err, app.ErrNotFound) {
			c, err := a.apps.Create(ctx, actor, app.CreateParams{Name: pc.name, AccountPrefix: pc.prefix})
			if err != nil {
				return err
			}
			p = c.App
			fmt.Fprintf(w, "created app %s (%s); its webhook secret was discarded, rotate it to get one\n", p.Name, p.AccountPrefix)
		} else if err != nil {
			return err
		}
	}
	fmt.Fprintln(w, "\nDemo apps ready (re-running is safe).")
	return nil
}
