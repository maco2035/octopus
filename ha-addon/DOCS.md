# Octopus — Home Assistant Add-on

## Install

1. Home Assistant → **Settings → Add-ons → Add-on Store** (some HA versions
   show this as **Settings → Apps → App Store**) → **⋮ (top right) →
   Repositories** → paste `https://github.com/maco2035/octopus` → **Add**.
2. Find **Octopus** in the store list (you may need to refresh the page)
   and click **Install**. This pulls a pre-built image from GHCR — it does
   not compile anything on your Home Assistant host, so it's fast and
   doesn't need the Go toolchain or a git checkout on that machine.
3. Open the **Configuration** tab, set `admin_password` (and at least one
   of the API key fields for whichever coding CLI you plan to use), and
   hit **Save**.
4. **Start** the add-on, then open its **Web UI** (or find it in the HA
   sidebar if Ingress is on, which it is by default) and log in with
   `admin` / whatever you set as `admin_password`.

That's it — no `hash-password` command to run yourself, no local build, no
config file to hand-edit. `run.sh` hashes the password and writes the real
`config.yaml` for you at container start, from whatever you filled in on
the Configuration tab.

## Configuration options

Only `admin_password` is actually required. Everything else has a working
default or can stay blank until you need it:

| Option | Required? | What it's for |
|---|---|---|
| `admin_username` | no (defaults to `admin`) | web UI login |
| `admin_password` | **yes** | web UI login — set this to anything before starting |
| `gemini_api_key` / `anthropic_api_key` / `openai_api_key` | no | only needed for the coding-CLI presets that use that provider; leave blank if you're not using it yet |
| `slack_signing_secret` | no | leave blank to skip Slack entirely; see the root [`README.md`](../README.md) for the app manifest and how to get this value |
| `branch_pattern` | no (has a default) | how per-run git branches are named |
| `runner_enabled` | no (defaults to `true`) | see below |
| `web_base_url` | no | only matters once you're using Slack (see below) |

All the secret-shaped fields are masked in the Configuration tab.

## Runners

By default (`runner_enabled: true`) this add-on is the central brain
only — it dispatches git/coding-CLI work to `octopus-runner` processes on
your dev machines rather than doing it itself (see the root
[`README.md`](../README.md) for `octopus-runner` install/registration
steps: web UI → Runners → New runner). Set `runner_enabled: false` if
you'd rather this add-on execute git/CLI work directly on the HA host —
only sensible if the HA host itself has git, network access to your
repos, and whichever coding CLIs your pipelines use.

Set `web_base_url` (e.g. your Nabu Casa/HA Cloud URL) once you're using
Slack — it's what "Open in web UI" links in review cards point at.

## Persistence & networking

Persistent state (SQLite DB, per-run git clones) lives under `/data`,
inside the add-on's normal persistent volume — it survives add-on
restarts/updates.

The web UI is exposed via **Ingress**, so it's reachable through the Home
Assistant UI itself (behind HA's own login) rather than needing a separate
exposed port with its own auth.

## For maintainers: how the pre-built image gets published

Supervisor only ever uses this `ha-addon/` folder as the Docker build
context when installing directly from a repository — it can't see
`go.mod`/`cmd/`/`internal` at the repo root, so a local build here fails no
matter how this folder is arranged (verified: `docker build -f
ha-addon/Dockerfile ha-addon/` fails on the very first `COPY go.mod`; the
same build with the repo root as context succeeds).

`.github/workflows/publish-ha-addon.yml` builds `ha-addon/Dockerfile` with
the **repo root** as context (CI has the whole repo checked out, so this
isn't a problem there) on every push to `main` that touches the Go source
or the add-on files, and pushes multi-arch images to
`ghcr.io/maco2035/octopus-addon-{amd64,aarch64}`. `config.yaml`'s `image:`
field points at that, so Supervisor just pulls it — no local build, no
Go toolchain needed on the HA host, no repo-root-context problem.

**One-time step after the workflow's first run:** GHCR packages default to
private, and Supervisor pulls anonymously. Go to the repo's **Packages**
tab on GitHub (or
`https://github.com/users/maco2035/packages/container/octopus-addon-amd64/settings`,
and the same for `-aarch64`) and set visibility to **Public** — otherwise
installs will fail to pull the image.

If you change something under `cmd/`, `internal/`, or `ha-addon/` and want
it live, either push to `main` (the workflow runs automatically) or
trigger it manually from the Actions tab (`workflow_dispatch`).
