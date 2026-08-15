// Package web is the server-rendered UI (PLAN.md Phase 4): Go templates +
// a couple of small hand-rolled vanilla-JS files, gated end to end by
// session login. internal/web/routes.go equivalent lives in this file as
// Server.Routes — every page route is wrapped in requireLogin except
// /login itself, /healthz, and static assets.
package web

import (
	"net/http"
	"time"

	"octopus/internal/agents"
	"octopus/internal/ratelimit"
	"octopus/internal/scheduler"
	"octopus/internal/store"
)

// ConnectedRunners is the small slice of runnerhub.Hub the web package
// needs — just enough to show which registered runners are online right
// now on the Runners admin page and to disconnect one immediately on
// revocation, without web importing runnerhub itself.
type ConnectedRunners interface {
	ConnectedRunnerIDs() []string
	DisconnectRunner(id string)
}

type Server struct {
	Store     store.Store
	Scheduler *scheduler.Scheduler
	Registry  *agents.Registry

	// Hub is optional — nil if the Slack/runner protocol isn't wired up,
	// in which case the Runners page just shows everyone as offline.
	Hub ConnectedRunners

	// InsecureCookies disables the Secure flag on the session cookie, for
	// local dev / tests running over plain HTTP. Never set in a real
	// deployment — Octopus expects TLS in front of it (PLAN.md §8).
	InsecureCookies bool

	// LoginLimiter and RunTriggerLimiter default to sane values if left
	// nil — PLAN.md Phase 8 calls out both /login (now an internet-
	// reachable password check) and the run-trigger endpoint by name as
	// needing rate limiting, separate from Slack's own (gateway.Gateway
	// has its own limiter for /api/slack/command).
	LoginLimiter      *ratelimit.Limiter
	RunTriggerLimiter *ratelimit.Limiter
}

func (s *Server) Routes() *http.ServeMux {
	if s.LoginLimiter == nil {
		s.LoginLimiter = ratelimit.New(10, time.Minute)
	}
	if s.RunTriggerLimiter == nil {
		s.RunTriggerLimiter = ratelimit.New(30, time.Minute)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", handleHealthz)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticSubFS())))

	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.LoginLimiter.Middleware(ratelimit.RemoteAddrKey, s.handleLoginSubmit))
	mux.HandleFunc("POST /logout", s.requireLogin(s.handleLogout))

	mux.HandleFunc("GET /{$}", s.requireLogin(s.handleProjectsList))
	mux.HandleFunc("POST /projects", s.requireLogin(s.handleCreateProject))
	mux.HandleFunc("GET /projects/{id}", s.requireLogin(s.handleProjectDetail))

	mux.HandleFunc("GET /projects/{id}/pipelines/new", s.requireLogin(s.handlePipelineEditor))
	mux.HandleFunc("GET /projects/{id}/pipelines/{defID}/edit", s.requireLogin(s.handlePipelineEditor))
	mux.HandleFunc("POST /projects/{id}/pipelines/{defID}/run",
		s.requireLogin(s.RunTriggerLimiter.Middleware(ratelimit.RemoteAddrKey, s.handleTriggerRun)))
	mux.HandleFunc("POST /projects/{id}/pipelines/{defID}/make-default", s.requireLogin(s.handleMakeDefaultPipeline))

	mux.HandleFunc("GET /runs/{runID}", s.requireLogin(s.handleRunStatus))
	mux.HandleFunc("GET /runs/{runID}/fragment", s.requireLogin(s.handleRunFragment))
	mux.HandleFunc("POST /runs/{runID}/review", s.requireLogin(s.handleRunReview))
	mux.HandleFunc("POST /runs/{runID}/continue", s.requireLogin(s.handleRunContinue))

	mux.HandleFunc("GET /api/agent-types", s.requireLogin(s.handleAgentTypes))
	mux.HandleFunc("POST /api/pipeline-defs", s.requireLogin(s.handleSavePipelineDef))

	mux.HandleFunc("GET /runners", s.requireLogin(s.handleRunnersList))
	mux.HandleFunc("POST /runners", s.requireLogin(s.handleCreateRunner))
	mux.HandleFunc("POST /runners/{id}/revoke", s.requireLogin(s.handleRevokeRunner))

	return mux
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// SecurityHeaders wraps next with a standard set of response headers.
// Composed in main.go around the whole mux (web routes, Slack, runner
// connect) — cheap, harmless on non-browser endpoints, and worth having
// uniformly rather than trying to remember it per-route:
//
//   - X-Content-Type-Options: nosniff — stops a browser from guessing a
//     response is HTML/JS from content sniffing when the server said
//     otherwise, closing a class of stored-content XSS.
//   - X-Frame-Options: DENY — the login page and review-gate forms are
//     exactly what clickjacking targets (an invisible iframe over a
//     "confirm" button); Octopus never needs to be framed.
//   - Referrer-Policy: same-origin — run IDs and pipeline names appear in
//     URLs; no reason for them to leak into a Referer header on outbound
//     links (e.g. a "view diff" link out to GitHub).
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}
