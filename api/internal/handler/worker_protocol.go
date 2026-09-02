package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/termsafe"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
	"github.com/vtmocanu/uzi/api/internal/workertmpl"
)

// maxSelfReportedBytes caps a worker's free-form self-reported string fields
// (version) before they reach the DB / worker-list UI. Generous for any real
// version string, tight enough to bound abuse.
const maxSelfReportedBytes = 64

// maxAdvertisedConcurrentRuns is the sanity ceiling for a worker's self-reported
// concurrency cap (PRD #42). The cap is observability only — the server enforces no
// cap — so this is a generous absurd-value guard (far above the documented soft
// ceiling of 8), NOT a policy limit. A report outside
// [1, maxAdvertisedConcurrentRuns] is treated as unadvertised (stored NULL), so a
// hostile/garbled value can never flow into the fleet UI's "N/M runs" math.
const maxAdvertisedConcurrentRuns = 256

// sanitizeSelfReported bounds an untrusted worker-reported string: trim, drop
// control AND format characters (so no terminal escape and no bidi override reaches
// a log, a terminal or the UI), and truncate to max bytes (the length check runs
// after each whole rune is written, so it never splits a multi-byte rune — the cap
// is max..max+3 bytes). It sanitizes rather than rejects — these fields are
// observability, and a register must never fail over cosmetic input (it would wedge
// the worker's retry loop).
//
// The Cf half is issue #124 at the SOURCE, and this function is where its headline
// case lands: it carries a review recommendation's `Target` (judge_worker.go:369),
// which is a FILENAME. Cc and Cf are disjoint categories, so IsControl alone never
// saw U+202E — meaning a judge could persist a target that visually names a
// different file than the one it points at, and every reader downstream (web, CLI,
// TUI) inherited it. The predicate now matches sanitizeTTY (api/cmd/uzi/run.go:524)
// and hasUnsafeChar (workersvc/agent_selection.go:236); those two and this are the
// codebase's one answer to "unsafe in untrusted display text".
//
// NO whitespace exception here, deliberately, and that is the difference from
// termsafe.SanitizeBounded (which keeps \n and \t) rather than an oversight: these are
// single-line identifiers — a worker version, a pod phase, a target — where a
// newline is already something to drop.
func sanitizeSelfReported(s string, max int) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		if termsafe.Unsafe(r) {
			continue
		}
		b.WriteRune(r)
		if b.Len() >= max {
			break
		}
	}
	// TRIM AGAIN, and the order is the whole point. TrimSpace above runs BEFORE the strip,
	// and Cf is not White_Space — so one format character at each edge shields the adjacent
	// ASCII space from the trim, the loop then removes the Cf, and the space survives.
	// Measured: "<ZWSP>  api/foo.go  <ZWSP>" came out as "  api/foo.go  " while the plain
	// "  api/foo.go  " came out clean, which is what makes it a defect rather than a choice.
	//
	// It matters here beyond tidiness because this function guards `target`, a COORDINATE:
	// " api/foo.go " and "api/foo.go" are different (category, target) pairs, so the same
	// recommendation raised on two runs — one Cf-padded, one not — becomes two backlog
	// groups a human reads as identical. Newly reachable as of the Cf strip, and it now
	// looks CLEAN rather than obviously junk, which is worse.
	return strings.TrimSpace(b.String())
}

// canonicalizeTargetRe matches any run of ASCII whitespace or ASCII punctuation, which
// canonicalizeTarget collapses to a single ASCII space. BOTH POSIX classes here —
// [:space:] and [:punct:] — are ASCII-ONLY in Go's RE2, and that narrowness is deliberate
// on two counts. First, folding cosmetic ASCII drift ("git-identity" ↔ "git identity",
// "foo.bar" ↔ "foo bar") must not reach into any script's letters or digits, so it can
// never fuse two adjacent words into one token. Second — the load-bearing property for the
// issue-#232 fix — it mirrors the 00097 backfill's regexp_replace(target COLLATE "C",
// '[[:space:][:punct:]]+', ' ', 'g') EXACTLY: the COLLATE "C" forces Postgres's POSIX
// classes to the same ASCII-only classes RE2 uses, so ingest and the historical rows fold
// byte-for-byte identically regardless of the production DB's UTF-8 locale.
var canonicalizeTargetRe = regexp.MustCompile(`[[:space:][:punct:]]+`)

