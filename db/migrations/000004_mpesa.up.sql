-- M-Pesa via Daraja.
--
--   * stk_pushes: one row per STK push (Lipa na M-Pesa Online prompt) we ask
--     Safaricom to send for a charge. It owns the pending mpesa_stk
--     transaction that the callback (or staff, after a status check) later
--     completes. At most one push per charge is live at a time, so a
--     customer's phone cannot be spammed.
--   * mpesa_callbacks: inbox. Every callback Safaricom sends is stored here
--     verbatim, together with its processing job, BEFORE we answer
--     Safaricom. Processing failures can be retried; nothing sent is lost.
--     Rows are never deleted; only the processing outcome columns change.
--
-- Money rules: a charge is paid only on Safaricom success with an M-Pesa
-- receipt. A status check (STK Query) that says "success" carries no
-- receipt, so the transaction goes to needs_review and staff enter the
-- receipt. A prompt always asks for the charge's exact amount.

SET lock_timeout = '10s';

CREATE TYPE stk_push_status AS ENUM (
  'initiating', -- row written, Daraja not (yet) known to have accepted it
  'sent',       -- Daraja accepted it; waiting for the customer / callback
  'succeeded',  -- callback with a receipt; transaction succeeded
  'failed',     -- Daraja rejected it, the customer cancelled, timed out, ...
  'unknown'     -- status check said success but no receipt: staff review
);

CREATE TABLE stk_pushes (
  id                  uuid PRIMARY KEY,
  app_id              uuid NOT NULL REFERENCES apps (id),
  charge_id           uuid NOT NULL,
  transaction_id      uuid NOT NULL UNIQUE REFERENCES transactions (id),
  -- 'app' (via /v1) or 'pay_page' (hosted checkout page).
  source              text NOT NULL CHECK (source IN ('app', 'pay_page')),
  phone               text NOT NULL CHECK (phone ~ '^254[17][0-9]{8}$'),
  amount              bigint NOT NULL CHECK (amount > 0 AND amount <= 250000),
  -- /v1 pushes are idempotent on (app_id, idempotency_key). Pay-page
  -- pushes have no key.
  idempotency_key     text CHECK (idempotency_key IS NULL OR length(idempotency_key) BETWEEN 1 AND 255),
  request_hash        bytea,
  merchant_request_id text CHECK (merchant_request_id IS NULL OR length(merchant_request_id) BETWEEN 1 AND 128),
  checkout_request_id text UNIQUE CHECK (checkout_request_id IS NULL OR length(checkout_request_id) BETWEEN 1 AND 128),
  status              stk_push_status NOT NULL DEFAULT 'initiating',
  result_code         text CHECK (result_code IS NULL OR length(result_code) <= 32),
  result_desc         text CHECK (result_desc IS NULL OR length(result_desc) <= 500),
  query_attempts      integer NOT NULL DEFAULT 0 CHECK (query_attempts >= 0),
  next_query_at       timestamptz,
  created_at          timestamptz NOT NULL DEFAULT now(),
  updated_at          timestamptz NOT NULL DEFAULT now(),
  completed_at        timestamptz,
  -- Set once when the payment was recorded on a different transaction than
  -- transaction_id (late success after the push was failed).
  paid_transaction_id uuid REFERENCES transactions (id),
  -- Why staff may NOT confirm this push's transaction with a receipt, set
  -- when a callback reported an anomaly. Set once.
  confirm_blocked     text CHECK (confirm_blocked IS NULL OR confirm_blocked IN ('amount_mismatch', 'receipt_taken')),
  UNIQUE (app_id, idempotency_key),
  UNIQUE (app_id, id),
  FOREIGN KEY (app_id, charge_id) REFERENCES charges (app_id, id),
  CHECK ((idempotency_key IS NULL) = (request_hash IS NULL)),
  CHECK ((source = 'app') = (idempotency_key IS NOT NULL)),
  CHECK (status = 'initiating' OR status = 'failed' OR checkout_request_id IS NOT NULL),
  CHECK ((status IN ('succeeded', 'failed', 'unknown')) = (completed_at IS NOT NULL))
);
COMMENT ON TABLE stk_pushes IS 'STK push prompts sent through Daraja. Owns the pending mpesa_stk transaction the callback completes.';
-- One live push per charge.
CREATE UNIQUE INDEX stk_pushes_one_live ON stk_pushes (charge_id) WHERE status IN ('initiating', 'sent');
-- Status-check sweeper: live pushes that are due.
CREATE INDEX stk_pushes_due ON stk_pushes (next_query_at) WHERE status IN ('initiating', 'sent');
CREATE INDEX stk_pushes_charge ON stk_pushes (charge_id, created_at DESC);
CREATE INDEX stk_pushes_app_created ON stk_pushes (app_id, created_at DESC);

