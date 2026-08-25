// Package serverless boots New API inside a stateless function runtime
// (Vercel Functions, AWS Lambda, Cloud Run jobs, ...).
//
// It is a replacement for main.go, NOT a modification of it. main.go stays the
// canonical entrypoint for Docker / bare-metal deployments; nothing here
// changes its behaviour.
//
// WHAT IS DIFFERENT FROM main.go
// ------------------------------
// main.go ends with `sig := <-quit`, i.e. it is a daemon that blocks forever,
// and before that it starts ~13 background goroutines. A function runtime gives
// us the opposite execution model: code runs only while a request is in flight,
// the process is frozen or discarded afterwards, and there is no shutdown hook.
//
// So this package:
//
//   - initialises resources ONCE per instance, lazily, on the first request
//     (a "cold start") and reuses them for every subsequent request that the
//     same warm instance happens to serve;
//   - starts NO background goroutines;
//   - never listens on a port and never waits on a signal;
//   - does not embed or serve web/dist.
//
// CONFIGURATION
// -------------
// Settings that this runtime can determine for itself are forced or derived in
// applyServerlessDefaults rather than demanded from the operator. That is not a
// convenience: refusing to boot until a human sets them means EVERY request
// answers 500, including the /api/setup call that the first-run setup wizard
// needs, so the admin UI shows an error per panel and can never be configured.
//
//   - NODE_TYPE is FORCED to "slave". It is not a meaningful choice here, and
//     it is load-bearing twice:
//       1. StartSystemTaskRunner, StartSubscriptionQuotaResetTask and
//          StartCodexCredentialAutoRefreshTask each begin with
//          `if !common.IsMasterNode { return }`. Running as a slave means the
//          daemon loops cannot start even by accident. Their work is driven by
//          cron instead (see service/serverless_tasks.go).
//       2. router.SetRouter only calls SetWebRouter -- the sole consumer of the
//          embedded web/dist assets -- when the node IS master or when
//          FRONTEND_BASE_URL is empty. Slave + FRONTEND_BASE_URL set means the
//          embedded filesystem is never touched, so we can pass a zero-valued
//          WebAssets without tripping common.EmbedFolder.
//   - FRONTEND_BASE_URL is derived from the platform's own hostname when unset.
//     An explicit value always wins, which matters behind a custom domain.
//   - SESSION_COOKIE_SECURE defaults to true here, unlike upstream, because a
//     function platform terminates TLS for us and there is no plaintext
//     deployment to stay compatible with. It is not merely a cookie attribute:
//     middleware.SessionCookieOriginGuard disables ITSELF when the flag is off,
//     which removes the only CSRF defence on the cookie-authenticated auth
//     endpoints. See applySessionCookieDefaults.
//
// SQL_DSN and SESSION_SECRET are the two settings that cannot be guessed; see
// validateServerlessConfig for why neither has a usable fallback here.
//
// DATABASE MIGRATIONS
// -------------------
// Migration is explicit here, never a side effect of boot. A master node
// migrates inside model.InitDB() during startup; doing the same on a request
// path would bill one unlucky user for thirty-odd AutoMigrate statements, and
// two simultaneous cold starts would race on DDL.
//
// Call Migrate() instead -- once before first use, and again after an upgrade
// that changes the schema. The cron endpoint exposes it as ?job=migrate, so no
// Docker host or database client is required.
package serverless

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/oauth"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/relay"
	kitutil "github.com/QuantumNous/new-api/relaykit/relayconvert/kitutil"
	"github.com/QuantumNous/new-api/router"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/service/authz"
	_ "github.com/QuantumNous/new-api/setting/performance_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

var (
	initMu        sync.Mutex
	engine        *gin.Engine
	envDone       bool
	resourcesDone bool
)

// Engine returns the process-wide Gin engine, initialising it on first use.
//
// Deliberately NOT sync.Once: a cold start that fails because the database was
// briefly unreachable should be retryable by the next request on the same
// instance, instead of poisoning that instance for its whole lifetime. Steps
// that already succeeded are not repeated (see resourcesDone).
func Engine() (*gin.Engine, error) {
	initMu.Lock()
	defer initMu.Unlock()

	if engine != nil {
		return engine, nil
	}

	if err := initResources(); err != nil {
		return nil, err
	}

	built, err := buildEngine()
	if err != nil {
		return nil, err
	}

	engine = built
	return engine, nil
}

