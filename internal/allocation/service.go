package allocation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/money"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// The service methods run inside a caller-supplied database transaction
// (*store.Queries bound to a pgx.Tx), so allocation commits atomically with
// whatever recorded the money. Lock order is always:
// transaction row -> payer row -> payment request rows.

var (
	ErrNotAllocatable = errors.New("allocation: transaction cannot be allocated")
	ErrNoPayer        = errors.New("allocation: transaction is not matched to a payer")
	ErrCreditUsed     = errors.New("allocation: credit from this payment has already been used; reverse or adjust manually")
)

// RequestChange is the before/after state of a request touched by a batch.
// Callers use it to emit product webhooks (paid / partially_paid).
type RequestChange struct {
	RequestID uuid.UUID
	Before    Status
	After     Status
	Allocated money.KES
	Paid      money.KES
	Amount    money.KES
}

// Result describes one committed (or previously committed) allocation.
type Result struct {
	BatchID       uuid.UUID
	Duplicate     bool // the transaction had already been allocated: nothing changed
	Lines         []Line
	Requests      []RequestChange
	CreditDelta   money.KES
	CreditBalance money.KES
}

// Service writes allocation plans to the ledger.
type Service struct {
	logger *slog.Logger
}

// NewService builds an allocation service.
func NewService(logger *slog.Logger) *Service { return &Service{logger: logger} }

// AllocateTransaction allocates a succeeded incoming payment to its payer.
// It is idempotent: calling it again for the same transaction returns the
// original result with Duplicate=true and changes nothing.
func (s *Service) AllocateTransaction(ctx context.Context, q *store.Queries, actor audit.Actor, txnID uuid.UUID) (Result, error) {
	txn, err := q.LockTransactionInternal(ctx, txnID)
	if err != nil {
		return Result{}, fmt.Errorf("allocation: lock transaction: %w", err)
	}
	if txn.Kind != store.TxnKindPayment || txn.Direction != store.TxnDirectionIn || txn.Status != store.TxnStatusSucceeded {
		return Result{}, fmt.Errorf("%w: %s %s %s", ErrNotAllocatable, txn.Kind, txn.Direction, txn.Status)
	}
	if txn.PayerAccountID == nil || txn.ProductID == nil {
		return Result{}, ErrNoPayer
	}
	productID, payerID := *txn.ProductID, *txn.PayerAccountID

	if existing, err := q.GetPaymentBatchForTransaction(ctx, &txn.ID); err == nil {
		return s.existingResult(ctx, q, productID, payerID, existing)
	} else if !db.IsNotFound(err) {
		return Result{}, err
	}

	payer, err := q.LockPayer(ctx, store.LockPayerParams{ProductID: productID, ID: payerID})
	if err != nil {
		return Result{}, fmt.Errorf("allocation: lock payer: %w", err)
	}
	rows, err := q.ListPayableRequestsForUpdate(ctx, store.ListPayableRequestsForUpdateParams{ProductID: productID, PayerAccountID: payerID})
	if err != nil {
		return Result{}, fmt.Errorf("allocation: load requests: %w", err)
	}
	reqs, before := toRequests(rows)

	target := uuid.Nil
	if txn.PaymentRequestID != nil {
		target = *txn.PaymentRequestID
	}
	plan, err := PlanPayment(money.KES(txn.Amount), target, reqs)
	if err != nil {
		return Result{}, err
	}

	batchID, err := s.writeBatch(ctx, q, actor, writeBatchParams{
		productID: productID, payerID: payerID, kind: store.AllocationBatchKindPayment,
		transactionID: &txn.ID, expected: plan.Total(), plan: plan,
	})
	if err != nil {
		return Result{}, err
	}
	res, err := s.buildResult(ctx, q, productID, batchID, plan, before, money.KES(payer.CreditBalance))
	if err != nil {
		return Result{}, err
	}
	s.logger.InfoContext(ctx, "payment allocated",
		slog.String("transaction_id", txn.ID.String()), slog.String("payer_id", payerID.String()),
		slog.String("batch_id", batchID.String()), slog.Int64("amount", txn.Amount),
		slog.Int("requests", len(res.Requests)), slog.Int64("to_credit", int64(res.CreditDelta)))
	return res, nil
}

