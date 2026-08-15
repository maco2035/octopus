// Package scheduler runs multiple pipeline runs concurrently across any
// number of projects (PLAN.md Key Design Decision 11): starting or
// resuming one run never blocks, or is blocked by, another. Each run
// executes in its own goroutine; the Store (not this package's memory) is
// the durable source of truth for a run's state, so a process restart
// recovers via ResumeActive rather than losing track of anything.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/google/uuid"

	"octopus/internal/domain"
	"octopus/internal/engine"
	"octopus/internal/store"
)

// Scheduler starts/tracks pipeline runs, each in its own goroutine, backed
// by the Store.
type Scheduler struct {
	Store       store.Store
	CreateAgent engine.AgentFactory

	// Dispatcher, if set, makes every run's first action an implicit
	// prepare_branch job (PLAN.md Key Design Decision 13) — no agent ever
	// picks a branch name; the run simply fails to start if this fails.
	// nil (the default) skips this entirely, which is what every run
	// composed only of non-git agents (EchoAgent, most of this codebase's
	// own tests) relies on — they'd otherwise need a real git remote to
	// run at all.
	Dispatcher domain.JobDispatcher
	// BranchPattern is used to name the branch prepare_branch cuts, with
	// "{ticket_id}" substituted. Defaults to "octopus/{ticket_id}" if empty.
	BranchPattern string

	// OnSettled, if set, is called every time a run this Scheduler drove
	// stops making progress on its own — paused at a review gate,
	// completed, failed, or rejected — with its just-updated state. The
	// Slack gateway (Phase 5) uses this to post a review card the moment a
	// run reaches AWAITING_REVIEW, without polling. Optional: nil is a
	// no-op, which is what the web UI's poll-based views rely on instead.
	OnSettled func(state *domain.PipelineState)

	mu   sync.Mutex
	runs map[string]*engine.Pipeline // runID -> the Pipeline this process is actively driving, for introspection
	wg   sync.WaitGroup
}

func New(st store.Store, createAgent engine.AgentFactory) *Scheduler {
	return &Scheduler{Store: st, CreateAgent: createAgent, runs: make(map[string]*engine.Pipeline)}
}

// StartRun creates a brand-new run for pipelineDefID against ticketID and
// launches it in its own goroutine. It returns as soon as the run is
// durably recorded (StatusPending); the caller doesn't block on the
// pipeline finishing — poll the Store, or watch for a Slack/UI
// notification, for that.
func (s *Scheduler) StartRun(ctx context.Context, projectID, pipelineDefID, ticketID string) (*domain.PipelineState, error) {
	return s.startRunFrom(ctx, projectID, pipelineDefID, ticketID, "")
}

// StartContinuation is like StartRun but seeds the new run's SessionID from
// fromRunID's, so a coding-agent node resumes the same session instead of
// starting cold (PLAN.md Key Design Decision 27).
func (s *Scheduler) StartContinuation(ctx context.Context, projectID, pipelineDefID, ticketID, fromRunID string) (*domain.PipelineState, error) {
	prior, err := s.Store.Load(ctx, fromRunID)
	if err != nil {
		return nil, fmt.Errorf("loading prior run %s to continue from: %w", fromRunID, err)
	}
	return s.startRunFrom(ctx, projectID, pipelineDefID, ticketID, prior.SessionID)
}

