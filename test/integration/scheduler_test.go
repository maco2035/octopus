// Package integration_test also exercises the Scheduler against a real
// SQLite store — proving PLAN.md Phase 3's "Done when" criteria: two
// projects' pipelines run concurrently and both complete correctly.
package integration_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/domain"
	"octopus/internal/scheduler"
	"octopus/internal/store"
)

func singleEchoDef(id, projectID string) *domain.PipelineDef {
	return &domain.PipelineDef{
		ID:        id,
		ProjectID: projectID,
		Name:      id,
		Nodes: []domain.NodeDef{
			{ID: "only", AgentType: "echo", Config: map[string]any{"delay_ms": 150}},
		},
	}
}

// TestScheduler_TwoProjectsRunConcurrently starts a run for project A and,
// immediately after (back-to-back, not waited-on), a run for project B.
// Each run's single node sleeps 150ms before completing. If StartRun
// blocked on the run finishing, or the scheduler serialized runs, the total
// wall-clock time here would be >= 300ms; real concurrency keeps it well
// under that.
func TestScheduler_TwoProjectsRunConcurrently(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")

	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)

	defA := singleEchoDef("defA", "projA")
	defB := singleEchoDef("defB", "projB")
	if err := st.SavePipelineDef(ctx, defA); err != nil {
		t.Fatalf("SavePipelineDef A: %v", err)
	}
	if err := st.SavePipelineDef(ctx, defB); err != nil {
		t.Fatalf("SavePipelineDef B: %v", err)
	}

	sched := scheduler.New(st, reg.Create)

	start := time.Now()
	stateA, err := sched.StartRun(ctx, "projA", "defA", "TICKET-A")
	if err != nil {
		t.Fatalf("StartRun A: %v", err)
	}
	stateB, err := sched.StartRun(ctx, "projB", "defB", "TICKET-B")
	if err != nil {
		t.Fatalf("StartRun B: %v", err)
	}
	dispatchElapsed := time.Since(start)
	if dispatchElapsed > 100*time.Millisecond {
		t.Fatalf("StartRun blocked on completion instead of returning immediately: took %v", dispatchElapsed)
	}

	sched.Wait()
	totalElapsed := time.Since(start)
	if totalElapsed >= 300*time.Millisecond {
		t.Fatalf("runs did not overlap: took %v, expected well under 300ms if concurrent", totalElapsed)
	}

	reloadedA, err := st.Load(ctx, stateA.RunID)
	if err != nil {
		t.Fatalf("Load A: %v", err)
	}
	if reloadedA.Status != domain.StatusCompleted {
		t.Fatalf("run A: expected COMPLETED, got %s", reloadedA.Status)
	}

	reloadedB, err := st.Load(ctx, stateB.RunID)
	if err != nil {
		t.Fatalf("Load B: %v", err)
	}
	if reloadedB.Status != domain.StatusCompleted {
		t.Fatalf("run B: expected COMPLETED, got %s", reloadedB.Status)
	}
}

// TestScheduler_ContinuationSeedsSessionID proves the "carry context
// between multiple things to do" plumbing (Key Design Decision 27): a run
// started as an explicit continuation of a prior one inherits its
// SessionID before any node executes.
func TestScheduler_ContinuationSeedsSessionID(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")

	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)

	def := singleEchoDef("def1", "proj1")
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	sched := scheduler.New(st, reg.Create)

	first, err := sched.StartRun(ctx, "proj1", "def1", "TICKET-1")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	sched.Wait()

	if err := st.Save(ctx, &domain.PipelineState{
		RunID: first.RunID, ProjectID: "proj1", PipelineDefID: "def1",
		Status: domain.StatusCompleted, SessionID: "sess-abc", NodeOutputs: map[string]any{},
	}); err != nil {
		t.Fatalf("seeding SessionID on first run: %v", err)
	}

	second, err := sched.StartContinuation(ctx, "proj1", "def1", "TICKET-1-follow-up", first.RunID)
	if err != nil {
		t.Fatalf("StartContinuation: %v", err)
	}
	if second.SessionID != "sess-abc" {
		t.Fatalf("expected continuation to seed SessionID from prior run, got %q", second.SessionID)
	}
	sched.Wait()
}
