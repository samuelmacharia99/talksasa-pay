package integrationtest

import (
	"fmt"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/audit"
	"github.com/talksasa/talksasa-pay/internal/payer"
	"github.com/talksasa/talksasa-pay/internal/paymentrequest"
)

// Every audited operation (cancel, reversal, payer creation, ...) appends to
// the hash-chained audit_log. tsp_audit_chain() serialises appends with an
// advisory lock and links each row to "the row with the highest id", but the
// row's id comes from the identity sequence BEFORE the trigger takes the lock.
// Interleaving: B takes id 5, A takes id 6, A locks and commits (prev=4), B
// locks and links to 6. The next append C again picks max(id)=6 as its
// predecessor and collides with B on audit_log_prev_id_key. The UNIQUE
// constraint keeps the chain from forking, but the business operation
// (here: a product cancelling a request) fails with a raw 23505.
func TestConcurrencyAuditChainAppends(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.product("AUD").Product
	pr := e.payer(p.ID, "c1")

	const n = 200
	reqs := make([]paymentrequest.Request, n)
	for i := range reqs {
		c, err := e.createReq(p.ID, pr.ID, fmt.Sprintf("A-%d", i), fmt.Sprintf("a-%d", i), 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		reqs[i] = c.Request
	}
	errs := storm(n, func(i int) error {
		_, err := e.requests.Cancel(e.ctx, audit.Product(p.ID), p.ID, reqs[i].ID)
		return err
	})
	forks, other := 0, 0
	for i, err := range errs {
		switch {
		case err == nil:
		case isAuditFork(err):
			forks++
		default:
			other++
			t.Errorf("cancel %d: %v", i, err)
		}
	}
	// Whatever happened, the failed cancels rolled back completely and the
	// chain itself still verifies.
	if c := e.count(`SELECT count(*) FROM payment_requests WHERE product_id = $1 AND status = 'cancelled'`, p.ID); c != int64(n-forks-other) {
		t.Fatalf("cancelled rows %d, successful cancels %d", c, n-forks-other)
	}
	if c := e.count(`SELECT count(*) FROM audit_log WHERE action = 'payment_request.cancelled'`); c != int64(n-forks-other) {
		t.Fatalf("audit rows %d, successful cancels %d", c, n-forks-other)
	}
	e.checkInvariants(invariantOpts{})
	// Evidence for the mechanism: rows linked to a predecessor with a
	// higher id (B -> A above).
	inverted := e.count(`SELECT count(*) FROM audit_log WHERE prev_id > id`)
	t.Logf("%d of %d concurrent cancels failed on audit_log_prev_id_key; %d audit rows link to a higher-id predecessor", forks, n, inverted)
	// Once a row links to a higher-id predecessor, max(id) already has a
	// successor, so EVERY later append picks it again and fails: the wedge is
	// permanent. Probe with plain sequential audited operations.
	wedged := 0
	for i := 0; i < 5; i++ {
		if _, _, err := e.payers.Upsert(e.ctx, operator, p.ID, payer.UpsertParams{
			ExternalRef: fmt.Sprintf("after-%d", i), DisplayName: "After",
		}); isAuditFork(err) {
			wedged++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("after the storm, %d of 5 sequential audited operations still fail", wedged)
	if forks > 0 || wedged > 0 {
		t.Fatalf("%d of %d concurrent audited operations failed with audit_log_prev_id_key; %d of 5 later sequential ones too", forks, n, wedged)
	}
}
