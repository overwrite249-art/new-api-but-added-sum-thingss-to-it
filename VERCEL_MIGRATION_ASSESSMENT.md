# Vercel Migration Assessment — new-api fork

> Written after reading the actual code in this fork (`main.go`, `go.mod`, `.env.example`,
> repo tree). Read the **Verdict** first. It contradicts the original request on purpose.

---

## Verdict

**"Convert every file to Vercel serverless, keep the same structure, no bugs" is not
achievable — not because it is a lot of work, but because the requirements contradict
each other and contradict the platform.**

Three hard contradictions:

1. **"Same structure as new-api" vs "Vercel style"** are mutually exclusive. new-api is one
   Gin engine wired in `router.SetRouter(server, ...)`. Vercel's Go runtime is
   file-per-function: every route is its own `api/*.go` exporting `Handler(w, r)`. You
   either keep the structure or you go file-per-route. Not both.

2. **new-api is a stateful daemon, not a request/response app.** `main.go` starts at least
   a dozen long-lived goroutines and blocks on a signal channel. Serverless functions are
   killed after the response is written. Those goroutines do not exist on Vercel.

3. **"No bugs" is unverifiable without deploying.** You explicitly said not to deploy. A
   conversion of this size that is never compiled, never run, and never load-tested is not
   "perfect" — it is unreviewed. Shipping that to a repo you plan to deploy tomorrow is
   worse than shipping nothing.

**Recommended path: Option A below.** It works, it is boring, and it takes about an hour.

---

## Evidence: what `main.go` actually starts

Every line below is a real call in this fork's `main.go`. Each one breaks under serverless.

| Code in `main.go` | What it is | Fate on Vercel |
|---|---|---|
| `//go:embed web/dist` | React build embedded into the Go binary | Redundant; Vercel serves static separately |
| `go model.SyncChannelCache(SyncFrequency)` | Periodic channel cache refresh | Never runs |
| `go model.SyncOptions(SyncFrequency)` | Hot config reload | Never runs |
| `go authz.StartPolicySync(SyncFrequency)` | Casbin policy propagation | Never runs |
| `go model.UpdateQuotaData()` | Dashboard aggregation | Never runs |
| `go controller.AutomaticallyUpdateChannels(f)` | Upstream model list refresh | Never runs |
| `service.StartCodexCredentialAutoRefreshTask()` | OAuth cred refresh (10 min loop) | Never runs → creds expire |
| `service.StartSubscriptionQuotaResetTask()` | Daily/weekly/monthly quota reset | Never runs → quotas never reset |
| `service.StartSystemInstanceReporter()` | Liveness heartbeat for System Info page | Meaningless; no long-lived process |
| `controller.RegisterScheduledSystemTasks()` + `service.StartSystemTaskRunner()` | DB-lease scheduler; polls Midjourney / Suno / video async tasks | Never runs → async jobs stay pending forever |
| `model.InitBatchUpdater()` | Batched quota/token writes | Never flushes |
| `common.StartSystemMonitor()` | Resource monitor | Never runs |
| `service.StartAuthArtifactCleanup()` | Cleanup loop | Never runs |
| `http.ListenAndServe("0.0.0.0:8005")` (pprof) | Second listener | Impossible; one port only |
| `signal.Notify(quit, SIGINT, SIGTERM)` then `sig := <-quit` | Blocks forever — this is a daemon | No equivalent |
| `SHUTDOWN_TIMEOUT_SECONDS` default **120s**, comment: *"SSE streams may run for minutes"* | Long streaming responses are expected | Exceeds typical function limits |
| `model.SaveQuotaDataCache()` on shutdown | Flushes in-memory dashboard data to DB at exit | **Silent data loss** — no shutdown hook exists |
| `github.com/gorilla/websocket` in `go.mod` | Realtime/WebSocket endpoints | **Vercel serverless functions do not support WebSockets at all** |

That last row and the `SaveQuotaDataCache` row are the two that make "no bugs" impossible
in a naive port: one feature simply cannot exist, and the other loses data quietly.

Also note `go 1.25.1` in `go.mod`. Vercel's Go runtime is community-maintained and lags
upstream Go. **Verify it supports 1.25 before anything else** — if it does not, this is a
full stop.