// ApplyCredit applies the payer's available credit to a payable request
// (rule 5). Returns a zero Result (BatchID == uuid.Nil) when there is nothing
// to apply.
func (s *Service) ApplyCredit(ctx context.Context, q *store.Queries, actor audit.Actor, productID, requestID uuid.UUID) (Result, error) {
	reqRow, err := q.GetPaymentRequest(ctx, store.GetPaymentRequestParams{ProductID: productID, ID: requestID})
	if err != nil {
		return Result{}, fmt.Errorf("allocation: load request: %w", err)
	}
	payer, err := q.LockPayer(ctx, store.LockPayerParams{ProductID: productID, ID: reqRow.PayerAccountID})
	if err != nil {
		return Result{}, fmt.Errorf("allocation: lock payer: %w", err)
	}
	if payer.CreditBalance == 0 {
		return Result{CreditBalance: 0}, nil
	}
	reqRow, err = q.LockPaymentRequest(ctx, store.LockPaymentRequestParams{ProductID: productID, ID: requestID})
	if err != nil {
		return Result{}, fmt.Errorf("allocation: lock request: %w", err)
	}
	reqs, before := toRequests([]store.PaymentRequest{reqRow})
	plan, err := PlanCreditApplication(money.KES(payer.CreditBalance), reqs[0])
	if err != nil {
		return Result{}, err
	}
	if len(plan.Lines) == 0 {
		return Result{CreditBalance: money.KES(payer.CreditBalance)}, nil
	}
	batchID, err := s.writeBatch(ctx, q, actor, writeBatchParams{
		productID: productID, payerID: payer.ID, kind: store.AllocationBatchKindCreditApplication,
		requestID: &requestID, expected: 0, plan: plan,
	})
	if err != nil {
		return Result{}, err
	}
	res, err := s.buildResult(ctx, q, productID, batchID, plan, before, money.KES(payer.CreditBalance))
	if err != nil {
		return Result{}, err
	}
	s.logger.InfoContext(ctx, "credit applied to payment request",
		slog.String("payment_request_id", requestID.String()), slog.String("payer_id", payer.ID.String()),
		slog.Int64("amount", int64(-res.CreditDelta)))
	return res, nil
}

// ReverseBatch writes the exact negation of a payment batch, funded by the
// given reversal transaction. Requests it had paid reopen. If the credit the
// payment created has since been spent, the reversal is refused with
// ErrCreditUsed: that case needs a human decision.
func (s *Service) ReverseBatch(ctx context.Context, q *store.Queries, actor audit.Actor, reversalTxnID uuid.UUID, original store.AllocationBatch, reason string) (Result, error) {
	payer, err := q.LockPayer(ctx, store.LockPayerParams{ProductID: original.ProductID, ID: original.PayerAccountID})
	if err != nil {
		return Result{}, fmt.Errorf("allocation: lock payer: %w", err)
	}
	lines, err := q.ListAllocationsForBatch(ctx, original.ID)
	if err != nil {
		return Result{}, err
	}
	orig := make([]Line, 0, len(lines))
	var ids []uuid.UUID
	for _, l := range lines {
		ln := Line{Amount: money.KES(l.Amount)}
		if l.PaymentRequestID != nil {
			ln.RequestID = *l.PaymentRequestID
			ids = append(ids, *l.PaymentRequestID)
		}
		orig = append(orig, ln)
	}
	plan, err := PlanReversal(orig)
	if err != nil {
		return Result{}, err
	}
	if money.KES(payer.CreditBalance)+plan.CreditDelta() < 0 {
		return Result{}, ErrCreditUsed
	}
	var before map[uuid.UUID]RequestChange
	if len(ids) > 0 {
		rows, err := q.ListPaymentRequestsByIDs(ctx, store.ListPaymentRequestsByIDsParams{ProductID: original.ProductID, Ids: ids})
		if err != nil {
			return Result{}, err
		}
		_, before = toRequests(rows)
	}
	batchID, err := s.writeBatch(ctx, q, actor, writeBatchParams{
		productID: original.ProductID, payerID: original.PayerAccountID, kind: store.AllocationBatchKindReversal,
		transactionID: &reversalTxnID, reverses: &original.ID, expected: plan.Total(), plan: plan, reason: &reason,
	})
	if err != nil {
		return Result{}, err
	}
	return s.buildResult(ctx, q, original.ProductID, batchID, plan, before, money.KES(payer.CreditBalance))
}

type writeBatchParams struct {
	productID, payerID uuid.UUID
	kind               store.AllocationBatchKind
	transactionID      *uuid.UUID
	requestID          *uuid.UUID
	reverses           *uuid.UUID
	expected           money.KES
	plan               Plan
	reason             *string
}

