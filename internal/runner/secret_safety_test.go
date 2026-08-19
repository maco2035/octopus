package runner

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"octopus/internal/domain"
)

func secretBearingJob() *domain.GitJob {
	return &domain.GitJob{
		ID: "job-secret", RunID: "run-1", ProjectID: "project-1", Type: "run_agent",
		Payload: map[string]any{
			"tool":    "claude",
			"api_key": "sk-must-not-persist",
			"environment": map[string]any{
				"access_token": "token-must-not-persist",
				"safe":         "visible",
			},
		},
	}
}

func TestLocalQueueSaveReceivedRedactsCredentials(t *testing.T) {
	queue, err := NewLocalQueue(filepath.Join(t.TempDir(), "runner.db"))
	if err != nil {
		t.Fatalf("NewLocalQueue: %v", err)
	}
	defer queue.Close()

	job := secretBearingJob()
	if err := queue.SaveReceived(job); err != nil {
		t.Fatalf("SaveReceived: %v", err)
	}

	var stored string
	if err := queue.db.QueryRow(`SELECT job_json FROM received_jobs WHERE id = ?`, job.ID).Scan(&stored); err != nil {
		t.Fatalf("reading received job: %v", err)
	}
	if strings.Contains(stored, "sk-must-not-persist") || strings.Contains(stored, "token-must-not-persist") {
		t.Fatalf("runner database contains a credential: %s", stored)
	}

	var persisted domain.GitJob
	if err := json.Unmarshal([]byte(stored), &persisted); err != nil {
		t.Fatalf("decoding stored job: %v", err)
	}
	if persisted.Payload["tool"] != "claude" || persisted.Payload["environment"].(map[string]any)["safe"] != "visible" {
		t.Fatalf("non-secret recovery data was not preserved: %+v", persisted.Payload)
	}
	if job.Payload["api_key"] != "sk-must-not-persist" {
		t.Fatal("saving the durable copy mutated the live job")
	}
}

func TestManagerJobRecordRedactsCredentials(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "runner.yaml"))
	job := secretBearingJob()

	manager.onJobStart(job)
	_, _, _, active, _ := manager.GetStatus()
	if active == nil {
		t.Fatal("expected an active job record")
	}
	if strings.Contains(active.Payload, "sk-must-not-persist") || strings.Contains(active.Payload, "token-must-not-persist") {
		t.Fatalf("local dashboard record contains a credential: %s", active.Payload)
	}
	if !strings.Contains(active.Payload, `"safe":"visible"`) {
		t.Fatalf("expected useful non-secret payload data, got %s", active.Payload)
	}
	if job.Payload["api_key"] != "sk-must-not-persist" {
		t.Fatal("recording the dashboard copy mutated the live job")
	}
}
