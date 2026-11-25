-- Webhook delivery: worker and admin queries (Phase 3).

-- name: GetWebhookDelivery :one
-- Unlocked read: the worker's cheap pre-check before doing any work.
SELECT * FROM webhook_deliveries WHERE event_id = @event_id;

-- name: GetAppWebhookTarget :one
-- What the webhook worker needs about an app (never the secrets).
SELECT id, status, webhook_url FROM apps WHERE id = @id;

-- name: ListWebhookDeliveriesWithApp :many
-- Admin list, newest first, keyset-paginated on (created_at, event_id).
SELECT d.event_id, d.app_id, p.account_prefix, e.type AS event_type, e.subject_type, e.subject_id,
       d.status, d.attempts, d.last_attempt_at, d.last_status_code, d.last_error, d.delivered_at, d.created_at
  FROM webhook_deliveries d
  JOIN events e ON e.id = d.event_id
  JOIN apps p ON p.id = d.app_id
 WHERE (sqlc.narg(app_id)::uuid IS NULL OR d.app_id = sqlc.narg(app_id))
   AND (sqlc.narg(status)::webhook_delivery_status IS NULL OR d.status = sqlc.narg(status))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (d.created_at, d.event_id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY d.created_at DESC, d.event_id DESC
 LIMIT @lim;

-- name: GetWebhookDeliveryWithEvent :one
-- Admin detail: the delivery, its app prefix and the event body.
SELECT d.event_id, d.app_id, p.account_prefix, e.type AS event_type, e.subject_type, e.subject_id,
       d.status, d.attempts, d.last_attempt_at, d.last_status_code, d.last_error, d.delivered_at, d.created_at,
       e.data AS event_data, e.created_at AS event_created_at
  FROM webhook_deliveries d
  JOIN events e ON e.id = d.event_id
  JOIN apps p ON p.id = d.app_id
 WHERE d.event_id = @event_id;
