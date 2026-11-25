# Webhooks

When something changes for your app, Talksasa Pay sends an HTTP `POST` to
your app's webhook URL. Talksasa staff set the URL (HTTPS only) and give you
a signing secret that starts with `whsec_`.

Webhooks are fast but not guaranteed to arrive in order, or only once. Build
your handler with that in mind and add the [events feed](events-feed.md) as
a backstop.

## Event types

There are ten. `data` is the resource as it was when the event happened.

| Type | `data` is | Sent when |
|---|---|---|
| `charge.succeeded` | Charge | The charge is paid: Safaricom confirmed a payment with an M-Pesa receipt for exactly its amount. `transaction_id` names the payment. Also sent when late money succeeds an `expired` or `cancelled` charge. **The only event that means "paid".** |
| `charge.needs_review` | Charge | Safaricom said a prompt was paid but sent no receipt. Talksasa staff are checking. **Not paid.** |
| `charge.expired` | Charge | A `pending` charge passed its `expires_at`. |
| `charge.cancelled` | Charge | You cancelled the charge. |
| `stk_push.sent` | STKPush | An M-Pesa prompt is on the customer's phone. **Not paid.** |
| `stk_push.failed` | STKPush | The prompt was not paid: cancelled, timed out, insufficient funds, wrong PIN... `result_code` / `result_desc` say which. Nothing was charged and the charge stays `pending`. |
| `stk_push.succeeded` | STKPush | Safaricom confirmed the prompt with a receipt. `charge.succeeded` comes with it; act on that. |
| `stk_push.needs_review` | STKPush | Success without a receipt; comes with `charge.needs_review`. **Not paid.** |
| `payment.received` | PaymentReceived | Money reached your app without paying a waiting charge: a paybill payment with your prefix that matched nothing, an unmatched payment staff assigned to you, or a second payment for a charge that had already succeeded (`duplicate_of_charge` set). See [charges](charges.md#money-that-pays-no-charge). |
| `transaction.reversed` | Transaction | Talksasa staff recorded a reversal of a payment. `data` is the reversal transaction: `reverses_transaction_id` names the payment, `charge_id` the charge (now `reversed`), if any. |

One change can produce several events. A paid prompt produces
`stk_push.sent`, then `stk_push.succeeded` and `charge.succeeded` (order not
guaranteed). **Mark an invoice paid only on `charge.succeeded`**, never on
a `stk_push.*` event. See [M-Pesa payment prompts](stk-push.md).

New event types may be added; ignore types you don't know (answer `200`).

### Payloads

`charge.*` events carry the Charge, exactly as `GET /charges/{id}` returns
it at that moment:

```json
{
  "object": "event",
  "id": "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
  "type": "charge.succeeded",
  "api_version": "2026-11-25",
  "created_at": "2026-11-25T09:00:04Z",
  "data": {
    "object": "charge",
    "id": "0193a5f2-0000-7000-8000-0000000000aa",
    "reference": "INV-2026-0042",
    "amount": 1500,
    "currency": "KES",
    "description": "Cloud Hosting, December 2026",
    "metadata": { "customer_id": "1042" },
    "account_number": "HOST-4K7QZ9M",
    "status": "succeeded",
    "checkout_url": "https://pay.../k3j...",
    "paybill": null,
    "expires_at": "2026-11-26T09:00:00Z",
    "succeeded_at": "2026-11-25T09:00:04Z",
    "transaction_id": "0193a5f2-0000-7000-8000-0000000000dd",
    "latest_stk_push": { "object": "stk_push", "id": "0193...", "status": "succeeded", "...": "..." },
    "created_at": "2026-11-25T09:00:00Z",
    "updated_at": "2026-11-25T09:00:04Z"
  }
}
```

`stk_push.*` events carry the STKPush (see
[M-Pesa payment prompts](stk-push.md#prompt-an-existing-charge)), with
`charge_id`, `status`, `result_code` and `result_desc`.

`payment.received` carries the Transaction plus `duplicate_of_charge`:

```json
{
  "object": "event",
  "type": "payment.received",
  "...": "...",
  "data": {
    "object": "transaction",
    "id": "0193a5f2-0000-7000-8000-0000000000ee",
    "kind": "payment",
    "direction": "in",
    "channel": "mpesa_c2b",
    "status": "succeeded",
    "amount": 700,
    "currency": "KES",
    "receipt": "SKP4ABCD12",
    "phone": "2547****5678",
    "account_number": "HOST-1042",
    "charge_id": null,
    "reverses_transaction_id": null,
    "duplicate_of_charge": null,
    "occurred_at": "2026-11-25T10:42:00Z",
    "created_at": "2026-11-25T10:42:01Z",
    "completed_at": "2026-11-25T10:42:01Z"
  }
}
```

`transaction.reversed` carries the reversal Transaction: `kind` is
`reversal`, `direction` is `out`, `receipt` is Safaricom's reversal receipt,
`reverses_transaction_id` is the payment it undoes and `charge_id` the
charge that is now `reversed` (null if the payment paid no charge).

## The request

```
POST /hooks/talksasa-pay HTTP/1.1
Content-Type: application/json
User-Agent: TalksasaPay-Webhooks/1
Talksasa-Event-Id: 0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b
Talksasa-Event-Type: charge.succeeded
Talksasa-Signature: t=1795597200,v1=9ca7f905f4fc927940b5b7aa5134574247f011487e00617b39074cd30aadd70c

{"object":"event","id":"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","type":"charge.succeeded","api_version":"2026-11-25","created_at":"2026-11-25T09:00:00Z","data":{...}}
```

The body is the event envelope, the same object `GET /events` returns:

| Field | Meaning |
|---|---|
| `object` | Always `event`. |
| `id` | The event id (a UUID). Also in `Talksasa-Event-Id`. Unique forever. |
| `type` | One of the types above. Also in `Talksasa-Event-Type`. |
| `api_version` | The contract version the body follows, for example `2026-11-25`. |
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
t       1795597200
body    {"object":"event","id":"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","type":"charge.succeeded","api_version":"2026-11-25","created_at":"2026-11-25T09:00:00Z","data":{}}
message 1795597200.{"object":"event",...,"data":{}}
v1      9ca7f905f4fc927940b5b7aa5134574247f011487e00617b39074cd30aadd70c
```

Check it from a shell:

```sh
printf '%s.%s' 1795597200 '{"object":"event","id":"0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b","type":"charge.succeeded","api_version":"2026-11-25","created_at":"2026-11-25T09:00:00Z","data":{}}' \
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
twice, and `charge.succeeded` can arrive before the `stk_push.sent` for the
same charge.

1. **Verify** the signature. If it fails, answer `400` and stop.
2. **De-duplicate on the event id.** Store each `id` you process with a
   unique constraint. If the insert fails because it already exists, answer
   `200` and stop.
3. **Answer `2xx` fast**, within 10 seconds. Put the work on a queue and
   respond; don't call other services or send emails before responding.
4. **Re-read the resource** before acting on money. For `charge.*` call
   `GET /charges/{id}` and use its `status`. Treat the event as "something
   changed, go and look", not as the final word. This makes ordering
   irrelevant.
5. **Make your own updates idempotent.** "Mark invoice INV-42 paid" is safe to
   run twice; "add 1,500 to the customer's balance" is not. For
   `payment.received`, key whatever you do on the transaction `id`, so a
   repeat delivery never applies the money twice.

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

- Point a staging app at your staging URL, create a charge with a
  `tsp_test_` key and cancel it: you get a `charge.cancelled` webhook at
  once. Locally, a charge with a phone ending in `0` gives you
  `charge.succeeded` within a second (fake Daraja).
- Log the `Talksasa-Event-Id` and the response you returned, never the
  secret or the signature.
