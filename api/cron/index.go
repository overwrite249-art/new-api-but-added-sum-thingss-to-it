// Vercel Cron entrypoint: performs the periodic work that main.go runs in
// background goroutines, plus the one-off schema migration that main.go runs
// while booting a master node.
//
// Each invocation runs one pass of each requested job and returns. Jobs are
// selected with the ?job= query parameter; when it is omitted, every scheduled
// job runs in sequence.
//
// WHY ALL JOBS BY DEFAULT
// -----------------------
// Vercel's Hobby plan permits only one cron invocation per day per expression
// ("Hobby accounts are limited to daily cron jobs" is a hard deploy-time
// rejection), and scheduled invocations are plain GETs against the configured
// path with no query string. A single daily hit of /api/cron therefore has to
// cover every job. ?job= is retained for targeted manual runs.
//
// THE MIGRATE JOB
// ---------------
// ?job=migrate creates or updates the database schema. It is excluded from the
// scheduled run -- DDL should not be re-applied daily -- and is reachable only
// by asking for it by name. Run it once before first use and again after an
// upgrade that changes the schema.
//
// SECURITY
// --------
// This endpoint triggers expensive work (testing every channel, refreshing
// OAuth credentials, scanning subscriptions, altering the schema), so it must
// not be publicly callable. Set a CRON_SECRET environment variable; Vercel then
// sends it as `Authorization: Bearer <CRON_SECRET>` on scheduled invocations,
// and this handler rejects anything else with 401 using a constant-time
// comparison.
//
// If CRON_SECRET is unset the handler refuses to run at all rather than
// defaulting to open access. Failing closed is the right default for an
// endpoint that can be used as an amplification vector against upstream
// providers.
//
// BUILD NOTE: see the build note in api/index.go -- this must be a library
// package (`package handler`) because Vercel generates and imports a func
// main() wrapper at build time.
package handler

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/serverless"
	"github.com/QuantumNous/new-api/service"
)

const (
	jobSystemTasks      = "system-tasks"
	jobSubscriptions    = "subscriptions"
	jobCodexCredentials = "codex-credentials"
	jobMigrate          = "migrate"
)

// allJobs is the execution order used when no ?job= is supplied.
var allJobs = []string{jobSystemTasks, jobSubscriptions, jobCodexCredentials}

// manualJobs are accepted via ?job= but deliberately kept out of the scheduled
// run. Migration is DDL: it should happen when an operator asks for it, not
// every night.
var manualJobs = []string{jobMigrate}

type cronResponse struct {
	Success    bool     `json:"success"`
	Requested  []string `json:"requested,omitempty"`
	Completed  []string `json:"completed,omitempty"`
	DurationMs int64    `json:"duration_ms"`
	Message    string   `json:"message,omitempty"`
}

