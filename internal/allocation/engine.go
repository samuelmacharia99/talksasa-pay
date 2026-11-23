// Package allocation decides where incoming money goes. The engine in this
// file is pure: no I/O, no clock, no randomness. It is given a snapshot of the
// payer's payment requests (read under a row lock by the service) and
// returns an allocation plan that the service writes to the ledger.
//
// Rules (confirmed with Talksasa, 2026-11-22):
//
//  1. A payment started for a specific request (STK push / hosted page) pays
//     that request first, if it is still open or partially paid.
//  2. Anything left pays the payer's other open/partially-paid requests,
//     oldest first (created_at, then id as a tie-breaker).
//  3. Anything left after that becomes credit on the payer account.
//  4. Money that arrives for a cancelled or expired request is never
//     rejected: rule 1 is skipped and rules 2-3 apply.
//  5. When a new request is created, available credit is applied to it
//     immediately, up to the request amount.
//  6. A request's status is derived from what has been allocated to it:
//     nothing -> open, some -> partially_paid, all -> paid. A request can
//     never be over-allocated; the excess always goes to credit.
//
// Duplicate receipts are handled before the engine runs, by the ledger's
// unique (channel, provider_transaction_id) constraint.
package allocation

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/money"
)

// Status is a payment request status.
type Status string

const (
	StatusOpen          Status = "open"
	StatusPartiallyPaid Status = "partially_paid"
	StatusPaid          Status = "paid"
	StatusCancelled     Status = "cancelled"
	StatusExpired       Status = "expired"
)

// Payable reports whether new money may be allocated to a request in this status.
func (s Status) Payable() bool { return s == StatusOpen || s == StatusPartiallyPaid }

var (
	ErrNonPositiveAmount = errors.New("allocation: amount must be greater than zero")
	ErrInvalidRequest    = errors.New("allocation: inconsistent request state")
	ErrDuplicateRequest  = errors.New("allocation: request listed twice")
	ErrNothingToReverse  = errors.New("allocation: nothing to reverse")
)

// Request is a snapshot of one payment request.
type Request struct {
	ID        uuid.UUID
	Amount    money.KES
	Paid      money.KES
	Status    Status
	CreatedAt time.Time
}

// Outstanding is what is still owed on the request.
func (r Request) Outstanding() money.KES { return r.Amount - r.Paid }

func (r Request) validate() error {
	switch {
	case r.ID == uuid.Nil:
		return fmt.Errorf("%w: missing id", ErrInvalidRequest)
	case r.Amount <= 0:
		return fmt.Errorf("%w: request %s has non-positive amount", ErrInvalidRequest, r.ID)
	case r.Paid < 0 || r.Paid > r.Amount:
		return fmt.Errorf("%w: request %s paid %d of %d", ErrInvalidRequest, r.ID, r.Paid, r.Amount)
	}
	if DeriveStatus(r.Amount, r.Paid, r.Status) != r.Status {
		return fmt.Errorf("%w: request %s status %s does not match paid %d of %d", ErrInvalidRequest, r.ID, r.Status, r.Paid, r.Amount)
	}
	return nil
}

// DeriveStatus computes a request's status from its allocations. Cancelled and
// expired are terminal states set by the product or the expiry job and are
// kept as-is (they can only be entered with nothing paid).
func DeriveStatus(amount, paid money.KES, current Status) Status {
	if current == StatusCancelled || current == StatusExpired {
		return current
	}
	switch {
	case paid <= 0:
		return StatusOpen
	case paid < amount:
		return StatusPartiallyPaid
	default:
		return StatusPaid
	}
}

// Line is one allocation: money to a request, or (RequestID == uuid.Nil) to
// the payer's credit balance. Amounts are signed: credit applications and
// reversals produce negative lines.
type Line struct {
	RequestID uuid.UUID
	Amount    money.KES
}

// IsCredit reports whether the line targets the credit balance.
func (l Line) IsCredit() bool { return l.RequestID == uuid.Nil }

// Plan is an ordered list of allocation lines.
type Plan struct {
	Lines []Line
}

// Total is the sum of all lines. A payment plan totals the payment amount; a
// credit application or reversal-of-credit plan totals zero or the negated payment.
func (p Plan) Total() money.KES {
	var t money.KES
	for _, l := range p.Lines {
		t += l.Amount
	}
	return t
}

// CreditDelta is the net change to the payer's credit balance.
func (p Plan) CreditDelta() money.KES {
	var t money.KES
	for _, l := range p.Lines {
		if l.IsCredit() {
			t += l.Amount
		}
	}
	return t
}

// PlanPayment allocates an incoming payment. target may be uuid.Nil (C2B or
// suspense assignment: no specific request). requests is every request of the
// payer the service loaded; non-payable ones are ignored.
func PlanPayment(amount money.KES, target uuid.UUID, requests []Request) (Plan, error) {
	if amount <= 0 {
		return Plan{}, ErrNonPositiveAmount
	}
	seen := make(map[uuid.UUID]struct{}, len(requests))
	payable := make([]Request, 0, len(requests))
	for _, r := range requests {
		if err := r.validate(); err != nil {
			return Plan{}, err
		}
		if _, dup := seen[r.ID]; dup {
			return Plan{}, fmt.Errorf("%w: %s", ErrDuplicateRequest, r.ID)
		}
		seen[r.ID] = struct{}{}
		if r.Status.Payable() && r.Outstanding() > 0 {
			payable = append(payable, r)
		}
	}

	sort.SliceStable(payable, func(i, j int) bool {
		a, b := payable[i], payable[j]
		// Rule 1: the target request always sorts first.
		if (a.ID == target) != (b.ID == target) {
			return a.ID == target
		}
		// Rule 2: oldest first, id as a deterministic tie-breaker.
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		return a.ID.String() < b.ID.String()
	})

	remaining := amount
	var plan Plan
	for _, r := range payable {
		if remaining == 0 {
			break
		}
		take := money.Min(remaining, r.Outstanding())
		plan.Lines = append(plan.Lines, Line{RequestID: r.ID, Amount: take})
		remaining -= take
	}
	// Rule 3: leftover becomes credit.
	if remaining > 0 {
		plan.Lines = append(plan.Lines, Line{RequestID: uuid.Nil, Amount: remaining})
	}
	return plan, nil
}

// PlanCreditApplication applies available credit to a request (rule 5). The
// plan totals zero: +x to the request, -x from credit. An empty plan means
// there is nothing to apply.
func PlanCreditApplication(availableCredit money.KES, req Request) (Plan, error) {
	if availableCredit < 0 {
		return Plan{}, fmt.Errorf("%w: negative credit balance", ErrInvalidRequest)
	}
	if err := req.validate(); err != nil {
		return Plan{}, err
	}
	if !req.Status.Payable() {
		return Plan{}, nil
	}
	x := money.Min(availableCredit, req.Outstanding())
	if x == 0 {
		return Plan{}, nil
	}
	return Plan{Lines: []Line{
		{RequestID: req.ID, Amount: x},
		{RequestID: uuid.Nil, Amount: -x},
	}}, nil
}

// PlanReversal undoes the lines of an original payment, newest line first.
func PlanReversal(original []Line) (Plan, error) {
	if len(original) == 0 {
		return Plan{}, ErrNothingToReverse
	}
	out := make([]Line, 0, len(original))
	for i := len(original) - 1; i >= 0; i-- {
		out = append(out, Line{RequestID: original[i].RequestID, Amount: -original[i].Amount})
	}
	return Plan{Lines: out}, nil
}
