#!/usr/bin/env bash
# Talksasa Pay deploy tool. Run from your workstation.
#
#   scripts/deploy.sh local                         rebuild + restart the local Docker stack, then smoke test
#   scripts/deploy.sh staging    [options]          test, build, ship and roll out to the staging host
#   scripts/deploy.sh production [options]          same, with production guards
#   scripts/deploy.sh rollback <env> [--host H]     put the previous release back (migrations stay)
#   scripts/deploy.sh status   <env> [--host H]     show what is running
#   scripts/deploy.sh init-secrets <env> --age <age1...>
#                                                   create secrets/<env>.enc.yaml with fresh random values
#
# Options:
#   --host user@host   SSH target (or TSP_DEPLOY_HOST). The user needs passwordless sudo.
#   --skip-verify      skip `make verify` (staging only)
#   --allow-dirty      deploy uncommitted changes (staging only)
#   --yes              do not ask for confirmation (staging only)
#
# Nothing here prints secret values. Secrets travel to the host only inside the
# SOPS-encrypted bundle and are decrypted there with the host's age key.
set -Eeuo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
REPO="$PWD"
SOPS_IMAGE="ghcr.io/getsops/sops:v3.13.3-alpine"
PLATFORM="${TSP_DEPLOY_PLATFORM:-linux/amd64}"
REMOTE_ROOT="/opt/talksasa-pay"

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33mWARN: %s\033[0m\n' "$*" >&2; }
die() {
  printf '\033[31mERROR: %s\033[0m\n' "$*" >&2
  exit 1
}
usage() { sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; }

# ------------------------------------------------------------------ args
CMD="${1:-}"
[[ -n $CMD ]] || {
  usage
  exit 2
}
shift
ENV_NAME="" HOST="${TSP_DEPLOY_HOST:-}" SKIP_VERIFY=0 ALLOW_DIRTY=0 YES=0 AGE_RECIPIENT=""
case $CMD in
staging | production) ENV_NAME="$CMD" CMD=deploy ;;
rollback | status | init-secrets)
  ENV_NAME="${1:-}"
  shift || true
  ;;
local) ;;
-h | --help | help)
  usage
  exit 0
  ;;
*) die "unknown command '$CMD' (try --help)" ;;
esac
while (($#)); do
  case $1 in
  --host)
    HOST="${2:?--host needs a value}"
    shift
    ;;
  --age)
    AGE_RECIPIENT="${2:?--age needs a value}"
    shift
    ;;
  --skip-verify) SKIP_VERIFY=1 ;;
  --allow-dirty) ALLOW_DIRTY=1 ;;
  --yes) YES=1 ;;
  *) die "unknown option $1" ;;
  esac
  shift
done
if [[ $CMD != local ]]; then
  [[ $ENV_NAME == staging || $ENV_NAME == production ]] || die "environment must be staging or production"
fi

# ------------------------------------------------------------------ helpers
SSH_OPTS=(-o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=15 -o ServerAliveInterval=15)
need_host() {
  [[ -n $HOST ]] || die "no deploy host: pass --host user@host or set TSP_DEPLOY_HOST"
  [[ $HOST =~ ^[A-Za-z0-9._-]+@[A-Za-z0-9.:_-]+$ || $HOST =~ ^[A-Za-z0-9._-]+$ ]] || die "bad --host value"
}
# shellcheck disable=SC2029 # arguments are meant to expand locally
remote() { ssh "${SSH_OPTS[@]}" "$HOST" "$@"; }
remote_apply() { remote sudo -n "$REMOTE_ROOT/$ENV_NAME/current/host/apply.sh" "$@"; }

sops_local() {
  if command -v sops >/dev/null; then
    sops "$@"
  else
    docker run --rm -i -u "$(id -u):$(id -g)" -v "$REPO:/w" -w /w "$SOPS_IMAGE" "$@"
  fi
}

is_encrypted_bundle() {
  # A SOPS file carries a top-level `sops:` block and ENC[...] values only.
  grep -q '^sops:' "$1" && ! grep -Eq '^[a-z_]+: [^E|]' "$1"
}