// asciiLowerTarget lowercases ONLY ASCII bytes 'A'–'Z' → 'a'–'z', leaving every other byte
// untouched. The byte loop is safe for UTF-8: every byte of a multi-byte sequence has its
// high bit set, so no continuation byte can ever fall in the 'A'–'Z' range. This is
// DELIBERATELY not strings.ToLower (which is Unicode-aware): the fold must agree
// byte-for-byte with the 00097 backfill's lower(... COLLATE "C"), which is ASCII-only, and
// Postgres's locale-dependent Unicode lowercasing cannot be matched to Go's Unicode tables
// portably. Non-ASCII bytes pass through unchanged on both sides.
func asciiLowerTarget(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// canonicalizeTarget folds a recommendation target's COSMETIC drift so the same finding
// phrased with different casing/whitespace/punctuation collapses to one (category, target)
// coordinate — the exact key the cross-run backlog dedup and the disposition/filed-issue
// joins already match on (issue #232). "Worker Git-Identity Setup " and
// "worker  git-identity   setup" both become "worker git identity setup".
//
// Canonicalization is ASCII-ONLY and byte-deterministic: ASCII-lowercase A–Z, collapse
// every run of ASCII whitespace/punctuation (RE2 POSIX classes, ASCII-only) to one ASCII
// space, trim ASCII spaces. It folds ONLY those three cosmetic axes and DELIBERATELY does
// NOT reorder tokens, drop stopwords, or stem. That restraint is the whole safety argument,
// not a shortcut: this feeds a TRIAGE backlog, and over-merge (collapsing two genuinely
// different findings into one row a human then reads as identical) is the unsafe failure
// mode, strictly worse than under-merge (a cosmetic duplicate the human skims past).
// "worker clone setup (git identity)" and "worker runner clone setup" must stay distinct,
// and they do — nothing here moves or removes a word.
//
// It DELIBERATELY does NOT Unicode-NFC-normalize, Unicode-lowercase, or Unicode-trim —
// because it must agree BYTE-FOR-BYTE with the 00097 backfill SQL,
// lower(btrim(regexp_replace(target COLLATE "C", '[[:space:][:punct:]]+', ' ', 'g'))), and
// Postgres's locale-dependent Unicode class/lower/trim semantics cannot be matched to Go's
// Unicode tables portably. Non-ASCII bytes pass through UNCHANGED on both sides — an
// under-merge (the safe direction; over-merge is the unsafe one). The COLLATE "C" on the
// SQL side is what makes Postgres's POSIX classes, btrim and lower ASCII-only, so a value
// ingested today folds to exactly the coordinate the migration produced for the historical
// rows, no matter the DB locale or any Go/glibc Unicode-table difference.
//
// It runs AFTER sanitizeSelfReported + ScrubSecrets at ingest (judge_worker.go), so the
// control/Cf strip and secret redaction have already run on this string.
//
// `max` mirrors sanitizeSelfReported/termsafe.SanitizeBounded's (s, max) shape and re-bounds to
// the caller's cap. Today's sole caller passes ReviewTargetMaxBytes, so unparam flags it —
// suppressed below because the parameter is the byte-bound contract the plan requires and
// keeps this uniform with the two sibling scrubbers it runs beside on the ingest path.
//
//nolint:unparam // max is the byte-bound contract, uniform with the sibling scrubbers (see above)
func canonicalizeTarget(s string, max int) string {
	s = asciiLowerTarget(s)                           // ASCII-only lowercasing (A–Z); non-ASCII bytes untouched
	s = canonicalizeTargetRe.ReplaceAllString(s, " ") // ASCII whitespace/punct runs → one space
	s = strings.Trim(s, " ")                          // trim ASCII space (0x20) ONLY, matching Postgres btrim's default
	// Re-apply the byte bound rune-safely. This is now genuinely belt-and-suspenders: the
	// ASCII-only transforms above NEVER grow the byte length (folding runs and trimming only
	// shorten; ASCII-lower is 1:1), and the input already passed sanitizeSelfReported's byte
	// cap — so this branch never fires in practice. It stays rune-safe if it ever does: back
	// the cut off to the nearest rune boundary ≤ max, mirroring sanitizeSelfReported's
	// whole-rune cap style, then trim ASCII spaces again in case the cut exposed a fresh edge.
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.Trim(s[:cut], " ")
	}
	return s
}

