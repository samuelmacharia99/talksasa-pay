#!/bin/sh
# Procfile web process: build the service, apply migrations, then replace
# this shell with the service so the platform's stop signal reaches it
# directly (clean shutdown of the API and worker).
#
# Migrations run here as well as in the Procfile release step because some
# host modes (e.g. a split web + API stack) skip release. With nothing to
# apply, `migrate up` logs "schema already up to date" and exits 0.
set -eu
go build -buildvcs=false -trimpath -o /tmp/tsp ./cmd/talksasa-pay
/tmp/tsp migrate up
exec /tmp/tsp serve
