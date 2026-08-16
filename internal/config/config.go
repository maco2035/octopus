package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port   int          `yaml:"port"`
	Agents AgentsConfig `yaml:"agents"`
	Slack  SlackConfig  `yaml:"slack"`
	Store  StoreConfig  `yaml:"store"`
	Git    GitConfig    `yaml:"git"`
	Web    WebConfig    `yaml:"web"`
	Auth   AuthConfig   `yaml:"auth"`
	Runner RunnerConfig `yaml:"runner"`
}

// RunnerConfig switches the server between Phase 6's LocalDispatcher
// (git/CLI work runs directly on the server — the default, no setup
// needed) and Phase 7's runnerhub.Hub (work is routed to connected
// octopus-runner processes over /runner/connect). Both implement the same
// domain.JobDispatcher; this is the "purely a wiring change in main.go"
// PLAN.md Phase 7 describes.
type RunnerConfig struct {
	Enabled bool `yaml:"enabled"`
}

type AgentsConfig struct {
	AntigravityAPIKey string `yaml:"antigravity_api_key"`
	AnthropicAPIKey   string `yaml:"anthropic_api_key"`
	OpenAIAPIKey      string `yaml:"openai_api_key"`
	XAIAPIKey         string `yaml:"xai_api_key"` // Grok
}

// AuthConfig holds the single v1 admin account (PLAN.md Key Design Decision
// 23). AdminPasswordHash is a bcrypt hash, generated with
// `octopus hash-password` — never a plaintext password. Not yet enforced
// by validate(): the login system that reads these lands in Phase 2/4, and
// requiring them here would break booting the bare Phase 0/1 skeleton.
type AuthConfig struct {
	AdminUsername     string `yaml:"admin_username"`
	AdminPasswordHash string `yaml:"admin_password_hash"`
}

type SlackConfig struct {
	BotToken      string `yaml:"bot_token"`
	SigningSecret string `yaml:"signing_secret"`
}

type StoreConfig struct {
	Driver string `yaml:"driver"`
	DSN    string `yaml:"dsn"`
}

type GitConfig struct {
	BranchPattern string `yaml:"branch_pattern"`
	CloneCacheDir string `yaml:"clone_cache_dir"` // where run checkouts live, scoped per (project_id, run_id); defaults to ./data/clones if empty
}

type WebConfig struct {
	Enabled bool   `yaml:"enabled"`
	BaseURL string `yaml:"base_url"` // used to build Slack "Open in web UI" links; defaults to http://localhost:<port> if empty
}

// Load reads and validates config from path, expanding ${VAR} references
// against the process environment first. It fails fast on missing required
// fields rather than deferring to first request. Agent API keys aren't
// required yet — they become required once Phase 6 wires real agents that
// use them; requiring them at Phase 0 would block booting the bare skeleton.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %s: %w", path, err)
	}

	expanded := os.Expand(string(data), os.Getenv)

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config %s: %w", path, err)
	}

	if cfg.Port == 0 {
		cfg.Port = 8080
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	var missing []string
	if c.Store.Driver == "" {
		missing = append(missing, "store.driver")
	}
	if c.Store.DSN == "" {
		missing = append(missing, "store.dsn")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config missing required fields: %v", missing)
	}
	return nil
}
