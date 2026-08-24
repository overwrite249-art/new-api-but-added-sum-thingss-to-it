// Vercel Function entrypoint for the entire New API HTTP surface.
//
// This is a CATCH-ALL, not one function per route. That is a deliberate
// decision, and the reasoning matters:
//
// New API's routes are constructed imperatively by router.SetRouter, which wires
// hundreds of handlers plus per-group middleware chains (auth, rate limiting,
// i18n, request IDs, distributed tokens) onto a single *gin.Engine. Splitting
// that into file-per-route Vercel functions would mean re-deriving every
// middleware chain by hand for every endpoint, and any upstream change to a
// route group would silently desynchronise. Keeping one Gin engine behind one
// function preserves the upstream routing tree byte-for-byte, which is what
// "same structure as new-api" actually requires.
//
// The cost is that this function is invoked for every API request, so its cold
// start is the cold start of the whole API. That is why serverless.Engine()
// caches aggressively per instance.
//
// BUILD NOTE
// ----------
// Vercel's Go runtime requires each entrypoint to be `package main` exposing an
// exported http.HandlerFunc, and it injects the actual func main() at build
// time. Consequently `go build ./...` and `go vet ./...` will report
// "function main is undeclared in the main package" for ./api/... . That is
// expected. Exclude ./api/... from local builds and CI, e.g.:
//
//	go build $(go list ./... | grep -v '/api')
//
// The non-Vercel entrypoint (main.go at the repo root) is untouched, so Docker
// and bare-metal builds are unaffected.
package main

import (
	"fmt"
	"net/http"

	"github.com/QuantumNous/new-api/serverless"
)

// Handler is the Vercel Function entrypoint.
func Handler(w http.ResponseWriter, r *http.Request) {
	engine, err := serverless.Engine()
	if err != nil {
		// Surface init failures as JSON in the shape API clients already parse,
		// instead of an opaque platform-level 500 with no body. Cold-start
		// failures are almost always configuration (unset SQL_DSN, NODE_TYPE
		// left as master, unreachable database), so the message is the fastest
		// path to a fix.
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(
			w,
			"{\"error\":{\"message\":%q,\"type\":\"new_api_init_failed\"}}",
			"serverless initialization failed: "+err.Error(),
		)
		return
	}

	engine.ServeHTTP(w, r)
}
