package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// A gateway that routes /api/* to this service passes the path through:
// every route must answer the same under /api, callbacks included, and
// /api/health is the readiness probe.
func TestAPIPrefix(t *testing.T) {
	e := newCBEnv(t, nil)
	expectAck(t, e.post("/api/cb/mpesa/"+cbToken+"/stk", stkBody, fromSafaricom()))
	if n := len(e.stk.storedCallbacks()); n != 1 {
		t.Fatalf("stored %d callbacks via /api", n)
	}
	if r := serve(e.h, http.MethodGet, "/api/healthz", nil); r.code != 200 || !strings.Contains(r.body, `"ok"`) {
		t.Fatalf("/api/healthz: %d %s", r.code, r.body)
	}
	// Only an exact /api segment is a prefix.
	for _, p := range []string{"/api", "/api/", "/apix/healthz", "/api/api/healthz"} {
		r := serve(e.h, http.MethodGet, p, nil)
		if r.code == 200 {
			t.Fatalf("%s: unexpected 200 %s", p, r.body)
		}
	}
	// Unprefixed paths still work.
	if r := serve(e.h, http.MethodGet, "/healthz", nil); r.code != 200 {
		t.Fatalf("/healthz: %d", r.code)
	}
}
