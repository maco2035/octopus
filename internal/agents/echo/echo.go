// Package echo provides EchoAgent, a no-op agent used to exercise the DAG
// engine (parallel fan-out, review-gate pause/resume) without any network
// calls. It writes its configured "output" (or its own node ID, by
// default) into the shared PipelineState.
package echo

import (
	"context"
	"fmt"
	"time"

	"octopus/internal/domain"
)

type Agent struct {
	nodeID string
	output any
	delay  time.Duration
}

// New builds an EchoAgent from cfg. An optional "delay_ms" key makes it
// sleep before writing its output — used by tests (Phase 1's parallel-
// fanout timing test, Phase 3's cross-project concurrency test) to prove
// real overlap in time rather than accidental sequencing; zero by default,
// so ordinary use is instant.
func New(cfg map[string]any) (domain.Agent, error) {
	nodeID, _ := cfg["node_id"].(string)
	if nodeID == "" {
		return nil, fmt.Errorf("echo agent requires node_id in config")
	}

	var output any = nodeID
	if v, ok := cfg["output"]; ok {
		output = v
	}

	var delay time.Duration
	switch v := cfg["delay_ms"].(type) {
	case int:
		delay = time.Duration(v) * time.Millisecond
	case int64:
		delay = time.Duration(v) * time.Millisecond
	case float64:
		delay = time.Duration(v) * time.Millisecond
	}

	return &Agent{nodeID: nodeID, output: output, delay: delay}, nil
}

func (a *Agent) Name() string { return "echo:" + a.nodeID }

func (a *Agent) Execute(ctx context.Context, state *domain.PipelineState) error {
	if a.delay > 0 {
		select {
		case <-time.After(a.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	state.SetOutput(a.nodeID, a.output)
	return nil
}
