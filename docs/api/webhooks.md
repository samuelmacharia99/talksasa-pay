# Webhooks

When something changes for one of your payers, Talksasa Pay sends an HTTP
`POST` to your product's webhook URL. Talksasa staff set the URL (HTTPS
only) and give you a signing secret that starts with `whsec_`.

Webhooks are fast but not guaranteed to arrive in order, or only once. Build
your handler with that in mind and add the [events feed](events-feed.md) as
a backstop.

## Event types

| Type | `data` is | Sent when |
|---|---|---|
| `payment_request.created` | PaymentRequest | You created a request. |
| `payment_request.partially_paid` | PaymentRequest | A request moved to `partially_paid`: some money arrived, but not all. Not sent again for further part payments while it stays `partially_paid`; watch `transaction.allocated` for those. |
| `payment_request.paid` | PaymentRequest | A request is fully paid. |
| `payment_request.reopened` | PaymentRequest | A payment on the request was reversed, so it owes money again (`paid` to `partially_paid` or `open`, or `partially_paid` to `open`). |
| `payment_request.cancelled` | PaymentRequest | The request was cancelled. |
| `payment_request.expired` | PaymentRequest | The request passed its `expires_at` with nothing paid. |
| `transaction.allocated` | Transaction | A payment was matched to one of your payers and applied. `allocations` lists each bill it paid and any amount added to credit. |
| `transaction.reversed` | Transaction | A payment was reversed (for example M-Pesa reversed it). `data` is the reversal; `reverses_transaction_id` points to the original, and its allocations are negative. |
| `payer.credit_changed` | CreditChange | A payer's credit balance went up (overpayment) or down (credit applied to a new request, or a reversal). `delta` is the change, `credit_balance` the new total. |

One change can produce several events. A payment that settles one bill and
leaves change produces, in this order: `transaction.allocated`,
`payment_request.paid`, and `payer.credit_changed`. A new request that is
covered by existing credit produces `payment_request.created` followed by
`payment_request.paid` (or `partially_paid`) and `payer.credit_changed`.

`data` is the resource as it was when the event happened. New event types
may be added; ignore types you don't know (answer `200`).

## The request

```
POST /hooks/talksasa-pay HTTP/1.1
Content-Type: application/json
User-Agent: TalksasaPay-Webhooks/1
Talksasa-Event-Id: 0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b
Talksasa-Event-Type: payment_request.paid
Talksasa-Signature: t=1795510800,v1=65d85ffe5a454e3191fd569c1980fa2b550e824b592c476b6eca143bc43865bd

{"object":"event","id":"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","type":"payment_request.paid","api_version":"2026-11-24","created_at":"2026-11-24T09:00:00Z","data":{...}}
```

The body is the event envelope, the same object `GET /events` returns:

| Field | Meaning |
|---|---|
| `object` | Always `event`. |
| `id` | The event id (a UUID). Also in `Talksasa-Event-Id`. Unique forever. |
| `type` | One of the types above. Also in `Talksasa-Event-Type`. |
| `api_version` | The contract version the body follows, for example `2026-11-24`. |
| `created_at` | When the change happened. |
| `data` | The resource. |

## Verifying the signature

Never act on a webhook you have not verified. Anyone can send a `POST` to
your URL.

`Talksasa-Signature` has the form `t=<unix seconds>,v1=<hex>`, sometimes with
a second `v1=<hex>`. Each `v1` is:

```
HMAC-SHA256(key = your webhook secret, message = "<t>.<raw request body>")
```

written as lowercase hex. To verify:

1. Read the **raw** body bytes, before any JSON parsing. Re-encoding the JSON
   changes the bytes and breaks the signature.
2. Split the header on `,`. Take `t` and every `v1`.
3. Reject if `t` is more than **300 seconds** away from your clock (either
   direction). This stops replays of old messages. Keep your server clock in
   sync with NTP.
4. Compute the HMAC with your whole secret string, including the `whsec_`
   prefix, as the key.
5. Accept if **any** `v1` equals your result. Compare in constant time
   (`hash_equals` in PHP, `hmac.Equal` in Go).

### Worked example

```
secret  whsec_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst
t       1795510800
body    {"object":"event","id":"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","type":"payment_request.paid","api_version":"2026-11-24","created_at":"2026-11-24T09:00:00Z","data":{}}
message 1795510800.{"object":"event",...,"data":{}}
v1      65d85ffe5a454e3191fd569c1980fa2b550e824b592c476b6eca143bc43865bd
```

Check it from a shell:

