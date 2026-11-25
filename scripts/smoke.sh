#!/usr/bin/env bash
# End-to-end smoke test against the local Docker stack (make smoke).
#
# Builds and starts postgres + migrate + api + worker, then checks health, API-key
# auth, revocation, metrics isolation, container hardening, the audit chain
# and the money invariants (cmd/loadtest/invariants.sql: each receipt once,
# every succeeded charge paid by a matching receipted transaction, ...), and
# that the issued API key never appears in any log.
# It creates a throwaway app with a random prefix; it never deletes data.
set -euo pipefail
cd "$(dirname "$0")/.."

API=http://127.0.0.1:8088
METRICS=http://127.0.0.1:9464
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

pass=0
fail=0
ok()   { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (want $3, got $2)"; fi; }
status() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
admin() {
  TSP_ENV=development TSP_DATABASE_URL_FILE=deploy/dev-secrets/database_url \
    TSP_MASTER_KEYS_FILE=deploy/dev-secrets/master_keys TSP_API_KEY_PEPPER_FILE=deploy/dev-secrets/api_key_pepper \
    ./scripts/go.sh run ./cmd/talksasa-pay admin "$@" 2>>"$WORK/cli.log"
}

echo "== starting stack"
./scripts/dev-secrets.sh >/dev/null
docker compose up -d --build --wait >/dev/null 2>&1 || { docker compose ps; exit 1; }

echo "== health"
check "GET /healthz" "$(status $API/healthz)" 200
check "GET /readyz (database reachable)" "$(status $API/readyz)" 200
check "metrics served on internal port" "$(status $METRICS/metrics)" 200
check "metrics NOT served on public port" "$(status $API/metrics)" 404
check "worker container healthy" "$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose ps -q worker)")" healthy
check "worker /healthz reports running" \
  "$(docker compose exec -T worker /talksasa-pay healthcheck --url http://127.0.0.1:9465/healthz >/dev/null 2>&1 && echo ok || echo down)" ok

echo "== security headers"
hdrs=$(curl -s -D - -o /dev/null $API/healthz)
for h in X-Content-Type-Options X-Frame-Options Content-Security-Policy Strict-Transport-Security X-Request-Id; do
  if grep -qi "^$h:" <<<"$hdrs"; then ok "header $h"; else bad "header $h missing"; fi
done

echo "== container hardening"
for svc in api worker admin pay; do
  cid=$(docker compose ps -q "$svc")
  user=$(docker inspect -f '{{.Config.User}}' "$cid")
  check "$svc runs as non-root" "$([[ -n "$user" && "${user%%:*}" != 0 && "$user" != root ]] && echo yes || echo no)" yes
  check "$svc root filesystem read-only" "$(docker inspect -f '{{.HostConfig.ReadonlyRootfs}}' "$cid")" true
  check "$svc has all capabilities dropped" "$(docker inspect -f '{{.HostConfig.CapDrop}}' "$cid")" "[ALL]"
done

