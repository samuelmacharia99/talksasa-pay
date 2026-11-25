package stk

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/sync/errgroup"

	"github.com/talksasa/talksasa-pay/internal/mpesa"
	"github.com/talksasa/talksasa-pay/internal/platform/db"
	"github.com/talksasa/talksasa-pay/internal/store"
)

// SweepResult counts what one sweep did.
type SweepResult struct {
	Queried, Failed, NeedsReview, Rescheduled int
}

func (r *SweepResult) add(o SweepResult) {
	r.Queried += o.Queried
	r.Failed += o.Failed
	r.NeedsReview += o.NeedsReview
	r.Rescheduled += o.Rescheduled
}

// queryBackoff is the wait before the next status query after the n-th
// query (1-based) still found the push pending; the last value repeats.
var queryBackoff = []time.Duration{time.Minute, 2 * time.Minute, 5 * time.Minute, 15 * time.Minute}

// QueryBackoff returns the wait after the attempts-th status query.
func QueryBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > len(queryBackoff) {
		return queryBackoff[len(queryBackoff)-1]
	}
	return queryBackoff[attempts-1]
}

// Sweep checks due live pushes (worker periodic job): initiating past
// InitiatingTimeout -> failed; sent and due -> STK query: succeeded ->
// unknown + needs_review; failed -> failed; pending -> reschedule with
// backoff, or needs_review past GiveUpAfter.
//
// Due pushes are claimed in a short transaction (SKIP LOCKED, next check
// pushed out by SweepLease); Daraja is queried outside any transaction, and
// each outcome is applied in its own transaction that re-locks the push and
// re-checks its status, so a callback that won the race is never undone.
func (s *Service) Sweep(ctx context.Context, limit int) (SweepResult, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	now := s.o.Now()
	var due []Push
	err := db.WithTx(ctx, s.o.Pool, sweeperActor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		rows, err := q.ListDueSTKPushesAt(ctx, store.ListDueSTKPushesAtParams{Now: now, Lim: int32(limit)})
		if err != nil {
			return err
		}
		lease := now.Add(s.o.SweepLease)
		for _, p := range rows {
			if err := q.LeaseSTKPush(ctx, store.LeaseSTKPushParams{NextQueryAt: &lease, ID: p.ID}); err != nil {
				return err
			}
		}
		due = rows
		return nil
	})
	if err != nil {
		return SweepResult{}, err
	}

	var (
		mu       sync.Mutex
		res      SweepResult
		firstErr error
		g        errgroup.Group
	)
	g.SetLimit(s.o.SweepConcurrency)
	for _, p := range due {
		g.Go(func() error {
			r, err := s.sweepOne(ctx, p)
			mu.Lock()
			defer mu.Unlock()
			res.add(r)
			if err != nil {
				s.o.Logger.WarnContext(ctx, "stk sweep: push not checked; retried after its lease", slog.String("stk_push_id", p.ID.String()), slog.String("error", err.Error()))
				if firstErr == nil {
					firstErr = err
				}
			}
			return nil
		})
	}
	_ = g.Wait()
	return res, firstErr
}

