package cliagent_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"octopus/internal/agents/cliagent"
	"octopus/internal/domain"
	"octopus/internal/engine"
	"octopus/internal/store"
)

type fakeDispatcher struct {
	result *domain.GitJobResult
	err    error
	gotJob *domain.GitJob
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, job *domain.GitJob) (*domain.GitJobResult, error) {
	f.gotJob = job
	if f.err != nil {
		return nil, f.err
	}
	f.result.JobID = job.ID
	return f.result, nil
}

func newTestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")
	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	if err := st.SaveProject(context.Background(), &domain.Project{ID: "proj1", Name: "Proj1", GitRemoteURL: "git@x:y.git", BaseBranch: "main"}); err != nil {
		t.Fatalf("seeding proj1: %v", err)
	}
	return st
}

func newState() *domain.PipelineState {
	s := domain.NewPipelineState("run1")
	s.ProjectID = "proj1"
	s.TicketID = "TICKET-1"
	s.GitBranch = "octopus/TICKET-1"
	return s
}

func TestCliagent_SuccessWritesOutputAndSessionID(t *testing.T) {
	st := newTestStore(t)
	disp := &fakeDispatcher{result: &domain.GitJobResult{Success: true, Output: "did the work", SessionID: "sess-new"}}
	a := &cliagent.Agent{NodeID: "coder", Tool: "claude", RolePrompt: "be a coder", Dispatcher: disp, Store: st}

	state := newState()
	if err := a.Execute(context.Background(), state); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	out, ok := state.GetOutput("coder")
	if !ok || out != "did the work" {
		t.Fatalf("expected node output to be set, got %v (ok=%v)", out, ok)
	}
	if state.GetSessionID() != "sess-new" {
		t.Fatalf("expected SessionID sess-new, got %q", state.GetSessionID())
	}

	if disp.gotJob.Type != "run_agent" {
		t.Fatalf("expected run_agent job type, got %q", disp.gotJob.Type)
	}
	if disp.gotJob.Payload["tool"] != "claude" {
		t.Fatalf("expected tool=claude in payload, got %v", disp.gotJob.Payload["tool"])
	}
}

func TestCliagent_ResumesExistingSessionID(t *testing.T) {
	st := newTestStore(t)
	disp := &fakeDispatcher{result: &domain.GitJobResult{Success: true, Output: "continued", SessionID: "sess-old"}}
	a := &cliagent.Agent{NodeID: "coder", Tool: "claude", RolePrompt: "be a coder", Dispatcher: disp, Store: st}

	state := newState()
	state.SetSessionID("sess-old")

	if err := a.Execute(context.Background(), state); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if disp.gotJob.Payload["session_id"] != "sess-old" {
		t.Fatalf("expected the prior session id to be sent for resume, got %v", disp.gotJob.Payload["session_id"])
	}
}

func TestCliagent_JobFailureReturnsError(t *testing.T) {
	st := newTestStore(t)
	disp := &fakeDispatcher{result: &domain.GitJobResult{Success: false, Error: "cli exploded"}}
	a := &cliagent.Agent{NodeID: "coder", Tool: "claude", RolePrompt: "x", Dispatcher: disp, Store: st}

	err := a.Execute(context.Background(), newState())
	if err == nil {
		t.Fatal("expected an error when the job result is unsuccessful")
	}
	if !strings.Contains(err.Error(), "cli exploded") {
		t.Fatalf("expected error to include the job's failure reason, got: %v", err)
	}
}

func TestCliagent_RefusesToDispatchWhenAPendingJobAlreadyExists(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Simulate a prior crashed attempt: a run_agent job saved but never resolved.
	if err := st.SaveGitJob(ctx, &domain.GitJob{ID: "orphan-job", RunID: "run1", NodeID: "coder", ProjectID: "proj1", Type: "run_agent"}); err != nil {
		t.Fatalf("seeding orphan job: %v", err)
	}

	disp := &fakeDispatcher{result: &domain.GitJobResult{Success: true}}
	a := &cliagent.Agent{NodeID: "coder", Tool: "claude", RolePrompt: "x", Dispatcher: disp, Store: st}

	err := a.Execute(ctx, newState())
	if err == nil {
		t.Fatal("expected an error when a pending job already exists for this node")
	}
	if disp.gotJob != nil {
		t.Fatal("must not dispatch a second job when one is already pending — that's exactly the duplicate-session bug this check prevents")
	}
}

func TestCliagent_SecurityPresetBlocksPipeline(t *testing.T) {
	st := newTestStore(t)
	disp := &fakeDispatcher{result: &domain.GitJobResult{Success: true, Output: "BLOCKED: found a hardcoded API key in config.go"}}
	a := &cliagent.Agent{
		NodeID: "security", Tool: "claude", RolePrompt: "be a security reviewer",
		Dispatcher: disp, Store: st,
		DetectBlocked: func(output string) (bool, string) {
			if strings.HasPrefix(output, "BLOCKED") {
				return true, strings.TrimPrefix(output, "BLOCKED: ")
			}
			return false, ""
		},
	}

	err := a.Execute(context.Background(), newState())
	if err == nil {
		t.Fatal("expected an error when the security preset detects a blocking issue")
	}
	if !errors.Is(err, engine.ErrBlocked) {
		t.Fatalf("expected error to wrap engine.ErrBlocked, got: %v", err)
	}
	if !strings.Contains(err.Error(), "hardcoded API key") {
		t.Fatalf("expected the block reason in the error, got: %v", err)
	}
}

func TestCliagent_SecurityPresetPassesWhenClear(t *testing.T) {
	st := newTestStore(t)
	disp := &fakeDispatcher{result: &domain.GitJobResult{Success: true, Output: "CLEAR: no issues found"}}
	a := &cliagent.Agent{
		NodeID: "security", Tool: "claude", RolePrompt: "be a security reviewer",
		Dispatcher: disp, Store: st,
		DetectBlocked: func(output string) (bool, string) {
			return strings.HasPrefix(output, "BLOCKED"), ""
		},
	}

	if err := a.Execute(context.Background(), newState()); err != nil {
		t.Fatalf("expected no error for a CLEAR verdict, got: %v", err)
	}
}
