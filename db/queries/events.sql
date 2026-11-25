-- name: InsertEvent :one
INSERT INTO events (id, app_id, type, subject_type, subject_id, data)
VALUES (@id, @app_id, @type, @subject_type, @subject_id, @data)
RETURNING *;

-- name: ListEventsFeed :many
-- Gap-free feed: only events written by transactions older than every
-- transaction still running, ordered by (txid, id). A cursor taken from this
-- list can never skip an event that commits later.
SELECT * FROM events
 WHERE app_id = @app_id
   AND (txid, id) > (sqlc.arg(after_txid)::bigint, sqlc.arg(after_id)::uuid)
   AND txid < (pg_snapshot_xmin(pg_current_snapshot())::text)::bigint
 ORDER BY txid, id
 LIMIT @lim;

-- name: GetEventForApp :one
SELECT * FROM events WHERE app_id = @app_id AND id = @id;

-- name: GetEventInternal :one
-- Worker only: loads the event a webhook job refers to.
SELECT * FROM events WHERE id = @id;