```sh
printf '%s.%s' 1795510800 '{"object":"event","id":"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","type":"payment_request.paid","api_version":"2026-11-24","created_at":"2026-11-24T09:00:00Z","data":{}}' \
  | openssl dgst -sha256 -hmac 'whsec_abcdefghijklmnopqrstuvwxyz234567abcdefghijklmnopqrst'
```

### PHP

```php
/**
 * Returns true when $header is a valid Talksasa-Signature for $rawBody.
 * $rawBody must be the exact bytes received (file_get_contents('php://input')
 * or $request->getContent() in Laravel).
 */
function talksasa_verify(string $secret, string $header, string $rawBody, int $tolerance = 300): bool
{
    $t = null;
    $signatures = [];
    foreach (explode(',', $header) as $part) {
        [$k, $v] = array_pad(explode('=', trim($part), 2), 2, '');
        if ($k === 't' && ctype_digit($v)) {
            $t = (int) $v;
        } elseif ($k === 'v1' && $v !== '') {
            $signatures[] = $v;
        }
    }
    if ($t === null || $signatures === [] || abs(time() - $t) > $tolerance) {
        return false;
    }
    $expected = hash_hmac('sha256', $t . '.' . $rawBody, $secret);
    $ok = false;
    foreach ($signatures as $sig) {
        $ok = hash_equals($expected, $sig) || $ok; // check all; constant time
    }
    return $ok;
}
```

### Go

```go
import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Verify reports whether header is a valid Talksasa-Signature for body.
// body must be the raw bytes read from the request.
func Verify(secret, header string, body []byte, now time.Time) bool {
	var t int64
	var sigs []string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return false
			}
			t = n
		case "v1":
			sigs = append(sigs, v)
		}
	}
	if t == 0 || len(sigs) == 0 {
		return false
	}
	if d := now.Sub(time.Unix(t, 0)); d > 300*time.Second || d < -300*time.Second {
		return false
	}
	m := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(m, "%d.", t)
	m.Write(body)
	want := []byte(hex.EncodeToString(m.Sum(nil)))
	ok := false
	for _, s := range sigs {
		if hmac.Equal(want, []byte(s)) {
			ok = true
		}
	}
	return ok
}
```

## Secret rotation

When Talksasa staff rotate your webhook secret, you get the new secret once.
For the next **24 hours** every webhook carries two `v1` signatures: one made
with the new secret and one with the old. Because you accept a message if
*any* signature matches, your current code keeps working while you deploy
the new secret. After 24 hours only the new secret signs.

To rotate safely: ask staff to rotate, put the new secret in your
configuration and deploy within 24 hours. Rotate immediately if the secret
may have leaked.

## Handling deliveries correctly

Delivery is **at least once** and **not ordered**. The same event can arrive
twice, and `payment_request.paid` can arrive before the
`payment_request.created` for the same request.

1. **Verify** the signature. If it fails, answer `400` and stop.
2. **De-duplicate on the event id.** Store each `id` you process with a
   unique constraint. If the insert fails because it already exists, answer
   `200` and stop.
3. **Answer `2xx` fast**, within 10 seconds. Put the work on a queue and
   respond; don't call other services or send emails before responding.
4. **Re-read the resource** before acting on money. For
   `payment_request.*` call `GET /payment-requests/{id}` and use its
   `status`, `amount_paid` and `outstanding`. Treat the event as "something
   changed, go and look", not as the final word. This makes ordering
   irrelevant.
5. **Make your own updates idempotent.** "Mark invoice INV-42 paid" is safe to
   run twice; "add 1,500 to the customer's balance" is not.

Any response other than `2xx` counts as a failure, and so does no answer
within 10 seconds. Redirects are not followed (a `3xx` is a failure), so
give staff the final URL.

## Retries

Failed deliveries are retried with exponential backoff and random jitter,
starting after about 30 seconds and growing to a few hours apart, for **up to
25 attempts over about 3 days**. If you answer `429` or `503` with a
`Retry-After` header, the next attempt waits that long (up to 6 hours).

If every attempt fails, the delivery is marked failed. The event is not lost:
it stays in the [events feed](events-feed.md), and Talksasa staff can see
every attempt (status code, response time, error and the start of your
response body) in the admin panel and **redeliver** it once you have fixed
the problem. Ask them with the event id.

Talksasa Pay also slows down deliveries to an endpoint that keeps failing, so
a long outage on your side may mean events arrive in a burst once you
recover. Size your queue for it.

## Testing your endpoint

- Point a staging product at your staging URL and create a request with a
  `tsp_test_` key: you get a `payment_request.created` webhook at once.
- Log the `Talksasa-Event-Id` and the response you returned, never the
  secret or the signature.
