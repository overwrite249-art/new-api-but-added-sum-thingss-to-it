# Deploying New API on Vercel (serverless)

This describes the serverless port added alongside the normal deployment. It
does **not** replace `main.go` — Docker and bare-metal deployments are
untouched and keep working exactly as before.

> **Status: deployed and building green.** The Go function boots, initialises,
> routes, and returns real responses. The remaining unknowns are narrow and
> listed under [Known risks](#known-risks). The single biggest one — whether the
> Go runtime streams or buffers responses — is still open and matters a lot if
> you rely on SSE.

---

## Why the app could not simply be deployed as-is

New API is a **daemon**. `main.go` ends on `sig := <-quit`, and before that it
starts roughly a dozen background goroutines: `SyncChannelCache`, `SyncOptions`,
`StartPolicySync`, `UpdateQuotaData`, `AutomaticallyUpdateChannels`,
`StartSystemTaskRunner`, `StartSubscriptionQuotaResetTask`,
`StartCodexCredentialAutoRefreshTask`, `StartSystemInstanceReporter`,
`InitBatchUpdater`, `StartSystemMonitor`, `StartAuthArtifactCleanup`, pprof.

A serverless function has the opposite execution model: code runs **only while a
request is in flight**, the instance is frozen or discarded afterwards, and
there is no shutdown hook. A `for range ticker.C` loop started during a request
never receives a second tick.

So the port is not "make it deployable" — it is a change of execution model.

### The finding that made this cheap

Upstream **already ships a no-background-work mode**. All three task daemons
open with the same guard:

```go
func StartSystemTaskRunner() {
    systemTaskRunnerOnce.Do(func() {
        if !common.IsMasterNode {
            return
        }
        ...
```

So `NODE_TYPE=slave` disables them with **zero changes to existing Go files**.

The same flag also solves the embedded-frontend problem. `router.SetRouter`:

```go
if common.IsMasterNode && frontendBaseUrl != "" {
    frontendBaseUrl = ""   // FRONTEND_BASE_URL is ignored on master
}
if frontendBaseUrl == "" {
    SetWebRouter(router, assets)   // the only consumer of embedded web/dist
} else {
    router.NoRoute(redirect)
}
```

`SetWebRouter` calls `common.EmbedFolder(assets.BuildFS, "web/dist")`, which
would fail on a zero-valued `embed.FS`. Running as **slave with a non-empty
`FRONTEND_BASE_URL`** takes the other branch, so the Go function never touches
the embed and does not need the frontend compiled into it.

One environment variable, both problems. The runtime now sets it itself.

---

## What was added

Only `CLAUDE.md` among pre-existing files was touched. `main.go`, `Dockerfile`
and `docker-compose.yml` are untouched, so this port is fully reversible and
still merges cleanly with upstream.

| File | Purpose |
|---|---|
| `serverless/bootstrap.go` | Lazy per-instance init + Gin engine, mirroring `InitResources()`/`main()` minus every goroutine. Also `Migrate()`. |
| `service/serverless_tasks.go` | Synchronous single-pass equivalents of the task daemons. In `package service` so it can reach unexported internals. |
| `model/serverless_migrate.go` | Re-exports the unexported `migrateDB`/`migrateLOGDB` so migration can be triggered over HTTP. |
| `api/index.go` | Vercel Function: the whole HTTP surface. |
| `api/cron/index.go` | Vercel Cron: authenticated one-shot job runner, plus `?job=migrate`. |
| `vercel.json` | Build, route map, function limits, cron schedule. |
| `.env.vercel.example` | Serverless env template with corrected defaults. |

### Goroutine triage

| Background goroutine | Fate | Reasoning |
|---|---|---|
| `StartSystemTaskRunner` | **Cron** | Already `IsMasterNode`-gated. Its DB-lease design (unique active key + per-type lock) is exactly what makes it safe to drive from stateless invocations. |
| `StartSubscriptionQuotaResetTask` | **Cron** | Same gate; `runSubscriptionQuotaResetOnce()` was already a single pass. |
| `StartCodexCredentialAutoRefreshTask` | **Cron** | Same gate; already a single pass. |
| `SyncChannelCache`, `SyncOptions`, `StartPolicySync` | **Dropped** | They refresh in-process caches. A frozen instance cannot refresh anything, and a cold start re-reads from the DB anyway. |
| `UpdateQuotaData`, `InitBatchUpdater` | **Dropped + disabled** | In-memory buffers flushed on a timer. Frozen timer + discarded instance = lost billing data. Disable via `BATCH_UPDATE_ENABLED=false`, `DATA_EXPORT_ENABLED=false`. |
| `StartSystemMonitor`, `StartAuthArtifactCleanup`, pprof, pyroscope | **Dropped** | Process-lifetime concerns with no meaning per-request. |
| `StartSystemInstanceReporter` | **Dropped** | Reports a long-lived node that does not exist. |

One deliberate behavioural difference: `runSystemTaskClaimPass` dispatches
handlers through `gopool.Go`. `runSystemTaskClaimPassSync` runs them **inline**,
because a function that returns while a handler is still running gets frozen
mid-task, leaving the row `RUNNING` until its lease expires.

---

## Deploying

### 1. Provision a database

Managed Postgres or MySQL. **SQLite cannot work** — it is a local file, the
bundle is read-only, and `/tmp` is per-instance and discarded on recycle, so
each concurrent instance would see a different empty database.

Prefer Postgres. The MySQL path calls `checkMySQLChineseSupport`, which
**panics** unless the charset is one of `utf8mb4`, `utf8`, `gbk`, `big5`,
`gb18030`.

Use a **transaction-mode connection pooler** (Neon's `-pooler` host, PgBouncer,
Supabase port `6543`). This is safe here because `chooseDB` opens Postgres with
`PreferSimpleProtocol: true`, which disables the implicit prepared statements
that transaction pooling cannot support.

Put the database in the **same region as the functions** (`iad1` pairs with
`aws-us-east-1`). Every query crosses the network, so cross-region latency is
paid on every request.

### 2. Set environment variables

Only three actually matter:

| Variable | Why |
|---|---|
| **`SQL_DSN`** | The one value that cannot be defaulted. Without it the app refuses to boot, with an explanation. |
| **`CRON_SECRET`** | `/api/cron` fails **closed**. Unset means every cron and migration request is rejected. |
| **`SESSION_SECRET`** | Upstream generates a random one per process if unset, so each instance signs cookies with a different key and logins break at random. Must be pinned. Never the literal `random_string` — `common.InitEnv` calls `log.Fatal` on that exact value. |

Strongly recommended, from `.env.vercel.example`:

- **`SQL_MAX_OPEN_CONNS=3`**, `SQL_MAX_IDLE_CONNS=1`, `SQL_MAX_LIFETIME=30` —
  upstream defaults to **1000**, sized for one process. Here the pool is *per
  instance*, so the real ceiling is `1000 x concurrent instances`; a modest
  spike exhausts the database's connection limit.
- **`BATCH_UPDATE_ENABLED=false`**, **`DATA_EXPORT_ENABLED=false`** — otherwise
  buffered billing/usage data is silently lost when an instance is discarded.
- **`TRUSTED_PROXIES`** — requests always arrive via Vercel's edge, so without
  this every client collapses into one rate-limit bucket.
- **`SESSION_COOKIE_SECURE=true`** — deployments are always HTTPS.

`NODE_TYPE` and `FRONTEND_BASE_URL` are **not** required. `applyServerlessDefaults`
forces the first and derives the second from `VERCEL_PROJECT_PRODUCTION_URL` or
`VERCEL_URL`. Setting `FRONTEND_BASE_URL` explicitly still wins, which is what
you want behind a custom domain.

This was originally the other way round, and it was a mistake worth recording:
demanding values the runtime can determine for itself meant a fresh deploy
answered 500 to **every** request — including the `/api/setup` call the
first-run wizard makes — so the UI rendered an error popup per panel and could
never be configured. Fail-closed validation is right for secrets and wrong for
anything derivable.

### 3. Deploy

Push to the production branch, or `vercel deploy`. The frontend builds via
`cd web && npm install && npm run build` (rsbuild) into `web/dist`, served as
static files. The Go function handles the API.

### 4. Create the schema

```bash
curl -i "https://<deployment>/api/cron?job=migrate" \
     -H "Authorization: Bearer $CRON_SECRET"
```

Slave nodes skip migration on boot, so this step is not optional on a fresh
database — an unmigrated database fails at query time rather than at boot, which
looks like a mysterious runtime bug. Re-run it after any upgrade that adds
migrations; `AutoMigrate` only adds what is missing, so repeat calls are safe.

This deliberately does **not** happen automatically on a cold start. Thirty-odd
`AutoMigrate` statements would be charged to whichever user's request triggered
them, and two simultaneous cold starts would race on DDL.

### 5. Create the first admin account

Open `/setup`. `createRootAccountIfNeed()` is **not** called by `InitDB()`, so a
migrated-but-empty database is the correct starting state: `CheckSetup()` finds
no setup record and no root user, sets `constant.Setup = false`, and the app
serves the first-run wizard.

### 6. Verify, in this order

```bash
curl -i https://<deployment>/api/status                     # JSON, not HTML
curl -i https://<deployment>/v1/models -H "Authorization: Bearer <token>"
curl -i "https://<deployment>/api/cron?job=subscriptions" \
     -H "Authorization: Bearer $CRON_SECRET"                # expect success:true
```

---

## Platform facts worth knowing

Each of these cost a failed deploy or a crash to establish.

### `framework` must be `null`

Vercel detects the **Go Framework Preset** from a root `go.mod` + `main.go`,
compiles `main.go` into a single server binary, and never scans `api/`. The
resulting deploy fails in about a second and a half with:

```
unused_function: The pattern "api/index.go" defined in `functions`
doesn't match any Serverless Functions inside the `api` directory.
```

`"framework": null` selects the "Other" preset. The project's stored framework
field may still read `go`; the per-deployment `vercel.json` is what counts.

### Entrypoints are `package handler`, not `package main`

Vercel generates and imports a `func main()` wrapper at build time, so the file
must be a library package exporting an `http.HandlerFunc`. `package main` builds
locally and fails on the platform. Since these are library packages, plain
`go build ./...` works — no exclusion needed.

### The filesystem is read-only, and `LogDir` will kill you

`common.LogDir` is a **flag** with default `./logs`, not an environment
variable, so no amount of platform configuration can change it. `InitEnv()`
`os.Mkdir`s it and `log.Fatal`s on failure. The bundle is mounted read-only at
`/var/task`, so every cold start died with:

```
mkdir /var/task/logs: read-only file system
Child exited with error exit status 1
```

Because `log.Fatal` is `os.Exit(1)`, the process died before any handler could
reply — surfacing as an opaque `FUNCTION_INVOCATION_FAILED` with no body. The
fix is `*common.LogDir = ""` before `InitEnv()`. Note the diagnostic: the
*absence* of the port's own JSON error was the clue that the process was dying
rather than returning.

`common/disk_cache.go` needs no such fix — `GetDiskCacheDir()` already falls
back to `os.TempDir()`, and `/tmp` is writable.

### `/api/*` does not collide — confirmed

New API serves its dashboard API under `/api/...`; Vercel treats `api/` as its
functions directory. The filesystem is checked before rewrites, so:

- `/api/cron` reaches the real function `api/cron/index.go`
- `/api/status` falls through the `/api/:path*` rewrite to `api/index.go`

Both verified against a live deployment. If this ever regresses, move the
catch-all out of `/api` (e.g. `api/gateway/index.go`, reached only via rewrites)
so no real function path shares the prefix.

### Verified limits

| | Hobby | Pro |
|---|---|---|
| `maxDuration` | 300s default **and** max | 300 default, 800 max |
| Memory | 2 GB / 1 vCPU, fixed | up to 4 GB / 2 vCPU |
| Cron frequency | **once per day**, hour-level precision (±59 min) | once per minute |
| Bundle | 250 MB uncompressed (the 5 GB tier is Node/Python only, not Go) | same |

`go 1.25.1` builds fine — the Go version comes from the `go` directive in a
root `go.mod`.

---

## Known risks

Ordered by how likely they are to actually hurt.

### 1. Response streaming is unverified — the biggest open question

Whether Vercel's Go runtime streams incrementally or buffers the full response
is **not established**. If it buffers, SSE arrives all at once at the end and
streaming relay is unusable for chat clients. gzip is deliberately left off
(upstream disables it with the comment "This will cause SSE not to work!!!"), so
nothing in this port adds buffering, but the platform's own behaviour is the
open variable. **Test with a streaming completion before trusting this in
anger.**

### 2. Hobby cron cannot replace the pollers

`midjourneyPollHandler` and `asyncTaskPollHandler` declare
`Interval() = 15 * time.Second`. Hobby cron runs **once per day** with only
hour-level precision, and sub-daily expressions are rejected **at deploy time**:

```
Hobby accounts are limited to daily cron jobs.
This cron expression would run more than once per day.
```

There is no way to approximate 15-second polling on Hobby. Async image/video
task completion will simply not be detected until the daily run. Options:

- **Pro**, which allows once-per-minute — still 4x slower than upstream, but
  workable;
- drive `/api/cron` from any external scheduler (GitHub Actions, cron-job.org,
  a cheap VPS) with the same `Bearer` secret. The endpoint is deliberately
  platform-agnostic, and this is the cheapest real fix.

Polling is idempotent, so this is latency, never corruption.

### 3. WebSockets cannot work — no workaround

`/v1/realtime` is registered via `gorilla/websocket`. Serverless functions
cannot hold a bidirectional long-lived connection. OpenAI Realtime relaying is
**permanently unavailable** here; it needs a real server. This is the one
feature genuinely lost rather than degraded.

### 4. Long streams get truncated at 300s

Any SSE stream or relay exceeding `maxDuration` is killed mid-response and the
client sees a truncated stream rather than a New API error. `RELAY_TIMEOUT=280`
and `STREAMING_TIMEOUT=280` are set below the ceiling so New API fails first
with a proper error where it can.

### 5. Configuration staleness

With `SyncOptions`/`SyncChannelCache` dropped, a warm instance can serve stale
configuration until it is recycled. Configuring Redis narrows this window
considerably. Expect admin changes not to take effect instantly.

### 6. A malformed `SQL_DSN` dies opaquely

`model.InitDB` calls `common.FatalLog` when the connection cannot be opened,
which exits the process instead of returning an error. A *missing* DSN is caught
by `validateServerlessConfig` and reported as clean JSON; a *malformed* one
surfaces as `FUNCTION_INVOCATION_FAILED`. Check the runtime logs if a deploy
goes opaque right after a DSN change.

---

## Security notes

Tightened while porting, because serverless changes the threat model:

- **`/api/cron` fails closed.** Unset `CRON_SECRET` denies all requests instead
  of allowing them. The secret is compared with
  `crypto/subtle.ConstantTimeCompare` so it cannot be recovered through response
  timing. Without this the endpoint is an amplification vector: an anonymous
  caller could force channel tests against every upstream provider on demand —
  and, now, re-run DDL.
- **`NoRoute` redirect loop removed.** `SetRouter`'s redirect branch 301s all
  unmatched paths to `FRONTEND_BASE_URL`. On a single-domain deployment, where
  `FRONTEND_BASE_URL` *is* this origin, an unmatched `/api/...` path would
  redirect to itself forever. `serverless/bootstrap.go` re-registers `NoRoute`
  with a plain JSON 404.
- **Rate limiting depends on `TRUSTED_PROXIES`.** Without it, IP-based limits
  see only Vercel edge addresses and collapse into a single bucket.
- **Rate limiting without Redis is per-instance**, so effective limits are
  silently multiplied by the number of live instances. Redis is strongly
  recommended, not merely a performance option.
- **Never commit `SQL_DSN`.** `vercel.json` supports an `env` block, which is
  tempting and wrong: it lives in the repository. Set secrets in project
  settings.

No claim is made that the codebase is free of vulnerabilities. These are
deployment-model issues found while porting — **not** the result of a security
audit of the ~78 files in `service/` or ~88 in `controller/`.

---

## If serverless turns out to be the wrong fit

Given WebSockets being impossible, possible response buffering, Hobby cron
granularity, and per-instance connection pools, a long-running container is a
better match for this application. `Dockerfile` and `docker-compose.yml` already
work unchanged, and Railway / Fly.io / Render deploy them directly with none of
the caveats above.

There is also a middle path: Vercel's **Go Framework Preset** (`framework: "go"`,
root `main.go` listening on `$PORT`, gin explicitly supported) would preserve
the entire upstream routing tree with zero new code, and could serve the
embedded frontend by building `web/dist` before `go build` to satisfy the
`//go:embed`. The trade-off is that it needs `NODE_TYPE=master`, which re-enables
every background goroutine inside a runtime that freezes them — mitigable with
`BATCH_UPDATE_ENABLED=false` and `DATA_EXPORT_ENABLED=false`, but not cleanly.

This port exists because Vercel was the explicit requirement. It is the honest
maximum for that constraint, not a claim that the constraint is optimal.
