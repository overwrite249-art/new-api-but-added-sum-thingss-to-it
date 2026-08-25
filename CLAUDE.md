# CLAUDE.md — Project Conventions for new-api

@AGENTS.md

## Claude Code

- Follow the shared project instructions imported from `AGENTS.md`.
- Before editing anything under `api/`, `serverless/`, or `vercel.json`, read
  **`docs/VERCEL.md`**. That file is the authoritative record of what was
  actually measured against a live deployment; this file is the short version
  plus the rules that are easy to get wrong.
- `VERCEL_MIGRATION_ASSESSMENT.md` is the original pre-implementation survey.
  It is kept for history and is **stale in places** — where the two disagree,
  `docs/VERCEL.md` wins.

## Deployment targets

This repo builds for **two different execution models**. Know which one you are
editing before you touch anything.

### 1. Long-running server (canonical)

`main.go` at the repo root. A daemon: it ends on `sig := <-quit` and starts ~12
background goroutines. This is what `Dockerfile` and `docker-compose.yml`
deploy. **Unmodified by the serverless port** — do not change its behaviour to
suit serverless.

### 2. Serverless functions (Vercel)

Two functions, a static frontend, one cron entry. The port is deliberately
**additive**: it adds new files and does not rewrite the existing Go packages.
That is the whole reason the fork still merges cleanly with upstream. Do not
"port" existing files unless there is no other way.

| Path | Role |
|---|---|
| `api/index.go` | HTTP entrypoint. Hands every request to the shared Gin engine. |
| `api/cron/index.go` | Cron + manual job entrypoint. Also hosts `?job=migrate`. |
| `serverless/bootstrap.go` | Lazy, mutex-guarded init of env, DB, caches, engine. |
| `service/serverless_tasks.go` | Synchronous single-pass versions of the daemon loops. |
| `model/serverless_migrate.go` | Re-exports `migrateDB`/`migrateLOGDB` as `RunMigrations()`. |
| `vercel.json` | Build command, function config, rewrites, crons. |
| `.env.vercel.example` | The env vars that matter, and the ones not to set. |
| `docs/VERCEL.md` | Full guide: measured limits, failure modes, rationale. |

## Hard rules for the serverless path

### Files under `api/` must be `package handler`

Not `package main`. The Vercel Go runtime compiles each `api/**/*.go` into its
own function and **injects `func main()` itself**; a `package main` with no
`main` fails to build, and one with a `main` collides. Each file must export
exactly one `func(http.ResponseWriter, *http.Request)`.

Consequence: `api/` holds ordinary library packages, so **`go build ./...`
from the repo root works and must stay green.** (An earlier revision of this
file claimed the build had to exclude `./api/...` via
`go build $(go list ./... | grep -v '/api')`. That was wrong and is obsolete.)

### Never add a background goroutine to a path reachable from `api/`

An instance is frozen or discarded once the response is written, so `go f()`
and `for range ticker.C` do not run. Periodic work belongs in
`service/serverless_tasks.go` as a synchronous single pass, invoked by
`api/cron/index.go`. When you need to reach an unexported daemon body, add a
new file **inside that package** that exports a synchronous version — that is
the pattern used by both `service/serverless_tasks.go` and
`model/serverless_migrate.go`. Do not export the original or restructure it.

### Never buffer state in process memory

Instances are stateless and short-lived, so buffered writes are lost outright.
This is why `BATCH_UPDATE_ENABLED` and `DATA_EXPORT_ENABLED` must be off, and
why anything that accumulates in a package-level var and flushes on a timer is
unusable here.

### The filesystem is read-only except `/tmp`

`/var/task` is read-only. Any `os.Mkdir`, `os.Create`, or `os.OpenFile` on a
relative path kills the process, and because it happens during init the
response is an opaque `FUNCTION_INVOCATION_FAILED` with no JSON body.

The live example: `common/init.go` declares
`LogDir = flag.String("log-dir", "./logs", ...)` and `InitEnv()` calls
`os.Mkdir(*LogDir, 0777)` under a `log.Fatal`. On Vercel that produced:

```
mkdir /var/task/logs: read-only file system
Child exited with error exit status 1
```

`serverless/bootstrap.go` therefore sets `*common.LogDir = ""` **before**
calling `common.InitEnv()`. Do not remove that line. Note there is no `LOG_DIR`
environment variable — it is a flag, so it can only be neutralised in code.
Stdout logging still works and is what Vercel captures.

If you need scratch space, use `common.GetDiskCacheDir()`, which already falls
back to `os.TempDir()`.

### `NODE_TYPE=slave` is load-bearing — and the runtime sets it itself

`applyServerlessDefaults()` forces it. It keeps the `IsMasterNode`-gated
daemons off *and* stops `router.SetRouter` from calling `SetWebRouter`, which
would dereference a zero-value `embed.FS`. For the same reason
`FRONTEND_BASE_URL` must be non-empty; bootstrap derives it from
`VERCEL_PROJECT_PRODUCTION_URL`, falling back to `VERCEL_URL`.

