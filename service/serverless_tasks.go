package service

import (
	"context"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
)

// Serverless (Vercel) single-pass task entrypoints.
//
// WHY THIS FILE EXISTS
// --------------------
// main.go drives all periodic work with long-lived background goroutines
// (StartSystemTaskRunner, StartSubscriptionQuotaResetTask,
// StartCodexCredentialAutoRefreshTask, ...). None of that survives on a
// serverless platform:
//
//   - A function instance only executes while it is handling a request. Any
//     goroutine started during a request is frozen or killed once the response
//     is written, so a `for range ticker.C` loop never gets a second tick.
//   - Every one of those Start* functions returns immediately unless
//     common.IsMasterNode, and a serverless deployment must run as
//     NODE_TYPE=slave so the goroutines stay off in the first place.
//
// So instead of "start a loop and return", each function here performs exactly
// ONE pass and blocks until it is done. Vercel Cron (or any external scheduler,
// or `curl` from a cron box) calls the endpoint that wraps them.
//
// TWO DELIBERATE DIFFERENCES FROM THE GOROUTINE VERSIONS
// ------------------------------------------------------
//  1. Nothing here checks common.IsMasterNode. The serverless deployment runs
//     as a slave so the daemon loops stay disabled, but the cron endpoint still
//     has to be allowed to perform the master-side work. Mutual exclusion is
//     NOT lost: the system task framework already dedups via a DB lease
//     (active_key unique index + per-type lock), which is exactly what makes
//     this safe to drive from N concurrent stateless invocations.
//  2. Claimed tasks run SYNCHRONOUSLY. runSystemTaskClaimPass dispatches via
//     gopool.Go, which is right for a daemon and wrong here: the handler would
//     still be running when the function returned, and the platform would
//     freeze or kill the instance mid-task, leaving the row RUNNING until its
//     lease expired.
//
// KNOWN LIMITATION
// ----------------
// midjourneyPollHandler and asyncTaskPollHandler declare Interval() of 15s.
// Platform cron granularity is coarser (typically 1 minute at best), so async
// image/video task completion is detected more slowly than on a real server.
// This is a latency regression, not a correctness one: the polling is
// idempotent and Enabled() already returns false when nothing is in flight.

// RunSystemTaskPassOnce performs one complete system-task pass and blocks until
// it finishes:
//
//  1. expire leases abandoned by instances that were killed mid-task,
//  2. let the scheduler create rows for any scheduled handler that is due,
//  3. claim and execute at most one pending task per registered type.
//
// Handlers must already be registered (controller.RegisterScheduledSystemTasks)
// and, for async task polling, GetTaskAdaptorFunc must already be wired.
//
// runnerID identifies the lease holder; pass "" to generate one.
func RunSystemTaskPassOnce(ctx context.Context, runnerID string) {
	if runnerID == "" {
		runnerID = fmt.Sprintf("serverless-%s", common.GetRandomString(8))
	}

	// A serverless instance can be killed at any point, including while holding
	// a task lock, so reclaiming expired leases is more important here than on a
	// long-lived server.
	if err := model.ExpireStaleSystemTaskLocks(common.GetTimestamp()); err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("serverless: stale system task lock cleanup failed: %v", err))
	}

	runSystemTaskScheduler()
	runSystemTaskClaimPassSync(ctx, runnerID)
}

// runSystemTaskClaimPassSync mirrors runSystemTaskClaimPass but executes each
// claimed task inline rather than handing it to gopool, so the caller can block
// until the work is genuinely complete.
//
// Tasks are still run through runWithLeaseHeartbeat: the heartbeat renews the
// per-type lock while the handler works and cancels the handler ctx if the lock
// is lost, which is what lets a long channel test outlive the 60s lease TTL.
func runSystemTaskClaimPassSync(ctx context.Context, runnerID string) {
	handlers := registeredSystemTaskHandlers()
	if len(handlers) == 0 {
		logger.LogWarn(ctx, "serverless: no system task handlers registered; did you call controller.RegisterScheduledSystemTasks()?")
		return
	}

	taskTypes := make([]string, 0, len(handlers))
	for _, handler := range handlers {
		taskTypes = append(taskTypes, handler.Type())
	}

	pendingTasks, err := model.FindEarliestPendingSystemTasks(taskTypes)
	if err != nil {
		logger.LogWarn(ctx, fmt.Sprintf("serverless: system task query failed: %v", err))
		return
	}

	for _, handler := range handlers {
		task := pendingTasks[handler.Type()]
		if task == nil {
			continue
		}

		claimedTask, claimed, err := model.ClaimSystemTask(task.ID, handler.Type(), runnerID, systemTaskLockUntil())
		if err != nil {
			logger.LogWarn(ctx, fmt.Sprintf("serverless: system task claim failed: type=%s err=%v", handler.Type(), err))
			continue
		}
		if !claimed {
			// Another invocation already owns this type for now.
			continue
		}

		runHandler := handler
		runTask := claimedTask
		runWithLeaseHeartbeat(runTask, runnerID, func(runCtx context.Context) {
			runHandler.Run(runCtx, runTask, runnerID)
		})
	}
}

// RunSubscriptionMaintenanceOnce performs one subscription expire + quota reset
// pass, replacing the StartSubscriptionQuotaResetTask goroutine (1m tick).
//
// Safe to call concurrently: guarded by an in-process CAS and, more importantly,
// batched DB updates that only touch rows that are actually due.
func RunSubscriptionMaintenanceOnce() {
	runSubscriptionQuotaResetOnce()
}

// RunCodexCredentialRefreshOnce performs one Codex OAuth credential refresh
// pass, replacing the StartCodexCredentialAutoRefreshTask goroutine (10m tick).
//
// Only refreshes credentials expiring within the built-in 24h threshold, so
// running it more often than necessary is cheap and idempotent.
func RunCodexCredentialRefreshOnce() {
	runCodexCredentialAutoRefreshOnce()
}
