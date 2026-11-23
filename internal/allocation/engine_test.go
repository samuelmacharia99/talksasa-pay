package allocation

import (
	"errors"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/money"
)

var t0 = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func req(id string, amount, paid money.KES, ageDays int) Request {
	return Request{
		ID:        uuid.MustParse(id),
		Amount:    amount,
		Paid:      paid,
		Status:    DeriveStatus(amount, paid, StatusOpen),
		CreatedAt: t0.AddDate(0, 0, ageDays),
	}
}

const (
	idA = "00000000-0000-0000-0000-00000000000a"
	idB = "00000000-0000-0000-0000-00000000000b"
	idC = "00000000-0000-0000-0000-00000000000c"
)

func mustPlan(t *testing.T, amount money.KES, target string, rs ...Request) Plan {
	t.Helper()
	tgt := uuid.Nil
	if target != "" {
		tgt = uuid.MustParse(target)
	}
	p, err := PlanPayment(amount, tgt, rs)
	if err != nil {
		t.Fatalf("PlanPayment: %v", err)
	}
	if p.Total() != amount {
		t.Fatalf("plan total %d != amount %d", p.Total(), amount)
	}
	return p
}

func assertLines(t *testing.T, got Plan, want ...Line) {
	t.Helper()
	if len(got.Lines) != len(want) {
		t.Fatalf("lines = %+v, want %+v", got.Lines, want)
	}
	for i := range want {
		if got.Lines[i] != want[i] {
			t.Fatalf("line %d = %+v, want %+v (all: %+v)", i, got.Lines[i], want[i], got.Lines)
		}
	}
}

func L(id string, amt money.KES) Line {
	if id == "" {
		return Line{RequestID: uuid.Nil, Amount: amt}
	}
	return Line{RequestID: uuid.MustParse(id), Amount: amt}
}

func TestExactPayment(t *testing.T) {
	p := mustPlan(t, 1000, "", req(idA, 1000, 0, 0))
	assertLines(t, p, L(idA, 1000))
	if p.CreditDelta() != 0 {
		t.Fatalf("credit delta = %d", p.CreditDelta())
	}
}

func TestOverpaymentBecomesCredit(t *testing.T) {
	p := mustPlan(t, 1500, "", req(idA, 1000, 0, 0))
	assertLines(t, p, L(idA, 1000), L("", 500))
	if p.CreditDelta() != 500 {
		t.Fatalf("credit delta = %d", p.CreditDelta())
	}
}

func TestUnderpaymentLeavesPartiallyPaid(t *testing.T) {
	r := req(idA, 1000, 0, 0)
	p := mustPlan(t, 400, "", r)
	assertLines(t, p, L(idA, 400))
	if s := DeriveStatus(r.Amount, r.Paid+400, r.Status); s != StatusPartiallyPaid {
		t.Fatalf("status = %s", s)
	}
}

func TestPartialThenBalance(t *testing.T) {
	r := req(idA, 1000, 400, 0)
	p := mustPlan(t, 600, "", r)
	assertLines(t, p, L(idA, 600))
	if s := DeriveStatus(r.Amount, r.Paid+600, r.Status); s != StatusPaid {
		t.Fatalf("status = %s", s)
	}
}

func TestOldestFirstAcrossRequests(t *testing.T) {
	// Given out of order on purpose.
	p := mustPlan(t, 1700, "",
		req(idC, 500, 0, 2),
		req(idA, 1000, 0, 0),
		req(idB, 500, 0, 1),
	)
	assertLines(t, p, L(idA, 1000), L(idB, 500), L(idC, 200))
}

func TestTargetFirstThenOldestThenCredit(t *testing.T) {
	p := mustPlan(t, 2200, idC,
		req(idA, 1000, 0, 0),
		req(idB, 500, 0, 1),
		req(idC, 500, 0, 2),
	)
	assertLines(t, p, L(idC, 500), L(idA, 1000), L(idB, 500), L("", 200))
}

