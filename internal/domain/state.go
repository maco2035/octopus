package domain

import "sync"

// PipelineState is a single run's shared, mutable state. Multiple agents
// can execute concurrently against the same *PipelineState within a DAG
// level, so NodeOutputs is guarded by a mutex rather than accessed as a
// bare map — agents must go through SetOutput/GetOutput, never touch
// NodeOutputs directly.
type PipelineState struct {
	RunID         string
	ProjectID     string
	PipelineDefID string
	TicketID      string
	GitBranch     string
	Status        Status
	PendingNodeID string
	ActionToken   string
	Summary       string

	mu          sync.Mutex
	NodeOutputs map[string]any
}

func NewPipelineState(runID string) *PipelineState {
	return &PipelineState{
		RunID:       runID,
		Status:      StatusPending,
		NodeOutputs: make(map[string]any),
	}
}

func (s *PipelineState) SetOutput(nodeID string, value any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.NodeOutputs[nodeID] = value
}

func (s *PipelineState) GetOutput(nodeID string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.NodeOutputs[nodeID]
	return v, ok
}
