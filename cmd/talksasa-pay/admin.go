package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/talksasa/talksasa-pay/internal/app"
	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/config"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const adminUsage = `usage: talksasa-pay admin <command> [flags]

Every command that changes data needs --actor "<your name>"; it is written to
the audit log.

  app create   --name NAME --prefix PREFIX [--webhook-url URL] --actor NAME
  app list
  app set-webhook --app ID|PREFIX --url URL --actor NAME
  app rotate-webhook-secret --app ID|PREFIX --actor NAME
  key issue        --app ID|PREFIX --label LABEL [--expires YYYY-MM-DD] --actor NAME
  key list         --app ID|PREFIX
  key revoke       --app ID|PREFIX --key-id KEYID --actor NAME
  user invite      --email EMAIL --name NAME --role viewer|finance|approver|admin --actor NAME
  user list
  user reset       --email EMAIL --actor NAME   (lost 2FA / re-onboard: new setup link)
  user disable     --email EMAIL --actor NAME   (offboarding)
  audit verify     check the audit log hash chain
  mpesa register-urls --actor NAME   register the paybill's C2B callback URLs with Safaricom
  mpesa status     M-Pesa configuration (no secrets) and STK/callback counts to watch
  seed demo        --actor NAME   development only: demo apps (and, later, charges and payments)