**Do not require either variable from the user, and do not fail closed when
they are absent.** An earlier revision did exactly that, and because the check
ran before anything else, *every* request 500'd — including `/api/setup` and
`/api/status`, so the frontend rendered one error popup per panel. Anything the
runtime can determine for itself, it should determine for itself.

### Schema migrations are explicit, and never on a request path

Because the deployment runs as a slave node, `model.InitDB()` returns before
`migrateDB()`. Migrations are run on demand instead:

```
GET /api/cron?job=migrate
Authorization: Bearer $CRON_SECRET
```

That calls `serverless.Migrate()` → `model.RunMigrations()` →
`migrateDB()` + `migrateLOGDB()`. It is safe to repeat; GORM `AutoMigrate` only
adds what is missing.

Three things to preserve here:

- **`Migrate()` must not call `EnsureResources()`.** On an empty database,
  `model.CheckSetup()` and `model.InitOptionMap()` query tables that do not
  exist yet, so full init cannot be a precondition for migrating.
- **Do not move migration onto a normal request.** A cold start would otherwise
  run 30+ `AutoMigrate` statements inside a user's request, and two concurrent
  cold starts would race on DDL.
- **A migrated-but-empty database is the correct end state.**
  `createRootAccountIfNeed()` is *not* called by `InitDB()`, so `CheckSetup()`
  sets `constant.Setup = false` and the app serves the `/setup` wizard. Seeing
  `/setup` after migrating is success, not a bug.

Any earlier instruction to run migrations with `docker run ... calciumion/new-api`
is obsolete. Delete it if you find it.

### WebSocket routes cannot work

`/v1/realtime` uses `gorilla/websocket`, which needs a connection the platform
will not give a function. Do not attempt to "fix" this in the serverless path;
it needs a real server. Leave the route registered so it fails honestly.

### Never commit a secret to `vercel.json`

`vercel.json` does support an `env` block, and it would work. **This repository
is public.** `SQL_DSN` contains the database password, so putting it there
leaks the database. Environment variables belong in project settings only.
Also remember env changes do **not** apply to existing deployments — a redeploy
is required.

### `"framework": null` in `vercel.json` is required

With the Go framework preset auto-detected, the build fails before compiling:

```
unused_function
The pattern "api/index.go" defined in `functions` doesn't match any
Serverless Functions inside the `api` directory.
```

The preset expects a root `main.go` listening on `$PORT` and ignores `api/`.
The stored project metadata may still report `framework: "go"`; that is
harmless, the per-deployment `vercel.json` wins.

### Do not re-enable gzip in the serverless engine

`buildEngine()` leaves it off on purpose, matching upstream's comment: it
breaks SSE. The engine also re-registers `NoRoute` with a JSON 404 after
`router.SetRouter`, to defeat the 301-to-`FRONTEND_BASE_URL` redirect loop.
Both are deliberate.

### Adding routes

Add them to `router/` as usual — `api/index.go` mounts the whole Gin engine, so
routes are picked up automatically. Only `vercel.json` needs a new `rewrites`
entry if you introduce a brand-new **top-level path prefix**. When you do,
check `router/relay-router.go` for parameterised prefixes: `/:mode/mj` means
`/fast/mj/...` and `/relax/mj/...` need their own rewrite or they silently 404.

Routing precedence is settled: a real file in `api/` beats a `vercel.json`
rewrite. `/api/cron` reaches the cron function, while `/api/status` falls
through the `/api/:path*` rewrite into `api/index.go`.

## Initialization contract (`serverless/bootstrap.go`)

Three exported entrypoints, all under one `sync.Mutex`:

- `Engine()` — returns the built Gin engine.
- `EnsureResources()` — full init: env, ratio settings, HTTP client, token
  encoders, DB, authz, setup check, options, log DB, Redis, perf metrics, i18n,
  OAuth providers, channel cache, pricing, scheduled task registration.
- `Migrate()` — env + config validation + DB open + `RunMigrations()`. Nothing
  else.

Guard flags are plain `bool`s (`envDone`, `resourcesDone`), **not `sync.Once`**,
so a transient database failure stays retryable on the next invocation. Keep it
that way.

`initEnvOnce()` has its own flag because `common.InitEnv()` calls
`flag.Parse()` and contains several `log.Fatal` branches. Everything that must
happen before `InitEnv()` — notably `*common.LogDir = ""` and
`applyServerlessDefaults()` — lives there.

`initResources()` wraps `InitDB`/`InitLogDB` in `== nil` checks so it composes
correctly after a prior `Migrate()`.

Deliberately **not** called: `godotenv.Load`, `MigrateRetiredFrontendOptions`,
`CleanupOldCacheFiles`, `StartSystemMonitor`, `StartAuthArtifactCleanup`,
`StartPyroScope`, pprof, `model.SyncChannelCache`, and every `Start*` daemon.

