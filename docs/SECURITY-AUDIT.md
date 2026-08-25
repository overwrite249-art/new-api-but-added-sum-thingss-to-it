# Security audit — pass 1

**Scope of this pass:** `middleware/` (auth, rate limiting, client IP, CORS,
Turnstile, security proofs, cookie origin guard), `router/relay-router.go`,
`router/api-router.go`, the session/secret handling in `common/init.go` and
`common/session_cookie.go`, and the deployment model of the serverless port.

**Not reviewed:** `service/` (~78 files), `controller/` (~88 files), all of
`relay/` except `mjproxy_handler.go`, `model/`, `oauth/`, `pkg/billingexpr`, and
the frontend. Quota and billing invariants were not tested.

**Therefore: this document does not claim the codebase is free of
vulnerabilities.** It records what was actually read and verified. Anything not
listed here was not checked, which is different from being safe.

Severity is judged for a **publicly reachable** deployment.

| ID | Severity | Summary | Status |
|---|---|---|---|
| S-1 | High | `TRUSTED_PROXIES=0.0.0.0/0` made `c.ClientIP()` forgeable | **Fixed** (was introduced by this port) |
| S-2 | High | Without Redis, no rate limiting works on serverless | **Documented**, needs config |
| S-9 | High | No `SESSION_SECRET` requirement, so sessions and HMACs failed across instances | **Fixed** (was a defect of this port) |
| S-3 | Medium | `/mj/image/:id` is unauthenticated and not user-scoped | Upstream, unchanged |
| S-4 | Medium | Model rate limiter behaves differently on Redis vs memory | Upstream, unchanged |
| S-10 | Medium | Secure-cookie default disabled the auth-endpoint CSRF guard | **Fixed** (inherited default, wrong here) |
| S-5 | Low | Wildcard CORS is advertised with credentials enabled | Upstream, note only |
| S-6 | Info | API keys accepted in the query string | Upstream, by design |
| S-7 | Info | `TokenAuthReadOnly` accepts expired/exhausted tokens | Upstream, by design |
| S-8 | Info | `WssAuth` is an empty unused middleware | Upstream, dead code |

Three of the ten findings (S-1, S-9, S-10) are the serverless port's own doing,
not upstream's. That ratio is the main lesson of this pass: **configuration
written to make a deployment work is part of its attack surface**, and it has to
be reviewed with the same suspicion as code.

---

## S-1 — High — Forgeable client IP via `TRUSTED_PROXIES=0.0.0.0/0`

**This one was my own mistake**, introduced by the serverless port rather than
found in upstream. It is recorded rather than quietly deleted.

`.env.vercel.example` and `docs/VERCEL.md` both recommended
`TRUSTED_PROXIES=0.0.0.0/0`, reasoning that requests always arrive through the
platform edge so the edge may as well be trusted.

### Why it is wrong

Gin resolves the client address by walking `X-Forwarded-For` **right to left**,
returning the first hop it does not trust. Its `validateHeader` loop stops on:

```go
if (i == 0) || (!e.isTrustedProxy(ip)) {
    return ipStr, true
}
```

The `i == 0` arm is the problem. When *every* address is trusted, the walk never
finds an untrusted hop and falls through to `items[0]` — the **leftmost** entry,
which is whatever the client sent. So:

```
X-Forwarded-For: 1.2.3.4
```

makes `c.ClientIP()` return `1.2.3.4`, fully attacker-chosen and freely rotated
per request.

### Impact

| Control | Consequence |
|---|---|
| Token IP allowlists — `token.GetIpLimits()` checked in `TokenAuth` via `common.IsIpInCIDRList` | Bypassed outright. This is an **access control**, not throttling. |
| `GlobalWebRateLimit` (`GW`), `GlobalAPIRateLimit` (`GA`), `CriticalRateLimit` (`CT`), `UploadRateLimit`, `DownloadRateLimit` | All keyed by `redisIPRateLimitKey(mark, c.ClientIP())`. Bypassed by rotating the header. |
| `EmailVerificationRateLimit` (2 per 30 s per IP) | Stops limiting; the verification endpoint becomes a mail bomber. |
| `TurnstileCheck` | Sends a forged `remoteip` to Cloudflare, weakening risk scoring. |
| Audit logging | Records attacker-chosen addresses, so abuse cannot be traced. |

