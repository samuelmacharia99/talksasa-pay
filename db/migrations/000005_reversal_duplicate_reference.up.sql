-- A reversal must always be recordable (the money really left at Safaricom).
-- If it reopens a paid request whose product_reference has since been billed
-- again by a newer live request, the reopened request is kept live and
-- flagged duplicate_reference; it is exempt from the one-live-request-per-
-- reference index so the reversal never fails. Staff resolve the duplicate.
ALTER TABLE payment_requests ADD COLUMN duplicate_reference boolean NOT NULL DEFAULT false;
COMMENT ON COLUMN payment_requests.duplicate_reference IS
  'Set by a reversal that reopened this request while another live request had the same product_reference. Maintained only by the allocations trigger.';

DROP INDEX payment_requests_live_reference;
CREATE UNIQUE INDEX payment_requests_live_reference ON payment_requests (product_id, product_reference)
  WHERE status IN ('open', 'partially_paid') AND NOT duplicate_reference;
CREATE INDEX payment_requests_duplicate_reference ON payment_requests (created_at DESC)
  WHERE duplicate_reference AND status IN ('open', 'partially_paid');

CREATE OR REPLACE FUNCTION tsp_payment_requests_guard() RETURNS trigger LANGUAGE plpgsql AS $$
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
  IF NEW.duplicate_reference <> OLD.duplicate_reference THEN
    RAISE EXCEPTION 'duplicate_reference can only change through allocations'
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

CREATE OR REPLACE FUNCTION tsp_allocations_apply() RETURNS trigger LANGUAGE plpgsql AS $$
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
    IF r.status = 'paid' AND v_status <> 'paid' AND NOT r.duplicate_reference THEN
      -- A reversal reopening a paid request: it rejoins the live-reference
      -- index and may collide with a newer request for the same reference.
      BEGIN
        UPDATE payment_requests
           SET amount_paid = v_paid, status = v_status, paid_at = NULL
         WHERE id = r.id;
      EXCEPTION WHEN unique_violation THEN
        UPDATE payment_requests
           SET amount_paid = v_paid, status = v_status, paid_at = NULL, duplicate_reference = true
         WHERE id = r.id;
      END;
    ELSE
      UPDATE payment_requests
         SET amount_paid = v_paid,
             status      = v_status,
             paid_at     = CASE WHEN v_status = 'paid' THEN coalesce(paid_at, now()) ELSE NULL END
       WHERE id = r.id;
    END IF;
  END IF;

  PERFORM set_config('talksasa.allocating', 'off', true);
  RETURN NEW;
END $$;