// maxPackBytes caps a checkpoint publish's raw packfile body (PRD #122 M8). Sized
// generously — a milestone delta is a handful of commits, but a checkpoint after a
// long turn can be larger — while bounding an abusive/garbled worker. The api reads
// the whole body into memory before handing it to the go-git broker, so this is
// also the memory ceiling per in-flight publish.
const maxPackBytes = 64 << 20 // 64 MiB

// checkpointTipRe matches exactly a 40-char lowercase-hex SHA-1 — the only shape a
// git object id takes on the wire. It is validated at the handler so a malformed
// tip is a 400 (a worker bug) and never reaches the broker as a would-be object id.
var checkpointTipRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

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
		// Capabilities is the worker's self-reported REACHABLE capability set (PRD #83
		// Q1: today only ["docker"], meaning a daemon sidecar is reachable). Threaded
		// into wsvc.Register (PRD #84 M1), which UNIONs it with the template-derived
		// caps and passes the result through the server-owned capability.Filter before
		// persisting to workers.capabilities — so an unknown/garbled name here is
		// dropped, never stored, and the register never 400s over this field.
		Capabilities []string `json:"capabilities"`
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
	// Cap + strip control AND format chars (Cc/Cf) so a hostile worker can't smuggle
	// unbounded text, terminal escapes, or a bidi override there. Sanitize (never
	// reject) — it is observability.
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
	updated, err := h.wsvc.Register(r.Context(), wkr, version, reported, advertisedCap, req.Capabilities)
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
	// wkr.TokenHash, NOT updated.TokenHash: `wkr` is the row RequireWorker
	// authenticated against, so its hash is what this caller actually PROVED.
	// `updated` came back from RegisterWorker's RETURNING *, so its hash is a fresh
	// read that would already reflect a rotation committed during that round trip —
	// passing it would silently defeat the qualification and let this request destroy
	// a token it never held. The store statement re-checks the proved hash is still
	// current, so a mid-flight rotation matches zero rows and leaves the new token
	// pending for the pod that actually has it.
	//
	// Orchestrated here rather than inside workersvc on purpose: workersvc owns runs
	// and must not learn about hosted workers, and handler-level orchestration is this
	// repo's idiom. hsvc is nil unless hosting is enabled, and the Kind check keeps an
	// ordinary hand-run worker off this path entirely.
	//
	// Best-effort and NON-FATAL: this is buffer cleanup, and a worker that has already
	// registered successfully must never be failed because the cleanup did not land.
	// The TTL sweep is the backstop. WithoutCancel so a worker that disconnects the
	// instant its registration lands does not cancel its own cleanup — it would
	// self-heal on the retry and the TTL, but there is no reason to leave the buffer
	// sitting for an hour over a dropped connection.
	if h.hsvc != nil && updated.Kind == "hosted" {
		if err := h.hsvc.NoteRegistered(context.WithoutCancel(r.Context()), updated.ID, wkr.TokenHash); err != nil {
			slog.Error("note hosted worker registered", "worker_id", updated.ID.String(), "error", err)
		}
	}
	// worker_id is echoed for the worker's convenience; identity on every other
	// call comes from the Bearer token, never a URL path (M2 wire contract).
	httpx.JSON(w, http.StatusOK, map[string]any{
		"worker_id": updated.ID.String(),
		"worker":    workerDTOFromWorker(updated, 0, false, "", h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt),
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
	httpx.JSON(w, http.StatusOK, map[string]any{"worker": workerDTOFromWorker(updated, 0, false, "", h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt)})
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
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req struct {
		Messages []workersvc.IncomingMessage `json:"messages"`
	}
	// DecodeJSONLimited, not DecodeJSON: this is the one route whose client is a
	// machine that must decide whether to retry (PRD #108 M2). DecodeJSON's
	// io.LimitReader truncates silently, so an oversize batch arrives as malformed
	// JSON and the api literally cannot say "too large" — it would be a THIRD
	// unrelated cause answered 400 through the same generic body, leaving the
	// worker to tell "split and retry" from "bisect out the poison" by
	// prose-matching an error string across a version skew.
	if err := httpx.DecodeJSONLimited(w, r, &req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// 413 is a SPLIT-AND-RETRY signal, never a poison verdict: a healthy,
			// chatty run that merely rode out a transient outage grows its batch
			// across the cap, and failing it for that would be the mirror-image bug.
			// The worker's own byte cap (batcher.ts) is below this one, so reaching
			// here means an un-updated worker — which now gets a truthful answer
			// instead of an ambiguous one.
			//
			// The body is uzi's own prose, never err.Error(): that string is the
			// fixed net/http literal "http: request body too large", pinned by
			// Hyrum's law in the stdlib, and echoing it would couple our wire
			// contract to a stdlib constant. err.Limit is likewise never disclosed.
			//
			// Counted against the run's persistence-failure streak (PRD #108 M4)
			// because this arm answers BEFORE AppendMessages runs, so the recorder
			// inside it never sees a 413. That blind spot is the incident's own long
			// tail: a pre-0.10.1 worker's retry batch grows, so the failure rotates
			// 500 → 413 and then stays 413 forever. NoteOversizeBatch re-checks
			// ownership itself — this handler has authenticated the worker but has not
			// yet established that it holds THIS run.
			h.wsvc.NoteOversizeBatch(r.Context(), wkr, runID)
			httpx.Error(w, http.StatusRequestEntityTooLarge, "this batch is too large; split it and retry")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := h.wsvc.AppendMessages(r.Context(), wkr, runID, req.Messages); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrInvalidMessage):
			httpx.Error(w, http.StatusBadRequest, "each message needs a positive seq, a kind, and a JSON payload")
		case errors.Is(err, workersvc.ErrUnstorableMessage):
			// PRD #108 M2: the database refused this batch for a reason that can
			// never succeed on retry. 400 IS the fix — the status code is the retry
			// contract, and answering 500 here is what turned one poisoned payload
			// into a 27-minute, 239-message wedge (the batcher treats any throw as
			// retryable and re-posts the identical batch at ~2 Hz).
			//
			// Deliberately NOT logged here, and not because it does not matter:
			// workersvc already emitted one WARN carrying run id, seq, kind and the
			// SQLSTATE at the failing insert, which is the only place the seq and
			// kind exist. A second line here would add a run id this route can
			// already be grepped by and duplicate the event at whatever rate the
			// worker retries — while the `default:` arm below deliberately keeps
			// ERROR with the full error, because a genuine 500 is an operator's
			// problem and there is no seq to attach to it.
			//
			// The body carries no SQLSTATE and nothing derived from the database
			// error: it is returned to an untrusted worker, and 22P02/22021 messages
			// quote a fragment of the offending value.
			httpx.Error(w, http.StatusBadRequest, "a message in this batch cannot be stored and will never succeed; do not retry it unchanged")
		default:
			// This logs the WRAPPED error, which for a store failure is a
			// *pgconn.PgError — safe for a NON-OBVIOUS reason, because the obvious
			// "improvement" breaks it.
			//
			// slog special-cases values implementing `error` and renders err.Error(),
			// and PgError.Error() emits only Severity + Message + SQLSTATE. Its
			// Detail/Where/File/Hint fields — which for 22P02 and 22021 quote the
			// offending value, i.e. worker-controlled payload bytes — never reach the
			// line. Keep the error attached AS an error: passing it as a plain value
			// ("pg", *pgErr) makes slog marshal the struct field by field and the
			// poison lands in the log.
			//
			// TestWorkerMessagesLogDoesNotLeakPgErrorFields holds this, rather than
			// this comment holding it — a comment cannot stop the change it warns
			// about. It runs both the JSON and text handlers, because the property
			// belongs to how the error is ATTACHED, not to which handler is wired.
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
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
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
			httpx.Error(w, http.StatusBadRequest, "state must be one of running, awaiting_approval, awaiting_input, awaiting_followup, limit_wait, completed, failed")
		default:
			slog.Error("worker run state", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	if !applied {
		// The transition was a no-op: 409 with the run's REAL status, which the worker
		// reads off the body.
		//
		// This used to say "the run was already terminal (e.g. cancelled out from under
		// the worker)", and PRD #35 made that false in the same commit that shipped
		// limit_wait. A 409 now means any of: the run was already terminal; the park's
		// POSITIVE source guard rejected a report against a run that is not `running`;
		// the report was a re-delivered park onto an already-parked run; or the run is a
		// judge, which never parks. What they share — and the only thing the worker may
		// infer — is that THIS report changed nothing.
		//
		// 🔴 A 409 IS NOT "THE RUN IS FINISHED", AND THE WORKER MUST NOT KEY CLEANUP ON
		// IT. The worker's carve-out (skip the clone/plugin-dir/HOME removals) keys off
		// the RETURNED STATUS being literally "limit_wait", never off applied — because
		// the three most common ways a park does not happen (budget spent, park too far,
		// run opted out) are server-side FAILURES delivered as 200s with
		// status: "failed", where applied is TRUE. An applied-keyed branch leaks the
		// disk on exactly those.
		httpx.JSON(w, http.StatusConflict, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run))})
}

