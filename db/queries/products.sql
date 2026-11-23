-- name: CreateProduct :one
INSERT INTO products (id, name, account_prefix, webhook_url, webhook_secret_ciphertext, webhook_secret_key_version)
VALUES (@id, @name, @account_prefix, @webhook_url, @webhook_secret_ciphertext, @webhook_secret_key_version)
RETURNING *;

-- name: CreateAccountCounter :exec
INSERT INTO product_account_counters (product_id, next_base) VALUES (@product_id, @first_base);

-- name: GetProduct :one
SELECT * FROM products WHERE id = @id;

-- name: GetProductByPrefix :one
SELECT * FROM products WHERE account_prefix = @account_prefix;

-- name: ListProducts :many
SELECT * FROM products ORDER BY account_prefix;

-- name: UpdateProductWebhookSecret :one
UPDATE products
   SET webhook_secret_ciphertext = @webhook_secret_ciphertext,
       webhook_secret_key_version = @webhook_secret_key_version
 WHERE id = @id
RETURNING *;

-- name: UpdateProductWebhookURL :one
UPDATE products SET webhook_url = @webhook_url WHERE id = @id RETURNING *;

-- name: SetProductStatus :one
UPDATE products SET status = @status WHERE id = @id RETURNING *;

-- name: NextAccountBase :one
-- Row-locks the counter, so concurrent payer creation never issues the same number.
UPDATE product_account_counters
   SET next_base = next_base + 1
 WHERE product_id = @product_id
RETURNING (next_base - 1)::bigint AS base;

-- name: CreateAPIKey :one
INSERT INTO product_api_keys (id, product_id, key_id, secret_hash, label, created_by, expires_at)
VALUES (@id, @product_id, @key_id, @secret_hash, @label, @created_by, @expires_at)
RETURNING *;

-- name: GetAPIKeyForAuth :one
SELECT k.id, k.product_id, k.key_id, k.secret_hash, k.expires_at, k.revoked_at, k.last_used_at,
       p.status AS product_status, p.account_prefix, p.name AS product_name
  FROM product_api_keys k
  JOIN products p ON p.id = k.product_id
 WHERE k.key_id = @key_id;

-- name: TouchAPIKey :exec
-- Throttled so authentication does not write on every request.
UPDATE product_api_keys
   SET last_used_at = now()
 WHERE id = @id
   AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute');

-- name: RevokeAPIKey :one
UPDATE product_api_keys
   SET revoked_at = now(), revoked_by = @revoked_by
 WHERE product_id = @product_id AND key_id = @key_id AND revoked_at IS NULL
RETURNING *;

-- name: ListAPIKeys :many
SELECT * FROM product_api_keys WHERE product_id = @product_id ORDER BY created_at;
