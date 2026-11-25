#!/usr/bin/env bash
# Browser end-to-end tests against the local Docker stack (make e2e).
#
# Prepares inputs through the real interfaces: demo data via the admin CLI
# (idempotent), a fresh one-time staff invitation, and charges created
# through the app API (/v1) with a throwaway key, paid where needed by the
# fake Daraja. Then runs Playwright in Microsoft's container.
#
# Checkout-page tokens handed to Playwright (the last path segment of each
# charge's checkout_url):
#   E2E_PENDING_TOKEN     a pending charge, nothing paid
#   E2E_SUCCEEDED_TOKEN   a charge paid by an M-Pesa prompt (fake Daraja)
#   E2E_EXPIRED_TOKEN     an expired charge from `seed demo` (empty if none:
#                         /v1 charges live at least 15 minutes)
#   E2E_STK_TOKEN         a fresh pending charge for the "prompt paid" test
#   E2E_STK_CANCEL_TOKEN  a fresh pending charge for the "prompt cancelled" test
# E2E_OPEN_TOKEN and E2E_PAID_TOKEN are kept as aliases of the pending and
# succeeded tokens for older specs.
set -euo pipefail
cd "$(dirname "$0")/.."

PW_IMAGE=mcr.microsoft.com/playwright:v1.65.1-noble
API=http://127.0.0.1:8088

admin() {
  TSP_ENV=development TSP_DATABASE_URL_FILE=deploy/dev-secrets/database_url \
    TSP_MASTER_KEYS_FILE=deploy/dev-secrets/master_keys TSP_API_KEY_PEPPER_FILE=deploy/dev-secrets/api_key_pepper \
    TSP_ADMIN_SETUP_BASE_URL=http://127.0.0.1:3001/setup/ \
    ./scripts/go.sh run ./cmd/talksasa-pay admin "$@" 2>/dev/null
}
sql() { docker compose exec -T postgres psql -U postgres -d talksasa_pay -Atc "$1"; }
json() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }
token_of() { grep -oE '[a-z2-7]{52}$'; } # checkout_url -> token

echo "== stack"
./scripts/dev-secrets.sh >/dev/null
docker compose up -d --build --wait >/dev/null 2>&1 || { docker compose ps; exit 1; }

echo "== test data"
admin seed demo --actor e2e >/dev/null
email="e2e-$(date +%s)-$RANDOM@talksasa.test"
setup_url=$(admin user invite --email "$email" --name "E2E Tester" --role admin --actor e2e | grep -oE 'http://[^ ]+/setup/[a-z2-7]{52}')
[[ -n "$setup_url" ]] || { echo "could not create invitation"; exit 1; }

# Charges through the real app API with a throwaway key that never leaves
# this process. The fake Daraja pays phones ending in 0 and cancels phones
# ending in 1.
keyout=$(admin key issue --app HOST --label "e2e $(date +%s)" --actor e2e)
api_key=$(grep -oE 'tsp_(test|live)_[a-z2-7]+_[a-z2-7]+' <<<"$keyout" | head -1)
key_id=$(grep -oE 'key id: +[a-z2-7]+' <<<"$keyout" | awk '{print $3}')
unset keyout
[[ -n "$api_key" ]] || { echo "could not issue an API key"; exit 1; }
trap '[[ -n "${key_id:-}" ]] && admin key revoke --app HOST --key-id "$key_id" --actor e2e >/dev/null 2>&1 || true' EXIT
v1() { curl -fsS -H "Authorization: Bearer $api_key" -H "Content-Type: application/json" "$@"; }
run="e2e-$(date +%s)-$RANDOM"
charge() { # name amount [extra json fields] -> charge json
  v1 -X POST "$API/v1/charges" -H "Idempotency-Key: $run-$1" \
    -d "{\"reference\":\"$run-$1\",\"amount\":$2,\"description\":\"E2E $1\"${3:+,$3}}"
}

pending_token=$(charge pending 120 | json 'd["checkout_url"]' | token_of)
paid=$(charge paid 150 '"phone":"0712345670"')
paid_id=$(json 'd["id"]' <<<"$paid")
paid_token=$(json 'd["checkout_url"]' <<<"$paid" | token_of)
status=""
for _ in $(seq 1 60); do
  status=$(v1 "$API/v1/charges/$paid_id" | json 'd["status"]')
  [[ "$status" == succeeded ]] && break
  sleep 0.5
done
[[ "$status" == succeeded ]] || { echo "the prompt-paid charge did not succeed (status $status)"; exit 1; }
stk_token=$(charge stk 120 | json 'd["checkout_url"]' | token_of)
stk_cancel_token=$(charge stk-cancel 120 | json 'd["checkout_url"]' | token_of)
expired_token=$(sql "SELECT hosted_token FROM charges WHERE status = 'expired' ORDER BY created_at LIMIT 1")
unset api_key
for t in pending_token paid_token stk_token stk_cancel_token; do
  [[ -n "${!t}" ]] || { echo "no checkout token for $t"; exit 1; }
done

echo "== playwright"
mkdir -p web/e2e/test-results
docker run --rm --network host --ipc host -u "$(id -u):$(id -g)" -e HOME=/tmp \
  -e E2E_SETUP_URL="$setup_url" -e E2E_EMAIL="$email" \
  -e E2E_PENDING_TOKEN="$pending_token" -e E2E_SUCCEEDED_TOKEN="$paid_token" -e E2E_EXPIRED_TOKEN="$expired_token" \
  -e E2E_OPEN_TOKEN="$pending_token" -e E2E_PAID_TOKEN="$paid_token" \
  -e E2E_STK_TOKEN="$stk_token" -e E2E_STK_CANCEL_TOKEN="$stk_cancel_token" \
  -v "$PWD/web:/web" -w /web/e2e "$PW_IMAGE" \
  ./node_modules/.bin/playwright test "$@"
