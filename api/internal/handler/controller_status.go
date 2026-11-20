package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/hostedsvc"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
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
// failing the report — one bad row must never blind the whole fleet. The optional
// pacing diagnostic and roll marker are weaker still: a malformed one is discarded
// (see decodeOptionalPacing) and the row's health is recorded regardless, so a
// discarded pacing is treated as absent (a fleet report clears stored pacing; a
// readiness or legacy report preserves it).
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
	var req statusEnvelope
	var raw json.RawMessage
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxStatusBodyBytes))
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(raw) != 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	var wire struct {
		Workers []json.RawMessage `json:"workers"`
	}
	// The typed decode above already validated the ordinary report envelope.
	_ = json.Unmarshal(raw, &wire)
	if req.Kind != "" && req.Kind != "fleet" && req.Kind != "readiness" {
		httpx.Error(w, http.StatusBadRequest, "invalid report kind")
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

	// Advance the fleet-INDEPENDENT controller-report singleton (PRD #1484 M2) on the
	// unconditional path, so EVERY report — including a zero-worker one that upserts no
	// worker_upgrade_reports row — leaves a trace of "the controller is still reporting".
	// It is the only signal the controller.report health check can read when there are no
	// hosted worker rows to stamp; gating it on len(req.Workers) would blind that check
	// for a fleet at zero capacity, which is exactly when it matters most.
	//
	// BEST-EFFORT, not fatal: a failed singleton write is logged and the report still
	// answers 204. The write is display-only, and failing the whole controller report over
	// it would be strictly worse than a briefly-stale singleton (the check degrades to
	// unknown/danger on staleness on its own). Guarded on h.q because the struct-literal
	// test handlers wire no *store.Queries; production always sets it (mirrors
	// reportedRunsByWorker's nil-store posture).
	if h.q != nil {
		if err := h.q.UpsertControllerReport(r.Context(), pgconv.Time(observedAt)); err != nil {
			slog.Error("controller status: advancing controller-report singleton", "error", err)
		}
		// PRD #4205: record the hibernation protocol version and poll interval beside it
		// (version 0 is an honest "this controller does not speak it"). PRD #4213 adds the
		// admin-size version to the same write. Best-effort like
		// the write above; admission refuses on a missing or stale advertisement.
		// Only a fleet report speaks for the advertisement: a readiness report carries
		// Hibernation 0 by construction and must not overwrite a fleet report's value.
		if req.Kind != "" && req.Kind != "fleet" {
			// readiness: leave the stored advertisement untouched
		} else if err := h.q.UpsertControllerHibernationAdvertisement(r.Context(), store.UpsertControllerHibernationAdvertisementParams{
			AdvertisedAt:        pgconv.Time(observedAt),
			HibernationVersion:  pgtype.Int4{Int32: clampInt32(req.Hibernation), Valid: true},
			PollIntervalSeconds: pgtype.Int4{Int32: clampInt32(req.PollIntervalSeconds), Valid: true},
			// PRD #4213: the admin-size support version rides the same fleet-only upsert (0
			// means the controller ignores DesiredWorker.Resources), stamped with the same
			// receipt clock, so it is never advanced by a readiness report either.
			AdminSizesVersion: pgtype.Int4{Int32: clampInt32(req.AdminSizes), Valid: true},
		}); err != nil {
			slog.Error("controller status: recording hibernation advertisement", "error", err)
		}
	}

	var accepted, dropped int
	for _, row := range wire.Workers {
		// Decode independently: repeated workers arrays may reuse struct elements
		// during the envelope decode, retaining an earlier row's identity or fields.
		var ws statusRow
		if err := json.Unmarshal(row, &ws); err != nil {
			dropped++
			continue
		}
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
		// Pacing is an optional diagnostic: a malformed one is discarded (the row's
		// health and marker still land) and an unusable identifier is omitted alone.
		pacing, pacingReason := decodeOptionalPacing(ws.Pacing)
		if pacingReason != "" {
			slog.Warn("controller status: ignoring invalid optional pacing diagnostic",
				"worker_id", id.String(), "reason", pacingReason)
		}
		// The marker is display-only: an invalid one is logged (without its raw value)
		// and discarded, never a reason to drop the entry.
		marker, err := workersvc.ParseRollMarkerReport(ws.RollMarker)
		if err != nil {
			slog.Warn("controller status: ignoring invalid roll marker", "worker_id", ws.ID, "error", err)
			marker = nil
		}
		n, err := h.wsvc.RecordRollHealth(r.Context(), workersvc.RollHealthReport{
			BlockingKind:         rawBlockingKind(ws.WorkerStatus, row),
			Kind:                 req.Kind,
			Pacing:               pacing,
			RollMarker:           marker,
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

// statusRow is one wire worker entry whose optional pacing diagnostic stays raw. The
// shallower Pacing field shadows the embedded hostedsvc.WorkerStatus.Pacing under
// encoding/json's field-resolution rules, so a mistyped pacing value can fail neither
// the envelope decode nor the row decode; decodeOptionalPacing interprets it.
type statusRow struct {
	hostedsvc.WorkerStatus
	Pacing json.RawMessage `json:"pacing"`
}

// statusEnvelope is the report envelope with raw-pacing rows.
type statusEnvelope struct {
	hostedsvc.StatusReport
	Workers []statusRow `json:"workers"`
}

// decodeOptionalPacing interprets an optional pacing diagnostic leniently. Absent or
// null is nil with no reason. A wrongly shaped value yields nil and
// "invalid_pacing_shape"; a worker identifier that is not a string holding a UUID is
// omitted alone (siblings kept) with "invalid_canary_id" or
// "invalid_blocking_worker_id" (comma-joined when both apply); content the strict
// workersvc.NormalizePacingReport refuses yields nil and "invalid_pacing_content".
// Reasons are fixed tokens, never controller-supplied text.
func decodeOptionalPacing(raw json.RawMessage) (*workersvc.PacingReport, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, ""
	}
	// encoding/json silently replaces invalid UTF-8 with U+FFFD, which would hide the
	// corruption from NormalizePacingReport's utf8.ValidString bound; check the raw
	// bytes first so corrupt pacing is discarded rather than repaired.
	if !utf8.Valid(raw) {
		return nil, "invalid_pacing_content"
	}
	var wire struct {
		State            string          `json:"state"`
		TargetImage      string          `json:"target_image"`
		TargetVersion    string          `json:"target_version"`
		CanaryID         json.RawMessage `json:"canary_id"`
		BlockingWorkerID json.RawMessage `json:"blocking_worker_id"`
		BlockingReason   *string         `json:"blocking_reason"`
		RetainedImage    *string         `json:"retained_image"`
		RetainedVersion  *string         `json:"retained_version"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, "invalid_pacing_shape"
	}
	var reasons []string
	parseID := func(v json.RawMessage, reason string) *string {
		if len(v) == 0 || string(v) == "null" {
			return nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil || !workersvc.ValidPacingWorkerID(s) {
			reasons = append(reasons, reason)
			return nil
		}
		return &s
	}
	report := workersvc.PacingReport{
		State:            wire.State,
		TargetImage:      wire.TargetImage,
		TargetVersion:    wire.TargetVersion,
		CanaryID:         parseID(wire.CanaryID, "invalid_canary_id"),
		BlockingWorkerID: parseID(wire.BlockingWorkerID, "invalid_blocking_worker_id"),
		BlockingReason:   wire.BlockingReason,
		RetainedImage:    wire.RetainedImage,
		RetainedVersion:  wire.RetainedVersion,
	}
	normalized, err := workersvc.NormalizePacingReport(&report)
	if err != nil {
		return nil, "invalid_pacing_content"
	}
	return normalized, strings.Join(reasons, ",")
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

// clampInt32 bounds a controller-supplied count to the non-negative int32 range.
func clampInt32(v int) int32 {
	if v < 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(v)
}
