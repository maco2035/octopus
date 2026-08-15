# Octopus — Home Assistant Add-on

Runs the same `octopus` binary as the root `Dockerfile`, packaged for the
Home Assistant Supervisor. Config options (API keys, Slack credentials,
branch naming pattern) are set from the add-on's **Configuration** tab in
the HA UI instead of a mounted `config.yaml` — `run.sh` translates them into
one at container start.

Persistent state (SQLite DB) lives at `/data/octopus.db`, inside the add-on's
normal persistent `/data` volume — it survives add-on restarts/updates.

By default (`runner_enabled: true`) this add-on acts as the central brain
only — it dispatches git/coding-CLI work to `octopus-runner` processes
running on your dev machines rather than doing it itself (see the root
[`README.md`](../README.md) for `octopus-runner` install/registration
steps). Set `runner_enabled: false` if you'd rather have this add-on
execute git/CLI work directly on the HA host — only sensible if the HA
host itself has git, network access to your repos, and whichever coding
CLIs your pipelines use.

Set `web_base_url` (e.g. your Nabu Casa/HA Cloud URL) if you're using
Slack — it's what "Open in web UI" links in review cards point at.

The web UI is exposed via **Ingress**, so it's reachable through the Home
Assistant UI itself (behind HA's own login) rather than needing a separate
exposed port with its own auth — this satisfies the "web UI needs a network
boundary" requirement from `PLAN.md` Phase 8 for free, as long as you're
using Ingress rather than the raw `8080/tcp` port mapping.

## Building this add-on

Home Assistant Supervisor builds an add-on using **the add-on's own folder**
as the Docker build context — it can't see files elsewhere in this repo. But
`ha-addon/Dockerfile` needs the repo root (`go.mod`, `cmd/`, `internal/`) to
build the binary. Two ways to reconcile that:

1. **Local/manual build** (works today): from the repo root, run
   `docker build -f ha-addon/Dockerfile -t octopus-addon --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:3.19 .`
   — note the context is `.` (repo root), not `ha-addon/`.
2. **Installing via a HA add-on repository** (Supervisor adding this repo
   directly): Supervisor will hit the context problem above. The standard
   fix is to build and publish a multi-arch image via CI (e.g. GitHub
   Actions, building with repo-root context exactly like option 1) and set
   `image: "ghcr.io/<you>/octopus-addon-{arch}"` in `config.yaml` — then
   Supervisor just pulls the pre-built image instead of building locally.
   No CI workflow is wired up yet since this repo has no remote configured;
   add one when you're ready to publish.
