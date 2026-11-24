package adminapi

import (
	"net/http"

	"github.com/talksasa/talksasa-pay/internal/platform/httpx"
)

// Phase 2 placeholders: implemented when the STK service is wired.

func (h *handlers) resolveTransaction(w http.ResponseWriter, r *http.Request) {
	httpx.WriteProblem(w, r, httpx.NewProblem(http.StatusServiceUnavailable, "mpesa_unavailable", "Not available yet."))
}

func (h *handlers) listSTKPushes(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": []any{}})
}

func (h *handlers) listMpesaCallbacks(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": []any{}})
}
