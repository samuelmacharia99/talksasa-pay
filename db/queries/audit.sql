-- name: InsertAudit :exec
INSERT INTO audit_log (actor_type, actor_id, action, entity_type, entity_id, app_id, details)
VALUES (@actor_type, @actor_id, @action, @entity_type, @entity_id, @app_id, @details);

-- name: VerifyAuditChain :one
-- 0 means the whole chain verifies.
SELECT coalesce(tsp_audit_verify(), 0)::bigint AS first_bad_id;

-- name: SetActor :exec
-- Recorded by triggers (transaction_events.actor) for the current DB transaction.
SELECT set_config('talksasa.actor', @actor::text, true);
