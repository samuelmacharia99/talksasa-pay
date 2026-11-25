-- name: SetSTKPushPaidTransaction :one
-- Late success after give-up: the money is on a new transaction.
UPDATE stk_pushes SET paid_transaction_id = @paid_transaction_id
 WHERE id = @id AND paid_transaction_id IS NULL
RETURNING *;

-- name: BlockSTKPushConfirm :exec
-- A callback anomaly: staff must not confirm this push with a receipt.
UPDATE stk_pushes SET confirm_blocked = @reason WHERE id = @id AND confirm_blocked IS NULL;

-- name: FindLiveSTKPushesForC2B :many
-- A paybill (C2B) confirmation may be Safaricom's second notice for an STK
-- payment. Candidates: prompts of this app's live charges with this account
-- number, still waiting (or in review) for exactly this amount, whose
-- transaction has no receipt yet, not blocked by an anomaly; phone must
-- match when the notice carries a usable one. The caller acts only on
-- exactly one candidate.
SELECT p.* FROM stk_pushes p
  JOIN transactions t ON t.id = p.transaction_id
  JOIN charges c ON c.app_id = p.app_id AND c.id = p.charge_id
 WHERE p.app_id = @app_id
   AND c.account_number_norm = @account_number_norm
   AND c.status IN ('pending', 'needs_review')
   AND p.amount = @amount
   AND p.status IN ('initiating', 'sent', 'unknown')
   AND p.confirm_blocked IS NULL
   AND t.status IN ('pending', 'needs_review')
   AND (sqlc.narg(phone)::text IS NULL OR p.phone = sqlc.narg(phone))
   AND p.created_at > now() - interval '1 day'
 ORDER BY p.created_at DESC
 LIMIT 2;