// WorkerRunInputs consumes and returns any pending steering inputs, FIFO.
func (h *Handler) WorkerRunInputs(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
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

// WorkerRunOwnership returns the current status of a run this worker owns —
// a lightweight, READ-ONLY ownership/terminality probe (#559). The interactive
// park-SKIP path uses it to detect a mid-turn reclaim (404) or a terminal
// transition (200 with a terminal status) early, restoring the ACK the skipped
// awaiting_followup park report used to give.
func (h *Handler) WorkerRunOwnership(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	status, err := h.wsvc.RunOwnership(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run ownership", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"status": status})
}

// workerMemoryToDTO maps a stored entry to the worker-facing DTO. It carries run_id
// (provenance) but OMITS repo_id/repo_name — the worker already knows the run's repo
// and the write derives it server-side, so echoing it would be redundant surface.
func workerMemoryToDTO(m store.AgentMemory) apitypes.AgentMemoryDTO {
	dto := apitypes.AgentMemoryDTO{
		ID:        m.ID.String(),
		Title:     m.Title,
		Body:      m.Body,
		Basis:     normalizeMemoryBasis(m.Basis),
		Evidence:  memoryEvidence(m.Evidence),
		CreatedAt: m.CreatedAt.Time,
	}
	if m.RunID.Valid {
		dto.RunID = uuid.UUID(m.RunID.Bytes).String()
	}
	return dto
}

// normalizeMemoryBasis is the READ-side default for writer-declared provenance
// (PRD #266): a stored basis is surfaced only when it is one of the known trust
// labels ("observed" = the run saw it; "inferred" = the lead reasoned it). Anything
// else — NULL (a legacy pre-provenance row), empty, or an unrecognized value a
// worker sent — reads back as "inferred", the conservative label, so the DTO's Basis
// is never blank and an untrusted worker cannot invent a stronger-looking basis.
func normalizeMemoryBasis(basis pgtype.Text) string {
	if basis.Valid {
		switch basis.String {
		case "observed", "inferred":
			return basis.String
		}
	}
	return "inferred"
}

// memoryEvidence surfaces the stored evidence pointer, or "" when NULL/absent (the
// DTO field is omitempty, so an empty value is dropped from the JSON).
func memoryEvidence(evidence pgtype.Text) string {
	if evidence.Valid {
		return evidence.String
	}
	return ""
}

// WorkerSaveMemory persists one cross-run memory entry for the run's (user, repo)
// — the worker's save_memory tool (PRD #90). The identity is derived from the run
// claim inside the service, NEVER from the body: the body carries only {title,
// body}. A repo-less run is a 409, oversize/empty content a 400, the per-run write
// cap a 429.
func (h *Handler) WorkerSaveMemory(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var req apitypes.AgentMemoryWriteRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	mem, err := h.wsvc.SaveMemory(r.Context(), wkr, runID, req.Title, req.Body, req.Basis, req.Evidence)
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrMemoryNoRepo):
			httpx.Error(w, http.StatusConflict, "this run has no repo, so it has no memory to write")
		case errors.Is(err, workersvc.ErrMemoryEmpty):
			httpx.Error(w, http.StatusBadRequest, "memory title and body must be non-empty")
		case errors.Is(err, workersvc.ErrMemoryTooLarge):
			httpx.Error(w, http.StatusBadRequest, "memory title must be at most 200 bytes and body at most 2048 bytes")
		case errors.Is(err, workersvc.ErrMemoryWriteCap):
			httpx.Error(w, http.StatusTooManyRequests, "this run has reached its memory write limit")
		default:
			slog.Error("worker save memory", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	// The write echo is the bare entry {id,title,body,basis,evidence,created_at}
	// (run_id is present on the read path, not the write echo). Basis is normalized
	// the same way the read mappers do, so the echo reflects what a later read will
	// return (an unknown/empty basis the worker sent comes back as "inferred").
	httpx.JSON(w, http.StatusCreated, apitypes.AgentMemoryDTO{
		ID:        mem.ID.String(),
		Title:     mem.Title,
		Body:      mem.Body,
		Basis:     normalizeMemoryBasis(mem.Basis),
		Evidence:  memoryEvidence(mem.Evidence),
		CreatedAt: mem.CreatedAt.Time,
	})
}

// WorkerListMemory returns the run's (user, repo) memory, newest first — the read
// half of the loop the worker composes (nonce-fenced, inert) into the lead's prompt
// at claim time (PRD #90). Scoped to the OWNED run's (user, repo); a repo-less run
// yields an empty list.
func (h *Handler) WorkerListMemory(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	rows, err := h.wsvc.ListMemoryForRun(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker list memory", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]apitypes.AgentMemoryDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, workerMemoryToDTO(m))
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"memories": out})
}

// WorkerRunPublish is the api side of the M8 brokered checkpoint publish (PRD
// #122): the worker POSTs a raw delta PACKFILE (octet-stream body) plus the tip OID
// it claims (header X-Uzi-Checkpoint-Tip), naming ONLY the run id in the path. The
// api derives the repo/branch/PAT entirely from the run row and pushes the pack
// NON-FORCED to refs/uzi-checkpoints/<branch> — the worker can never name the
// repo/ref/credential.
//
// This is a best-effort durability path: a 500 is ignored by the worker, and the
// benign "origin moved / unsupported" outcomes are 200 skips, not errors.
func (h *Handler) WorkerRunPublish(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	tip := r.Header.Get("X-Uzi-Checkpoint-Tip")
	if !checkpointTipRe.MatchString(tip) {
		httpx.Error(w, http.StatusBadRequest, "invalid checkpoint tip")
		return
	}
	// Raw octet-stream body, NOT JSON: httpx.DecodeJSON is JSON+1 MiB only. Cap with
	// MaxBytesReader so an over-cap pack is a truthful 413 (a split/too-large signal),
	// not a silently truncated — and thus malformed — pack.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPackBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.Error(w, http.StatusRequestEntityTooLarge, "checkpoint pack too large")
			return
		}
		httpx.Error(w, http.StatusBadRequest, "could not read checkpoint pack")
		return
	}
	res, err := h.wsvc.Publish(r.Context(), wkr, runID, tip, body)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		// A genuine 5xx from the broker (transport/non-sentinel fault; the benign
		// "origin moved / unsupported" outcomes come back with err==nil and a res.Skipped
		// reason, logged below). Carry run_id + worker_id so an operator can pin the log
		// line to a run without a second lookup, and a static `reason` ("internal") that
		// distinguishes this outcome the same way a metric label would — matching the
		// repo's log-based observability idiom (there is no metrics surface in the api;
		// see internal/workersvc/autostop.go). NOTE: the checkpoint branch is derived
		// server-side inside workersvc.Publish (from the run's issue iid) and is NOT
		// returned on this error path, so it cannot be attached here — run_id is the
		// correlation key. The error is already PAT-scrubbed by workersvc
		// (secretscrub.Scrub) before it reaches this line.
		slog.Error("worker run publish",
			"run_id", runID.String(),
			"worker_id", wkr.ID.String(),
			"reason", "internal",
			"error", err,
		)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := map[string]any{"published": res.Published, "ref": res.Ref}
	if res.Skipped != "" {
		// A benign non-publish outcome (origin moved / workflow-scope / unsupported /
		// no-ref): a 200, not a fault. Log it at INFO with the mapped skip reason so the
		// distribution of publish outcomes is observable by `reason` — the same label a
		// checkpoint_publish_failures_total counter would carry — without adding a metrics
		// dependency the api does not have (see the error arm above). INFO, not WARN:
		// not_descendant is the common "origin advanced" case and must not read as an
		// operator alert.
		slog.Info("worker run publish skipped",
			"run_id", runID.String(),
			"worker_id", wkr.ID.String(),
			"reason", res.Skipped,
		)
		out["skipped"] = res.Skipped
	}
	httpx.JSON(w, http.StatusOK, out)
}
