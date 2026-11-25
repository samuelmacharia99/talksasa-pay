-- Talksasa Pay core schema: a generic payment gateway.
--
-- Apps (any system holding an API key) create charges: "collect KES X from
-- this phone for my reference Y". The gateway asks M-Pesa, and a charge is
-- succeeded only when a succeeded transaction carrying an M-Pesa receipt is
-- attached to it. The gateway knows nothing about an app's customers or
-- invoices (decision 2026-11-25, ADR 0002).
--
-- Money is always bigint whole KES. Integrity rules live in the database as
-- well as in Go, so a bug in application code cannot silently corrupt them:
--   * transactions and audit rows are never deleted; amounts never change;
--   * a charge's amount, reference and account number never change, and it
--     can only become succeeded by pointing at a succeeded transaction of
--     the same app for exactly its amount (charges_guard below);
--   * composite foreign keys (app_id, id) make cross-app references
--     impossible.
--
-- The application connects as role talksasa_app, which has no DELETE or
-- TRUNCATE privileges. Migrations run as the schema owner.

SET lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Types
-- ---------------------------------------------------------------------------
CREATE TYPE app_status       AS ENUM ('active', 'suspended');
CREATE TYPE charge_status    AS ENUM ('pending', 'needs_review', 'succeeded', 'expired', 'cancelled', 'reversed');
CREATE TYPE txn_direction    AS ENUM ('in', 'out');
CREATE TYPE txn_kind         AS ENUM ('payment', 'refund', 'reversal', 'adjustment');
CREATE TYPE txn_channel      AS ENUM ('mpesa_stk', 'mpesa_c2b', 'mpesa_b2c', 'manual');
CREATE TYPE txn_status       AS ENUM ('pending', 'succeeded', 'failed', 'cancelled', 'needs_review');
CREATE TYPE audit_actor_type AS ENUM ('system', 'admin', 'cli', 'app');

-- ---------------------------------------------------------------------------
-- Shared trigger functions
-- ---------------------------------------------------------------------------
CREATE FUNCTION tsp_set_updated_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.updated_at := now();
  RETURN NEW;
END $$;

CREATE FUNCTION tsp_forbid_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'rows in % are permanent and cannot be deleted', TG_TABLE_NAME
    USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE FUNCTION tsp_forbid_update() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'rows in % are append-only and cannot be updated', TG_TABLE_NAME
    USING ERRCODE = 'integrity_constraint_violation';
END $$;

CREATE FUNCTION tsp_forbid_truncate() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION '% cannot be truncated', TG_TABLE_NAME
    USING ERRCODE = 'integrity_constraint_violation';
END $$;

-- ---------------------------------------------------------------------------
-- Apps
-- ---------------------------------------------------------------------------
CREATE TABLE apps (
  id                              uuid PRIMARY KEY,
  name                            text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
  -- Paybill account numbers starting with this prefix (then a digit) are
  -- routed to this app, e.g. HOST-1042 -> the app with prefix HOST.
  account_prefix                  text NOT NULL UNIQUE CHECK (account_prefix ~ '^[A-Z]{2,6}$'),
  webhook_url                     text CHECK (webhook_url IS NULL OR (webhook_url ~ '^https?://' AND length(webhook_url) <= 2048)),
  webhook_secret_ciphertext       bytea NOT NULL,
  webhook_secret_key_version      integer NOT NULL CHECK (webhook_secret_key_version > 0),
  -- The previous webhook secret keeps signing for 24 h after a rotation.
  webhook_secret_prev_ciphertext  bytea,
  webhook_secret_prev_key_version integer CHECK (webhook_secret_prev_key_version IS NULL OR webhook_secret_prev_key_version > 0),
  webhook_secret_prev_expires_at  timestamptz,
  status                          app_status NOT NULL DEFAULT 'active',
  created_at                      timestamptz NOT NULL DEFAULT now(),
  updated_at                      timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT apps_webhook_secret_prev_complete CHECK (
    (webhook_secret_prev_ciphertext IS NULL) = (webhook_secret_prev_key_version IS NULL)
    AND (webhook_secret_prev_ciphertext IS NULL) = (webhook_secret_prev_expires_at IS NULL))
);
COMMENT ON TABLE apps IS 'Systems that collect money through the gateway (API keys, webhook). The gateway holds no data about their customers.';

