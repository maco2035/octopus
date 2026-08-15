package domain_test

import (
	"testing"

	"octopus/internal/domain"
)

func TestGitJob_Redacted_StripsAPIKey(t *testing.T) {
	job := &domain.GitJob{
		ID: "j1", RunID: "r1", ProjectID: "p1", Type: "run_agent",
		Payload: map[string]any{"tool": "claude", "api_key": "sk-supersecret", "prompt": "do stuff"},
	}

	redacted := job.Redacted()

	if redacted.Payload["api_key"] == "sk-supersecret" {
		t.Fatal("expected Redacted to strip the real api_key")
	}
	if redacted.Payload["tool"] != "claude" || redacted.Payload["prompt"] != "do stuff" {
		t.Fatalf("expected non-secret fields preserved, got %+v", redacted.Payload)
	}

	// The original job must be untouched — it still needs to carry the
	// real key to whatever actually dispatches it.
	if job.Payload["api_key"] != "sk-supersecret" {
		t.Fatal("Redacted must not mutate the original job")
	}
}

func TestGitJob_Redacted_NoAPIKeyIsNoop(t *testing.T) {
	job := &domain.GitJob{
		ID: "j2", Type: "prepare_branch",
		Payload: map[string]any{"remote_url": "git@x:y.git", "base_branch": "main"},
	}

	redacted := job.Redacted()

	if redacted.Payload["remote_url"] != "git@x:y.git" {
		t.Fatalf("expected payload preserved when there's no api_key, got %+v", redacted.Payload)
	}
}