func (s *Scheduler) startRunFrom(ctx context.Context, projectID, pipelineDefID, ticketID, sessionID string) (*domain.PipelineState, error) {
	def, err := s.Store.LoadPipelineDef(ctx, pipelineDefID)
	if err != nil {
		return nil, fmt.Errorf("loading pipeline def %s: %w", pipelineDefID, err)
	}

	state := domain.NewPipelineState(uuid.NewString())
	state.ProjectID = projectID
	state.PipelineDefID = pipelineDefID
	state.TicketID = ticketID
	state.SessionID = sessionID

	if err := s.Store.Save(ctx, state); err != nil {
		return nil, fmt.Errorf("saving new run %s: %w", state.RunID, err)
	}

	if s.Dispatcher != nil {
		if err := s.prepareBranch(ctx, state); err != nil {
			if errors.Is(err, domain.ErrNoRunnerAvailable) {
				// Durably queued (SaveGitJob already happened inside
				// prepareBranch); nothing to launch yet — ResumeRun
				// retries this exact step the moment a runner connects.
				state.Status = domain.StatusAwaitingRunner
				_ = s.Store.Save(ctx, state)
				return state, nil
			}
			state.Status = domain.StatusFailed
			state.Summary = err.Error()
			_ = s.Store.Save(ctx, state)
			return nil, err
		}
	}

	p := &engine.Pipeline{Def: def, State: state, CreateAgent: s.CreateAgent, Checkpoint: s.Store}
	s.launch(p, func(ctx context.Context) error {
		return p.Run(ctx, nil)
	})

	return state, nil
}

// prepareBranch dispatches the implicit prepare_branch job every run needs
// before any node executes, and sets state.GitBranch from the result — the
// engine and every agent just read GitBranch off state from then on;
// nothing downstream ever names a branch itself.
func (s *Scheduler) prepareBranch(ctx context.Context, state *domain.PipelineState) error {
	project, err := s.Store.LoadProject(ctx, state.ProjectID)
	if err != nil {
		return fmt.Errorf("loading project %s for prepare_branch: %w", state.ProjectID, err)
	}

	pattern := s.BranchPattern
	if pattern == "" {
		pattern = "octopus/{ticket_id}"
	}
	branch := strings.ReplaceAll(pattern, "{ticket_id}", state.TicketID)

	job := &domain.GitJob{
		ID: uuid.NewString(), RunID: state.RunID, ProjectID: state.ProjectID,
		Type: "prepare_branch",
		Payload: map[string]any{
			"remote_url":  project.GitRemoteURL,
			"base_branch": project.BaseBranch,
			"branch_name": branch,
		},
	}
	if err := s.Store.SaveGitJob(ctx, job.Redacted()); err != nil {
		return fmt.Errorf("saving prepare_branch job: %w", err)
	}

	result, err := s.Dispatcher.Dispatch(ctx, job)
	if err != nil {
		return fmt.Errorf("dispatching prepare_branch: %w", err)
	}
	if err := s.Store.ResolveGitJob(ctx, job.ID, result); err != nil {
		return fmt.Errorf("resolving prepare_branch job: %w", err)
	}
	if !result.Success {
		return fmt.Errorf("prepare_branch failed: %s", result.Error)
	}

	state.GitBranch = branch
	return s.Store.Save(ctx, state)
}

// Continue resolves a paused review gate and, if approved, resumes the rest
// of the run's DAG in its own goroutine. Rejection is terminal, so nothing
// further gets launched.
func (s *Scheduler) Continue(ctx context.Context, runID, actionToken string, approve bool, editedOutputs map[string]any) error {
	if err := s.Store.ResolveReview(ctx, runID, actionToken, approve, editedOutputs); err != nil {
		return err
	}
	if !approve {
		return nil
	}

	s.resumeAsync(runID, "resume after review failed")
	return nil
}

