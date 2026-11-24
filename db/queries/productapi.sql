-- Product-facing (/v1) reads. Every query is scoped by product_id, which the
-- handler takes ONLY from the authenticated API key.

-- name: ListPaymentRequestsForProduct :many
SELECT pr.*, pa.external_ref AS payer_external_ref, pa.account_number
  FROM payment_requests pr
  JOIN payer_accounts pa ON pa.product_id = pr.product_id AND pa.id = pr.payer_account_id
 WHERE pr.product_id = @product_id
   AND (sqlc.narg(status)::payment_request_status IS NULL OR pr.status = sqlc.narg(status))
   AND (sqlc.narg(reference)::text IS NULL OR pr.product_reference = sqlc.narg(reference))
   AND (sqlc.narg(payer_id)::uuid IS NULL OR pr.payer_account_id = sqlc.narg(payer_id))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (pr.created_at, pr.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY pr.created_at DESC, pr.id DESC
 LIMIT @lim;

-- name: GetPaymentRequestForProduct :one
SELECT pr.*, pa.external_ref AS payer_external_ref, pa.account_number
  FROM payment_requests pr
  JOIN payer_accounts pa ON pa.product_id = pr.product_id AND pa.id = pr.payer_account_id
 WHERE pr.product_id = @product_id AND pr.id = @id;

-- name: ListTransactionsForProduct :many
SELECT t.id, t.direction, t.kind, t.channel, t.status, t.amount, t.currency, t.provider_transaction_id,
       t.phone, t.payer_account_id, t.payment_request_id, t.reverses_transaction_id,
       t.occurred_at, t.created_at, t.completed_at, pa.external_ref AS payer_external_ref, pa.account_number
  FROM transactions t
  LEFT JOIN payer_accounts pa ON pa.product_id = t.product_id AND pa.id = t.payer_account_id
 WHERE t.product_id = @product_id
   AND (sqlc.narg(payer_id)::uuid IS NULL OR t.payer_account_id = sqlc.narg(payer_id))
   AND (sqlc.narg(since)::timestamptz IS NULL OR t.created_at >= sqlc.narg(since))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (t.created_at, t.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY t.created_at DESC, t.id DESC
 LIMIT @lim;

-- name: ListAllocationsForTransactionProduct :many
SELECT a.batch_id, a.line_no, a.payment_request_id, a.amount, b.kind AS batch_kind, b.created_at
  FROM allocations a
  JOIN allocation_batches b ON b.id = a.batch_id
 WHERE a.product_id = @product_id AND b.transaction_id = @transaction_id
 ORDER BY b.created_at, a.line_no;

-- name: GetTransactionViewForProduct :one
SELECT t.id, t.direction, t.kind, t.channel, t.status, t.amount, t.currency, t.provider_transaction_id,
       t.phone, t.payer_account_id, t.payment_request_id, t.reverses_transaction_id,
       t.occurred_at, t.created_at, t.completed_at, pa.external_ref AS payer_external_ref, pa.account_number
  FROM transactions t
  LEFT JOIN payer_accounts pa ON pa.product_id = t.product_id AND pa.id = t.payer_account_id
 WHERE t.product_id = @product_id AND t.id = @id;
