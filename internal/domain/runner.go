package domain

import "time"

// Runner is a dev machine's octopus-runner registration (PLAN.md Key
// Design Decision 14) — it connects outbound and declares which projects
// it can do git work for.
type Runner struct {
	ID         string
	Name       string // hostname or user-given label
	TokenHash  string // shared secret, hashed at rest, generated from the web UI
	ProjectIDs []string
	LastSeen   time.Time
}

// GitJob is one unit of git/shell work dispatched to a runner.
type GitJob struct {
	ID        string
	RunID     string
	ProjectID string
	Type      string // "prepare_branch" | "diff" | "apply_patch" | "commit" | "push" | "merge" | "shell_exec"
	Payload   map[string]any
}

// GitJobResult carries a runner's full output back, not just success/fail
// — this is what makes logs visible centrally regardless of which machine
// ran the job (Key Design Decision 16).
type GitJobResult struct {
	JobID   string
	Success bool
	Output  string
	Error   string
}