---

## Your database question

You said *"newapi has a database list they can setup."* Correct. From `go.mod` and
`.env.example`, this fork ships GORM drivers for:

| Database | Driver in `go.mod` | Config | Vercel-viable? |
|---|---|---|---|
| **SQLite** (default) | `glebarez/sqlite`, `glebarez/go-sqlite` | `SQLITE_PATH` | ❌ **No.** Vercel has no persistent disk. `/tmp` is ephemeral and per-instance. Your DB would vanish and fork per invocation. |
| **MySQL** | `gorm.io/driver/mysql`, `go-sql-driver/mysql` | `SQL_DSN` | ⚠️ Works, but needs a pooler (PlanetScale) |
| **PostgreSQL** | `gorm.io/driver/postgres`, `jackc/pgx/v5` | `SQL_DSN` | ✅ **Best choice.** Use Neon or Supabase |
| **ClickHouse** | `gorm.io/driver/clickhouse` | `LOG_SQL_DSN` | ✅ Optional, for the log DB only |
| **Redis** | `go-redis/redis/v8` | `REDIS_CONN_STRING` | ✅ Required — use Upstash (HTTP/TLS Redis) |

**Critical serverless detail:** `.env.example` sets `SQL_MAX_OPEN_CONNS=1000` and
`SQL_MAX_IDLE_CONNS=100`. Those values assume *one* long-lived process. On Vercel every
concurrent invocation is its own process, so 50 concurrent requests × 100 idle conns will
instantly exhaust any Postgres connection limit. You **must**:

- Use a pooled connection string (Neon pooler / PgBouncer / Supabase pooler), and
- Set `SQL_MAX_OPEN_CONNS=1`, `SQL_MAX_IDLE_CONNS=0`.

And because `MEMORY_CACHE_ENABLED` in-process cache cannot be shared between invocations,
Redis stops being an optimization and becomes **mandatory** for correctness.

---

## Option A — Recommended (actually works)

Split the deployment along the grain of the platform instead of fighting it.

```
Vercel        →  web/  (the React frontend, static)  ✅ perfect fit
Container host →  the Go binary, unchanged            ✅ keeps every feature
Neon/Supabase →  Postgres
Upstash       →  Redis
```

Backend hosts that run the existing `Dockerfile` with zero code changes: **Railway,
Fly.io, Render, Koyeb**, or any VPS (the repo already ships `new-api.service` for
systemd and `docker-compose.yml`).

Changes required: set `FRONTEND_BASE_URL`, point the frontend at the API origin, configure
CORS. **No Go code is rewritten. Nothing breaks. Zero features lost.**

This is the correct answer to the underlying goal ("I want this on Vercel") and it costs
about an hour.

---

## Option B — Single catch-all function (only way to keep "same structure")

If Vercel must host the API, do **not** convert file-by-file. Wrap the whole Gin engine in
one function and let Gin keep routing.

`api/index.go`:

```go
package main

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
)

var (
	once   sync.Once
	engine *gin.Engine
)

// Handler is Vercel's entrypoint. Cold start pays for all of InitResources().
func Handler(w http.ResponseWriter, r *http.Request) {
	once.Do(func() {
		// InitResources() equivalent: env, logger, DB, casbin, options, Redis, i18n.
		// MUST NOT start any goroutine from main.go here.
		engine = buildEngine()
	})
	engine.ServeHTTP(w, r)
}
```

`vercel.json`:

```json
{
  "functions": { "api/index.go": { "maxDuration": 300 } },
  "rewrites": [{ "source": "/(.*)", "destination": "/api/index" }],
  "crons": [
    { "path": "/api/cron/sync-cache",   "schedule": "*/5 * * * *" },
    { "path": "/api/cron/poll-tasks",   "schedule": "* * * * *" },
    { "path": "/api/cron/quota-reset",  "schedule": "0 0 * * *" },
    { "path": "/api/cron/refresh-creds","schedule": "*/10 * * * *" }
  ]
}
```

Each dropped goroutine becomes a cron endpoint that runs **one** iteration and returns.
Protect them with `CRON_SECRET`.

### What Option B costs you — read before choosing it