CREATE FUNCTION tsp_stk_pushes_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.app_id <> OLD.app_id OR NEW.charge_id <> OLD.charge_id
     OR NEW.transaction_id <> OLD.transaction_id OR NEW.source <> OLD.source OR NEW.phone <> OLD.phone
     OR NEW.amount <> OLD.amount OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
     OR NEW.request_hash IS DISTINCT FROM OLD.request_hash OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'stk push % : identity, phone and amount are immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF (OLD.merchant_request_id IS NOT NULL AND NEW.merchant_request_id IS DISTINCT FROM OLD.merchant_request_id)
     OR (OLD.checkout_request_id IS NOT NULL AND NEW.checkout_request_id IS DISTINCT FROM OLD.checkout_request_id)
     OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at) THEN
    RAISE EXCEPTION 'stk push % : Safaricom ids cannot be changed once set', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF NEW.query_attempts < OLD.query_attempts THEN
    RAISE EXCEPTION 'stk push % : query counter cannot go backwards', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  -- initiating -> sent | failed; sent -> succeeded | failed | unknown;
  -- unknown -> succeeded | failed (a late callback or staff resolution);
  -- failed -> succeeded only when a late callback brings a receipt for a
  -- push we had given up on (the money really arrived).
  IF NEW.status <> OLD.status AND NOT (
       (OLD.status = 'initiating' AND NEW.status IN ('sent', 'failed'))
    OR (OLD.status = 'sent'       AND NEW.status IN ('succeeded', 'failed', 'unknown'))
    OR (OLD.status = 'unknown'    AND NEW.status IN ('succeeded', 'failed'))
    OR (OLD.status = 'failed'     AND NEW.status = 'succeeded')
  ) THEN
    RAISE EXCEPTION 'stk push % : status % -> % is not allowed', OLD.id, OLD.status, NEW.status
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.status = 'failed' AND NEW.status = 'succeeded' THEN
    NEW.completed_at := OLD.completed_at; -- keep the first completion time
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER stk_pushes_guard BEFORE UPDATE ON stk_pushes FOR EACH ROW EXECUTE FUNCTION tsp_stk_pushes_guard();
CREATE TRIGGER stk_pushes_updated_at BEFORE UPDATE ON stk_pushes FOR EACH ROW EXECUTE FUNCTION tsp_set_updated_at();
CREATE TRIGGER stk_pushes_no_delete BEFORE DELETE ON stk_pushes FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();

-- ---------------------------------------------------------------------------
-- Callback inbox
-- ---------------------------------------------------------------------------
CREATE TYPE mpesa_callback_kind AS ENUM ('stk', 'c2b_confirmation');

CREATE TABLE mpesa_callbacks (
  id            uuid PRIMARY KEY,
  kind          mpesa_callback_kind NOT NULL,
  remote_ip     inet,
  -- Safaricom's body exactly as received (it never contains our secrets).
  raw           jsonb NOT NULL CHECK (jsonb_typeof(raw) = 'object' AND octet_length(raw::text) <= 16384),
  received_at   timestamptz NOT NULL DEFAULT now(),
  processed_at  timestamptz,
  -- Outcome of the last processing attempt (e.g. "duplicate receipt",
  -- "amount mismatch"); NULL on success.
  error         text CHECK (error IS NULL OR length(error) <= 500),
  attempts      integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  transaction_id uuid REFERENCES transactions (id)
);
COMMENT ON TABLE mpesa_callbacks IS 'Inbox of every Daraja callback, stored before we acknowledge it. Never deleted.';
CREATE INDEX mpesa_callbacks_unprocessed ON mpesa_callbacks (received_at) WHERE processed_at IS NULL;
CREATE INDEX mpesa_callbacks_received ON mpesa_callbacks (received_at DESC);

CREATE FUNCTION tsp_mpesa_callbacks_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.kind <> OLD.kind OR NEW.remote_ip IS DISTINCT FROM OLD.remote_ip
     OR NEW.raw <> OLD.raw OR NEW.received_at <> OLD.received_at THEN
    RAISE EXCEPTION 'mpesa callback % : what Safaricom sent is immutable', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.processed_at IS NOT NULL AND NEW.processed_at IS DISTINCT FROM OLD.processed_at THEN
    RAISE EXCEPTION 'mpesa callback % : already processed', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  IF OLD.transaction_id IS NOT NULL AND NEW.transaction_id IS DISTINCT FROM OLD.transaction_id THEN
    RAISE EXCEPTION 'mpesa callback % : transaction link is set once', OLD.id
      USING ERRCODE = 'integrity_constraint_violation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER mpesa_callbacks_guard BEFORE UPDATE ON mpesa_callbacks FOR EACH ROW EXECUTE FUNCTION tsp_mpesa_callbacks_guard();
CREATE TRIGGER mpesa_callbacks_no_delete BEFORE DELETE ON mpesa_callbacks FOR EACH ROW EXECUTE FUNCTION tsp_forbid_delete();
CREATE TRIGGER mpesa_callbacks_no_truncate BEFORE TRUNCATE ON mpesa_callbacks FOR EACH STATEMENT EXECUTE FUNCTION tsp_forbid_truncate();

-- ---------------------------------------------------------------------------
-- Privileges
-- ---------------------------------------------------------------------------
REVOKE ALL ON stk_pushes, mpesa_callbacks FROM PUBLIC;

DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'talksasa_app') THEN
    GRANT SELECT, INSERT, UPDATE ON stk_pushes, mpesa_callbacks TO talksasa_app;
  ELSE
    RAISE WARNING 'role talksasa_app does not exist; skipping grants';
  END IF;
END $$;
