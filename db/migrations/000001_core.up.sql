-- Talksasa Pay core schema: products, payers, payment requests, the
-- append-only transaction ledger, allocations and the audit log.
--
-- Money is always bigint whole KES. Integrity rules live in the database as
-- well as in Go, so a bug in application code cannot silently corrupt the
-- ledger:
--   * transactions, allocation batches, allocations and audit rows are never
--     deleted; money columns are never updated (triggers below);
--   * payment_requests.amount_paid and payer_accounts.credit_balance are
--     caches maintained ONLY by the allocations trigger;
--   * every allocation batch must sum to its expected total (deferred check);
--   * composite foreign keys (product_id, id) make cross-product references
--     impossible: a row can only point at rows of the same product.
--
-- The application connects as role talksasa_app, which has no DELETE or
-- TRUNCATE privileges. Migrations run as the schema owner.

SET lock_timeout = '10s';

-- ---------------------------------------------------------------------------
-- Types
-- ---------------------------------------------------------------------------
CREATE TYPE product_status         AS ENUM ('active', 'suspended');
CREATE TYPE payer_status           AS ENUM ('active', 'closed');
CREATE TYPE payment_request_status AS ENUM ('open', 'partially_paid', 'paid', 'cancelled', 'expired');
CREATE TYPE txn_direction          AS ENUM ('in', 'out');
CREATE TYPE txn_kind               AS ENUM ('payment', 'refund', 'reversal', 'adjustment');
CREATE TYPE txn_channel            AS ENUM ('mpesa_stk', 'mpesa_c2b', 'mpesa_b2c', 'manual');
CREATE TYPE txn_status             AS ENUM ('pending', 'succeeded', 'failed', 'cancelled', 'needs_review');
CREATE TYPE allocation_batch_kind  AS ENUM ('payment', 'credit_application', 'reversal');
CREATE TYPE audit_actor_type       AS ENUM ('system', 'admin', 'cli', 'product');

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

-- True only while the allocations trigger is updating cached balances. The
-- trigger-depth test cannot be faked by a client: a direct UPDATE from the
-- application runs guard triggers at depth 1, the allocations trigger's
-- nested UPDATE runs them at depth 2.
CREATE FUNCTION tsp_allocating() RETURNS boolean LANGUAGE sql STABLE AS $$
  SELECT pg_trigger_depth() > 1
     AND coalesce(current_setting('talksasa.allocating', true), '') = 'on'
$$;

-- ---------------------------------------------------------------------------
-- Products
-- ---------------------------------------------------------------------------
CREATE TABLE products (
  id                         uuid PRIMARY KEY,
  name                       text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 100),
  account_prefix             text NOT NULL UNIQUE CHECK (account_prefix ~ '^[A-Z]{2,6}$'),
  webhook_url                text CHECK (webhook_url IS NULL OR (webhook_url ~ '^https?://' AND length(webhook_url) <= 2048)),
  webhook_secret_ciphertext  bytea NOT NULL,
  webhook_secret_key_version integer NOT NULL CHECK (webhook_secret_key_version > 0),
  status                     product_status NOT NULL DEFAULT 'active',
  created_at                 timestamptz NOT NULL DEFAULT now(),
  updated_at                 timestamptz NOT NULL DEFAULT now()
);
COMMENT ON TABLE products IS 'Talksasa''s own products. There is deliberately no payout/settlement configuration: all money collected is Talksasa revenue.';

CREATE FUNCTION tsp_products_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.account_prefix <> OLD.account_prefix OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'products.id, account_prefix and created_at are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER products_guard BEFORE UPDATE ON products FOR EACH ROW EXECUTE FUNCTION tsp_products_guard();
CREATE TRIGGER products_updated_at BEFORE UPDATE ON products FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER products_no_delete BEFORE DELETE ON products FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

-- Per-product counter for account numbers. Values only ever increase, so an
-- account number is never reused.
CREATE TABLE product_account_counters (
  product_id uuid PRIMARY KEY REFERENCES products (id),
  next_base  bigint NOT NULL CHECK (next_base >= 100)
);

CREATE FUNCTION tsp_counter_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.product_id <> OLD.product_id OR NEW.next_base <= OLD.next_base THEN
    RAISE EXCEPTION 'account counters can only move forward'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER product_account_counters_guard BEFORE UPDATE ON product_account_counters FOR EACH ROW EXECUTE FUNCTION tsp_counter_guard();
