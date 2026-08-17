package domain

import "sync"

// PipelineState is a single run's shared, mutable state. Multiple agents
// can execute concurrently against the same *PipelineState within a DAG
// level, so NodeOutputs is guarded by a mutex rather than accessed as a
// bare map — agents must go through SetOutput/GetOutput, never touch
// NodeOutputs directly.
type PipelineState struct {
	RunID            string
	ProjectID        string
	PipelineDefID    string
	WorkItemID       string
	TicketID         string
	GitBranch        string
	AssignedRunnerID string
	ReviewLoops      int
	SessionID        string // coding-agent session to resume; seeded from a prior run when this one is an explicit continuation (Key Design Decision 27)
	Status           Status
	PendingNodeID    string
	ActionToken      string
	Summary          string

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

// SetSessionID and GetSessionID guard SessionID the same way SetOutput and
// GetOutput guard NodeOutputs: multiple cliagent nodes (Phase 6) can run
// concurrently within one DAG level, and a bare field read racing a bare
// field write there is a real data race, not just a style nit. Code that
// only ever touches SessionID between levels (the scheduler seeding it
// before a run starts, the Store persisting it after a level fully
// finishes) can still use the bare field safely — but any read or write
// that might happen while nodes are executing must go through these.
func (s *PipelineState) SetSessionID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SessionID = id
}

func (s *PipelineState) GetSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.SessionID
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