func TestTargetAlreadyPaidFallsThroughToOldest(t *testing.T) {
	p := mustPlan(t, 300, idB, req(idA, 1000, 0, 0), req(idB, 500, 500, 1))
	assertLines(t, p, L(idA, 300))
}

func TestTargetCancelledOrExpiredIsSkipped(t *testing.T) {
	for _, st := range []Status{StatusCancelled, StatusExpired} {
		dead := Request{ID: uuid.MustParse(idB), Amount: 500, Status: st, CreatedAt: t0}
		p := mustPlan(t, 800, idB, dead, req(idA, 1000, 0, 5))
		assertLines(t, p, L(idA, 800))
	}
}

func TestNoOpenRequestsAllToCredit(t *testing.T) {
	p := mustPlan(t, 750, "")
	assertLines(t, p, L("", 750))
}

func TestUnknownTargetIgnored(t *testing.T) {
	p := mustPlan(t, 100, idC, req(idA, 1000, 0, 0))
	assertLines(t, p, L(idA, 100))
}

func TestTieOnCreatedAtBrokenByID(t *testing.T) {
	a, b := req(idA, 100, 0, 0), req(idB, 100, 0, 0)
	p := mustPlan(t, 150, "", b, a)
	assertLines(t, p, L(idA, 100), L(idB, 50))
}

func TestRejectsBadInput(t *testing.T) {
	if _, err := PlanPayment(0, uuid.Nil, nil); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("zero amount: %v", err)
	}
	if _, err := PlanPayment(-5, uuid.Nil, nil); !errors.Is(err, ErrNonPositiveAmount) {
		t.Fatalf("negative amount: %v", err)
	}
	over := Request{ID: uuid.MustParse(idA), Amount: 100, Paid: 150, Status: StatusPaid, CreatedAt: t0}
	if _, err := PlanPayment(10, uuid.Nil, []Request{over}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("over-allocated request: %v", err)
	}
	wrongStatus := Request{ID: uuid.MustParse(idA), Amount: 100, Paid: 100, Status: StatusOpen, CreatedAt: t0}
	if _, err := PlanPayment(10, uuid.Nil, []Request{wrongStatus}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("status mismatch: %v", err)
	}
	if _, err := PlanPayment(10, uuid.Nil, []Request{req(idA, 1, 0, 0), req(idA, 1, 0, 0)}); !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestCreditApplication(t *testing.T) {
	cases := []struct {
		name   string
		credit money.KES
		r      Request
		want   []Line
	}{
		{"full cover", 1500, req(idA, 1000, 0, 0), []Line{L(idA, 1000), L("", -1000)}},
		{"partial cover", 300, req(idA, 1000, 0, 0), []Line{L(idA, 300), L("", -300)}},
		{"exact", 1000, req(idA, 1000, 0, 0), []Line{L(idA, 1000), L("", -1000)}},
		{"no credit", 0, req(idA, 1000, 0, 0), nil},
		{"already paid", 500, req(idA, 1000, 1000, 0), nil},
		{"cancelled", 500, Request{ID: uuid.MustParse(idA), Amount: 10, Status: StatusCancelled, CreatedAt: t0}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, err := PlanCreditApplication(tc.credit, tc.r)
			if err != nil {
				t.Fatal(err)
			}
			if p.Total() != 0 {
				t.Fatalf("credit application must net to zero, got %d", p.Total())
			}
			assertLines(t, p, tc.want...)
		})
	}
	if _, err := PlanCreditApplication(-1, req(idA, 10, 0, 0)); err == nil {
		t.Fatal("negative credit accepted")
	}
}

func TestReversal(t *testing.T) {
	orig := []Line{L(idA, 1000), L(idB, 500), L("", 200)}
	p, err := PlanReversal(orig)
	if err != nil {
		t.Fatal(err)
	}
	assertLines(t, p, L("", -200), L(idB, -500), L(idA, -1000))
	if p.Total() != -1700 {
		t.Fatalf("total = %d", p.Total())
	}
	if _, err := PlanReversal(nil); !errors.Is(err, ErrNothingToReverse) {
		t.Fatalf("empty reversal: %v", err)
	}
}