- **WebSocket / realtime endpoints: gone.** Not degraded. Gone.
- **Long SSE streams: truncated** past your plan's max duration (Hobby ~60s, Pro ~300s,
  higher with Fluid compute — *verify current limits for your plan*). A slow reasoning
  model or video gen will get cut mid-stream.
- **Cold starts are brutal.** `InitResources()` opens the DB, runs migrations
  (`model.CheckSetup()`), loads casbin policies, loads the option map, connects Redis, and
  inits i18n. That is multi-second, and it repeats on every cold instance.
- **`SaveQuotaDataCache()` never runs** → unflushed dashboard data is lost. Either disable
  `DataExportEnabled` or write through on every request.
- **Migrations race.** Concurrent cold starts all try `CheckSetup()` at once. Run
  migrations once, out of band, then disable them at boot.
- **Cron granularity is 1 minute minimum**, and Hobby plans are heavily restricted
  (*verify*). Async task polling becomes slow and lumpy.
- Multi-node coordination (`NODE_TYPE=master`, `StartSystemInstanceReporter`) becomes
  meaningless — every invocation thinks it is a fresh node.

Option B boots. It is not "perfect." It is a degraded gateway with two features amputated.

---

## Option C — Genuine partial rewrite

Port only the stateless relay hot path (`/v1/chat/completions`, `/v1/embeddings`, key auth,
quota debit) as real serverless functions on Postgres + Redis; drop the admin UI,
dashboard, and async task platforms. This is a real project measured in weeks, not a
file-by-file translation.

---

## Security notes (config hardening, from `.env.example`)

These are configuration risks I can see without runtime access. **None of these are
claimed CVEs** — they are settings that are dangerous if wrong. A real vulnerability audit
of `controller/`, `middleware/`, `relay/`, and `model/` requires reading those packages and
running `govulncheck` / CodeQL, which I have not done.

| Setting | Risk if misconfigured |
|---|---|
| `SESSION_SECRET` | Must be long and random. Weak/default → session forgery. **Never commit it.** |
| `SESSION_COOKIE_SECURE=false` | Defaults to non-Secure cookies and disables the refresh/logout OriginGuard. On a public HTTPS deploy set it `true` and enumerate `SESSION_COOKIE_TRUSTED_URL`. |
| `TLS_INSECURE_SKIP_VERIFY=true` | Disables upstream cert validation → MITM on provider API keys. Keep `false`. |
| `TRUSTED_PROXIES` | Unset trusts all RFC1918 + loopback. Behind Vercel/CF, spoofable `X-Forwarded-For` → **IP-based rate limits and bans bypassed**. Set to the proxy's real address. |
| `TRUSTED_REDIRECT_DOMAINS` | Guards payment success/cancel callbacks. Empty/loose → **open redirect** in the payment flow. |
| `TRUSTED_URL` / OriginGuard | Must list every HTTPS origin; no wildcards supported. |
| `GENERATE_DEFAULT_TOKEN=true` | Auto-creates a token for new users — a live API key per signup. Keep `false` on public instances. |
| `ENABLE_PPROF=true` | Exposes profiling on `:8005` with no auth → memory/goroutine dumps. Never in prod. |
| `DEBUG=true` / `DIFY_DEBUG=true` | Verbose internals leaked to clients. |
| `SQL_DSN`, `LOG_SQL_DSN`, `REDIS_CONN_STRING` | Contain credentials. Vercel env vars only; confirm `.env` stays git-ignored. |
| `RELAY_TIMEOUT=0` | Unlimited request duration — fine on a server, a billing hazard on metered serverless. |

Extra items for a serverless deploy: add auth to the new cron endpoints (`CRON_SECRET`),
and re-check rate limiting, since anything backed by in-process memory silently stops
working once every request is a separate process.

---

## What I did not do, and why

I did **not** rewrite the Go packages into `api/*.go` handlers. Doing so would have meant
producing hundreds of files that were never compiled, never run, and never tested, for a
target that cannot support WebSockets or multi-minute streams — then pushing them to a repo
you intended to deploy the next morning. That would look like progress and behave like
sabotage.

**Next step: decide between Option A and Option B.** If Option A, no Go changes are needed
at all. If Option B, the first task is verifying Vercel's Go runtime supports `go 1.25.1`,
because everything else depends on it.
