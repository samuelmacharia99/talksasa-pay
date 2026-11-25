#!/bin/sh
# Procfile web process: build the service, then replace this shell with it so
# the platform's stop signal reaches it directly (clean shutdown of the API and
# worker). Migrations run separately in the Procfile release step.
set -eu
go build -buildvcs=false -trimpath -o /tmp/tsp ./cmd/talksasa-pay
exec /tmp/tsp serve
