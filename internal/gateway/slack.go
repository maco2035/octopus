// Package gateway implements Octopus's Slack integration (PLAN.md Phase
// 5): a slash command that triggers a run against a project's default
// PipelineDef, and a Block Kit review card — posted the moment a run
// reaches ANY AWAITING_REVIEW gate, not just a final one — whose "Approve"
// button resolves it inline via the same Scheduler.Continue path the web
// UI uses.
package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"octopus/internal/domain"
	"octopus/internal/ratelimit"
	"octopus/internal/scheduler"
	"octopus/internal/store"
)

type Gateway struct {
	Store         store.Store
	Scheduler     *scheduler.Scheduler
	SigningSecret string
	WebBaseURL    string // used to build "Open in web UI" links, e.g. https://octopus.example.com
	HTTPClient    *http.Client

	// CommandLimiter defaults to a sane value if left nil — PLAN.md
	// Phase 8 calls out /api/slack/command by name for rate limiting.
	// Keyed by team_id (falls back to remote address if Slack ever omits
	// it), so one noisy workspace can't exhaust another's budget.
	CommandLimiter *ratelimit.Limiter

	mu           sync.Mutex
	responseURLs map[string]string // runID -> the Slack response_url to post follow-up cards to
}

func New(st store.Store, sched *scheduler.Scheduler, signingSecret, webBaseURL string) *Gateway {
	g := &Gateway{
		Store: st, Scheduler: sched, SigningSecret: signingSecret, WebBaseURL: webBaseURL,
		HTTPClient: http.DefaultClient, responseURLs: map[string]string{},
		CommandLimiter: ratelimit.New(20, time.Minute),
	}
	sched.OnSettled = g.onSettled
	return g
}

// RegisterRoutes adds Octopus's two Slack endpoints to an existing mux —
// called from main.go alongside web.Server.Routes() so both share one
// server and one listener. Slack's own signature verifies the caller
// instead of session login, per PLAN.md Key Design Decision 22's carve-out
// for service-to-service traffic.
func (g *Gateway) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/slack/command", g.verifySignature(g.CommandLimiter.Middleware(commandRateLimitKey, g.handleCommand)))
	mux.HandleFunc("POST /api/slack/action", g.verifySignature(g.handleAction))
}

// commandRateLimitKey keys by Slack's team_id where available (a form
// field, so it's only readable after verifySignature has already accepted
// the body) so the limit is per-workspace rather than per-IP — Slack
// itself often proxies from a shared IP range, which would otherwise lump
// unrelated workspaces into one budget.
func commandRateLimitKey(r *http.Request) string {
	if teamID := r.FormValue("team_id"); teamID != "" {
		return teamID
	}
	return ratelimit.RemoteAddrKey(r)
}

// handleCommand implements a slash command shaped "/octopus <project_id>
// <ticket_id...>" — looks up the project's default PipelineDef and starts a
// run against it, acknowledging immediately (Slack requires a response
// within 3s) and remembering response_url so onSettled can post a review
// card there later, however long the run takes to reach one.
func (g *Gateway) handleCommand(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	text := strings.TrimSpace(r.FormValue("text"))
	responseURL := r.FormValue("response_url")

	parts := strings.SplitN(text, " ", 2)
	if len(parts) < 2 || parts[0] == "" || strings.TrimSpace(parts[1]) == "" {
		respondEphemeral(w, "Usage: /octopus <project_id> <ticket_id>")
		return
	}
	projectID, ticketID := parts[0], strings.TrimSpace(parts[1])
	// This is the least-trusted place ticket_id enters the system — any
	// workspace member who can run a slash command, not just the logged-in
	// admin — and it ends up as a bare positional argument to git
	// (checkout/push/merge) via the branch name. See
	// domain.ValidTicketID's doc comment for why a leading "-" matters.
	if !domain.ValidTicketID(ticketID) {
		respondEphemeral(w, "ticket_id may only contain letters, digits, '.', '_', '-', and must start with a letter or digit.")
		return
	}

	project, err := g.Store.LoadProject(r.Context(), projectID)
	if errors.Is(err, store.ErrNotFound) {
		respondEphemeral(w, fmt.Sprintf("Unknown project %q.", projectID))
		return
	}
	if err != nil {
		slog.Error("gateway: looking up project", "project_id", projectID, "error", err)
		respondEphemeral(w, "Error looking up the project — check the server log.")
		return
	}
	if project.DefaultPipelineDefID == "" {
		respondEphemeral(w, fmt.Sprintf("Project %q has no default pipeline set yet — set one from the web UI.", projectID))
		return
	}

	state, err := g.Scheduler.StartRun(r.Context(), projectID, project.DefaultPipelineDefID, ticketID)
	if err != nil {
		slog.Error("gateway: starting run", "project_id", projectID, "ticket_id", ticketID, "error", err)
		respondEphemeral(w, "Failed to start the run — check the server log.")
		return
	}

	if responseURL != "" {
		g.mu.Lock()
		g.responseURLs[state.RunID] = responseURL
		g.mu.Unlock()
	}

	respondEphemeral(w, fmt.Sprintf("Started run `%s` for ticket `%s`. I'll post here again if it needs review.", state.RunID, ticketID))
}

