package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/httpx"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workertmpl"
)

// maxSelfReportedBytes caps a worker's free-form self-reported string fields
// (version) before they reach the DB / worker-list UI. Generous for any real
// version string, tight enough to bound abuse.
const maxSelfReportedBytes = 64

// maxAdvertisedConcurrentRuns is the sanity ceiling for a worker's self-reported
// concurrency cap (PRD #42). The cap is observability only — the server enforces no
// cap — so this is a generous absurd-value guard (far above the documented soft
// ceiling of 8 and multica's daemon cap of 20), NOT a policy limit. A report outside
// [1, maxAdvertisedConcurrentRuns] is treated as unadvertised (stored NULL), so a
// hostile/garbled value can never flow into the fleet UI's "N/M runs" math.
const maxAdvertisedConcurrentRuns = 256

// sanitizeSelfReported bounds an untrusted worker-reported string: trim, drop
// control characters (so no terminal escapes reach a log or the UI), and truncate
// to max bytes (the length check runs after each whole rune is written, so it
// never splits a multi-byte rune — the cap is max..max+3 bytes). It sanitizes
// rather than rejects — these fields are observability, and a register must never
// fail over cosmetic input (it would wedge the worker's retry loop).
func sanitizeSelfReported(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	return b.String()
}

// WorkerRegister brings the worker online (and recovers any runs it orphaned by
// restarting). Accepts an optional {version} body.
func (h *Handler) WorkerRegister(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	var req struct {
		Version string `json:"version"`
		// Name is accepted for wire compatibility (the M2 worker announces both
		// name and version) but deliberately ignored: the authoritative worker
		// name is the user-chosen label set at token issuance, not something the
		// worker may overwrite. DecodeJSON rejects unknown fields, so this must be
		// declared even though nothing reads it.
		Name string `json:"name"`
		// Template is the worker's self-reported image template (PRD #18). Unlike
		// Name, it IS read + persisted (as template_reported): it is observability
		// the server surfaces and badges drift on, never an authn/authz input.
		// Optional — an older image omits it and the column stays NULL.
		Template string `json:"template"`
		// MaxConcurrentRuns is the worker's advertised concurrency cap (PRD #42
		// Decisions 3 & 10): observability the server records and the UI renders as
		// "N/M runs", never enforced server-side. Optional — an older image (and
		// every M3a worker, before the M2 agent starts sending it) omits it and the
		// column stays NULL. A pointer so absent (NULL) is distinct from a sent 0.
		MaxConcurrentRuns *int `json:"max_concurrent_runs"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// template is untrusted worker self-report bound for the DB + web UI. Bound it
	// to a tight charset (workertmpl.WellFormed) and DROP anything else to empty
	// (persisted as NULL) rather than 400 — a soft observability field must never
	// wedge a worker's register-retry loop. Membership is NOT checked here: an
	// unknown-but-well-formed name is the drift signal, not an error.
	reported := strings.TrimSpace(req.Template)
	if reported != "" && !workertmpl.WellFormed(reported) {
		slog.Warn("worker reported a malformed template; dropping", "worker_id", wkr.ID.String())
		reported = ""
	}
	// version is the sibling self-reported field from the same join-token-
	// authenticated (but untrusted) worker, bound for the DB + worker list UI.
	// Cap + strip control chars so a hostile worker can't smuggle unbounded text
	// or terminal escapes there. Sanitize (never reject) — it is observability.
	version := sanitizeSelfReported(req.Version, maxSelfReportedBytes)
	// max_concurrent_runs is the sibling self-reported cap (PRD #42). It is pure
	// observability the server never enforces AND it flows into the fleet UI's
	// "N/M runs" math, so a nonsensical report must be neither trusted nor allowed
	// to wedge the register-retry loop: accept it only within a sane
	// [1, maxAdvertisedConcurrentRuns] band, else drop it to NULL (treat as
	// unadvertised) with a warn — like a malformed template, never a 400. The worker
	// validates ≥ 1 and warns above the documented soft ceiling before sending (M2);
	// this is the server-side backstop against a hostile/garbled report.
	advertisedCap := req.MaxConcurrentRuns
	if advertisedCap != nil && (*advertisedCap < 1 || *advertisedCap > maxAdvertisedConcurrentRuns) {
		slog.Warn("worker reported an out-of-range max_concurrent_runs; dropping", "worker_id", wkr.ID.String(), "value", *advertisedCap)
		advertisedCap = nil
	}
	updated, err := h.wsvc.Register(r.Context(), wkr, version, reported, advertisedCap)
	if err != nil {
		slog.Error("worker register", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// A hosted worker registering is the PROOF that its join token reached the pod
	// (PRD #58 Decision 3): RequireWorker resolved wkr by matching sha256(the token
	// this caller presented) against workers.token_hash, so getting here means a pod
	// holds the current token and it works. That — not any report from the controller
	// — is what licenses destroying the api's sealed copy.
	//
	// Orchestrated here rather than inside workersvc on purpose: workersvc owns runs
	// and must not learn about hosted workers, and handler-level orchestration is this
	// repo's idiom. hsvc is nil unless hosting is enabled, and the Kind check keeps an
	// ordinary hand-run worker off this path entirely.
	//
	// Best-effort and NON-FATAL: this is buffer cleanup, and a worker that has already
	// registered successfully must never be failed because the cleanup did not land.
	// The TTL sweep is the backstop.
	if h.hsvc != nil && updated.Kind == "hosted" {
		if err := h.hsvc.NoteRegistered(r.Context(), updated.ID); err != nil {
			slog.Error("note hosted worker registered", "worker_id", updated.ID.String(), "error", err)
		}
	}
	// worker_id is echoed for the worker's convenience; identity on every other
	// call comes from the Bearer token, never a URL path (M2 wire contract).
	httpx.JSON(w, http.StatusOK, map[string]any{
		"worker_id": updated.ID.String(),
		"worker":    workerDTOFromWorker(updated, 0, false),
	})
}

// maxWorkerCPUPct clamps a worker's self-reported CPU percentage (PRD #49 Decision
// 5): 100% × 64 CPUs. The worker is the least-trusted component and can report
// anything; stats are display-only, so this is an absurd-value ceiling that also keeps
// a hostile 6400000% out of the DOM (the UI additionally clamps the bar to 100%), not
// a policy limit.
const maxWorkerCPUPct = 100 * 64

// WorkerHeartbeat refreshes liveness and records the worker's latest resource sample
// (PRD #49). Accepts an optional {version, stats} body.
func (h *Handler) WorkerHeartbeat(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	// Decode contract (PRD #49 Decision 3), mirroring register EXACTLY so a strict
	// decoder never bricks the fleet: declare `version` (accepted, ignored — every
	// current worker already sends {"version":...} and DisallowUnknownFields would 400
	// it otherwise, marking the whole fleet stale within the sweeper window), tolerate
	// an empty body via io.EOF, and capture `stats` as a json.RawMessage so a malformed
	// NUMBER inside it can never abort THIS decode. A literal float64 field would 400
	// the entire heartbeat on 1e999 / int64-overflow before any validation runs — one
	// bad telemetry number becoming a self-DoS. Liveness must never hinge on telemetry
	// hygiene, so the stats are parsed defensively in a second step below.
	var req struct {
		Version string          `json:"version"`
		Stats   json.RawMessage `json:"stats"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Second step: validate + clamp the isolated stats (Decision 5). A malformed or
	// invalid sample drops to nil (columns written NULL) and the heartbeat still 200s.
	stats := parseWorkerStats(req.Stats, wkr.ID)
	updated, err := h.wsvc.Heartbeat(r.Context(), wkr, stats)
	if err != nil {
		slog.Error("worker heartbeat", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"worker": workerDTOFromWorker(updated, 0, false)})
}

// parseWorkerStats is Decision 3's second-step defensive parse plus Decision 5's
// validation/clamping of the heartbeat's untrusted `stats`. It NEVER fails the
// heartbeat: an absent, malformed, or invalid sample returns nil (every stats_ column
// is written NULL) and the 200 stands. On a drop it logs worker_id + a STATIC reason
// only — never the raw values or the source string, which is attacker-controlled until
// the enum check passes (mirrors sanitizeSelfReported's no-echo posture).
func parseWorkerStats(raw json.RawMessage, workerID uuid.UUID) *workersvc.WorkerStats {
	if len(raw) == 0 || string(raw) == "null" {
		return nil // no stats on this tick (older worker, or the collector produced none)
	}
	drop := func() *workersvc.WorkerStats {
		slog.Warn("worker reported invalid stats; dropping", "worker_id", workerID.String())
		return nil
	}
	// Typed second-step decode of the isolated RawMessage: a 1e999 (float64 overflow)
	// or an int64-overflow mem value errors HERE only, dropping the stats without
	// touching the outer heartbeat decode.
	var s struct {
		CPUPct   *float64 `json:"cpu_pct"`
		MemBytes *int64   `json:"mem_bytes"`
		MemLimit *int64   `json:"mem_limit_bytes"`
		Source   string   `json:"source"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return drop()
	}
	// Validation (Decision 5): any violation drops the WHOLE stats object.
	if s.MemBytes == nil || *s.MemBytes < 0 {
		return drop()
	}
	if s.MemLimit != nil && *s.MemLimit < 0 {
		return drop()
	}
	if s.Source != "cgroup" && s.Source != "process" {
		return drop()
	}
	out := &workersvc.WorkerStats{MemBytes: *s.MemBytes, MemLimit: s.MemLimit, Source: s.Source}
	// cpu_pct is optional (omitted on the worker's first tick). When present it must be
	// finite; clamp to [0, maxWorkerCPUPct].
	if s.CPUPct != nil {
		v := *s.CPUPct
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return drop()
		}
		switch {
		case v < 0:
			v = 0
		case v > maxWorkerCPUPct:
			v = maxWorkerCPUPct
		}
		out.CPUPct = &v
	}
	return out
}

// WorkerClaim atomically claims the next run for the worker's user. 204 when the
// queue is idle; otherwise the full claim payload (never logged — it carries
// decrypted credentials).
//
// The optional ?lane= query selects which queue to claim from (PRD #39 Decision 4):
// the default/absent/"run" lane claims issue+ci_fix runs (back-compat — an older
// worker sends no lane), and "chat" claims chat runs via the disjoint chat lane and
// returns the narrower ChatClaimPayload (no forge PAT). The worker runs the two
// lanes as independent, concurrent claim loops.
func (h *Handler) WorkerClaim(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}

	switch r.URL.Query().Get("lane") {
	case "chat":
		payload, err := h.wsvc.ClaimChat(r.Context(), wkr)
		if err != nil {
			slog.Error("worker claim (chat lane)", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if payload == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpx.JSON(w, http.StatusOK, payload)
	case "", "run":
		payload, err := h.wsvc.Claim(r.Context(), wkr)
		if err != nil {
			slog.Error("worker claim", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
			return
		}
		if payload == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		httpx.JSON(w, http.StatusOK, payload)
	default:
		httpx.Error(w, http.StatusBadRequest, "lane must be one of run, chat")
	}
}

// WorkerRunMessages appends a batch of seq-numbered messages (idempotent on
// (run_id, seq)).
func (h *Handler) WorkerRunMessages(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req struct {
		Messages []workersvc.IncomingMessage `json:"messages"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.wsvc.AppendMessages(r.Context(), wkr, runID, req.Messages); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrInvalidMessage):
			httpx.Error(w, http.StatusBadRequest, "each message needs a positive seq, a kind, and a JSON payload")
		default:
			slog.Error("worker run messages", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// WorkerRunState applies a state transition and echoes the run's resulting
// status, so the worker learns if the run was cancelled out from under it.
func (h *Handler) WorkerRunState(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	var req workersvc.StateRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, applied, err := h.wsvc.SetState(r.Context(), wkr, runID, req)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrInvalidState):
			httpx.Error(w, http.StatusBadRequest, "state must be one of running, awaiting_approval, completed, failed")
		default:
			slog.Error("worker run state", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if !applied {
		// The run was already terminal (e.g. cancelled out from under the worker):
		// 409 with the run's real status. The worker treats 409 as success and
		// stops (M2 wire contract).
		httpx.JSON(w, http.StatusConflict, map[string]any{"run": runToDTO(run)})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run)})
}

// WorkerRunInputs consumes and returns any pending steering inputs, FIFO.
func (h *Handler) WorkerRunInputs(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid run id")
		return
	}
	inputs, err := h.wsvc.ConsumeInputs(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run inputs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"inputs": inputs})
}
