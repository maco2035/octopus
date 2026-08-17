package domain

type Status string

const (
	StatusPending        Status = "PENDING"
	StatusRunning        Status = "RUNNING"
	StatusBlocked        Status = "BLOCKED"         // security node halted it
	StatusAwaitingRunner Status = "AWAITING_RUNNER" // a git job is queued, no connected runner for this project yet
	StatusAwaitingReview Status = "AWAITING_REVIEW" // paused at a node flagged RequiresReview
	StatusNeedsHuman     Status = "NEEDS_HUMAN"     // remediation loop limit reached at a review gate
	StatusRejected       Status = "REJECTED"        // a human rejected at a review gate
	StatusCompleted      Status = "COMPLETED"
	StatusFailed         Status = "FAILED"
	StatusCancelled      Status = "CANCELLED"
)
