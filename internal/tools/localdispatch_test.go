package tools_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"octopus/internal/domain"
	"octopus/internal/tools"
)

func TestLocalDispatcher_PrepareBranchThenRunAgentCommitsAndPushes(t *testing.T) {
	ctx := context.Background()
	remote := newBareRemote(t, "main")

	d := tools.NewLocalDispatcher(t.TempDir())
	// Swap in the fake CLI for "claude" so this test doesn't need the real
	// binary or an API key — same Invocation contract either way.
	fakeCLIScript := `echo "agent wrote this" > agent_output.txt; echo '{"result":"done","session_id":"sess-1"}'`
	d.Invocation["claude"] = tools.CLIInvocation{
		Binary: writeFakeCLI(t, fakeCLIScript),
		BuildArgs: func(prompt, sessionID string) []string {
			args := []string{"-p", prompt}
			if sessionID != "" {
				args = append(args, "--resume", sessionID)
			}
			return args
		},
		ParseSessionID: tools.ClaudeCodeInvocation.ParseSessionID,
	}

	prepareJob := &domain.GitJob{
		ID: "job-prepare", RunID: "run1", ProjectID: "proj1", Type: "prepare_branch",
		Payload: map[string]any{"remote_url": remote, "base_branch": "main", "branch_name": "octopus/TICKET-1"},
	}
	result, err := d.Dispatch(ctx, prepareJob)
	if err != nil {
		t.Fatalf("Dispatch prepare_branch: %v", err)
	}
	if !result.Success {
		t.Fatalf("prepare_branch failed: %s", result.Error)
	}

	runAgentJob := &domain.GitJob{
		ID: "job-agent", RunID: "run1", NodeID: "coder", ProjectID: "proj1", Type: "run_agent",
		Payload: map[string]any{"tool": "claude", "prompt": "do the ticket", "session_id": "", "branch": "octopus/TICKET-1"},
	}
	result, err = d.Dispatch(ctx, runAgentJob)
	if err != nil {
		t.Fatalf("Dispatch run_agent: %v", err)
	}
	if !result.Success {
		t.Fatalf("run_agent failed: %s", result.Error)
	}
	if result.SessionID != "sess-1" {
		t.Fatalf("expected session id sess-1, got %q", result.SessionID)
	}

	// The fake CLI's file edit must have been committed and pushed.
	verify := t.TempDir()
	runGit(t, "", "clone", "-b", "octopus/TICKET-1", remote, verify)
	content, err := os.ReadFile(filepath.Join(verify, "agent_output.txt"))
	if err != nil {
		t.Fatalf("expected agent_output.txt to be pushed to the remote branch: %v", err)
	}
	if strings.TrimSpace(string(content)) != "agent wrote this" {
		t.Fatalf("unexpected file content: %q", content)
	}
}

func TestLocalDispatcher_UnknownJobTypeFailsGracefully(t *testing.T) {
	d := tools.NewLocalDispatcher(t.TempDir())
	result, err := d.Dispatch(context.Background(), &domain.GitJob{ID: "j1", Type: "not_a_real_type"})
	if err != nil {
		t.Fatalf("Dispatch itself should not error for a bad job type, only the result: %v", err)
	}
	if result.Success {
		t.Fatal("expected an unknown job type to fail")
	}
}

func TestLocalDispatcher_RunAgentFailureDoesNotCommit(t *testing.T) {
	ctx := context.Background()
	remote := newBareRemote(t, "main")
	d := tools.NewLocalDispatcher(t.TempDir())
	d.Invocation["claude"] = tools.CLIInvocation{
		Binary:         writeFakeCLI(t, `echo "boom" >&2; exit 1`),
		BuildArgs:      func(prompt, sessionID string) []string { return []string{} },
		ParseSessionID: tools.ClaudeCodeInvocation.ParseSessionID,
	}

	if _, err := d.Dispatch(ctx, &domain.GitJob{
		ID: "j-prep", RunID: "run2", ProjectID: "proj2", Type: "prepare_branch",
		Payload: map[string]any{"remote_url": remote, "base_branch": "main", "branch_name": "octopus/T2"},
	}); err != nil {
		t.Fatalf("prepare_branch dispatch: %v", err)
	}

	result, err := d.Dispatch(ctx, &domain.GitJob{
		ID: "j-agent", RunID: "run2", NodeID: "coder", ProjectID: "proj2", Type: "run_agent",
		Payload: map[string]any{"tool": "claude", "prompt": "p"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result.Success {
		t.Fatal("expected run_agent to fail when the CLI exits non-zero")
	}
	if !strings.Contains(result.Error, "boom") {
		t.Fatalf("expected failure output preserved, got: %s", result.Error)
	}
}
