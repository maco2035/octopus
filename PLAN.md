# Octopus — Multi-Agent Orchestration Server

**Status:** Planning
**Language:** Go (central server + `octopus-runner` agent binary) + server-rendered HTML/htmx + vanilla JS (web UI, no frontend build step)
**Purpose:** A control plane that runs multi-model AI agent pipelines (e.g. Gemini for drafting, Claude for review, Codex for reporting) against real tickets across multiple projects, dispatches Git workflows to whichever dev machine is available, and gates progress behind human review via Slack/Discord or the web UI.

This doc is meant to live at the root of a fresh repo as `PLAN.md` and be handed to Claude Code as the source of truth for implementation order. Each phase is scoped to be doable in one sitting and independently testable.

---

## 1. Goals & Non-Goals

**Goals**
- Trigger a pipeline from Slack (`/octopus <ticket-id>`) **or** from a web UI, run agent nodes against shared state, and post/show a review card.
- Pipelines are a **DAG**, not a fixed sequence: independent nodes can run **in parallel** (e.g. a linter agent and a security agent firing at once), while dependent nodes wait on their upstream nodes.
- **Web UI lets you drag and drop agent nodes onto a canvas**, wire them into a DAG (including parallel branches), and save that as a reusable pipeline definition per project.
- **Multiple projects (repos) run concurrently** — each project has its own pipeline definition(s) and its own in-flight runs; working on project A doesn't block project B.
- **One central server holds all state and orchestration logic; git/shell work is dispatched to a lightweight `octopus-runner` process installed on each dev machine.** Runners connect *outbound* to the server (like a self-hosted CI runner) — no inbound networking or port-forwarding needed on a laptop. You never have to think about "which machine is the server" — that's one fixed, always-on host; your dev machines just come and go as runners.
- Since real work syncs through GitHub already (branches cut from a release line, pushed, merged), Octopus doesn't invent its own locking between machines/people — a runner just fetches, branches, commits, and pushes like a human would.
- **Every run gets its own git branch, created automatically before any agent node executes.** No agent ever picks a branch name — the engine has a runner cut it off the base branch as an implicit first step, and every git operation for that run is locked to that branch. Nothing gets built in an ad hoc location.
- **Any node can be a human review gate, not just a final merge step.** A pipeline builder can flag a node "pause for review." When that node finishes, the run halts and its output (e.g. a plan, a draft, a diff) is shown in the web UI for editing — a human can fix it up and continue the pipeline from there, or approve as-is via Slack.
- **All output is centralized, no matter which machine ran it.** Git/shell work executed by a remote runner streams its full output back to the central server and is stored with the run — logs, diffs, and node outputs are always visible from one place (web UI or Slack), regardless of which dev machine did the work.
- **A runner that loses its connection mid-job doesn't lose the work.** If a laptop goes offline (bad signal, closed lid, a train tunnel) while it's already executing a git job, it keeps working from local state and queues its result durably until it can reach the server again — nothing gets re-done or dropped. Starting a *brand-new* pipeline still needs a connection (to the scheduler and to the AI provider APIs), so agent nodes simply wait for connectivity like any other queued step — no offline LLM fallback in v1.
- Survive a process restart at any point mid-pipeline, including mid-parallel-fanout and mid-review-pause — state must be durable, not goroutine-local.
- Make it hard to accidentally merge unreviewed code or let an unauthenticated request/runner trigger work.
- Keep agents pluggable via a registry: adding a new agent type means writing one file and registering it — the web UI then lists it as a draggable node automatically, no further app code changes.
- Data model leaves room for **more than one person** using Octopus eventually (e.g. an `owner` field on projects/runs), without building real auth/multi-tenancy now.

