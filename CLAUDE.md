# CLAUDE.md — Octopus Project Guide

Octopus is a multi-agent AI pipeline orchestrator (drag-and-drop pipelines, multi-project, multi-machine runners, Slack approvals). It orchestrates coding agent CLIs (Claude Code, Gemini CLI, Codex) across tickets and projects, dispatching execution to remote dev machines.

---

## 🛠️ Common Commands

### Testing & Quality
```bash
# Run all tests
go test ./...

# Run specific package tests
go test -v ./internal/web/
go test -v ./internal/engine/
go test -v ./internal/store/
go test -v ./test/integration/
```

### Building Binaries
```bash
# Build Octopus Server
CGO_ENABLED=0 go build -ldflags "-X main.Version=$(cat VERSION)" -o octopus ./cmd/octopus

# Build Octopus Runner
CGO_ENABLED=0 go build -ldflags "-X main.Version=$(cat VERSION)" -o octopus-runner ./cmd/octopus-runner
```

### Running Locally (From Source)
```bash
# 1. Setup local config
cp octopus.example.yaml octopus.yaml

# 2. Generate admin password hash
go run ./cmd/octopus hash-password

# 3. Start server
go run ./cmd/octopus
```

---

## 🏃 Runner Deployment Modes

### 1. Standalone Binary (Primary / Recommended for Dev Machines)
Runs directly on the host with native access to dev tools, SSH keys, language compilers, and installed CLI agents (`claude`, `gemini`, `codex`):
```bash
# Build runner binary
go build -o octopus-runner ./cmd/octopus-runner

# Configure runner.yaml
cat > runner.yaml <<EOF
server_url: ws://<server-ip>:8080/runner/connect
runner_token: "<runner-token-from-web-ui>"
clone_cache_dir: ~/.octopus/clones
local_queue_db: ~/.octopus/runner.db
EOF

# Start runner
OCTOPUS_RUNNER_CONFIG=./runner.yaml ./octopus-runner
```

### 2. Docker Runner (Future / Isolated Deployment Idea)
Runs `octopus-runner` inside a containerized Debian Trixie environment:
```bash
# Using Docker Compose
OCTOPUS_SERVER_URL="ws://<server-ip>:8080/runner/connect" \
OCTOPUS_RUNNER_TOKEN="<runner-token>" \
docker compose -f docker-compose.runner.yml up -d

# Using docker run
docker run -d \
  --name octopus-runner \
  -e SERVER_URL="ws://<server-ip>:8080/runner/connect" \
  -e RUNNER_TOKEN="<runner-token>" \
  -v ~/.ssh:/root/.ssh:ro \
  -v octopus-runner-data:/root/.octopus \
  --restart unless-stopped \
  $(docker build -q -f Dockerfile.runner .)
```

---

## 🐳 Server Docker & Home Assistant Setup

- **Base Image**: Debian Trixie (`debian:trixie-slim` / `golang:1.26-trixie` / `ghcr.io/home-assistant/{arch}-base-debian:trixie`).
- **Server Compose**: `docker compose up -d` (bind-mounts `./data:/app/data`).
- **Home Assistant Add-on**: Auto-detects `/data` as persistent storage, connects via Ingress on `:8080`, with `X-Frame-Options: SAMEORIGIN` and dynamic cookie security.
- **Default Credentials**: `admin` / `admin` (overridden via `OCTOPUS_ADMIN_PASSWORD` / `OCTOPUS_ADMIN_USERNAME`).

---

## 📐 Architecture & Key Design Decisions

1. **Pure Go / No CGO**: Database uses `modernc.org/sqlite`. Zero external C dependencies.
2. **Control Plane / Runner Separation**: Central server (`cmd/octopus`) plans and schedules; remote runners (`cmd/octopus-runner`) open outbound WebSockets to execute Git and CLI agent jobs.
3. **Dynamic Cookie Security**: Session cookies evaluate incoming protocol (`r.TLS` / `X-Forwarded-Proto`) to work seamlessly on plain HTTP, LAN IPs, and Home Assistant Ingress without dropping session cookies.
4. **Input Safety**: All IDs/slugs are strictly validated with `slugPattern` (`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`) to prevent directory traversal and CLI injection.
