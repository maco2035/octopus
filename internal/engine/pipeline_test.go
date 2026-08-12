package engine_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/domain"
	"octopus/internal/engine"
)

// TestDiamondDAG_RunsLevelsInParallel builds A -> {B, C} -> D and asserts
// B and C (which have no dependency on each other) actually overlap in
// execution time, proving the engine runs a DAG level concurrently rather
// than happening to execute nodes in a sequence that merely satisfies
// ordering constraints.
func TestDiamondDAG_RunsLevelsInParallel(t *testing.T) {
	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)

	var mu sync.Mutex
	starts := map[string]time.Time{}

	reg.Register("sleep-echo", func(cfg map[string]any) (domain.Agent, error) {
		nodeID, _ := cfg["node_id"].(string)
		return funcAgent{
			name: "sleep-echo:" + nodeID,
			fn: func(ctx context.Context, state *domain.PipelineState) error {
				mu.Lock()
				starts[nodeID] = time.Now()
				mu.Unlock()
				time.Sleep(100 * time.Millisecond)
				state.SetOutput(nodeID, "done")
				return nil
			},
		}, nil
	})

	def := &domain.PipelineDef{
		ID:   "diamond",
		Name: "diamond",
		Nodes: []domain.NodeDef{
			{ID: "A", AgentType: "echo"},
			{ID: "B", AgentType: "sleep-echo"},
			{ID: "C", AgentType: "sleep-echo"},
			{ID: "D", AgentType: "echo"},
		},
		Edges: []domain.EdgeDef{
			{From: "A", To: "B"},
			{From: "A", To: "C"},
			{From: "B", To: "D"},
			{From: "C", To: "D"},
		},
	}

	state := domain.NewPipelineState("run-diamond")
	p := &engine.Pipeline{Def: def, State: state, CreateAgent: reg.Create}

	if err := p.Run(context.Background(), 0); err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if state.Status != domain.StatusCompleted {
		t.Fatalf("expected COMPLETED, got %s", state.Status)
	}

	mu.Lock()
	bStart, cStart := starts["B"], starts["C"]
	mu.Unlock()

	if bStart.IsZero() || cStart.IsZero() {
		t.Fatalf("expected both B and C to have run, got starts=%v", starts)
	}

	gap := bStart.Sub(cStart)
	if gap < 0 {
		gap = -gap
	}
	if gap > 50*time.Millisecond {
		t.Fatalf("expected B and C to start within 50ms of each other (proving parallelism), got gap %v", gap)
	}
}

// TestReviewGate_PausesEditsAndContinues marks the middle node of a 3-node
// chain RequiresReview and asserts: the engine halts before the downstream
// node runs, editing the paused node's output via Continue is honored, and
// the downstream node observes the edited value once resumed.
func TestReviewGate_PausesEditsAndContinues(t *testing.T) {
	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)
	reg.Register("copy", func(cfg map[string]any) (domain.Agent, error) {
		nodeID, _ := cfg["node_id"].(string)
		from, _ := cfg["from"].(string)
		return funcAgent{
			name: "copy:" + nodeID,
			fn: func(ctx context.Context, state *domain.PipelineState) error {
				v, _ := state.GetOutput(from)
				state.SetOutput(nodeID, v)
				return nil
			},
		}, nil
	})

	def := &domain.PipelineDef{
		ID:   "review-chain",
		Name: "review-chain",
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

	state := domain.NewPipelineState("run-review")
	p := &engine.Pipeline{Def: def, State: state, CreateAgent: reg.Create}

	err := p.Run(context.Background(), 0)
	if !errors.Is(err, engine.ErrAwaitingReview) {
		t.Fatalf("expected ErrAwaitingReview, got %v", err)
	}
	if state.Status != domain.StatusAwaitingReview {
		t.Fatalf("expected AWAITING_REVIEW, got %s", state.Status)
	}
	if state.PendingNodeID != "B" {
		t.Fatalf("expected pending node B, got %q", state.PendingNodeID)
	}
	if state.ActionToken == "" {
		t.Fatalf("expected a non-empty action token while awaiting review")
	}
	if _, ran := state.GetOutput("C"); ran {
		t.Fatalf("node C must not run before the review gate is resolved")
	}

	if err := p.Continue(context.Background(), true, map[string]any{"B": "edited-plan"}); err != nil {
		t.Fatalf("continue failed: %v", err)
	}

	if state.Status != domain.StatusCompleted {
		t.Fatalf("expected COMPLETED after continue, got %s", state.Status)
	}
	if state.PendingNodeID != "" || state.ActionToken != "" {
		t.Fatalf("expected pending node/token cleared after continue, got %q / %q", state.PendingNodeID, state.ActionToken)
	}

	cOut, ok := state.GetOutput("C")
	if !ok || cOut != "edited-plan" {
		t.Fatalf("expected downstream node C to see the edited output, got %v", cOut)
	}
}

// TestReviewGate_Reject asserts a rejected review gate ends the run without
// ever executing the downstream node.
func TestReviewGate_Reject(t *testing.T) {
	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)

	def := &domain.PipelineDef{
		ID:   "reject-chain",
		Name: "reject-chain",
		Nodes: []domain.NodeDef{
			{ID: "A", AgentType: "echo", RequiresReview: true},
			{ID: "B", AgentType: "echo"},
		},
		Edges: []domain.EdgeDef{{From: "A", To: "B"}},
	}

	state := domain.NewPipelineState("run-reject")
	p := &engine.Pipeline{Def: def, State: state, CreateAgent: reg.Create}

	if err := p.Run(context.Background(), 0); !errors.Is(err, engine.ErrAwaitingReview) {
		t.Fatalf("expected ErrAwaitingReview, got %v", err)
	}

	if err := p.Continue(context.Background(), false, nil); err != nil {
		t.Fatalf("continue (reject) failed: %v", err)
	}
	if state.Status != domain.StatusRejected {
		t.Fatalf("expected REJECTED, got %s", state.Status)
	}
	if _, ran := state.GetOutput("B"); ran {
		t.Fatalf("node B must not run after rejection")
	}
}
