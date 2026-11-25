-- Money core (internal/ledger, internal/stk) helpers.

-- name: SetTransactionCharge :one
-- Staff assigned an unmatched payment to an app and it matched exactly one
-- live charge of that app: name the charge (set once; the guard trigger
-- refuses a change, and the composite FK keeps it inside the app).
UPDATE transactions SET charge_id = @charge_id
 WHERE id = @id AND app_id = @app_id AND charge_id IS NULL
RETURNING *;

-- name: CountReviewTransactionsForCharge :one
-- needs_review payments still naming a charge (a charge leaves needs_review
-- only when none is left).
SELECT count(*) FROM transactions
 WHERE charge_id = @charge_id AND status = 'needs_review' AND kind = 'payment';