# ------------------------------------------------------------------ local
cmd_local() {
  say "rebuilding and restarting the local stack"
  make up
  ./scripts/smoke.sh
  say "local stack is up: admin http://127.0.0.1:3001  pay http://127.0.0.1:3000  api http://127.0.0.1:8088"
}

# ------------------------------------------------------------------ init-secrets
cmd_init_secrets() {
  local out="secrets/$ENV_NAME.enc.yaml"
  [[ ! -e $out ]] || die "$out already exists; edit it with: sops $out"
  [[ $AGE_RECIPIENT =~ ^age1[0-9a-z]{58}$ ]] || die "pass the host's age PUBLIC key: --age age1..."
  mkdir -p secrets
  if ! grep -qs "$ENV_NAME\\\\.enc" .sops.yaml; then
    [[ -f .sops.yaml ]] || printf 'creation_rules:\n' >.sops.yaml
    printf '  - path_regex: secrets/%s\\.enc\\.yaml$\n    age: %s\n' "$ENV_NAME" "$AGE_RECIPIENT" >>.sops.yaml
    say "added a $ENV_NAME rule to .sops.yaml (commit it)"
  fi
  rnd() { head -c 96 /dev/urandom | base64 | tr -d '\n=+/' | head -c "$1"; }
  local su owner app
  su="$(rnd 40)" owner="$(rnd 40)" app="$(rnd 40)"
  # Generated values go straight from this pipe into sops; plaintext is
  # never written to disk or shown.
  {
    printf 'pg_superuser_password: %s\n' "$su"
    printf 'pg_owner_password: %s\n' "$owner"
    printf 'pg_app_password: %s\n' "$app"
    printf 'database_url: postgres://talksasa_app:%s@postgres:5432/talksasa_pay?sslmode=require\n' "$app"
    printf 'migrate_database_url: postgres://talksasa_owner:%s@postgres:5432/talksasa_pay?sslmode=require\n' "$owner"
    printf 'master_keys: "1:%s"\n' "$(head -c 32 /dev/urandom | base64 | tr -d '\n')"
    printf 'api_key_pepper: %s\n' "$(rnd 64)"
    # Secret path segment of the Safaricom callback URLs. The Daraja
    # consumer key/secret and passkey are added by hand: sops <bundle>
    # (docs/runbooks/mpesa.md).
    printf 'mpesa_callback_token: %s\n' "$(rnd 48)"
  } | sops_local --encrypt --filename-override "$out" --input-type yaml --output-type yaml /dev/stdin >"$out.tmp" || {
    rm -f "$out.tmp"
    die "sops --encrypt failed; nothing written"
  }
  is_encrypted_bundle "$out.tmp" || {
    rm -f "$out.tmp"
    die "sops output does not look encrypted; nothing written"
  }
  mv "$out.tmp" "$out"
  say "wrote $out (encrypted; safe to commit). Database passwords in it are used when the $ENV_NAME database is first created."
}

# ------------------------------------------------------------------ deploy
preflight() {
  command -v git >/dev/null || die "git is required"
  local dirty branch
  dirty="$(git status --porcelain)"
  if [[ -n $dirty ]]; then
    [[ $ENV_NAME == staging && $ALLOW_DIRTY == 1 ]] || die "working tree has uncommitted changes (commit first${ENV_NAME:+; --allow-dirty is allowed for staging only})"
    warn "deploying uncommitted changes to staging"
  fi
  if [[ $ENV_NAME == production ]]; then
    ((SKIP_VERIFY == 0)) || die "--skip-verify is not allowed for production"
    branch="$(git rev-parse --abbrev-ref HEAD)"
    [[ $branch == main ]] || die "production deploys come from main (on $branch)"
    git fetch -q origin main || die "cannot reach origin to check main is pushed"
    [[ $(git rev-parse HEAD) == "$(git rev-parse origin/main)" ]] || die "HEAD is not origin/main: push (or pull) first, so production runs exactly what is in GitHub"
  fi
  [[ -f secrets/$ENV_NAME.enc.yaml ]] || die "secrets/$ENV_NAME.enc.yaml is missing (scripts/deploy.sh init-secrets $ENV_NAME --age <host public key>)"
  is_encrypted_bundle "secrets/$ENV_NAME.enc.yaml" || die "secrets/$ENV_NAME.enc.yaml is not SOPS-encrypted"

  VERSION="$(git rev-parse --short=12 HEAD)"
  if [[ -n $dirty ]]; then VERSION="$VERSION-dirty$(date -u +%m%d%H%M)"; fi
}

