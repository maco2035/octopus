package agents

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"octopus/internal/agents/cliagent"
	"octopus/internal/domain"
	"octopus/internal/engine"
	"octopus/internal/store"
)

// PresetConfig is what a deployment provides once to wire the CLI-backed
// presets into a Registry: which JobDispatcher actually runs a run_agent
// job (tools.LocalDispatcher in Phase 6, runnerhub.Hub in Phase 7 — same
// interface, different wiring in main.go), the Store cliagent needs for
// its own crash-recovery bookkeeping, and each provider's API key.
//
// There's no Grok/xAI preset here even though Key Design Decision 24 lists
// Grok alongside Gemini/Claude/OpenAI as a supported provider: that
// decision predates PLAN.md's pivot to CLI delegation, and unlike Claude
// Code, Codex CLI, and Antigravity CLI, there's no xAI-published agentic
// coding CLI to delegate to here — adding one would mean guessing at a tool
// and invocation contract that don't exist, rather than adapting a real one.
type PresetConfig struct {
	Dispatcher        domain.JobDispatcher
	Store             store.Store
	AnthropicAPIKey   string
	OpenAIAPIKey      string
	AntigravityAPIKey string
}

// RegisterCLIPresets registers the named agent types PLAN.md Phase 6
// describes — "a Claude-backed coder, a Codex-backed reviewer, a
// reporter" plus a security gate — each just fixing cliagent.Agent's tool
// and role prompt. This is what the web UI's drag-and-drop palette lists;
// there's no separate Go package per provider.
func RegisterCLIPresets(reg *Registry, cfg PresetConfig) {
	register := func(agentType, tool, rolePrompt, apiKey string, detectBlocked func(string) (bool, string)) {
		reg.Register(agentType, func(nodeCfg map[string]any) (domain.Agent, error) {
			nodeID, _ := nodeCfg["node_id"].(string)
			return &cliagent.Agent{
				NodeID: nodeID, Tool: tool, RolePrompt: rolePrompt, APIKey: apiKey,
				Dispatcher: cfg.Dispatcher, Store: cfg.Store, DetectBlocked: detectBlocked,
			}, nil
		})
	}

	register("claude-coder", "claude", coderPrompt, cfg.AnthropicAPIKey, nil)
	register("codex-reviewer", "codex", reviewerPrompt, cfg.OpenAIAPIKey, nil)
	register("antigravity-reporter", "antigravity", reporterPrompt, cfg.AntigravityAPIKey, nil)
	register("claude-security", "claude", securityPrompt, cfg.AnthropicAPIKey, detectBlockedPrefix)

	// "merge" isn't CLI-backed — it's a direct git operation (Key Design
	// Decision 20's "every git job fetches before acting and pushes after
	// mutating"), typically the last node after a review gate approves.
	// Its base branch comes from the node's own Config (set when the
	// pipeline is built) rather than a fresh Project lookup, since the
	// engine only hands agents cfg + state, not a Store.
	reg.Register("merge", func(nodeCfg map[string]any) (domain.Agent, error) {
		nodeID, _ := nodeCfg["node_id"].(string)
		baseBranch, _ := nodeCfg["base_branch"].(string)
		if baseBranch == "" {
			baseBranch = "main"
		}
		return &mergeAgent{nodeID: nodeID, baseBranch: baseBranch, dispatcher: cfg.Dispatcher, store: cfg.Store}, nil
	})
}

// mergeAgent fast-forwards the project's base branch with the run's
// branch and pushes the result — what approving a review ultimately does.
type mergeAgent struct {
	nodeID     string
	baseBranch string
	dispatcher domain.JobDispatcher
	store      store.Store
}

func (a *mergeAgent) Name() string { return "merge:" + a.nodeID }

func (a *mergeAgent) Execute(ctx context.Context, state *domain.PipelineState) error {
	project, err := a.store.LoadProject(ctx, state.ProjectID)
	if err != nil {
		return fmt.Errorf("merge %s: loading project: %w", a.nodeID, err)
	}

	job := &domain.GitJob{
		ID: uuid.NewString(), RunID: state.RunID, NodeID: a.nodeID, ProjectID: state.ProjectID,
		Type: "merge", Payload: map[string]any{"base_branch": a.baseBranch, "branch": state.GitBranch, "remote_url": project.GitRemoteURL},
	}
	if err := a.store.SaveGitJob(ctx, job.Redacted()); err != nil {
		return fmt.Errorf("merge %s: saving job: %w", a.nodeID, err)
	}
	result, err := a.dispatcher.Dispatch(ctx, job)
	if err != nil {
		if errors.Is(err, domain.ErrNoRunnerAvailable) {
			return fmt.Errorf("%w: %s", engine.ErrAwaitingRunner, a.nodeID)
		}
		return fmt.Errorf("merge %s: dispatch: %w", a.nodeID, err)
	}
	if err := a.store.ResolveGitJob(ctx, job.ID, result); err != nil {
		return fmt.Errorf("merge %s: resolving job: %w", a.nodeID, err)
	}
	if !result.Success {
		return fmt.Errorf("merge %s: %s", a.nodeID, result.Error)
	}
	state.SetOutput(a.nodeID, result.Output)
	return nil
}

const coderPrompt = `You are the coding agent for this Octopus pipeline node. Read the ticket, make the necessary code changes in this checkout, and run the project's tests before finishing.`

const reviewerPrompt = `You are the review agent for this Octopus pipeline node. Review the diff on this branch against its base for correctness, style, and risk. Leave your findings as your final output.`

const reporterPrompt = `You are the reporting agent for this Octopus pipeline node. Summarize what changed on this branch and why, in a form suitable for a ticket comment.`

const securityPrompt = `You are the security agent for this Octopus pipeline node. Review the diff on this branch for security issues: secrets, injection, unsafe deserialization, auth bypasses, and similar. Start your response with the single word BLOCKED followed by the reason if you find a real blocking issue; otherwise start with the single word CLEAR.`

// detectBlockedPrefix implements securityPrompt's contract: the security
// preset's own instructions are what BLOCKED/CLEAR means, so parsing it
// here only works because those two are kept in sync — a prompt change
// that stops asking for a leading BLOCKED/CLEAR silently breaks this.
//
// It's a substring search rather than a strict prefix check because
// result.Output is the CLI's full raw stdout — with --output-format json
// (ClaudeCodeInvocation), that's a JSON envelope, and this code doesn't
// assume a specific field name for the model's own answer inside it (that
// schema isn't verified here — see cli_invoke.go). A substring search
// finds BLOCKED wherever the model's text ends up, at the cost of being
// less precise than a strict prefix check would be against plain text.
func detectBlockedPrefix(output string) (bool, string) {
	idx := strings.Index(output, "BLOCKED")
	if idx == -1 {
		return false, ""
	}
	return true, strings.TrimSpace(output[idx+len("BLOCKED"):])
}
