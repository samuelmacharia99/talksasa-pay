package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDAcceptsValidRejectsInjection(t *testing.T) {
	var seen string
	h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { seen = RequestIDFrom(r.Context()) }))
	for in, keep := range map[string]bool{"abc-123_XYZ.9": true, "short": false, "bad id\nInjected: 1": false, "": false} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set(RequestIDHeader, in)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if keep != (seen == in) || seen == "" || w.Header().Get(RequestIDHeader) != seen {
			t.Errorf("in=%q seen=%q", in, seen)
		}
	}
}

func TestRecoverReturnsProblem(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	h := RequestID(Recover(logger)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != 500 || !strings.Contains(w.Body.String(), "internal_error") || strings.Contains(w.Body.String(), "boom") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestMaxBody(t *testing.T) {
	h := MaxBody(10)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
		}
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("x", 11))))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("code %d", w.Code)
	}
}
