-- name: SetSTKPushPaidTransaction :one
-- Late success after give-up: the money is on a new transaction.
UPDATE stk_pushes SET paid_transaction_id = @paid_transaction_id
 WHERE id = @id AND paid_transaction_id IS NULL
RETURNING *;

-- name: BlockSTKPushConfirm :exec
-- A callback anomaly: staff must not confirm this push with a receipt.
UPDATE stk_pushes SET confirm_blocked = @reason WHERE id = @id AND confirm_blocked IS NULL;
