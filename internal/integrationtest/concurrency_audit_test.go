package integrationtest

import (
	"fmt"
	"testing"

	"github.com/talksasa/talksasa-pay/internal/audit"
)

// Every audited operation (key issue, webhook change, reversal, ...) appends
// to the hash-chained audit_log. tsp_audit_chain() serialises appends with an
// advisory lock and draws the row id while holding it, so concurrent writers
// can never pick the same predecessor (the 000004 concurrency fix, kept in the
// baseline). Regression test: a storm of audited operations all succeed and
// the chain verifies.
func TestConcurrencyAuditChainAppends(t *testing.T) {
	e := newConcEnv(t, concPoolSize)
	p := e.app("AUD").App

	const n = 200
	errs := storm(n, func(i int) error {
		_, err := e.apps.IssueAPIKey(e.ctx, audit.CLI("storm"), p.ID, fmt.Sprintf("key-%d", i), nil)
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
			t.Errorf("issue key %d: %v", i, err)
		}
	}
	// Whatever happened, the failed operations rolled back completely and
	// the chain itself still verifies.
	if c := e.count(`SELECT count(*) FROM app_api_keys WHERE app_id = $1`, p.ID); c != int64(n-forks-other) {
		t.Fatalf("keys %d, successful issues %d", c, n-forks-other)
	}
	if c := e.count(`SELECT count(*) FROM audit_log WHERE action = 'api_key.issued' AND app_id = $1`, p.ID); c != int64(n-forks-other) {
		t.Fatalf("audit rows %d, successful issues %d", c, n-forks-other)
	}
	e.checkInvariants()
	if inverted := e.count(`SELECT count(*) FROM audit_log WHERE prev_id > id`); inverted != 0 {
		t.Fatalf("%d audit rows link to a higher-id predecessor", inverted)
	}
	// Sequential audited operations still append after the storm.
	for i := 0; i < 5; i++ {
		if _, err := e.apps.IssueAPIKey(e.ctx, operator, p.ID, fmt.Sprintf("after-%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if forks > 0 {
		t.Fatalf("%d of %d concurrent audited operations failed with audit_log_prev_id_key", forks, n)
	}
}
