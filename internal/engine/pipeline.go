package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"golang.org/x/sync/errgroup"

	"octopus/internal/domain"
)

// ErrAwaitingReview is returned by Run when the pipeline has paused at a
// node flagged RequiresReview. It is not a failure: the caller should
// persist state and call Continue once a human has responded.
var ErrAwaitingReview = errors.New("pipeline paused: awaiting review")

// ErrBlocked is what a security-preset cliagent node wraps its returned
// error in in to halt the run outright (PLAN.md Phase 6: "Security agent
// ... can set StatusBlocked; engine short-circuits and notifies"), rather
// than a plain node failure (StatusFailed) or a review pause
// (StatusAwaitingReview) — a blocked run needs its own status because
// "the security check found a real problem" reads differently, in the web
// UI and in Slack, than "a node crashed" or "waiting on a human to sign
// off."
var ErrBlocked = errors.New("pipeline blocked: security check failed")

// ErrAwaitingRunner is what a git-tool node (cliagent, mergeAgent) wraps
// domain.ErrNoRunnerAvailable in when no octopus-runner is currently
// connected to serve its project (PLAN.md Phase 7). Like ErrAwaitingReview,
// this is not a failure — the node deliberately isn't checkpointed, so a
// later Resume (triggered automatically by runnerhub.Hub the moment a
// matching runner connects) re-executes exactly this node rather than
// skipping it or duplicating earlier work.
var ErrAwaitingRunner = errors.New("pipeline paused: awaiting runner")

// AgentFactory matches agents.Registry.Create's signature exactly, so a
// *agents.Registry can be passed directly as a Pipeline's CreateAgent.
type AgentFactory func(agentType string, cfg map[string]any) (domain.Agent, error)

// Checkpointer is the subset of Store a Pipeline needs to persist progress.
// A *store.SQLiteStore satisfies this structurally — engine deliberately
// doesn't import the store package, so it stays testable without a DB.
type Checkpointer interface {
	Save(ctx context.Context, state *domain.PipelineState) error
	SaveCheckpoint(ctx context.Context, runID, completedNodeID string) error
}

type Pipeline struct {
	Def         *domain.PipelineDef
	State       *domain.PipelineState
	CreateAgent AgentFactory
	Checkpoint  Checkpointer // nil is fine — checkpointing becomes a no-op (e.g. Phase 1 tests)
}

// Run executes the DAG level by level. completed is the set of node IDs
// that have already finished in a prior call (nil/empty for a fresh run) —
// their nodes are skipped rather than re-executed, which is what makes
// Resume safe after a crash mid-level: nodes that already checkpointed
// don't run twice, and nodes that never got to run do. Nodes within a
// level that do need to run execute concurrently. Run returns
// ErrAwaitingReview if it pauses at a review gate, nil on full completion,
// or another error if an agent fails.
func (p *Pipeline) Run(ctx context.Context, completed map[string]bool) error {
	if completed == nil {
		completed = map[string]bool{}
	}

	levels, err := Levels(p.Def)
	if err != nil {
		return err
	}

	nodeByID := make(map[string]domain.NodeDef, len(p.Def.Nodes))
	for _, n := range p.Def.Nodes {
		nodeByID[n.ID] = n
	}

	p.State.Status = domain.StatusRunning
	if err := p.persist(ctx); err != nil {
		return err
	}

	for _, level := range levels {
		var toRun []string
		for _, id := range level {
			if !completed[id] {
				toRun = append(toRun, id)
			}
		}

		// Every node in this level already finished in a prior call (e.g.
		// Continue resuming past a level that just passed its review gate,
		// or Resume replaying levels before the crash point) — nothing to
		// run, and this level's review-gate decision (if any) was already
		// made back when it first completed. Move straight to the next.
		if len(toRun) == 0 {
			continue
		}

		g, gctx := errgroup.WithContext(ctx)
		for _, nodeID := range toRun {
			node := nodeByID[nodeID]
			g.Go(func() error {
				cfg := make(map[string]any, len(node.Config)+1)
				for k, v := range node.Config {
					cfg[k] = v
				}
				cfg["node_id"] = node.ID

				agent, err := p.CreateAgent(node.AgentType, cfg)
				if err != nil {
					return fmt.Errorf("node %s: %w", node.ID, err)
				}
				if err := agent.Execute(gctx, p.State); err != nil {
					return fmt.Errorf("node %s: %w", node.ID, err)
				}
				if err := p.checkpointNode(ctx, node.ID); err != nil {
					return fmt.Errorf("node %s: checkpoint: %w", node.ID, err)
				}
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			switch {
			case errors.Is(err, ErrBlocked):
				p.State.Status = domain.StatusBlocked
				p.State.Summary = err.Error()
			case errors.Is(err, ErrAwaitingRunner):
				p.State.Status = domain.StatusAwaitingRunner
			default:
				p.State.Status = domain.StatusFailed
			}
			_ = p.persist(ctx)
			return err
		}

		if reviewNodeID, ok := firstReviewNode(level, nodeByID); ok {
			p.State.Status = domain.StatusAwaitingReview
			p.State.PendingNodeID = reviewNodeID
			p.State.ActionToken = newActionToken()
			if err := p.persist(ctx); err != nil {
				return err
			}
			return ErrAwaitingReview
		}
	}

	p.State.Status = domain.StatusCompleted
	return p.persist(ctx)
}

// Continue resolves a review-gate pause. approve=false rejects the run
// (terminal). approve=true applies editedOutputs (if any) on top of the
// pending node's output and resumes the DAG from the level after it —
// every node up to and including the paused level is, by construction,
// already checkpointed (the pause only happens once its whole level
// finishes), so it's passed to Run as already-completed.
func (p *Pipeline) Continue(ctx context.Context, approve bool, editedOutputs map[string]any) error {
	if p.State.Status != domain.StatusAwaitingReview {
		return fmt.Errorf("run %s is not awaiting review", p.State.RunID)
	}

	if !approve {
		p.State.Status = domain.StatusRejected
		p.State.PendingNodeID = ""
		p.State.ActionToken = ""
		return p.persist(ctx)
	}

	for k, v := range editedOutputs {
		p.State.SetOutput(k, v)
	}

	pendingNodeID := p.State.PendingNodeID
	p.State.PendingNodeID = ""
	p.State.ActionToken = ""

	levels, err := Levels(p.Def)
	if err != nil {
		return err
	}

	levelIdx := -1
	for i, lvl := range levels {
		for _, id := range lvl {
			if id == pendingNodeID {
				levelIdx = i
			}
		}
	}
	if levelIdx == -1 {
		return fmt.Errorf("pending node %q not found in pipeline %q", pendingNodeID, p.Def.ID)
	}

	completed := map[string]bool{}
	for i := 0; i <= levelIdx; i++ {
		for _, id := range levels[i] {
			completed[id] = true
		}
	}

	return p.Run(ctx, completed)
}

func (p *Pipeline) checkpointNode(ctx context.Context, nodeID string) error {
	if p.Checkpoint == nil {
		return nil
	}
	if err := p.Checkpoint.Save(ctx, p.State); err != nil {
		return err
	}
	return p.Checkpoint.SaveCheckpoint(ctx, p.State.RunID, nodeID)
}

func (p *Pipeline) persist(ctx context.Context) error {
	if p.Checkpoint == nil {
		return nil
	}
	return p.Checkpoint.Save(ctx, p.State)
}

func firstReviewNode(level []string, nodeByID map[string]domain.NodeDef) (string, bool) {
	for _, id := range level {
		if nodeByID[id].RequiresReview {
			return id, true
		}
	}
	return "", false
}

func newActionToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
