-- Staff (admin panel) accounts, sessions and two-factor authentication, plus
-- indexes for the admin panel's list screens.
--
-- Security properties enforced here, in addition to Go:
--   * no row is ever deleted (accounts are disabled, sessions revoked), so the
--     audit trail of who could sign in, and when, survives;
--   * credentials are stored only as argon2id hashes (passwords), SHA-256 /
--     HMAC digests (session, invitation and recovery tokens) and AES-GCM
--     ciphertext (TOTP secrets); the CHECKs reject anything that looks like a
--     plaintext value;
--   * an account can only be `active` once it has both a password and TOTP;
--   * session stage, expiry and token digest are immutable, and a revoked
--     session or used invitation can never be revived.

SET lock_timeout = '10s';

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TYPE admin_role          AS ENUM ('viewer', 'finance', 'approver', 'admin');
CREATE TYPE admin_user_status   AS ENUM ('invited', 'active', 'disabled');
CREATE TYPE admin_session_stage AS ENUM ('mfa', 'full');

-- ---------------------------------------------------------------------------
-- Staff accounts
-- ---------------------------------------------------------------------------
CREATE TABLE admin_users (
  id                     uuid PRIMARY KEY,
  email                  text NOT NULL UNIQUE CHECK (
                           email = lower(email) AND length(email) <= 254
                           AND email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),
  name                   text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
  role                   admin_role NOT NULL,
  status                 admin_user_status NOT NULL DEFAULT 'invited',
  password_hash          text CHECK (password_hash IS NULL OR password_hash LIKE '$argon2id$%'),
  totp_ciphertext        bytea,
  totp_key_version       integer CHECK (totp_key_version IS NULL OR totp_key_version > 0),
  totp_last_step         bigint,
  failed_attempts        integer NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
  locked_until           timestamptz,
  last_login_at          timestamptz,
  password_changed_at    timestamptz,
  created_by             text NOT NULL,
  created_at             timestamptz NOT NULL DEFAULT now(),
  updated_at             timestamptz NOT NULL DEFAULT now(),
  disabled_at            timestamptz,
  disabled_by            text,
  CHECK ((totp_ciphertext IS NULL) = (totp_key_version IS NULL)),
  CHECK (status <> 'active' OR (password_hash IS NOT NULL AND totp_ciphertext IS NOT NULL)),
  CHECK ((status = 'disabled') = (disabled_at IS NOT NULL)),
  CHECK ((disabled_at IS NULL) = (disabled_by IS NULL))
);
COMMENT ON TABLE admin_users IS 'Talksasa staff who use the admin panel. Never deleted: offboarding sets status = disabled.';

-- Allowed status changes:
--   invited  -> active    (setup completed: password + TOTP enrolled)
--   active   -> invited   (2FA/password reset: credentials cleared, new invite)
--   disabled -> invited   (re-onboarding)
--   any      -> disabled
CREATE FUNCTION tsp_admin_users_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.email <> OLD.email OR NEW.created_at <> OLD.created_at OR NEW.created_by <> OLD.created_by THEN
    RAISE EXCEPTION 'admin_users id, email, created_at and created_by are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'invited'  AND NEW.status = 'active')
    OR (OLD.status = 'active'   AND NEW.status = 'invited')
    OR (OLD.status = 'disabled' AND NEW.status = 'invited')
    OR (NEW.status = 'disabled')) THEN
    RAISE EXCEPTION 'admin user status cannot change from % to %', OLD.status, NEW.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  -- Going back to `invited` must wipe credentials so the old ones stop working.
  IF NEW.status = 'invited' AND OLD.status <> 'invited'
     AND (NEW.password_hash IS NOT NULL OR NEW.totp_ciphertext IS NOT NULL) THEN
    RAISE EXCEPTION 'resetting an admin user to invited must clear password and TOTP'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER admin_users_guard BEFORE UPDATE ON admin_users FOR EACH ROW EXECUTE FUNCTION tsp_admin_users_guard();
CREATE TRIGGER admin_users_updated_at BEFORE UPDATE ON admin_users FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER admin_users_no_delete BEFORE DELETE ON admin_users FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER admin_users_no_truncate BEFORE TRUNCATE ON admin_users FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- One-time setup links (invitation / reset). Only the SHA-256 is stored.
-- ---------------------------------------------------------------------------
CREATE TABLE admin_invitations (
  id          uuid PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES admin_users (id),
  token_hash  bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  created_by  text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL,
  used_at     timestamptz,
  revoked_at  timestamptz,
  CHECK (expires_at > created_at),
  CHECK (used_at IS NULL OR revoked_at IS NULL)
);
CREATE INDEX admin_invitations_user ON admin_invitations (user_id) WHERE used_at IS NULL AND revoked_at IS NULL;

CREATE FUNCTION tsp_admin_invitations_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.user_id <> OLD.user_id OR NEW.token_hash <> OLD.token_hash
     OR NEW.created_at <> OLD.created_at OR NEW.expires_at <> OLD.expires_at OR NEW.created_by <> OLD.created_by THEN
    RAISE EXCEPTION 'admin invitation identity, token and expiry are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF (OLD.used_at IS NOT NULL AND NEW.used_at IS DISTINCT FROM OLD.used_at)
     OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
    RAISE EXCEPTION 'a used or revoked invitation cannot be changed'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER admin_invitations_guard BEFORE UPDATE ON admin_invitations FOR EACH ROW EXECUTE FUNCTION tsp_admin_invitations_guard();