**Non-goals (for v1)**
- No full multi-tenant / cross-org isolation — single team, single Slack workspace, shared database. Schema is forward-compatible with multiple people, but there's no login system or access control yet.
- No distributed consensus / leaderless coordination between machines. There is exactly one central brain (the server); runners are dumb, replaceable, stateless workers. If you want "no single machine matters," that's solved by hosting the server somewhere reliable (small VM, NAS, always-on box) — not by making every machine a peer.
- No public exposure of the web UI without a network boundary (VPN/localhost/reverse-proxy auth) — that's called out explicitly in hardening, not solved by the app itself in v1.
- Discord / ServiceNow gateways remain optional, deferred to the end.

---

## 2. Key Design Decisions (changes from the first draft)

These are fixes/extensions to the original sketch, decided up front so Claude Code doesn't have to re-derive them mid-build:

1. **Persistence from day one.** Pipeline state is stored in SQLite (via `modernc.org/sqlite`, no CGO needed, **WAL mode** for concurrent readers/writers) behind a small `Store` interface, not held only in a goroutine.
2. **Slack request verification is mandatory, not a TODO.** Every Slack webhook handler verifies the `X-Slack-Signature` header using the signing secret before doing anything else. Phase work, not hardening bolted on later.
3. **Pipeline definitions are data, not code.** A `PipelineDef` is nodes + edges (a DAG), persisted as JSON in SQLite, scoped to a `Project`. This is what makes the drag-and-drop UI possible — editing a pipeline means editing rows, not editing Go source.
4. **Agent registry pattern.** `internal/agents/registry.go` maps an `agent_type` string to a constructor. The web UI queries `/api/agent-types` to populate the drag-and-drop palette; the engine uses the same registry to instantiate nodes at run time.
5. **Checkpointing tracks a set of completed nodes, not a single "last node."** Because nodes can finish out of order within a parallel DAG level, `Resume` recomputes the DAG frontier (nodes whose dependencies are all complete) and re-runs only what's left.
6. **Review gates are generalized, not just a final approval.** Any `NodeDef` can be flagged `RequiresReview`. When such a node finishes, the engine checkpoints, sets the run to `AWAITING_REVIEW` with a `PendingNodeID`, and stops advancing the DAG. A human can approve as-is, edit that node's output and continue, or reject (ends the run). One-time `action_token`s make approve/edit/reject idempotent — a second click on a stale token is a no-op with a friendly message, not a second merge attempt.
7. **`docker-compose.yml` does not mount the Docker socket** unless a concrete need shows up. Root-equivalent host access isn't a default.
8. **Security agent can block, not just log.** `PipelineState.Status` gets a `BLOCKED` value the security node can set; the engine checks this after each node and halts the run (no review card sent) rather than proceeding.
9. **Config via a single `config.yaml` + env var overrides**, loaded once at startup and validated (missing API keys fail fast at boot, not on first request).
10. **Web UI is server-rendered Go templates + htmx**, with a small vendored vanilla-JS library (e.g. SortableJS) for the drag-and-drop canvas interactions. No React/Vue/SPA build step — stays inside the Go module/binary.
11. **Scheduler manages concurrent runs.** Each pipeline run executes in its own goroutine tracked by a `Scheduler`; within a run, each DAG "level" (nodes whose deps are all satisfied) executes concurrently via `errgroup.Group`, then the engine advances to the next level.
12. **`Project` is a first-class entity** (id, name, git remote URL, base_branch, nullable `owner`) — every pipeline def and run is scoped to a project. This is what "multiple projects at once" hangs off of.
13. **Every run auto-creates its branch before any node runs.** The engine's first action on a new run is a `prepare_branch` git job (fetch `base_branch`, create `<branch_pattern>`, default `octopus/{ticket_id}`) dispatched to a runner. The resulting branch name is stored on `PipelineState.GitBranch` and every later git job for that run references it — agents never choose a branch.
14. **Git/shell work is dispatched to runners over an outbound connection, not executed by the server itself.** `octopus-runner` (a separate small binary) runs on each dev machine, opens a persistent outbound connection to the server, authenticates with a per-runner token, and declares which project IDs it can do git work for. The server never needs to reach into a dev machine's network.
15. **No runner online is not a failure.** If a run needs a git job and no runner is currently connected for that project, the run's status becomes `AWAITING_RUNNER` (queued, not failed) and resumes automatically the moment a matching runner reconnects. Laptops closing is an expected, ordinary state, not an error path.
16. **Runner job results carry full output, not just success/fail.** Every `GitJobResult` includes stdout/stderr, persisted on the run. This is what makes logs "come up to the main person" regardless of which machine actually ran the git command — the web UI and Slack always show the same central record.
17. **Runners keep a small local durable queue.** `octopus-runner` persists in-progress and unsent-result jobs to its own local SQLite file (`~/.octopus/runner.db`, separate from the server's DB). If the connection to the server drops mid-job, the runner keeps executing against local state (a git commit doesn't need the server's help) and queues the result. On reconnect it flushes everything it's queued; the server dedupes by `GitJob.ID` so a redelivered result is a safe no-op, never a double-apply.
18. **`push` retries locally with backoff instead of failing.** It's the one git step that inherently needs a live connection (to GitHub, not to the Octopus server). A runner mid-push when connectivity drops just keeps retrying with backoff on its own until it succeeds — it doesn't need the central server's involvement to do that, and the job isn't marked failed just because a network blip happened.
19. **The server persists outstanding dispatched jobs too, not just the runner.** A `GitJob` the server has sent but hasn't gotten a `GitJobResult` for yet is saved via `SaveGitJob` before dispatch. If the server itself restarts while a job is in flight (runner offline, job mid-retry, whatever), `runnerhub` reloads pending jobs via `LoadPendingGitJobs` on boot and keeps waiting for their results — a late-arriving `GitJobResult` is matched by `GitJob.ID` and applied correctly even if the server restarted in between. Checkpointing covers completed nodes; this covers nodes that are dispatched but not yet resolved.
20. **Every git job fetches before acting and pushes after mutating — handoff between machines always goes through GitHub, never through assuming one runner stays assigned to a run.** `prepare_branch` pushes the new (even empty) branch immediately so it's visible on GitHub right away. This matters because the runner that handles a run's first job isn't guaranteed to be the one that handles its next job — if runner A creates a branch and goes offline, and runner B (also registered for that project) picks up the following job, B needs to be able to `git fetch` and see exactly what A already pushed.
21. **A runner's local clone is scoped per `(project_id, run_id)`, not per project.** Two concurrent runs against the same project (two different tickets) landing on the same runner must not share one working directory — each run gets its own clone/worktree under `clone_cache_dir/<project_id>/<run_id>`, so simultaneous runs never clobber each other's checkout.

---

## 3. Directory Structure

```
octopus/
├── PLAN.md
├── README.md
├── repository.yaml             # lets this repo be added directly as a Home Assistant add-on repository
├── .gitignore
├── Dockerfile                  # generic self-host image (docker / docker-compose)
├── docker-compose.yml
├── go.mod
├── go.sum
├── config.example.yaml
├── runner.example.yaml         # config template for octopus-runner (server URL, token, project IDs)
├── ha-addon/                   # Home Assistant add-on packaging (separate Dockerfile, HA base image + bashio)
│   ├── config.yaml             # add-on manifest: options schema, ingress, ports
│   ├── build.yaml              # per-arch BUILD_FROM base images
│   ├── Dockerfile
│   ├── run.sh                  # translates HA add-on options into config.yaml at container start
│   └── DOCS.md
├── cmd/
│   ├── octopus/
│   │   └── main.go             # central server
│   └── octopus-runner/
│       ├── main.go             # runner binary installed on each dev machine
│       └── localqueue.go       # durable local queue (own SQLite file) for in-progress jobs + unsent results
├── internal/
│   ├── domain/
│   │   ├── agent.go            # Agent interface
│   │   ├── project.go          # Project
│   │   ├── pipeline_def.go     # NodeDef, EdgeDef, PipelineDef (the DAG)
│   │   ├── state.go            # PipelineState (a run)
│   │   ├── runner.go           # Runner, GitJob, GitJobResult
│   │   └── status.go           # Status enum (typed, not raw strings)
│   ├── engine/
│   │   ├── dag.go              # topological leveling of a PipelineDef
│   │   ├── pipeline.go         # level-by-level executor (parallel within a level, pauses on RequiresReview)
│   │   └── checkpoint.go       # persist/resume logic (completed-node sets)
│   ├── scheduler/
│   │   └── scheduler.go        # runs multiple pipeline runs concurrently across projects
│   ├── runnerhub/
│   │   └── hub.go              # server-side: tracks connected runners, dispatches GitJobs, awaits results
│   ├── store/
│   │   ├── store.go            # Store interface
│   │   └── sqlite.go           # SQLite implementation (WAL mode)
│   ├── agents/
│   │   ├── registry.go         # agent_type -> constructor
│   │   ├── gemini/coder.go
│   │   ├── claude/security.go
│   │   └── codex/reporter.go
│   ├── gateway/
│   │   ├── slack.go
│   │   ├── slack_verify.go     # signature verification middleware
│   │   └── servicenow.go
│   ├── web/
│   │   ├── routes.go           # UI routes (project list, pipeline editor, run status, review-gate editing)
│   │   └── api.go              # JSON endpoints the UI's JS calls (save DAG, list agent types, run/review actions)
│   ├── tools/
│   │   ├── git.go              # actual git command execution — used server-side (Phase 6) and by octopus-runner (Phase 7)
│   │   └── shell.go
│   └── config/
│       └── config.go
├── web/
│   ├── templates/               # Go html/template files
│   │   ├── projects.html
│   │   ├── pipeline_editor.html # the drag-and-drop canvas
│   │   └── run_status.html      # includes the review-gate editor view
│   └── static/
│       ├── js/
│       │   ├── sortable.min.js  # vendored, no CDN dependency
│       │   └── canvas.js        # drag/drop + edge-wiring logic, posts DAG JSON via htmx/fetch
│       └── css/
│           └── app.css
└── test/
    └── integration/
        ├── pipeline_test.go
        ├── scheduler_test.go
        └── runnerhub_test.go
```

Using `internal/` instead of `pkg/` — nothing here is meant to be imported by other modules, and `internal/` enforces that at compile time. `web/` (templates + static assets) sits at the repo root since it's not Go code, but is only ever served by `internal/web`.

---

## 4. Core Interfaces (decide these before writing agents)

```go
// domain/agent.go
type Agent interface {
    Name() string
    Execute(ctx context.Context, state *PipelineState) error
}

// domain/project.go
type Project struct {
    ID          string
    Name        string
    GitRemoteURL string
    BaseBranch  string
    Owner       string // nullable/empty for now; forward-compat for multi-person later
}

// domain/pipeline_def.go — the DAG a user builds in the web UI
type NodeDef struct {
    ID             string
    AgentType      string         // looked up in the agent registry
    Config         map[string]any
    RequiresReview bool           // if true, engine pauses after this node until a human continues it
}

type EdgeDef struct {
    From string // NodeDef.ID
    To   string // NodeDef.ID
}

type PipelineDef struct {
    ID        string
    ProjectID string
    Name      string
    Nodes     []NodeDef
    Edges     []EdgeDef
}

// domain/state.go
type PipelineState struct {
    RunID         string
    ProjectID     string
    PipelineDefID string
    TicketID      string
    GitBranch     string         // set once before any node runs; every git job is scoped to this
    Status        Status
    PendingNodeID string         // set when Status == StatusAwaitingReview
    ActionToken   string         // one-time token for approve/edit-continue/reject
    NodeOutputs   map[string]any // per-node outputs; a human can edit these during a review pause
    Summary       string
}

// domain/status.go
type Status string

const (
    StatusPending        Status = "PENDING"
    StatusRunning        Status = "RUNNING"
    StatusBlocked        Status = "BLOCKED"           // security node halted it
    StatusAwaitingRunner Status = "AWAITING_RUNNER"    // a git job is queued, no connected runner for this project yet
    StatusAwaitingReview Status = "AWAITING_REVIEW"    // paused at a node flagged RequiresReview
    StatusRejected       Status = "REJECTED"           // a human rejected at a review gate
    StatusCompleted      Status = "COMPLETED"
    StatusFailed         Status = "FAILED"
    StatusCancelled      Status = "CANCELLED"
)

// domain/runner.go
type Runner struct {
    ID         string
    Name       string    // hostname or user-given label
    TokenHash  string    // shared secret, hashed at rest, generated from the web UI
    ProjectIDs []string  // which projects this runner can do git work for
    LastSeen   time.Time
}

type GitJob struct {
    ID        string
    RunID     string
    ProjectID string
    Type      string // "prepare_branch" | "diff" | "apply_patch" | "commit" | "push" | "merge" | "shell_exec"
    Payload   map[string]any
}

type GitJobResult struct {
    JobID   string
    Success bool
    Output  string // full stdout/stderr — persisted centrally so logs are visible regardless of which machine ran it
    Error   string
}

// agents/registry.go
type Registry interface {
    Register(agentType string, factory func(cfg map[string]any) (domain.Agent, error))
    Create(agentType string, cfg map[string]any) (domain.Agent, error)
    ListTypes() []string // powers the web UI's drag-and-drop palette
}

// store/store.go
type Store interface {
    SaveProject(ctx context.Context, p *domain.Project) error
    LoadProject(ctx context.Context, id string) (*domain.Project, error)
    ListProjects(ctx context.Context) ([]*domain.Project, error)

    SavePipelineDef(ctx context.Context, d *domain.PipelineDef) error
    LoadPipelineDef(ctx context.Context, id string) (*domain.PipelineDef, error)
    ListPipelineDefs(ctx context.Context, projectID string) ([]*domain.PipelineDef, error)

    Save(ctx context.Context, state *domain.PipelineState) error
    Load(ctx context.Context, runID string) (*domain.PipelineState, error)
    ListActiveRuns(ctx context.Context) ([]*domain.PipelineState, error) // across all projects, for the scheduler + UI

    SaveCheckpoint(ctx context.Context, runID, completedNodeID string) error // appends to the completed set
    LoadCheckpoint(ctx context.Context, runID string) (completedNodeIDs []string, err error)

    ResolveReview(ctx context.Context, runID, actionToken string, approve bool, editedOutputs map[string]any) error

    SaveRunner(ctx context.Context, r *domain.Runner) error
    ListRunners(ctx context.Context) ([]*domain.Runner, error)
    TouchRunnerHeartbeat(ctx context.Context, runnerID string) error

    SaveGitJob(ctx context.Context, job *domain.GitJob) error               // called before dispatch, so it survives a server restart
    LoadPendingGitJobs(ctx context.Context) ([]*domain.GitJob, error)       // reloaded by runnerhub on boot
    ResolveGitJob(ctx context.Context, jobID string, result *domain.GitJobResult) error // idempotent by jobID
}
```

---

## 5. Phased Build Plan

### Phase 0 — Skeleton
- `go mod init`, directory layout above, `main.go` boots an HTTP server on `:8080` with `/healthz` and one server-rendered "hello" page via `html/template` (proves the web-serving path works end to end before real UI is built).
- `config.go` loads `config.yaml`, validates required keys, fails fast if missing.
- **Done when:** `go run ./cmd/octopus` boots, `/healthz` returns 200, and the hello page renders.

### Phase 1 — DAG engine + fake agents + review-gate pause/resume
- `PipelineDef` (nodes/edges), `dag.go` computes topological levels, `pipeline.go` executes a level's nodes concurrently via `errgroup`, then advances.
- `EchoAgent`(s) that just mutate `state.NodeOutputs[node.ID]`.
- Engine halts after a node flagged `RequiresReview`: sets `AWAITING_REVIEW` + `PendingNodeID`, stops advancing until an explicit `Continue(runID, actionToken, approve, editedOutputs)` call.
- Unit test 1: a diamond DAG (A → B, A → C, B+C → D). Assert B and C both run after A and before D, and that they overlap in time (artificial sleep + timestamps) to prove real parallelism, not accidental sequencing.
- Unit test 2: mark B `RequiresReview`. Assert the engine halts after B (C and D do not run), then `Continue` with an edited output for B causes C to see the edited value.
- **Done when:** DAG engine tests pass, including parallel-fanout and review-pause/edit/continue, zero network calls.

### Phase 2 — Store: projects, pipeline defs, checkpointing, review state
- SQLite (WAL mode). `Project` and `PipelineDef` CRUD.
- Checkpoint records a set of completed node IDs per run (not a single pointer). `Resume(ctx, runID)` recomputes the DAG frontier from that set and re-runs only nodes not yet complete — including resuming correctly from an `AWAITING_REVIEW` state after a restart.
- Integration test: run the 4-node diamond, kill after B completes but before C finishes, resume, assert C runs (not re-run if it already finished), B does not re-run, D waits for both then runs.
- **Done when:** a simulated crash mid-parallel-fanout, and a simulated crash while `AWAITING_REVIEW`, both resume correctly.

### Phase 3 — Scheduler: concurrent runs across projects
- `Scheduler` starts/tracks pipeline runs, each in its own goroutine, backed by the `Store`. `ListActiveRuns` powers a cross-project status view.
- Integration test: trigger runs for two different projects back-to-back (with an artificial delay in `EchoAgent`), assert both make progress concurrently rather than one blocking the other.
- **Done when:** two projects' pipelines run concurrently and both complete correctly.

### Phase 4 — Web UI (htmx + vanilla JS drag-and-drop)
- Pages: project list/create, pipeline editor (the canvas), run status/log view (polls or uses htmx SSE/polling for live updates), and a **review-gate view**: shows the pending node's output in an editable form with Approve / Edit & Continue / Reject actions.
- `/api/agent-types` lists registry contents for the drag-and-drop palette. A node's `RequiresReview` flag is a checkbox in the editor.
- `canvas.js` (vendored SortableJS + hand-rolled edge-drawing) lets you place nodes, connect them into a DAG (including a parallel branch), and save — POSTs a `PipelineDef` JSON to `internal/web/api.go`, persisted through the `Store`.
- A "Run" button on a project triggers the scheduler against that project's saved `PipelineDef`.
- **Done when:** you can open the UI, build a 3-node pipeline with one parallel branch and one review gate by dragging, save it, hit Run, watch it pause at the review gate, edit the output, continue, and see it finish.

### Phase 5 — Slack gateway (verification from the start)
- `slack_verify.go`: HMAC-SHA256 signature check, timestamp replay-window check (reject requests >5 min old).
- `/api/slack/command` → looks up the project + its default `PipelineDef`, creates a run via the scheduler, calls back with a review Block Kit card when the run reaches any `AWAITING_REVIEW` gate (not just a final one) — "Approve" resolves it inline, "Open in web UI" links out for edits.
- `/api/slack/action` → verifies signature, looks up `action_token`, marks it used, calls `ResolveReview`.
- **Done when:** a real Slack app (dev workspace) can trigger a run end to end, including a mid-pipeline review gate and a final approve, each working exactly once.

### Phase 6 — Real agents + local git tools (single machine)
- Swap `EchoAgent`s for `gemini.CoderAgent`, `claude.SecurityAgent`, `codex.ReporterAgent`, registered in the agent registry, each behind real API clients.
- Security agent can set `StatusBlocked`; engine short-circuits and notifies (Slack and/or UI) with the block reason instead of a review card.
- `internal/tools/git.go` implements `prepare_branch`, `diff`, `apply_patch`, `commit`, `push`, `merge` against a real local clone. The engine calls these **directly** (server executes its own git jobs in this phase — no runner dispatch yet, to isolate agent-logic bugs from networking bugs). Every job type fetches latest before acting and pushes after mutating (even on a single machine, to keep the behavior identical once Phase 7 adds real handoff between machines); `prepare_branch` pushes the new branch immediately rather than leaving it local-only.
- Every run's first step is the implicit `prepare_branch` job; `PipelineState.GitBranch` is set from it and all later git tool calls use it. The working clone lives at `clone_cache_dir/<project_id>/<run_id>` — scoped per run, not per project, so concurrent runs never share a checkout.
- **Done when:** a real ticket ID, built via a UI-designed pipeline or Slack command, produces a real branch (auto-created), a real diff, a real security note, and a real merge on approval — all on one machine.

### Phase 7 — Runner protocol (multi-machine dispatch)
- `cmd/octopus-runner/main.go`: reads `runner.example.yaml`-style config (server URL, runner token, which project IDs it serves), opens a persistent outbound connection (WebSocket) to the server, and executes dispatched `GitJob`s using the *same* `internal/tools/git.go` code from Phase 6.
- `internal/runnerhub/hub.go` (server-side): tracks connected runners and their declared projects, routes each project's `GitJob`s to any available connected runner for that project, awaits `GitJobResult`. Every dispatched job is written via `SaveGitJob` first; on boot, `hub.go` calls `LoadPendingGitJobs` and rebuilds its outstanding-job table so a server restart doesn't orphan a job a runner is still working on.
- If no runner is connected for a project when a job is ready, the run's status becomes `AWAITING_RUNNER` and the job stays queued — dispatched automatically the moment a matching runner connects.
- `GitJobResult.Output` (full stdout/stderr) is persisted on the run via `ResolveGitJob`, visible in the web UI/Slack exactly like Phase 6's local output. `ResolveGitJob` is idempotent by `jobID`, so a result redelivered after a server restart or a runner retry is a safe no-op.
- Runner tokens are generated/revoked from the web UI (a simple "Runners" admin page), hashed at rest, checked on connect.
- `localqueue.go`: every received job is written to the runner's local SQLite before execution starts; every result is written locally before the runner attempts to send it. Reconnect logic flushes any locally-queued unsent results first, then resumes normal dispatch.
- `push` jobs specifically retry with backoff inside the runner (not surfaced as a failure to the server) until they succeed or a long timeout is hit.
- Any runner registered for a project can pick up any job for that project's runs — a run's jobs are not pinned to whichever runner handled its first step. This only works because every job fetches before acting and pushes after mutating (Key Design Decision 20); test this explicitly, not just assume it.
- Integration test 1: start a fake `octopus-runner` in-process against a local git repo, connect it to the hub, trigger a run needing git work, assert the job round-trips and output is persisted; disconnect the runner mid-run and assert status flips to `AWAITING_RUNNER` instead of failing, then reconnect and assert it resumes.
- Integration test 2 (offline resilience): give the runner a job, sever its connection to the hub *while the job is executing*, assert it still completes the git work locally and the result lands in `localqueue.go`; restore the connection and assert the result flushes to the server exactly once (kill and restart the runner process mid-disconnect too, to prove the queue survives a restart, not just a network blip).
- Integration test 3 (cross-machine handoff): run two fake runners against the hub, both registered for the same project. Assert `prepare_branch` on runner A is visible (via `git fetch`) to runner B when the hub dispatches the next job to B instead of A — proves handoff doesn't depend on sticking to one machine.
- Integration test 4 (server restart mid-job): dispatch a job, kill and restart the server process before the result arrives, assert `LoadPendingGitJobs` recovers it and the eventually-delivered `GitJobResult` still resolves the run correctly.
- **Done when:** git tool calls work identically whether the server executes them directly (Phase 6 mode) or dispatches them to a real `octopus-runner` process running on a different machine; a runner that goes offline mid-job never loses or duplicates work; and neither a runner restart nor a server restart orphans an in-flight job.

### Phase 8 — Hardening / polish
- Structured logging (`slog`) with run ID and project ID on every line.
- Rate limiting on `/api/slack/command` and the web UI's run-trigger endpoint (per-user/per-project).
- **Explicit note:** the web UI has no auth in v1 — document that it must sit behind a VPN/localhost/reverse-proxy with its own auth before any shared/remote deployment. Runner tokens are the only access control on the git-execution path — treat them like credentials (rotate/revoke from the web UI if a machine is decommissioned). Running Octopus as the Home Assistant add-on (`ha-addon/`) and accessing it via HA **Ingress** satisfies this network-boundary requirement for free, since Ingress puts HA's own login in front of it — the raw `8080/tcp` port mapping still has none, so prefer Ingress when running that way.
- Dockerfile + compose file finalized, no socket mount (the server doesn't need Docker at all now that git work happens on runners).
- README with setup instructions, Slack app manifest, required scopes, and `octopus-runner` install/registration steps.
- Revisit the `Project.Owner` field — still just a label in v1, but this is where real multi-person access control would hang later if needed.

### Phase 9 (optional) — Discord gateway, ServiceNow gateway
- Mirror the Slack gateway pattern; only build if actually needed.

---

## 6. Config Shape

```yaml
# config.example.yaml — the central server
port: 8080
agents:
  gemini_api_key: ${GEMINI_API_KEY}
  anthropic_api_key: ${ANTHROPIC_API_KEY}
  openai_api_key: ${OPENAI_API_KEY}
slack:
  bot_token: ${SLACK_BOT_TOKEN}
  signing_secret: ${SLACK_SIGNING_SECRET}
store:
  driver: sqlite
  dsn: ./data/octopus.db
git:
  branch_pattern: "octopus/{ticket_id}"   # used for the implicit per-run branch
web:
  enabled: true
  # no auth config in v1 — see Phase 8 hardening note
```

```yaml
# runner.example.yaml — installed on each dev machine, used by cmd/octopus-runner
server_url: https://octopus.internal:8080
runner_token: ${OCTOPUS_RUNNER_TOKEN}   # generated from the web UI's Runners page
project_ids:
  - proj_abc123
clone_cache_dir: ~/.octopus/clones      # runner-local; not synced anywhere. Actual checkouts live at clone_cache_dir/<project_id>/<run_id>
```

Note: `git.repo_path` from the original draft is gone — repo location is runner-local (`clone_cache_dir`), scoped per `(project_id, run_id)` so concurrent runs never share a checkout, and `Project.GitRemoteURL` is what's shared centrally. Any runner registered for a project can pick up any job for that project's runs, not just the one that handled its first step — every job fetches before acting and pushes after mutating, so GitHub (not a particular runner's disk) is always the source of truth.

---

## 7. Testing Strategy
- Unit tests for DAG leveling and parallel execution (fake agents, no network) — including the diamond-DAG concurrency assertion.
- Unit tests for the review-gate pause/edit/continue path.
- Unit tests for Slack signature verification (known-good and tampered payloads).
- Integration test for checkpoint/resume mid-parallel-fanout and mid-review-pause using SQLite in a temp file.
- Integration test for the scheduler running 2+ projects concurrently.
- Integration test for the runner protocol: job round-trip, output persistence, `AWAITING_RUNNER` on disconnect, resume on reconnect.
- Manual checklist for the web UI (drag-and-drop build, save, run, review-gate edit) and for Phase 5/6/7 (real Slack workspace, real repo, a second physical/VM machine running `octopus-runner`) — documented as a runbook in `README.md`.

---

## 8. How to Hand This to Claude Code

Suggested first prompt once this file is in the repo root:

> Read PLAN.md. Implement Phase 0 and Phase 1 only. Stop after Phase 1's tests pass (including the parallel-fanout DAG test and the review-gate pause/edit/continue test) and show me the diff before continuing.

Working phase-by-phase with an explicit stop keeps each step reviewable instead of getting a 2000-line first commit. Multi-machine dispatch (Phase 7) is deliberately late — Phases 0–6 are all provable on a single machine first, so agent-logic bugs and networking/runner bugs never get debugged at the same time.
