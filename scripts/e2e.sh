#!/usr/bin/env bash
# Browser end-to-end tests against the local Docker stack (make e2e).
#
# Prepares inputs through the real interfaces: demo data via the admin CLI
# (idempotent), a fresh one-time staff invitation, and payment-link tokens
# read from the database. Then runs Playwright in Microsoft's container.
set -euo pipefail
cd "$(dirname "$0")/.."

PW_IMAGE=mcr.microsoft.com/playwright:v1.65.1-noble

admin() {
  TSP_ENV=development TSP_DATABASE_URL_FILE=deploy/dev-secrets/database_url \
    TSP_MASTER_KEYS_FILE=deploy/dev-secrets/master_keys TSP_API_KEY_PEPPER_FILE=deploy/dev-secrets/api_key_pepper \
    TSP_ADMIN_SETUP_BASE_URL=http://127.0.0.1:3001/setup/ \
    ./scripts/go.sh run ./cmd/talksasa-pay admin "$@" 2>/dev/null
}
sql() { docker compose exec -T postgres psql -U postgres -d talksasa_pay -Atc "$1"; }

echo "== stack"
./scripts/dev-secrets.sh >/dev/null
docker compose up -d --build --wait >/dev/null 2>&1 || { docker compose ps; exit 1; }

echo "== test data"
admin seed demo --actor e2e >/dev/null
email="e2e-$(date +%s)-$RANDOM@talksasa.test"
setup_url=$(admin user invite --email "$email" --name "E2E Tester" --role admin --actor e2e | grep -oE 'http://[^ ]+/setup/[a-z2-7]{52}')
[[ -n "$setup_url" ]] || { echo "could not create invitation"; exit 1; }
open_token=$(sql "SELECT hosted_token FROM payment_requests WHERE status = 'open' ORDER BY created_at LIMIT 1")
partial_token=$(sql "SELECT hosted_token FROM payment_requests WHERE status = 'partially_paid' ORDER BY created_at LIMIT 1")
paid_token=$(sql "SELECT hosted_token FROM payment_requests WHERE status = 'paid' ORDER BY created_at LIMIT 1")

# Two fresh bills for the M-Pesa prompt tests (the fake Daraja pays phones
# ending in 0 and cancels phones ending in 1). Created through the real
# product API with a throwaway key that never leaves this process.
api_key=$(admin key issue --product HOST --label "e2e $(date +%s)" --actor e2e | grep -oE 'tsp_(test|live)_[A-Za-z0-9_]+' | head -1)
[[ -n "$api_key" ]] || { echo "could not issue an API key"; exit 1; }
v1() { curl -fsS -H "Authorization: Bearer $api_key" -H "Content-Type: application/json" "$@"; }
run="e2e-$(date +%s)-$RANDOM"
v1 -X PUT "http://127.0.0.1:8088/v1/payers/$run" -d '{"display_name":"E2E Payer"}' >/dev/null
stk_bill() {
  v1 -X POST http://127.0.0.1:8088/v1/payment-requests -H "Idempotency-Key: $run-$1" \
    -d "{\"payer\":\"$run\",\"reference\":\"$run-$1\",\"description\":\"E2E prompt test\",\"amount\":120}" |
    grep -oE '"pay_url":"[^"]+"' | grep -oE '[a-z2-7]{52}'
}
stk_token=$(stk_bill pay)
stk_cancel_token=$(stk_bill cancel)
unset api_key

echo "== playwright"
mkdir -p web/e2e/test-results
docker run --rm --network host --ipc host -u "$(id -u):$(id -g)" -e HOME=/tmp \
  -e E2E_SETUP_URL="$setup_url" -e E2E_EMAIL="$email" \
  -e E2E_OPEN_TOKEN="$open_token" -e E2E_PARTIAL_TOKEN="$partial_token" -e E2E_PAID_TOKEN="$paid_token" \
  -e E2E_STK_TOKEN="$stk_token" -e E2E_STK_CANCEL_TOKEN="$stk_cancel_token" \
  -v "$PWD/web:/web" -w /web/e2e "$PW_IMAGE" \
  ./node_modules/.bin/playwright test "$@"
