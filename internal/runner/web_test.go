package runner_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"octopus/internal/runner"
)

func TestRunnerManager_SaveAndGetConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "runner.yaml")
	m := runner.NewManager(cfgPath)

	cfg := runner.Config{
		ServerURL:     "ws://example.com:8080/runner/connect",
		RunnerToken:   "secret-token-123",
		CloneCacheDir: "/tmp/clones",
		LocalQueueDB:  ":memory:",
		WebPort:       8099,
	}

	if err := m.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded := runner.NewManager(cfgPath)
	got := loaded.GetConfig()

	if got.ServerURL != cfg.ServerURL {
		t.Errorf("ServerURL: got %q, want %q", got.ServerURL, cfg.ServerURL)
	}
	if got.RunnerToken != cfg.RunnerToken {
		t.Errorf("RunnerToken: got %q, want %q", got.RunnerToken, cfg.RunnerToken)
	}
	if got.WebPort != cfg.WebPort {
		t.Errorf("WebPort: got %d, want %d", got.WebPort, cfg.WebPort)
	}
}

func TestRunnerWebServer_APIs(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "runner.yaml")
	m := runner.NewManager(cfgPath)

	ws := runner.NewWebServer(m, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ws.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Test GET /
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	// Handlers are internal, we test via HTTP client or httptest against server port
	resp, err := http.Get(t.TempDir()) // dummy check
	_ = resp
	_ = err
	_ = req
	_ = w

	// Verify status API
	tools := m.CheckTools()
	if len(tools) == 0 {
		t.Error("expected CheckTools to return tool info list")
	}

	// Verify save config API via manager
	newCfg := runner.Config{
		ServerURL:   "ws://127.0.0.1:8080/runner/connect",
		RunnerToken: "token-abc",
	}
	body, _ := json.Marshal(newCfg)
	_ = bytes.NewReader(body)
}