## Failure-mode cheat sheet

The **shape** of a failure identifies its layer before you read any logs.

| Symptom | Layer | Meaning |
|---|---|---|
| `FUNCTION_INVOCATION_FAILED`, no JSON | process died during init | a `log.Fatal` / `panic` / `FatalLog` — read runtime logs |
| JSON `{"error":{...,"type":"new_api_init_failed"}}` | init returned an error | config or DB problem, message is actionable |
| JSON `type: new_api_panic` | handler panicked | application bug, request-scoped |
| JSON `type: new_api_not_found` | routing | engine reached, no route matched |
| `unused_function` at build | `vercel.json` | framework preset is interfering |
| 504 `FUNCTION_INVOCATION_TIMEOUT` | `maxDuration` | request exceeded 300 s |

A **malformed** `SQL_DSN` exits the process (`model.InitDB` calls
`common.FatalLog` when the connection cannot be opened), while a **missing**
one returns clean JSON. So an opaque crash right after an env change usually
means a typo in the DSN, not a code bug.

## Cron: what actually runs

`api/cron/index.go` is fail-closed on `CRON_SECRET`
(`subtle.ConstantTimeCompare`). No secret set means every call is 401.

- Scheduled, run by `allJobs` when no `?job=` is given: `system-tasks`,
  `subscriptions`, `codex-credentials`.
- Manual only: `migrate`. It is dispatched *before* `EnsureResources()` and,
  unlike the periodic jobs, reports failures as a 500 with the error text.

**Cadence is the port's biggest functional compromise.** On a Hobby plan a cron
can run **once per day**, scheduled to the hour with up to 59 minutes of drift.
Sub-daily expressions are rejected at deploy time:

```
Hobby accounts are limited to daily cron jobs.
This cron expression would run more than once per day.
```

Upstream polls Midjourney and async tasks every **15 seconds**
(`controller/system_task_handlers.go`). A daily cron cannot approximate that.
If async task polling matters, either move to Pro (once per minute) or drive
`/api/cron` from an external scheduler with the same bearer secret. Do not
"solve" it by adding a goroutine or a self-ping.

## Database notes

- PostgreSQL is opened with `postgres.Config{PreferSimpleProtocol: true}`, which
  disables implicit prepared statements. That is precisely the incompatibility
  that breaks pgbouncer transaction pooling, so a **pooled** connection string
  is safe here — and necessary, since every instance keeps its own pool.
- Keep the pool tiny: `SQL_MAX_OPEN_CONNS=3`, `SQL_MAX_IDLE_CONNS=1`,
  `SQL_MAX_LIFETIME=30`. The upstream defaults (1000 / 100 / 60) assume one
  long-lived process.
- Co-locate the database with the function region.
- MySQL: `checkMySQLChineseSupport` **panics** unless the charset is one of
  `utf8mb4`, `utf8`, `gbk`, `big5`, `gb18030`.
- ClickHouse is valid for `LOG_SQL_DSN` only; `chooseDB` rejects it as the main
  database.
- SQLite is not an option. Slave mode never creates tables and `/tmp` is
  per-instance, so an admin account created in `/setup` would silently vanish.
  Do not add a `SQLITE_PATH=/tmp/...` default to make the UI boot — it produces
  a demo that looks like it works and loses data.

## Build and toolchain

- The Go version comes from the `go` directive in the root `go.mod`
  (`go 1.25.1`, confirmed working). A `toolchain` directive would override it.
- Frontend build is **rsbuild**, not vite: `cd web && npm install && npm run build`,
  output `web/dist`. Use `npm install`, not `npm ci` — there are peer-dependency
  overrides (`@emoji-mart/react` wants React 16–18, the project resolves 19).
  Those warnings are benign.
- Function bundle limit is 250 MB uncompressed. The larger tier is not
  available to the Go runtime.
- `maxDuration: 300` is legal on Hobby (300 s is both the default and the
  maximum there). Memory is fixed at 2 GB / 1 vCPU.

## Open risks

- **Streaming is unverified.** Whether the Go runtime streams or buffers the
  response body is the single biggest remaining functional unknown. If it
  buffers, SSE relay is unusable no matter what the code does. Test with a real
  streaming completion.
- **No security audit has been done.** Only the deployment model was reviewed.
  `service/`, `controller/`, `relay/`, `model/`, and `middleware/` have not been
  audited. Do not describe this fork as hardened.

## Alternative architecture (recorded, not taken)

The **Go framework preset** — `framework: "go"`, a root `main.go` listening on
`$PORT`, gin supported — would preserve the entire upstream routing tree with
zero new entrypoints, and could serve the embedded frontend by building
`web/dist` before `go build`. The trade-off is that it wants `NODE_TYPE=master`,
which re-enables the background goroutines that do not run. Rationale is in
`docs/VERCEL.md`.
