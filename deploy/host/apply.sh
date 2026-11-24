#!/usr/bin/env bash
# Talksasa Pay: host-side deploy step. Runs ON the deploy host as root,
# invoked by scripts/deploy.sh over SSH from inside a release directory:
#
#   apply.sh deploy   <env> <version>   decrypt secrets, back up DB, migrate, roll out, verify
#   apply.sh rollback <env>             put the previous release's containers back
#   apply.sh secrets  <env>             re-decrypt secrets to tmpfs (boot unit)
#   apply.sh status   <env>             show versions and container health
#
# It never prints secret values: they go from `sops --decrypt` straight into
# 0400 files on tmpfs.
set -Eeuo pipefail
umask 077

ROOT="${TSP_DEPLOY_ROOT:-/opt/talksasa-pay}"
ETC="${TSP_ETC_DIR:-/etc/talksasa-pay}"
RUN="${TSP_RUN_DIR:-/run/talksasa-pay}"
BACKUPS="${TSP_BACKUP_DIR:-/var/backups/talksasa-pay}"
LOGS="${TSP_LOG_DIR:-/var/log/talksasa-pay}"
AGE_KEY="${TSP_AGE_KEY_FILE:-$ETC/age.key}"
APP_UID=65532 # distroless nonroot (Go service)
PG_UID=70     # postgres:alpine
SECRET_NAMES=(pg_superuser_password pg_owner_password pg_app_password database_url migrate_database_url master_keys api_key_pepper)
PG_SECRETS=" pg_superuser_password pg_owner_password pg_app_password "

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33mWARN: %s\033[0m\n' "$*" >&2; }
die() {
  printf '\033[31mERROR: %s\033[0m\n' "$*" >&2
  exit 1
}

usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'; }

# ---------------------------------------------------------------- settings
# Parses KEY=VALUE lines instead of `source`-ing the file, so it can only set
# TSP_* variables and never run code.
load_env() {
  local file="$ETC/$ENV_NAME.env" line key val
  [[ -f $file ]] || die "missing $file (copy deploy/env.example there and fill it in)"
  while IFS= read -r line || [[ -n $line ]]; do
    line="${line%$'\r'}"
    [[ $line =~ ^[[:space:]]*(#|$) ]] && continue
    [[ $line =~ ^[[:space:]]*(TSP_[A-Z0-9_]+)=(.*)$ ]] || die "$file: cannot parse line: ${line%%=*}"
    key="${BASH_REMATCH[1]}" val="${BASH_REMATCH[2]}"
    val="${val%%[[:space:]]#*}"
    val="${val#"${val%%[![:space:]]*}"}" val="${val%"${val##*[![:space:]]}"}"
    [[ $val =~ ^\"(.*)\"$ || $val =~ ^\'(.*)\'$ ]] && val="${BASH_REMATCH[1]}"
    case $key in TSP_ENV | TSP_VERSION | TSP_SECRETS_DIR | TSP_PG_TLS_DIR | TSP_SHARED_DIR) die "$file must not set $key" ;; esac
    export "$key=$val"
  done <"$file"

  local missing=()
  for key in TSP_PAY_DOMAIN TSP_ADMIN_DOMAIN TSP_API_DOMAIN TSP_ACME_EMAIL TSP_ADMIN_ALLOW_CIDRS TSP_MPESA_PAYBILL; do
    [[ -n ${!key:-} ]] || missing+=("$key")
  done
  ((${#missing[@]} == 0)) || die "$file is missing: ${missing[*]}"
  [[ $TSP_MPESA_PAYBILL =~ ^[0-9]{5,7}$ ]] || die "TSP_MPESA_PAYBILL must be 5-7 digits"
  # Money safety: customers must never be shown a placeholder or the
  # Safaricom sandbox paybill in production.
  if [[ $ENV_NAME == production ]]; then
    case $TSP_MPESA_PAYBILL in 000000 | 174379) die "TSP_MPESA_PAYBILL is a placeholder/sandbox number; set the real Talksasa paybill" ;; esac
    [[ "$TSP_PAY_DOMAIN $TSP_ADMIN_DOMAIN $TSP_API_DOMAIN" != *localhost* ]] || die "production domains cannot be localhost"
  fi
  [[ $TSP_ADMIN_ALLOW_CIDRS != *203.0.113.10/32* ]] || die "TSP_ADMIN_ALLOW_CIDRS still has the example address"

  export TSP_ENV="$ENV_NAME"
  export TSP_SECRETS_DIR="$RUN/$ENV_NAME/secrets"
  export TSP_PG_TLS_DIR="$ETC/$ENV_NAME-pg-tls"
  export TSP_SHARED_DIR="$ROOT/$ENV_NAME/shared"
  HTTPS_PORT="${TSP_HTTPS_PORT:-443}"
}

compose() { docker compose -f "$RELEASE_DIR/compose.prod.yml" --project-directory "$RELEASE_DIR" "$@"; }

# SERVICES: the long-running services RELEASE_DIR's compose file defines, in
# start order. Older releases have no worker; rolling back to one removes it
# (--remove-orphans) instead of failing on an unknown service.
release_services() {
  local all s
  all=" $(compose config --services 2>/dev/null | tr '\n' ' ') "
  SERVICES=()
  for s in api worker admin pay caddy; do
    if [[ $all == *" $s "* ]]; then SERVICES+=("$s"); fi
  done
  [[ " ${SERVICES[*]} " == *" api "* ]] || die "$RELEASE_DIR/compose.prod.yml defines no api service"
}

wait_all_healthy() {
  local svc
  for svc in "${SERVICES[@]}"; do wait_healthy "$svc" || return 1; done
}

# ---------------------------------------------------------------- secrets
sops_bin() {
  command -v sops >/dev/null || die "sops is not installed on this host (see docs/runbooks/deploy.md)"
  SOPS_AGE_KEY_FILE="$AGE_KEY" sops "$@"
}

decrypt_secrets() {
  local enc="$1" dir="$RUN/$ENV_NAME/secrets" tmp name owner
  [[ -f $enc ]] || die "missing encrypted secrets bundle $enc"
  [[ -f $AGE_KEY ]] || die "missing age private key $AGE_KEY"
  [[ $(stat -c %a "$AGE_KEY") =~ ^[46]00$ ]] || die "$AGE_KEY must be mode 0400 or 0600"

  mkdir -p "$RUN/$ENV_NAME"
  chmod 0711 "$RUN" "$RUN/$ENV_NAME"
  [[ $(findmnt -n -o FSTYPE --target "$RUN/$ENV_NAME") == tmpfs ]] || die "$RUN is not on tmpfs; refusing to write secrets to disk"
  tmp="$(mktemp -d "$RUN/$ENV_NAME/.secrets.XXXXXX")"
  trap 'rm -rf "$tmp"' RETURN
  for name in "${SECRET_NAMES[@]}"; do
    if ! sops_bin --decrypt --extract "[\"$name\"]" "$enc" >"$tmp/$name" 2>/dev/null; then
      die "could not decrypt \"$name\" from $(basename "$enc") (wrong age key, or key missing from the bundle)"
    fi
    [[ -s $tmp/$name ]] || die "secret \"$name\" is empty"
    if [[ $PG_SECRETS == *" $name "* ]]; then owner=$PG_UID; else owner=$APP_UID; fi
    chown "$owner:$owner" "$tmp/$name"
    chmod 0400 "$tmp/$name"
  done
  # The service also enforces these, but fail here with a clearer message.
  grep -Eq 'sslmode=(require|verify-ca|verify-full)' "$tmp/database_url" || die "database_url must use sslmode=require (or stricter)"
  grep -Eq 'sslmode=(require|verify-ca|verify-full)' "$tmp/migrate_database_url" || die "migrate_database_url must use sslmode=require (or stricter)"
  (($(wc -c <"$tmp/api_key_pepper") >= 32)) || die "api_key_pepper must be at least 32 characters"

  chmod 0711 "$tmp"
  rm -rf "$dir.old"
  [[ -d $dir ]] && mv "$dir" "$dir.old"
  mv "$tmp" "$dir"
  rm -rf "$dir.old"
  trap - RETURN
  say "secrets decrypted to tmpfs (${#SECRET_NAMES[@]} files, 0400)"
}

ensure_pg_tls() {
  local dir="$TSP_PG_TLS_DIR"
  [[ -f $dir/server.crt && -f $dir/server.key ]] && return
  command -v openssl >/dev/null || die "openssl is needed once to create the Postgres TLS certificate"
  say "creating Postgres TLS certificate in $dir"
  mkdir -p "$dir"
  openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -subj "/CN=postgres" \
    -keyout "$dir/server.key" -out "$dir/server.crt" 2>/dev/null
  chown "$PG_UID:$PG_UID" "$dir/server.key" "$dir/server.crt"
  chmod 0600 "$dir/server.key"
  chmod 0644 "$dir/server.crt"
  chmod 0755 "$dir"
}

install_boot_unit() {
  command -v systemctl >/dev/null || return 0
  [[ -d /etc/systemd/system ]] || return 0
  local unit="/etc/systemd/system/talksasa-pay-secrets@.service"
  if ! cmp -s "$RELEASE_DIR/host/talksasa-pay-secrets@.service" "$unit"; then
    install -m 0644 "$RELEASE_DIR/host/talksasa-pay-secrets@.service" "$unit"
    systemctl daemon-reload
  fi
  systemctl enable --quiet "talksasa-pay-secrets@$ENV_NAME.service" || warn "could not enable the boot unit"
}

# Files mounted by long-lived containers live at stable paths; `cp` keeps the
# inode so the running Caddy sees the new Caddyfile on reload.
sync_shared() {
  mkdir -p "$TSP_SHARED_DIR/postgres"
  install -m 0755 "$RELEASE_DIR/postgres/init-roles.prod.sh" "$TSP_SHARED_DIR/postgres/init-roles.prod.sh"
  cp "$RELEASE_DIR/Caddyfile" "$TSP_SHARED_DIR/Caddyfile"
  chmod 0644 "$TSP_SHARED_DIR/Caddyfile"
}

reload_caddy() {
  compose exec -T caddy caddy reload --config /etc/caddy/Caddyfile >/dev/null 2>&1 || warn "caddy reload failed; the previous Caddyfile stays active"
}

# ---------------------------------------------------------------- helpers
container_health() {
  local id
  id="$(compose ps -q "$1" 2>/dev/null || true)"
  [[ -n $id ]] || {
    echo missing
    return
  }
  docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$id"
}

wait_healthy() {
  local deadline=$((SECONDS + ${2:-180})) s
  while ((SECONDS < deadline)); do
    s="$(container_health "$1")"
    [[ $s == healthy ]] && return 0
    [[ $s == unhealthy || $s == exited || $s == dead ]] && break
    sleep 2
  done
  warn "$1 is $(container_health "$1"); last log lines:"
  compose logs --no-color --tail 30 "$1" >&2 || true
  return 1
}

http_check() { # name url want...
  local name="$1" url="$2" host code
  shift 2
  host="$(sed -E 's#^https://([^/:]+).*#\1#' <<<"$url")"
  code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 15 \
    --resolve "$host:$HTTPS_PORT:127.0.0.1" "${url/$host/$host:$HTTPS_PORT}" || true)"
  for want in "$@"; do [[ $code == "$want" ]] && return 0; done
  warn "$name: $url returned $code (wanted $*)"
  return 1
}

verify_edge() {
  local ok=0
  http_check "pay page" "https://$TSP_PAY_DOMAIN/" 200 || ok=1
  http_check "pay page bad token" "https://$TSP_PAY_DOMAIN/not-a-token" 404 || ok=1
  http_check "api health" "https://$TSP_API_DOMAIN/healthz" 200 || ok=1
  http_check "api hides internals" "https://$TSP_API_DOMAIN/public/v1/pay/x" 404 || ok=1
  # 200 when this host is on the admin allow-list, 404 otherwise; both prove
  # Caddy is serving the admin site.
  http_check "admin panel" "https://$TSP_ADMIN_DOMAIN/login" 200 404 || ok=1
  return $ok
}

db_exists() {
  compose exec -T -u postgres postgres psql -tAc "select 1 from pg_database where datname='talksasa_pay'" 2>/dev/null | grep -q 1
}

backup_db() {
  local label="$1" dir="$BACKUPS/$ENV_NAME" file
  mkdir -p "$dir"
  chmod 0700 "$BACKUPS" "$dir"
  file="$dir/$(date -u +%Y%m%dT%H%M%SZ)-before-$label.dump"
  say "backing up database to $file"
  compose exec -T -u postgres postgres pg_dump -Fc talksasa_pay >"$file.partial" || die "pg_dump failed; nothing was changed"
  mv "$file.partial" "$file"
  # shellcheck disable=SC2012 # names are our own timestamps
  ls -1t "$dir"/*.dump 2>/dev/null | tail -n +"$((${TSP_BACKUP_KEEP:-30} + 1))" | xargs -r rm -f
}

log_deploy() {
  mkdir -p "$LOGS"
  printf '%s env=%s version=%s action=%s by=%s result=%s\n' "$(date -u +%FT%TZ)" "$ENV_NAME" \
    "${1}" "${2}" "${SUDO_USER:-$(id -un)}" "${3}" >>"$LOGS/deploys.log"
}

prune_releases() {
  local keep=5 dir v keep_versions=()
  # shellcheck disable=SC2012
  mapfile -t old < <(ls -1dt "$ENV_ROOT"/releases/*/ 2>/dev/null | tail -n +$((keep + 1)))
  for v in "$(readlink -f "$ENV_ROOT/current" || true)" "$(readlink -f "$ENV_ROOT/previous" || true)"; do keep_versions+=("$v"); done
  for dir in "${old[@]}"; do
    dir="${dir%/}"
    [[ " ${keep_versions[*]} " == *" $dir "* ]] && continue
    v="$(basename "$dir")"
    rm -rf "$dir"
    docker image rm "talksasa-pay:$v" "talksasa-pay-admin:$v" "talksasa-pay-page:$v" >/dev/null 2>&1 || true
  done
}

# ---------------------------------------------------------------- commands
cmd_deploy() {
  local version="$1" prev_dir="" prev_version=""
  [[ $version =~ ^[A-Za-z0-9._-]{1,64}$ ]] || die "bad version"
  RELEASE_DIR="$ENV_ROOT/releases/$version"
  [[ -f $RELEASE_DIR/compose.prod.yml ]] || die "release $version is not uploaded to $RELEASE_DIR"
  for img in "talksasa-pay:$version" "talksasa-pay-admin:$version" "talksasa-pay-page:$version"; do
    docker image inspect "$img" >/dev/null 2>&1 || die "image $img is not loaded on this host"
  done
  if [[ -L $ENV_ROOT/current ]]; then
    prev_dir="$(readlink -f "$ENV_ROOT/current")"
    prev_version="$(basename "$prev_dir")"
  fi
  export TSP_VERSION="$version"
  say "deploying $ENV_NAME $version (currently: ${prev_version:-nothing})"

  decrypt_secrets "$RELEASE_DIR/secrets.enc.yaml"
  ensure_pg_tls
  sync_shared
  compose config --quiet || die "compose file does not validate"

  compose up -d --wait postgres || die "postgres did not become healthy"
  local first_deploy=1
  if db_exists && [[ -n $prev_version ]]; then
    first_deploy=0
    backup_db "$version"
  fi

  say "running migrations"
  if ! compose --profile migrate run --rm -T migrate; then
    log_deploy "$version" deploy "migration-failed"
    die "migration failed; the running release ($prev_version) was not touched. A backup was taken before migrating (see $BACKUPS/$ENV_NAME)."
  fi

  say "starting $version"
  local healthy=1
  release_services
  compose up -d --remove-orphans "${SERVICES[@]}" || healthy=0
  ((healthy)) && reload_caddy
  if ((healthy)); then
    wait_all_healthy || healthy=0
  fi
  if ((healthy)); then
    say "checking the public edge"
    local tries=0
    until verify_edge; do
      ((++tries < 10)) || {
        healthy=0
        break
      }
      sleep 3
    done
  fi

  if ((!healthy)); then
    log_deploy "$version" deploy failed
    warn "container status and recent logs of the failed release:"
    compose ps -a --format '  {{.Service}}: {{.Status}}' >&2 || true
    compose logs --no-color --tail 20 "${SERVICES[@]:0:${#SERVICES[@]}-1}" >&2 || true # all but caddy
    if [[ -n $prev_version && -d $prev_dir ]]; then
      warn "deploy failed; restoring $prev_version (database migrations are NOT rolled back)"
      RELEASE_DIR="$prev_dir" TSP_VERSION="$prev_version"
      export TSP_VERSION
      sync_shared
      release_services
      compose up -d --remove-orphans "${SERVICES[@]}" && reload_caddy && wait_all_healthy && log_deploy "$prev_version" auto-rollback ok
    fi
    die "deploy of $version failed"
  fi

  if [[ -n $prev_version && $prev_version != "$version" ]]; then
    ln -sfn "$prev_dir" "$ENV_ROOT/previous"
  fi
  ln -sfn "$RELEASE_DIR" "$ENV_ROOT/current"
  install_boot_unit
  prune_releases
  log_deploy "$version" deploy ok
  ((first_deploy)) && say "first deploy: create the first admin with: docker compose -p talksasa-pay-$ENV_NAME exec api /talksasa-pay admin user invite --email ... --name ... --role admin --actor ..."
  say "deployed $ENV_NAME $version"
}

cmd_rollback() {
  [[ -L $ENV_ROOT/previous ]] || die "no previous release recorded for $ENV_NAME"
  local cur prev version
  cur="$(readlink -f "$ENV_ROOT/current")"
  prev="$(readlink -f "$ENV_ROOT/previous")"
  version="$(basename "$prev")"
  RELEASE_DIR="$prev"
  export TSP_VERSION="$version"
  say "rolling $ENV_NAME back to $version (database migrations are NOT rolled back)"
  decrypt_secrets "$RELEASE_DIR/secrets.enc.yaml"
  sync_shared
  release_services
  compose up -d --remove-orphans "${SERVICES[@]}"
  reload_caddy
  wait_all_healthy || {
    log_deploy "$version" rollback failed
    die "rollback: a service is unhealthy (see above)"
  }
  verify_edge || warn "edge checks failed after rollback"
  ln -sfn "$prev" "$ENV_ROOT/current"
  ln -sfn "$cur" "$ENV_ROOT/previous"
  log_deploy "$version" rollback ok
  say "rolled back to $version"
}

cmd_secrets() {
  [[ -L $ENV_ROOT/current ]] || die "nothing deployed for $ENV_NAME yet"
  RELEASE_DIR="$(readlink -f "$ENV_ROOT/current")"
  decrypt_secrets "$RELEASE_DIR/secrets.enc.yaml"
}

cmd_status() {
  [[ -L $ENV_ROOT/current ]] || die "nothing deployed for $ENV_NAME yet"
  RELEASE_DIR="$(readlink -f "$ENV_ROOT/current")"
  export TSP_VERSION
  TSP_VERSION="$(basename "$RELEASE_DIR")"
  echo "current:  $TSP_VERSION"
  [[ -L $ENV_ROOT/previous ]] && echo "previous: $(basename "$(readlink -f "$ENV_ROOT/previous")")"
  compose ps --format 'table {{.Service}}\t{{.Image}}\t{{.Status}}'
  tail -n 5 "$LOGS/deploys.log" 2>/dev/null || true
}

main() {
  (($# >= 2)) || {
    usage
    exit 2
  }
  local cmd="$1"
  ENV_NAME="$2"
  [[ $ENV_NAME == staging || $ENV_NAME == production ]] || die "environment must be staging or production"
  [[ $(id -u) == 0 ]] || die "run as root (sudo)"
  command -v docker >/dev/null || die "docker is not installed"
  docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"
  ENV_ROOT="$ROOT/$ENV_NAME"
  load_env

  mkdir -p /run/lock
  exec 9>"/run/lock/talksasa-pay-$ENV_NAME.lock"
  flock -n 9 || die "another deploy for $ENV_NAME is running"

  case $cmd in
  deploy)
    (($# == 3)) || die "usage: apply.sh deploy <env> <version>"
    cmd_deploy "$3"
    ;;
  rollback) cmd_rollback ;;
  secrets) cmd_secrets ;;
  status) cmd_status ;;
  *)
    usage
    exit 2
    ;;
  esac
}

main "$@"
