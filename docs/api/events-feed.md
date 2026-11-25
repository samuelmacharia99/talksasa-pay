# Events feed

`GET /v1/events` returns every event for your app, oldest first, in pages.
It holds exactly the same events as the webhooks, with the same ids and
bodies. Webhooks get news to you within seconds; the feed makes sure nothing
is ever missed.

Use both: webhooks for speed, a scheduled poll of the feed as a backstop.
If you can't receive webhooks at all (for example during development behind
a firewall), polling the feed alone is a complete integration.

## Why it is gap-free

The feed never skips an event, even one from a database transaction that
committed after later events were already visible. Talksasa Pay holds back
an event until every transaction that could come before it has finished.
In practice new events show up in the feed within a second or two. The
practical upshot: once you have read past a cursor, nothing new will ever
appear before it.

The trade-off: while a long-running database transaction is open on our side
(for example the backup taken just before a deploy), the newest events wait
in the feed until it finishes. Nothing is lost and webhooks are not
affected; the feed simply catches up afterwards. Don't treat a quiet feed as
"no payments": treat it as "nothing new yet".

## Reading the feed

```sh
# First run: start from the beginning.
curl -sS "$TSP_API/events?limit=200" -H "Authorization: Bearer $TSP_KEY"
```

```json
{
  "data": [ { "object": "event", "id": "0193...", "type": "charge.succeeded", "...": "..." } ],
  "next_cursor": "eyJ0...",
  "has_more": true
}
```

```sh
# Next page, and every later poll: pass the stored cursor as `after`.
curl -sS "$TSP_API/events?after=eyJ0...&limit=200" -H "Authorization: Bearer $TSP_KEY"
```

| Field | Meaning |
|---|---|
| `data` | Up to `limit` events (1-200, default 50), oldest first. |
| `next_cursor` | Pass it as `after` next time. `null` when `data` is empty: keep the cursor you already have. |
| `has_more` | `true` when more events are ready right now. Keep reading without waiting. |

The cursor is opaque. Store it as a string and send it back unchanged; don't
build or edit it.

## Storing the cursor

Keep one cursor per app per environment in your database, for example a
row in a `settings` or `talksasa_pay_cursors` table.

Save the new cursor **after** the page's events are processed, in the same
database transaction as your processing if you can. If your job crashes
halfway, the next run re-reads part of a page. That's fine because you
de-duplicate on event id (see below). Saving the cursor first and crashing
would skip events, which is not fine.

## Recommended reconciliation loop

Run it every minute or two from your scheduler (cron, Laravel's scheduler, a
worker loop). Make sure only one copy runs at a time.

```text
cursor = load_cursor()            # null on the very first run
loop:
    page = GET /events?after=cursor&limit=200     # omit `after` when null
    for event in page.data:
        if already_processed(event.id):           # same table the webhook handler uses
            continue
        handle(event)                             # same code the webhook handler uses
        mark_processed(event.id)
    if page.next_cursor != null:
        cursor = page.next_cursor
        save_cursor(cursor)
    if not page.has_more:
        break
```

Notes:

- **Share the de-duplication table and the handler with your webhook
  endpoint.** An event processed from a webhook is skipped by the poll and
  the other way round. This is what makes the two safe to run together.
- **Re-read before acting on money**, exactly as for webhooks: on a
  `charge.*` event, fetch `GET /charges/{id}` and use its current `status`.
- **First run on an app with history**: starting with no cursor replays
  every event since the app was created. That is safe with
  de-duplication but may take a few pages. If you only care about new events,
  read to the end once without handling, save the cursor, then switch on
  handling.
- Respect `429` and `503` with `Retry-After` as described in
  [idempotency and retries](idempotency-and-retries.md).
- If the loop finds an event your webhook endpoint never processed, that is
  a sign webhooks are failing. Log it, and check with Talksasa staff, who can
  see every delivery attempt.