CREATE FUNCTION tsp_apps_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.account_prefix <> OLD.account_prefix OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'apps.id, account_prefix and created_at are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER apps_guard BEFORE UPDATE ON apps FOR EACH ROW EXECUTE FUNCTION tsp_apps_guard();
CREATE TRIGGER apps_updated_at BEFORE UPDATE ON apps FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER apps_no_delete BEFORE DELETE ON apps FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

CREATE TABLE app_api_keys (
  id            uuid PRIMARY KEY,
  app_id        uuid NOT NULL REFERENCES apps (id),
  key_id        text NOT NULL UNIQUE CHECK (key_id ~ '^[a-z2-7]{16,32}$'),
  secret_hash   bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
  label         text NOT NULL CHECK (length(btrim(label)) BETWEEN 1 AND 100),
  -- NULL = any address. Otherwise requests must come from one of these networks.
  allowed_cidrs cidr[] CHECK (allowed_cidrs IS NULL OR cardinality(allowed_cidrs) BETWEEN 1 AND 50),
  created_by    text NOT NULL,
  created_at    timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz,
  revoked_at    timestamptz,
  revoked_by    text,
  last_used_at  timestamptz,
  CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE INDEX app_api_keys_app_idx ON app_api_keys (app_id);

CREATE FUNCTION tsp_api_keys_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.app_id <> OLD.app_id OR NEW.key_id <> OLD.key_id
     OR NEW.secret_hash <> OLD.secret_hash OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'API key identity and hash are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN
    RAISE EXCEPTION 'a revoked API key cannot be un-revoked'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER app_api_keys_guard BEFORE UPDATE ON app_api_keys FOR EACH ROW EXECUTE FUNCTION tsp_api_keys_guard();
CREATE TRIGGER app_api_keys_no_delete BEFORE DELETE ON app_api_keys FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

-- ---------------------------------------------------------------------------
-- Charges
-- ---------------------------------------------------------------------------
-- One request by an app to collect an exact amount. Money rules (approved
-- 2026-11-25):
--   * amount, reference and account number never change;
--   * succeeded only via a succeeded, receipted transaction of the same app
--     for exactly the amount (checked below);
--   * a failed prompt does not fail the charge: it stays pending;
--   * late money still succeeds a pending, expired or cancelled charge;
--   * money for an already-succeeded charge is its own transaction
--     (transactions.charge_id = this charge, but not charges.transaction_id).
CREATE TABLE charges (
  id                  uuid PRIMARY KEY,
  app_id              uuid NOT NULL REFERENCES apps (id),
  -- The app's own reference. Unique per app for ever (like Paystack), so a
  -- late payment can never collide with a newer charge.
  reference           text NOT NULL CHECK (length(btrim(reference)) BETWEEN 1 AND 128),
  description         text CHECK (description IS NULL OR length(btrim(description)) BETWEEN 1 AND 500),
  currency            char(3) NOT NULL DEFAULT 'KES' CHECK (currency = 'KES'),
  amount              bigint NOT NULL CHECK (amount BETWEEN 1 AND 250000),
  metadata            jsonb CHECK (metadata IS NULL OR (jsonb_typeof(metadata) = 'object' AND octet_length(metadata::text) <= 4096)),
  -- What the customer types at the paybill, also sent as the STK
  -- AccountReference (Daraja allows 12 characters). Either the app's own
  -- (e.g. its customer number HOST-1042, may repeat across charges) or
  -- generated (unique while live).
  account_number      text NOT NULL CHECK (account_number ~ '^[A-Z]{2,6}-[0-9][A-Z0-9]*$' AND length(account_number) <= 12),
  account_number_norm text NOT NULL CHECK (account_number_norm = replace(account_number, '-', '')),
  account_generated   boolean NOT NULL,
  -- Optional phone the app gave (the first prompt goes there).
  phone               text CHECK (phone IS NULL OR phone ~ '^254[17][0-9]{8}$'),
  status              charge_status NOT NULL DEFAULT 'pending',
  hosted_token        text NOT NULL UNIQUE CHECK (length(hosted_token) >= 40),
  idempotency_key     text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
  request_hash        bytea NOT NULL,
  expires_at          timestamptz NOT NULL,
  -- The transaction that paid this charge. Set once.
  transaction_id      uuid,
  succeeded_at        timestamptz,
  closed_at           timestamptz, -- expired / cancelled
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (app_id, id),
  UNIQUE (app_id, reference),
  UNIQUE (app_id, idempotency_key),
  CHECK (account_number LIKE '%-%'),
  CHECK ((status IN ('succeeded', 'reversed')) = (transaction_id IS NOT NULL)),
  CHECK ((status IN ('succeeded', 'reversed')) = (succeeded_at IS NOT NULL)),
  CHECK (status NOT IN ('expired', 'cancelled') OR closed_at IS NOT NULL)
);
COMMENT ON TABLE charges IS 'A request by an app to collect an exact amount. Paid only by a receipted transaction.';
CREATE INDEX charges_app_created ON charges (app_id, created_at DESC, id DESC);
-- Paybill matching: live charges by account number.
CREATE INDEX charges_live_account ON charges (app_id, account_number_norm) WHERE status IN ('pending', 'needs_review');
-- A generated account number identifies exactly one live charge.
CREATE UNIQUE INDEX charges_generated_account_live ON charges (app_id, account_number_norm)
  WHERE account_generated AND status IN ('pending', 'needs_review');
CREATE INDEX charges_expiring ON charges (expires_at) WHERE status = 'pending';

CREATE FUNCTION tsp_charges_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  v_prefix text;
BEGIN
  IF NEW.status <> 'pending' OR NEW.transaction_id IS NOT NULL THEN
    RAISE EXCEPTION 'charges are created pending and unpaid' USING ERRCODE = 'check_violation';
  END IF;
  SELECT account_prefix INTO v_prefix FROM apps WHERE id = NEW.app_id;
  IF NEW.account_number NOT LIKE v_prefix || '-%' THEN
    RAISE EXCEPTION 'account number % does not carry app prefix %', NEW.account_number, v_prefix
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER charges_insert BEFORE INSERT ON charges FOR EACH ROW EXECUTE FUNCTION tsp_charges_insert();

CREATE TRIGGER charges_updated_at BEFORE UPDATE ON charges FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER charges_no_delete BEFORE DELETE ON charges FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER charges_no_truncate BEFORE TRUNCATE ON charges FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- Transactions ledger (append-only)
-- ---------------------------------------------------------------------------
CREATE TABLE transactions (
  id                      uuid PRIMARY KEY,
  -- NULL only while a payment cannot be routed to an app (unmatched).
  app_id                  uuid REFERENCES apps (id),
  -- The charge this money was for, when known. Several transactions may
  -- name the same charge (a customer who paid twice); the one that paid it
  -- is charges.transaction_id.
  charge_id               uuid,
  direction               txn_direction NOT NULL,
  kind                    txn_kind NOT NULL,
  channel                 txn_channel NOT NULL,
  -- Receipt namespace: all M-Pesa channels share one receipt space, so the
  -- same receipt can never be recorded twice even via different channels.
  provider                text NOT NULL CHECK (provider IN ('mpesa', 'manual')),
  status                  txn_status NOT NULL,
  amount                  bigint NOT NULL CHECK (amount > 0 AND amount <= 1000000000000),
  currency                char(3) NOT NULL DEFAULT 'KES' CHECK (currency = 'KES'),
  provider_transaction_id text CHECK (provider_transaction_id IS NULL OR length(provider_transaction_id) BETWEEN 1 AND 64),
  provider_request_id     text CHECK (provider_request_id IS NULL OR length(provider_request_id) BETWEEN 1 AND 128),
  phone                   text CHECK (phone IS NULL OR phone ~ '^254[17][0-9]{8}$'),
  -- The account number as the customer typed it / as sent with the prompt.
  account_number          text CHECK (account_number IS NULL OR length(account_number) <= 64),
  reverses_transaction_id uuid REFERENCES transactions (id),
  failure_code            text CHECK (failure_code IS NULL OR length(failure_code) <= 32),
  failure_reason          text CHECK (failure_reason IS NULL OR length(failure_reason) <= 500),
  reason                  text CHECK (reason IS NULL OR length(btrim(reason)) BETWEEN 3 AND 500),
  created_by              text,
  raw_payload             jsonb,
  occurred_at             timestamptz,
  created_at              timestamptz NOT NULL DEFAULT now(),
  updated_at              timestamptz NOT NULL DEFAULT now(),
  completed_at            timestamptz,
  CONSTRAINT transactions_provider_receipt_uq UNIQUE (provider, provider_transaction_id),
  CONSTRAINT transactions_provider_request_uq UNIQUE (channel, provider_request_id),
  UNIQUE (app_id, id),
  FOREIGN KEY (app_id, charge_id) REFERENCES charges (app_id, id),
  -- With MATCH SIMPLE a NULL app_id would skip the composite FK check.
  CHECK (charge_id IS NULL OR app_id IS NOT NULL),
  CHECK ((channel = 'manual') = (provider = 'manual')),
  CHECK (
       (kind = 'payment'  AND direction = 'in')
    OR (kind = 'refund'   AND direction = 'out')
    OR (kind = 'reversal' AND direction = 'out')
    OR  kind = 'adjustment'
  ),
  CHECK (kind = 'payment' OR (reason IS NOT NULL AND created_by IS NOT NULL)),
  CHECK ((kind = 'reversal') = (reverses_transaction_id IS NOT NULL)),
  -- A successful M-Pesa payment always carries its receipt.
  CHECK (status <> 'succeeded' OR provider = 'manual' OR kind <> 'payment' OR provider_transaction_id IS NOT NULL),
  CHECK ((status IN ('succeeded', 'failed', 'cancelled')) = (completed_at IS NOT NULL))
);
COMMENT ON TABLE transactions IS 'Append-only ledger of money movements. Amounts are never changed and rows are never deleted; corrections are new reversal rows.';
CREATE UNIQUE INDEX transactions_one_reversal ON transactions (reverses_transaction_id) WHERE kind = 'reversal';
CREATE INDEX transactions_app_created ON transactions (app_id, created_at DESC);
CREATE INDEX transactions_charge ON transactions (charge_id) WHERE charge_id IS NOT NULL;
CREATE INDEX transactions_pending ON transactions (channel, created_at) WHERE status = 'pending';
CREATE INDEX transactions_phone ON transactions (phone) WHERE phone IS NOT NULL;

-- The charge FK to its paying transaction (declared here, after both tables).
ALTER TABLE charges ADD CONSTRAINT charges_transaction_fk
  FOREIGN KEY (app_id, transaction_id) REFERENCES transactions (app_id, id);
CREATE FUNCTION tsp_charges_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  t transactions%ROWTYPE;
BEGIN
  IF NEW.id <> OLD.id OR NEW.app_id <> OLD.app_id OR NEW.reference <> OLD.reference
     OR NEW.description IS DISTINCT FROM OLD.description OR NEW.currency <> OLD.currency
     OR NEW.amount <> OLD.amount OR NEW.metadata IS DISTINCT FROM OLD.metadata
     OR NEW.account_number <> OLD.account_number OR NEW.account_number_norm <> OLD.account_number_norm
     OR NEW.account_generated <> OLD.account_generated OR NEW.phone IS DISTINCT FROM OLD.phone
     OR NEW.hosted_token <> OLD.hosted_token OR NEW.idempotency_key <> OLD.idempotency_key
     OR NEW.request_hash <> OLD.request_hash OR NEW.expires_at <> OLD.expires_at
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'charge % : identity, amount, reference and account number are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF (OLD.transaction_id IS NOT NULL AND NEW.transaction_id IS DISTINCT FROM OLD.transaction_id)
     OR (OLD.succeeded_at IS NOT NULL AND NEW.succeeded_at IS DISTINCT FROM OLD.succeeded_at) THEN
    RAISE EXCEPTION 'charge % : the paying transaction is set once', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  -- pending      -> needs_review | succeeded | expired | cancelled
  -- needs_review -> pending (staff: not paid) | succeeded  (never expires
  --                 while staff are checking)
  -- expired, cancelled -> succeeded (late money is never dropped)
  -- succeeded    -> reversed
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'pending'      AND NEW.status IN ('needs_review', 'succeeded', 'expired', 'cancelled'))
    OR (OLD.status = 'needs_review' AND NEW.status IN ('pending', 'succeeded'))
    OR (OLD.status IN ('expired', 'cancelled') AND NEW.status = 'succeeded')
    OR (OLD.status = 'succeeded'    AND NEW.status = 'reversed')
  ) THEN
    RAISE EXCEPTION 'charge % : status % -> % is not allowed', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  IF NEW.status = 'succeeded' AND OLD.status <> 'succeeded' THEN
    SELECT * INTO t FROM transactions WHERE id = NEW.transaction_id;
    IF NOT FOUND OR t.status <> 'succeeded' OR t.kind <> 'payment' OR t.direction <> 'in'
       OR t.app_id IS DISTINCT FROM NEW.app_id OR t.charge_id IS DISTINCT FROM NEW.id
       OR t.amount <> NEW.amount
       OR (t.provider = 'mpesa' AND t.provider_transaction_id IS NULL) THEN
      RAISE EXCEPTION 'charge % : can only be paid by a succeeded, receipted payment of this app and charge for exactly KES %', OLD.id, OLD.amount
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  IF NEW.status = 'reversed' AND NOT EXISTS (
       SELECT 1 FROM transactions r WHERE r.kind = 'reversal' AND r.reverses_transaction_id = NEW.transaction_id) THEN
    RAISE EXCEPTION 'charge % : reversed only after its payment has a reversal transaction', OLD.id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER charges_guard BEFORE UPDATE ON charges FOR EACH ROW EXECUTE FUNCTION tsp_charges_guard();