// actionValue is what a review card's "Approve" button carries as its
// value — just enough to resolve the review without a server-side lookup
// (Slack round-trips button values verbatim).
type actionValue struct {
	RunID       string `json:"run_id"`
	ActionToken string `json:"action_token"`
}

type slackInteractionPayload struct {
	Type    string `json:"type"`
	Actions []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
	ResponseURL string `json:"response_url"`
}

// handleAction resolves a Block Kit button click. Only "approve" is wired
// to a button (per PLAN.md Phase 5: reject/edit stay web-UI-only) — the
// underlying store.ResolveReview marks the action_token used, so a
// double-click naturally surfaces as ErrStaleActionToken, not a double
// apply.
func (g *Gateway) handleAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}

	var payload slackInteractionPayload
	if err := json.Unmarshal([]byte(r.FormValue("payload")), &payload); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest)
		return
	}
	if len(payload.Actions) == 0 {
		http.Error(w, "no action", http.StatusBadRequest)
		return
	}
	action := payload.Actions[0]

	var val actionValue
	if err := json.Unmarshal([]byte(action.Value), &val); err != nil {
		http.Error(w, "bad action value", http.StatusBadRequest)
		return
	}

	if payload.ResponseURL != "" {
		g.mu.Lock()
		g.responseURLs[val.RunID] = payload.ResponseURL
		g.mu.Unlock()
	}

	err := g.Scheduler.Continue(r.Context(), val.RunID, val.ActionToken, action.ActionID == "approve", nil)
	if errors.Is(err, store.ErrStaleActionToken) {
		respondReplace(w, "This review was already resolved.")
		return
	}
	if err != nil {
		// Same reasoning as internal/web's serverError: don't hand
		// internal error detail back over the wire (here, into a Slack
		// channel everyone in it can read) — log it, respond generically.
		slog.Error("gateway: resolving review", "run_id", val.RunID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	respondReplace(w, fmt.Sprintf("Approved run `%s`. Continuing…", val.RunID))
}

// onSettled is the Scheduler.OnSettled hook: whenever a run this process
// drove stops on its own, decide whether Slack needs to hear about it.
func (g *Gateway) onSettled(state *domain.PipelineState) {
	g.mu.Lock()
	url, ok := g.responseURLs[state.RunID]
	g.mu.Unlock()
	if !ok {
		return
	}

	switch state.Status {
	case domain.StatusAwaitingReview:
		g.post(url, g.reviewCard(state))
	case domain.StatusCompleted, domain.StatusFailed, domain.StatusRejected, domain.StatusBlocked:
		g.post(url, slackMessage{
			ReplaceOriginal: true,
			Text:            fmt.Sprintf("Run `%s` (ticket `%s`) finished: *%s*", state.RunID, state.TicketID, state.Status),
		})
		g.mu.Lock()
		delete(g.responseURLs, state.RunID)
		g.mu.Unlock()
	}
}

type slackMessage struct {
	ResponseType    string           `json:"response_type,omitempty"`
	ReplaceOriginal bool             `json:"replace_original,omitempty"`
	Text            string           `json:"text,omitempty"`
	Blocks          []map[string]any `json:"blocks,omitempty"`
}

func (g *Gateway) reviewCard(state *domain.PipelineState) slackMessage {
	val, _ := json.Marshal(actionValue{RunID: state.RunID, ActionToken: state.ActionToken})
	elements := []map[string]any{
		{
			"type":      "button",
			"text":      map[string]any{"type": "plain_text", "text": "Approve"},
			"style":     "primary",
			"action_id": "approve",
			"value":     string(val),
		},
	}
	if g.WebBaseURL != "" {
		elements = append(elements, map[string]any{
			"type": "button",
			"text": map[string]any{"type": "plain_text", "text": "Open in web UI"},
			"url":  g.WebBaseURL + "/runs/" + state.RunID,
		})
	}

	return slackMessage{
		Text: fmt.Sprintf("Run %s awaiting review at %s", state.RunID, state.PendingNodeID),
		Blocks: []map[string]any{
			{
				"type": "section",
				"text": map[string]any{
					"type": "mrkdwn",
					"text": fmt.Sprintf("*Run `%s`* (ticket `%s`) is awaiting review at *%s*", state.RunID, state.TicketID, state.PendingNodeID),
				},
			},
			{"type": "actions", "elements": elements},
		},
	}
}

func (g *Gateway) post(url string, msg slackMessage) {
	b, err := json.Marshal(msg)
	if err != nil {
		slog.Error("gateway: marshaling slack message", "error", err)
		return
	}
	resp, err := g.HTTPClient.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		slog.Error("gateway: posting to slack response_url", "error", err)
		return
	}
	resp.Body.Close()
}

func respondEphemeral(w http.ResponseWriter, text string) {
	writeSlackJSON(w, slackMessage{ResponseType: "ephemeral", Text: text})
}

func respondReplace(w http.ResponseWriter, text string) {
	writeSlackJSON(w, slackMessage{ReplaceOriginal: true, Text: text})
}

func writeSlackJSON(w http.ResponseWriter, msg slackMessage) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(msg)
}
