// Package store defines the Store interface (PLAN.md §5) — every piece of
// durable state Octopus needs lives behind it, so a process restart never
// loses a run mid-flight.
package store

import (
	"context"
	"errors"

	"octopus/internal/domain"
)

// ErrNotFound is returned by Load-style methods when nothing matches.
var ErrNotFound = errors.New("not found")

// ErrStaleActionToken is returned by ResolveReview when the given token
// doesn't match the run's current pending action — either it's already
// been used, or the run has moved on. Callers should treat this as a
// friendly no-op (PLAN.md Key Design Decision 6), not a hard failure.
var ErrStaleActionToken = errors.New("stale or already-resolved action token")

type Store interface {
	SaveProject(ctx context.Context, p *domain.Project) error
	LoadProject(ctx context.Context, id string) (*domain.Project, error)
	ListProjects(ctx context.Context) ([]*domain.Project, error)

	SavePipelineDef(ctx context.Context, d *domain.PipelineDef) error
	LoadPipelineDef(ctx context.Context, id string) (*domain.PipelineDef, error)
	ListPipelineDefs(ctx context.Context, projectID string) ([]*domain.PipelineDef, error)

	SaveWorkItem(ctx context.Context, item *domain.WorkItem) error
	LoadWorkItem(ctx context.Context, id string) (*domain.WorkItem, error)
	ListWorkItems(ctx context.Context, projectID string) ([]*domain.WorkItem, error)

	Save(ctx context.Context, state *domain.PipelineState) error
	Load(ctx context.Context, runID string) (*domain.PipelineState, error)
	ListActiveRuns(ctx context.Context) ([]*domain.PipelineState, error) // across all projects, for the scheduler + UI

	SaveCheckpoint(ctx context.Context, runID, completedNodeID string) error // appends to the completed set
	LoadCheckpoint(ctx context.Context, runID string) ([]string, error)

	ResolveReview(ctx context.Context, runID, actionToken string, approve bool, editedOutputs map[string]any) error

	SaveRunner(ctx context.Context, r *domain.Runner) error
	ListRunners(ctx context.Context) ([]*domain.Runner, error)
	TouchRunnerHeartbeat(ctx context.Context, runnerID string) error
	DeleteRunner(ctx context.Context, runnerID string) error // revocation (PLAN.md §8: "rotate/revoke from the web UI if a machine is decommissioned")

	SaveGitJob(ctx context.Context, job *domain.GitJob) error                               // called before dispatch, so it survives a server restart
	LoadPendingGitJobs(ctx context.Context) ([]*domain.GitJob, error)                       // reloaded by runnerhub on boot
	LoadPendingGitJobFor(ctx context.Context, runID, nodeID string) (*domain.GitJob, error) // ErrNotFound if this node has no unresolved job — the re-run-after-restart check (Key Design Decision 19)
	ResolveGitJob(ctx context.Context, jobID string, result *domain.GitJobResult) error
	LoadGitJobResult(ctx context.Context, jobID string) (*domain.GitJobResult, error) // ErrNotFound if jobID doesn't exist or isn't resolved yet — lets AwaitingDispatcher.Await check "did this already finish while I wasn't looking" (e.g. across a server restart) before registering a fresh waiter

	LoadUserByUsername(ctx context.Context, username string) (*domain.User, error)
	LoadUserByID(ctx context.Context, id string) (*domain.User, error) // used to resolve Session.UserID back to a User
	SaveUser(ctx context.Context, u *domain.User) error
	ListUsers(ctx context.Context) ([]*domain.User, error)

	SaveSession(ctx context.Context, s *domain.Session) error
	LoadSession(ctx context.Context, token string) (*domain.Session, error) // caller checks ExpiresAt; an expired-but-present row is not treated as ErrNotFound here
	DeleteSession(ctx context.Context, token string) error                  // logout

	Close() error
}