Note which limiters are *not* affected: `UserCriticalRateLimit` and
`SearchRateLimit` go through `userRateLimitFactory`, keyed on the authenticated
user ID. Upstream's own comment says this is deliberately "resistant to proxy
rotation attacks" — the codebase already knew IP keys were weak.

### Fix

**Leave `TRUSTED_PROXIES` unset.** `middleware/trusted_proxies.go` defaults to
`defaultTrustedProxyCIDRs` — `127.0.0.0/8`, `::1`, `10.0.0.0/8`,
`172.16.0.0/12`, `192.168.0.0/16`, `fc00::/7` — which covers the platform's
internal hop, because the function process is invoked from a local proxy and not
directly from the internet. Any forged entry then sits to the *left* of a real
untrusted address, so the right-to-left walk returns the genuine one.

Widen it only for a proxy you operate, listing that proxy's addresses
explicitly.

`TRUSTED_PROXIES=none` is the opposite error: `SetTrustedProxies(nil)` makes Gin
ignore `X-Forwarded-For` entirely, every client collapses into the internal
address, and one user exhausts everybody's limit.

---

## S-2 — High — Rate limiting silently does nothing without Redis

`rateLimitFactory` falls back to `inMemoryRateLimiter` whenever
`common.RedisEnabled` is false, and that counter lives in the instance's own
memory. `ModelRequestRateLimit` has the same fallback, as does
`EmailVerificationRateLimit`.

On a long-lived single process that is correct. In a function runtime it is not:
instances scale horizontally and are recycled freely, so the effective limit is
`configured limit x live instances`, and it resets whenever an instance is
replaced. Under exactly the load that rate limiting exists to control, the
platform adds instances, which *raises* the ceiling.

The failure is silent — no warning, and the limiter appears configured.

**Action:** treat `REDIS_CONN_STRING` as required for any publicly reachable
deployment. Use a serverless-friendly Redis; a TCP pool per instance has the
same fan-out problem as the database pool.

---

## S-3 — Medium — `/mj/image/:id` is unauthenticated and not user-scoped

In `router/relay-router.go`:

```go
func registerMjRouterGroup(relayMjRouter *gin.RouterGroup) {
    relayMjRouter.GET("/image/:id", relay.RelayMidjourneyImage)
    relayMjRouter.Use(middleware.TokenAuth(), middleware.Distribute())
    { /* every POST route */ }
}
```

**Root cause is a Gin ordering rule:** a route's handler chain is fixed when the
route is registered. `.Use()` afterwards does not retroactively apply. So
`GET /image/:id` runs with `RouteTag` and `SystemPerformanceCheck` (added to the
group *before* this call) but **without `TokenAuth` or `Distribute`**, while
every POST below it is authenticated.

Because `registerMjRouterGroup` is invoked for both `/mj` and `/:mode/mj`, the
endpoint is reachable at `/mj/image/:id` **and** `/<anything>/mj/image/:id`.

The handler then does:

```go
taskId := c.Param("id")
midjourneyTask := model.GetByOnlyMJId(taskId)
```

`GetByOnlyMJId` takes no user ID. Every other Midjourney path uses
`model.GetByMJId(userId, taskId)` or `GetByMJIds(userId, ids)`, so this is the
only lookup in the group that is not scoped to the caller.

### Consequences

- **Cross-user read.** Anyone who learns or guesses a task ID retrieves that
  user's generated image. No authentication, no ownership check.
- **Unauthenticated outbound fetch.** Each request makes the server perform a
  `GET` and stream the result back — an unauthenticated egress trigger and, on a
  per-invocation billing model, an unauthenticated cost trigger.
- **No token-level throttling**, since `TokenAuth` never runs. Combined with S-2
  that means effectively no throttling.

### What is *not* wrong here