build_images() {
  say "building images $VERSION ($PLATFORM)"
  docker build --platform "$PLATFORM" -f deploy/Dockerfile --build-arg VERSION="$VERSION" -t "talksasa-pay:$VERSION" .
  docker build --platform "$PLATFORM" -f deploy/web.Dockerfile --build-arg APP=admin -t "talksasa-pay-admin:$VERSION" .
  docker build --platform "$PLATFORM" -f deploy/web.Dockerfile --build-arg APP=pay -t "talksasa-pay-page:$VERSION" .
}

ship() {
  local images=("talksasa-pay:$VERSION" "talksasa-pay-admin:$VERSION" "talksasa-pay-page:$VERSION") missing=() img
  for img in "${images[@]}"; do
    remote sudo -n docker image inspect "$img" >/dev/null 2>&1 || missing+=("$img")
  done
  if ((${#missing[@]})); then
    say "shipping ${#missing[@]} image(s) to $HOST"
    docker save "${missing[@]}" | gzip -1 | remote 'gunzip | sudo -n docker load -q'
  else
    say "images already on $HOST"
  fi

  say "uploading release bundle"
  STAGE="$(mktemp -d)"
  trap 'rm -rf "$STAGE"' EXIT
  local stage="$STAGE"
  mkdir -p "$stage/postgres" "$stage/host"
  cp deploy/compose.prod.yml deploy/Caddyfile "$stage/"
  cp deploy/postgres/init-roles.prod.sh "$stage/postgres/"
  cp deploy/host/apply.sh "deploy/host/talksasa-pay-secrets@.service" "$stage/host/"
  cp "secrets/$ENV_NAME.enc.yaml" "$stage/secrets.enc.yaml"
  printf '%s\n' "$VERSION" >"$stage/VERSION"
  local dest="$REMOTE_ROOT/$ENV_NAME/releases/$VERSION"
  tar -C "$stage" -czf - . | remote "sudo -n mkdir -p '$dest' && sudo -n tar --no-same-owner -xzf - -C '$dest' && sudo -n chmod 0755 '$dest' '$dest/host/apply.sh'"
}

cmd_deploy() {
  need_host
  preflight
  echo
  echo "  environment  $ENV_NAME"
  echo "  host         $HOST"
  echo "  version      $VERSION  ($(git log -1 --format=%s | cut -c1-60))"
  echo
  if [[ $ENV_NAME == production ]]; then
    local answer
    read -r -p "Type 'production' to deploy $VERSION to production: " answer
    [[ $answer == production ]] || die "cancelled"
  elif ((YES == 0)); then
    read -r -p "Deploy to $ENV_NAME? [y/N] " answer
    [[ $answer == [yY]* ]] || die "cancelled"
  fi

  remote 'command -v docker >/dev/null && command -v sops >/dev/null && sudo -n true' || die "$HOST: needs docker, sops and passwordless sudo for this user (docs/runbooks/deploy.md)"

  if ((SKIP_VERIFY)); then
    warn "skipping make verify"
  else
    say "make verify (tests, lint, vuln scan, web checks, smoke, browser tests)"
    make verify
  fi
  build_images
  ship
  say "rolling out on $HOST"
  remote sudo -n "$REMOTE_ROOT/$ENV_NAME/releases/$VERSION/host/apply.sh" deploy "$ENV_NAME" "$VERSION"
  say "done: $ENV_NAME is on $VERSION"
}

case $CMD in
local) cmd_local ;;
init-secrets) cmd_init_secrets ;;
deploy) cmd_deploy ;;
rollback)
  need_host
  remote_apply rollback "$ENV_NAME"
  ;;
status)
  need_host
  remote_apply status "$ENV_NAME"
  ;;
esac
