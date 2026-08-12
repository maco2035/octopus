// Package echo provides EchoAgent, a no-op agent used to exercise the DAG
// engine (parallel fan-out, review-gate pause/resume) without any network
// calls. It writes its configured "output" (or its own node ID, by
// default) into the shared PipelineState.
package echo

import (
	"context"
	"fmt"

	"octopus/internal/domain"
)

type Agent struct {
	nodeID string
	output any
}

func New(cfg map[string]any) (domain.Agent, error) {
	nodeID, _ := cfg["node_id"].(string)
	if nodeID == "" {
		return nil, fmt.Errorf("echo agent requires node_id in config")
	}

	var output any = nodeID
	if v, ok := cfg["output"]; ok {
		output = v
	}

	return &Agent{nodeID: nodeID, output: output}, nil
}

func (a *Agent) Name() string { return "echo:" + a.nodeID }

func (a *Agent) Execute(ctx context.Context, state *domain.PipelineState) error {
	state.SetOutput(a.nodeID, a.output)
	return nil
}