// EnsureResources initialises everything the background jobs need WITHOUT
// building the HTTP engine. Used by the cron endpoint, which needs a database
// connection, loaded options and registered task handlers, but no routes.
func EnsureResources() error {
	initMu.Lock()
	defer initMu.Unlock()
	return initResources()
}

// Migrate creates or updates the database schema, then returns. It replaces the
// migration that main.go performs implicitly while booting a master node.
//
// Intentionally minimal, and intentionally NOT built on initResources: on a
// fresh database model.CheckSetup() and model.InitOptionMap() read tables that
// do not exist until this has run, so full initialisation cannot be a
// precondition for migrating. This opens the database and nothing else.
//
// Safe to call against an already-migrated database; see model.RunMigrations.
//
// Caveat inherited from upstream: model.InitDB calls common.FatalLog when the
// connection itself cannot be opened, which exits the process rather than
// returning. A malformed SQL_DSN therefore surfaces as an opaque platform-level
// invocation failure instead of the JSON error below. validateServerlessConfig
// catches the common case of it being absent entirely.
func Migrate() error {
	initMu.Lock()
	defer initMu.Unlock()

	initEnvOnce()

	if err := validateServerlessConfig(); err != nil {
		return err
	}

	// Reuse the pool if a previous request on this warm instance already opened
	// one. model.InitDB is not idempotent -- calling it twice would abandon the
	// first *sql.DB along with its connections.
	if model.DB == nil {
		if err := model.InitDB(); err != nil {
			return fmt.Errorf("failed to initialize database: %w", err)
		}
	}
	if model.LOG_DB == nil {
		if err := model.InitLogDB(); err != nil {
			return fmt.Errorf("failed to initialize log database: %w", err)
		}
	}

	if err := model.RunMigrations(); err != nil {
		return fmt.Errorf("schema migration failed: %w", err)
	}

	common.SysLog("serverless: schema migration completed")
	return nil
}

// initEnvOnce performs the process-wide environment setup that both the request
// path and Migrate depend on. Caller must hold initMu.
//
// Split out of initResources so that Migrate can reuse it without pulling in
// the database-dependent steps that a not-yet-migrated database cannot serve.
//
// Guarded by its own flag rather than folded into resourcesDone: common.InitEnv
// calls flag.Parse() and contains several log.Fatal branches, so repeating it is
// pointless at best and fatal at worst.
func initEnvOnce() {
	if envDone {
		return
	}

	kitutil.SetLogging(common.SysLog, func(message string) {
		logger.LogError(nil, message)
	})
	kitutil.SetSystemErrorLogging(common.SysError)

	// MUST happen before InitEnv(). common.LogDir is a flag whose default is
	// "./logs" -- a flag, not an environment variable, so no amount of platform
	// configuration can change it. InitEnv() resolves it with filepath.Abs and
	// then os.Mkdir()s it, calling log.Fatal(err) on failure. A function bundle
	// is mounted read-only at /var/task, so leaving this alone means every cold
	// start dies with
	//
	//	mkdir /var/task/logs: read-only file system
	//
	// and log.Fatal's os.Exit(1) kills the process before any handler can
	// respond -- the caller sees an opaque FUNCTION_INVOCATION_FAILED with no
	// body instead of the JSON error this package tries hard to return.
	//
	// Clearing it disables file logging, which is the right behaviour here: the
	// platform already collects stdout/stderr, and a log file on an ephemeral
	// per-instance disk could never be read back. logger.SetupLogger() skips
	// file setup when LogDir is empty, and controller.getLogFiles() returns no
	// files rather than erroring, so the admin log viewer degrades cleanly.
	*common.LogDir = ""

	// Also MUST happen before InitEnv(), which is where common.IsMasterNode is
	// derived from NODE_TYPE and where InitSessionCookieSettings() reads the
	// SESSION_COOKIE_* variables.
	applyServerlessDefaults()

	// Note: no godotenv.Load here. A function runtime injects configuration as
	// real environment variables and the deployment bundle has no .env file.
	common.InitEnv()
	logger.SetupLogger()

	envDone = true
}

