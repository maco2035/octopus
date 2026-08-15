package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"octopus/internal/domain"
)

// newRunnerToken generates the raw token shown to the operator exactly
// once — only its bcrypt hash is ever persisted (PLAN.md Phase 7: "Runner
// tokens are ... hashed at rest, checked on connect"), the same treatment
// as a user's password.
func newRunnerToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return "octor_" + hex.EncodeToString(b)
}

func (s *Server) handleRunnersList(w http.ResponseWriter, r *http.Request) {
	runners, err := s.Store.ListRunners(r.Context())
	if err != nil {
		serverError(w, r, err)
		return
	}

	connected := map[string]bool{}
	if s.Hub != nil {
		for _, id := range s.Hub.ConnectedRunnerIDs() {
			connected[id] = true
		}
	}

	type row struct {
		*domain.Runner
		Connected bool
	}
	rows := make([]row, 0, len(runners))
	for _, rn := range runners {
		rows = append(rows, row{Runner: rn, Connected: connected[rn.ID]})
	}

	s.renderWithUser(w, r, "runners.html", map[string]any{"Runners": rows})
}

func (s *Server) handleCreateRunner(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	var projectIDs []string
	for _, p := range strings.Split(r.FormValue("project_ids"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			projectIDs = append(projectIDs, p)
		}
	}

	token := newRunnerToken()
	hash, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.DefaultCost)
	if err != nil {
		serverError(w, r, err)
		return
	}

	rn := &domain.Runner{
		ID: newSessionToken(), Name: name, TokenHash: string(hash), ProjectIDs: projectIDs, LastSeen: time.Time{},
	}
	if err := s.Store.SaveRunner(r.Context(), rn); err != nil {
		serverError(w, r, err)
		return
	}

	// The raw token is shown exactly once, right now — it can't be
	// recovered later, only reissued as a brand-new runner.
	s.renderWithUser(w, r, "runner_token.html", map[string]any{"Runner": rn, "Token": token})
}

// handleRevokeRunner deletes a runner's registration and, if it's
// currently connected, disconnects it immediately — revocation that
// actually takes effect right away, not just "won't be able to
// reconnect eventually" (PLAN.md §8: runner tokens are credentials,
// treat them like ones that can be rotated/revoked).
func (s *Server) handleRevokeRunner(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.Hub != nil {
		s.Hub.DisconnectRunner(id)
	}
	if err := s.Store.DeleteRunner(r.Context(), id); err != nil {
		serverError(w, r, err, "runner_id", id)
		return
	}
	http.Redirect(w, r, "/runners", http.StatusSeeOther)
}
