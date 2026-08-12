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
// persist state (Phase 2) and call Continue once a human has responded.
var ErrAwaitingReview = errors.New("pipeline paused: awaiting review")

// AgentFactory matches agents.Registry.Create's signature exactly, so a
// *agents.Registry can be passed directly as a Pipeline's CreateAgent.
type AgentFactory func(agentType string, cfg map[string]any) (domain.Agent, error)

type Pipeline struct {
	Def         *domain.PipelineDef
	State       *domain.PipelineState
	CreateAgent AgentFactory
}

// Run executes the DAG level by level starting at fromLevel (0 for a fresh
// run). Nodes within a level run concurrently. It returns ErrAwaitingReview
// if it pauses at a review gate, nil on full completion, or another error
// if an agent fails.
func (p *Pipeline) Run(ctx context.Context, fromLevel int) error {
	levels, err := Levels(p.Def)
	if err != nil {
		return err
	}

	nodeByID := make(map[string]domain.NodeDef, len(p.Def.Nodes))
	for _, n := range p.Def.Nodes {
		nodeByID[n.ID] = n
	}

	p.State.Status = domain.StatusRunning

	for li := fromLevel; li < len(levels); li++ {
		level := levels[li]

		g, gctx := errgroup.WithContext(ctx)
		for _, nodeID := range level {
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
				return nil
			})
		}

		if err := g.Wait(); err != nil {
			p.State.Status = domain.StatusFailed
			return err
		}

		if reviewNodeID, ok := firstReviewNode(level, nodeByID); ok {
			p.State.Status = domain.StatusAwaitingReview
			p.State.PendingNodeID = reviewNodeID
			p.State.ActionToken = newActionToken()
			return ErrAwaitingReview
		}
	}

	p.State.Status = domain.StatusCompleted
	return nil
}

// Continue resolves a review-gate pause. approve=false rejects the run
// (terminal). approve=true applies editedOutputs (if any) on top of the
// pending node's output and resumes the DAG from the level after it.
func (p *Pipeline) Continue(ctx context.Context, approve bool, editedOutputs map[string]any) error {
	if p.State.Status != domain.StatusAwaitingReview {
		return fmt.Errorf("run %s is not awaiting review", p.State.RunID)
	}

	if !approve {
		p.State.Status = domain.StatusRejected
		p.State.PendingNodeID = ""
		p.State.ActionToken = ""
		return nil
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

	return p.Run(ctx, levelIdx+1)
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
