# CLAUDE.md — Project Conventions for new-api

@AGENTS.md

## Claude Code

- Follow the shared project instructions imported from `AGENTS.md`.

## Deployment targets

This repo builds for **two different execution models**. Know which one you are
editing before you touch anything.

### 1. Long-running server (canonical)

`main.go` at the repo root. A daemon: it ends on `sig := <-quit` and starts ~12
background goroutines. This is what `Dockerfile` and `docker-compose.yml`
deploy. **Unmodified by the serverless port** — do not change its behaviour to
suit serverless.

### 2. Serverless functions (Vercel)

See **`docs/VERCEL.md`** for the full guide. Files: `api/`, `serverless/`,
`service/serverless_tasks.go`, `vercel.json`, `.env.vercel.example`.

Rules that are easy to get wrong here:

- **Never add a background goroutine to a code path reachable from `api/`.**
  A function instance is frozen or discarded once the response is written, so
  `go f()` and `for range ticker.C` do not run. Periodic work belongs in
  `service/serverless_tasks.go` as a synchronous single pass, invoked by
  `api/cron/index.go`.
- **Never buffer state in process memory** and flush it later. Instances are
  stateless and short-lived, so buffered writes are lost outright — this is why
  `BATCH_UPDATE_ENABLED` and `DATA_EXPORT_ENABLED` must be off.
- **The serverless deployment runs as `NODE_TYPE=slave`.** This is load-bearing:
  it keeps the `IsMasterNode`-gated daemons off *and* stops `router.SetRouter`
  from calling `SetWebRouter`, which would need the embedded `web/dist`. It also
  means **schema migrations do not run** — they must be applied separately.
- **`go build ./...` fails on `./api/...`** by design (Vercel injects
  `func main()` at build time). Use
  `go build $(go list ./... | grep -v '/api')`.
- **WebSocket routes cannot work** on Vercel (`/v1/realtime`). Do not attempt to
  "fix" this in the serverless path; it needs a real server.

When adding a route, add it to `router/` as usual — `api/index.go` mounts the
whole Gin engine, so routes are picked up automatically. Only `vercel.json`
needs a new `rewrites` entry if you introduce a brand-new **top-level path
prefix**.
