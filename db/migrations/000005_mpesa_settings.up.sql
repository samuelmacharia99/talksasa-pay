-- M-Pesa (Daraja) paybill settings, edited by admins in the panel.
--
-- One row (id = 1) for the whole gateway: one paybill, sandbox or
-- production. When the row is absent the api and worker fall back to the
-- TSP_MPESA_* configuration (SOPS); when present, it wins.
--
-- The three Daraja credentials are sealed with the master keyring
-- (AES-256-GCM, the same envelope as apps.webhook_secret_ciphertext). They
-- are write-only: the API never returns them. The callback token is NOT
-- stored here; it stays in TSP_MPESA_CALLBACK_TOKEN.
--
-- version goes up by one on every change of the paybill configuration (the
-- trigger sets it); the api and worker rebuild their Daraja client when it
-- changes. Recording a credential test or a URL registration leaves it alone.
-- The row is never deleted.

SET lock_timeout = '10s';

CREATE TABLE mpesa_settings (
  id                         smallint PRIMARY KEY DEFAULT 1 CHECK (id = 1),
  environment                text NOT NULL CHECK (environment IN ('sandbox', 'production')),
  -- The paybill: shown on the pay page, C2B URLs are registered for it, and
  -- a C2B confirmation for any other shortcode is not recorded.
  shortcode                  text NOT NULL CHECK (shortcode ~ '^[0-9]{5,7}$'),
  -- BusinessShortCode for STK push; NULL means the paybill.
  stk_shortcode              text CHECK (stk_shortcode IS NULL OR stk_shortcode ~ '^[0-9]{5,7}$'),
  consumer_key_ciphertext    bytea NOT NULL CHECK (octet_length(consumer_key_ciphertext) BETWEEN 29 AND 2048),
  consumer_secret_ciphertext bytea NOT NULL CHECK (octet_length(consumer_secret_ciphertext) BETWEEN 29 AND 2048),
  passkey_ciphertext         bytea NOT NULL CHECK (octet_length(passkey_ciphertext) BETWEEN 29 AND 2048),
  -- Master key version all three ciphertexts are sealed with.
  key_version                integer NOT NULL CHECK (key_version > 0),
  -- Public base Safaricom calls back (https outside development; the
  -- service enforces that, the database only the shape).
  callback_base_url          text NOT NULL CHECK (length(callback_base_url) <= 500 AND callback_base_url ~ '^https?://[^\s?#]+/$'),
  updated_by                 text NOT NULL CHECK (length(updated_by) BETWEEN 1 AND 200),
  updated_at                 timestamptz NOT NULL DEFAULT now(),
  version                    bigint NOT NULL DEFAULT 1 CHECK (version > 0),
  -- Last successful C2B URL registration. The URL is stored with the
  -- callback token replaced by [TOKEN].
  urls_registered_at         timestamptz,
  urls_registered_by         text CHECK (urls_registered_by IS NULL OR length(urls_registered_by) BETWEEN 1 AND 200),
  urls_registered_url        text CHECK (urls_registered_url IS NULL OR (length(urls_registered_url) <= 600 AND position('[TOKEN]' IN urls_registered_url) > 0)),
  urls_registered_shortcode  text CHECK (urls_registered_shortcode IS NULL OR urls_registered_shortcode ~ '^[0-9]{5,7}$'),
  -- Last credential test (an OAuth token request).
  last_test_at               timestamptz,
  last_test_ok               boolean,
  CHECK ((urls_registered_at IS NULL) = (urls_registered_by IS NULL)
     AND (urls_registered_at IS NULL) = (urls_registered_url IS NULL)
     AND (urls_registered_at IS NULL) = (urls_registered_shortcode IS NULL)),
  CHECK ((last_test_at IS NULL) = (last_test_ok IS NULL))
);
COMMENT ON TABLE mpesa_settings IS 'The gateway''s one M-Pesa paybill (Daraja) configuration. Credentials are encrypted and write-only. Never deleted.';

CREATE FUNCTION tsp_mpesa_settings_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id THEN
    RAISE EXCEPTION 'mpesa settings: id is immutable' USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.environment IS DISTINCT FROM OLD.environment OR NEW.shortcode IS DISTINCT FROM OLD.shortcode
     OR NEW.stk_shortcode IS DISTINCT FROM OLD.stk_shortcode
     OR NEW.consumer_key_ciphertext IS DISTINCT FROM OLD.consumer_key_ciphertext
     OR NEW.consumer_secret_ciphertext IS DISTINCT FROM OLD.consumer_secret_ciphertext
     OR NEW.passkey_ciphertext IS DISTINCT FROM OLD.passkey_ciphertext
     OR NEW.key_version IS DISTINCT FROM OLD.key_version
     OR NEW.callback_base_url IS DISTINCT FROM OLD.callback_base_url THEN
    -- A configuration change: the version moves on so every process
    -- rebuilds its Daraja client, and the change is stamped.
    NEW.version := OLD.version + 1;
    NEW.updated_at := now();
  ELSE
    -- Only a test or URL registration was recorded: who last changed the
    -- configuration, and when, stay as they were.
    NEW.version := OLD.version;
    NEW.updated_at := OLD.updated_at;
    NEW.updated_by := OLD.updated_by;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER mpesa_settings_guard BEFORE UPDATE ON mpesa_settings FOR EACH ROW EXECUTE FUNCTION tsp_mpesa_settings_guard();
CREATE TRIGGER mpesa_settings_no_delete BEFORE DELETE ON mpesa_settings FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER mpesa_settings_no_truncate BEFORE TRUNCATE ON mpesa_settings FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- Privileges
-- ---------------------------------------------------------------------------
REVOKE ALL ON mpesa_settings FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    GRANT SELECT, INSERT, UPDATE ON mpesa_settings TO talksasa_app;
  ELSE
    RAISE WARNING 'role talksasa_app does not exist; skipping grants';
  END IF;
END $$;
