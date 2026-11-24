-- Product API (/v1) batch reads that keep list endpoints free of N+1
-- queries. Scoped by product_id, taken ONLY from the authenticated API key.

-- name: ListAllocationsForTransactionsProduct :many
-- Allocation lines of several transactions at once (transaction list page).
SELECT b.transaction_id, a.batch_id, a.line_no, a.payment_request_id, a.amount, b.kind AS batch_kind, b.created_at
  FROM allocations a
  JOIN allocation_batches b ON b.product_id = a.product_id AND b.id = a.batch_id
 WHERE a.product_id = @product_id AND b.product_id = @product_id
   AND b.transaction_id = ANY(@transaction_ids::uuid[])
 ORDER BY b.transaction_id, b.created_at, a.line_no;