// initResources mirrors InitResources() in main.go, minus everything that only
// makes sense in a long-lived process. Caller must hold initMu.
func initResources() error {
	if resourcesDone {
		return nil
	}

	initEnvOnce()

	if err := validateServerlessConfig(); err != nil {
		return err
	}

	ratio_setting.InitRatioSettings()
	service.InitHttpClient()
	service.InitTokenEncoders()

	if model.DB == nil {
		if err := model.InitDB(); err != nil {
			return fmt.Errorf("failed to initialize database: %w", err)
		}
	}
	if err := authz.Init(model.DB); err != nil {
		return fmt.Errorf("failed to initialize authorization: %w", err)
	}

	model.CheckSetup()
	model.InitOptionMap()

	if model.LOG_DB == nil {
		if err := model.InitLogDB(); err != nil {
			return fmt.Errorf("failed to initialize log database: %w", err)
		}
	}
	if err := common.InitRedisClient(); err != nil {
		return fmt.Errorf("failed to initialize redis: %w", err)
	}

	perfmetrics.Init()

	if err := i18n.Init(); err != nil {
		// Matches main.go: i18n is not critical enough to abort startup.
		common.SysError("failed to initialize i18n: " + err.Error())
	} else {
		common.SysLog("i18n initialized with languages: " + strings.Join(i18n.SupportedLanguages(), ", "))
	}
	i18n.SetUserLangLoader(model.GetUserLanguage)

	if err := oauth.LoadCustomProviders(); err != nil {
		common.SysError("failed to load custom OAuth providers: " + err.Error())
	}

	kitutil.Debug.Store(common.DebugEnabled)

	if common.RedisEnabled {
		// Kept for parity with main.go's backwards-compatibility behaviour.
		common.MemoryCacheEnabled = true
	}
	if common.MemoryCacheEnabled {
		// Same panic-recover-and-repair dance as main.go. Note that
		// model.SyncChannelCache is deliberately NOT started: a frozen instance
		// cannot refresh anything, so the cache is simply rebuilt on each cold
		// start. See the staleness note in docs/VERCEL.md.
		func() {
			defer func() {
				if r := recover(); r != nil {
					common.SysLog(fmt.Sprintf("InitChannelCache panic: %v, attempting repair", r))
					if _, _, fixErr := model.FixAbility(); fixErr != nil {
						common.SysError(fmt.Sprintf("InitChannelCache repair failed: %s", fixErr.Error()))
					}
				}
			}()
			model.InitChannelCache()
		}()
	}

	// Warm pricing so Advanced Custom endpoint inference can read cached route
	// settings on the very first relay request handled by this instance.
	model.GetPricing()

	// Breaks the service -> relay import cycle. MUST be set before any async
	// task polling runs, because RunTaskPollingOnce depends on it.
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor {
		adaptor := relay.GetTaskAdaptor(platform)
		if adaptor == nil {
			return nil
		}
		return adaptor
	}

	// Registers the channel test / model update / Midjourney poll / async task
	// poll handlers. Registration is just a map write, so it is safe and cheap
	// here even though the runner loop is never started.
	controller.RegisterScheduledSystemTasks()

	resourcesDone = true
	return nil
}

// applyServerlessDefaults fills in configuration that this runtime can work out
// for itself. Anything set explicitly by the operator wins.
//
// Caller must invoke this BEFORE common.InitEnv(), which reads NODE_TYPE and
// the SESSION_COOKIE_* variables.
func applyServerlessDefaults() {
	// Not "default to slave" -- FORCE slave. A master node here is always a
	// misconfiguration: it would start daemon goroutines that the platform
	// freezes mid-work, and it would send router.SetRouter into SetWebRouter,
	// which reads a web/dist embed that this build does not contain. See the
	// package comment.
	_ = os.Setenv("NODE_TYPE", "slave")

	applyFrontendBaseURLDefault()

	// Ordered after the frontend default on purpose: it reuses the resolved
	// FRONTEND_BASE_URL as the trusted cookie origin.
	applySessionCookieDefaults()
}

// applyFrontendBaseURLDefault derives the site URL from the platform when the
// operator has not supplied one.
func applyFrontendBaseURLDefault() {
	if strings.TrimSpace(os.Getenv("FRONTEND_BASE_URL")) != "" {
		return
	}

	// VERCEL_PROJECT_PRODUCTION_URL is the project's canonical production
	// hostname and is populated even on preview deployments, so links baked into
	// e-mails and OAuth redirects keep pointing somewhere durable. VERCEL_URL is
	// the per-deployment hostname and is the fallback when no production alias
	// exists yet. Neither includes a scheme.
	for _, key := range []string{"VERCEL_PROJECT_PRODUCTION_URL", "VERCEL_URL"} {
		if host := strings.TrimSpace(os.Getenv(key)); host != "" {
			_ = os.Setenv("FRONTEND_BASE_URL", "https://"+host)
			common.SysLog("serverless: derived FRONTEND_BASE_URL from " + key)
			return
		}
	}
}

