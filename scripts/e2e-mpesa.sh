#!/usr/bin/env bash
# M-Pesa end-to-end test against the local Docker stack and the fake Daraja
# (make e2e-mpesa). Everything goes through real interfaces:
#
#   product API (payer, bill, stk-push) -> fake Daraja -> Safaricom-style
#   callback to /cb/mpesa/... -> worker records + allocates the money ->
#   signed webhook -> a PHP receiver that verifies it with the PHP SDK
#   (clients/php: Verifier + typed Event models).
#
# Cases: prompt paid; prompt cancelled by the customer; a paybill (C2B)
# payment that matches a waiting prompt (completes that prompt).
#
# The receiver runs in the worker container's network namespace so the
# worker reaches it on 127.0.0.1 (TSP_WEBHOOK_ALLOW_LOOPBACK, development
# only). Secrets (API key, webhook secret) live in a 0700 temp dir and are
# never printed.
set -euo pipefail
cd "$(dirname "$0")/.."

PREFIX=MPXE
API=http://127.0.0.1:8088
FAKE=http://127.0.0.1:8090
PORT=9900
RECEIVER=talksasa-pay-e2e-webhook-receiver
WORKER=talksasa-pay-worker-1

admin() {
  TSP_ENV=development TSP_DATABASE_URL_FILE=deploy/dev-secrets/database_url \
    TSP_MASTER_KEYS_FILE=deploy/dev-secrets/master_keys TSP_API_KEY_PEPPER_FILE=deploy/dev-secrets/api_key_pepper \
    ./scripts/go.sh run ./cmd/talksasa-pay admin "$@" 2>/dev/null
}
secret_of() { grep -oE "$1[A-Za-z0-9_]+" | head -1; }

