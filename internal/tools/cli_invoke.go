package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CLIInvocation describes how to drive one coding CLI non-interactively:
// which flags start/resume a session, and how to pull the session id back
// out of its output so a later run can pass it to BuildArgs again.
type CLIInvocation struct {
	Binary         string
	BuildArgs      func(prompt, sessionID string) []string
	ParseSessionID func(output string) string
}

// ClaudeCodeInvocation matches the real Claude Code CLI's actual
// non-interactive contract: -p/--print for a single-shot run, --resume
// <id> to continue a prior session, and --output-format json to get a
// parseable result that includes session_id — this one is verified against
// real Claude Code CLI behavior, not guessed.
var ClaudeCodeInvocation = CLIInvocation{
	Binary: "claude",
	BuildArgs: func(prompt, sessionID string) []string {
		args := []string{"-p", prompt, "--output-format", "json"}
		if sessionID != "" {
			args = append(args, "--resume", sessionID)
		}
		return args
	},
	ParseSessionID: parseJSONField(sessionIDKeys...),
}

// CodexInvocation follows the same shape (a print/non-interactive flag, a
// resume flag, JSON output) since that's the common convention across these
// tools, but — unlike ClaudeCodeInvocation — the exact flag names here are
// a best-effort default, not verified against the CLI's current --help
// output. Confirm against the installed version before relying on this in
// a real deployment; this environment has no codex binary installed to
// check against.
var CodexInvocation = CLIInvocation{
	Binary: "codex",
	BuildArgs: func(prompt, sessionID string) []string {
		args := []string{"exec", prompt, "--json"}
		if sessionID != "" {
			args = append(args, "--resume", sessionID)
		}
		return args
	},
	ParseSessionID: parseJSONField(sessionIDKeys...),
}

// AntigravityInvocation drives Google's Antigravity CLI (binary `agy`, the
// successor Google pointed Gemini CLI users to — see
// https://antigravity.google/docs/cli/headless). Unlike CodexInvocation,
// these flags ARE verified against agy's published docs: `-p`/`--print` for
// a single non-interactive prompt, `--output-format json`, and
// `--conversation <id>` (not `--resume`) to continue a prior run using the
// `conversation_id` the JSON output reports.
//
// What's NOT verified — and, per a maintainer on
// google-antigravity/antigravity-cli#78, not currently supported at all —
// is API-key authentication for headless use: agy's headless mode expects
// cached credentials from a prior interactive `agy` login on the runner
// machine, not an injected env var. EnvVarForTool still maps an
// ANTIGRAVITY_API_KEY entry below for parity with the other tools and in
// case upstream adds support, but as of this writing it has no effect —
// authenticate the runner host interactively instead.
var AntigravityInvocation = CLIInvocation{
	Binary: "agy",
	BuildArgs: func(prompt, sessionID string) []string {
		args := []string{"-p", prompt, "--output-format", "json"}
		if sessionID != "" {
			args = append(args, "--conversation", sessionID)
		}
		return args
	},
	ParseSessionID: parseJSONField(append([]string{"conversation_id"}, sessionIDKeys...)...),
}

var sessionIDKeys = []string{"session_id", "sessionId", "SessionID"}

// parseJSONField tries each key in turn against the last JSON object it
// can find in output (CLIs sometimes emit non-JSON progress lines before
// their final JSON result), returning the first non-empty match.
func parseJSONField(keys ...string) func(string) string {
	return func(output string) string {
		obj := lastJSONObject(output)
		if obj == "" {
			return ""
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(obj), &v); err != nil {
			return ""
		}
		for _, k := range keys {
			if s, ok := v[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
}

func lastJSONObject(output string) string {
	end := strings.LastIndex(output, "}")
	if end == -1 {
		return ""
	}
	depth := 0
	for i := end; i >= 0; i-- {
		switch output[i] {
		case '}':
			depth++
		case '{':
			depth--
			if depth == 0 {
				return output[i : end+1]
			}
		}
	}
	return ""
}

// EnvVarForTool maps a preset's tool name to the environment variable its
// CLI expects an API key in. Used to inject a run_agent job's api_key
// (Key Design Decision 28) as an ephemeral env var for that one subprocess
// only — never written to disk.
var EnvVarForTool = map[string]string{
	"claude":      "ANTHROPIC_API_KEY",
	"codex":       "OPENAI_API_KEY",
	"antigravity": "ANTIGRAVITY_API_KEY",
}

// CLIRunner invokes a coding CLI non-interactively in dir.
type CLIRunner struct{}

// Invoke runs inv's Binary with the args for prompt (resuming sessionID if
// set), with apiKey exposed to the subprocess only, under the given env
// var name. It returns combined stdout+stderr and whatever session id the
// tool reported — falling back to the sessionID it was called with if the
// tool didn't report a new one, so a caller can always feed the return
// value straight back into the next invocation.
func (c *CLIRunner) Invoke(ctx context.Context, dir string, inv CLIInvocation, prompt, sessionID, apiKeyEnvVar, apiKey string) (output, newSessionID string, err error) {
	args := inv.BuildArgs(prompt, sessionID)
	cmd := exec.CommandContext(ctx, inv.Binary, args...)
	cmd.Dir = dir

	env := os.Environ()
	if apiKeyEnvVar != "" && apiKey != "" {
		env = append(env, apiKeyEnvVar+"="+apiKey)
	}
	cmd.Env = env

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	runErr := cmd.Run()

	output = buf.String()
	newSessionID = inv.ParseSessionID(output)
	if newSessionID == "" {
		newSessionID = sessionID
	}

	if runErr != nil {
		return output, newSessionID, fmt.Errorf("running %s: %w: %s", inv.Binary, runErr, strings.TrimSpace(output))
	}
	return output, newSessionID, nil
}
