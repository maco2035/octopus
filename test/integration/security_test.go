// TestSecurity_APIKeyNeverPersistedToDisk is part of a security-hardening
// pass: API keys must never be persisted to the server's own SQLite
// database, only used ephemerally per-invocation (PLAN.md Key Design
// Decision 28).
package integration_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"octopus/internal/agents"
	"octopus/internal/domain"
	"octopus/internal/scheduler"
	"octopus/internal/store"
	"octopus/internal/tools"
)

// TestSecurity_APIKeyNeverPersistedToDisk runs a real coder node (real
// LocalDispatcher, real git, a fake CLI standing in for claude) with a
// distinctive, obviously-fake API key, then inspects the raw SQLite file
// directly — not through the Store interface, which could itself hide a
// leak — for that key anywhere in the git_jobs table.
func TestSecurity_APIKeyNeverPersistedToDisk(t *testing.T) {
	ctx := context.Background()
	remote := newBareRemoteWithMain(t)
	const secretAPIKey = "sk-DO-NOT-PERSIST-ME-1234567890"

	dbPath := filepath.Join(t.TempDir(), "octopus.db")
	st, err := store.New(dbPath, "", "")
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	if err := st.SaveProject(ctx, &domain.Project{ID: "proj1", Name: "Proj1", GitRemoteURL: remote, BaseBranch: "main"}); err != nil {
		t.Fatalf("SaveProject: %v", err)
	}
	def := &domain.PipelineDef{
		ID: "def1", ProjectID: "proj1", Name: "def1",
		Nodes: []domain.NodeDef{{ID: "coder", AgentType: "claude-coder"}},
	}
	if err := st.SavePipelineDef(ctx, def); err != nil {
		t.Fatalf("SavePipelineDef: %v", err)
	}

	dispatcher := tools.NewLocalDispatcher(t.TempDir())
	dispatcher.Invocation["claude"] = fakeInvocation(writeFakeClaude(t, `
if [ "$ANTHROPIC_API_KEY" != "`+secretAPIKey+`" ]; then
  echo "FAIL: wrong or missing api key" >&2
  exit 1
fi
echo "did the work" > work.txt
echo '{"result":"done","session_id":"sess-1"}'`))

	reg := agents.NewRegistry()
	agents.RegisterCLIPresets(reg, agents.PresetConfig{Dispatcher: dispatcher, Store: st, AnthropicAPIKey: secretAPIKey})

	sched := scheduler.New(st, reg.Create)
	sched.Dispatcher = dispatcher

	state, err := sched.StartRun(ctx, "proj1", "def1", "TICKET-1")
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
		if reloaded.Status == domain.StatusCompleted || reloaded.Status == domain.StatusFailed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if reloaded.Status != domain.StatusCompleted {
		t.Fatalf("expected COMPLETED (proves the real key really was used, via the fake CLI's own check), got %s: %s", reloaded.Status, reloaded.Summary)
	}
	st.Close()

	// Now the actual assertion: open the SQLite file directly, bypassing
	// the Store interface entirely, and scan every text column of every
	// row in git_jobs for the secret. This is deliberately not "does
	// LoadPendingGitJobFor return it" — a bug in a different Store method
	// could hide a leak that's still sitting in the file on disk.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("opening db directly: %v", err)
	}
	defer raw.Close()

	rows, err := raw.QueryContext(ctx, `SELECT payload_json, output, error FROM git_jobs`)
	if err != nil {
		t.Fatalf("querying git_jobs: %v", err)
	}
	defer rows.Close()

	rowCount := 0
	found := false
	for rows.Next() {
		rowCount++
		var payloadJSON, output, jobErr string
		if err := rows.Scan(&payloadJSON, &output, &jobErr); err != nil {
			t.Fatalf("scanning row: %v", err)
		}
		if strings.Contains(payloadJSON, secretAPIKey) || strings.Contains(output, secretAPIKey) || strings.Contains(jobErr, secretAPIKey) {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating rows: %v", err)
	}
	if rowCount == 0 {
		t.Fatal("expected at least one git_jobs row (sanity check that this test is actually exercising anything)")
	}
	if found {
		t.Fatal("SECURITY: the API key was found persisted in the server's own SQLite database")
	}
}