SSRF is genuinely handled, and it would be easy to misreport this as an SSRF
hole. When no channel proxy is configured the handler calls
`service.ValidateSSRFProtectedFetchURL(...)` and fetches with
`service.GetSSRFProtectedHTTPClient()`, which validates per-IP at dial time. On
the channel-proxy path it falls back to a single pre-request
`common.ValidateURLWithFetchSetting(...)`, and the code says why: the connection
is established by the proxy, so per-IP dial-time checks are impossible. That
leaves a documented TOCTOU / DNS-rebinding window on the proxy path only.

The URL is also not attacker-supplied directly — it comes from a stored task row
written by the upstream provider or by `RelayMidjourneyNotify`.

### Assessment

This looks **deliberate**: `coverMidjourneyTaskDto` builds
`system_setting.ServerAddress + "/mj/image/" + originTask.MjId` and hands it to
clients as a plain image URL, which only works if it is fetchable without a
header. Gated behind `setting.MjForwardUrlEnabled`.

So it is a design trade-off, not an accident — but it is an unauthenticated,
unthrottled, cross-user endpoint, and anyone operating this publicly should know
that. If you do not need image forwarding, disable `MjForwardUrlEnabled`.
Hardening it properly means a signed, expiring URL rather than a bare task ID.

**Not changed here**, because it is upstream behaviour that clients depend on
and this port deliberately does not alter application semantics.

---

## S-4 — Medium — The model rate limiter is not equivalent across backends

In `middleware/model-rate-limit.go`:

1. **The memory path counts attempts, the Redis path counts successes.**
   `redisRateLimitHandler` only calls `recordRedisRequest(...)` when
   `c.Writer.Status() < 400`. `memoryRateLimitHandler` first consumes a slot on
   a synthetic `successKey + "_check"` key for *every* request, then consumes
   another on `successKey` if the request succeeded. So the "success limit"
   throttles failures too, and a user can be locked out by their own errors on
   one backend but not the other.

2. **`checkRedisRateLimit` is not atomic.** It issues `LLen`, then `LIndex`,
   then later `LPush`/`LTrim` as separate commands. Concurrent requests can all
   observe `length < maxCount` and proceed, overshooting the limit. Contrast
   `middleware/rate-limit.go`, which deliberately uses a single Lua script and
   documents the fixed-window trade-off. The model limiter did not get the same
   treatment.

3. **A corrupt stored timestamp fails closed indefinitely.** If
   `time.Parse(modelRateLimitTimeFormat, oldTimeStr)` fails, the function
   returns an error, the handler answers 500, and it keeps doing so until the
   key expires.

Serverless makes (2) worse, since concurrency arrives as separate instances.

---

## S-5 — Low — Wildcard CORS with credentials enabled

`middleware/cors.go`:

```go
config.AllowAllOrigins = true
config.AllowCredentials = true
config.AllowHeaders = []string{"*"}
```

This is **not currently exploitable**: with `Access-Control-Allow-Origin: *`,
browsers refuse to send credentials, so a hostile page cannot ride a logged-in
admin's session cookie.

It is listed for one reason: the combination looks like a bug, and the obvious
"fix" — echoing the request's `Origin` back so credentials work — would turn it
into a real cross-origin session-riding vulnerability on every dashboard
endpoint. **Do not reflect `Origin` here.** If specific origins need
credentialed access, allowlist them explicitly.

Also worth knowing, same Gin ordering rule as S-3: `SetRelayRouter` calls
`router.Use(...)` on the engine, so those global middlewares only attach to
routes registered after that point. Middleware coverage has to be checked per
group rather than assumed from the file.

---

## S-6 — Info — API keys accepted in the query string

`TokenAuth` accepts the relay key from several places for provider
compatibility: `Authorization`, `x-api-key` (Anthropic paths),
`x-goog-api-key`, `mj-api-secret`, `Sec-WebSocket-Protocol`
(`openai-insecure-api-key.<key>`), and **`?key=`** on `/v1/models`,
`/v1beta/models`, `/v1beta/openai/models` and `/v1/models/`.

Query strings are the weakest of these: they land in platform request logs,
proxy logs, browser history and `Referer` headers. This is required for Gemini
SDK compatibility, so it is by design — but treat any key that has ever been
sent that way as logged, and prefer headers where the client allows it.

---

## S-7 — Info — `TokenAuthReadOnly` intentionally accepts weak tokens

