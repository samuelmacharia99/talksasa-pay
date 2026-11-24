package productapi

import "net/http"

// sendSTKPush: POST /payment-requests/{id}/stk-push (Phase 2).
// Placeholder until the M-Pesa service is wired: M-Pesa unavailable.
func (h *handlers) sendSTKPush(w http.ResponseWriter, r *http.Request) {
	problem(w, r, http.StatusServiceUnavailable, "mpesa_unavailable", "M-Pesa payment prompts are not available.")
}

// getSTKPush: GET /stk-pushes/{id} (Phase 2). Placeholder.
func (h *handlers) getSTKPush(w http.ResponseWriter, r *http.Request) {
	problem(w, r, http.StatusNotFound, "not_found", "No such resource.")
}