// applySessionCookieDefaults turns on secure session cookies, which upstream
// leaves off.
//
// The default matters for more than the cookie's Secure attribute.
// middleware.SessionCookieOriginGuard short-circuits with c.Next() when
// common.SessionCookieSecure is false, so with the upstream default the
// Origin/Referer check on the cookie-authenticated /api/user/auth/refresh and
// /api/user/auth/logout endpoints does not run at all. Upstream defaults to off
// for local http development; a function platform terminates TLS for us and has
// no plaintext mode, so that trade-off does not apply.
//
// Enabling it requires a trusted origin in the SAME pass: with
// SESSION_COOKIE_SECURE=true and SESSION_COOKIE_TRUSTED_URL empty,
// common.InitSessionCookieSettings() returns an error and common.InitEnv turns
// that into log.Fatal -- another opaque FUNCTION_INVOCATION_FAILED with no
// response body, exactly like the read-only LogDir crash. So the flag is only
// set when an https origin is actually available to pair with it.
func applySessionCookieDefaults() {
	secure := strings.TrimSpace(os.Getenv("SESSION_COOKIE_SECURE"))

	// An explicit opt-out is honoured. It is a legitimate choice when this
	// deployment sits behind a proxy that terminates TLS somewhere else.
	if strings.EqualFold(secure, "false") {
		return
	}

	trusted := strings.TrimSpace(os.Getenv("SESSION_COOKIE_TRUSTED_URL"))
	origin := httpsOriginOf(os.Getenv("FRONTEND_BASE_URL"))

	if origin == "" && trusted == "" {
		// Nothing to pair the flag with. Setting it anyway would abort the
		// process instead of degrading, so leave upstream's default in place.
		// Unreachable in practice: validateServerlessConfig already refuses to
		// serve without a resolvable FRONTEND_BASE_URL.
		if strings.EqualFold(secure, "true") {
			common.SysError(
				"serverless: SESSION_COOKIE_SECURE=true requires SESSION_COOKIE_TRUSTED_URL, " +
					"and no https origin could be derived from FRONTEND_BASE_URL to supply one",
			)
		}
		return
	}

	if trusted == "" {
		_ = os.Setenv("SESSION_COOKIE_TRUSTED_URL", origin)
		trusted = origin
	}
	if secure == "" {
		_ = os.Setenv("SESSION_COOKIE_SECURE", "true")
		common.SysLog(
			"serverless: enabled SESSION_COOKIE_SECURE with trusted origin " + trusted +
				" (also re-enables the auth-endpoint Origin check)",
		)
	}
}

// httpsOriginOf reduces a site URL to the scheme-and-host-only https origin
// that common.NormalizeOrigin will accept, or "" if it cannot.
//
// Rejecting non-https is deliberate rather than defensive: NormalizeOrigin is
// happy with http, but InitSessionCookieSettings then requires every trusted URL
// to start with https://, so passing an http origin through would trade a
// working insecure default for a fatal error.
func httpsOriginOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	// Drop any path/query an operator appended; NormalizeOrigin rejects those.
	// Case and default ports are normalised there.
	return "https://" + parsed.Host
}

