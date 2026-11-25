-- name: InsertWebhookDelivery :exec
INSERT INTO webhook_deliveries (event_id, app_id) VALUES (@event_id, @app_id);

-- name: LockWebhookDelivery :one
SELECT * FROM webhook_deliveries WHERE event_id = @event_id FOR NO KEY UPDATE;

-- name: RecordWebhookAttempt :one
-- Updates the summary row after one HTTP attempt.
UPDATE webhook_deliveries
   SET status = @status, attempts = attempts + 1, last_attempt_at = now(),
       last_status_code = sqlc.narg(last_status_code), last_error = sqlc.narg(last_error),
       delivered_at = CASE WHEN @status::webhook_delivery_status = 'delivered' THEN now() ELSE NULL END
 WHERE event_id = @event_id
RETURNING *;

-- name: SkipWebhookDelivery :exec
-- No webhook URL configured (or app suspended): nothing to send. The
-- app still sees the event in /v1/events.
UPDATE webhook_deliveries SET status = 'skipped', last_error = sqlc.narg(reason)
 WHERE event_id = @event_id AND status = 'pending';

-- name: InsertWebhookAttempt :exec
INSERT INTO webhook_attempts (event_id, attempt, url_host, status_code, latency_ms, error, response_body)
VALUES (@event_id, @attempt, @url_host, sqlc.narg(status_code), @latency_ms, sqlc.narg(error), sqlc.narg(response_body));

-- name: RequeueWebhookDelivery :one
-- Admin "redeliver": a failed or skipped delivery goes back to pending.
UPDATE webhook_deliveries SET status = 'pending', last_error = NULL
 WHERE event_id = @event_id AND status IN ('failed', 'skipped')
RETURNING *;

-- name: ListWebhookDeliveriesAdmin :many
SELECT d.*, e.type AS event_type, e.subject_type, e.subject_id
  FROM webhook_deliveries d
  JOIN events e ON e.id = d.event_id
 WHERE (sqlc.narg(app_id)::uuid IS NULL OR d.app_id = sqlc.narg(app_id))
   AND (sqlc.narg(status)::webhook_delivery_status IS NULL OR d.status = sqlc.narg(status))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (d.created_at, d.event_id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY d.created_at DESC, d.event_id DESC
 LIMIT @lim;

-- name: ListWebhookAttempts :many
SELECT * FROM webhook_attempts WHERE event_id = @event_id ORDER BY attempt;

-- name: DeleteOldWebhookAttempts :execrows
DELETE FROM webhook_attempts
 WHERE id IN (SELECT id FROM webhook_attempts WHERE attempted_at < sqlc.arg(before)::timestamptz LIMIT @batch_size);