CREATE TRIGGER product_account_counters_no_delete BEFORE DELETE ON product_account_counters FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

CREATE TABLE product_api_keys (
  id           uuid PRIMARY KEY,
  product_id   uuid NOT NULL REFERENCES products (id),
  key_id       text NOT NULL UNIQUE CHECK (key_id ~ '^[a-z2-7]{16,32}$'),
  secret_hash  bytea NOT NULL CHECK (octet_length(secret_hash) = 32),
  label        text NOT NULL CHECK (length(btrim(label)) BETWEEN 1 AND 100),
  created_by   text NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz,
  revoked_at   timestamptz,
  revoked_by   text,
  last_used_at timestamptz,
  CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);
CREATE INDEX product_api_keys_product_idx ON product_api_keys (product_id);

CREATE FUNCTION tsp_api_keys_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.product_id <> OLD.product_id OR NEW.key_id <> OLD.key_id
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
CREATE TRIGGER product_api_keys_guard BEFORE UPDATE ON product_api_keys FOR EACH ROW EXECUTE FUNCTION tsp_api_keys_guard();
CREATE TRIGGER product_api_keys_no_delete BEFORE DELETE ON product_api_keys FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

-- ---------------------------------------------------------------------------
-- Payer accounts
-- ---------------------------------------------------------------------------
CREATE TABLE payer_accounts (
  id                  uuid PRIMARY KEY,
  product_id          uuid NOT NULL REFERENCES products (id),
  external_ref        text NOT NULL CHECK (length(btrim(external_ref)) BETWEEN 1 AND 128),
  account_number      text NOT NULL UNIQUE CHECK (account_number ~ '^[A-Z]{2,6}-[1-9][0-9]+$'),
  account_number_norm text NOT NULL UNIQUE,
  display_name        text NOT NULL CHECK (length(btrim(display_name)) BETWEEN 1 AND 200),
  email               text CHECK (email IS NULL OR (length(email) <= 254 AND email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$')),
  status              payer_status NOT NULL DEFAULT 'active',
  credit_balance      bigint NOT NULL DEFAULT 0 CHECK (credit_balance >= 0),
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  UNIQUE (product_id, external_ref),
  UNIQUE (product_id, id),
  CHECK (account_number_norm = replace(account_number, '-', ''))
);
COMMENT ON COLUMN payer_accounts.credit_balance IS 'Cache of the sum of credit allocations. Maintained only by the allocations trigger.';

CREATE FUNCTION tsp_payer_accounts_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  v_prefix text;
BEGIN
  SELECT account_prefix INTO v_prefix FROM products WHERE id = NEW.product_id;
  IF NEW.account_number NOT LIKE v_prefix || '-%' THEN
    RAISE EXCEPTION 'account number % does not carry product prefix %', NEW.account_number, v_prefix
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.credit_balance <> 0 THEN
    RAISE EXCEPTION 'payer accounts must start with zero credit'
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payer_accounts_insert BEFORE INSERT ON payer_accounts FOR EACH ROW EXECUTE FUNCTION tsp_payer_accounts_insert();

CREATE FUNCTION tsp_payer_accounts_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.product_id <> OLD.product_id OR NEW.external_ref <> OLD.external_ref
     OR NEW.account_number <> OLD.account_number OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'payer account identity and account number are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.credit_balance <> OLD.credit_balance AND NOT tsp_allocating() THEN
    RAISE EXCEPTION 'credit_balance can only change through allocations'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payer_accounts_guard BEFORE UPDATE ON payer_accounts FOR EACH ROW EXECUTE FUNCTION tsp_payer_accounts_guard();
CREATE TRIGGER payer_accounts_updated_at BEFORE UPDATE ON payer_accounts FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER payer_accounts_no_delete BEFORE DELETE ON payer_accounts FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

CREATE TABLE payer_phones (
  product_id       uuid NOT NULL,
  payer_account_id uuid NOT NULL,
  phone            text NOT NULL CHECK (phone ~ '^254[17][0-9]{8}$'),
  is_primary       boolean NOT NULL DEFAULT false,
  created_at       timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (payer_account_id, phone),
  FOREIGN KEY (product_id, payer_account_id) REFERENCES payer_accounts (product_id, id)
);
CREATE UNIQUE INDEX payer_phones_one_primary ON payer_phones (payer_account_id) WHERE is_primary;
CREATE INDEX payer_phones_lookup ON payer_phones (product_id, phone);
-- Phones may be removed when a customer changes number (data minimisation);
-- historical phones remain on the transactions they paid with.

-- ---------------------------------------------------------------------------
-- Payment requests
-- ---------------------------------------------------------------------------
CREATE TABLE payment_requests (
  id                uuid PRIMARY KEY,
  product_id        uuid NOT NULL REFERENCES products (id),
  payer_account_id  uuid NOT NULL,
  product_reference text NOT NULL CHECK (length(btrim(product_reference)) BETWEEN 1 AND 128),
  description       text NOT NULL CHECK (length(btrim(description)) BETWEEN 1 AND 500),
  currency          char(3) NOT NULL DEFAULT 'KES' CHECK (currency = 'KES'),
  amount            bigint NOT NULL CHECK (amount > 0 AND amount <= 1000000000000),
  amount_paid       bigint NOT NULL DEFAULT 0,
  status            payment_request_status NOT NULL DEFAULT 'open',
  expires_at        timestamptz,
  hosted_token      text NOT NULL UNIQUE CHECK (length(hosted_token) >= 40),
  idempotency_key   text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 255),
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  paid_at           timestamptz,
  closed_at         timestamptz,
  FOREIGN KEY (product_id, payer_account_id) REFERENCES payer_accounts (product_id, id),
  UNIQUE (product_id, idempotency_key),
  UNIQUE (product_id, id),
  CHECK (amount_paid >= 0 AND amount_paid <= amount),
  -- Status must agree with what has been allocated. Cancelled/expired
  -- requests can only ever have had nothing allocated to them.
  CHECK (
       (status = 'open'           AND amount_paid = 0)
    OR (status = 'partially_paid' AND amount_paid > 0 AND amount_paid < amount)
    OR (status = 'paid'           AND amount_paid = amount)
    OR (status IN ('cancelled', 'expired') AND amount_paid = 0)
  ),
  CHECK ((status = 'paid') = (paid_at IS NOT NULL)),
  CHECK ((status IN ('cancelled', 'expired')) = (closed_at IS NOT NULL))
);
COMMENT ON COLUMN payment_requests.amount_paid IS 'Cache of the sum of allocations to this request. Maintained only by the allocations trigger.';
-- A product reference can have only one live request at a time.
CREATE UNIQUE INDEX payment_requests_live_reference ON payment_requests (product_id, product_reference)
  WHERE status IN ('open', 'partially_paid');
-- Allocation reads: the payer's payable requests, oldest first.
CREATE INDEX payment_requests_payable ON payment_requests (payer_account_id, created_at, id)
  WHERE status IN ('open', 'partially_paid');
CREATE INDEX payment_requests_expiry ON payment_requests (expires_at) WHERE status = 'open' AND expires_at IS NOT NULL;
CREATE INDEX payment_requests_product_created ON payment_requests (product_id, created_at DESC);

CREATE FUNCTION tsp_payment_requests_insert() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.amount_paid <> 0 OR NEW.status <> 'open' THEN
    RAISE EXCEPTION 'payment requests are created open with nothing paid'
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payment_requests_insert BEFORE INSERT ON payment_requests FOR EACH ROW EXECUTE FUNCTION tsp_payment_requests_insert();

CREATE FUNCTION tsp_payment_requests_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.product_id <> OLD.product_id OR NEW.payer_account_id <> OLD.payer_account_id
     OR NEW.product_reference <> OLD.product_reference OR NEW.description <> OLD.description
     OR NEW.currency <> OLD.currency OR NEW.amount <> OLD.amount OR NEW.hosted_token <> OLD.hosted_token
     OR NEW.idempotency_key <> OLD.idempotency_key OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'payment request identity and amount are immutable'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;

  IF tsp_allocating() THEN
    RETURN NEW; -- the allocations trigger derives amount_paid/status itself
  END IF;

  IF NEW.amount_paid <> OLD.amount_paid THEN
    RAISE EXCEPTION 'amount_paid can only change through allocations'
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  -- Outside allocation the only status changes allowed are closing an
  -- untouched request: open -> cancelled | expired.
  IF NEW.status <> OLD.status AND NOT (OLD.status = 'open' AND NEW.status IN ('cancelled', 'expired')) THEN
    RAISE EXCEPTION 'payment request status % -> % is not allowed', OLD.status, NEW.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER payment_requests_guard BEFORE UPDATE ON payment_requests FOR EACH ROW EXECUTE FUNCTION tsp_payment_requests_guard();
CREATE TRIGGER payment_requests_updated_at BEFORE UPDATE ON payment_requests FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER payment_requests_no_delete BEFORE DELETE ON payment_requests FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

-- ---------------------------------------------------------------------------
-- Transactions ledger (append-only)
-- ---------------------------------------------------------------------------
CREATE TABLE transactions (
  id                      uuid PRIMARY KEY,
  -- NULL only while a payment cannot be routed to a product (suspense).
  product_id              uuid REFERENCES products (id),
  direction               txn_direction NOT NULL,
  kind                    txn_kind NOT NULL,
  channel                 txn_channel NOT NULL,
  -- Receipt namespace: all M-Pesa channels share one receipt space, so the
  -- same receipt can never be recorded twice even via different channels
  -- (e.g. STK callback and a later statement import).
  provider                text NOT NULL CHECK (provider IN ('mpesa', 'manual')),
  status                  txn_status NOT NULL,
  amount                  bigint NOT NULL CHECK (amount > 0 AND amount <= 1000000000000),
  currency                char(3) NOT NULL DEFAULT 'KES' CHECK (currency = 'KES'),
  provider_transaction_id text CHECK (provider_transaction_id IS NULL OR length(provider_transaction_id) BETWEEN 1 AND 64),
  provider_request_id     text CHECK (provider_request_id IS NULL OR length(provider_request_id) BETWEEN 1 AND 128),
  phone                   text CHECK (phone IS NULL OR phone ~ '^254[17][0-9]{8}$'),
  bill_ref                text CHECK (bill_ref IS NULL OR length(bill_ref) <= 64),
  payer_account_id        uuid,
  payment_request_id      uuid,
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
  UNIQUE (product_id, id),
  FOREIGN KEY (product_id, payer_account_id)   REFERENCES payer_accounts (product_id, id),
  FOREIGN KEY (product_id, payment_request_id) REFERENCES payment_requests (product_id, id),
  -- With MATCH SIMPLE a NULL product_id would skip the composite FK checks.
  CHECK (payer_account_id IS NULL OR product_id IS NOT NULL),
  CHECK (payment_request_id IS NULL OR payer_account_id IS NOT NULL),
  CHECK ((channel = 'manual') = (provider = 'manual')),
  CHECK (
       (kind = 'payment'  AND direction = 'in')
    OR (kind = 'refund'   AND direction = 'out')
    OR (kind = 'reversal' AND direction = 'out')
    OR  kind = 'adjustment'
  ),
  CHECK (kind = 'payment' OR (reason IS NOT NULL AND created_by IS NOT NULL)),
  CHECK ((kind = 'reversal') = (reverses_transaction_id IS NOT NULL)),
  -- A successful M-Pesa transaction always carries its receipt.
  CHECK (status <> 'succeeded' OR provider = 'manual' OR provider_transaction_id IS NOT NULL),
  CHECK ((status IN ('succeeded', 'failed', 'cancelled')) = (completed_at IS NOT NULL))
);
COMMENT ON TABLE transactions IS 'Append-only ledger of money movements. Amounts are never changed and rows are never deleted; corrections are new reversal/adjustment rows.';
CREATE UNIQUE INDEX transactions_one_reversal ON transactions (reverses_transaction_id) WHERE kind = 'reversal';
CREATE INDEX transactions_product_created ON transactions (product_id, created_at DESC);
CREATE INDEX transactions_payer ON transactions (payer_account_id, created_at DESC) WHERE payer_account_id IS NOT NULL;
CREATE INDEX transactions_request ON transactions (payment_request_id) WHERE payment_request_id IS NOT NULL;
CREATE INDEX transactions_pending ON transactions (channel, created_at) WHERE status = 'pending';
CREATE INDEX transactions_phone ON transactions (phone) WHERE phone IS NOT NULL;

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
  IF (OLD.provider_transaction_id IS NOT NULL AND NEW.provider_transaction_id IS DISTINCT FROM OLD.provider_transaction_id)
     OR (OLD.provider_request_id IS NOT NULL AND NEW.provider_request_id IS DISTINCT FROM OLD.provider_request_id)
     OR (OLD.phone IS NOT NULL AND NEW.phone IS DISTINCT FROM OLD.phone)
     OR (OLD.product_id IS NOT NULL AND NEW.product_id IS DISTINCT FROM OLD.product_id)
     OR (OLD.payer_account_id IS NOT NULL AND NEW.payer_account_id IS DISTINCT FROM OLD.payer_account_id)
     OR (OLD.payment_request_id IS NOT NULL AND NEW.payment_request_id IS DISTINCT FROM OLD.payment_request_id)
     OR (OLD.raw_payload IS NOT NULL AND NEW.raw_payload IS DISTINCT FROM OLD.raw_payload)
     OR (OLD.occurred_at IS NOT NULL AND NEW.occurred_at IS DISTINCT FROM OLD.occurred_at)
     OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at)
     OR (OLD.failure_code IS NOT NULL AND NEW.failure_code IS DISTINCT FROM OLD.failure_code)
     OR (OLD.failure_reason IS NOT NULL AND NEW.failure_reason IS DISTINCT FROM OLD.failure_reason)
     OR (OLD.bill_ref IS NOT NULL AND NEW.bill_ref IS DISTINCT FROM OLD.bill_ref) THEN
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
-- Allocations (append-only)
-- ---------------------------------------------------------------------------
-- A batch groups the allocation lines produced by one decision of the
-- allocation engine. Its lines must sum to expected_total:
--   payment:            +transaction amount (money in, spread over requests/credit)
--   credit_application:  0 (+x to a request, -x from credit)
--   reversal:           -original amount (undo a payment batch)
CREATE TABLE allocation_batches (
  id                 uuid PRIMARY KEY,
  product_id         uuid NOT NULL REFERENCES products (id),
  payer_account_id   uuid NOT NULL,
  kind               allocation_batch_kind NOT NULL,
  transaction_id     uuid,
  payment_request_id uuid,
  reverses_batch_id  uuid REFERENCES allocation_batches (id),
  expected_total     bigint NOT NULL,
  created_by         text NOT NULL,
  reason             text,
  created_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (product_id, id),
  FOREIGN KEY (product_id, payer_account_id)   REFERENCES payer_accounts (product_id, id),
  FOREIGN KEY (product_id, transaction_id)     REFERENCES transactions (product_id, id),
  FOREIGN KEY (product_id, payment_request_id) REFERENCES payment_requests (product_id, id),
  CHECK (
       (kind = 'payment'            AND transaction_id IS NOT NULL AND reverses_batch_id IS NULL AND payment_request_id IS NULL AND expected_total > 0)
    OR (kind = 'credit_application' AND transaction_id IS NULL     AND reverses_batch_id IS NULL AND payment_request_id IS NOT NULL AND expected_total = 0)
    OR (kind = 'reversal'           AND transaction_id IS NOT NULL AND reverses_batch_id IS NOT NULL AND payment_request_id IS NULL AND expected_total < 0 AND reason IS NOT NULL)
  )
);
-- Idempotency guard: a transaction is allocated at most once, and a payment
-- batch is reversed at most once.
CREATE UNIQUE INDEX allocation_batches_one_payment ON allocation_batches (transaction_id) WHERE kind = 'payment';
CREATE UNIQUE INDEX allocation_batches_one_reversal ON allocation_batches (reverses_batch_id) WHERE kind = 'reversal';
CREATE INDEX allocation_batches_payer ON allocation_batches (payer_account_id, created_at);

