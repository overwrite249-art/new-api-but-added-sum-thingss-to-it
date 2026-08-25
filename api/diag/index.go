// Streaming diagnostic for the Vercel Go runtime.
//
// WHY THIS EXISTS
// ---------------
// New API is an AI gateway, so the overwhelming majority of its traffic is
// server-sent events relayed from upstream model providers. Whether that works
// on a function runtime depends on a platform property that is not documented
// and cannot be inferred from a successful build: does the runtime write the
// response through to the client incrementally, or does it buffer the handler's
// output and send it once the handler returns?
//
// If it buffers, streaming is not "slower" -- it is broken in a way that looks
// like the application's fault:
//
//   - every token-by-token completion arrives as one delayed blob, so clients
//     that render incrementally show nothing at all until the request ends
//   - a response that exceeds maxDuration is lost in full rather than
//     delivered in part, because nothing was ever sent
//   - keep-alive comments and heartbeats never reach the client, so proxies in
//     between are free to time the connection out
//
// No application-level change fixes that. It is a go/no-go property of the
// deployment target, so it is worth one small endpoint to establish it as fact
// instead of an assumption.
//
// WHY IT IS A SEPARATE FUNCTION
// -----------------------------
// This handler deliberately does NOT call serverless.Engine(). It touches no
// configuration, no database and no Redis, so it answers the streaming question
// while SQL_DSN is still unset -- which is exactly when you want the answer,
// before spending time provisioning infrastructure for an architecture that may
// not support the product's main use case.
//
// HOW TO READ THE RESULT
// ----------------------
// Three independent signals, so no single one has to be trusted alone:
//
//   1. flusher_implemented -- if false, streaming is impossible, full stop.
//   2. writer_type -- the concrete type behind http.ResponseWriter. A stdlib
//      type suggests a direct pass-through; a runtime-specific wrapper is a
//      strong hint that output is being collected before transmission.
//   3. written_at / elapsed_ms per chunk, compared against the response
//      headers. A streamed response must use transfer-encoding: chunked,
//      because a buffering runtime knows the total size up front and will
//      send content-length instead. That header difference is the most
//      reliable single tell, and it is observable by any HTTP client.
//
// SAFETY
// ------
// Bounded to maxChunks * maxDelayMs of wall clock so it cannot be used to hold
// a function instance open indefinitely, and it returns as soon as the client
// disconnects. It emits only timing information and a Go type name: no
// configuration values, no secrets, no database contents. Once the streaming
// behaviour is recorded in docs/VERCEL.md this file can be deleted.
package handler

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

const (
	defaultChunks  = 5
	maxChunks      = 20
	defaultDelayMs = 500
	maxDelayMs     = 1000
)

// Handler is the Vercel Function entrypoint. The exported-single-handler shape
// and `package handler` clause are required by the Go runtime's generated
// wrapper, which imports this package rather than compiling it as a command.
func Handler(w http.ResponseWriter, r *http.Request) {
	chunks := clampQueryInt(r.URL.Query().Get("chunks"), defaultChunks, 1, maxChunks)
	delayMs := clampQueryInt(r.URL.Query().Get("delay"), defaultDelayMs, 0, maxDelayMs)

	// Capture this before writing anything. If the runtime hands us a writer
	// that cannot flush, the rest of the measurement is moot.
	flusher, canFlush := w.(http.Flusher)
	writerType := fmt.Sprintf("%T", w)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Hint to any intermediate proxy not to buffer on its own account, so that a
	// buffered result can be attributed to the runtime rather than the edge.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	start := time.Now()

	fmt.Fprintf(
		w,
		"event: meta\ndata: {\"flusher_implemented\":%t,\"writer_type\":%q,\"chunks\":%d,\"delay_ms\":%d,\"started_at\":%q}\n\n",
		canFlush,
		writerType,
		chunks,
		delayMs,
		start.UTC().Format(time.RFC3339Nano),
	)
	if canFlush {
		flusher.Flush()
	}

	for i := 1; i <= chunks; i++ {
		if delayMs > 0 {
			select {
			case <-r.Context().Done():
				// Client hung up. Stop burning function time immediately.
				return
			case <-time.After(time.Duration(delayMs) * time.Millisecond):
			}
		}

		now := time.Now()
		fmt.Fprintf(
			w,
			"data: {\"chunk\":%d,\"of\":%d,\"written_at\":%q,\"elapsed_ms\":%d}\n\n",
			i,
			chunks,
			now.UTC().Format(time.RFC3339Nano),
			now.Sub(start).Milliseconds(),
		)
		if canFlush {
			flusher.Flush()
		}
	}

	fmt.Fprintf(
		w,
		"event: done\ndata: {\"total_elapsed_ms\":%d}\n\n",
		time.Since(start).Milliseconds(),
	)
	if canFlush {
		flusher.Flush()
	}
}

// clampQueryInt parses a query parameter into a bounded integer, falling back to
// def for anything absent or unparseable. Bounding matters here: this endpoint
// is unauthenticated by design (it must work before the database exists), so the
// caller must not be able to choose how long a function instance stays alive.
func clampQueryInt(raw string, def, minimum, maximum int) int {
	if raw == "" {
		return def
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
