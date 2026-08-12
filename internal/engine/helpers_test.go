package engine_test

import (
	"context"

	"octopus/internal/domain"
)

// funcAgent adapts a plain function to domain.Agent, for test fixtures that
// need behavior EchoAgent can't express (e.g. sleeping, reading another
// node's output).
type funcAgent struct {
	name string
	fn   func(ctx context.Context, state *domain.PipelineState) error
}

func (a funcAgent) Name() string { return a.name }

func (a funcAgent) Execute(ctx context.Context, state *domain.PipelineState) error {
	return a.fn(ctx, state)
}
