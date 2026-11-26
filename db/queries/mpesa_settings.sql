-- name: GetMpesaSettings :one
SELECT * FROM mpesa_settings WHERE id = 1;

-- name: LockMpesaSettings :one
SELECT * FROM mpesa_settings WHERE id = 1 FOR UPDATE;

-- name: GetMpesaSettingsVersion :one
-- Polled by the api and worker (cheap) to notice a change.
SELECT version FROM mpesa_settings WHERE id = 1;

-- name: UpsertMpesaSettings :one
-- The guard trigger bumps version and updated_at on an update.
INSERT INTO mpesa_settings (id, environment, shortcode, stk_shortcode, consumer_key_ciphertext,
                            consumer_secret_ciphertext, passkey_ciphertext, callback_token_ciphertext,
                            key_version, callback_base_url, updated_by)
VALUES (1, @environment, @shortcode, sqlc.narg(stk_shortcode), @consumer_key_ciphertext,
        @consumer_secret_ciphertext, @passkey_ciphertext, @callback_token_ciphertext,
        @key_version, @callback_base_url, @updated_by)
ON CONFLICT (id) DO UPDATE
   SET environment = EXCLUDED.environment, shortcode = EXCLUDED.shortcode, stk_shortcode = EXCLUDED.stk_shortcode,
       consumer_key_ciphertext = EXCLUDED.consumer_key_ciphertext,
       consumer_secret_ciphertext = EXCLUDED.consumer_secret_ciphertext,
       passkey_ciphertext = EXCLUDED.passkey_ciphertext,
       callback_token_ciphertext = EXCLUDED.callback_token_ciphertext, key_version = EXCLUDED.key_version,
       callback_base_url = EXCLUDED.callback_base_url, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: RecordMpesaSettingsTest :execrows
UPDATE mpesa_settings SET last_test_at = sqlc.arg(tested_at)::timestamptz, last_test_ok = sqlc.arg(ok)::boolean WHERE id = 1;

-- name: RecordMpesaURLsRegistered :execrows
UPDATE mpesa_settings
   SET urls_registered_at = sqlc.arg(registered_at)::timestamptz, urls_registered_by = sqlc.arg(registered_by)::text,
       urls_registered_url = sqlc.arg(registered_url)::text, urls_registered_shortcode = sqlc.arg(registered_shortcode)::text
 WHERE id = 1;