CREATE TRIGGER admin_invitations_no_delete BEFORE DELETE ON admin_invitations FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER admin_invitations_no_truncate BEFORE TRUNCATE ON admin_invitations FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- Sessions. `mfa` sessions only allow the TOTP step; completing it creates a
-- new `full` session with a new token (no session fixation).
-- ---------------------------------------------------------------------------
CREATE TABLE admin_sessions (
  id            uuid PRIMARY KEY,
  user_id       uuid NOT NULL REFERENCES admin_users (id),
  token_hash    bytea NOT NULL UNIQUE CHECK (octet_length(token_hash) = 32),
  stage         admin_session_stage NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  ip            inet,
  user_agent    text CHECK (user_agent IS NULL OR length(user_agent) <= 300),
  revoked_at    timestamptz,
  revoke_reason text CHECK (revoke_reason IS NULL OR length(revoke_reason) <= 64),
  CHECK (expires_at > created_at),
  CHECK ((revoked_at IS NULL) = (revoke_reason IS NULL))
);
CREATE INDEX admin_sessions_user_live ON admin_sessions (user_id) WHERE revoked_at IS NULL;

CREATE FUNCTION tsp_admin_sessions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.user_id <> OLD.user_id OR NEW.token_hash <> OLD.token_hash OR NEW.stage <> OLD.stage
     OR NEW.created_at <> OLD.created_at OR NEW.expires_at <> OLD.expires_at
     OR NEW.ip IS DISTINCT FROM OLD.ip OR NEW.user_agent IS DISTINCT FROM OLD.user_agent THEN
    RAISE EXCEPTION 'admin session identity, stage and expiry are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.revoked_at IS NOT NULL AND (NEW.revoked_at IS DISTINCT FROM OLD.revoked_at OR NEW.last_seen_at <> OLD.last_seen_at) THEN
    RAISE EXCEPTION 'a revoked session cannot be changed'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER admin_sessions_guard BEFORE UPDATE ON admin_sessions FOR EACH ROW EXECUTE FUNCTION tsp_admin_sessions_guard();
CREATE TRIGGER admin_sessions_no_delete BEFORE DELETE ON admin_sessions FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER admin_sessions_no_truncate BEFORE TRUNCATE ON admin_sessions FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- Recovery codes (HMAC-SHA256 with the server pepper). Single use.
-- ---------------------------------------------------------------------------
CREATE TABLE admin_recovery_codes (
  id          uuid PRIMARY KEY,
  user_id     uuid NOT NULL REFERENCES admin_users (id),
  code_hash   bytea NOT NULL UNIQUE CHECK (octet_length(code_hash) = 32),
  created_at  timestamptz NOT NULL DEFAULT now(),
  used_at     timestamptz,
  revoked_at  timestamptz,
  CHECK (used_at IS NULL OR revoked_at IS NULL)
);
CREATE INDEX admin_recovery_codes_user_live ON admin_recovery_codes (user_id) WHERE used_at IS NULL AND revoked_at IS NULL;

CREATE FUNCTION tsp_admin_recovery_codes_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.user_id <> OLD.user_id OR NEW.code_hash <> OLD.code_hash OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'recovery code identity is immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF (OLD.used_at IS NOT NULL AND NEW.used_at IS DISTINCT FROM OLD.used_at)
     OR (OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at) THEN
    RAISE EXCEPTION 'a used or revoked recovery code cannot be changed'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER admin_recovery_codes_guard BEFORE UPDATE ON admin_recovery_codes FOR EACH ROW EXECUTE FUNCTION tsp_admin_recovery_codes_guard();
CREATE TRIGGER admin_recovery_codes_no_delete BEFORE DELETE ON admin_recovery_codes FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER admin_recovery_codes_no_truncate BEFORE TRUNCATE ON admin_recovery_codes FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- Indexes for the admin panel's list and search screens
-- ---------------------------------------------------------------------------
CREATE INDEX transactions_created ON transactions (created_at DESC, id DESC);
CREATE INDEX transactions_suspense ON transactions (created_at DESC, id DESC)
  WHERE payer_account_id IS NULL AND direction = 'in' AND status = 'succeeded';
CREATE INDEX transactions_needs_review ON transactions (created_at DESC) WHERE status = 'needs_review';
CREATE INDEX payment_requests_created ON payment_requests (created_at DESC, id DESC);
CREATE INDEX payment_requests_status ON payment_requests (status, created_at DESC);
CREATE INDEX payer_accounts_created ON payer_accounts (created_at DESC, id DESC);
CREATE INDEX payer_accounts_name_trgm ON payer_accounts USING gin (lower(display_name) gin_trgm_ops);
CREATE INDEX payer_accounts_ext_ref_trgm ON payer_accounts USING gin (lower(external_ref) gin_trgm_ops);
CREATE INDEX audit_log_occurred ON audit_log (occurred_at DESC, id DESC);

-- ---------------------------------------------------------------------------
-- Privileges
-- ---------------------------------------------------------------------------
REVOKE ALL ON admin_users, admin_invitations, admin_sessions, admin_recovery_codes FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    GRANT SELECT, INSERT, UPDATE ON admin_users, admin_invitations, admin_sessions, admin_recovery_codes TO talksasa_app;
  ELSE
    RAISE WARNING 'role talksasa_app does not exist; skipping grants';
  END IF;
END $$;