CREATE FUNCTION tsp_allocation_batches_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  t transactions%ROWTYPE;
  orig allocation_batches%ROWTYPE;
BEGIN
  IF NEW.kind IN ('payment', 'reversal') THEN
    SELECT * INTO t FROM transactions WHERE id = NEW.transaction_id;
    IF t.status <> 'succeeded' THEN
      RAISE EXCEPTION 'only succeeded transactions can be allocated (transaction % is %)', t.id, t.status
        USING ERRCODE = 'check_violation';
    END IF;
    IF t.payer_account_id IS DISTINCT FROM NEW.payer_account_id THEN
      RAISE EXCEPTION 'allocation payer does not match transaction payer'
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;

  IF NEW.kind = 'payment' THEN
    IF t.kind <> 'payment' OR t.direction <> 'in' OR t.amount <> NEW.expected_total THEN
      RAISE EXCEPTION 'payment batch must allocate exactly the incoming payment amount'
        USING ERRCODE = 'check_violation';
    END IF;
  ELSIF NEW.kind = 'reversal' THEN
    SELECT * INTO orig FROM allocation_batches WHERE id = NEW.reverses_batch_id;
    IF orig.kind <> 'payment' OR t.kind <> 'reversal' OR t.reverses_transaction_id <> orig.transaction_id
       OR NEW.expected_total <> -orig.expected_total OR orig.payer_account_id <> NEW.payer_account_id THEN
      RAISE EXCEPTION 'reversal batch must exactly undo the original payment batch'
        USING ERRCODE = 'check_violation';
    END IF;
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER allocation_batches_insert BEFORE INSERT ON allocation_batches FOR EACH ROW EXECUTE FUNCTION tsp_allocation_batches_insert();
CREATE TRIGGER allocation_batches_no_update BEFORE UPDATE ON allocation_batches FOR EACH ROW EXECUTE FUNCTION tsp_forbid_update();
CREATE TRIGGER allocation_batches_no_delete BEFORE DELETE ON allocation_batches FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER allocation_batches_no_truncate BEFORE TRUNCATE ON allocation_batches FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