`

func runAdmin(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return usageError{adminUsage}
	}
	cmd := args[0]
	if len(args) >= 2 && !strings.HasPrefix(args[1], "-") {
		cmd += " " + args[1]
		args = args[2:]
	} else {
		args = args[1:]
	}

	fs := flag.NewFlagSet("admin "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	name := fs.String("name", "", "app name")
	prefix := fs.String("prefix", "", "account prefix (2-6 uppercase letters)")
	webhookURL := fs.String("webhook-url", "", "app webhook URL")
	urlFlag := fs.String("url", "", "URL")
	appRef := fs.String("app", "", "app id or account prefix")
	label := fs.String("label", "", "API key label")
	expires := fs.String("expires", "", "API key expiry date (YYYY-MM-DD, UTC)")
	keyID := fs.String("key-id", "", "API key id")
	actorName := fs.String("actor", "", "your name, recorded in the audit log")
	email := fs.String("email", "", "staff email")
	role := fs.String("role", "", "staff role: viewer, finance, approver or admin")
	if err := fs.Parse(args); err != nil {
		return usageError{adminUsage}
	}
	actor := audit.CLI(strings.TrimSpace(*actorName))
	needActor := func() error {
		if err := actor.Validate(); err != nil {
			return usageError{"--actor is required for commands that change data"}
		}
		return nil
	}

	a, err := newApp(ctx, stderr, "admin")
	if err != nil {
		return err
	}
	defer a.close()

	switch cmd {
	case "app create":
		if err := needActor(); err != nil {
			return err
		}
		c, err := a.apps.Create(ctx, actor, appCreateParams(*name, *prefix, *webhookURL))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "App created\n  id:             %s\n  name:           %s\n  account prefix: %s\n  webhook url:    %s\n\n",
			c.App.ID, c.App.Name, c.App.AccountPrefix, orNone(c.App.WebhookURL))
		printSecretOnce(stdout, "Webhook signing secret", c.WebhookSecret)
		return nil

	case "app list":
		ps, err := a.apps.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPREFIX\tNAME\tSTATUS\tWEBHOOK")
		for _, p := range ps {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.AccountPrefix, p.Name, p.Status, orNone(p.WebhookURL))
		}
		return tw.Flush()

	case "app set-webhook":
		if err := needActor(); err != nil {
			return err
		}
		p, err := a.apps.Resolve(ctx, *appRef)
		if err != nil {
			return err
		}
		p, err = a.apps.SetWebhookURL(ctx, actor, p.ID, *urlFlag)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Webhook URL for %s set to %s\n", p.AccountPrefix, orNone(p.WebhookURL))
		return nil

	case "app rotate-webhook-secret":
		if err := needActor(); err != nil {
			return err
		}
		p, err := a.apps.Resolve(ctx, *appRef)
		if err != nil {
			return err
		}
		secret, err := a.apps.RotateWebhookSecret(ctx, actor, p.ID)
		if err != nil {
			return err
		}
		printSecretOnce(stdout, "New webhook signing secret for "+p.AccountPrefix, secret)
		return nil

	case "key issue":
		if err := needActor(); err != nil {
			return err
		}
		p, err := a.apps.Resolve(ctx, *appRef)
		if err != nil {
			return err
		}
		var exp *time.Time
		if *expires != "" {
			t, err := time.Parse("2006-01-02", *expires)
			if err != nil {
				return usageError{"--expires must be YYYY-MM-DD"}
			}
			exp = &t
		}
		k, err := a.apps.IssueAPIKey(ctx, actor, p.ID, *label, exp)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "API key issued for %s\n  key id: %s\n  label:  %s\n\n", p.AccountPrefix, k.Key.KeyID, k.Key.Label)
		printSecretOnce(stdout, "API key", k.Plaintext)
		return nil

	case "key list":
		p, err := a.apps.Resolve(ctx, *appRef)
		if err != nil {
			return err
		}
		keys, err := a.apps.ListAPIKeys(ctx, p.ID)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "KEY ID\tLABEL\tCREATED\tLAST USED\tEXPIRES\tREVOKED")
		for _, k := range keys {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", k.KeyID, k.Label, k.CreatedAt.UTC().Format(time.RFC3339),
				fmtTime(k.LastUsedAt), fmtTime(k.ExpiresAt), fmtTime(k.RevokedAt))
		}
		return tw.Flush()

	case "key revoke":
		if err := needActor(); err != nil {
			return err
		}
		p, err := a.apps.Resolve(ctx, *appRef)
		if err != nil {
			return err
		}
		if err := a.apps.RevokeAPIKey(ctx, actor, p.ID, *keyID); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "API key %s revoked\n", *keyID)
		return nil

	case "user invite":
		if err := needActor(); err != nil {
			return err
		}
		inv, err := a.admin.Invite(ctx, actor, *email, *name, store.AdminRole(*role))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Staff account created for %s (%s, role %s)\n\n", inv.User.Name, inv.User.Email, inv.User.Role)
		printSetupLink(stdout, inv.URL, inv.ExpiresAt)
		return nil

	case "user list":
		users, err := a.admin.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "EMAIL\tNAME\tROLE\tSTATUS\tLAST SIGN-IN")
		for _, u := range users {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", u.Email, u.Name, u.Role, u.Status, fmtTime(u.LastLoginAt))
		}
		return tw.Flush()

	case "user reset", "user disable":
		if err := needActor(); err != nil {
			return err
		}
		u, err := a.admin.GetUserByEmail(ctx, *email)
		if err != nil {
			return err
		}
		if cmd == "user disable" {
			if err := a.admin.Disable(ctx, actor, nil, u.ID); err != nil {
				return err
			}
			fmt.Fprintf(stdout, "%s disabled and signed out everywhere\n", u.Email)
			return nil
		}
		inv, err := a.admin.Reset(ctx, actor, nil, u.ID)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "%s reset: password, 2FA and recovery codes cleared, all sessions signed out\n\n", u.Email)
		printSetupLink(stdout, inv.URL, inv.ExpiresAt)
		return nil

	case "seed demo":
		if err := needActor(); err != nil {
			return err
		}
		if a.cfg.Env != config.EnvDevelopment {
			return errors.New("seed demo only runs with TSP_ENV=development")
		}
		return seedDemo(ctx, a, actor, stdout)

	case "audit verify":
		bad, err := store.New(a.pool).VerifyAuditChain(ctx)
		if err != nil {
			return err
		}
		if bad != 0 {
			return fmt.Errorf("AUDIT LOG TAMPERING DETECTED: chain breaks at row %d", bad)
		}
		fmt.Fprintln(stdout, "Audit log chain verified: OK")
		return nil

	case "mpesa register-urls":
		if err := needActor(); err != nil {
			return err
		}
		if a.mpesa == nil {
			return errors.New("M-Pesa is off (TSP_MPESA_ENV is not set)")
		}
		return registerC2BURLs(ctx, a.mpesa, a.pool, actor, a.cfg.Public.Paybill, a.cfg.Mpesa.CallbackToken.Reveal(), stdout)

	case "mpesa status":
		return printMpesaStatus(ctx, a.cfg, store.New(a.pool), stdout)

	}
	return usageError{adminUsage}
}

func printSetupLink(w io.Writer, url string, expires time.Time) {
	fmt.Fprintf(w, "One-time setup link (shown ONCE; send it to the person over a private channel).\nIt expires %s:\n\n    %s\n\n",
		expires.UTC().Format(time.RFC1123), url)
}

func printSecretOnce(w io.Writer, what, secret string) {
	fmt.Fprintf(w, "%s (shown ONCE; store it in the app's secret manager now):\n\n    %s\n\n", what, secret)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func fmtTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

func runKeygen(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	ver := fs.Int("version", 1, "key version number")
	if err := fs.Parse(args); err != nil || *ver < 1 {
		return usageError{"usage: talksasa-pay keygen --version N"}
	}
	line, err := crypto.GenerateKeyLine(*ver)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, line)
	return nil
}

func runHealthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	u := fs.String("url", "http://127.0.0.1:8088/healthz", "health URL")
	if err := fs.Parse(args); err != nil {
		return usageError{"usage: talksasa-pay healthcheck --url URL"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, *u, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health endpoint returned %d", resp.StatusCode)
	}
	return nil
}

func appCreateParams(name, prefix, webhookURL string) app.CreateParams {
	return app.CreateParams{Name: name, AccountPrefix: prefix, WebhookURL: webhookURL}
}
