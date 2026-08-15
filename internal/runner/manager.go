package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"octopus/internal/domain"
	"octopus/internal/tools"
)

type Config struct {
	ServerURL     string   `yaml:"server_url" json:"server_url"`
	RunnerToken   string   `yaml:"runner_token" json:"runner_token"`
	ProjectIDs    []string `yaml:"project_ids,omitempty" json:"project_ids,omitempty"`
	CloneCacheDir string   `yaml:"clone_cache_dir" json:"clone_cache_dir"`
	LocalQueueDB  string   `yaml:"local_queue_db" json:"local_queue_db"`
	WebPort       int      `yaml:"web_port,omitempty" json:"web_port,omitempty"`
}

type JobRecord struct {
	ID        string               `json:"id"`
	ProjectID string               `json:"project_id"`
	RunID     string               `json:"run_id"`
	Type      string               `json:"type"`
	Payload   string               `json:"payload"`
	Status    string               `json:"status"` // "running", "success", "failed"
	StartedAt time.Time            `json:"started_at"`
	EndedAt   *time.Time           `json:"ended_at,omitempty"`
	Duration  string               `json:"duration,omitempty"`
	Result    *domain.GitJobResult `json:"result,omitempty"`
}

type ToolInfo struct {
	Name      string `json:"name"`
	Installed bool   `json:"installed"`
	Path      string `json:"path,omitempty"`
	Version   string `json:"version,omitempty"`
}

type RunnerStatus string

const (
	StatusNotConfigured RunnerStatus = "not_configured"
	StatusConnecting    RunnerStatus = "connecting"
	StatusConnected     RunnerStatus = "connected"
	StatusDisconnected  RunnerStatus = "disconnected"
	StatusError         RunnerStatus = "error"
)

type Manager struct {
	mu            sync.RWMutex
	configPath    string
	config        Config
	status        RunnerStatus
	lastError     string
	lastConnected *time.Time
	activeJob     *JobRecord
	history       []*JobRecord
	cancelRunner  context.CancelFunc
	client        *Client
	queue         *LocalQueue
}

func NewManager(configPath string) *Manager {
	if configPath == "" {
		configPath = "runner.yaml"
	}
	m := &Manager{
		configPath: configPath,
		status:     StatusNotConfigured,
		config: Config{
			ServerURL:     "",
			RunnerToken:   "",
			CloneCacheDir: "~/.octopus/clones",
			LocalQueueDB:  "~/.octopus/runner.db",
			WebPort:       8088,
		},
		history: make([]*JobRecord, 0),
	}
	_ = m.LoadConfig()
	return m
}

func (m *Manager) ConfigPath() string {
	return m.configPath
}

func (m *Manager) LoadConfig() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Environment variable overrides
	if envURL := os.Getenv("OCTOPUS_SERVER_URL"); envURL != "" {
		m.config.ServerURL = envURL
	}
	if envToken := os.Getenv("OCTOPUS_RUNNER_TOKEN"); envToken != "" {
		m.config.RunnerToken = envToken
	}

	data, err := os.ReadFile(m.configPath)
	if err != nil {
		return err
	}
	expanded := os.Expand(string(data), os.Getenv)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return fmt.Errorf("parsing config %s: %w", m.configPath, err)
	}

	if cfg.ServerURL != "" {
		m.config.ServerURL = cfg.ServerURL
	}
	if cfg.RunnerToken != "" {
		m.config.RunnerToken = cfg.RunnerToken
	}
	if cfg.CloneCacheDir != "" {
		m.config.CloneCacheDir = cfg.CloneCacheDir
	}
	if cfg.LocalQueueDB != "" {
		m.config.LocalQueueDB = cfg.LocalQueueDB
	}
	if cfg.WebPort != 0 {
		m.config.WebPort = cfg.WebPort
	}
	if len(cfg.ProjectIDs) > 0 {
		m.config.ProjectIDs = cfg.ProjectIDs
	}

	return nil
}

func (m *Manager) SaveConfig(cfg Config) error {
	m.mu.Lock()
	if cfg.CloneCacheDir == "" {
		cfg.CloneCacheDir = "~/.octopus/clones"
	}
	if cfg.LocalQueueDB == "" {
		cfg.LocalQueueDB = "~/.octopus/runner.db"
	}
	if cfg.WebPort == 0 {
		cfg.WebPort = 8088
	}
	m.config = cfg
	configPath := m.configPath
	m.mu.Unlock()

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	dir := filepath.Dir(configPath)
	if dir != "." && dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}

	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		return fmt.Errorf("writing config %s: %w", configPath, err)
	}
	return nil
}

func (m *Manager) GetConfig() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

func (m *Manager) GetStatus() (RunnerStatus, string, *time.Time, *JobRecord, []Config) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.status, m.lastError, m.lastConnected, m.activeJob, nil
}

func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.cancelRunner != nil {
		m.cancelRunner()
	}

	cfg := m.config
	if cfg.ServerURL == "" || cfg.RunnerToken == "" {
		m.status = StatusNotConfigured
		m.lastError = "Server URL and Runner Token are required"
		m.mu.Unlock()
		slog.Info("runner: not configured yet, open web UI to configure", "web_url", fmt.Sprintf("http://localhost:%d", m.GetWebPort()))
		return
	}

	runnerCtx, cancel := context.WithCancel(ctx)
	m.cancelRunner = cancel
	m.status = StatusConnecting
	m.lastError = ""
	m.mu.Unlock()

	go m.runLoop(runnerCtx, cfg)
}

