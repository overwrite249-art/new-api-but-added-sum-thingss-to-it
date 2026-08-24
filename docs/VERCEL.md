# Deploying New API on Vercel (serverless)

This describes the serverless port added alongside the normal deployment. It
does **not** replace `main.go` — Docker and bare-metal deployments are
untouched and keep working exactly as before.

> **Status: written but never built, run, or deployed.** Nothing in this port
> has been compiled. Go cannot be compiled in the environment where this was
> authored. Treat the first `vercel deploy` as the first real test, and read
> [Known risks](#known-risks-read-before-deploying) first — some items there
> will require a fix, not just a config tweak.

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

One environment variable, both problems.

---

## What was added

All new files. No existing file was modified, so there is nothing to un-merge if
you abandon this.

| File | Purpose |
|---|---|
| `service/serverless_tasks.go` | Synchronous single-pass equivalents of the task daemons. In `package service` so it can reach unexported internals. |
| `serverless/bootstrap.go` | Lazy per-instance init + Gin engine, mirroring `InitResources()`/`main()` minus every goroutine. |
| `api/index.go` | Vercel Function: the whole HTTP surface. |
| `api/cron/index.go` | Vercel Cron: authenticated one-shot job runner. |
| `vercel.json` | Build, route map, function limits, cron schedules. |
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

### 1. Run migrations first — this will bite you otherwise

Slave nodes **intentionally skip schema migration** (`model.InitDB()` only
AutoMigrates on master). Because this deployment runs as a slave, **migrations
never run**, and an unmigrated database fails at query time rather than at boot,
which looks like a mysterious runtime bug.

Migrate once against the same database before the first deploy — easiest is to
boot the normal Docker image against it, let it migrate, then stop it:

```bash
docker run --rm -e SQL_DSN="postgres://..." -e NODE_TYPE=master \
  calciumion/new-api:latest
```

Repeat this after any upgrade that adds migrations.

### 2. Provision a database

Managed Postgres or MySQL. **SQLite cannot work** — it is a local file, and each
instance would get its own empty copy that vanishes on recycle.

Use a **transaction-mode connection pooler** (PgBouncer, Neon pooled endpoint,
Supabase port `6543`). See the pooling note in step 3.

### 3. Set environment variables

Copy from `.env.vercel.example`. The non-obvious ones, in rough order of how
badly they bite:

- **`SQL_MAX_OPEN_CONNS=3`** — upstream defaults to **1000**, sized for one
  process. Here the pool is *per instance*, so the real ceiling is
  `1000 x concurrent instances`; a modest spike exhausts the database
  connection limit and it starts refusing connections.
- **`SESSION_SECRET`** — upstream generates a random one at startup if unset.
  Across instances that means each signs cookies with a *different* key, so
  dashboard logins break seemingly at random. Must be pinned.
- **`NODE_TYPE=slave`** and **`FRONTEND_BASE_URL`** — required; the bootstrap
  refuses to start without them and explains why.
- **`CRON_SECRET`** — `/api/cron` fails **closed**; unset means every cron
  request is rejected.
- **`BATCH_UPDATE_ENABLED=false`**, **`DATA_EXPORT_ENABLED=false`** — otherwise
  buffered billing/usage data is silently lost.
- **`TRUSTED_PROXIES`** — requests always arrive via Vercel's edge, so without
  this every client collapses into one rate-limit bucket.

### 4. Deploy

```bash
vercel deploy
```

The frontend builds via `cd web && npm install && npm run build` (rsbuild) into
`web/dist`, served as static files. The Go function handles the API.

### 5. Verify, in this order

```bash
curl -i https://<deployment>/api/status                     # dashboard API reachable
curl -i https://<deployment>/v1/models -H "Authorization: Bearer <token>"
curl -i "https://<deployment>/api/cron?job=subscriptions" \
     -H "Authorization: Bearer $CRON_SECRET"                # expect {"success":true}
```

If `/api/status` returns HTML instead of JSON, the `/api/*` rewrite lost to the
SPA fallback — see the route collision below.

---

## Known risks (read before deploying)

Ordered by how likely they are to actually break the deployment.

### 1. `/api/*` route collision — most likely to break

New API serves its dashboard API under `/api/...`. Vercel also treats `api/` as
its **functions directory**. These two conventions overlap directly.

The mitigation in `vercel.json` relies on Vercel checking the filesystem before
applying rewrites, so `/api/cron` resolves to the real function while
`/api/user/self` falls through to the `/api/:path*` rewrite. **This ordering was
not tested.** Check `/api/status` first. If it misroutes, move the catch-all out
of `/api` (e.g. `api/gateway/index.go` reached only via rewrites) so no real
function path shares the prefix.

### 2. Go runtime version — unverified

`go.mod` declares **`go 1.25.1`**. Whether Vercel's Go runtime supports that
version could not be confirmed (both documentation lookups failed: the
`vercel-community/go` README 404s and `api.github.com` was rate-limited). If the
build rejects the toolchain, lower the `go` directive. **This gates everything
else.**

### 3. WebSockets cannot work — no workaround

`/v1/realtime` is registered via `gorilla/websocket`. Serverless functions
cannot hold a bidirectional long-lived connection. OpenAI Realtime relaying is
**permanently unavailable** on this deployment; it needs a real server. This is
the one feature that is genuinely lost rather than degraded.

### 4. Cron granularity

`vercel.json` requests `* * * * *` (every minute) for system tasks, but
`midjourneyPollHandler` and `asyncTaskPollHandler` declare
`Interval() = 15 * time.Second`. Platform cron cannot go below one minute, so
async image/video completion is detected up to ~4x slower. Polling is
idempotent, so this is latency, not corruption.

Also: **Hobby plans restrict cron count and frequency** (believed to be roughly
daily). Exact current limits could not be verified — check your plan. If
minute-level cron is unavailable, drive `/api/cron` from any external scheduler
(GitHub Actions, cron-job.org, a cheap VPS) with the same `Bearer` secret; the
endpoint is deliberately platform-agnostic.

### 5. Long streams get truncated

`maxDuration` is set to `300`, which requires Pro; Hobby is lower. Any SSE
stream or relay exceeding the limit is killed mid-response, and the client sees
a truncated stream rather than a New API error. `RELAY_TIMEOUT`/
`STREAMING_TIMEOUT` are set below the ceiling so New API fails first with a
proper error where possible.

Separately: whether Vercel's Go runtime streams incrementally or buffers the
full response is **unverified**. If SSE arrives all at once at the end,
streaming responses are unusable and this is a hard blocker for chat clients.

### 6. Configuration staleness

With `SyncOptions`/`SyncChannelCache` dropped, a warm instance can serve stale
configuration until it is recycled. Configuring Redis narrows this window
considerably. Expect admin changes not to take effect instantly.

### 7. `go build ./...` now reports an error

Vercel requires entrypoints to be `package main` exposing an exported handler,
and injects `func main()` at build time. So local builds complain that
`function main is undeclared in the main package` for `./api/...`. Exclude it:

```bash
go build $(go list ./... | grep -v '/api')
```

This affects local tooling and CI only. The root `main.go` build is unaffected.

---

## Security notes

Tightened while porting, because serverless changes the threat model:

- **`/api/cron` fails closed.** Unset `CRON_SECRET` denies all requests instead
  of allowing them. The secret is compared with `crypto/subtle.ConstantTimeCompare`
  so it cannot be recovered through response timing. Without this the endpoint
  is an amplification vector: an anonymous caller could force channel tests
  against every upstream provider on demand.
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

No claim is made that the codebase is free of vulnerabilities. These are
deployment-model issues found while porting, not the result of a security audit
of the ~78 files in `service/` or ~88 in `controller/`.

---

## If serverless turns out to be the wrong fit

Given WebSockets being impossible, possible response buffering, cron
granularity, and per-instance connection pools, a long-running container is a
better match for this application. `Dockerfile` and `docker-compose.yml` already
work unchanged, and Railway / Fly.io / Render deploy them directly with none of
the caveats above.

This port exists because Vercel was the explicit requirement. It is the honest
maximum for that constraint, not a claim that the constraint is optimal.
