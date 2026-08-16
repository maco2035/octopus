// Package cliagent implements one generic domain.Agent that delegates its
// real work to a coding CLI (Claude Code, Codex CLI, Antigravity CLI) running on
// a runner, instead of Octopus reimplementing a read/edit/test/iterate
// loop centrally (PLAN.md Key Design Decision 25). Every named preset in
// agents/presets.go is this same Agent, just constructed with a different
// tool + role prompt.
package cliagent

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"octopus/internal/domain"
	"octopus/internal/engine"
	"octopus/internal/store"
)

type Agent struct {
	NodeID     string
	Tool       string // "claude" | "codex" | "antigravity" — which CLI this node delegates to
	RolePrompt string // fixed per preset: what this node's role is (coder/reviewer/reporter/security)
	APIKey     string // this tool's provider key; rides in the job payload for one invocation only (Key Design Decision 28)
	Dispatcher domain.JobDispatcher
	Store      store.Store

	// DetectBlocked, if set, inspects a successful run's output and — if
	// it finds a real problem — returns (true, reason), which Execute
	// turns into an engine.ErrBlocked so the pipeline halts with
	// StatusBlocked instead of finishing normally (PLAN.md Phase 6's
	// security agent). nil for every non-security preset.
	DetectBlocked func(output string) (blocked bool, reason string)
}

func (a *Agent) Name() string { return "cliagent:" + a.Tool + ":" + a.NodeID }

// Execute checks for an unresolved job from a prior attempt first. If one
// exists and the configured Dispatcher can await it (Phase 7's
// runnerhub.Hub — the runner keeps working independently of the server),
// it awaits that job's eventual result instead of starting a duplicate
// coding session. Otherwise (Phase 6's tools.LocalDispatcher, where a
// crashed local subprocess truly has nothing left to await) it surfaces a
// clear error rather than silently duplicating or hanging forever. With no
// prior job, it dispatches a fresh run_agent job and, on success, writes
// the CLI's output into this node's output and the session it reports
// into the run's SessionID so a later continuation resumes the same
// conversation.
func (a *Agent) Execute(ctx context.Context, state *domain.PipelineState) error {
	existing, err := a.Store.LoadPendingGitJobFor(ctx, state.RunID, a.NodeID)
	switch {
	case err == nil:
		awaiter, ok := a.Dispatcher.(domain.AwaitingDispatcher)
		if !ok {
			return fmt.Errorf("cliagent %s: an unresolved run_agent job (id=%s) already exists for this node — "+
				"the process likely crashed mid-dispatch, and this dispatcher has no independent runner process "+
				"still working on it to await. Resolve or clear the job row before retrying this run", a.NodeID, existing.ID)
		}
		result, err := awaiter.Await(ctx, existing.ID)
		if err != nil {
			return a.handleDispatchErr(err)
		}
		return a.applyResult(ctx, state, existing.ID, result)
	case errors.Is(err, store.ErrNotFound):
		// No pending job — the normal path.
	default:
		return fmt.Errorf("cliagent %s: checking for a pending job: %w", a.NodeID, err)
	}

	project, err := a.Store.LoadProject(ctx, state.ProjectID)
	if err != nil {
		return fmt.Errorf("cliagent %s: loading project: %w", a.NodeID, err)
	}

	job := &domain.GitJob{
		ID: uuid.NewString(), RunID: state.RunID, NodeID: a.NodeID, ProjectID: state.ProjectID,
		Type: "run_agent",
		Payload: map[string]any{
			"tool":       a.Tool,
			"prompt":     a.buildPrompt(state),
			"session_id": state.GetSessionID(),
			"api_key":    a.APIKey,
			"branch":     state.GitBranch,
			"remote_url": project.GitRemoteURL,
		},
	}
	// Redacted: job.Payload carries the real API key for this one
	// dispatch, but nothing persisted to the Store should ever have it —
	// see domain.GitJob.Redacted's doc comment.
	if err := a.Store.SaveGitJob(ctx, job.Redacted()); err != nil {
		return fmt.Errorf("cliagent %s: saving job before dispatch: %w", a.NodeID, err)
	}

	result, err := a.Dispatcher.Dispatch(ctx, job)
	if err != nil {
		return a.handleDispatchErr(err)
	}
	return a.applyResult(ctx, state, job.ID, result)
}

// handleDispatchErr turns domain.ErrNoRunnerAvailable into
// engine.ErrAwaitingRunner (PLAN.md Phase 7's "no runner online is not a
// failure") and wraps every other dispatch error plainly.
func (a *Agent) handleDispatchErr(err error) error {
	if errors.Is(err, domain.ErrNoRunnerAvailable) {
		return fmt.Errorf("%w: %s", engine.ErrAwaitingRunner, a.NodeID)
	}
	return fmt.Errorf("cliagent %s: dispatch: %w", a.NodeID, err)
}

func (a *Agent) applyResult(ctx context.Context, state *domain.PipelineState, jobID string, result *domain.GitJobResult) error {
	if err := a.Store.ResolveGitJob(ctx, jobID, result); err != nil {
		return fmt.Errorf("cliagent %s: resolving job: %w", a.NodeID, err)
	}
	if !result.Success {
		return fmt.Errorf("cliagent %s: %s", a.NodeID, result.Error)
	}

	state.SetOutput(a.NodeID, result.Output)
	if result.SessionID != "" {
		state.SetSessionID(result.SessionID)
	}

	if a.DetectBlocked != nil {
		if blocked, reason := a.DetectBlocked(result.Output); blocked {
			return fmt.Errorf("%w: %s", engine.ErrBlocked, reason)
		}
	}
	return nil
}

func (a *Agent) buildPrompt(state *domain.PipelineState) string {
	return fmt.Sprintf("%s\n\nTicket: %s\nRun: %s\n", a.RolePrompt, state.TicketID, state.RunID)
}
