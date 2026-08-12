# Octopus

Multi-agent AI pipeline orchestration server. See [`PLAN.md`](PLAN.md) for
the full design and phased build plan.

## Run locally

```sh
cp config.example.yaml config.yaml   # first time only
go run ./cmd/octopus
curl localhost:8080/healthz
```

## Run in Docker

```sh
cp config.example.yaml config.yaml   # first time only
docker compose up --build
```

## Run as a Home Assistant add-on

See [`ha-addon/DOCS.md`](ha-addon/DOCS.md).

## Test

```sh
go test ./... -race
```

## Status

Phase 0 (skeleton) and Phase 1 (DAG engine, parallel execution, review-gate
pause/resume) are implemented. Everything else in `PLAN.md` — persistence,
the scheduler, the web UI, Slack, real agents, and the multi-machine runner
protocol — is not built yet.
