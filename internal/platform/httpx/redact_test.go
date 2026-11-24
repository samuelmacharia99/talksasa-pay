package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	tlog "github.com/talksasa/talksasa-pay/internal/platform/log"
)

func TestRedactSegments(t *testing.T) {
	red := RedactSegments("/cb/mpesa/*", "/cb/**", "/public/v1/pay/*")
	cases := map[string]string{
		"/cb/mpesa/SECRETtoken123/stk":              "/cb/mpesa/[redacted]/stk",
		"/cb/mpesa/SECRETtoken123/c2b/confirmation": "/cb/mpesa/[redacted]/c2b/confirmation",
		"/cb/mpesa/SECRETtoken123":                  "/cb/mpesa/[redacted]",
		"/cb/mpesa/SECRETtoken123/":                 "/cb/mpesa/[redacted]",
		"//cb/mpesa/SECRETtoken123/stk":             "/cb/mpesa/[redacted]/stk",
		"/CB/Mpesa/SECRETtoken123/stk":              "/CB/Mpesa/[redacted]/stk",
		"/cb/mpesa":                                 "/cb/mpesa",
		"/cb/other/SECRETtoken123/x":                "/cb/[redacted]",
		"/cb//SECRETtoken123":                       "/cb/[redacted]",
		"/cb/mpesa//SECRETtoken123/stk":             "/cb/mpesa/[redacted]/stk",
		"/public/v1/pay/abcdefgh/stk":               "/public/v1/pay/[redacted]/stk",
		"/v1/payment-requests":                      "/v1/payment-requests",
		"/":                                         "/",
		"/healthz":                                  "/healthz",
	}
	for in, want := range cases {
		if got := red(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestInPrefixes(t *testing.T) {
	ps := []netip.Prefix{netip.MustParsePrefix("196.201.214.0/24"), netip.MustParsePrefix("2001:db8::/32")}
	for ip, want := range map[string]bool{
		"196.201.214.200":        true,
		"::ffff:196.201.214.200": true, // v4-mapped
		"196.201.215.1":          false,
		"2001:db8::1":            true,
		"10.0.0.1":               false,
	} {
		if got := InPrefixes(netip.MustParseAddr(ip), ps); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
	if InPrefixes(netip.Addr{}, ps) || InPrefixes(netip.MustParseAddr("10.0.0.1"), nil) {
		t.Error("invalid ip or empty list must not match")
	}
}

// The access log (and any log line written by the handler with the request
// context) shows the redacted path, never the raw one.
func TestAccessLogRedactsPath(t *testing.T) {
	var buf bytes.Buffer
	logger := tlog.New(&buf, tlog.Options{Level: slog.LevelDebug})
	const secret = "s3cretTOKENs3cretTOKENs3cretTOKEN"
	r := chi.NewRouter()
	r.Use(RequestID, ClientIP(nil), AccessLog(logger, nil, LogPath(RedactSegments("/cb/mpesa/*"))), Recover(logger))
	r.Post("/cb/mpesa/{token}/stk", func(w http.ResponseWriter, r *http.Request) {
		logger.WarnContext(r.Context(), "inner log")
		panic("boom")
	})
	r.Get("/plain", func(w http.ResponseWriter, r *http.Request) {})
	for _, p := range []string{"/cb/mpesa/" + secret + "/stk", "/cb/mpesa/" + secret + "/nope", "/plain?token=" + secret} {
		method := http.MethodPost
		if strings.HasPrefix(p, "/plain") {
			method = http.MethodGet
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, p, nil))
	}
	logs := buf.String()
	if strings.Contains(logs, secret) {
		t.Fatalf("logs contain the secret:\n%s", logs)
	}
	var paths, inner int
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatal(err)
		}
		switch m["msg"] {
		case "http request":
			paths++
			want := "/cb/mpesa/[redacted]/stk"
			switch m["route"] {
			case "unmatched":
				want = "/cb/mpesa/[redacted]/nope"
			case "/plain":
				want = "/plain"
			}
			if m["path"] != want {
				t.Errorf("access log path %v, want %s (%s)", m["path"], want, line)
			}
		case "inner log", "panic in handler":
			inner++
			if m["path"] != "/cb/mpesa/[redacted]/stk" {
				t.Errorf("handler log path %v: %s", m["path"], line)
			}
		}
	}
	if paths != 3 || inner != 2 {
		t.Fatalf("access lines %d, handler lines %d:\n%s", paths, inner, logs)
	}
}

// Without LogPath the access log has no path attribute at all.
func TestAccessLogDefaultHasNoPath(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := AccessLog(logger, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x/secret", nil))
	if strings.Contains(buf.String(), "secret") || strings.Contains(buf.String(), `"path"`) {
		t.Fatal(buf.String())
	}
}
