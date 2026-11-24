-- Development only (the migrate command refuses down migrations elsewhere).
DROP INDEX IF EXISTS payment_requests_expiring;
ALTER TABLE products
  DROP CONSTRAINT IF EXISTS products_webhook_secret_prev_complete,
  DROP COLUMN IF EXISTS webhook_secret_prev_expires_at,
  DROP COLUMN IF EXISTS webhook_secret_prev_key_version,
  DROP COLUMN IF EXISTS webhook_secret_prev_ciphertext;
ALTER TABLE product_api_keys DROP COLUMN IF EXISTS allowed_cidrs;
DROP TABLE IF EXISTS webhook_attempts, webhook_deliveries, events CASCADE;
DROP FUNCTION IF EXISTS tsp_webhook_deliveries_guard() CASCADE;
DROP TYPE IF EXISTS webhook_delivery_status;
