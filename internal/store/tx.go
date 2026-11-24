package store

import "github.com/jackc/pgx/v5"

// Tx returns the transaction these queries run in, if any. Used by the event
// outbox to enqueue River jobs in the same transaction (River's InsertTx).
// Hand-written: sqlc does not touch this file.
func (q *Queries) Tx() (pgx.Tx, bool) {
	tx, ok := q.db.(pgx.Tx)
	return tx, ok
}
