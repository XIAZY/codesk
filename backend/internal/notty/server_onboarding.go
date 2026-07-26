package notty

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleGetOnboarding returns the authenticated account's completed onboarding items. The
// frontend reads this key-set to decide whether to show the flow (not all milestones present
// and no dismiss row) and to draw the ✓ ticks from the same set.
func (s *Server) handleGetOnboarding(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFromContext(r.Context())
	if !ok || auth.AccountID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	completions, err := listOnboardingCompletions(s.sqlDB(), auth.AccountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"completions": completions})
}

type onboardingCompletionRequest struct {
	ItemKey string `json:"itemKey"`
}

// handleCreateOnboardingCompletion records a client-driven onboarding completion for the
// authenticated account and returns the updated key-set. Only client-insertable items are
// accepted — the two server-witnessed milestones (daemon connected, agent run started) are
// inserted by the backend at its own seams and are rejected here, so a client can't forge them.
func (s *Server) handleCreateOnboardingCompletion(w http.ResponseWriter, r *http.Request) {
	auth, ok := authFromContext(r.Context())
	if !ok || auth.AccountID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req onboardingCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	itemKey := strings.TrimSpace(req.ItemKey)
	if !isClientOnboardingItemKey(itemKey) {
		writeError(w, http.StatusBadRequest, "unknown or non-client onboarding item")
		return
	}
	if err := insertOnboardingCompletion(s.sqlDB(), auth.AccountID, itemKey); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	completions, err := listOnboardingCompletions(s.sqlDB(), auth.AccountID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"completions": completions})
}
