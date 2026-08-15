// Command octopus-runner is the small binary installed on each dev machine
// (PLAN.md Phase 7): it opens a persistent outbound connection to the
// central Octopus server, authenticates with a per-runner token, and
// executes whatever GitJobs the server dispatches to it — using the exact
// same git/CLI execution code (internal/tools) Phase 6 runs directly on
// the server.
package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"octopus/internal/runner"
	"octopus/internal/tools"
)

// Config is runner.example.yaml's shape. ProjectIDs is read and logged for
// the operator's own reference but isn't what authorizes anything — which
// projects this runner may serve is decided server-side, by whatever
// project_ids the web UI's Runners page attached to this token when it was
// minted. A runner declaring its own scope here and having that trusted
// blindly would let a stolen token grant itself access to projects it was
// never actually registered for.
type Config struct {
	ServerURL     string   `yaml:"server_url"`
	RunnerToken   string   `yaml:"runner_token"`
	ProjectIDs    []string `yaml:"project_ids"`
	CloneCacheDir string   `yaml:"clone_cache_dir"`
	LocalQueueDB  string   `yaml:"local_queue_db"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}
	expanded := os.Expand(string(data), os.Getenv)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	var missing []string
	if cfg.ServerURL == "" {
		missing = append(missing, "server_url")
	}
	if cfg.RunnerToken == "" {
		missing = append(missing, "runner_token")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("config missing required fields: %v", missing)
	}

	if cfg.CloneCacheDir == "" {
		cfg.CloneCacheDir = "~/.octopus/clones"
	}
	if cfg.LocalQueueDB == "" {
		cfg.LocalQueueDB = "~/.octopus/runner.db"
	}
	cfg.CloneCacheDir = expandHome(cfg.CloneCacheDir)
	cfg.LocalQueueDB = expandHome(cfg.LocalQueueDB)

	return &cfg, nil
}

func expandHome(path string) string {
	if !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return home + path[1:]
}

func main() {
	configPath := os.Getenv("OCTOPUS_RUNNER_CONFIG")
	if configPath == "" {
		configPath = "runner.yaml"
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	queue, err := runner.NewLocalQueue(cfg.LocalQueueDB)
	if err != nil {
		log.Fatalf("local queue: %v", err)
	}
	defer queue.Close()

	client := &runner.Client{
		ServerURL:  cfg.ServerURL,
		Token:      cfg.RunnerToken,
		Dispatcher: tools.NewLocalDispatcher(cfg.CloneCacheDir),
		Queue:      queue,
	}

	slog.Info("octopus-runner starting", "server_url", cfg.ServerURL, "project_ids", cfg.ProjectIDs)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatalf("runner: %v", err)
	}
}