func (s *Service) sweepOne(ctx context.Context, p Push) (SweepResult, error) {
	now := s.o.Now()
	attrs := []any{slog.String("stk_push_id", p.ID.String()), slog.String("charge_id", p.ChargeID.String())}
	switch p.Status {
	case store.StkPushStatusInitiating:
		if now.Sub(p.CreatedAt) < s.o.InitiatingTimeout {
			next := p.CreatedAt.Add(s.o.InitiatingTimeout)
			_, err := s.applySweep(ctx, p.ID, store.StkPushStatusInitiating, func(ctx context.Context, q *store.Queries, _ Push) error {
				return q.LeaseSTKPush(ctx, store.LeaseSTKPushParams{NextQueryAt: &next, ID: p.ID})
			})
			return SweepResult{}, err
		}
		const why = "No answer from Safaricom when sending the prompt"
		applied, err := s.applySweep(ctx, p.ID, store.StkPushStatusInitiating, func(ctx context.Context, q *store.Queries, cur Push) error {
			_, err := s.failPushTx(ctx, q, cur, nil, why, failNotSent, why)
			return err
		})
		if err != nil || !applied {
			return SweepResult{}, err
		}
		s.o.Logger.WarnContext(ctx, "stk push never confirmed by Daraja; failed", attrs...)
		return SweepResult{Failed: 1}, nil

	case store.StkPushStatusSent:
		var (
			res  SweepResult
			qr   mpesa.STKQueryResult
			qerr error
		)
		if s.o.Client == nil || p.CheckoutRequestID == nil {
			qerr = mpesa.ErrUnavailable
		} else {
			qr, qerr = s.o.Client.STKQuery(ctx, *p.CheckoutRequestID)
			res.Queried = 1
		}
		switch {
		case qerr == nil && qr.State == mpesa.QuerySucceeded:
			// Paid per Safaricom, but a query carries no receipt: never
			// record money from it. Staff confirm with the receipt.
			const why = "Safaricom's status check says paid, but no receipt has arrived; check the M-Pesa portal"
			applied, err := s.applySweep(ctx, p.ID, store.StkPushStatusSent, func(ctx context.Context, q *store.Queries, cur Push) error {
				_, err := s.reviewPushTx(ctx, q, cur, optStr(truncate(qr.ResultCode, 32)), qr.ResultDesc, failNoReceipt, why)
				return err
			})
			if err != nil || !applied {
				return res, err
			}
			s.alarm(ctx, "STK status check says paid but no receipt arrived; transaction needs staff review", attrs...)
			res.NeedsReview = 1
			return res, nil

		case qerr == nil && qr.State == mpesa.QueryFailed:
			code := truncate(qr.ResultCode, 32)
			applied, err := s.applySweep(ctx, p.ID, store.StkPushStatusSent, func(ctx context.Context, q *store.Queries, cur Push) error {
				_, err := s.failPushTx(ctx, q, cur, optStr(code), qr.ResultDesc, code, qr.ResultDesc)
				return err
			})
			if err != nil || !applied {
				return res, err
			}
			s.o.Logger.InfoContext(ctx, "stk push failed (status check)", append(attrs, slog.String("result_code", code))...)
			res.Failed = 1
			return res, nil
		}

		// Still pending, or the query itself failed: ask again later, or
		// give up to staff review.
		if now.Sub(p.CreatedAt) >= s.o.GiveUpAfter {
			const why = "No definite answer from Safaricom; check the M-Pesa portal"
			applied, err := s.applySweep(ctx, p.ID, store.StkPushStatusSent, func(ctx context.Context, q *store.Queries, cur Push) error {
				_, err := s.reviewPushTx(ctx, q, cur, nil, why, failNoAnswer, why)
				return err
			})
			if err != nil || !applied {
				return res, err
			}
			s.alarm(ctx, "STK push has no definite answer; transaction needs staff review", attrs...)
			res.NeedsReview = 1
			return res, nil
		}
		if qerr != nil && res.Queried == 1 {
			s.o.Logger.WarnContext(ctx, "stk status query failed; will retry", append(attrs, slog.String("error", qerr.Error()))...)
		}
		next := now.Add(QueryBackoff(int(p.QueryAttempts) + 1))
		_, err := s.applySweep(ctx, p.ID, store.StkPushStatusSent, func(ctx context.Context, q *store.Queries, _ Push) error {
			return q.ScheduleSTKQuery(ctx, store.ScheduleSTKQueryParams{NextQueryAt: &next, ID: p.ID})
		})
		if err != nil {
			return res, err
		}
		res.Rescheduled = 1
		return res, nil
	}
	return SweepResult{}, nil
}

// applySweep runs fn on the push in its own transaction if it is still in
// status want (a callback may have won the race; then nothing is done).
func (s *Service) applySweep(ctx context.Context, id uuid.UUID, want store.StkPushStatus, fn func(context.Context, *store.Queries, Push) error) (applied bool, err error) {
	err = db.WithTx(ctx, s.o.Pool, sweeperActor.String(), func(ctx context.Context, q *store.Queries, _ pgx.Tx) error {
		applied = false
		cur, err := q.LockSTKPush(ctx, id)
		if err != nil {
			return err
		}
		if cur.Status != want {
			return nil
		}
		applied = true
		return fn(ctx, q, cur)
	})
	return applied, err
}