CREATE TABLE allocations (
  id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  batch_id           uuid NOT NULL,
  line_no            integer NOT NULL CHECK (line_no >= 1),
  product_id         uuid NOT NULL,
  payer_account_id   uuid NOT NULL,
  payment_request_id uuid, -- NULL = the payer's credit balance
  amount             bigint NOT NULL CHECK (amount <> 0),
  created_at         timestamptz NOT NULL DEFAULT now(),
  UNIQUE (batch_id, line_no),
  FOREIGN KEY (product_id, batch_id)           REFERENCES allocation_batches (product_id, id),
  FOREIGN KEY (product_id, payer_account_id)   REFERENCES payer_accounts (product_id, id),
  FOREIGN KEY (product_id, payment_request_id) REFERENCES payment_requests (product_id, id)
);
CREATE INDEX allocations_request ON allocations (payment_request_id) WHERE payment_request_id IS NOT NULL;
CREATE INDEX allocations_payer_credit ON allocations (payer_account_id) WHERE payment_request_id IS NULL;

-- Applies one allocation line to the cached balances. Over-allocation and
-- negative credit are rejected by the CHECK constraints on the target rows.
CREATE FUNCTION tsp_allocations_apply() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  b allocation_batches%ROWTYPE;
  r payment_requests%ROWTYPE;
  v_paid bigint;
  v_status payment_request_status;
