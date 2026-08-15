# Octopus

A control plane that orchestrates real agentic coding CLIs (Claude Code,
Codex CLI, Gemini CLI) against real tickets across multiple projects — the
central server decides what needs to happen next and dispatches it to
whichever dev machine can do the work, and gates progress behind human
review via Slack or the web UI. See [`PLAN.md`](PLAN.md) for the full
design.

## Quick start (single machine, no runner)

The simplest way to run Octopus at all is Docker Compose — see below. To
run straight from source instead:

```sh
cp octopus.example.yaml octopus.yaml
go run ./cmd/octopus hash-password        # generates a bcrypt hash — enter a password when prompted
# put that hash in octopus.yaml's auth.admin_password_hash (or export OCTOPUS_ADMIN_PASSWORD_HASH)
go run ./cmd/octopus
curl localhost:8080/healthz
```

Open `http://localhost:8080`, log in, create a project (pointing at a real
git remote), build a pipeline in the drag-and-drop editor, and hit Run. In
this mode the server executes git and coding-CLI work itself
(`tools.LocalDispatcher`) — simplest to get started with, but everything
runs on whatever machine the server is on, and that machine needs the
relevant coding CLIs (`claude` / `codex` / `gemini`) installed and any
API keys it needs available as `agents.*_api_key` in `octopus.yaml`.

## Multi-machine (the intended real deployment)

Set `runner.enabled: true` (`OCTOPUS_RUNNER_ENABLED=true` for Docker/the HA
add-on, `runner.enabled: true` in `octopus.yaml` from source). The server
now dispatches git/coding-CLI work over `/runner/connect` to any connected
`octopus-runner` process instead of doing it itself — no AI provider keys
need to live on the server's own disk beyond what it's already configured
with, and the machine actually running `claude`/`codex`/`gemini` can be
your laptop, not wherever the server happens to be hosted.

On each dev machine that should do work:

```sh
# Start the runner directly:
go run ./cmd/octopus-runner

# Or build the binary:
go build -o octopus-runner ./cmd/octopus-runner
./octopus-runner
```

When started, `octopus-runner` automatically hosts a local Web Dashboard at **`http://localhost:8088`**. You can open it in your browser to enter the server WebSocket URL and runner token, verify your CLI toolchains, and monitor live execution. Alternatively, you can pre-configure `runner.yaml` (from `runner.example.yaml`).

Make sure the runner machine has `git`, and whichever of `claude` /
`codex` / `gemini` your pipelines use, actually installed and on `PATH` —
the runner shells out to them non-interactively (see `internal/tools`).

A project's default pipeline (used by the Slack slash command, and shown
in its project page) is whichever `PipelineDef` was saved first; change it
from the project page's "Make default" button.

## Slack integration

Set the signing secret (`slack.signing_secret` in `octopus.yaml` from
source; `SLACK_SIGNING_SECRET` for Docker; `slack_signing_secret` in the HA
add-on's Configuration tab) — the gateway turns on automatically once one
is present. Create the Slack app from
[`slack-app-manifest.yaml`](slack-app-manifest.yaml) (api.slack.com/apps →
"Create New App" → "From an app manifest"), swap in your real server
address in both request URLs, install it to your workspace, and copy the
Signing Secret from the app's "Basic Information" page.

Usage: `/octopus <project_id> <ticket_id>` starts a run against that
project's default pipeline; Octopus posts a Block Kit card back to the
same channel the moment the run reaches any review gate (not just the
final one), with an "Approve" button and an "Open in web UI" link for
anything that needs an edit or a reject.

## Run in Docker

```sh
echo "OCTOPUS_ADMIN_PASSWORD=pick-something" > .env   # the only required setting
docker compose up --build
```

Then open **http://localhost:8080** and log in as `admin` / whatever you
put in `.env`. No config file to copy, no `hash-password` command to run
by hand — `docker-entrypoint.sh` hashes it and writes the real config
itself at container start. Data persists in the `octopus-data` named
volume. Everything else (API keys, Slack, the runner switch) is optional —
see `docker-compose.yml` for the full list of environment variables.

This runs the server only — `octopus-runner` isn't containerized (it needs
a real local git identity, SSH/PAT credentials, and whichever coding CLIs
are installed on that specific machine, so it's meant to run natively on
each dev machine, not as a disposable container).

### Changing the port / exposing it via Cloudflare Tunnel

Octopus always listens on `8080` *inside* the container — that's fixed.
What's configurable is the **host** side of the mapping:

```sh
echo "OCTOPUS_HOST_PORT=8085" >> .env
docker compose up --build
```

now reaches it at `http://localhost:8085`.

If you're putting this behind `cloudflared`, you likely don't need
`OCTOPUS_HOST_PORT` (or a published port at all): `docker-compose.yml` has
a commented-out `cloudflared` service that joins the same Docker network
and reaches Octopus at `http://octopus:8080` directly — the container port,
not whatever host port you picked. Uncomment it, set `TUNNEL_TOKEN` in
`.env` from a tunnel you create in the Cloudflare Zero Trust dashboard
(**Networks → Tunnels → Create a tunnel → Docker**, Public Hostname
pointing at `http://octopus:8080`), and you can drop the `ports:` block on
the `octopus` service entirely — nothing outside the Docker network can
reach it except through the tunnel.

## Run as a Home Assistant add-on

This is the deployment shape `PLAN.md` is actually written around: an
always-on HA box as the central brain, your laptop as a runner that comes
and goes.

1. HA → **Settings → Add-ons → Add-on Store** (shown as **Settings → Apps
   → App Store** on some HA versions) → **⋮ → Repositories** → add
   `https://github.com/maco2035/octopus`.
2. Install **Octopus**. Supervisor pulls a pre-built image from GHCR
   (`.github/workflows/publish-ha-addon.yml` builds and publishes it on
   every push to `main`) rather than building locally — Supervisor's
   local-build path turned out to hit a real Supervisor bug, failing every
   install with `Invalid token for access /addons/self/options/config`
   right after the build finished (home-assistant/supervisor#4111, #1930).
   One-time setup if you're maintaining your own fork: after the workflow
   runs once, its GHCR packages default to private — flip
   `octopus-addon-amd64` and `octopus-addon-aarch64` to Public under the
   repo's Packages tab, or Supervisor's anonymous pull will fail.
3. Configuration tab → set `admin_password` → Save → Start. Everything
   else (API keys, Slack, the runner switch) is optional, same as Docker.
4. Open the add-on's **Web UI** (or find Octopus in the HA sidebar —
   Ingress is on by default) and log in as `admin` / whatever you set.

To change the host-facing port (e.g. to run behind `cloudflared` on a
non-default port), the add-on's page has a **Network** tab — remap the
`8080/tcp` container port to whatever host port you want there, no config
file edit needed; Supervisor persists it. If you're using Ingress, though,
you don't need any of this: Ingress already reaches the add-on over HA's
internal network regardless of the host port mapping, so route `cloudflared`
at HA's own Ingress URL instead of Octopus's container port directly.

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
