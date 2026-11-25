#!/usr/bin/env bash
# M-Pesa end-to-end test against the local Docker stack and the fake Daraja
# (make e2e-mpesa). Everything goes through real interfaces:
#
#   app API (POST /v1/charges, stk-push) -> fake Daraja -> Safaricom-style
#   callback to /cb/mpesa/... -> worker records the money and succeeds the
#   charge -> signed webhook -> a PHP receiver that verifies it with the PHP
#   SDK (clients/php: Verifier; data parsed generically).
#
# Cases (money rules in docs/api/charges.md):
#   1. charge with a phone -> prompt -> charge.succeeded
#   2. prompt cancelled by the customer -> stk_push.failed, charge still pending
#   3. paybill payment with the charge's account number + amount -> charge.succeeded
#   4. paybill payment while a prompt waits -> the prompt and the charge succeed
#   5. paybill payment with the app's prefix that matches nothing -> payment.received
#   6. unknown prefix -> unmatched (no event) -> `admin txn assign` -> payment.received
#   7. `admin txn reverse` -> transaction.reversed, charge reversed
#   8. a forged webhook is refused by the SDK
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
sql() { docker compose exec -T postgres psql -U postgres -d talksasa_pay -Atc "$1"; }
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
  if [[ -n "${key_id:-}" ]]; then admin key revoke --app "$PREFIX" --key-id "$key_id" --actor e2e-mpesa >/dev/null || true; fi
  admin app set-webhook --app "$PREFIX" --url "" --actor e2e-mpesa >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

echo "== stack"
./scripts/dev-secrets.sh >/dev/null
docker compose up -d --build --wait >/dev/null 2>&1 || { docker compose ps; exit 1; }

echo "== app, webhook secret, API key"
hook="http://127.0.0.1:$PORT/hook"
admin app list >"$work/apps.txt"
if awk '{print $2}' "$work/apps.txt" | grep -qx "$PREFIX"; then
  admin app set-webhook --app "$PREFIX" --url "$hook" --actor e2e-mpesa >/dev/null
  admin app rotate-webhook-secret --app "$PREFIX" --actor e2e-mpesa | secret_of whsec_ >"$work/webhook_secret"
else
  admin app create --name "M-Pesa E2E" --prefix "$PREFIX" --webhook-url "$hook" --actor e2e-mpesa | secret_of whsec_ >"$work/webhook_secret"
fi
[[ -s "$work/webhook_secret" ]] || { echo "no webhook secret"; exit 1; }
chmod 600 "$work/webhook_secret" # the receiver runs as this user
keyout=$(admin key issue --app "$PREFIX" --label "e2e-mpesa $(date +%s)" --actor e2e-mpesa)
key_id=$(grep -oE 'key id: +[a-z2-7]+' <<<"$keyout" | awk '{print $3}')
api_key=$(secret_of 'tsp_(test|live)_' <<<"$keyout")
unset keyout
[[ -n "$api_key" && -n "$key_id" ]] || { echo "could not issue an API key"; exit 1; }

