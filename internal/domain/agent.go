package domain

import "context"

// Agent is one node's executable behavior. It reads and mutates the shared
// PipelineState — most commonly via state.SetOutput(nodeID, ...) — rather
// than returning a value directly, since a run's state must be durable and
// inspectable independent of any single agent call.
type Agent interface {
	Name() string
	Execute(ctx context.Context, state *PipelineState) error
}