// Handler is the Vercel Function entrypoint for scheduled jobs.
func Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if !authorized(r) {
		writeJSON(w, http.StatusUnauthorized, cronResponse{
			Success: false,
			Message: "unauthorized: set CRON_SECRET and send it as 'Authorization: Bearer <CRON_SECRET>'",
		})
		return
	}

	jobs := allJobs
	if requested := strings.TrimSpace(r.URL.Query().Get("job")); requested != "" {
		jobs = []string{requested}
	}

	// Validate before initialising anything: a typo should cost a 400, not a
	// database cold start.
	for _, job := range jobs {
		if !knownJob(job) {
			writeJSON(w, http.StatusBadRequest, cronResponse{
				Success:   false,
				Requested: jobs,
				Message: fmt.Sprintf(
					"unknown job %q (expected one of: %s)",
					job, strings.Join(knownJobNames(), ", "),
				),
			})
			return
		}
	}

	// Migration runs before, and instead of, full initialisation. On a database
	// with no tables yet EnsureResources fails, because model.CheckSetup and
	// model.InitOptionMap read rows that only exist once migration has run --
	// so requiring initialisation first would make this endpoint unusable in
	// exactly the situation it exists for.
	if len(jobs) == 1 && jobs[0] == jobMigrate {
		start := time.Now()

		if err := serverless.Migrate(); err != nil {
			// Reported as an error, unlike the fire-and-forget periodic jobs: a
			// half-applied schema needs to be visible now, not discovered later
			// as a confusing query failure.
			writeJSON(w, http.StatusInternalServerError, cronResponse{
				Success:    false,
				Requested:  jobs,
				DurationMs: time.Since(start).Milliseconds(),
				Message:    "migration failed: " + err.Error(),
			})
			return
		}

		writeJSON(w, http.StatusOK, cronResponse{
			Success:    true,
			Requested:  jobs,
			Completed:  jobs,
			DurationMs: time.Since(start).Milliseconds(),
			Message:    "schema is up to date; open /setup to create the first admin account",
		})
		return
	}

	// Cron runs need a database, loaded options and registered task handlers,
	// but no HTTP routes -- so skip building the Gin engine entirely and keep
	// this cold start cheaper than an API cold start.
	if err := serverless.EnsureResources(); err != nil {
		writeJSON(w, http.StatusInternalServerError, cronResponse{
			Success:   false,
			Requested: jobs,
			Message:   "initialization failed: " + err.Error(),
		})
		return
	}

	// Inherit the request context so that when the platform's function timeout
	// approaches and the context is cancelled, in-flight handlers observe it and
	// stop cleanly instead of being hard-killed holding a task lease.
	ctx := r.Context()
	start := time.Now()
	completed := make([]string, 0, len(jobs))

	for _, job := range jobs {
		// Several jobs in one invocation share a single timeout budget, so stop
		// at a cancelled context rather than starting work that cannot finish.
		if ctx.Err() != nil {
			writeJSON(w, http.StatusOK, cronResponse{
				Success:    false,
				Requested:  jobs,
				Completed:  completed,
				DurationMs: time.Since(start).Milliseconds(),
				Message:    "stopped early: " + ctx.Err().Error(),
			})
			return
		}

		runJob(ctx, job)
		completed = append(completed, job)
	}

	writeJSON(w, http.StatusOK, cronResponse{
		Success:    true,
		Requested:  jobs,
		Completed:  completed,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// knownJobNames lists every accepted ?job= value, scheduled and manual alike.
func knownJobNames() []string {
	names := make([]string, 0, len(allJobs)+len(manualJobs))
	names = append(names, allJobs...)
	names = append(names, manualJobs...)
	return names
}

func knownJob(job string) bool {
	for _, candidate := range knownJobNames() {
		if candidate == job {
			return true
		}
	}
	return false
}

// runJob performs exactly one pass of one scheduled job. Each underlying
// function is already guarded against overlapping runs (atomic bools upstream,
// plus the database task lease for system tasks), so a slow invocation that
// overlaps the next schedule is safe.
//
// jobMigrate is not handled here: it is dispatched earlier, before
// initialisation, and needs to surface its error.
func runJob(ctx context.Context, job string) {
	switch job {
	case jobSystemTasks:
		// Covers all four scheduled handlers registered by
		// controller.RegisterScheduledSystemTasks: channel test, upstream model
		// update, Midjourney polling and async (Suno/video) task polling. Each
		// handler's own Enabled() decides whether it is due.
		service.RunSystemTaskPassOnce(ctx, "")
	case jobSubscriptions:
		service.RunSubscriptionMaintenanceOnce()
	case jobCodexCredentials:
		service.RunCodexCredentialRefreshOnce()
	}
}

// authorized fails closed: an unset CRON_SECRET denies every request rather
// than allowing all of them.
func authorized(r *http.Request) bool {
	secret := strings.TrimSpace(os.Getenv("CRON_SECRET"))
	if secret == "" {
		return false
	}

	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if provided == "" {
		return false
	}

	// Constant-time to avoid leaking the secret through response timing.
	return subtle.ConstantTimeCompare([]byte(provided), []byte(secret)) == 1
}

func writeJSON(w http.ResponseWriter, status int, body cronResponse) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
