# Octopus

A control plane that orchestrates real agentic coding CLIs (Claude Code,
Codex CLI, Gemini CLI) against real tickets across multiple projects — the
central server decides what needs to happen next and dispatches it to
whichever dev machine can do the work, and gates progress behind human
review via Slack or the web UI. See [`PLAN.md`](PLAN.md) for the full
design.

## Quick start (single machine, no runner)

```sh
cp config.example.yaml config.yaml
go run ./cmd/octopus hash-password        # generates a bcrypt hash — enter a password when prompted
# put that hash in config.yaml's auth.admin_password_hash (or export OCTOPUS_ADMIN_PASSWORD_HASH)
go run ./cmd/octopus
curl localhost:8080/healthz
```

Open `http://localhost:8080`, log in, create a project (pointing at a real
git remote), build a pipeline in the drag-and-drop editor, and hit Run. In
this mode the server executes git and coding-CLI work itself
(`tools.LocalDispatcher`) — simplest to get started with, but everything
runs on whatever machine the server is on, and that machine needs the
relevant coding CLIs (`claude` / `codex` / `gemini`) installed and any
API keys it needs available as `agents.*_api_key` in `config.yaml`.

## Multi-machine (the intended real deployment)

Set `runner.enabled: true` in `config.yaml`. The server now dispatches
git/coding-CLI work over `/runner/connect` to any connected
`octopus-runner` process instead of doing it itself — no AI provider keys
need to live on the server's own disk beyond what's in `config.yaml`
already, and the machine actually running `claude`/`codex`/`gemini` can be
your laptop, not wherever the server happens to be hosted.

On each dev machine that should do work:

```sh
go build -o octopus-runner ./cmd/octopus-runner
cp runner.example.yaml runner.yaml
# log into the web UI > Runners > "New runner" — copy the token it shows you (shown once)
# put server_url + that token in runner.yaml
OCTOPUS_RUNNER_CONFIG=./runner.yaml ./octopus-runner
```

Make sure the runner machine has `git`, and whichever of `claude` /
`codex` / `gemini` your pipelines use, actually installed and on `PATH` —
the runner shells out to them non-interactively (see `internal/tools`).

A project's default pipeline (used by the Slack slash command, and shown
in its project page) is whichever `PipelineDef` was saved first; change it
from the project page's "Make default" button.

## Slack integration

Set `slack.signing_secret` (and, if you plan to extend the notifier beyond
`response_url`, `slack.bot_token`) in `config.yaml` — the gateway turns on
automatically once a signing secret is present. Create the Slack app from
[`slack-app-manifest.yaml`](slack-app-manifest.yaml) (api.slack.com/apps →
"Create New App" → "From an app manifest"), swap in your real server
address in both request URLs, install it to your workspace, and copy the
Signing Secret from the app's "Basic Information" page into
`config.yaml`/`SLACK_SIGNING_SECRET`.

Usage: `/octopus <project_id> <ticket_id>` starts a run against that
project's default pipeline; Octopus posts a Block Kit card back to the
same channel the moment the run reaches any review gate (not just the
final one), with an "Approve" button and an "Open in web UI" link for
anything that needs an edit or a reject.

## Run in Docker

```sh
cp config.example.yaml config.yaml   # first time only
docker compose up --build
```

This runs the server only — `octopus-runner` isn't containerized (it needs
a real local git identity, SSH/PAT credentials, and whichever coding CLIs
are installed on that specific machine, so it's meant to run natively on
each dev machine, not as a disposable container).

## Run as a Home Assistant add-on

See [`ha-addon/DOCS.md`](ha-addon/DOCS.md) — this is the deployment shape
`PLAN.md` is actually written around: an always-on HA box as the central
brain (`runner_enabled: true` by default in the add-on), your laptop as a
runner that comes and goes.

## Test

```sh
go test ./... -race
```

Almost everything is covered by real integration tests rather than mocks:
`internal/tools` runs actual `git` against local repos; the Phase 6/7
tests spin up a real `runnerhub.Hub`, real `runner.Client`s, and real
WebSocket connections, standing in only for the coding CLI itself (a small
fixture script satisfies the same `tools.CLIInvocation` contract a real
`claude`/`codex`/`gemini` binary would, since this environment has none of
those installed).

## Status

All of `PLAN.md`'s phases are implemented: the DAG engine, SQLite
persistence with crash-safe checkpoint/resume, the concurrent-run
scheduler, the web UI (drag-and-drop pipeline editor, login, review
gates), the Slack gateway, CLI-delegated agents with a real git toolchain,
and the multi-machine runner protocol (WebSocket hub + runner, durable
local queue, AWAITING_RUNNER queuing with automatic resume on reconnect,
cross-machine handoff, server-restart recovery). Phase 8 hardening
(structured logging, rate limiting on `/login`/the run-trigger
endpoint/`/api/slack/command`, runner token revocation) is in place too.

Known gaps, called out honestly rather than glossed over:
- **Codex CLI and Gemini CLI invocation flags are best-effort, not
  verified** — this environment has neither binary installed to check
  against. `ClaudeCodeInvocation` in `internal/tools/cli_invoke.go` is
  accurate to the real Claude Code CLI; confirm `CodexInvocation` and
  `GeminiInvocation` against each tool's actual `--help` before relying on
  them.
- No Grok/xAI preset — there's no published xAI agentic coding CLI to
  delegate to the way Claude Code/Codex CLI/Gemini CLI work.
- Discord and ServiceNow gateways (Phase 9) aren't built — `PLAN.md` marks
  them optional, "only build if actually needed."
