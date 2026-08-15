package domain

import (
	"context"
	"errors"
	"time"
)

// JobDispatcher is the one dependency a cliagent (or any git-tool agent)
// needs — it doesn't know or care whether a GitJob runs directly on the
// server (Phase 6's tools.LocalDispatcher) or is routed to a remote
// octopus-runner over a persistent connection (Phase 7's runnerhub.Hub).
// Swapping one for the other is a wiring change in main.go; cliagent.go
// itself never changes. Defined here rather than in package agents (as
// PLAN.md's directory sketch originally suggested) because agents/presets.go
// needs to import agents/cliagent, and cliagent needs this type — putting
// it in package agents would make that an import cycle; domain is the
// natural shared ground since GitJob/GitJobResult already live here.
type JobDispatcher interface {
	Dispatch(ctx context.Context, job *GitJob) (*GitJobResult, error)
}

// AwaitingDispatcher is an optional capability a JobDispatcher can offer:
// instead of a node failing outright when it finds a job already pending
// for itself (the re-run-after-restart case, Key Design Decision 19),
// cliagent calls Await to pick up that job's eventual result rather than
// dispatching a duplicate. tools.LocalDispatcher (Phase 6) doesn't
// implement this — a crashed local subprocess genuinely has nothing left
// to await. runnerhub.Hub (Phase 7) does: the runner is a separate,
// independent process that keeps working regardless of what happens to the
// server, so its eventual result can still be awaited after a server
// restart.
type AwaitingDispatcher interface {
	Await(ctx context.Context, jobID string) (*GitJobResult, error)
}

// ErrNoRunnerAvailable is what a JobDispatcher returns from Dispatch when
// the job is durably queued (already SaveGitJob'd) but nothing is
// currently connected to run it — PLAN.md Phase 7's "no runner online is
// not a failure." The caller (cliagent, mergeAgent) turns this into
// engine.ErrAwaitingRunner so the run pauses with StatusAwaitingRunner
// instead of StatusFailed.
var ErrNoRunnerAvailable = errors.New("no runner currently connected for this project")

// Runner is a dev machine's octopus-runner registration (PLAN.md Key
// Design Decision 14) — it connects outbound and declares which projects
// it can do git work for.
type Runner struct {
	ID         string
	Name       string // hostname or user-given label
	TokenHash  string // shared secret, hashed at rest, generated from the web UI
	ProjectIDs []string
	LastSeen   time.Time
}

// GitJob is one unit of work dispatched to a runner. The name predates
// Type including non-git work ("run_agent") — kept as the name Phase 2
// already shipped rather than churning it for a rename (PLAN.md Key Design
// Decision 19).
type GitJob struct {
	ID        string
	RunID     string
	NodeID    string // which node this job belongs to; lets a resumed node find its own still-unresolved job instead of dispatching a duplicate
	ProjectID string
	Type      string // "prepare_branch" | "run_agent" | "diff" | "apply_patch" | "commit" | "push" | "merge" | "shell_exec"
	Payload   map[string]any // for run_agent: {"tool": "claude"|"codex"|"gemini", "prompt": "...", "session_id": "...", "api_key": "..."}
}

// Redacted returns a copy of j with Payload["api_key"] removed — safe to
// persist (Store.SaveGitJob) or log. The real job (with the real key)
// still goes to Dispatch/over the wire to a runner exactly once, for that
// one subprocess invocation, per Key Design Decision 28 ("used only as an
// ephemeral environment variable ... never written to the runner's disk
// or config"); that promise only holds if the *server* doesn't write it
// to its own disk either; SaveGitJob persisting Payload verbatim to
// SQLite would do exactly that — the whole payload, api_key included,
// sitting in git_jobs.payload_json indefinitely, including after the job
// resolves. Every SaveGitJob call site must use this, not the raw job.
func (j *GitJob) Redacted() *GitJob {
	if _, ok := j.Payload["api_key"]; !ok {
		return j
	}
	redactedPayload := make(map[string]any, len(j.Payload))
	for k, v := range j.Payload {
		redactedPayload[k] = v
	}
	redactedPayload["api_key"] = ""
	cp := *j
	cp.Payload = redactedPayload
	return &cp
}

// GitJobResult carries a runner's full output back, not just success/fail
// — this is what makes logs visible centrally regardless of which machine
// ran the job (Key Design Decision 16).
type GitJobResult struct {
	JobID     string
	Success   bool
	Output    string // full stdout/stderr — persisted centrally so logs are visible regardless of which machine ran it
	Error     string
	SessionID string // set for run_agent jobs: the session to pass on the next invocation that should continue this conversation
}