CREATE FUNCTION tsp_transactions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- Immutable columns.
  IF NEW.id <> OLD.id OR NEW.direction <> OLD.direction OR NEW.kind <> OLD.kind
     OR NEW.channel <> OLD.channel OR NEW.provider <> OLD.provider
     OR NEW.amount <> OLD.amount OR NEW.currency <> OLD.currency
     OR NEW.reverses_transaction_id IS DISTINCT FROM OLD.reverses_transaction_id
     OR NEW.reason IS DISTINCT FROM OLD.reason OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'transaction % : amount, direction, kind, channel and authorship are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  -- Set-once columns: may go from NULL to a value, never change afterwards.
  -- (app_id: staff assigning unmatched money to an app.)
  IF (OLD.provider_transaction_id IS NOT NULL AND NEW.provider_transaction_id IS DISTINCT FROM OLD.provider_transaction_id)
     OR (OLD.provider_request_id IS NOT NULL AND NEW.provider_request_id IS DISTINCT FROM OLD.provider_request_id)
     OR (OLD.phone IS NOT NULL AND NEW.phone IS DISTINCT FROM OLD.phone)
     OR (OLD.app_id IS NOT NULL AND NEW.app_id IS DISTINCT FROM OLD.app_id)
     OR (OLD.charge_id IS NOT NULL AND NEW.charge_id IS DISTINCT FROM OLD.charge_id)
     OR (OLD.raw_payload IS NOT NULL AND NEW.raw_payload IS DISTINCT FROM OLD.raw_payload)
     OR (OLD.occurred_at IS NOT NULL AND NEW.occurred_at IS DISTINCT FROM OLD.occurred_at)
     OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at)
     OR (OLD.failure_code IS NOT NULL AND NEW.failure_code IS DISTINCT FROM OLD.failure_code)
     OR (OLD.failure_reason IS NOT NULL AND NEW.failure_reason IS DISTINCT FROM OLD.failure_reason)
     OR (OLD.account_number IS NOT NULL AND NEW.account_number IS DISTINCT FROM OLD.account_number) THEN
    RAISE EXCEPTION 'transaction % : recorded provider details cannot be changed', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  -- Status lifecycle.
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'pending'      AND NEW.status IN ('succeeded', 'failed', 'cancelled', 'needs_review'))
    OR (OLD.status = 'needs_review' AND NEW.status IN ('succeeded', 'failed', 'cancelled'))
  ) THEN
    RAISE EXCEPTION 'transaction % : status % -> % is not allowed', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER transactions_guard BEFORE UPDATE ON transactions FOR EACH ROW EXECUTE FUNCTION tsp_transactions_guard();
