package domain

import "time"

// WorkItem is the durable board-level unit of work. A run is an execution
// attempt; a work item owns its ticket branch, assignment, and integration
// order across attempts.
type WorkItem struct {
	ID               string
	ProjectID        string
	TicketID         string
	Title            string
	Description      string
	Kind             WorkKind
	PipelineDefID    string
	AssignedRunnerID string // empty means any eligible runner
	Branch           string
	QueuePosition    int
	RunID            string
	CreatedAt        time.Time
}

type WorkKind string

const (
	WorkKindChange   WorkKind = "change"
	WorkKindResearch WorkKind = "research"
)