Documented in its own comment: it validates only that the key exists, skipping
status, expiry and quota checks, so expired or exhausted tokens can still read
their own usage logs. Only `common.TokenStatusDisabled` is rejected, and banned
users are still blocked.

Deliberate, and reasonable for read-only usage views. Flagged so nobody mounts
it on anything that is not read-only. Currently mounted on
`/api/usage/token` and `/api/log/token` only.

---

## S-8 — Info — `WssAuth` is an empty middleware

```go
func WssAuth(c *gin.Context) {

}
```

It does nothing — no authentication, and not even `c.Next()`. A repository-wide
search finds exactly one occurrence: this definition. **It is dead code and is
not mounted anywhere**, so it is not currently a vulnerability.

It is a trap, though: the name implies it authenticates WebSocket upgrades, and
mounting it would silently authenticate nothing. `/v1/realtime` is in fact
protected, because `relayV1Router.Use(middleware.TokenAuth())` covers the
`wsRouter` subgroup. Deleting `WssAuth` would be an improvement.

---

## S-9 — High — Sessions and HMACs failed across instances without `SESSION_SECRET`

**Another defect of this port**, and a worse one than S-1 in the sense that it
fails *silently and intermittently* rather than being exploitable on demand.

`validateServerlessConfig` refused to serve without `SQL_DSN` but said nothing
about `SESSION_SECRET`. Upstream's fallback is in `common/constants.go`:

```go
var SessionSecret = uuid.New().String()
var CryptoSecret  = uuid.New().String()
```

A package-level initialiser, so it is evaluated **once per process**. And in
`common/init.go`, `SESSION_SECRET` is only consulted when non-empty:

```go
if os.Getenv("SESSION_SECRET") != "" { /* ... */ SessionSecret = ss }
if os.Getenv("CRYPTO_SECRET") != "" {
    CryptoSecret = os.Getenv("CRYPTO_SECRET")
} else {
    CryptoSecret = SessionSecret
}
```

Note that `InitEnv` rejects the literal placeholder `random_string` with
`log.Fatal`, but accepts the variable being absent entirely.

### Why the deployment model changes the severity

For a daemon this is a minor annoyance: one process, one secret, and a restart
signs everybody out. A function runtime serves requests from many concurrent
instances, each of which ran that initialiser separately, so there is no shared
key at all. Consequences:

- **Sessions and access tokens issued by one instance are rejected by the next.**
  The user sees random logouts and login loops. Nothing is logged, because from
  each instance's point of view it simply received an invalid credential.
- **`CryptoSecret` inherits the same divergence**, and `common/crypto.go` keys
  HMAC-SHA256 with it:

  ```go
  func GenerateHMAC(data string) string {
      h := hmac.New(sha256.New, []byte(CryptoSecret))
      ...
  }
  ```

  So an HMAC produced on one instance does not verify on another.

The reason this is rated High is the diagnosis cost. There is no error message
to search for, the failure rate scales with how many instances are warm, and it
looks exactly like an application session bug.

**Scope note:** only `GenerateHMAC` was verified as a consumer of
`CryptoSecret`. HMAC is authentication, not encryption, so this is a
verification-failure problem — **not** a claim that stored ciphertext becomes
undecryptable. Whether anything encrypts at rest with this key was not checked.

### Fix

`SESSION_SECRET` is now required by `validateServerlessConfig`, alongside
`SQL_DSN`, with an error that explains the per-process fallback. A missing value
now produces a JSON 500 naming the variable instead of behaving erratically.

---

## S-10 — Medium — The secure-cookie default disabled the auth CSRF guard

Inherited from upstream rather than written here, but wrong for this deployment
model and therefore this port's responsibility.

`common.SessionCookieSecure` defaults to `false`. That reads like a cookie
attribute, but `middleware/auth_origin.go` turns the whole guard off with it:

```go
func SessionCookieOriginGuard() gin.HandlerFunc {
    return func(c *gin.Context) {
        if !common.SessionCookieSecure {
            c.Next()
            return
        }
        origin, ok := requestBrowserOrigin(c.Request)
        if !ok || !isAllowedSessionOrigin(c.Request, origin) {
            /* 403 AUTH_ORIGIN_FORBIDDEN */
        }
```