func TestDeriveStatus(t *testing.T) {
	cases := []struct {
		amount, paid money.KES
		cur, want    Status
	}{
		{100, 0, StatusOpen, StatusOpen},
		{100, 1, StatusOpen, StatusPartiallyPaid},
		{100, 99, StatusPartiallyPaid, StatusPartiallyPaid},
		{100, 100, StatusPartiallyPaid, StatusPaid},
		{100, 0, StatusPaid, StatusOpen}, // full reversal reopens
		{100, 0, StatusCancelled, StatusCancelled},
		{100, 0, StatusExpired, StatusExpired},
	}
	for _, c := range cases {
		if got := DeriveStatus(c.amount, c.paid, c.cur); got != c.want {
			t.Errorf("DeriveStatus(%d,%d,%s) = %s, want %s", c.amount, c.paid, c.cur, got, c.want)
		}
	}
}

// TestPropertyInvariants checks, over many random scenarios, that a plan
// always totals the payment, never over-allocates a request, never touches a
// non-payable request, only credits after every payable request is full, and
// pays the target first.
func TestPropertyInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 2026))
	statuses := []Status{StatusOpen, StatusPartiallyPaid, StatusPaid, StatusCancelled, StatusExpired}
	for iter := 0; iter < 5000; iter++ {
		n := rng.IntN(6)
		rs := make([]Request, 0, n)
		byID := map[uuid.UUID]Request{}
		for i := 0; i < n; i++ {
			amt := money.KES(1 + rng.IntN(5000))
			st := statuses[rng.IntN(len(statuses))]
			var paid money.KES
			switch st {
			case StatusPartiallyPaid:
				if amt == 1 {
					st = StatusOpen
				} else {
					paid = money.KES(1 + rng.IntN(int(amt-1)))
				}
			case StatusPaid:
				paid = amt
			}
			r := Request{ID: uuid.New(), Amount: amt, Paid: paid, Status: st, CreatedAt: t0.Add(time.Duration(rng.IntN(1000)) * time.Minute)}
			rs = append(rs, r)
			byID[r.ID] = r
		}
		target := uuid.Nil
		if n > 0 && rng.IntN(2) == 0 {
			target = rs[rng.IntN(n)].ID
		}
		amount := money.KES(1 + rng.IntN(20000))

		p, err := PlanPayment(amount, target, rs)
		if err != nil {
			t.Fatalf("iter %d: %v", iter, err)
		}
		if p.Total() != amount {
			t.Fatalf("iter %d: total %d != %d", iter, p.Total(), amount)
		}
		var payableOutstanding money.KES
		for _, r := range rs {
			if r.Status.Payable() {
				payableOutstanding += r.Outstanding()
			}
		}
		for i, l := range p.Lines {
			if l.Amount <= 0 {
				t.Fatalf("iter %d: non-positive line %+v", iter, l)
			}
			if l.IsCredit() {
				if i != len(p.Lines)-1 {
					t.Fatalf("iter %d: credit line not last", iter)
				}
				if amount-l.Amount != payableOutstanding {
					t.Fatalf("iter %d: credited while requests still owed", iter)
				}
				continue
			}
			r := byID[l.RequestID]
			if !r.Status.Payable() {
				t.Fatalf("iter %d: allocated to %s request", iter, r.Status)
			}
			if l.Amount > r.Outstanding() {
				t.Fatalf("iter %d: over-allocated request", iter)
			}
		}
		if target != uuid.Nil && byID[target].Status.Payable() && len(p.Lines) > 0 && p.Lines[0].RequestID != target {
			t.Fatalf("iter %d: payable target not paid first", iter)
		}
	}
}
