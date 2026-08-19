package domain

import (
	"context"
	"errors"
	"strings"
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
	Type      string         // "prepare_branch" | "run_agent" | "diff" | "apply_patch" | "commit" | "push" | "merge" | "shell_exec"
	Payload   map[string]any // for run_agent: {"tool": "claude"|"codex"|"antigravity", "prompt": "...", "session_id": "...", "api_key": "..."}
}

// Redacted returns a deep-copied job whose credential-shaped payload fields
// have been blanked. It is the only form safe to persist or expose through
// diagnostics. The original job remains untouched so its ephemeral secrets
// can still reach the one subprocess invocation that needs them.
//
// Payloads are intentionally open-ended, so redaction is recursive and
// recognizes common token/password/secret names in addition to api_key.
// Every durable or user-visible job boundary must use this method.
func (j *GitJob) Redacted() *GitJob {
	cp := *j
	cp.Payload = redactPayloadMap(j.Payload)
	return &cp
}

func redactPayloadMap(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	redacted := make(map[string]any, len(payload))
	for key, value := range payload {
		if sensitivePayloadKey(key) {
			redacted[key] = ""
			continue
		}
		redacted[key] = redactPayloadValue(value)
	}
	return redacted
}

func redactPayloadValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return redactPayloadMap(typed)
	case map[string]string:
		redacted := make(map[string]string, len(typed))
		for key, nested := range typed {
			if sensitivePayloadKey(key) {
				redacted[key] = ""
			} else {
				redacted[key] = nested
			}
		}
		return redacted
	case []any:
		redacted := make([]any, len(typed))
		for i, nested := range typed {
			redacted[i] = redactPayloadValue(nested)
		}
		return redacted
	default:
		return value
	}
}

func sensitivePayloadKey(key string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
	switch normalized {
	case "api_key", "access_token", "auth_token", "refresh_token", "runner_token",
		"authorization", "cookie", "password", "passphrase", "private_key",
		"ssh_key", "token", "secret", "signing_secret", "client_secret":
		return true
	}
	for _, suffix := range []string{
		"_api_key", "_access_token", "_auth_token", "_refresh_token",
		"_runner_token", "_password", "_passphrase", "_private_key",
		"_signing_secret", "_client_secret",
	} {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
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