BEGIN
  SELECT * INTO b FROM allocation_batches WHERE id = NEW.batch_id;
  IF b.payer_account_id <> NEW.payer_account_id THEN
    RAISE EXCEPTION 'allocation line payer does not match its batch'
      USING ERRCODE = 'check_violation';
  END IF;

  PERFORM set_config('talksasa.allocating', 'on', true);

  IF NEW.payment_request_id IS NULL THEN
    UPDATE payer_accounts SET credit_balance = credit_balance + NEW.amount WHERE id = NEW.payer_account_id;
  ELSE
    SELECT * INTO r FROM payment_requests WHERE id = NEW.payment_request_id FOR NO KEY UPDATE;
    IF r.payer_account_id <> NEW.payer_account_id THEN
      RAISE EXCEPTION 'payment request % does not belong to payer %', r.id, NEW.payer_account_id
        USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.amount > 0 AND r.status NOT IN ('open', 'partially_paid') THEN
      RAISE EXCEPTION 'cannot allocate to % payment request %', r.status, r.id
        USING ERRCODE = 'check_violation';
    END IF;
    v_paid := r.amount_paid + NEW.amount;
    v_status := CASE
      WHEN v_paid <= 0 THEN 'open'
      WHEN v_paid < r.amount THEN 'partially_paid'
      ELSE 'paid' END;
    UPDATE payment_requests
       SET amount_paid = v_paid,
           status      = v_status,
           paid_at     = CASE WHEN v_status = 'paid' THEN coalesce(paid_at, now()) ELSE NULL END
     WHERE id = r.id;
  END IF;

  PERFORM set_config('talksasa.allocating', 'off', true);
  RETURN NEW;
