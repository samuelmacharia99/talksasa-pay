package reporting

import (
	"context"

	"github.com/talksasa/talksasa-pay/internal/store"
)

// CallbackFilter filters the M-Pesa callback inbox.
type CallbackFilter struct {
	// AttentionOnly keeps callbacks not processed yet or whose processing
	// recorded an error.
	AttentionOnly bool
	Cursor        string
	Limit         int
}

// ListMpesaCallbacks lists stored Daraja callbacks, newest first. Bodies are
// not included (they hold phone numbers and names).
func (s *Service) ListMpesaCallbacks(ctx context.Context, f CallbackFilter) (Page[store.ListMpesaCallbacksAttentionRow], error) {
	var out Page[store.ListMpesaCallbacksAttentionRow]
	at, id, err := uuidCursor(f.Cursor)
	if err != nil {
		return out, err
	}
	lim := limit(f.Limit)
	rows, err := store.New(s.pool).ListMpesaCallbacksAttention(ctx, store.ListMpesaCallbacksAttentionParams{
		AttentionOnly: f.AttentionOnly, CursorAt: at, CursorID: id, Lim: lim + 1,
	})
	if err != nil {
		return out, err
	}
	if len(rows) > int(lim) {
		rows = rows[:lim]
		last := rows[len(rows)-1]
		out.NextCursor = encodeCursor(last.ReceivedAt, last.ID.String())
	}
	out.Items = rows
	return out, nil
}
