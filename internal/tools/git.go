// Package tools implements the actual git/shell/CLI subprocess execution
// used server-side in Phase 6 (single machine) and, unchanged, by
// octopus-runner in Phase 7 (PLAN.md directory structure).
package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitRunner executes real git commands against a local clone scoped to
// clone_cache_dir/<project_id>/<run_id> — per run, not per project (Key
// Design Decision 21), so concurrent runs against the same project never
// share a checkout.
type GitRunner struct {
	CloneCacheDir string
}

// WorkDir returns the local clone path for a given run.
func (g *GitRunner) WorkDir(projectID, runID string) string {
	return filepath.Join(g.CloneCacheDir, projectID, runID)
}

func (g *GitRunner) run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	if err != nil {
		return buf.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(buf.String()))
	}
	return buf.String(), nil
}

// PrepareBranch is every run's implicit first git job (Key Design Decision
// 13): clone (or fetch, if the clone already exists) remoteURL, cut
// branchName off baseBranch, and push it immediately — even empty — so
// it's visible on GitHub right away (Key Design Decision 20) and any other
// runner can fetch it later.
func (g *GitRunner) PrepareBranch(ctx context.Context, remoteURL, baseBranch, branchName, projectID, runID string) (string, error) {
	dir := g.WorkDir(projectID, runID)
	var out strings.Builder

	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return out.String(), fmt.Errorf("creating clone parent dir: %w", err)
		}
		o, err := g.run(ctx, "", "clone", remoteURL, dir)
		out.WriteString(o)
		if err != nil {
			return out.String(), err
		}
	} else {
		o, err := g.run(ctx, dir, "fetch", "origin", baseBranch)
		out.WriteString(o)
		if err != nil {
			return out.String(), err
		}
	}

	o, err := g.run(ctx, dir, "checkout", "-B", branchName, "origin/"+baseBranch)
	out.WriteString(o)
	if err != nil {
		return out.String(), err
	}

	// "--" marks the end of options: branchName is a ref name from here on,
	// never reinterpreted as a flag even if it somehow started with "-".
	// Defense in depth — domain.ValidTicketID already rejects that shape
	// at the boundary — for exactly the reason a second layer earns its
	// keep: it still holds if that validation ever regresses or a new
	// caller forgets it.
	o, err = g.run(ctx, dir, "push", "-u", "origin", "--", branchName)
	out.WriteString(o)
	return out.String(), err
}

// EnsureCheckout makes sure this runner has branch checked out locally,
// cloning fresh if it's never seen this run before (a different runner's
// disk, or this one after prepare_branch ran elsewhere) and fetching +
// resetting to origin's copy otherwise — self-healing rather than assuming
// whatever's on disk is current. This is what makes cross-machine handoff
// actually work for jobs *after* the first (Key Design Decision 20 and
// 21): any runner picking up a run_agent or merge job for an existing run
// gets a correct, up-to-date checkout regardless of which machine ran
// prepare_branch.
func (g *GitRunner) EnsureCheckout(ctx context.Context, remoteURL, branch, projectID, runID string) (string, error) {
	dir := g.WorkDir(projectID, runID)
	var out strings.Builder

	if _, err := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return out.String(), fmt.Errorf("creating clone parent dir: %w", err)
		}
		o, err := g.run(ctx, "", "clone", "--branch", branch, remoteURL, dir)
		out.WriteString(o)
		return out.String(), err
	}

	o, err := g.run(ctx, dir, "fetch", "origin", branch)
	out.WriteString(o)
	if err != nil {
		return out.String(), err
	}
	o, err = g.run(ctx, dir, "checkout", "-B", branch, "origin/"+branch)
	out.WriteString(o)
	if err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// Diff returns the branch's changes relative to baseBranch.
func (g *GitRunner) Diff(ctx context.Context, projectID, runID, baseBranch string) (string, error) {
	dir := g.WorkDir(projectID, runID)
	return g.run(ctx, dir, "diff", fmt.Sprintf("origin/%s...HEAD", baseBranch))
}

