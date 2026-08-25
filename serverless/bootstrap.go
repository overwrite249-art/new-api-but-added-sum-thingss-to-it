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
//
// SQL_DSN is the one setting that cannot be guessed; see
// validateServerlessConfig for why the SQLite fallback cannot work here.
//
// DATABASE MIGRATIONS ARE NOT RUN HERE
// ------------------------------------
// Slave nodes intentionally skip schema migration. Run migrations once against
// the same database before the first serverless deploy (easiest: boot the
// normal Docker image against that database, let it migrate, then shut it
// down). Deploying this package against an unmigrated database will fail at
// query time, not at boot.
package serverless

import (
	"fmt"
	"net/http"
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

// initResources mirrors InitResources() in main.go, minus everything that only
// makes sense in a long-lived process. Caller must hold initMu.
func initResources() error {
	if resourcesDone {
		return nil
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
	// derived from NODE_TYPE.
	applyServerlessDefaults()

	// Note: no godotenv.Load here. A function runtime injects configuration as
	// real environment variables and the deployment bundle has no .env file.
	common.InitEnv()
	logger.SetupLogger()

	if err := validateServerlessConfig(); err != nil {
		return err
	}

	ratio_setting.InitRatioSettings()
	service.InitHttpClient()
	service.InitTokenEncoders()

	if err := model.InitDB(); err != nil {
		return fmt.Errorf("failed to initialize database: %w", err)
	}
	if err := authz.Init(model.DB); err != nil {
		return fmt.Errorf("failed to initialize authorization: %w", err)
	}

	model.CheckSetup()
	model.InitOptionMap()

	if err := model.InitLogDB(); err != nil {
		return fmt.Errorf("failed to initialize log database: %w", err)
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
// Caller must invoke this BEFORE common.InitEnv(), which reads NODE_TYPE.
func applyServerlessDefaults() {
	// Not "default to slave" -- FORCE slave. A master node here is always a
	// misconfiguration: it would start daemon goroutines that the platform
	// freezes mid-work, and it would send router.SetRouter into SetWebRouter,
	// which reads a web/dist embed that this build does not contain. See the
	// package comment.
	_ = os.Setenv("NODE_TYPE", "slave")

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
				"database and all data would be lost. Provision a hosted database, run migrations " +
				"against it once (a normal container with NODE_TYPE=master does this on boot), then " +
				"set SQL_DSN here",
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
