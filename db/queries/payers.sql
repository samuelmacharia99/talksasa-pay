-- name: CreatePayer :one
INSERT INTO payer_accounts (id, product_id, external_ref, account_number, account_number_norm, display_name, email)
VALUES (@id, @product_id, @external_ref, @account_number, @account_number_norm, @display_name, @email)
RETURNING *;

-- name: GetPayer :one
SELECT * FROM payer_accounts WHERE product_id = @product_id AND id = @id;

-- name: GetPayerByExternalRef :one
SELECT * FROM payer_accounts WHERE product_id = @product_id AND external_ref = @external_ref;

-- name: GetPayerByAccountNumber :one
SELECT * FROM payer_accounts WHERE product_id = @product_id AND account_number_norm = @account_number_norm;

-- name: RouteAccountNumber :one
-- The only payer lookup not scoped by an authenticated product: used to route
-- an incoming payment by the account number the customer typed. The product
-- is derived from the account itself.
SELECT pa.id, pa.product_id, pa.account_number, pa.status, p.status AS product_status
  FROM payer_accounts pa
  JOIN products p ON p.id = pa.product_id
 WHERE pa.account_number_norm = @account_number_norm;

-- name: UpdatePayerProfile :one
UPDATE payer_accounts
   SET display_name = @display_name, email = @email
 WHERE product_id = @product_id AND id = @id
RETURNING *;

-- name: LockPayer :one
-- Per-payer mutex for allocation: every money movement for a payer takes
-- this lock first, then locks the payer's requests. NO KEY UPDATE (not
-- FOR UPDATE) so it does not conflict with the KEY SHARE locks that foreign
-- key checks take when inserting transactions/allocations, which would
-- otherwise deadlock concurrent payments for the same payer.
SELECT * FROM payer_accounts WHERE product_id = @product_id AND id = @id FOR NO KEY UPDATE;

-- name: ListPayerPhones :many
SELECT * FROM payer_phones WHERE product_id = @product_id AND payer_account_id = @payer_account_id
 ORDER BY is_primary DESC, created_at;

-- name: ClearPrimaryPhone :exec
UPDATE payer_phones SET is_primary = false
 WHERE product_id = @product_id AND payer_account_id = @payer_account_id AND is_primary;

-- name: UpsertPayerPhone :exec
INSERT INTO payer_phones (product_id, payer_account_id, phone, is_primary)
VALUES (@product_id, @payer_account_id, @phone, @is_primary)
ON CONFLICT (payer_account_id, phone) DO UPDATE SET is_primary = EXCLUDED.is_primary;

-- name: DeletePayerPhonesExcept :exec
DELETE FROM payer_phones
 WHERE product_id = @product_id AND payer_account_id = @payer_account_id
   AND NOT (phone = ANY(@keep::text[]));