# A prefix no app has, for the unmatched case.
letters=ABCDEFGHJKMNPRSTUVWXYZ
unknown=""
while [[ -z "$unknown" ]] || awk '{print $2}' "$work/apps.txt" | grep -qx "$unknown"; do
  unknown="Q"
  for _ in 1 2 3; do unknown+=${letters:RANDOM%${#letters}:1}; done
done

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
charge() { # name amount [extra json fields] -> charge json
  v1 -X POST "$API/v1/charges" -H "Idempotency-Key: $run-$1" \
    -d "{\"reference\":\"$run-$1\",\"amount\":$2,\"description\":\"M-Pesa E2E $1\"${3:+,$3}}"
}
paybill() { # account amount -> receipt
  curl -fsS -X POST "$FAKE/fake/c2b" -d "{\"bill_ref\":\"$1\",\"amount\":$2,\"phone\":\"254712345678\"}" | json 'd["receipt"]'
}
charge_status() { v1 "$API/v1/charges/$1" | json 'd["status"]'; }
# wait_event TYPE FIELD VALUE: until a verified event with FIELD=VALUE arrives (45 s)
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
count_events() { # TYPE FIELD VALUE
  python3 - "$work/out/events.jsonl" "$@" <<'PY'
import json, sys
path, typ, f, v = sys.argv[1:]
try:
    print(sum(1 for l in open(path) if json.loads(l)["type"] == typ and str(json.loads(l).get(f)) == v))
except FileNotFoundError:
    print(0)
PY
}

echo "== 1. charge with a phone: prompt paid"
c1=$(charge paid 150 '"phone":"0712345670"')
c1_id=$(json 'd["id"]' <<<"$c1")
check "charge created pending" "$(json 'd["status"]' <<<"$c1")" "pending"
check "first prompt sent with the charge" "$(json '(d["latest_stk_push"] or {}).get("status")' <<<"$c1")" "sent"
expect_event "charge.succeeded delivered and verified by the PHP SDK" charge.succeeded charge_id "$c1_id"
expect_event "stk_push.succeeded delivered and verified" stk_push.succeeded charge_id "$c1_id"
check "charge.succeeded carries the amount" "$(event_field charge.succeeded charge_id "$c1_id" amount)" "150"
c1_txn=$(event_field charge.succeeded charge_id "$c1_id" transaction_id)
check "charge succeeded (API)" "$(charge_status "$c1_id")" "succeeded"
txn1=$(v1 "$API/v1/transactions/$c1_txn")
check "paying transaction has an M-Pesa receipt" "$(json 'bool(d["receipt"])' <<<"$txn1")" "True"
check "paying transaction is for exactly the charge amount" "$(json 'd["amount"]' <<<"$txn1")" "150"

echo "== 2. prompt cancelled by the customer"
c2=$(charge cancel 90 '"phone":"0712345671"')
c2_id=$(json 'd["id"]' <<<"$c2")
expect_event "stk_push.failed delivered and verified" stk_push.failed charge_id "$c2_id"
check "result code is the customer's cancel" "$(event_field stk_push.failed charge_id "$c2_id" result_code)" "1032"
check "charge still pending (API)" "$(charge_status "$c2_id")" "pending"
check "no charge.succeeded for the cancelled prompt" "$(count_events charge.succeeded charge_id "$c2_id")" "0"

echo "== 3. paybill payment matching the charge"
c3=$(charge c2b 240)
c3_id=$(json 'd["id"]' <<<"$c3")
c3_acct=$(json 'd["account_number"]' <<<"$c3")
check "generated account number is 12 characters with the prefix" \
  "$([[ ${#c3_acct} -eq 12 && $c3_acct == "$PREFIX-"* ]] && echo yes || echo "no ($c3_acct)")" yes
r3=$(paybill "$c3_acct" 240)
expect_event "paybill payment succeeded the charge (charge.succeeded)" charge.succeeded charge_id "$c3_id"
check "the charge's payment carries the paybill receipt" \
  "$(v1 "$API/v1/transactions/$(event_field charge.succeeded charge_id "$c3_id" transaction_id)" | json 'd["receipt"]')" "$r3"

echo "== 4. paybill payment while a prompt waits"
c4=$(charge waiting 310 '"phone":"0712345673"') # fake: no callback, the prompt stays waiting
c4_id=$(json 'd["id"]' <<<"$c4")
c4_push=$(json 'd["latest_stk_push"]["id"]' <<<"$c4")
paybill "$(json 'd["account_number"]' <<<"$c4")" 310 >/dev/null
expect_event "paybill notice completed the waiting prompt (stk_push.succeeded)" stk_push.succeeded push_id "$c4_push"
expect_event "charge.succeeded for the prompt's charge" charge.succeeded charge_id "$c4_id"

echo "== 5. paybill payment with our prefix that matches no charge"
nomatch="$PREFIX-9$(printf '%06d' $((RANDOM * RANDOM % 1000000)))"
r5=$(paybill "$nomatch" 77)
expect_event "payment.received forwarded to the app" payment.received receipt "$r5"
check "payment.received has the account number as typed" "$(event_field payment.received receipt "$r5" account_number)" "$nomatch"
check "payment.received paid no charge" "$(event_field payment.received receipt "$r5" charge_id)" "None"

echo "== 6. unknown prefix: unmatched, then assigned by staff"
r6=$(paybill "$unknown-1042" 55)
t6=""
for _ in $(seq 1 60); do
  t6=$(sql "SELECT id FROM transactions WHERE provider = 'mpesa' AND provider_transaction_id = '$r6' AND app_id IS NULL")
  [[ -n "$t6" ]] && break
  sleep 0.5
done
check "unknown prefix recorded as unmatched" "$([[ -n "$t6" ]] && echo yes || echo no)" yes
check "no event before assignment" "$(count_events payment.received receipt "$r6")" "0"
if [[ -n "$t6" ]]; then
  admin txn assign --txn "$t6" --app "$PREFIX" --reason "e2e: customer mistyped the prefix" --actor e2e-mpesa >/dev/null ||
    bad "admin txn assign failed"
  expect_event "payment.received after admin txn assign" payment.received transaction_id "$t6"
  if admin txn assign --txn "$t6" --app "$PREFIX" --reason "e2e: second assign" --actor e2e-mpesa >/dev/null 2>&1; then
    bad "a payment can be assigned only once"
  else
    ok "a payment can be assigned only once"
  fi
fi

echo "== 7. reversal by an admin"
rr="RV$(date +%s | tail -c 9)"
admin txn reverse --txn "$c1_txn" --receipt "$rr" --reason "e2e: Safaricom reversed the payment" --actor e2e-mpesa >/dev/null ||
  bad "admin txn reverse failed"
expect_event "transaction.reversed delivered and verified" transaction.reversed reverses_transaction_id "$c1_txn"
check "reversal names the charge" "$(event_field transaction.reversed reverses_transaction_id "$c1_txn" charge_id)" "$c1_id"
check "reversal is kind reversal" "$(event_field transaction.reversed reverses_transaction_id "$c1_txn" kind)" "reversal"
check "charge reversed (API)" "$(charge_status "$c1_id")" "reversed"

echo "== 8. deliveries"
rejected() { if [[ -f "$work/out/rejected.jsonl" ]]; then wc -l <"$work/out/rejected.jsonl" | tr -d ' '; else echo 0; fi; }
check "no genuine webhook failed signature verification" "$(rejected)" "0"
# A forged delivery (right shape, wrong secret) must be refused by the SDK.
forged=$(docker exec "$RECEIVER" php -r '
  $body = json_encode(["object"=>"event","id"=>"00000000-0000-7000-8000-000000000000","type"=>"charge.succeeded","api_version"=>"2026-11-25","created_at"=>gmdate("c"),"data"=>new stdClass]);
  $t = time(); $sig = "t=$t,v1=" . hash_hmac("sha256", "$t.$body", "whsec_not_the_secret");
  $ctx = stream_context_create(["http"=>["method"=>"POST","header"=>"Content-Type: application/json\r\nTalksasa-Signature: $sig\r\n","content"=>$body,"ignore_errors"=>true]]);
  @file_get_contents("http://127.0.0.1:'"$PORT"'/hook", false, $ctx);
  echo explode(" ", $http_response_header[0] ?? "x 0")[1];')
check "forged webhook refused by the PHP SDK" "$forged" "400"
check "forged webhook logged as rejected" "$(rejected)" "1"
check "event id header matches body" "$(python3 -c "
import json
print(all(json.loads(l)['id']==json.loads(l)['header_id'] for l in open('$work/out/events.jsonl')))")" "True"
dups=$(python3 -c "
import json,collections
c=collections.Counter()
for l in open('$work/out/events.jsonl'):
    e=json.loads(l)
    c[e['id']]+=1
    if e['type'] in ('charge.succeeded','stk_push.succeeded','payment.received','transaction.reversed'):
        c[(e['type'], e.get('charge_id') if e['type']=='charge.succeeded' else e.get('push_id') or e.get('transaction_id'))]+=1
print(sum(1 for v in c.values() if v>1))")
check "no event (or money event for the same subject) delivered twice" "$dups" "0"

echo
echo "e2e-mpesa: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