END $$;
CREATE TRIGGER allocations_apply BEFORE INSERT ON allocations FOR EACH ROW EXECUTE FUNCTION tsp_allocations_apply();
CREATE TRIGGER allocations_no_update BEFORE UPDATE ON allocations FOR EACH ROW EXECUTE FUNCTION tsp_forbid_update();
CREATE TRIGGER allocations_no_delete BEFORE DELETE ON allocations FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER allocations_no_truncate BEFORE TRUNCATE ON allocations FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- At commit, every batch touched in the transaction must balance. Firing on
-- both tables catches empty batches and lines added to an old batch.
CREATE FUNCTION tsp_check_batch_balanced(p_batch uuid) RETURNS void LANGUAGE plpgsql AS $$
DECLARE
  v_expected bigint;
  v_sum bigint;
BEGIN
  SELECT expected_total INTO v_expected FROM allocation_batches WHERE id = p_batch;
  SELECT coalesce(sum(amount), 0) INTO v_sum FROM allocations WHERE batch_id = p_batch;
  IF v_sum <> v_expected THEN
    RAISE EXCEPTION 'allocation batch % is unbalanced: lines sum to %, expected %', p_batch, v_sum, v_expected
      USING ERRCODE = 'check_violation';
  END IF;
END $$;

CREATE FUNCTION tsp_allocation_batches_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM tsp_check_batch_balanced(NEW.id);
  RETURN NULL;
