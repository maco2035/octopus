package tools_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"octopus/internal/tools"
)

// writeFakeCLI writes a small script standing in for claude/codex/antigravity —
// this sandbox has none of those installed (no API keys either), so this
// is what proves the invocation plumbing (args, env, session round-trip)
// works, independent of any real provider. It echoes its args (so a test
// can assert --resume was or wasn't passed), can optionally write a marker
// file into its working directory (standing in for "the coding agent
// edited a file"), and prints a JSON result with a session_id.
func writeFakeCLI(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake CLI fixture is a shell script; skip on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("writing fake cli script: %v", err)
	}
	return path
}

func genericInvocation(binary string) tools.CLIInvocation {
	return tools.CLIInvocation{
		Binary: binary,
		BuildArgs: func(prompt, sessionID string) []string {
			args := []string{"-p", prompt, "--output-format", "json"}
			if sessionID != "" {
				args = append(args, "--resume", sessionID)
			}
			return args
		},
		ParseSessionID: func(output string) string {
			// Reuses the same "last JSON object in output" contract as
			// ClaudeCodeInvocation by shelling back out to a real one for
			// the parse step — simplest way to exercise the real parser.
			return tools.ClaudeCodeInvocation.ParseSessionID(output)
		},
	}
}

func TestCLIRunner_InvokeStartsFreshSession(t *testing.T) {
	script := `echo '{"result":"did the thing","session_id":"sess-new"}'`
	bin := writeFakeCLI(t, script)

	c := &tools.CLIRunner{}
	output, sessionID, err := c.Invoke(context.Background(), t.TempDir(), genericInvocation(bin), "do the thing", "", "MY_API_KEY", "secret")
	if err != nil {
		t.Fatalf("Invoke: %v: %s", err, output)
	}
	if sessionID != "sess-new" {
		t.Fatalf("expected session id sess-new, got %q", sessionID)
	}
}

func TestCLIRunner_InvokeResumesSession(t *testing.T) {
	// Fails unless --resume sess-old is actually in argv — proves BuildArgs'
	// session-resume branch really gets exercised, not just constructed.
	script := `
case "$*" in
  *--resume\ sess-old*) echo '{"result":"continued","session_id":"sess-old"}' ;;
  *) echo "FAIL: no --resume in args: $*" >&2; exit 1 ;;
esac`
	bin := writeFakeCLI(t, script)

	c := &tools.CLIRunner{}
	output, sessionID, err := c.Invoke(context.Background(), t.TempDir(), genericInvocation(bin), "continue", "sess-old", "", "")
	if err != nil {
		t.Fatalf("Invoke: %v: %s", err, output)
	}
	if sessionID != "sess-old" {
		t.Fatalf("expected session id sess-old, got %q", sessionID)
	}
}

func TestCLIRunner_InvokeExposesAPIKeyOnlyAsEnvVar(t *testing.T) {
	script := `
if [ "$MY_API_KEY" != "topsecret" ]; then
  echo "FAIL: expected MY_API_KEY=topsecret, got $MY_API_KEY" >&2
  exit 1
fi
echo '{"result":"ok","session_id":"s1"}'`
	bin := writeFakeCLI(t, script)

	c := &tools.CLIRunner{}
	if _, _, err := c.Invoke(context.Background(), t.TempDir(), genericInvocation(bin), "p", "", "MY_API_KEY", "topsecret"); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}

func TestCLIRunner_InvokeFallsBackToPriorSessionIfNoneReported(t *testing.T) {
	script := `echo 'plain text output with no JSON at all'`
	bin := writeFakeCLI(t, script)

	c := &tools.CLIRunner{}
	_, sessionID, err := c.Invoke(context.Background(), t.TempDir(), genericInvocation(bin), "p", "sess-keep", "", "")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if sessionID != "sess-keep" {
		t.Fatalf("expected fallback to prior session sess-keep, got %q", sessionID)
	}
}

func TestCLIRunner_InvokePropagatesFailure(t *testing.T) {
	bin := writeFakeCLI(t, `echo "boom" >&2; exit 1`)
	c := &tools.CLIRunner{}
	_, _, err := c.Invoke(context.Background(), t.TempDir(), genericInvocation(bin), "p", "", "", "")
	if err == nil {
		t.Fatal("expected an error when the CLI exits non-zero")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error to include the CLI's stderr, got: %v", err)
	}
}

func TestCLIRunner_InvokeEditsFilesInWorkingDir(t *testing.T) {
	// Proves the CLI actually runs *in* the given directory, the way a
	// real coding CLI needs to in order to edit the checkout.
	script := `echo "edited" > touched.txt; echo '{"result":"ok","session_id":"s1"}'`
	bin := writeFakeCLI(t, script)
	dir := t.TempDir()

	c := &tools.CLIRunner{}
	if _, _, err := c.Invoke(context.Background(), dir, genericInvocation(bin), "p", "", "", ""); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "touched.txt")); err != nil {
		t.Fatalf("expected the CLI's working directory to be %s: %v", dir, err)
	}
}

func TestParseJSONField_FindsLastJSONObjectAmongNoise(t *testing.T) {
	output := fmt.Sprintf("some progress log line\nanother line\n%s\n", `{"session_id":"deep-in-output"}`)
	got := tools.ClaudeCodeInvocation.ParseSessionID(output)
	if got != "deep-in-output" {
		t.Fatalf("expected deep-in-output, got %q", got)
	}
}
