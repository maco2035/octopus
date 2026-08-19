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

func TestGitJob_Redacted_RecursivelyStripsCredentialFields(t *testing.T) {
	job := &domain.GitJob{Payload: map[string]any{
		"api_key": "top-level-key",
		"environment": map[string]any{
			"OPENAI_API_KEY": "nested-key",
			"safe":           "kept",
		},
		"credentials": []any{
			map[string]any{"access_token": "nested-token", "region": "us-west-2"},
		},
		"settings": map[string]string{
			"client_secret": "nested-secret",
			"mode":          "fast",
		},
	}}

	redacted := job.Redacted()
	environment := redacted.Payload["environment"].(map[string]any)
	credentials := redacted.Payload["credentials"].([]any)[0].(map[string]any)
	settings := redacted.Payload["settings"].(map[string]string)

	if redacted.Payload["api_key"] != "" || environment["OPENAI_API_KEY"] != "" ||
		credentials["access_token"] != "" || settings["client_secret"] != "" {
		t.Fatalf("expected all credential fields blanked, got %+v", redacted.Payload)
	}
	if environment["safe"] != "kept" || credentials["region"] != "us-west-2" || settings["mode"] != "fast" {
		t.Fatalf("expected non-secret nested fields preserved, got %+v", redacted.Payload)
	}

	// Deep-copying is part of the contract: neither maps nor slices in the
	// original live job may be changed by a durable/UI redaction pass.
	if job.Payload["api_key"] != "top-level-key" ||
		job.Payload["environment"].(map[string]any)["OPENAI_API_KEY"] != "nested-key" ||
		job.Payload["credentials"].([]any)[0].(map[string]any)["access_token"] != "nested-token" {
		t.Fatal("Redacted mutated the original nested payload")
	}
}