pass=0 fail=0
ok() { printf '  \033[32mPASS\033[0m %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf '  \033[31mFAIL\033[0m %s\n' "$1"; fail=$((fail + 1)); }
check() { if [[ "$2" == "$3" ]]; then ok "$1"; else bad "$1 (got '$2', want '$3')"; fi; }

work=$(mktemp -d)
chmod 700 "$work"
mkdir -p "$work/out"
cleanup() {
  docker rm -f "$RECEIVER" >/dev/null 2>&1 || true
  if [[ -n "${key_id:-}" ]]; then admin key revoke --product "$PREFIX" --key-id "$key_id" --actor e2e-mpesa >/dev/null || true; fi
  admin product set-webhook --product "$PREFIX" --url "" --actor e2e-mpesa >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "== stack"
./scripts/dev-secrets.sh >/dev/null
docker compose up -d --build --wait >/dev/null 2>&1 || { docker compose ps; exit 1; }

echo "== product, webhook secret, API key"
hook="http://127.0.0.1:$PORT/hook"
if admin product list | awk '{print $2}' | grep -qx "$PREFIX"; then
  admin product set-webhook --product "$PREFIX" --url "$hook" --actor e2e-mpesa >/dev/null
  admin product rotate-webhook-secret --product "$PREFIX" --actor e2e-mpesa | secret_of whsec_ >"$work/webhook_secret"
else
  admin product create --name "M-Pesa E2E" --prefix "$PREFIX" --webhook-url "$hook" --actor e2e-mpesa | secret_of whsec_ >"$work/webhook_secret"
fi
[[ -s "$work/webhook_secret" ]] || { echo "no webhook secret"; exit 1; }
chmod 600 "$work/webhook_secret" # the receiver runs as this user
keyout=$(admin key issue --product "$PREFIX" --label "e2e-mpesa $(date +%s)" --actor e2e-mpesa)
key_id=$(grep -oE 'key id: +[a-z2-7]+' <<<"$keyout" | awk '{print $3}')
api_key=$(secret_of 'tsp_(test|live)_' <<<"$keyout")
unset keyout
[[ -n "$api_key" && -n "$key_id" ]] || { echo "could not issue an API key"; exit 1; }

echo "== C2B URLs (fake Daraja)"
docker compose exec -T api /talksasa-pay admin mpesa register-urls --actor e2e-mpesa >/dev/null 2>&1

echo "== webhook receiver (PHP SDK) in the worker's network namespace"
docker rm -f "$RECEIVER" >/dev/null 2>&1 || true
docker run -d --name "$RECEIVER" --network "container:$WORKER" -u "$(id -u):$(id -g)" \
  -v "$PWD/clients/php:/sdk:ro" -v "$PWD/scripts/e2e:/app:ro" -v "$work/out:/out" \
  -v "$work/webhook_secret:/run/webhook_secret:ro" \
  php:8.4-cli php -S "127.0.0.1:$PORT" /app/webhook-receiver.php >/dev/null
for _ in $(seq 1 30); do
  docker exec "$RECEIVER" php -r "exit(@file_get_contents('http://127.0.0.1:$PORT/') === \"ok\n\" ? 0 : 1);" && break
  sleep 0.5
done

v1() { curl -fsS -H "Authorization: Bearer $api_key" -H "Content-Type: application/json" "$@"; }
json() { python3 -c "import sys,json; d=json.load(sys.stdin); print($1)"; }
run="e2e$(date +%s)$RANDOM"
v1 -X PUT "$API/v1/payers/$run" -d '{"display_name":"M-Pesa E2E customer"}' >"$work/payer.json"
account=$(json 'd["account_number"]' <"$work/payer.json")

bill() { # reference amount -> id
  v1 -X POST "$API/v1/payment-requests" -H "Idempotency-Key: $run-$1" \
    -d "{\"payer\":\"$run\",\"reference\":\"$run-$1\",\"description\":\"M-Pesa E2E $1\",\"amount\":$2}" | json 'd["id"]'
}
push() { # request-id phone key -> push json
  v1 -X POST "$API/v1/payment-requests/$1/stk-push" -H "Idempotency-Key: $3" -d "{\"phone\":\"$2\"}"
}
# wait_event TYPE FIELD VALUE: until a verified event with data FIELD=VALUE arrives (45 s)
wait_event() {
  for _ in $(seq 1 90); do
    if [[ -f "$work/out/events.jsonl" ]] && python3 - "$work/out/events.jsonl" "$1" "$2" "$3" <<'PY'
import json, sys
path, typ, field, value = sys.argv[1:]
for line in open(path):
    e = json.loads(line)
    if e["type"] == typ and str(e.get(field)) == value:
        sys.exit(0)
sys.exit(1)
PY
    then return 0; fi
    sleep 0.5
  done
  return 1
}
expect_event() { # DESC TYPE FIELD VALUE
  local desc=$1
  shift
  if wait_event "$@"; then ok "$desc"; else bad "$desc (no verified $1 event with $2=$3)"; fi
}
event_field() { # TYPE MATCHFIELD MATCHVALUE FIELD
  python3 - "$work/out/events.jsonl" "$@" <<'PY'
import json, sys
path, typ, mf, mv, field = sys.argv[1:]
for line in open(path):
    e = json.loads(line)
    if e["type"] == typ and str(e.get(mf)) == mv:
        print(e.get(field)); break
PY
}

echo "== prompt paid"
req=$(bill paid 150)
p=$(push "$req" 0712345670 "$run-push-paid")
pid=$(json 'd["id"]' <<<"$p")
check "push accepted" "$(json 'd["status"]' <<<"$p")" "sent"
expect_event "payment_request.paid delivered and verified by the PHP SDK" payment_request.paid request_id "$req"
expect_event "stk_push.succeeded delivered and verified" stk_push.succeeded push_id "$pid"
check "paid event amount_paid" "$(event_field payment_request.paid request_id "$req" amount_paid)" "150"
expect_event "transaction.allocated delivered" transaction.allocated status succeeded
check "request paid (API)" "$(v1 "$API/v1/payment-requests/$req" | json 'd["status"]')" "paid"

echo "== prompt cancelled by the customer"
req2=$(bill cancel 90)
p2=$(push "$req2" 0712345671 "$run-push-cancel")
pid2=$(json 'd["id"]' <<<"$p2")
expect_event "stk_push.failed delivered and verified" stk_push.failed push_id "$pid2"
check "failure is the customer's cancel (SDK)" "$(event_field stk_push.failed push_id "$pid2" cancelled)" "True"
check "result code" "$(event_field stk_push.failed push_id "$pid2" result_code)" "1032"
check "request still open (API)" "$(v1 "$API/v1/payment-requests/$req2" | json 'd["status"]')" "open"

echo "== paybill payment matching a waiting prompt"
req3=$(bill c2b 240)
p3=$(push "$req3" 0712345673 "$run-push-c2b") # fake: no callback, status stays processing
pid3=$(json 'd["id"]' <<<"$p3")
txn3=$(json 'd["transaction_id"]' <<<"$p3")
curl -fsS -X POST "$FAKE/fake/c2b" -d "{\"bill_ref\":\"$account\",\"amount\":240}" >/dev/null
expect_event "paybill notice completed the waiting prompt (stk_push.succeeded)" stk_push.succeeded push_id "$pid3"
expect_event "payment_request.paid for the prompt's bill" payment_request.paid request_id "$req3"
check "money on the prompt's own transaction" "$(event_field stk_push.succeeded push_id "$pid3" transaction_id)" "$txn3"

echo "== deliveries"
rejected() { if [[ -f "$work/out/rejected.jsonl" ]]; then wc -l <"$work/out/rejected.jsonl" | tr -d ' '; else echo 0; fi; }
check "no genuine webhook failed signature verification" "$(rejected)" "0"
# A forged delivery (right shape, wrong secret) must be refused by the SDK.
forged=$(docker exec "$RECEIVER" php -r '
  $body = json_encode(["object"=>"event","id"=>"00000000-0000-7000-8000-000000000000","type"=>"payment_request.paid","api_version"=>"2026-11-24","created_at"=>gmdate("c"),"data"=>new stdClass]);
  $t = time(); $sig = "t=$t,v1=" . hash_hmac("sha256", "$t.$body", "whsec_not_the_secret");
  $ctx = stream_context_create(["http"=>["method"=>"POST","header"=>"Content-Type: application/json\r\nTalksasa-Signature: $sig\r\n","content"=>$body,"ignore_errors"=>true]]);
  @file_get_contents("http://127.0.0.1:'"$PORT"'/hook", false, $ctx);
  echo explode(" ", $http_response_header[0] ?? "x 0")[1];')
check "forged webhook refused by the PHP SDK" "$forged" "400"
check "forged webhook logged as rejected" "$(rejected)" "1"
check "event id header matches body" "$(python3 -c "
import json,sys
print(all(json.loads(l)['id']==json.loads(l)['header_id'] for l in open('$work/out/events.jsonl')))")" "True"
dups=$(python3 -c "
import json,collections
c=collections.Counter((json.loads(l)['type'], json.loads(l).get('push_id') or json.loads(l).get('request_id') or json.loads(l).get('transaction_id')) for l in open('$work/out/events.jsonl'))
print(sum(1 for k,v in c.items() if v>1 and k[0].startswith(('stk_push.','payment_request.paid'))))")
check "no event delivered twice for the same subject" "$dups" "0"

echo
echo "e2e-mpesa: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
