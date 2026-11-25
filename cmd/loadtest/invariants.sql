-- Money invariants (plan "Verification"; ADR 0002). One query; every row it
-- returns is a violation, as "<check>|<id>". No rows = all good.
--
-- Used by cmd/loadtest (-db-url-file), scripts/smoke.sh and the deploy
-- runbook. Read-only: runs as talksasa_app or any role that can SELECT.
--
--   receipt_twice          an M-Pesa receipt recorded on more than one transaction
--   charge_payment         a succeeded/reversed charge whose paying transaction is
--                          not a succeeded, receipted payment of the same app and
--                          charge for exactly the charge amount
--   charge_shared_payment  one transaction paying more than one charge
--   money_dropped          a pending/expired/cancelled charge that a succeeded
--                          payment names (late money must succeed it)
--   mpesa_no_receipt       a succeeded M-Pesa payment without a receipt
--   money_without_event    app money without its charge.succeeded or
--                          payment.received event
--   delivered_twice        an event answered 2xx by the app more than once
--   reversed_no_reversal   a reversed charge without a reversal transaction
--   audit_chain            the audit log hash chain is broken (id of first bad row)
SELECT 'receipt_twice' AS check_name, provider_transaction_id AS id
  FROM transactions
 WHERE provider = 'mpesa' AND provider_transaction_id IS NOT NULL
 GROUP BY provider_transaction_id
HAVING count(*) > 1
UNION ALL
SELECT 'charge_payment', c.id::text
  FROM charges c
  LEFT JOIN transactions t ON t.id = c.transaction_id
 WHERE c.status IN ('succeeded', 'reversed')
   AND (t.id IS NULL OR t.status <> 'succeeded' OR t.kind <> 'payment' OR t.direction <> 'in'
        OR t.amount <> c.amount OR t.charge_id IS DISTINCT FROM c.id OR t.app_id IS DISTINCT FROM c.app_id
        OR (t.provider = 'mpesa' AND t.provider_transaction_id IS NULL))
UNION ALL
SELECT 'charge_shared_payment', transaction_id::text
  FROM charges
 WHERE transaction_id IS NOT NULL
 GROUP BY transaction_id
HAVING count(*) > 1
UNION ALL
SELECT 'money_dropped', c.id::text
  FROM charges c
 WHERE c.status IN ('pending', 'expired', 'cancelled')
   AND EXISTS (SELECT 1 FROM transactions t
                WHERE t.charge_id = c.id AND t.kind = 'payment' AND t.status = 'succeeded')
UNION ALL
SELECT 'mpesa_no_receipt', id::text
  FROM transactions
 WHERE provider = 'mpesa' AND kind = 'payment' AND status = 'succeeded' AND provider_transaction_id IS NULL
UNION ALL
SELECT 'money_without_event', t.id::text
  FROM transactions t
 WHERE t.kind = 'payment' AND t.status = 'succeeded' AND t.app_id IS NOT NULL
   AND NOT EXISTS (
     SELECT 1 FROM events e
      WHERE e.app_id = t.app_id
        AND e.subject_id IN (t.id, t.charge_id)
        AND ((e.type = 'payment.received' AND e.data->>'id' = t.id::text)
          OR (e.type = 'charge.succeeded' AND e.data->>'transaction_id' = t.id::text)))
UNION ALL
SELECT 'delivered_twice', event_id::text
  FROM webhook_attempts
 WHERE status_code BETWEEN 200 AND 299
 GROUP BY event_id
HAVING count(*) > 1
UNION ALL
SELECT 'reversed_no_reversal', c.id::text
  FROM charges c
 WHERE c.status = 'reversed'
   AND NOT EXISTS (SELECT 1 FROM transactions r WHERE r.kind = 'reversal' AND r.reverses_transaction_id = c.transaction_id)
UNION ALL
SELECT 'audit_chain', v::text
  FROM (SELECT tsp_audit_verify() AS v) a
 WHERE v IS NOT NULL;
