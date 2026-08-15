package tools_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"octopus/internal/tools"
)

// newBareRemote creates a real local git repository (not a mock) with one
// commit on baseBranch, and returns its path as a file:// clone target —
// this environment has no network access to a real GitHub, but plain local
// git is fully real and exercises the actual git plumbing tools.GitRunner
// shells out to.
func newBareRemote(t *testing.T, baseBranch string) string {
	t.Helper()
	seed := t.TempDir()
	runGit(t, seed, "init", "-b", baseBranch)
	runGit(t, seed, "config", "user.email", "test@example.com")
	runGit(t, seed, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing seed file: %v", err)
	}
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "initial")

	bare := t.TempDir()
	runGit(t, "", "clone", "--bare", seed, bare)
	return bare
}

func runGit(t *testing.T, dir string, args ...string) string {
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

func TestGitRunner_PrepareBranchDiffCommitPushMerge(t *testing.T) {
	ctx := context.Background()
	remote := newBareRemote(t, "main")

	g := &tools.GitRunner{CloneCacheDir: t.TempDir()}

	// prepare_branch: clone, cut a branch, push it immediately (even empty).
	if _, err := g.PrepareBranch(ctx, remote, "main", "octopus/TICKET-1", "proj1", "run1"); err != nil {
		t.Fatalf("PrepareBranch: %v", err)
	}
	branches := runGit(t, remote, "branch", "--list", "octopus/TICKET-1")
	if !strings.Contains(branches, "octopus/TICKET-1") {
		t.Fatalf("expected branch pushed to remote immediately, branches: %s", branches)
	}

	workDir := g.WorkDir("proj1", "run1")
	if err := os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("new feature\n"), 0o644); err != nil {
		t.Fatalf("writing feature file: %v", err)
	}

	// commit: stages and commits the change.
	commitOut, err := g.Commit(ctx, "proj1", "run1", "add feature")
	if err != nil {
		t.Fatalf("Commit: %v: %s", err, commitOut)
	}

	// commit again with nothing changed: must not error.
	if out, err := g.Commit(ctx, "proj1", "run1", "no-op"); err != nil {
		t.Fatalf("Commit with clean tree should not error: %v: %s", err, out)
	}

	// diff: shows the committed change relative to base.
	diff, err := g.Diff(ctx, "proj1", "run1", "main")
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "feature.txt") {
		t.Fatalf("expected diff to mention feature.txt, got: %s", diff)
	}

	// push: sends the commit to the remote.
	if _, err := g.Push(ctx, "proj1", "run1", "octopus/TICKET-1"); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// merge: fast-forwards main with the branch and pushes it.
	if _, err := g.Merge(ctx, "proj1", "run1", "main", "octopus/TICKET-1"); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	// Verify against a fresh clone of the remote that main now has the file.
	verify := t.TempDir()
	runGit(t, "", "clone", remote, verify)
	if _, err := os.Stat(filepath.Join(verify, "feature.txt")); err != nil {
		t.Fatalf("expected feature.txt to be merged into main on the remote: %v", err)
	}
}

func TestGitRunner_ApplyPatch(t *testing.T) {
	ctx := context.Background()
	remote := newBareRemote(t, "main")
	g := &tools.GitRunner{CloneCacheDir: t.TempDir()}

	if _, err := g.PrepareBranch(ctx, remote, "main", "octopus/TICKET-2", "proj1", "run2"); err != nil {
		t.Fatalf("PrepareBranch: %v", err)
	}

	patch := "diff --git a/patched.txt b/patched.txt\n" +
		"new file mode 100644\n" +
		"index 0000000..e69de29\n" +
		"--- /dev/null\n" +
		"+++ b/patched.txt\n" +
		"@@ -0,0 +1 @@\n" +
		"+patched content\n"

	if out, err := g.ApplyPatch(ctx, "proj1", "run2", patch); err != nil {
		t.Fatalf("ApplyPatch: %v: %s", err, out)
	}

	workDir := g.WorkDir("proj1", "run2")
	content, err := os.ReadFile(filepath.Join(workDir, "patched.txt"))
	if err != nil {
		t.Fatalf("reading patched file: %v", err)
	}
	if strings.TrimSpace(string(content)) != "patched content" {
		t.Fatalf("unexpected patched content: %q", content)
	}
}
