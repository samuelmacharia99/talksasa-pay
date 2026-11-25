package appapi

import (
	"bufio"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/talksasa/talksasa-pay/internal/apiview"
	"github.com/talksasa/talksasa-pay/internal/store"
	"github.com/talksasa/talksasa-pay/internal/testutil/specshape"
)

// The JSON resources the handlers render have exactly the fields the spec
// documents (no undocumented field can reach an app).
func TestResponseShapesMatchSpec(t *testing.T) {
	const spec = "../../api/app.openapi.yaml"
	now := time.Now()
	desc := "Hosting"
	c := store.Charge{
		ID: uuid.New(), AppID: uuid.New(), Reference: "INV-1", Description: &desc, Currency: "KES", Amount: 100,
		Metadata: []byte(`{"a":1}`), AccountNumber: "HOST-1042", Status: store.ChargeStatusPending,
		HostedToken: "tok", IdempotencyKey: "secret-key", ExpiresAt: now, CreatedAt: now, UpdatedAt: now,
	}
	push := store.StkPush{ID: uuid.New(), ChargeID: c.ID, Phone: "254712345678", Status: store.StkPushStatusSent, CreatedAt: now, UpdatedAt: now}
	v := apiview.View{PayBaseURL: "https://pay.test/", Paybill: "174379"}
	ch := v.Charge(c, &push)
	specshape.Match(t, spec, "Charge", ch)
	specshape.Match(t, spec, "Paybill", ch.Paybill)
	specshape.Match(t, spec, "STKPush", ch.LatestSTKPush)
	specshape.Match(t, spec, "Transaction", v.Transaction(apiview.TransactionRow{ID: uuid.New()}))
	specshape.Match(t, spec, "WhoAmI", whoAmIJSON{})
	specshape.Match(t, spec, "Event", apiview.Event{})
	// A charge that is no longer payable: paybill and latest_stk_push are
	// still present (null).
	c.Status = store.ChargeStatusCancelled
	specshape.Match(t, spec, "Charge", v.Charge(c, nil))
}

// api/app.openapi.yaml is the app contract. This test fails when a
// route exists in Go but not in the spec, or vice versa.
func TestOpenAPISpecMatchesRoutes(t *testing.T) {
	f, err := os.Open("../../api/app.openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	pathRe := regexp.MustCompile(`^  (/[^:]*):\s*$`)
	methodRe := regexp.MustCompile(`^    (get|post|put|patch|delete):`)
	spec := map[string]bool{}
	var cur string
	sc := bufio.NewScanner(f)
	inPaths := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && !strings.HasPrefix(line, " "):
			inPaths = false
		case inPaths && pathRe.MatchString(line):
			cur = pathRe.FindStringSubmatch(line)[1]
		case inPaths && cur != "" && methodRe.MatchString(line):
			spec[strings.ToUpper(methodRe.FindStringSubmatch(line)[1])+" "+cur] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(spec) == 0 {
		t.Fatal("parsed no paths from the spec")
	}
	code := map[string]bool{}
	for _, r := range Routes(Deps{}) {
		code[r.Method+" "+r.Pattern] = true
	}
	for k := range code {
		if !spec[k] {
			t.Errorf("route %s is not documented in api/app.openapi.yaml", k)
		}
	}
	for k := range spec {
		if !code[k] {
			t.Errorf("api/app.openapi.yaml documents %s, which the router does not serve", k)
		}
	}
}
