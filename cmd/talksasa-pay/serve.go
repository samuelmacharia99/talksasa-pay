package main

import (
	"context"
	"io"
)

// runServe runs the API and the worker in one process, for hosts that run a
// single process per service (e.g. a Procfile "web" line). Without the
// worker, M-Pesa callbacks, status checks, webhooks and charge expiry would
// never be processed, so both always run together here. When either stops
// (signal or error), the other is stopped too and the first error returned.
func runServe(ctx context.Context, logw io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- runAPI(ctx, logw) }()
	go func() { errs <- runWorker(ctx, logw) }()
	first := <-errs
	cancel()
	second := <-errs
	if first != nil {
		return first
	}
	return second
}