// Commit stages everything and commits it. A clean tree (nothing for the
// preceding step to have changed) is not an error — plenty of nodes (a
// review, a report) never touch the working tree.
func (g *GitRunner) Commit(ctx context.Context, projectID, runID, message string) (string, error) {
	dir := g.WorkDir(projectID, runID)
	var out strings.Builder

	status, err := g.run(ctx, dir, "status", "--porcelain")
	out.WriteString(status)
	if err != nil {
		return out.String(), err
	}
	if strings.TrimSpace(status) == "" {
		out.WriteString("nothing to commit, working tree clean\n")
		return out.String(), nil
	}

	o, err := g.run(ctx, dir, "add", "-A")
	out.WriteString(o)
	if err != nil {
		return out.String(), err
	}

	o, err = g.run(ctx, dir, "commit", "-m", message)
	out.WriteString(o)
	return out.String(), err
}

// Push pushes the run's branch, retrying with exponential backoff instead
// of failing on the first blip (Key Design Decision 18) — it's the one git
// step that inherently needs a live connection to GitHub, not to the
// Octopus server, so this applies identically whether it's running as part
// of Phase 6's local dispatch or Phase 7's octopus-runner (same code,
// unlike PLAN.md's original split of "runner retries, server doesn't" —
// there's no reason a single-machine push deserves to fail faster than a
// runner's would).
func (g *GitRunner) Push(ctx context.Context, projectID, runID, branch string) (string, error) {
	dir := g.WorkDir(projectID, runID)
	var out strings.Builder

	backoff := 500 * time.Millisecond
	const maxAttempts = 5
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return out.String(), ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}
		o, err := g.run(ctx, dir, "push", "origin", "--", branch)
		out.WriteString(o)
		if err == nil {
			return out.String(), nil
		}
		lastErr = err
	}
	return out.String(), lastErr
}

// Merge fast-forwards baseBranch with branch and pushes the result —
// what a review's final "Approve" does.
func (g *GitRunner) Merge(ctx context.Context, projectID, runID, baseBranch, branch string) (string, error) {
	dir := g.WorkDir(projectID, runID)
	var out strings.Builder

	// "--" before the ticket-derived ref names, same reasoning as Push and
	// PrepareBranch above — added to fetch/merge/push, which is where it's
	// actually needed (baseBranch/branch land as bare positional args
	// there) and where I verified empirically (against a real local repo,
	// not from memory of git's docs) that it doesn't change behavior.
	// Deliberately NOT added to checkout or reset --hard: I tested those
	// too, and "--" changes their meaning outright — `git checkout --
	// <name>` stops treating <name> as a branch and tries to restore it as
	// a pathspec instead (fails here since no such path exists), and `git
	// reset --hard -- <ref>` fails immediately ("Cannot do hard reset with
	// paths"). Both those args are validated at the input boundary
	// (domain.ValidTicketID) instead, which is the actual defense; -m's
	// value binds directly to -m regardless of what follows it, so it
	// needs no special treatment either way.
	steps := [][]string{
		{"fetch", "origin", "--", baseBranch},
		{"checkout", baseBranch},
		{"reset", "--hard", "origin/" + baseBranch},
		{"merge", "--no-ff", "-m", fmt.Sprintf("Merge %s into %s", branch, baseBranch), "--", branch},
		{"push", "origin", "--", baseBranch},
	}
	for _, args := range steps {
		o, err := g.run(ctx, dir, args...)
		out.WriteString(o)
		if err != nil {
			return out.String(), err
		}
	}
	return out.String(), nil
}

// ApplyPatch applies a unified diff to the working tree.
func (g *GitRunner) ApplyPatch(ctx context.Context, projectID, runID, patch string) (string, error) {
	dir := g.WorkDir(projectID, runID)
	f, err := os.CreateTemp("", "octopus-patch-*.diff")
	if err != nil {
		return "", fmt.Errorf("writing patch to temp file: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(patch); err != nil {
		f.Close()
		return "", fmt.Errorf("writing patch to temp file: %w", err)
	}
	f.Close()

	return g.run(ctx, dir, "apply", f.Name())
}