END $$;

CREATE FUNCTION tsp_allocations_balanced() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  PERFORM tsp_check_batch_balanced(NEW.batch_id);
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER allocation_batches_balanced AFTER INSERT ON allocation_batches
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION tsp_allocation_batches_balanced();
CREATE CONSTRAINT TRIGGER allocations_balanced AFTER INSERT ON allocations
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION tsp_allocations_balanced();

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
  product_id  uuid REFERENCES products (id),
  details     jsonb NOT NULL DEFAULT '{}'::jsonb,
  prev_id     bigint UNIQUE REFERENCES audit_log (id),
  prev_hash   bytea,
  hash        bytea NOT NULL
);
CREATE INDEX audit_log_entity ON audit_log (entity_type, entity_id);
CREATE INDEX audit_log_product ON audit_log (product_id, occurred_at DESC);

CREATE FUNCTION tsp_audit_row_digest(a audit_log, prev bytea) RETURNS bytea LANGUAGE sql STABLE AS $$
  SELECT sha256(coalesce(prev, ''::bytea) || convert_to(concat_ws('|',
    a.id::text, to_char(a.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US'),
    a.actor_type::text, a.actor_id, a.action, a.entity_type, coalesce(a.entity_id, ''),
    coalesce(a.product_id::text, ''), a.details::text), 'UTF8'))
$$;

CREATE FUNCTION tsp_audit_chain() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
  last audit_log%ROWTYPE;
BEGIN
  -- Serialise appends so the chain stays linear. Under READ COMMITTED the
  -- SELECT below sees the row committed by whoever held the lock before us.
  PERFORM pg_advisory_xact_lock(hashtext('talksasa.audit_log'));
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
  -- Walk the chain from its head following prev_id links.
  FOR a IN
    WITH RECURSIVE chain AS (
      SELECT al.*, 1 AS depth FROM audit_log al WHERE al.prev_id IS NULL
      UNION ALL
      SELECT al.*, c.depth + 1 FROM audit_log al JOIN chain c ON al.prev_id = c.id
    )
    SELECT id, occurred_at, actor_type, actor_id, action, entity_type, entity_id, product_id, details, prev_id, prev_hash, hash
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
  -- Rows not reachable from the head (chain was cut) are also a failure.
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
    GRANT SELECT, INSERT, UPDATE ON products, product_account_counters, product_api_keys,
                                    payer_accounts, payment_requests, transactions TO talksasa_app;
    GRANT SELECT, INSERT, UPDATE, DELETE ON payer_phones TO talksasa_app;
    GRANT SELECT, INSERT ON transaction_events, allocation_batches, allocations, audit_log TO talksasa_app;
    GRANT EXECUTE ON FUNCTION tsp_audit_verify() TO talksasa_app;
  ELSE
    RAISE WARNING 'role talksasa_app does not exist; skipping grants (create it, then re-run grants from this migration)';
  END IF;
END $$;
