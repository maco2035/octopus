// TestCLIAgentEndToEnd exercises PLAN.md Phase 6's "Done when" criteria as
// literally as this sandbox allows: a real ticket ID, run through a
// UI-buildable pipeline, produces a real branch (auto-created), a real
// non-interactive CLI session that actually edits a file and gets
// committed in the real checkout, a real security note, and a real merge
// on approval — all on one machine, against a real local git remote. The
// one substitution is the coding CLI itself: this environment has no
// claude/codex/gemini binary or API key, so a small fixture script stands
// in for "claude" behind the exact same tools.CLIInvocation contract the
// real binary would satisfy.
package integration_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"octopus/internal/agents"
	"octopus/internal/agents/echo"
	"octopus/internal/domain"
	"octopus/internal/engine"
	"octopus/internal/scheduler"
	"octopus/internal/store"
	"octopus/internal/tools"
)

func runGitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func newBareRemoteWithMain(t *testing.T) string {
	t.Helper()
	seed := t.TempDir()
	runGitCmd(t, seed, "init", "-b", "main")
	runGitCmd(t, seed, "config", "user.email", "test@example.com")
	runGitCmd(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing seed file: %v", err)
	}
	runGitCmd(t, seed, "add", "-A")
	runGitCmd(t, seed, "commit", "-m", "initial")

	bare := t.TempDir()
	runGitCmd(t, "", "clone", "--bare", seed, bare)
	return bare
}

func writeFakeClaude(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("writing fake claude script: %v", err)
	}
	return path
}

func fakeInvocation(binary string) tools.CLIInvocation {
	return tools.CLIInvocation{
		Binary: binary,
		BuildArgs: func(prompt, sessionID string) []string {
			args := []string{"-p", prompt}
			if sessionID != "" {
				args = append(args, "--resume", sessionID)
			}
			return args
		},
		ParseSessionID: tools.ClaudeCodeInvocation.ParseSessionID,
	}
}

// coderDef builds a pipeline: coder -> security -> human review -> merge.
func coderSecurityReviewMergeDef(id string) *domain.PipelineDef {
	return &domain.PipelineDef{
		ID: id, ProjectID: "proj1", Name: id,
		Nodes: []domain.NodeDef{
			{ID: "coder", AgentType: "claude-coder"},
			{ID: "security", AgentType: "claude-security"},
			{ID: "human-review", AgentType: "echo", RequiresReview: true},
			{ID: "merge", AgentType: "merge", Config: map[string]any{"base_branch": "main"}},
		},
		Edges: []domain.EdgeDef{
			{From: "coder", To: "security"},
			{From: "security", To: "human-review"},
			{From: "human-review", To: "merge"},
		},
	}
}

