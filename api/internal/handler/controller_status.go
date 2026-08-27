package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// maxStatusReportWorkers caps how many worker entries one report may carry.
//
// The body cap does NOT subsume this. DecodeJSON bounds the BODY at 1 MiB, and at
// roughly 120 bytes per entry a perfectly legal 1 MiB body still carries ~8700
// entries — i.e. ~8700 upserts every 10 seconds from a single authenticated caller.
// Bounding bytes is not bounding work, which is why this is its own explicit limit
// rather than a consequence of the decoder's.
//
// 512 is far above any real fleet (the hosted quota is single digits per user) and far
// below anything that matters, which is the right place for a sanity bound.
const maxStatusReportWorkers = 512

// maxStatusBodyBytes bounds the request body. Deliberately independent of the house
// DecodeJSON helper, because this route cannot use it — see the decoder comment below.
const maxStatusBodyBytes = 1 << 20

// ControllerStatus accepts the hosted-worker controller's roll-health report (PRD #113
// M4, Decision 10).
//
// DISPLAY-ONLY, and structurally so rather than by promise. What this handler may not
// touch, named so a reviewer has a checklist instead of an adjective: the register path
// and anything destroying a sealed join token; the hosted token buffer,
// workers.token_hash, workers.join_token_*; Claim, runs, run_messages; workers.status
// and last_heartbeat_at (liveness is heartbeat-owned — if a report could reach it, a
// lying controller could hold a dead worker "online", which is run-scheduling state,
// not a badge); workers.generation / hosted_generation (writable from a report means a
// self-driving loop); and any INSERT into workers. The enforcement is in the SQL: the
// upsert is an INSERT ... SELECT FROM workers WHERE kind = 'hosted', so it can only
// ever UPDATE roll-health rows for workers that already exist.
//
// Authenticated by mw.RequireController on the route group — the same fleet-scoped
// bearer credential the poll uses, so this adds no new auth mechanism, no cookie and
// no CSRF step. And it exists only when hosting is enabled: with hosting off the route
// is ABSENT, so an api that never had this feature and an api with the feature disabled
// both answer 404, which is the single behaviour the controller's skew tolerance was
// built for.
//
// Answers 204 with NO BODY. That is Decision 10 made structural: there is no response
// for the controller to read state out of, so this endpoint cannot quietly grow into a
// second desired-state channel. Per-row garbage is dropped with a warning rather than
// failing the report — one bad row must never blind the whole fleet.
func (h *Handler) ControllerStatus(w http.ResponseWriter, r *http.Request) {
	// A ROUTE-LOCAL LENIENT DECODER, and it must stay lenient.
	//
	// Both house helpers (httpx.DecodeJSON and friends) set DisallowUnknownFields,
	// which is right for browser-facing routes and WRONG here: Decision 10 requires
	// forward compatibility across controller/api version skew, and a strict decoder
	// turns a NEWER controller sending one extra field into a 400 for the entire
	// fleet's report. That is a self-inflicted outage of the observability feature at
	// exactly the moment a rollout is in flight.
	//
	// If you are here to "fix" this by using the house helper: the contract TEST does
	// set DisallowUnknownFields (drift is caught at the gate), and the production
	// decoder deliberately does not. Two decoders, opposite policies, one wire.
	var req hostedsvc.StatusReport
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStatusBodyBytes))
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Workers) > maxStatusReportWorkers {
		httpx.Error(w, http.StatusBadRequest, "too many workers in report")
		return
	}

	// The api's OWN receipt time, stamped once for the whole report so every row in one
	// report shares a freshness basis. This — never req.ReportedAt — is what freshness
	// is computed from.
	observedAt := h.clock()

	var accepted, dropped int
	for _, ws := range req.Workers {
		id, err := uuid.Parse(ws.ID)
		if err != nil {
			dropped++
			continue
		}
		// Closed enum, validated not sanitized: an unmodelled phase means the api does
		// not know what state that is, so the ENTRY is dropped. Never persist-and-render
		// a free string — this value selects a badge.
		if !workersvc.ValidRollPhase(ws.Phase) {
			dropped++
			slog.Warn("controller status: dropping entry with an unmodelled phase",
				"worker_id", ws.ID, "phase", sanitizeSelfReported(ws.Phase, maxSelfReportedBytes))
			continue
		}
		n, err := h.wsvc.RecordRollHealth(r.Context(), workersvc.RollHealthReport{
			WorkerID:             id,
			Phase:                ws.Phase,
			PhaseSince:           ws.PhaseSince,
			TargetImage:          sanitizeSelfReported(ws.TargetImage, maxSelfReportedBytes),
			PodPhase:             sanitizeSelfReported(ws.PodPhase, maxSelfReportedBytes),
			BlockingContainer:    sanitizeSelfReportedPtr(ws.BlockingContainer),
			BlockingReason:       sanitizeSelfReportedPtr(ws.BlockingReason),
			RestartCount:         ws.RestartCount,
			LastExitCode:         ws.LastExitCode,
			ControllerReportedAt: req.ReportedAt,
			ObservedAt:           observedAt,
			PollIntervalSeconds:  req.PollIntervalSeconds,
			WorkerImageTag:       sanitizeSelfReported(req.WorkerImageTag, maxSelfReportedBytes),
		})
		if err != nil {
			// One row failing is not the fleet's problem. Log and keep going; the report
			// is display-only and a partial update is strictly better than none.
			dropped++
			slog.Error("controller status: recording roll health", "worker_id", ws.ID, "error", err)
			continue
		}
		if n == 0 {
			// The upsert matched no row: an unknown id, or an EXTERNAL worker the
			// controller has no jurisdiction over. Both are dropped by the query itself
			// rather than by a check here, which is why this is a count and not a guard.
			dropped++
			continue
		}
		accepted++
	}
	if dropped > 0 {
		slog.Warn("controller status: some entries were not recorded",
			"accepted", accepted, "dropped", dropped, "reported", len(req.Workers))
	}
	// 204, no body. See the doc comment.
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeSelfReportedPtr applies the same bound-and-strip treatment to an optional
// wire string, preserving the null/empty distinction the wire carries.
//
// The sanitizer is load-bearing rather than cosmetic here: these strings become
// upgrade_detail, which the CLI prints straight into a terminal
// (api/cmd/uzi/worker.go's tabwriter row), so an unstripped terminal escape from a
// compromised controller would execute in an operator's terminal. It also truncates on
// a rune boundary, so a multi-byte name cannot be cut into invalid UTF-8.
func sanitizeSelfReportedPtr(s *string) *string {
	if s == nil {
		return nil
	}
	out := sanitizeSelfReported(*s, maxSelfReportedBytes)
	return &out
}
