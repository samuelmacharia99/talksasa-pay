-- Development only (the migrate command refuses down migrations elsewhere).
CREATE OR REPLACE FUNCTION tsp_stk_pushes_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.id <> OLD.id OR NEW.product_id <> OLD.product_id OR NEW.payment_request_id <> OLD.payment_request_id
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

ALTER TABLE stk_pushes DROP COLUMN IF EXISTS confirm_blocked;
ALTER TABLE stk_pushes DROP COLUMN IF EXISTS paid_transaction_id;