func (m *Manager) Restart(ctx context.Context) {
	m.Start(ctx)
}

func (m *Manager) Stop() {
	m.mu.Lock()
	if m.cancelRunner != nil {
		m.cancelRunner()
		m.cancelRunner = nil
	}
	if m.queue != nil {
		_ = m.queue.Close()
		m.queue = nil
	}
	m.status = StatusDisconnected
	m.mu.Unlock()
}

func (m *Manager) GetWebPort() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.config.WebPort != 0 {
		return m.config.WebPort
	}
	return 8088
}

func (m *Manager) runLoop(ctx context.Context, cfg Config) {
	cloneDir := ExpandHome(cfg.CloneCacheDir)
	queueDB := ExpandHome(cfg.LocalQueueDB)

	queue, err := NewLocalQueue(queueDB)
	if err != nil {
		m.mu.Lock()
		m.status = StatusError
		m.lastError = fmt.Sprintf("local queue init: %v", err)
		m.mu.Unlock()
		slog.Error("runner: local queue failed", "error", err)
		return
	}

	m.mu.Lock()
	m.queue = queue
	m.mu.Unlock()

	client := &Client{
		ServerURL:      cfg.ServerURL,
		Token:          cfg.RunnerToken,
		Dispatcher:     tools.NewLocalDispatcher(cloneDir),
		Queue:          queue,
		ReconnectDelay: 3 * time.Second,
	}

	m.mu.Lock()
	m.client = client
	m.mu.Unlock()

	slog.Info("octopus-runner client starting", "server_url", cfg.ServerURL)

	for {
		if ctx.Err() != nil {
			return
		}

		m.mu.Lock()
		m.status = StatusConnecting
		m.mu.Unlock()

		err := client.connectAndServeWithHooks(ctx, m.onConnect, m.onDisconnect, m.onJobStart, m.onJobFinish)
		if ctx.Err() != nil {
			return
		}

		m.mu.Lock()
		m.status = StatusError
		if err != nil {
			m.lastError = err.Error()
		}
		m.mu.Unlock()

		slog.Error("runner: connection lost, retrying", "error", err, "retry_in", "3s")

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (m *Manager) onConnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = StatusConnected
	m.lastError = ""
	now := time.Now()
	m.lastConnected = &now
	slog.Info("runner: connected to server", "server", m.config.ServerURL)
}

func (m *Manager) onDisconnect(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = StatusDisconnected
	if err != nil {
		m.lastError = err.Error()
	}
}

func (m *Manager) onJobStart(job *domain.GitJob) {
	m.mu.Lock()
	defer m.mu.Unlock()

	payloadBytes, _ := json.Marshal(job.Payload)
	record := &JobRecord{
		ID:        job.ID,
		ProjectID: job.ProjectID,
		RunID:     job.RunID,
		Type:      string(job.Type),
		Payload:   string(payloadBytes),
		Status:    "running",
		StartedAt: time.Now(),
	}
	m.activeJob = record
	slog.Info("runner: executing job", "job_id", job.ID, "type", job.Type, "project", job.ProjectID)
}

func (m *Manager) onJobFinish(job *domain.GitJob, res *domain.GitJobResult) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	var duration string
	var status string = "failed"
	if res != nil && res.Success {
		status = "success"
	}

	record := m.activeJob
	if record != nil && record.ID == job.ID {
		record.EndedAt = &now
		record.Duration = now.Sub(record.StartedAt).Round(time.Millisecond).String()
		record.Status = status
		record.Result = res
		m.activeJob = nil
	} else {
		record = &JobRecord{
			ID:        job.ID,
			ProjectID: job.ProjectID,
			RunID:     job.RunID,
			Type:      string(job.Type),
			Status:    status,
			StartedAt: now,
			EndedAt:   &now,
			Duration:  "0s",
			Result:    res,
		}
	}

	// Prepend to history, keep up to 50
	m.history = append([]*JobRecord{record}, m.history...)
	if len(m.history) > 50 {
		m.history = m.history[:50]
	}
	slog.Info("runner: job finished", "job_id", job.ID, "status", status, "duration", duration)
}

func (m *Manager) GetHistory() []*JobRecord {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*JobRecord, len(m.history))
	copy(out, m.history)
	return out
}

func (m *Manager) CheckTools() []ToolInfo {
	toolsList := []struct {
		name string
		bin  string
		arg  string
	}{
		{"Git", "git", "--version"},
		{"Claude Code CLI", "claude", "--version"},
		{"Gemini CLI", "gemini", "--version"},
		{"Codex CLI", "codex", "--version"},
	}

	var results []ToolInfo
	for _, t := range toolsList {
		path, err := exec.LookPath(t.bin)
		info := ToolInfo{
			Name:      t.name,
			Installed: err == nil,
			Path:      path,
		}
		if err == nil {
			out, vErr := exec.Command(t.bin, t.arg).Output()
			if vErr == nil {
				info.Version = strings.TrimSpace(string(out))
				// take first line
				if idx := strings.Index(info.Version, "\n"); idx != -1 {
					info.Version = info.Version[:idx]
				}
			}
		}
		results = append(results, info)
	}
	return results
}

func ExpandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[2:])
}