echo "== admin and pay front ends"
check "admin API port NOT reachable from the host" "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 http://127.0.0.1:8089/admin/v1/auth/me || true)" "000"
check "admin sign-in page loads" "$(status http://127.0.0.1:3001/login)" 200
check "admin pages without a session redirect" "$(status http://127.0.0.1:3001/transactions)" 307
csp=$(curl -s -D - -o /dev/null http://127.0.0.1:3001/login | grep -i '^content-security-policy:' || true)
if grep -q "nonce-" <<<"$csp" && grep -q "frame-ancestors 'none'" <<<"$csp"; then ok "admin CSP with nonce"; else bad "admin CSP with nonce"; fi
check "pay page: unknown link is 404" "$(status "http://127.0.0.1:3000/$(printf 'a%.0s' {1..52})")" 404
check "pay API: malformed token is 404" "$(status $API/public/v1/pay/not-a-token)" 404
check "pay API: never indexed" "$(curl -s -D - -o /dev/null $API/public/v1/pay/not-a-token | grep -ci '^x-robots-tag: noindex' || true)" 1

echo "== app and API key"
letters=ABCDEFGHIJKLMNOPQRSTUVWXYZ prefix=""
for _ in 1 2 3 4 5; do prefix+=${letters:RANDOM%26:1}; done
admin app create --name "Smoke $prefix" --prefix "$prefix" --actor smoke-test >"$WORK/app.out"
check "app $prefix created" "$(grep -c "account prefix: $prefix" "$WORK/app.out")" 1
if admin app create --name "Dup" --prefix "${prefix,,}" --actor smoke-test >/dev/null 2>&1; then
  bad "duplicate prefix rejected"
else
  ok "duplicate prefix rejected (case-insensitive)"
fi
if admin app create --name "No actor" --prefix ZZQ >/dev/null 2>&1; then
  bad "--actor required"
else
  ok "--actor required for writes"
fi

admin key issue --app "$prefix" --label smoke --actor smoke-test >"$WORK/key.out"
key=$(grep -oE 'tsp_(test|live)_[a-z2-7]+_[a-z2-7]+' "$WORK/key.out" | head -1)
keyid=$(awk '/key id:/ {print $3}' "$WORK/key.out")
if [[ -n "$key" && -n "$keyid" ]]; then ok "API key issued"; else bad "API key issued"; exit 1; fi

echo "== API authentication"
check "whoami without key -> 401" "$(status $API/v1/whoami)" 401
check "whoami with malformed key -> 401" "$(status -H 'Authorization: Bearer nope' $API/v1/whoami)" 401
check "whoami with wrong secret -> 401" "$(status -H "Authorization: Bearer ${key%_*}_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" $API/v1/whoami)" 401
check "whoami with key -> 200" "$(status -H "Authorization: Bearer $key" $API/v1/whoami)" 200
body=$(curl -s -H "Authorization: Bearer $key" $API/v1/whoami)
check "whoami reports the right app" "$(grep -c "\"$prefix\"" <<<"$body")" 1
check "whoami response does not echo the key" "$(grep -c "${key##*_}" <<<"$body" || true)" 0
ctype=$(curl -s -o /dev/null -w '%{content_type}' $API/v1/whoami)
check "errors are application/problem+json" "$ctype" "application/problem+json"

admin key revoke --app "$prefix" --key-id "$keyid" --actor smoke-test >/dev/null
check "revoked key -> 401" "$(status -H "Authorization: Bearer $key" $API/v1/whoami)" 401

echo "== integrity"
check "audit chain verifies" "$(admin audit verify)" "Audit log chain verified: OK"
# Money invariants straight from the database (read-only). Each output line
# is "<check>|<id>"; none means every succeeded charge's transaction matches
# it (same app and charge, exact amount, receipt), no receipt twice, etc.
violations=$(docker compose exec -T postgres psql -U postgres -d talksasa_pay -v ON_ERROR_STOP=1 -At \
  <cmd/loadtest/invariants.sql 2>&1) || violations="psql failed: $violations"
check "money invariants hold (charges, receipts, events, deliveries)" "${violations:-none}" none
[[ -z "$violations" ]] || printf '    %s\n' "$violations" | head -20

echo "== secret leakage"
secret=${key##*_}
docker compose logs --no-color api worker migrate >"$WORK/stack.log" 2>&1
check "API key secret absent from api/worker/migrate logs" "$(grep -c "$secret" "$WORK/stack.log" || true)" 0
check "API key secret absent from CLI logs" "$(grep -c "$secret" "$WORK/cli.log" || true)" 0
whsec=$(grep -oE 'whsec_[a-z2-7]+' "$WORK/app.out" | head -1)
check "webhook secret printed once at creation" "$([[ -n $whsec ]] && echo yes || echo no)" yes
check "webhook secret absent from all logs" "$(cat "$WORK/stack.log" "$WORK/cli.log" | grep -c "${whsec:-none}" || true)" 0
check "Authorization header never logged" "$(grep -ci 'bearer tsp_' "$WORK/stack.log" || true)" 0

echo
echo "smoke: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