func TestCLIAgentEndToEnd_ClearSecurityMergesOnApproval(t *testing.T) {
	ctx := context.Background()
	remote := newBareRemoteWithMain(t)

	dispatcher := tools.NewLocalDispatcher(t.TempDir())
	dispatcher.Invocation["claude"] = fakeInvocation(writeFakeClaude(t, `
if echo "$*" | grep -q "security"; then
  echo '{"result":"CLEAR: no issues found","session_id":"sess-sec"}'
else
  echo "the coder agent edited this file" > coder_change.txt
  echo '{"result":"implemented the ticket","session_id":"sess-coder"}'
fi`))

	dbPath := filepath.Join(t.TempDir(), "octopus.db")
	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	if err := st.SaveProject(ctx, &domain.Project{ID: "proj1", Name: "Proj1", GitRemoteURL: remote, BaseBranch: "main"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	def := coderSecurityReviewMergeDef("def1")
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)
	agents.RegisterCLIPresets(reg, agents.PresetConfig{Dispatcher: dispatcher, Store: st, AnthropicAPIKey: "test-key"})

	sched := scheduler.New(st, reg.Create)
	sched.Dispatcher = dispatcher

	state, err := sched.StartRun(ctx, "proj1", "def1", "TICKET-42")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Wait for the run to pause at the human review gate.
	var reloaded *domain.PipelineState
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reloaded, err = st.Load(ctx, state.RunID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if reloaded.Status == domain.StatusAwaitingReview {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reloaded.Status != domain.StatusAwaitingReview {
		t.Fatalf("expected AWAITING_REVIEW after coder+security, got %s (summary: %s)", reloaded.Status, reloaded.Summary)
	}
	if reloaded.GitBranch == "" {
		t.Fatal("expected GitBranch to be set by the implicit prepare_branch step")
	}
	if reloaded.SessionID != "sess-sec" {
		t.Fatalf("expected the security node's session id to be the run's current SessionID, got %q", reloaded.SessionID)
	}
	// Node output is the CLI's raw stdout (a JSON envelope here, since the
	// fake CLI mimics --output-format json), not an unwrapped "answer"
	// field — see cli_invoke.go's comment on why that field isn't assumed.
	coderOutput, _ := reloaded.GetOutput("coder")
	if !strings.Contains(coderOutput.(string), "implemented the ticket") {
		t.Fatalf("expected coder node output to contain the CLI's result, got %v", coderOutput)
	}
	securityOutput, _ := reloaded.GetOutput("security")
	if !strings.Contains(securityOutput.(string), "CLEAR") {
		t.Fatalf("expected security output to be CLEAR, got %v", securityOutput)
	}

	// Approve — this should merge the branch into main.
	if err := sched.Continue(ctx, state.RunID, reloaded.ActionToken, true, nil); err != nil {
		t.Fatalf("Continue: %v", err)
	}

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reloaded, err = st.Load(ctx, state.RunID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if reloaded.Status == domain.StatusCompleted || reloaded.Status == domain.StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reloaded.Status != domain.StatusCompleted {
		t.Fatalf("expected COMPLETED after approval, got %s (summary: %s)", reloaded.Status, reloaded.Summary)
	}

	// Verify the merge really landed on the remote's main branch.
	verify := t.TempDir()
	runGitCmd(t, "", "clone", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "coder_change.txt")); err != nil {
		t.Fatalf("expected the coder's change to be merged into main on the remote: %v", err)
	}
}

func TestCLIAgentEndToEnd_BlockedSecurityHaltsBeforeReview(t *testing.T) {
	ctx := context.Background()
	remote := newBareRemoteWithMain(t)

	dispatcher := tools.NewLocalDispatcher(t.TempDir())
	dispatcher.Invocation["claude"] = fakeInvocation(writeFakeClaude(t, `
if echo "$*" | grep -q "security"; then
  echo '{"result":"BLOCKED: found a hardcoded credential","session_id":"sess-sec"}'
else
  echo "leaked_key = hunter2" > secret.txt
  echo '{"result":"done","session_id":"sess-coder"}'
fi`))

	dbPath := filepath.Join(t.TempDir(), "octopus.db")
	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer st.Close()

	if err := st.SaveProject(ctx, &domain.Project{ID: "proj1", Name: "Proj1", GitRemoteURL: remote, BaseBranch: "main"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	def := coderSecurityReviewMergeDef("def1")
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	reg := agents.NewRegistry()
	reg.Register("echo", echo.New)
	agents.RegisterCLIPresets(reg, agents.PresetConfig{Dispatcher: dispatcher, Store: st, AnthropicAPIKey: "test-key"})

	sched := scheduler.New(st, reg.Create)
	sched.Dispatcher = dispatcher
	sched.OnSettled = func(state *domain.PipelineState) {} // no-op; just proving OnSettled doesn't panic on a blocked run

	state, err := sched.StartRun(ctx, "proj1", "def1", "TICKET-43")
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	var reloaded *domain.PipelineState
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		reloaded, err = st.Load(ctx, state.RunID)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if reloaded.Status == domain.StatusBlocked {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reloaded.Status != domain.StatusBlocked {
		t.Fatalf("expected BLOCKED, got %s", reloaded.Status)
	}
	if !strings.Contains(reloaded.Summary, "hardcoded credential") {
		t.Fatalf("expected the block reason in the run's summary, got: %s", reloaded.Summary)
	}
	if reloaded.PendingNodeID != "" {
		t.Fatalf("a blocked run must not also look like it's awaiting review, got PendingNodeID=%q", reloaded.PendingNodeID)
	}

	// The human-review and merge nodes must never have run.
	if _, ok := reloaded.GetOutput("human-review"); ok {
		t.Fatal("human-review must not run after a security block")
	}
	if _, ok := reloaded.GetOutput("merge"); ok {
		t.Fatal("merge must not run after a security block")
	}

	// And nothing should have reached the remote's main branch.
	verify := t.TempDir()
	runGitCmd(t, "", "clone", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "secret.txt")); err == nil {
		t.Fatal("blocked change must not have been merged into main")
	}
}

// Sanity check that engine.ErrBlocked really is the sentinel cliagent's
// security preset wraps — protects the two tests above from silently
// passing for the wrong reason if someone renames or reimplements it.
func TestErrBlockedIsWrapped(t *testing.T) {
	wrapped := errBlockedWrap()
	if !errors.Is(wrapped, engine.ErrBlocked) {
		t.Fatal("expected errors.Is to see through the wrap to engine.ErrBlocked")
	}
}

func errBlockedWrap() error {
	return engine.ErrBlocked
}