// validateServerlessConfig fails fast, with an explanation, on configuration
// that would otherwise break confusingly further downstream.
//
// The NODE_TYPE and FRONTEND_BASE_URL checks are assertions rather than
// requirements: applyServerlessDefaults has already handled both, so they
// should be unreachable. They stay so that a future change to the default logic
// fails here, saying why, instead of panicking inside router.SetRouter.
func validateServerlessConfig() error {
	if common.IsMasterNode {
		return fmt.Errorf(
			"serverless: NODE_TYPE resolved to master (env %q) despite being forced to slave. "+
				"A master node starts background goroutines that a function runtime freezes mid-work, "+
				"and makes router.SetRouter serve the embedded frontend, which is not bundled here",
			os.Getenv("NODE_TYPE"),
		)
	}
	if strings.TrimSpace(os.Getenv("FRONTEND_BASE_URL")) == "" {
		return fmt.Errorf(
			"serverless: FRONTEND_BASE_URL is empty and could not be derived from the platform " +
				"(VERCEL_PROJECT_PRODUCTION_URL and VERCEL_URL were both unset). While it is empty, " +
				"router.SetRouter calls SetWebRouter, which reads the embedded web/dist filesystem " +
				"that this build intentionally omits. Set FRONTEND_BASE_URL to your site URL",
		)
	}
	if strings.TrimSpace(os.Getenv("SQL_DSN")) == "" {
		return fmt.Errorf(
			"serverless: SQL_DSN must be set to a MySQL or PostgreSQL connection string. " +
				"This is the one setting that cannot be defaulted. Without it New API falls back to " +
				"SQLite in a local file, which cannot work in a function runtime: the deployment " +
				"bundle is mounted read-only, and /tmp is per-instance and discarded when the " +
				"instance is recycled, so each concurrent instance would see a different empty " +
				"database and all data would be lost. Provision a hosted database, set SQL_DSN to " +
				"its connection string, then create the schema by calling the cron endpoint with " +
				"?job=migrate",
		)
	}
	// Checked here rather than left to upstream's default because the default is
	// process-scoped and this runtime has many processes. common.InitEnv only
	// rejects the literal placeholder "random_string"; absent entirely, it is
	// silently accepted.
	if strings.TrimSpace(os.Getenv("SESSION_SECRET")) == "" {
		return fmt.Errorf(
			"serverless: SESSION_SECRET must be set to a long random string. Left unset, " +
				"common.SessionSecret falls back to uuid.New().String(), which is evaluated once " +
				"per PROCESS. A daemon has exactly one process, so upstream can treat that as " +
				"'a restart signs everybody out'. A function runtime serves requests from many " +
				"concurrent instances, each with its own value, and the resulting breakage is " +
				"silent rather than loud: sessions and access tokens issued by one instance are " +
				"rejected by the next, so users see intermittent logouts and login loops with no " +
				"error logged anywhere. CRYPTO_SECRET also defaults to this value, and " +
				"common.GenerateHMAC keys HMAC-SHA256 with it, so HMACs written by one instance " +
				"do not verify on another. Set SESSION_SECRET to a fixed random string (and " +
				"CRYPTO_SECRET too if you want them to differ). Note that the literal value " +
				"'random_string' is rejected by common.InitEnv with log.Fatal",
		)
	}
	return nil
}

// buildEngine mirrors the engine setup in main.go. Caller must hold initMu.
func buildEngine() (*gin.Engine, error) {
	if os.Getenv("GIN_MODE") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}

	server := gin.New()

	if err := middleware.ConfigureTrustedProxies(server); err != nil {
		return nil, fmt.Errorf("failed to configure trusted proxies: %w", err)
	}

	server.Use(gin.CustomRecovery(func(c *gin.Context, err any) {
		common.SysLog(fmt.Sprintf("panic detected: %v", err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("Panic detected, error: %v. Please submit a issue here: https://github.com/Calcium-Ion/new-api", err),
				"type":    "new_api_panic",
			},
		})
	}))

	// gzip stays disabled, exactly as in main.go, where it is commented out with
	// "This will cause SSE not to work!!!". Streaming relay responses depend on
	// unbuffered writes.
	server.Use(middleware.RequestId())
	server.Use(middleware.Version())
	server.Use(middleware.I18n())
	middleware.SetUpLogger(server)

	// A zero-valued WebAssets is safe here ONLY because applyServerlessDefaults
	// guarantees slave + non-empty FRONTEND_BASE_URL, which makes SetRouter take
	// its redirect branch and never call SetWebRouter.
	router.SetRouter(server, router.WebAssets{})

	// SetRouter's redirect branch installs a NoRoute that 301s everything to
	// FRONTEND_BASE_URL. On a single-domain deployment, where FRONTEND_BASE_URL
	// IS this origin, an unmatched /api/... path would redirect to itself and
	// loop. Re-registering NoRoute replaces that handler with an honest 404.
	server.NoRoute(func(c *gin.Context) {
		c.Set(middleware.RouteTagKey, "web")
		c.JSON(http.StatusNotFound, gin.H{
			"error": gin.H{
				"message": "path not found: " + c.Request.URL.Path,
				"type":    "new_api_not_found",
			},
		})
	})

	return server, nil
}