Its own comment is explicit that this is intentional: *"In insecure local
development mode it preserves the legacy behavior and intentionally performs no
Origin validation."* Reasonable for `http://localhost`. Not reasonable for a
public HTTPS deployment, which is the only kind this platform produces.

The guard is mounted in `router/api-router.go` on exactly two routes:

```go
userRoute.POST("/auth/refresh", middleware.SessionCookieOriginGuard(), ...)
userRoute.POST("/auth/logout",  middleware.SessionCookieOriginGuard(), ...)
```

Both are cookie-authenticated, so with the default they were the classic CSRF
shape: a cross-site page could force a token refresh or a logout using the
victim's ambient cookie. Impact is bounded — neither endpoint moves money or
changes credentials — which is why this is Medium and not High.

### Fix, and the trap in it

`applySessionCookieDefaults` now sets `SESSION_COOKIE_SECURE=true` before
`common.InitEnv()` runs. The obvious one-line version of that fix **bricks every
cold start**, because `common/session_cookie.go` enforces a pairing:

```go
if trustedURLsRaw == "" {
    return fmt.Errorf("SESSION_COOKIE_SECURE=true requires SESSION_COOKIE_TRUSTED_URL")
}
```

and `InitEnv` handles that error with `log.Fatal`, i.e. `os.Exit(1)` before any
handler can reply — the same opaque `FUNCTION_INVOCATION_FAILED` class of
failure as the read-only `logs` directory crash. So the default is applied only
when an https origin can be derived from the resolved `FRONTEND_BASE_URL` to
pair with it, an explicit `SESSION_COOKIE_SECURE=false` is still honoured, and
an operator who sets the flag themselves without a trusted URL gets the derived
origin filled in rather than a dead deployment.

Trusted origins must be https and scheme+host only; `common.NormalizeOrigin`
rejects paths, queries, credentials and wildcards, and `isAllowedSessionOrigin`
compares with `crypto/subtle.ConstantTimeCompare` against both the request's own
origin and the configured list.

---

## Deployment-model items (already addressed)

- **`/api/cron` fails closed** on an unset `CRON_SECRET` and compares with
  `crypto/subtle.ConstantTimeCompare`. This matters more now that
  `?job=migrate` runs DDL: the secret authorises schema changes, so it must be
  long and random. It is also an amplification vector if left open — the
  scheduled jobs can force channel tests against every configured upstream.
- **`NoRoute` redirect loop removed.** Upstream 301s unmatched paths to
  `FRONTEND_BASE_URL`; on a single-domain deployment that is a self-redirect
  loop. The bootstrap re-registers a JSON 404.
- **Secrets must not go in `vercel.json`.** Its `env` block works and is
  committed to a public repository.
- **Rotate anything that has passed through a chat, log or terminal**, including
  database passwords and provider API keys.

---

## Suggested next passes

In descending expected value:

1. `controller/user.go` (~42 KB) — registration, login, password reset, e-mail
   binding, session lifecycle.
2. `oauth/` — OAuth `state` handling and CSRF protection on account binding.
   Note that `/api/oauth/:provider` is a GET with `TryUserAuth`, so the binding
   flow is reachable while authenticated; `state` is the only defence.
3. `service/authz/` and `RequirePermission` — whether every admin route is
   actually gated, and whether `RequirePermission` is reachable without
   `authHelper` having run (it reads `c.GetInt("role")`, which defaults to 0).
4. `pkg/billingexpr` and the quota paths — `AGENTS.md` documents extensive
   overflow invariants; regressions there are financial.
5. `relay/channel/*` — per-provider adaptors handling untrusted upstream
   responses.
6. Token generation and password hashing — confirm `crypto/rand` throughout.
7. The unauthenticated payment webhooks in `router/api-router.go`
   (`/api/stripe/webhook`, `/api/creem/webhook`, `/api/waffo/webhook`,
   `/api/waffo-pancake/webhook/:env`, `/api/user/epay/notify`,
   `/api/subscription/epay/notify`) — signature verification is the only thing
   standing between these and forged balance top-ups.
