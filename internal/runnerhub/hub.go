// Package runnerhub is the server side of Octopus's Phase 7 runner
// protocol: it tracks connected octopus-runner processes over persistent
// WebSocket connections and implements domain.JobDispatcher by routing
// each job to any currently-connected runner registered for that job's
// project (PLAN.md Key Design Decision 14 — the server never reaches into
// a dev machine's network; runners connect outbound, like a self-hosted CI
// runner).
package runnerhub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/bcrypt"

	"octopus/internal/domain"
	"octopus/internal/store"
)

// wireMessage is the envelope both directions use: the server sends "job"
// messages, the runner sends "result" messages.
type wireMessage struct {
	Type   string               `json:"type"`
	Job    *domain.GitJob       `json:"job,omitempty"`
	Result *domain.GitJobResult `json:"result,omitempty"`
}

type runnerConn struct {
	runner *domain.Runner
	ws     *websocket.Conn

	writeMu sync.Mutex
}

func (c *runnerConn) send(msg wireMessage) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.ws.WriteJSON(msg)
}

// Hub implements domain.JobDispatcher (Dispatch) and domain.AwaitingDispatcher
// (Await) — cliagent.go never needs to know it's talking to a real,
// possibly-offline remote machine instead of Phase 6's LocalDispatcher.
type Hub struct {
	Store store.Store

	// OnRunnerConnected, if set, is called after a runner authenticates
	// and registers, with the project IDs it serves. This is what makes
	// "resumes automatically the moment a matching runner reconnects"
	// (PLAN.md Phase 7) actually happen: main.go wires this to look up
	// every AWAITING_RUNNER run scoped to those projects and call
	// Scheduler.ResumeRun on each, instead of requiring a human to notice
	// and retry.
	OnRunnerConnected func(projectIDs []string)

	upgrader websocket.Upgrader

	mu      sync.Mutex
	conns   map[string]*runnerConn               // runner.ID -> live connection
	waiters map[string]chan *domain.GitJobResult // job.ID -> whoever's awaiting its result (Dispatch or Await)
}

func New(st store.Store) *Hub {
	h := &Hub{
		Store: st,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Runners are our own binary connecting outbound, not browsers
			// — there's no cross-site request to guard against here.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
		conns:   make(map[string]*runnerConn),
		waiters: make(map[string]chan *domain.GitJobResult),
	}

	// Best-effort visibility into what's still outstanding at boot — every
	// pending job's eventual resolution actually happens lazily, the
	// moment the run that dispatched it gets resumed and its node's
	// Execute calls Await (which checks the Store for an already-arrived
	// result before registering a fresh waiter). See Await's doc comment.
	if jobs, err := st.LoadPendingGitJobs(context.Background()); err == nil && len(jobs) > 0 {
		slog.Info("runnerhub: pending jobs from before this boot", "count", len(jobs))
	}

	return h
}

// HandleConnect upgrades a runner's connection after checking its token
// against every registered Runner's hashed token, tracks it, and then just
// reads "result" messages off it until it disconnects — everything else
// (heartbeat, resume-on-connect) happens around the upgrade itself.
func (h *Hub) HandleConnect(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-Runner-Token")
	if token == "" {
		http.Error(w, "missing X-Runner-Token", http.StatusUnauthorized)
		return
	}

	runner, err := h.authenticate(r.Context(), token)
	if err != nil {
		http.Error(w, "invalid runner token", http.StatusUnauthorized)
		return
	}

	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("runnerhub: websocket upgrade failed", "error", err)
		return
	}
	conn := &runnerConn{runner: runner, ws: ws}

	h.mu.Lock()
	h.conns[runner.ID] = conn
	h.mu.Unlock()
	slog.Info("runnerhub: runner connected", "runner_id", runner.ID, "name", runner.Name, "project_ids", runner.ProjectIDs)

	_ = h.Store.TouchRunnerHeartbeat(r.Context(), runner.ID)
	if h.OnRunnerConnected != nil {
		// Must not run synchronously here: OnRunnerConnected typically
		// resumes a run, which can call Dispatch and block waiting for a
		// result — but the only thing that can ever deliver that result is
		// this very function's read loop below, which hasn't started yet.
		// Calling it synchronously would deadlock every single connection.
		go h.OnRunnerConnected(runner.ProjectIDs)
	}

	defer func() {
		h.mu.Lock()
		if h.conns[runner.ID] == conn {
			delete(h.conns, runner.ID)
		}
		h.mu.Unlock()
		ws.Close()
		slog.Info("runnerhub: runner disconnected", "runner_id", runner.ID)
	}()

	for {
		var msg wireMessage
		if err := ws.ReadJSON(&msg); err != nil {
			return // connection broken or closed — cleanup happens in the deferred func above
		}
		if msg.Type != "result" || msg.Result == nil {
			continue
		}
		h.resolveResult(r.Context(), msg.Result)
	}
}

