package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

// laneWorkerRoute is one /api/worker route an isolated-lane worker may call, as the method and
// the chi pattern RELATIVE to the /api/worker mount (the path mountWorkerRoutes registers).
type laneWorkerRoute struct {
	method  string
	pattern string
}

// laneWorkerAllowlist is the complete set of /api/worker routes an isolated-lane worker
// (workers.isolated_lane, PRD #1906 M5, D-D) may call. Every other worker route answers it 403,
// including any route added later, which is refused until it is listed here on purpose.
//
// The list is exactly what a lane pod's agent uses: the worker lifecycle (agent/src/worker.ts:
// register, heartbeat and the claim loop) and what agent/src/isolated-runner.ts reaches through
// its imports:
//   - the run state report and the message batch (isolated-runner.ts reportState, batcher.ts
//     postMessages, and terminal-resolve.ts postTerminalState, which sends both);
//   - the message-gap read terminal-resolve.ts fills a fenced terminal report from;
//   - the steering input poll and its receipts (steering.ts ChatSteering: getInputs, ackInputs,
//     applyInputs), which is how a cancel reaches a research run;
//   - the three job routes a profile-bound job needs (PRD #1976): the result post, the input
//     file download and the output file upload. Each handler fences itself to job runs held by
//     the calling worker, so a lane worker reaches them only for its own job.
//
// Deliberately NOT listed: agent memory, every forge read and write, the judge trace and
// review routes, the task review, the chat-agent reads of the owner's other runs, proposals,
// findings, summaries, PR descriptions, checkpoint publish, the recovery archive and hold
// routes, the Codex bridge, the completion interlock, the wall park, the follow-up history,
// the orphan classification and the ownership probe, the discarded-input receipt (only
// the plan-gate SteeringChannel sends it), and the inclusion receipt (only a follow-up reaches an
// executor prompt, and a lane run takes none). A profile-bound run needs none of them, and several
// return third-party content (Decision 5).
var laneWorkerAllowlist = []laneWorkerRoute{
	{http.MethodPost, "/register"},
	{http.MethodPost, "/heartbeat"},
	{http.MethodPost, "/runs/claim"},
	{http.MethodPost, "/runs/{id}/state"},
	{http.MethodPost, "/runs/{id}/messages"},
	{http.MethodGet, "/runs/{id}/message-gaps"},
	{http.MethodGet, "/runs/{id}/inputs"},
	{http.MethodPost, "/runs/{id}/inputs/ack"},
	{http.MethodPost, "/runs/{id}/inputs/applied"},
	{http.MethodPost, "/runs/{id}/job-result"},
	{http.MethodGet, "/runs/{id}/files/{fileID}"},
	{http.MethodPost, "/runs/{id}/files"},
}

// laneAllowMux matches a request path against laneWorkerAllowlist with chi's own router, so
// the allowlist is matched by route pattern exactly as the real router matches it, with no
// hand-written path parsing. Its handlers are never invoked.
var laneAllowMux = func() *chi.Mux {
	m := chi.NewMux()
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, rt := range laneWorkerAllowlist {
		m.Method(rt.method, rt.pattern, noop)
	}
	return m
}()

// laneRouteAllowed reports whether method + path (relative to the /api/worker mount) is on
// the lane allowlist. Anything the allowlist does not match, a new route included, is false.
func laneRouteAllowed(method, path string) bool {
	return laneAllowMux.Match(chi.NewRouteContext(), method, path)
}

// laneWorkerRouteGuard is the D-D route allowlist for isolated-lane workers, mounted right
// after RequireWorker on the /api/worker group. A worker whose row has isolated_lane = false
// passes through untouched. A lane worker reaches only the laneWorkerAllowlist routes; every
// other worker route answers 403 before its handler runs. The chat claim lane (POST
// /runs/claim?lane=chat) answers 204, idle, the same as a chat queue with nothing to claim:
// the lane never runs a chat (ClaimChatRun also excludes every ephemeral worker), and the
// agent's chat loop polls it unconditionally, so a refusal would only turn into log noise.
//
// The lane marker comes from the worker row RequireWorker loaded, which only the ephemeral
// provisioner writes. The path is the one the /api/worker sub-router is routing (chi's
// RoutePath), which is exactly what the allowlist patterns are written against.
func laneWorkerRouteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wkr, ok := mw.WorkerFromContext(r.Context())
		if !ok || !wkr.IsolatedLane {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.Path
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePath != "" {
			path = rctx.RoutePath
		}
		if !laneRouteAllowed(r.Method, path) {
			httpx.Error(w, http.StatusForbidden, "this route is not available to an isolated-lane worker")
			return
		}
		if r.Method == http.MethodPost && path == "/runs/claim" && r.URL.Query().Get("lane") == "chat" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
