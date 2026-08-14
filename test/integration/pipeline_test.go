// Package integration_test exercises the engine and the Store together —
// unlike internal/engine's unit tests (Phase 1, no network/DB), these use
// a real SQLite file to prove a run genuinely surviving a process restart,
// per PLAN.md Phase 2's "Done when" criteria.
package integration_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/domain"
	"octopus/internal/engine"
	"octopus/internal/store"
)

// countingAgent records how many times each node actually executed, so a
// test can assert a node that was already checkpointed before a simulated
// crash does NOT run again during Resume.
type countingAgent struct {
	nodeID string
	mu     *sync.Mutex
	ran    map[string]int
}

func (a countingAgent) Name() string { return "counting:" + a.nodeID }

func (a countingAgent) Execute(ctx context.Context, state *domain.PipelineState) error {
	a.mu.Lock()
	a.ran[a.nodeID]++
	a.mu.Unlock()
	state.SetOutput(a.nodeID, a.nodeID+"-done")
	return nil
}

func diamondDef() *domain.PipelineDef {
	return &domain.PipelineDef{
		ID:        "diamond",
		ProjectID: "proj1",
		Name:      "diamond",
		Nodes: []domain.NodeDef{
			{ID: "A", AgentType: "counting"},
			{ID: "B", AgentType: "counting"},
			{ID: "C", AgentType: "counting"},
			{ID: "D", AgentType: "counting"},
		},
		Edges: []domain.EdgeDef{
			{From: "A", To: "B"},
			{From: "A", To: "C"},
			{From: "B", To: "D"},
			{From: "C", To: "D"},
		},
	}
}

// TestDiamondPipeline_ResumeAfterCrash simulates a process dying right
// after node B finishes (checkpointed) but before C ever started — exactly
// the scenario PLAN.md Phase 2 describes: "kill after B completes but
// before C finishes, resume, assert C runs (not re-run if it already
// finished), B does not re-run, D waits for both then runs." The "crash"
// is simulated by directly seeding the store with exactly what a real
// crash at that point would have left behind, then reconstructing a fresh
// Pipeline via Resume — as a second process would.
func TestDiamondPipeline_ResumeAfterCrash(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")

	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	def := diamondDef()
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	// Seed state as if a prior process ran A and B (checkpointed both, with
	// their outputs saved) and then died before C ever started.
	state := domain.NewPipelineState("run-1")
	state.ProjectID = "proj1"
	state.PipelineDefID = def.ID
	state.Status = domain.StatusRunning
	state.SetOutput("A", "A-done")
	state.SetOutput("B", "B-done")
	if err := st.Save(ctx, state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.SaveCheckpoint(ctx, "run-1", "A"); err != nil {
		t.Fatalf("SaveCheckpoint A: %v", err)
	}
	if err := st.SaveCheckpoint(ctx, "run-1", "B"); err != nil {
		t.Fatalf("SaveCheckpoint B: %v", err)
	}

	var mu sync.Mutex
	ran := map[string]int{}
	countingFactory := func(agentType string, cfg map[string]any) (domain.Agent, error) {
		nodeID, _ := cfg["node_id"].(string)
		return countingAgent{nodeID: nodeID, mu: &mu, ran: ran}, nil
	}

	// "Second process": reconstruct and resume.
	p, err := engine.Resume(ctx, st, st, "run-1", countingFactory)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if p.State.Status != domain.StatusCompleted {
		t.Fatalf("expected COMPLETED, got %s", p.State.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	if ran["A"] != 0 {
		t.Fatalf("A must not re-run after resume, ran %d times", ran["A"])
	}
	if ran["B"] != 0 {
		t.Fatalf("B must not re-run after resume, ran %d times", ran["B"])
	}
	if ran["C"] != 1 {
		t.Fatalf("C must run exactly once during resume, ran %d times", ran["C"])
	}
	if ran["D"] != 1 {
		t.Fatalf("D must run exactly once during resume, ran %d times", ran["D"])
	}

	reloaded, err := st.Load(ctx, "run-1")
	if err != nil {
		t.Fatalf("Load after resume: %v", err)
	}
	if reloaded.Status != domain.StatusCompleted {
		t.Fatalf("expected persisted status COMPLETED, got %s", reloaded.Status)
	}
	if out, _ := reloaded.GetOutput("D"); out != "D-done" {
		t.Fatalf("expected D's output to be durably persisted, got %v", out)
	}
}

// TestPipeline_ResumeWhileAwaitingReview simulates a crash immediately
// after a run paused at a review gate. Resume must NOT auto-continue past
// it — that pause is waiting on a human, not on the process restarting —
// and once a (simulated) human calls Continue, the edited output must
// reach the downstream node.
func TestPipeline_ResumeWhileAwaitingReview(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")

	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	def := &domain.PipelineDef{
		ID:        "review-chain",
		ProjectID: "proj1",
		Name:      "review-chain",
		Nodes: []domain.NodeDef{
			{ID: "A", AgentType: "echo"},
			{ID: "B", AgentType: "echo", RequiresReview: true},
			{ID: "C", AgentType: "copy", Config: map[string]any{"from": "B"}},
		},
		Edges: []domain.EdgeDef{
			{From: "A", To: "B"},
			{From: "B", To: "C"},
		},
	}
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	state := domain.NewPipelineState("run-2")
	state.ProjectID = "proj1"
	state.PipelineDefID = def.ID
	state.Status = domain.StatusAwaitingReview
	state.PendingNodeID = "B"
	state.ActionToken = "tok-123"
	state.SetOutput("A", "A-out")
	state.SetOutput("B", "B-out")
	if err := st.Save(ctx, state); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.SaveCheckpoint(ctx, "run-2", "A"); err != nil {
		t.Fatalf("SaveCheckpoint A: %v", err)
	}
	if err := st.SaveCheckpoint(ctx, "run-2", "B"); err != nil {
		t.Fatalf("SaveCheckpoint B: %v", err)
	}

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)
	reg.Register("copy", func(cfg map[string]any) (domain.Agent, error) {
		nodeID, _ := cfg["node_id"].(string)
		from, _ := cfg["from"].(string)
		return copyAgent{nodeID: nodeID, from: from}, nil
	})

	p, err := engine.Resume(ctx, st, st, "run-2", reg.Create)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	if p.State.Status != domain.StatusAwaitingReview {
		t.Fatalf("Resume must not auto-continue past a review gate, got status %s", p.State.Status)
	}
	if p.State.PendingNodeID != "B" {
		t.Fatalf("expected pending node B preserved, got %q", p.State.PendingNodeID)
	}
	if p.State.ActionToken != "tok-123" {
		t.Fatalf("expected action token preserved, got %q", p.State.ActionToken)
	}
	if _, ran := p.State.GetOutput("C"); ran {
		t.Fatalf("C must not run before the review gate is resolved, even after resume")
	}

	if err := p.Continue(ctx, true, map[string]any{"B": "edited-B"}); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if p.State.Status != domain.StatusCompleted {
		t.Fatalf("expected COMPLETED after continue, got %s", p.State.Status)
	}
	if out, _ := p.State.GetOutput("C"); out != "edited-B" {
		t.Fatalf("expected downstream node C to see the edited output, got %v", out)
	}
}

type copyAgent struct {
	nodeID string
	from   string
}

func (a copyAgent) Name() string { return "copy:" + a.nodeID }

func (a copyAgent) Execute(ctx context.Context, state *domain.PipelineState) error {
	v, _ := state.GetOutput(a.from)
	state.SetOutput(a.nodeID, v)
	return nil
}