func (h *Hub) authenticate(ctx context.Context, token string) (*domain.Runner, error) {
	runners, err := h.Store.ListRunners(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range runners {
		if bcrypt.CompareHashAndPassword([]byte(r.TokenHash), []byte(token)) == nil {
			return r, nil
		}
	}
	return nil, errors.New("no runner matches the given token")
}

func (h *Hub) resolveResult(ctx context.Context, result *domain.GitJobResult) {
	if err := h.Store.ResolveGitJob(ctx, result.JobID, result); err != nil {
		slog.Error("runnerhub: resolving job", "job_id", result.JobID, "error", err)
	}

	h.mu.Lock()
	ch, ok := h.waiters[result.JobID]
	h.mu.Unlock()
	if ok {
		select {
		case ch <- result:
		default: // waiter already got a result from elsewhere (shouldn't happen, buffered chan) or gave up
		}
	}
}

// Dispatch implements domain.JobDispatcher. It durably records the job
// first (Key Design Decision 19), then either hands it to a connected
// runner for job.ProjectID and blocks for the result, or — if none is
// connected — returns domain.ErrNoRunnerAvailable immediately rather than
// blocking forever (Key Design Decision 15: "no runner online is not a
// failure," the caller turns this into StatusAwaitingRunner). The job stays
// durably queued either way; OnRunnerConnected is what picks it back up.
func (h *Hub) Dispatch(ctx context.Context, job *domain.GitJob) (*domain.GitJobResult, error) {
	// Redacted for the Store: an api_key in job.Payload is meant to reach
	// exactly one subprocess on one runner, ephemerally (Key Design
	// Decision 28) — persisting it here too would undo that, and would
	// also clobber the redacted copy cliagent already saved before
	// calling Dispatch (SaveGitJob upserts by job.ID). The unredacted
	// job — real key included — still goes out to the runner below;
	// that's the one place it's actually supposed to exist.
	if err := h.Store.SaveGitJob(ctx, job.Redacted()); err != nil {
		return nil, fmt.Errorf("saving job before dispatch: %w", err)
	}

	conn := h.pickRunner(job.ProjectID)
	if conn == nil {
		return nil, domain.ErrNoRunnerAvailable
	}

	ch := h.registerWaiter(job.ID)
	defer h.unregisterWaiter(job.ID)

	if err := conn.send(wireMessage{Type: "job", Job: job}); err != nil {
		return nil, fmt.Errorf("sending job to runner %s: %w", conn.runner.ID, err)
	}

	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Await implements domain.AwaitingDispatcher: cliagent calls this instead
// of Dispatch when it finds a job already pending for its node (a resumed
// run after a server restart). It checks the Store first — the runner may
// have already delivered the result while this process was down or
// starting back up — and only registers a fresh waiter if it genuinely
// hasn't arrived yet.
func (h *Hub) Await(ctx context.Context, jobID string) (*domain.GitJobResult, error) {
	if result, err := h.Store.LoadGitJobResult(ctx, jobID); err == nil {
		return result, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	ch := h.registerWaiter(jobID)
	defer h.unregisterWaiter(jobID)

	select {
	case result := <-ch:
		return result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// pickRunner returns any currently-connected runner registered for
// projectID — deliberately not pinned to whichever runner handled this
// run's earlier jobs (Key Design Decision 20: every job fetches before
// acting and pushes after mutating, specifically so handoff between
// runners works).
func (h *Hub) pickRunner(projectID string) *runnerConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.conns {
		for _, p := range c.runner.ProjectIDs {
			if p == projectID {
				return c
			}
		}
	}
	return nil
}

func (h *Hub) registerWaiter(jobID string) chan *domain.GitJobResult {
	ch := make(chan *domain.GitJobResult, 1)
	h.mu.Lock()
	h.waiters[jobID] = ch
	h.mu.Unlock()
	return ch
}

func (h *Hub) unregisterWaiter(jobID string) {
	h.mu.Lock()
	delete(h.waiters, jobID)
	h.mu.Unlock()
}

// ConnectedRunnerIDs is a small introspection hook for the web UI's
// Runners admin page (Phase 7/8) to show which registered runners are
// currently online, not just their last-seen timestamp.
func (h *Hub) ConnectedRunnerIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	ids := make([]string, 0, len(h.conns))
	for id := range h.conns {
		ids = append(ids, id)
	}
	return ids
}

// DisconnectRunner forcibly closes a specific runner's connection from the
// server side, without touching anything about the runner process itself
// — its local job execution (already independent of the network
// connection) keeps going. Exists mainly for tests that need to simulate
// "this connection genuinely dropped" precisely and on demand, but is a
// reasonable admin action too (e.g. revoking a runner immediately rather
// than waiting for its token to be deleted to take effect).
func (h *Hub) DisconnectRunner(runnerID string) {
	h.mu.Lock()
	conn, ok := h.conns[runnerID]
	h.mu.Unlock()
	if ok {
		conn.ws.Close()
	}
}