CREATE TRIGGER transactions_updated_at BEFORE UPDATE ON transactions FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER transactions_no_delete BEFORE DELETE ON transactions FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER transactions_no_truncate BEFORE TRUNCATE ON transactions FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- A reversal undoes exactly one succeeded payment of the same app, for its
-- full amount.
CREATE FUNCTION tsp_transactions_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  o transactions%ROWTYPE;
BEGIN
  IF NEW.kind = 'reversal' THEN
    SELECT * INTO o FROM transactions WHERE id = NEW.reverses_transaction_id;
    IF NOT FOUND OR o.kind <> 'payment' OR o.status <> 'succeeded' OR o.amount <> NEW.amount
       OR o.app_id IS DISTINCT FROM NEW.app_id OR o.charge_id IS DISTINCT FROM NEW.charge_id THEN
      RAISE EXCEPTION 'a reversal must undo a succeeded payment of the same app and charge for its full amount'
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER transactions_insert BEFORE INSERT ON transactions FOR EACH ROW EXECUTE FUNCTION tsp_transactions_insert();

-- Every status a transaction passes through, with who/what caused it.
CREATE TABLE transaction_events (
  id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  transaction_id uuid NOT NULL REFERENCES transactions (id),
  from_status    txn_status,
  to_status      txn_status NOT NULL,
  actor          text NOT NULL,
  occurred_at    timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX transaction_events_txn ON transaction_events (transaction_id, id);

CREATE FUNCTION tsp_transactions_event() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'INSERT' OR NEW.status <> OLD.status THEN
    INSERT INTO transaction_events (transaction_id, from_status, to_status, actor)
    VALUES (NEW.id, CASE WHEN TG_OP = 'UPDATE' THEN OLD.status END, NEW.status,
            coalesce(nullif(current_setting('talksasa.actor', true), ''), 'unknown'));
  END IF;
  RETURN NULL;
END $$;
CREATE TRIGGER transactions_event AFTER INSERT OR UPDATE OF status ON transactions FOR EACH ROW EXECUTE FUNCTION tsp_transactions_event();
CREATE TRIGGER transaction_events_no_update BEFORE UPDATE ON transaction_events FOR EACH ROW EXECUTE FUNCTION tsp_forbid_update();
CREATE TRIGGER transaction_events_no_delete BEFORE DELETE ON transaction_events FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER transaction_events_no_truncate BEFORE TRUNCATE ON transaction_events FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- Audit log (append-only, hash-chained)
-- ---------------------------------------------------------------------------
-- Each row stores the SHA-256 of (previous row hash || this row's content)
-- and points at the previous row, forming a single chain. Editing or deleting
-- any row (e.g. directly as a superuser) breaks the chain, which
-- tsp_audit_verify() detects.
CREATE TABLE audit_log (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  occurred_at timestamptz NOT NULL DEFAULT clock_timestamp(),
  actor_type  audit_actor_type NOT NULL,
  actor_id    text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 200),
  action      text NOT NULL CHECK (action ~ '^[a-z_]+(\.[a-z_]+)+$'),
  entity_type text NOT NULL,
  entity_id   text,
  app_id      uuid REFERENCES apps (id),
  details     jsonb NOT NULL DEFAULT '{}'::jsonb,
  prev_id     bigint UNIQUE REFERENCES audit_log (id),
  prev_hash   bytea,
  hash        bytea NOT NULL
);
CREATE INDEX audit_log_entity ON audit_log (entity_type, entity_id);
CREATE INDEX audit_log_app ON audit_log (app_id, occurred_at DESC);

CREATE FUNCTION tsp_audit_row_digest(a audit_log, prev bytea) RETURNS bytea LANGUAGE sql STABLE AS $$
  SELECT sha256(coalesce(prev, ''::bytea) || convert_to(concat_ws('|',
    a.id::text, to_char(a.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'),
    a.actor_type::text, a.actor_id, a.action, a.entity_type, coalesce(a.entity_id, ''),
    coalesce(a.app_id::text, ''), a.details::text), 'UTF8'))
$$;

-- The id is drawn while holding the lock, so id order equals chain order and
-- the highest id is always the head (concurrent writers otherwise commit in
-- the opposite order to their ids and collide on prev_id).
CREATE FUNCTION tsp_audit_chain() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  last audit_log%ROWTYPE;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtext('talksasa.audit_log'));
  NEW.id := nextval(pg_get_serial_sequence('audit_log', 'id'));
  SELECT * INTO last FROM audit_log ORDER BY id DESC LIMIT 1;
  IF FOUND THEN
    NEW.prev_id := last.id;
    NEW.prev_hash := last.hash;
  ELSE
    NEW.prev_id := NULL;
    NEW.prev_hash := NULL;
  END IF;
  NEW.hash := tsp_audit_row_digest(NEW, NEW.prev_hash);
  RETURN NEW;
END $$;
CREATE TRIGGER audit_log_chain BEFORE INSERT ON audit_log FOR EACH ROW EXECUTE FUNCTION tsp_audit_chain();
CREATE TRIGGER audit_log_no_update BEFORE UPDATE ON audit_log FOR EACH ROW EXECUTE FUNCTION tsp_forbid_update();
CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER audit_log_no_truncate BEFORE TRUNCATE ON audit_log FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- Returns the id of the first row whose hash or link is wrong, or NULL if the
-- whole chain verifies.
CREATE FUNCTION tsp_audit_verify() RETURNS bigint LANGUAGE plpgsql STABLE AS $$
DECLARE
  a audit_log%ROWTYPE;
  expected_prev bigint := NULL;
  expected_prev_hash bytea := NULL;
  n bigint := 0;
BEGIN
  FOR a IN
    WITH RECURSIVE chain AS (
      SELECT al.*, 1 AS depth FROM audit_log al WHERE al.prev_id IS NULL
      UNION ALL
      SELECT al.*, c.depth + 1 FROM audit_log al JOIN chain c ON al.prev_id = c.id
    )
    SELECT id, occurred_at, actor_type, actor_id, action, entity_type, entity_id, app_id, details, prev_id, prev_hash, hash
      FROM chain ORDER BY depth
  LOOP
    n := n + 1;
    IF a.prev_id IS DISTINCT FROM expected_prev OR a.prev_hash IS DISTINCT FROM expected_prev_hash
       OR a.hash <> tsp_audit_row_digest(a, a.prev_hash) THEN
      RETURN a.id;
    END IF;
    expected_prev := a.id;
    expected_prev_hash := a.hash;
  END LOOP;
  IF n <> (SELECT count(*) FROM audit_log) THEN
    RETURN (SELECT min(id) FROM audit_log al WHERE al.id NOT IN (
      WITH RECURSIVE chain AS (
        SELECT id FROM audit_log WHERE prev_id IS NULL
        UNION ALL
        SELECT al2.id FROM audit_log al2 JOIN chain c ON al2.prev_id = c.id)
      SELECT id FROM chain));
  END IF;
  RETURN NULL;
END $$;

-- ---------------------------------------------------------------------------
-- Privileges for the application role
-- ---------------------------------------------------------------------------
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    GRANT USAGE ON SCHEMA public TO talksasa_app;
    GRANT SELECT, INSERT, UPDATE ON apps, app_api_keys, charges, transactions TO talksasa_app;
    GRANT SELECT, INSERT ON transaction_events, audit_log TO talksasa_app;
    GRANT EXECUTE ON FUNCTION tsp_audit_verify() TO talksasa_app;
    -- The audit trigger runs as the inserting role and calls nextval itself.
    EXECUTE format('GRANT USAGE ON SEQUENCE %s TO talksasa_app', pg_get_serial_sequence('audit_log', 'id'));
  ELSE
    RAISE WARNING 'role talksasa_app does not exist; skipping grants (create it, then re-run grants from this migration)';
  END IF;
END $$;
