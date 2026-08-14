package engine

import (
	"context"
	"errors"
	"fmt"

	"octopus/internal/domain"
)

// ResumeStore is the subset of Store Resume needs to reconstruct a run.
// Structurally satisfied by *store.SQLiteStore — see Checkpointer for why
// engine doesn't import the store package directly.
type ResumeStore interface {
	Load(ctx context.Context, runID string) (*domain.PipelineState, error)
	LoadPipelineDef(ctx context.Context, id string) (*domain.PipelineDef, error)
	LoadCheckpoint(ctx context.Context, runID string) ([]string, error)
}

// Resume reconstructs a run from durable state after a restart (crash,
// deploy, whatever) and, if it was genuinely mid-flight, continues
// executing it — nodes already in the checkpoint set are skipped, so nodes
// that finished before the restart never run twice.
//
// If the run was paused at a review gate (StatusAwaitingReview) or waiting
// on a runner (StatusAwaitingRunner), Resume does NOT auto-continue it —
// those states are waiting on an external actor (a human, a runner
// reconnecting), not on the process having restarted, so the reconstructed
// Pipeline is returned as-is, still paused, ready for a later Continue
// call. Terminal states (Completed/Failed/Rejected/Cancelled) are likewise
// returned untouched — there's nothing left to run.
func Resume(ctx context.Context, s ResumeStore, cp Checkpointer, runID string, createAgent AgentFactory) (*Pipeline, error) {
	state, err := s.Load(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("loading run %s: %w", runID, err)
	}

	def, err := s.LoadPipelineDef(ctx, state.PipelineDefID)
	if err != nil {
		return nil, fmt.Errorf("loading pipeline def %s for run %s: %w", state.PipelineDefID, runID, err)
	}

	completedIDs, err := s.LoadCheckpoint(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("loading checkpoint for run %s: %w", runID, err)
	}

	p := &Pipeline{Def: def, State: state, CreateAgent: createAgent, Checkpoint: cp}

	switch state.Status {
	case domain.StatusPending, domain.StatusRunning:
		completed := make(map[string]bool, len(completedIDs))
		for _, id := range completedIDs {
			completed[id] = true
		}
		if err := p.Run(ctx, completed); err != nil && !errors.Is(err, ErrAwaitingReview) {
			return p, err
		}
		return p, nil
	default:
		// AwaitingReview, AwaitingRunner, Blocked, or a terminal status —
		// nothing for Resume itself to run; the caller (a human via
		// Continue, a reconnecting runner, whoever) drives what happens
		// next.
		return p, nil
	}
}
