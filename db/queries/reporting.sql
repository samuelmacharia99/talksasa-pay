-- Read-only queries for the admin panel and the hosted pay page.
--
-- Lists use keyset pagination on (created_at, id) DESC: pass the last row's
-- values as cursor_at / cursor_id to get the next page. Every filter is
-- optional (NULL = no filter). Search patterns arrive already lower-cased and
-- LIKE-escaped from Go (reporting.likePattern).

-- name: ListPayersAdmin :many
SELECT pa.id, pa.product_id, p.account_prefix, pa.external_ref, pa.account_number, pa.display_name,
       pa.email, pa.status, pa.credit_balance, pa.created_at
  FROM payer_accounts pa
  JOIN products p ON p.id = pa.product_id
 WHERE (sqlc.narg(product_id)::uuid IS NULL OR pa.product_id = sqlc.narg(product_id))
   AND (sqlc.narg(pattern)::text IS NULL
        OR pa.account_number_norm = sqlc.narg(account_norm)::text
        OR lower(pa.display_name) LIKE sqlc.narg(pattern)
        OR lower(pa.external_ref) LIKE sqlc.narg(pattern)
        OR EXISTS (SELECT 1 FROM payer_phones ph WHERE ph.payer_account_id = pa.id AND ph.phone = sqlc.narg(phone)::text))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (pa.created_at, pa.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY pa.created_at DESC, pa.id DESC
 LIMIT @lim;

-- name: GetPayerAdmin :one
SELECT pa.*, p.account_prefix, p.name AS product_name
  FROM payer_accounts pa
  JOIN products p ON p.id = pa.product_id
 WHERE pa.id = @id;

-- name: ListPaymentRequestsAdmin :many
SELECT pr.id, pr.product_id, p.account_prefix, pr.payer_account_id, pa.account_number, pa.display_name,
       pr.product_reference, pr.description, pr.amount, pr.amount_paid, pr.status, pr.expires_at,
       pr.created_at, pr.paid_at, pr.closed_at, pr.duplicate_reference
  FROM payment_requests pr
  JOIN payer_accounts pa ON pa.id = pr.payer_account_id
  JOIN products p ON p.id = pr.product_id
 WHERE (sqlc.narg(product_id)::uuid IS NULL OR pr.product_id = sqlc.narg(product_id))
   AND (NOT @duplicates_only::boolean OR (pr.duplicate_reference AND pr.status IN ('open', 'partially_paid')))
   AND (sqlc.narg(payer_id)::uuid IS NULL OR pr.payer_account_id = sqlc.narg(payer_id))
   AND (sqlc.narg(status)::payment_request_status IS NULL OR pr.status = sqlc.narg(status))
   AND (sqlc.narg(pattern)::text IS NULL
        OR pa.account_number_norm = sqlc.narg(account_norm)::text
        OR lower(pr.product_reference) LIKE sqlc.narg(pattern)
        OR lower(pr.description) LIKE sqlc.narg(pattern))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (pr.created_at, pr.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY pr.created_at DESC, pr.id DESC
 LIMIT @lim;

-- name: GetPaymentRequestAdmin :one
SELECT pr.*, p.account_prefix, p.name AS product_name, pa.account_number, pa.display_name
  FROM payment_requests pr
  JOIN payer_accounts pa ON pa.id = pr.payer_account_id
  JOIN products p ON p.id = pr.product_id
 WHERE pr.id = @id;

-- name: ListTransactionsAdmin :many
SELECT t.id, t.product_id, p.account_prefix, t.direction, t.kind, t.channel, t.status, t.amount,
       t.provider_transaction_id, t.phone, t.bill_ref, t.payer_account_id, pa.account_number,
       t.payment_request_id, t.failure_reason, t.created_at, t.completed_at
  FROM transactions t
  LEFT JOIN products p ON p.id = t.product_id
  LEFT JOIN payer_accounts pa ON pa.id = t.payer_account_id
 WHERE (sqlc.narg(product_id)::uuid IS NULL OR t.product_id = sqlc.narg(product_id))
   AND (sqlc.narg(payer_id)::uuid IS NULL OR t.payer_account_id = sqlc.narg(payer_id))
   AND (sqlc.narg(request_id)::uuid IS NULL OR t.payment_request_id = sqlc.narg(request_id))
   AND (sqlc.narg(channel)::txn_channel IS NULL OR t.channel = sqlc.narg(channel))
   AND (sqlc.narg(status)::txn_status IS NULL OR t.status = sqlc.narg(status))
   AND (sqlc.narg(direction)::txn_direction IS NULL OR t.direction = sqlc.narg(direction))
   AND (sqlc.narg(receipt)::text IS NULL OR t.provider_transaction_id = sqlc.narg(receipt))
   AND (sqlc.narg(phone)::text IS NULL OR t.phone = sqlc.narg(phone))
   AND (sqlc.narg(created_from)::timestamptz IS NULL OR t.created_at >= sqlc.narg(created_from))
   AND (sqlc.narg(created_to)::timestamptz IS NULL OR t.created_at < sqlc.narg(created_to))
   AND (NOT @suspense_only::boolean
        OR (t.payer_account_id IS NULL AND t.direction = 'in' AND t.status = 'succeeded'))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (t.created_at, t.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::uuid))
 ORDER BY t.created_at DESC, t.id DESC
 LIMIT @lim;

-- name: GetTransactionAdmin :one
SELECT t.*, p.account_prefix, pa.account_number, pa.display_name AS payer_name, pr.product_reference
  FROM transactions t
  LEFT JOIN products p ON p.id = t.product_id
  LEFT JOIN payer_accounts pa ON pa.id = t.payer_account_id
  LEFT JOIN payment_requests pr ON pr.id = t.payment_request_id
 WHERE t.id = @id;

-- name: ListTransactionEvents :many
SELECT * FROM transaction_events WHERE transaction_id = @transaction_id ORDER BY id;

-- name: ListAllocationsForTransactionAdmin :many
-- Every allocation line that came from this transaction (its payment batch
-- and, if reversed, the reversal batch).
SELECT a.id, a.batch_id, b.kind AS batch_kind, a.line_no, a.payment_request_id, pr.product_reference,
       a.amount, a.created_at
  FROM allocation_batches b
  JOIN allocations a ON a.batch_id = b.id
  LEFT JOIN payment_requests pr ON pr.id = a.payment_request_id
 WHERE b.transaction_id = @transaction_id
 ORDER BY a.created_at, a.batch_id, a.line_no;

-- name: ListAllocationsForRequestAdmin :many
SELECT a.id, a.batch_id, b.kind AS batch_kind, b.transaction_id, t.provider_transaction_id, a.amount, a.created_at
  FROM allocations a
  JOIN allocation_batches b ON b.id = a.batch_id
  LEFT JOIN transactions t ON t.id = b.transaction_id
 WHERE a.payment_request_id = @payment_request_id
 ORDER BY a.id;

-- name: ListCreditMovementsAdmin :many
-- Lines that moved money in or out of a payer's credit balance.
SELECT a.id, a.batch_id, b.kind AS batch_kind, b.transaction_id, b.payment_request_id, a.amount, a.created_at
  FROM allocations a
  JOIN allocation_batches b ON b.id = a.batch_id
 WHERE a.payer_account_id = @payer_account_id AND a.payment_request_id IS NULL
 ORDER BY a.id DESC
 LIMIT @lim;

-- name: ListPayerPhonesAdmin :many
SELECT phone, is_primary FROM payer_phones WHERE payer_account_id = @payer_account_id
 ORDER BY is_primary DESC, created_at;

-- name: ListAuditAdmin :many
SELECT a.id, a.occurred_at, a.actor_type, a.actor_id, a.action, a.entity_type, a.entity_id, a.product_id,
       p.account_prefix, a.details
  FROM audit_log a
  LEFT JOIN products p ON p.id = a.product_id
 WHERE (sqlc.narg(product_id)::uuid IS NULL OR a.product_id = sqlc.narg(product_id))
   AND (sqlc.narg(action_prefix)::text IS NULL OR a.action LIKE sqlc.narg(action_prefix))
   AND (sqlc.narg(actor_id)::text IS NULL OR a.actor_id = sqlc.narg(actor_id))
   AND (sqlc.narg(entity_id)::text IS NULL OR a.entity_id = sqlc.narg(entity_id))
   AND (sqlc.narg(cursor_at)::timestamptz IS NULL
        OR (a.occurred_at, a.id) < (sqlc.narg(cursor_at)::timestamptz, sqlc.narg(cursor_id)::bigint))
 ORDER BY a.occurred_at DESC, a.id DESC
 LIMIT @lim;

-- name: CollectionsByDay :many
-- Successful incoming payments per Nairobi calendar day and product.
-- Unrouted (suspense) money is still collected revenue: it is reported under
-- the nil UUID ("unassigned") rather than dropped.
SELECT ((t.completed_at AT TIME ZONE 'Africa/Nairobi')::date)::text AS day,
       coalesce(t.product_id, '00000000-0000-0000-0000-000000000000'::uuid)::uuid AS product_id,
       sum(t.amount)::bigint AS total, count(*)::bigint AS payments
  FROM transactions t
 WHERE t.direction = 'in' AND t.kind = 'payment' AND t.status = 'succeeded'
   AND t.completed_at >= sqlc.arg(since)::timestamptz
 GROUP BY 1, 2
 ORDER BY 1, 2;

-- name: DashboardCounts :one
SELECT
  (SELECT count(*) FROM payment_requests WHERE status IN ('open', 'partially_paid'))::bigint AS open_requests,
  (SELECT coalesce(sum(amount - amount_paid), 0) FROM payment_requests WHERE status IN ('open', 'partially_paid'))::bigint AS outstanding,
  (SELECT count(*) FROM transactions WHERE payer_account_id IS NULL AND direction = 'in' AND status = 'succeeded')::bigint AS suspense_count,
  (SELECT coalesce(sum(amount), 0) FROM transactions WHERE payer_account_id IS NULL AND direction = 'in' AND status = 'succeeded')::bigint AS suspense_amount,
  (SELECT count(*) FROM transactions WHERE status = 'needs_review')::bigint AS needs_review,
  (SELECT count(*) FROM transactions WHERE status = 'pending')::bigint AS pending,
  (SELECT coalesce(sum(credit_balance), 0) FROM payer_accounts)::bigint AS credit_held,
  (SELECT count(*) FROM payer_accounts)::bigint AS payers,
  (SELECT count(*) FROM payment_requests WHERE duplicate_reference AND status IN ('open', 'partially_paid'))::bigint AS duplicate_references;

-- name: GetPaymentRequestByHostedToken :one
SELECT pr.id, pr.product_id, p.name AS product_name, p.account_prefix, p.status AS product_status,
       pa.account_number, pr.product_reference, pr.description, pr.amount, pr.amount_paid, pr.status,
       pr.expires_at, pr.created_at, pr.paid_at
  FROM payment_requests pr
  JOIN products p ON p.id = pr.product_id
  JOIN payer_accounts pa ON pa.id = pr.payer_account_id
 WHERE pr.hosted_token = @hosted_token;
