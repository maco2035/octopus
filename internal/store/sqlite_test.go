package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"octopus/internal/domain"
	"octopus/internal/store"
)

func newTestStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")
	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestProject_SaveLoadList(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	p := &domain.Project{ID: "proj1", Name: "Octopus", GitRemoteURL: "git@github.com:x/y.git", BaseBranch: "main"}
	if err := st.SaveProject(ctx, p); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}

	loaded, err := st.LoadProject(ctx, "proj1")
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if *loaded != *p {
		t.Fatalf("loaded project mismatch: got %+v, want %+v", loaded, p)
	}

	// Upsert: save again with a changed field.
	p.BaseBranch = "1.0.2"
	if err := st.SaveProject(ctx, p); err != nil {
		t.Fatalf("SaveProject (update): %v", err)
	}
	loaded, err = st.LoadProject(ctx, "proj1")
	if err != nil {
		t.Fatalf("LoadProject after update: %v", err)
	}
	if loaded.BaseBranch != "1.0.2" {
		t.Fatalf("expected updated base branch, got %q", loaded.BaseBranch)
	}

	list, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 project, got %d", len(list))
	}

	if _, err := st.LoadProject(ctx, "does-not-exist"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestPipelineDef_SaveLoadList(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	def := &domain.PipelineDef{
		ID:        "def1",
		ProjectID: "proj1",
		Name:      "main pipeline",
		Nodes: []domain.NodeDef{
			{ID: "A", AgentType: "echo", Config: map[string]any{"output": "hi"}, RequiresReview: true},
		},
		Edges: []domain.EdgeDef{},
	}
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	loaded, err := st.LoadPipelineDef(ctx, "def1")
	if err != nil {
		t.Fatalf("LoadPipelineDef: %v", err)
	}
	if len(loaded.Nodes) != 1 || loaded.Nodes[0].ID != "A" || !loaded.Nodes[0].RequiresReview {
		t.Fatalf("loaded def nodes mismatch: %+v", loaded.Nodes)
	}
	if loaded.Nodes[0].Config["output"] != "hi" {
		t.Fatalf("expected node config to round-trip, got %+v", loaded.Nodes[0].Config)
	}

	list, err := st.ListPipelineDefs(ctx, "proj1")
	if err != nil {
		t.Fatalf("ListPipelineDefs: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 pipeline def for proj1, got %d", len(list))
	}

	empty, err := st.ListPipelineDefs(ctx, "no-such-project")
	if err != nil {
		t.Fatalf("ListPipelineDefs (empty): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected 0 pipeline defs, got %d", len(empty))
	}
}

func TestPipelineState_SaveLoadAndActiveRuns(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	running := domain.NewPipelineState("run-active")
	running.ProjectID = "proj1"
	running.Status = domain.StatusRunning
	running.SetOutput("A", "a-out")
	if err := st.Save(ctx, running); err != nil {
		t.Fatalf("Save running: %v", err)
	}

	done := domain.NewPipelineState("run-done")
	done.ProjectID = "proj1"
	done.Status = domain.StatusCompleted
	if err := st.Save(ctx, done); err != nil {
		t.Fatalf("Save done: %v", err)
	}

	loaded, err := st.Load(ctx, "run-active")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if out, ok := loaded.GetOutput("A"); !ok || out != "a-out" {
		t.Fatalf("expected NodeOutputs to round-trip, got %v", loaded.NodeOutputs)
	}

	active, err := st.ListActiveRuns(ctx)
	if err != nil {
		t.Fatalf("ListActiveRuns: %v", err)
	}
	if len(active) != 1 || active[0].RunID != "run-active" {
		t.Fatalf("expected only run-active to be active, got %+v", active)
	}

	if _, err := st.Load(ctx, "does-not-exist"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCheckpoint_SaveLoadIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	for i := 0; i < 2; i++ { // duplicate calls must not error or duplicate entries
		if err := st.SaveCheckpoint(ctx, "run-1", "A"); err != nil {
			t.Fatalf("SaveCheckpoint (attempt %d): %v", i, err)
		}
	}
	if err := st.SaveCheckpoint(ctx, "run-1", "B"); err != nil {
		t.Fatalf("SaveCheckpoint B: %v", err)
	}

	ids, err := st.LoadCheckpoint(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadCheckpoint: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 checkpointed nodes, got %v", ids)
	}
}

func TestResolveReview_ApproveRejectAndStaleToken(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	state := domain.NewPipelineState("run-review")
	state.ProjectID = "proj1"
	state.Status = domain.StatusAwaitingReview
	state.PendingNodeID = "B"
	state.ActionToken = "tok-1"
	state.SetOutput("B", "original")
	if err := st.Save(ctx, state); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Wrong token is a stale no-op, not a hard error path the caller can't detect.
	if err := st.ResolveReview(ctx, "run-review", "wrong-token", true, nil); !errors.Is(err, store.ErrStaleActionToken) {
		t.Fatalf("expected ErrStaleActionToken for wrong token, got %v", err)
	}

	if err := st.ResolveReview(ctx, "run-review", "tok-1", true, map[string]any{"B": "edited"}); err != nil {
		t.Fatalf("ResolveReview approve: %v", err)
	}

	reloaded, err := st.Load(ctx, "run-review")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded.Status != domain.StatusRunning {
		t.Fatalf("expected RUNNING after approve, got %s", reloaded.Status)
	}
	if reloaded.PendingNodeID != "" || reloaded.ActionToken != "" {
		t.Fatalf("expected pending fields cleared, got %q / %q", reloaded.PendingNodeID, reloaded.ActionToken)
	}
	if out, _ := reloaded.GetOutput("B"); out != "edited" {
		t.Fatalf("expected edited output persisted, got %v", out)
	}

	// A second click on the now-stale (already-used) token must be a no-op, not a second merge attempt.
	if err := st.ResolveReview(ctx, "run-review", "tok-1", true, nil); !errors.Is(err, store.ErrStaleActionToken) {
		t.Fatalf("expected ErrStaleActionToken on reuse, got %v", err)
	}

	// Separate run: rejection path.
	state2 := domain.NewPipelineState("run-reject")
	state2.Status = domain.StatusAwaitingReview
	state2.PendingNodeID = "X"
	state2.ActionToken = "tok-2"
	if err := st.Save(ctx, state2); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := st.ResolveReview(ctx, "run-reject", "tok-2", false, nil); err != nil {
		t.Fatalf("ResolveReview reject: %v", err)
	}
	reloaded2, err := st.Load(ctx, "run-reject")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if reloaded2.Status != domain.StatusRejected {
		t.Fatalf("expected REJECTED, got %s", reloaded2.Status)
	}
}

func TestRunner_SaveListHeartbeat(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	r := &domain.Runner{ID: "runner1", Name: "dev-laptop", TokenHash: "hashed", ProjectIDs: []string{"proj1", "proj2"}}
	if err := st.SaveRunner(ctx, r); err != nil {
		t.Fatalf("SaveRunner: %v", err)
	}

	list, err := st.ListRunners(ctx)
	if err != nil {
		t.Fatalf("ListRunners: %v", err)
	}
	if len(list) != 1 || len(list[0].ProjectIDs) != 2 {
		t.Fatalf("expected 1 runner with 2 project IDs, got %+v", list)
	}

	if err := st.TouchRunnerHeartbeat(ctx, "runner1"); err != nil {
		t.Fatalf("TouchRunnerHeartbeat: %v", err)
	}
	list, err = st.ListRunners(ctx)
	if err != nil {
		t.Fatalf("ListRunners after heartbeat: %v", err)
	}
	if list[0].LastSeen.IsZero() {
		t.Fatalf("expected LastSeen to be set after heartbeat")
	}

	if err := st.TouchRunnerHeartbeat(ctx, "no-such-runner"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestGitJob_SaveLoadPendingResolveIdempotent(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	job := &domain.GitJob{ID: "job1", RunID: "run1", ProjectID: "proj1", Type: "prepare_branch", Payload: map[string]any{"branch": "octopus/T-1"}}
	if err := st.SaveGitJob(ctx, job); err != nil {
		t.Fatalf("SaveGitJob: %v", err)
	}

	pending, err := st.LoadPendingGitJobs(ctx)
	if err != nil {
		t.Fatalf("LoadPendingGitJobs: %v", err)
	}
	if len(pending) != 1 || pending[0].Payload["branch"] != "octopus/T-1" {
		t.Fatalf("expected 1 pending job with payload round-tripped, got %+v", pending)
	}

	result := &domain.GitJobResult{JobID: "job1", Success: true, Output: "branch created"}
	if err := st.ResolveGitJob(ctx, "job1", result); err != nil {
		t.Fatalf("ResolveGitJob: %v", err)
	}

	// Idempotent: resolving again (simulating a redelivered result) must not error.
	if err := st.ResolveGitJob(ctx, "job1", result); err != nil {
		t.Fatalf("ResolveGitJob (redelivered): %v", err)
	}

	pending, err = st.LoadPendingGitJobs(ctx)
	if err != nil {
		t.Fatalf("LoadPendingGitJobs after resolve: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending jobs after resolve, got %d", len(pending))
	}

	if err := st.ResolveGitJob(ctx, "no-such-job", result); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestUser_SeededFromConstructor(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "octopus.db")

	st, err := store.New(dbPath, "admin", "bcrypt-hash-placeholder")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	u, err := st.LoadUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("LoadUserByUsername: %v", err)
	}
	if u.PasswordHash != "bcrypt-hash-placeholder" {
		t.Fatalf("expected seeded password hash, got %q", u.PasswordHash)
	}

	if _, err := st.LoadUserByUsername(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestSession_SaveLoadDelete(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	sess := &domain.Session{Token: "tok-abc", UserID: "admin", ExpiresAt: time.Now().Add(time.Hour)}
	if err := st.SaveSession(ctx, sess); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	loaded, err := st.LoadSession(ctx, "tok-abc")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if loaded.UserID != "admin" {
		t.Fatalf("expected UserID admin, got %q", loaded.UserID)
	}

	if err := st.DeleteSession(ctx, "tok-abc"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := st.LoadSession(ctx, "tok-abc"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
}
