-- Development only (the migrate command refuses down migrations elsewhere).

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
    UPDATE payment_requests
       SET amount_paid = v_paid,
           status      = v_status,
           paid_at     = CASE WHEN v_status = 'paid' THEN coalesce(paid_at, now()) ELSE NULL END
     WHERE id = r.id;
  END IF;

  PERFORM set_config('talksasa.allocating', 'off', true);
  RETURN NEW;
END $$;

DROP INDEX payment_requests_duplicate_reference;
DROP INDEX payment_requests_live_reference;
CREATE UNIQUE INDEX payment_requests_live_reference ON payment_requests (product_id, product_reference)
  WHERE status IN ('open', 'partially_paid');
ALTER TABLE payment_requests DROP COLUMN duplicate_reference;
