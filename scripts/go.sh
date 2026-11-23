#!/usr/bin/env bash
# Runs the Go toolchain inside a pinned container so contributors without a
# local Go install get identical builds. If Go 1.26+ is installed locally,
# run `make GO=go ...` instead.
set -euo pipefail

GO_IMAGE="${GO_IMAGE:-golang:1.26-bookworm}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

mkdir -p "${HOME}/go/pkg/mod" "${HOME}/go/pkg/sumdb" "${HOME}/.cache/go-build"

tty_flag=()
if [ -t 0 ] && [ -t 1 ]; then tty_flag=(-it); fi

# Forward only Go build settings and the service's own TSP_* settings (file
# paths and non-secret config). Never forward the whole host environment.
env_flags=(-e HOME=/tmp -e GOFLAGS=-modcacherw -e GOPATH=/gopath -e GOMODCACHE=/gopath/pkg/mod -e GOCACHE=/go-build-cache -e CGO_ENABLED="${CGO_ENABLED:-1}")
for v in GOOS GOARCH GOTOOLCHAIN $(compgen -e | grep -E '^TSP_' || true); do
  if [ -n "${!v:-}" ]; then env_flags+=(-e "$v"); fi
done

exec docker run --rm "${tty_flag[@]}" \
  --network host \
  -u "$(id -u):$(id -g)" \
  -v "${ROOT}:/src" -w /src \
  -v "${HOME}/go/pkg:/gopath/pkg" \
  -v "${HOME}/.cache/go-build:/go-build-cache" \
  "${env_flags[@]}" \
  "${GO_IMAGE}" go "$@"