// ResumeRun resumes a single run that's currently AWAITING_RUNNER — the
// runner-reconnect equivalent of what Continue does after a review
// approval. engine.Resume on its own deliberately leaves an
// AWAITING_RUNNER run paused (so a boot-time ResumeActive doesn't
// second-guess a state that's still legitimately waiting); calling
// ResumeRun is the explicit signal that something actually changed — a
// matching runner just connected — so, exactly like Continue does by
// calling ResolveReview first, this flips the run back to RUNNING before
// handing it to Resume. If the run isn't AWAITING_RUNNER (already resolved
// by another path, or never was), this is a safe no-op.
//
// A run can land on AWAITING_RUNNER two different ways — before its DAG
// ever started (prepare_branch itself found no runner) or mid-DAG (a
// cliagent/mergeAgent node did) — and engine.Resume only knows how to
// retry the second kind. If GitBranch is still empty, this retries
// prepare_branch itself first; if that's still stuck with nobody
// connected for this project (e.g. a different runner just connected for
// an unrelated project), it goes right back to AWAITING_RUNNER rather than
// erroring out.
func (s *Scheduler) ResumeRun(ctx context.Context, runID string) {
	state, err := s.Store.Load(ctx, runID)
	if err != nil {
		slog.Error("scheduler: loading run to resume", "run_id", runID, "error", err)
		return
	}
	if state.Status != domain.StatusAwaitingRunner {
		return
	}

	if state.GitBranch == "" && s.Dispatcher != nil {
		if err := s.prepareBranch(ctx, state); err != nil {
			if errors.Is(err, domain.ErrNoRunnerAvailable) {
				return // still nothing connected for this project; stays AWAITING_RUNNER
			}
			state.Status = domain.StatusFailed
			state.Summary = err.Error()
			_ = s.Store.Save(ctx, state)
			return
		}
	}

	state.Status = domain.StatusRunning
	if err := s.Store.Save(ctx, state); err != nil {
		slog.Error("scheduler: marking run RUNNING before resume", "run_id", runID, "error", err)
		return
	}

	s.resumeAsync(runID, "resume after runner connect failed")
}

func (s *Scheduler) resumeAsync(runID, failureLogMsg string) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		p, err := engine.Resume(context.Background(), s.Store, s.Store, runID, s.CreateAgent)
		if err != nil {
			slog.Error("scheduler: "+failureLogMsg, "run_id", runID, "error", err)
			return
		}
		s.track(runID, p)
		defer s.untrack(runID)
		if s.OnSettled != nil {
			s.OnSettled(p.State)
		}
	}()
}

// ResumeActive reconstructs and, where appropriate, resumes every non-
// terminal run the Store knows about — the boot-time recovery path after a
// process restart. Runs paused on a review gate or a runner are
// reconstructed but left paused, exactly as engine.Resume already
// guarantees; this just does it for every active run instead of one.
func (s *Scheduler) ResumeActive(ctx context.Context) error {
	active, err := s.Store.ListActiveRuns(ctx)
	if err != nil {
		return fmt.Errorf("listing active runs: %w", err)
	}

	for _, state := range active {
		s.resumeAsync(state.RunID, "resume on boot failed")
	}

	return nil
}

// Pipeline returns the in-memory Pipeline for a run this process is
// currently driving, if any — runs this process isn't actively executing
// (completed, paused, or being driven by a different process entirely)
// aren't here; the Store is always the authoritative source for those.
func (s *Scheduler) Pipeline(runID string) (*engine.Pipeline, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.runs[runID]
	return p, ok
}

// Wait blocks until every run this Scheduler has launched has finished (or
// paused). Intended for tests and graceful shutdown, not request paths.
func (s *Scheduler) Wait() {
	s.wg.Wait()
}

func (s *Scheduler) launch(p *engine.Pipeline, run func(ctx context.Context) error) {
	s.track(p.State.RunID, p)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.untrack(p.State.RunID)
		if err := run(context.Background()); err != nil &&
			!errors.Is(err, engine.ErrAwaitingReview) && !errors.Is(err, engine.ErrBlocked) && !errors.Is(err, engine.ErrAwaitingRunner) {
			slog.Error("scheduler: run failed", "run_id", p.State.RunID, "error", err)
		}
		if s.OnSettled != nil {
			s.OnSettled(p.State)
		}
	}()
}

func (s *Scheduler) track(runID string, p *engine.Pipeline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[runID] = p
}

func (s *Scheduler) untrack(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, runID)
}
