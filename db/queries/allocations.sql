-- name: InsertAllocationBatch :one
INSERT INTO allocation_batches (id, product_id, payer_account_id, kind, transaction_id, payment_request_id,
                                reverses_batch_id, expected_total, created_by, reason)
VALUES (@id, @product_id, @payer_account_id, @kind, @transaction_id, @payment_request_id,
        @reverses_batch_id, @expected_total, @created_by, @reason)
RETURNING *;

-- name: InsertAllocation :exec
INSERT INTO allocations (batch_id, line_no, product_id, payer_account_id, payment_request_id, amount)
VALUES (@batch_id, @line_no, @product_id, @payer_account_id, @payment_request_id, @amount);

-- name: GetPaymentBatchForTransaction :one
SELECT * FROM allocation_batches WHERE transaction_id = @transaction_id AND kind = 'payment';

-- name: ListAllocationsForBatch :many
SELECT * FROM allocations WHERE batch_id = @batch_id ORDER BY line_no;

-- name: ListAllocationsForRequest :many
SELECT * FROM allocations WHERE product_id = @product_id AND payment_request_id = @payment_request_id ORDER BY id;

-- name: FindBalanceDrift :many
-- Cached balances that disagree with the allocation lines. Must always be empty.
SELECT 'payer_credit'::text AS kind, pa.id, pa.credit_balance::bigint AS cached, coalesce(s.total, 0)::bigint AS actual
  FROM payer_accounts pa
  LEFT JOIN (SELECT payer_account_id, sum(amount) AS total FROM allocations
              WHERE payment_request_id IS NULL GROUP BY payer_account_id) s ON s.payer_account_id = pa.id
 WHERE pa.credit_balance <> coalesce(s.total, 0)
UNION ALL
SELECT 'request_paid'::text, pr.id, pr.amount_paid::bigint, coalesce(s.total, 0)::bigint
  FROM payment_requests pr
  LEFT JOIN (SELECT payment_request_id, sum(amount) AS total FROM allocations
              WHERE payment_request_id IS NOT NULL GROUP BY payment_request_id) s ON s.payment_request_id = pr.id
 WHERE pr.amount_paid <> coalesce(s.total, 0);
