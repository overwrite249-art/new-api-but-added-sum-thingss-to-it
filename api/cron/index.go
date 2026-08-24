// Vercel Cron entrypoint: performs the periodic work that main.go runs in
// background goroutines.
//
// Each invocation does ONE pass of ONE job and returns. The job is selected by
// the ?job= query parameter so a single deployed function can serve every
// schedule in vercel.json.
//
// SECURITY
// --------
// This endpoint triggers expensive work (testing every channel, refreshing
// OAuth credentials, scanning subscriptions), so it must not be publicly
// callable. Set a CRON_SECRET environment variable; Vercel then sends it as
// `Authorization: Bearer <CRON_SECRET>` on scheduled invocations, and this
// handler rejects anything else with 401 using a constant-time comparison.
//
// If CRON_SECRET is unset the handler refuses to run at all rather than
// defaulting to open access. Failing closed is the right default for an
// endpoint that can be used as an amplification vector against upstream
// providers.
//
// BUILD NOTE: see the build note in api/index.go -- `go build ./...` will
// complain about ./api/... because Vercel injects func main() at build time.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/serverless"
)

type cronResponse struct {
	Success    bool   `json:"success"`
	Job        string `json:"job"`
	DurationMs int64  `json:"duration_ms"`
	Message    string `json:"message,omitempty"`
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

	job := strings.TrimSpace(r.URL.Query().Get("job"))
	if job == "" {
		writeJSON(w, http.StatusBadRequest, cronResponse{
			Success: false,
			Message: "missing ?job= (expected one of: system-tasks, subscriptions, codex-credentials)",
		})
		return
	}

	// Cron runs need a database, loaded options and registered task handlers,
	// but no HTTP routes -- so skip building the Gin engine entirely and keep
	// this cold start cheaper than an API cold start.
	if err := serverless.EnsureResources(); err != nil {
		writeJSON(w, http.StatusInternalServerError, cronResponse{
			Success: false,
			Job:     job,
			Message: "initialization failed: " + err.Error(),
		})
		return
	}

	// Inherit the request context so that when the platform's function timeout
	// approaches and the context is cancelled, in-flight handlers observe it and
	// stop cleanly instead of being hard-killed holding a task lease.
	ctx := r.Context()
	start := time.Now()

	switch job {
	case "system-tasks":
		// Covers all four scheduled handlers registered by
		// controller.RegisterScheduledSystemTasks: channel test, upstream model
		// update, Midjourney polling and async (Suno/video) task polling.
		// Each handler's own Enabled() decides whether it is due, so this single
		// schedule is enough for all of them.
		service.RunSystemTaskPassOnce(ctx, "")

	case "subscriptions":
		service.RunSubscriptionMaintenanceOnce()

	case "codex-credentials":
		service.RunCodexCredentialRefreshOnce()

	default:
		writeJSON(w, http.StatusBadRequest, cronResponse{
			Success: false,
			Job:     job,
			Message: fmt.Sprintf("unknown job %q", job),
		})
		return
	}

	writeJSON(w, http.StatusOK, cronResponse{
		Success:    true,
		Job:        job,
		DurationMs: time.Since(start).Milliseconds(),
	})
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
