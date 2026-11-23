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

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/platform/crypto"
	"github.com/talksasa/talksasa-pay/internal/product"
	"github.com/talksasa/talksasa-pay/internal/store"
)

const adminUsage = `usage: talksasa-pay admin <command> [flags]

Every command that changes data needs --actor "<your name>"; it is written to
the audit log.

  product create   --name NAME --prefix PREFIX [--webhook-url URL] --actor NAME
  product list
  product set-webhook --product ID|PREFIX --url URL --actor NAME
  product rotate-webhook-secret --product ID|PREFIX --actor NAME
  key issue        --product ID|PREFIX --label LABEL [--expires YYYY-MM-DD] --actor NAME
  key list         --product ID|PREFIX
  key revoke       --product ID|PREFIX --key-id KEYID --actor NAME
  audit verify     check the audit log hash chain
  ledger check     check cached balances against allocation lines
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
	name := fs.String("name", "", "product name")
	prefix := fs.String("prefix", "", "account prefix (2-6 uppercase letters)")
	webhookURL := fs.String("webhook-url", "", "product webhook URL")
	urlFlag := fs.String("url", "", "URL")
	productRef := fs.String("product", "", "product id or account prefix")
	label := fs.String("label", "", "API key label")
	expires := fs.String("expires", "", "API key expiry date (YYYY-MM-DD, UTC)")
	keyID := fs.String("key-id", "", "API key id")
	actorName := fs.String("actor", "", "your name, recorded in the audit log")
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
	case "product create":
		if err := needActor(); err != nil {
			return err
		}
		c, err := a.products.Create(ctx, actor, productCreateParams(*name, *prefix, *webhookURL))
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Product created\n  id:             %s\n  name:           %s\n  account prefix: %s\n  webhook url:    %s\n\n",
			c.Product.ID, c.Product.Name, c.Product.AccountPrefix, orNone(c.Product.WebhookURL))
		printSecretOnce(stdout, "Webhook signing secret", c.WebhookSecret)
		return nil

	case "product list":
		ps, err := a.products.List(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPREFIX\tNAME\tSTATUS\tWEBHOOK")
		for _, p := range ps {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.AccountPrefix, p.Name, p.Status, orNone(p.WebhookURL))
		}
		return tw.Flush()

	case "product set-webhook":
		if err := needActor(); err != nil {
			return err
		}
		p, err := a.products.Resolve(ctx, *productRef)
		if err != nil {
			return err
		}
		p, err = a.products.SetWebhookURL(ctx, actor, p.ID, *urlFlag)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "Webhook URL for %s set to %s\n", p.AccountPrefix, orNone(p.WebhookURL))
		return nil

	case "product rotate-webhook-secret":
		if err := needActor(); err != nil {
			return err
		}
		p, err := a.products.Resolve(ctx, *productRef)
		if err != nil {
			return err
		}
		secret, err := a.products.RotateWebhookSecret(ctx, actor, p.ID)
		if err != nil {
			return err
		}
		printSecretOnce(stdout, "New webhook signing secret for "+p.AccountPrefix, secret)
		return nil

	case "key issue":
		if err := needActor(); err != nil {
			return err
		}
		p, err := a.products.Resolve(ctx, *productRef)
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
		k, err := a.products.IssueAPIKey(ctx, actor, p.ID, *label, exp)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "API key issued for %s\n  key id: %s\n  label:  %s\n\n", p.AccountPrefix, k.Key.KeyID, k.Key.Label)
		printSecretOnce(stdout, "API key", k.Plaintext)
		return nil

	case "key list":
		p, err := a.products.Resolve(ctx, *productRef)
		if err != nil {
			return err
		}
		keys, err := a.products.ListAPIKeys(ctx, p.ID)
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
		p, err := a.products.Resolve(ctx, *productRef)
		if err != nil {
			return err
		}
		if err := a.products.RevokeAPIKey(ctx, actor, p.ID, *keyID); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "API key %s revoked\n", *keyID)
		return nil

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

	case "ledger check":
		drift, err := store.New(a.pool).FindBalanceDrift(ctx)
		if err != nil {
			return err
		}
		if len(drift) > 0 {
			for _, d := range drift {
				fmt.Fprintf(stdout, "DRIFT %s %s cached=%d actual=%d\n", d.Kind, d.ID, d.Cached, d.Actual)
			}
			return errors.New("ledger cached balances disagree with allocations")
		}
		fmt.Fprintln(stdout, "Ledger balances verified: OK")
		return nil
	}
	return usageError{adminUsage}
}

func printSecretOnce(w io.Writer, what, secret string) {
	fmt.Fprintf(w, "%s (shown ONCE; store it in the product's secret manager now):\n\n    %s\n\n", what, secret)
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

func productCreateParams(name, prefix, webhookURL string) product.CreateParams {
	return product.CreateParams{Name: name, AccountPrefix: prefix, WebhookURL: webhookURL}
}
