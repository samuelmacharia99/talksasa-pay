-- A push we had given up on (failed) can still be paid: Safaricom's late
-- success callback is recorded as a NEW transaction, because the push's own
-- pending transaction was already failed and failed is final. The push then
-- points at the transaction that actually holds the money, so products see
-- the right transaction_id on stk_push.succeeded (user decision 2026-11-24).
-- NULL means the money (if any) is on stk_pushes.transaction_id.
SET lock_timeout = '10s';

ALTER TABLE stk_pushes ADD COLUMN paid_transaction_id uuid REFERENCES transactions (id);
-- Why staff may NOT confirm this push's transaction with a receipt, set when
-- a callback reported an anomaly (amount mismatch, receipt already recorded
-- elsewhere) — even if the push was already in review. Set once.
ALTER TABLE stk_pushes ADD COLUMN confirm_blocked text
  CHECK (confirm_blocked IS NULL OR confirm_blocked IN ('amount_mismatch', 'receipt_taken'));
COMMENT ON COLUMN stk_pushes.paid_transaction_id IS
  'Set once when the payment was recorded on a different transaction than transaction_id (late success after the push was failed).';

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
     OR (OLD.completed_at IS NOT NULL AND NEW.completed_at IS DISTINCT FROM OLD.completed_at)
     OR (OLD.paid_transaction_id IS NOT NULL AND NEW.paid_transaction_id IS DISTINCT FROM OLD.paid_transaction_id)
     OR (OLD.confirm_blocked IS NOT NULL AND NEW.confirm_blocked IS DISTINCT FROM OLD.confirm_blocked) THEN
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