func (s *Service) writeBatch(ctx context.Context, q *store.Queries, actor audit.Actor, p writeBatchParams) (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := q.InsertAllocationBatch(ctx, store.InsertAllocationBatchParams{
		ID: id, ProductID: p.productID, PayerAccountID: p.payerID, Kind: p.kind,
		TransactionID: p.transactionID, PaymentRequestID: p.requestID, ReversesBatchID: p.reverses,
		ExpectedTotal: int64(p.expected), CreatedBy: actor.String(), Reason: p.reason,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("allocation: insert batch: %w", err)
	}
	for i, l := range p.plan.Lines {
		var reqID *uuid.UUID
		if !l.IsCredit() {
			rid := l.RequestID
			reqID = &rid
		}
		if err := q.InsertAllocation(ctx, store.InsertAllocationParams{
			BatchID: id, LineNo: int32(i + 1), ProductID: p.productID, PayerAccountID: p.payerID,
			PaymentRequestID: reqID, Amount: int64(l.Amount),
		}); err != nil {
			if db.IsCheckViolation(err, "payer_accounts_credit_balance_check") {
				return uuid.Nil, ErrCreditUsed
			}
			return uuid.Nil, fmt.Errorf("allocation: insert line %d: %w", i+1, err)
		}
	}
	return id, nil
}

// buildResult re-reads touched requests and the payer's balance after the
// triggers have applied the lines, so the result reflects the database.
func (s *Service) buildResult(ctx context.Context, q *store.Queries, productID, batchID uuid.UUID, plan Plan, before map[uuid.UUID]RequestChange, creditBefore money.KES) (Result, error) {
	res := Result{BatchID: batchID, Lines: plan.Lines, CreditDelta: plan.CreditDelta(), CreditBalance: creditBefore + plan.CreditDelta()}
	var ids []uuid.UUID
	allocated := map[uuid.UUID]money.KES{}
	for _, l := range plan.Lines {
		if !l.IsCredit() {
			if _, seen := allocated[l.RequestID]; !seen {
				ids = append(ids, l.RequestID)
			}
			allocated[l.RequestID] += l.Amount
		}
	}
	if len(ids) == 0 {
		return res, nil
	}
	rows, err := q.ListPaymentRequestsByIDs(ctx, store.ListPaymentRequestsByIDsParams{ProductID: productID, Ids: ids})
	if err != nil {
		return Result{}, err
	}
	for _, r := range rows {
		b := before[r.ID]
		res.Requests = append(res.Requests, RequestChange{
			RequestID: r.ID, Before: b.Before, After: Status(r.Status), Allocated: allocated[r.ID],
			Paid: money.KES(r.AmountPaid), Amount: money.KES(r.Amount),
		})
	}
	return res, nil
}

func (s *Service) existingResult(ctx context.Context, q *store.Queries, productID, payerID uuid.UUID, b store.AllocationBatch) (Result, error) {
	lines, err := q.ListAllocationsForBatch(ctx, b.ID)
	if err != nil {
		return Result{}, err
	}
	payer, err := q.GetPayer(ctx, store.GetPayerParams{ProductID: productID, ID: payerID})
	if err != nil {
		return Result{}, err
	}
	res := Result{BatchID: b.ID, Duplicate: true, CreditBalance: money.KES(payer.CreditBalance)}
	for _, l := range lines {
		ln := Line{Amount: money.KES(l.Amount)}
		if l.PaymentRequestID != nil {
			ln.RequestID = *l.PaymentRequestID
		}
		res.Lines = append(res.Lines, ln)
	}
	res.CreditDelta = Plan{Lines: res.Lines}.CreditDelta()
	return res, nil
}

func toRequests(rows []store.PaymentRequest) ([]Request, map[uuid.UUID]RequestChange) {
	reqs := make([]Request, 0, len(rows))
	before := make(map[uuid.UUID]RequestChange, len(rows))
	for _, r := range rows {
		reqs = append(reqs, Request{
			ID: r.ID, Amount: money.KES(r.Amount), Paid: money.KES(r.AmountPaid),
			Status: Status(r.Status), CreatedAt: r.CreatedAt,
		})
		before[r.ID] = RequestChange{RequestID: r.ID, Before: Status(r.Status)}
	}
	return reqs, before
}
