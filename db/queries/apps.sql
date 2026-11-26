-- name: CreateApp :one
INSERT INTO apps (id, name, account_prefix, webhook_url, webhook_secret_ciphertext, webhook_secret_key_version, gateway_test)
VALUES (@id, @name, @account_prefix, @webhook_url, @webhook_secret_ciphertext, @webhook_secret_key_version, @gateway_test)
RETURNING *;

-- name: GetGatewayTestApp :one
-- The reserved internal app that Settings test payments are charged to.
SELECT * FROM apps WHERE gateway_test;

-- name: GetApp :one
SELECT * FROM apps WHERE id = @id;

-- name: GetAppByPrefix :one
SELECT * FROM apps WHERE account_prefix = @account_prefix;

-- name: ListApps :many
SELECT * FROM apps ORDER BY account_prefix;

-- name: UpdateAppWebhookSecret :one
-- The outgoing secret keeps signing until prev_expires_at (rotation grace).
UPDATE apps
   SET webhook_secret_prev_ciphertext  = webhook_secret_ciphertext,
       webhook_secret_prev_key_version = webhook_secret_key_version,
       webhook_secret_prev_expires_at  = sqlc.arg(prev_expires_at)::timestamptz,
       webhook_secret_ciphertext = @webhook_secret_ciphertext,
       webhook_secret_key_version = @webhook_secret_key_version
 WHERE id = @id
RETURNING *;

-- name: UpdateAppWebhookURL :one
UPDATE apps SET webhook_url = @webhook_url WHERE id = @id RETURNING *;

-- name: SetAppStatus :one
UPDATE apps SET status = @status WHERE id = @id RETURNING *;

-- name: CreateAPIKey :one
INSERT INTO app_api_keys (id, app_id, key_id, secret_hash, label, created_by, expires_at)
VALUES (@id, @app_id, @key_id, @secret_hash, @label, @created_by, @expires_at)
RETURNING *;

-- name: GetAPIKeyForAuth :one
SELECT k.id, k.app_id, k.key_id, k.secret_hash, k.expires_at, k.revoked_at, k.last_used_at, k.allowed_cidrs,
       a.status AS app_status, a.account_prefix, a.name AS app_name
  FROM app_api_keys k
  JOIN apps a ON a.id = k.app_id
 WHERE k.key_id = @key_id;

-- name: TouchAPIKey :exec
-- Throttled so authentication does not write on every request.
UPDATE app_api_keys
   SET last_used_at = now()
 WHERE id = @id
   AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: RevokeAPIKey :one
UPDATE app_api_keys
   SET revoked_at = now(), revoked_by = @revoked_by
 WHERE app_id = @app_id AND key_id = @key_id AND revoked_at IS NULL
RETURNING *;

-- name: ListAPIKeys :many
SELECT * FROM app_api_keys WHERE app_id = @app_id ORDER BY created_at;

-- name: SetAPIKeyAllowedCIDRs :one
UPDATE app_api_keys SET allowed_cidrs = sqlc.narg(allowed_cidrs)::cidr[]
 WHERE app_id = @app_id AND key_id = @key_id AND revoked_at IS NULL
RETURNING *;
