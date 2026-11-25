// Command talksasa-pay is the single binary for the Talksasa Pay service.
//
//	talksasa-pay api                 serve the HTTP API
//	talksasa-pay worker              run background jobs (webhooks, expiry, retention)
//	talksasa-pay migrate up|down|version
//	talksasa-pay admin ...           operator commands (apps, API keys, audit)
//	talksasa-pay keygen --version N  print a new master keyring line
//	talksasa-pay healthcheck --url   container health probe
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/talksasa/talksasa-pay/internal/config"
	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: talksasa-pay <command> [args]

commands:
  api          serve the HTTP API
  worker       run background jobs (webhook delivery, request expiry, retention)
  migrate      up | down | version   (uses TSP_MIGRATE_DATABASE_URL[_FILE])
  admin        operator commands; run "talksasa-pay admin" for details
  keygen       print a new master keyring line: keygen --version N
  healthcheck  probe an HTTP health endpoint: healthcheck --url URL
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch args[0] {
	case "api":
		err = runAPI(ctx, stderr)
	case "worker":
		err = runWorker(ctx, stderr)
	case "migrate":
		err = runMigrate(args[1:], stderr)
	case "admin":
		err = runAdmin(ctx, args[1:], stdout, stderr)
	case "keygen":
		err = runKeygen(args[1:], stdout)
	case "healthcheck":
		err = runHealthcheck(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, ue.Error())
			return 2
		}
		// Errors from config/services are written for operators and never
		// include secret values.
		fmt.Fprintf(stderr, "error: %v\n", err)
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (u usageError) Error() string { return u.msg }

func newLogger(w io.Writer, env config.Environment, level slog.Level) *slog.Logger {
	return tlog.New(w, tlog.Options{Level: level, Service: "talksasa-pay", Version: version, Env: string(env)})
}
