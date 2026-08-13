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

// Snapshot returns a copy of NodeOutputs, safe for a caller outside this
// package (the Store, serializing state for a checkpoint) to read without
// racing a concurrent SetOutput from another node's goroutine in the same
// DAG level. Reading s.NodeOutputs directly from another package would be
// a data race — always go through this or GetOutput/SetOutput instead.
func (s *PipelineState) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := make(map[string]any, len(s.NodeOutputs))
	for k, v := range s.NodeOutputs {
		cp[k] = v
	}
	return cp
}
