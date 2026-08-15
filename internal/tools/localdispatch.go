package tools

import (
	"context"
	"fmt"

	"octopus/internal/domain"
)

// LocalDispatcher is Phase 6's JobDispatcher: it runs a job directly in
// this process rather than routing it to a remote octopus-runner, so
// agent-logic bugs can be isolated from networking bugs before Phase 7
// adds the runner protocol. Phase 7's runnerhub.Hub implements the same
// agents.JobDispatcher interface; swapping one for the other in main.go's
// wiring doesn't require any change here or in cliagent.
type LocalDispatcher struct {
	Git        *GitRunner
	CLI        *CLIRunner
	Invocation map[string]CLIInvocation // tool name -> how to run it, e.g. "claude" -> ClaudeCodeInvocation
}

func NewLocalDispatcher(cloneCacheDir string) *LocalDispatcher {
	return &LocalDispatcher{
		Git: &GitRunner{CloneCacheDir: cloneCacheDir},
		CLI: &CLIRunner{},
		Invocation: map[string]CLIInvocation{
			"claude": ClaudeCodeInvocation,
			"codex":  CodexInvocation,
			"gemini": GeminiInvocation,
		},
	}
}

// Dispatch executes job and always returns a result — even a failed job is
// a successful *dispatch*; the error return is reserved for transport-
// level failure, which a same-process call can't have. A run_agent job
// additionally commits and pushes whatever the CLI changed, reusing the
// same fetch-before/push-after discipline as every other job type.
func (d *LocalDispatcher) Dispatch(ctx context.Context, job *domain.GitJob) (*domain.GitJobResult, error) {
	var output, sessionID string
	var err error

	switch job.Type {
	case "prepare_branch":
		output, err = d.Git.PrepareBranch(ctx,
			strField(job.Payload, "remote_url"), strField(job.Payload, "base_branch"),
			strField(job.Payload, "branch_name"), job.ProjectID, job.RunID)
	case "diff":
		output, err = d.Git.Diff(ctx, job.ProjectID, job.RunID, strField(job.Payload, "base_branch"))
	case "commit":
		output, err = d.Git.Commit(ctx, job.ProjectID, job.RunID, strField(job.Payload, "message"))
	case "push":
		output, err = d.Git.Push(ctx, job.ProjectID, job.RunID, strField(job.Payload, "branch"))
	case "merge":
		output, err = d.merge(ctx, job)
	case "apply_patch":
		output, err = d.Git.ApplyPatch(ctx, job.ProjectID, job.RunID, strField(job.Payload, "patch"))
	case "run_agent":
		output, sessionID, err = d.runAgent(ctx, job)
	default:
		err = fmt.Errorf("unknown job type %q", job.Type)
	}

	result := &domain.GitJobResult{JobID: job.ID, Success: err == nil, Output: output, SessionID: sessionID}
	if err != nil {
		result.Error = err.Error()
	}
	return result, nil
}

func (d *LocalDispatcher) runAgent(ctx context.Context, job *domain.GitJob) (output, sessionID string, err error) {
	tool := strField(job.Payload, "tool")
	inv, ok := d.Invocation[tool]
	if !ok {
		return "", "", fmt.Errorf("no CLI invocation configured for tool %q", tool)
	}

	prompt := strField(job.Payload, "prompt")
	priorSessionID := strField(job.Payload, "session_id")
	apiKey := strField(job.Payload, "api_key")
	branch := strField(job.Payload, "branch")
	remoteURL := strField(job.Payload, "remote_url")

	// Self-heal the checkout before doing anything else — this job may
	// well have landed on a runner that's never seen this run before
	// (Key Design Decision 20/21: any runner registered for a project can
	// pick up any of its jobs, not just the one that ran prepare_branch).
	if remoteURL != "" && branch != "" {
		if checkoutOutput, err := d.Git.EnsureCheckout(ctx, remoteURL, branch, job.ProjectID, job.RunID); err != nil {
			return checkoutOutput, "", fmt.Errorf("ensuring checkout: %w", err)
		}
	}

	dir := d.Git.WorkDir(job.ProjectID, job.RunID)
	cliOutput, newSessionID, err := d.CLI.Invoke(ctx, dir, inv, prompt, priorSessionID, EnvVarForTool[tool], apiKey)
	if err != nil {
		return cliOutput, newSessionID, err
	}

	commitOutput, err := d.Git.Commit(ctx, job.ProjectID, job.RunID, fmt.Sprintf("octopus: %s node", job.NodeID))
	if err != nil {
		return cliOutput + "\n" + commitOutput, newSessionID, err
	}

	if branch != "" {
		pushOutput, err := d.Git.Push(ctx, job.ProjectID, job.RunID, branch)
		if err != nil {
			return cliOutput + "\n" + commitOutput + "\n" + pushOutput, newSessionID, err
		}
	}

	return cliOutput, newSessionID, nil
}

// merge self-heals the checkout first, exactly like runAgent — a merge
// (dispatched after a review approval) can just as easily land on a
// runner that's never seen this run before.
func (d *LocalDispatcher) merge(ctx context.Context, job *domain.GitJob) (string, error) {
	branch := strField(job.Payload, "branch")
	remoteURL := strField(job.Payload, "remote_url")

	if remoteURL != "" && branch != "" {
		if checkoutOutput, err := d.Git.EnsureCheckout(ctx, remoteURL, branch, job.ProjectID, job.RunID); err != nil {
			return checkoutOutput, fmt.Errorf("ensuring checkout: %w", err)
		}
	}

	return d.Git.Merge(ctx, job.ProjectID, job.RunID, strField(job.Payload, "base_branch"), branch)
}

func strField(payload map[string]any, key string) string {
	s, _ := payload[key].(string)
	return s
}
