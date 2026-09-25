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
	"strconv"
	"strings"
	"time"
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
		// ProtocolCapabilities is the worker's self-reported PROTOCOL capability set
		// (PRD #1226 M1, D2: today ["completion_interlock_v1"], meaning this image
		// implements the structural completion protocol). Threaded into wsvc.Register,
		// which passes it through the server-owned capability.FilterProtocol before
		// persisting to workers.protocol_capabilities — a SEPARATE column from
		// workers.capabilities, so a protocol string never leaks into the scheduler
		// vocabulary or the web capability picker. An unknown/garbled name here is
		// dropped, never stored, and the register never 400s over this field.
		ProtocolCapabilities []string `json:"protocol_capabilities"`
		// ActiveSnapshot is the worker's active-run snapshot (PRD #1390 M2a). #1390's worker
		// NEVER sends it on register — the path exists for #1391's restart-replay of pending
		// outcomes (it is the one snapshot exempt from the nonce check). Captured as an isolated
		// json.RawMessage, parsed defensively below, so a malformed body can never 400 the
		// register (a register that fails over soft input wedges the worker's retry loop).
		ActiveSnapshot json.RawMessage `json:"active_snapshot"`
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
	// An ephemeral (run-bound) worker can claim only its bound run (PRD #529
	// Decision 4: ClaimRun's is_ephemeral/ephemeral_run_id clause), so its effective
	// run-lane cap is 1 whatever it advertises (absent, 2, out of range). Clamped
	// here, server-side, so the stored cap does not depend on the worker image
	// version; heartbeats never write the cap (only RegisterWorker does), so this is
	// the one place it is set. Persistent workers keep their advertised cap (issue #1624).
	if wkr.Ephemeral {
		one := 1
		advertisedCap = &one
	}
	// A register-carried snapshot is parsed only when the feature is enabled; #1390's worker
	// never sends one, so this is nil in practice (the path is #1391's). An unparseable body
	// yields nil (dropped, logged) — never a register failure.
	var regSnapshot *workersvc.ActiveSnapshot
	if !h.cfg.ActiveSnapshotDisabled {
		regSnapshot = parseActiveSnapshot(req.ActiveSnapshot, wkr.ID)
	}
	updated, registerNonce, err := h.wsvc.Register(r.Context(), wkr, version, reported, advertisedCap, req.Capabilities, req.ProtocolCapabilities, regSnapshot)
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
	//
	// protocol_features is the shared negotiation wire (PRD #1392 M1 / #1391 D8 / #1390 M2a):
	// the worker sends a gated wire extension only when its feature string appears here. A
	// just-registered worker holds nothing, so overlayOutbox is a no-op, but the register
	// response is one of the WorkerDTO surfaces and stays uniform with the list/heartbeat
	// paths.
	dto := workerDTOFromWorker(updated, 0, false, "", h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt)
	h.overlayOutbox(&dto, updated.ID)
	// register_nonce (PRD #1390 M2a): the per-registration nonce every subsequent heartbeat and
	// claim snapshot must echo. Minted + persisted on the worker row inside Register's tx and
	// returned here. protocol_features gates whether the worker even sends snapshots, but the
	// nonce is issued unconditionally (harmless to an old worker, which ignores it).
	httpx.JSON(w, http.StatusOK, map[string]any{
		"worker_id":         updated.ID.String(),
		"worker":            dto,
		"protocol_features": protocolFeatures(!h.cfg.ActiveSnapshotDisabled),
		"register_nonce":    registerNonce,
		// worker_outbox_max_pending (PRD #1391 Run B M3c): the server's terminal_pending outbox cap,
		// returned at register so the worker can size its own pending-outcome quota to match the
		// server's WORKER_OUTBOX_MAX_PENDING without a separate config channel.
		"worker_outbox_max_pending": h.cfg.WorkerOutboxMaxPending,
	})
}

// protocolFeatures is the set of optional wire-protocol behaviours this api implements,
// advertised on the register response — the shared negotiation wire the worker reads to
// decide which gated heartbeat/message extensions to send. Composed as a UNION of per-PRD
// slices, deduped — NEVER a single literal a sibling PRD would overwrite.
//
// 🔴 THE RULE, VERBATIM: each PRD adds its own slice at its landing rebase (union, never
// replace). PRD #1392 M1 adds `recovery_park_cause` and `recovery_release_exact_echo`;
// PRD #1391 Run A adds `heartbeat_outbox`; PRD #1247 adds `claim_generation_fence`.
//
// `claim_generation_fence` is a SERVER-SUPPORT advertisement, not an issue-ownership token:
// this api now implements the per-query claim-generation fence — a `credential_switch_v1`
// worker fails closed (ErrMissingClaimGeneration) if a mutating batch omits the generation.
// The advertisement is what lets a NON-capability (#1391-era) worker know it may stamp the
// field; a `credential_switch_v1` CAPABILITY worker stamps OPTIMISTICALLY regardless of this
// advertisement (its runs are fenced server-side, and a one-shot register may have missed the
// feature under rollout skew), and rides the strict-decode strip-and-retry fallback (PRD #1247
// fix round) on the message, /state and completion wires if it meets an api that predates the
// field. #1390 lands after #1247 and its own slice must preserve/dedupe
// this token, not activate it for the first time.
//
// `terminal_fence` (PRD #1391 Run B M3c) is Run B's own slice, added AFTER claim_generation_fence
// and BEFORE the conditional active_run_snapshot append. This api now implements the terminal
// fence — a terminal (completed/failed) report may stamp messages_through_seq and SetState refuses
// the transition (ErrMessagesPending / ErrGapUnrecoverable) until run_messages are contiguous
// through it — so advertising the token tells a fence-capable worker it may send the field. Returns
// a fresh slice so a caller cannot mutate the advertised set.
//
// `active_run_snapshot` (PRD #1390 M2a) is appended in its own group, gated on
// activeSnapshotEnabled (= !cfg.ActiveSnapshotDisabled). When the api is started with
// UZI_ACTIVE_SNAPSHOT_DISABLED set (the D7 rollback simulation) the token is omitted and the
// worker never sends the snapshot — the same shape an old worker sees.
func protocolFeatures(activeSnapshotEnabled bool) []string {
	groups := [][]string{
		{"recovery_park_cause", "recovery_release_exact_echo"}, // PRD #1392 M1
		{"heartbeat_outbox"},       // PRD #1391 M5, Run A
		{"claim_generation_fence"}, // PRD #1247 M5 (D11): this api fences message/report inserts on claim_generation for a credential_switch_v1 worker
		{"terminal_fence"},         // PRD #1391 Run B M3c: this api fences a terminal transition on messages_through_seq contiguity
	}
	if activeSnapshotEnabled {
		groups = append(groups, []string{"active_run_snapshot"}) // PRD #1390 M2a
	}
	seen := make(map[string]bool)
	out := make([]string, 0)
	for _, g := range groups {
		for _, f := range g {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
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
		// Outbox is the per-run outbox depth (PRD #1391 M5), its OWN isolated
		// json.RawMessage exactly like Stats and for the same reason: a malformed or
		// oversized report must drop the depth WITHOUT failing the heartbeat's liveness.
		// The worker sends it only when the register response advertised
		// `heartbeat_outbox` AND a run has depth, so an older api never sees it and a
		// current worker on a rolled-back api strips it on the generic-400 retry (D8).
		Outbox json.RawMessage `json:"outbox"`
		// ActiveSnapshot is the worker's active-run snapshot (PRD #1390 M2a), its OWN isolated
		// json.RawMessage like Stats/Outbox and for the same reason: a malformed body must drop
		// the snapshot WITHOUT failing the heartbeat's liveness. The worker sends it only when
		// the register response advertised `active_run_snapshot`. When the api is started with
		// UZI_ACTIVE_SNAPSHOT_DISABLED (D7's rollback simulation) it is not advertised, and a
		// heartbeat that still carries it is 400'd below — exactly the generic 400 that triggers
		// the worker's strip-and-retry fallback.
		ActiveSnapshot json.RawMessage `json:"active_snapshot"`
	}
	if err := httpx.DecodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	// Second step: validate + clamp the isolated stats (Decision 5). A malformed or
	// invalid sample drops to nil (columns written NULL) and the heartbeat still 200s.
	stats := parseWorkerStats(req.Stats, wkr.ID)
	// Same defensive second step for the isolated outbox (PRD #1391 M5): validate
	// drop-not-fail and hand the parsed entries to the service, which records them in
	// its in-process tracker. An absent/empty/malformed body yields nil — which CLEARS
	// the worker's tracked depth (the "clears on the next empty report" contract) — and
	// the 200 stands.
	outbox := parseWorkerOutbox(req.Outbox, wkr.ID)
	// Active-run snapshot (PRD #1390 M2a, D7). When the feature is DISABLED, a heartbeat carrying
	// the field is rejected with a generic 400 — the same rejection a pre-#1390 strict decoder
	// would give an unknown field, which is what makes the worker strip the field and retry
	// (never a lost heartbeat once it does). When enabled, parse defensively: a malformed body
	// drops to nil (snapshot ignored) and the heartbeat still 200s.
	var snapshot *workersvc.ActiveSnapshot
	if h.cfg.ActiveSnapshotDisabled {
		if req.ActiveSnapshot != nil {
			httpx.Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
	} else {
		snapshot = parseActiveSnapshot(req.ActiveSnapshot, wkr.ID)
	}
	updated, err := h.wsvc.Heartbeat(r.Context(), wkr, stats, outbox, snapshot)
	if err != nil {
		slog.Error("worker heartbeat", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	dto := workerDTOFromWorker(updated, 0, false, "", h.version, h.cfg.HostedWorkerVersion, h.clock(), h.startedAt)
	h.overlayOutbox(&dto, updated.ID)
	httpx.JSON(w, http.StatusOK, map[string]any{"worker": dto})
}

// Outbox heartbeat validation bounds (PRD #1391 M5). The report is untrusted
// worker self-report bound for an in-process map and the fleet UI.
const (
	// maxOutboxReportBytes is the WHOLE-REPORT byte cap: past it the entire report is
	// dropped (like parseWorkerStats drops the whole stats object), so a hostile worker
	// cannot make the api parse an unbounded array. A worker holds at most a handful of
	// runs, each entry a few hundred bytes, so this is generous headroom.
	maxOutboxReportBytes = 128 << 10 // 128 KiB
	// maxOutboxEntries is the ENTRY-COUNT cap: past it the whole report is dropped. Far
	// above any real worker's concurrent-run count (documented soft ceiling 8).
	maxOutboxEntries = 256
	// maxOutboxCount is the sane per-count ceiling. A count outside [0, maxOutboxCount]
	// drops only THAT entry (the granular precedent is parseWorkerStats' per-disk-field
	// drop). ~1e9 is orders of magnitude above any real backlog the quota permits.
	maxOutboxCount = 1 << 30
	// maxOutboxBlockedBytes bounds the blocked_reason string. It is sanitized (control
	// + format chars stripped) and truncated, never rejected — same posture as
	// sanitizeSelfReported.
	maxOutboxBlockedBytes = 200
)

// parseWorkerOutbox is the heartbeat's defensive second-step parse of the untrusted,
// isolated `outbox` field (PRD #1391 M5), mirroring parseWorkerStats. It NEVER fails
// the heartbeat: an absent, empty, null, oversized, or malformed body returns nil (a
// nil clears the worker's tracked depth) and the 200 stands. On a drop it logs
// worker_id + a STATIC reason only — never the raw values, which are attacker-
// controlled until validation passes (mirrors sanitizeSelfReported's no-echo posture).
//
// Drop granularity, chosen deliberately (see parseWorkerStats' two precedents — the
// coarse whole-object drop and the granular per-disk-field drop):
//   - WHOLE-REPORT drop: the byte cap, the entry-count cap, and a top-level non-array
//     (the outer []json.RawMessage decode fails) — none of these can be trusted to
//     bound anything.
//   - PER-ENTRY drop: a wrong-typed entry (its own typed Unmarshal fails), a bad run_id,
//     a negative/absurd count, or an invalid `since` drops only that one entry (the rest
//     of a valid report still lands), because one bad entry proves nothing about the others.
func parseWorkerOutbox(raw json.RawMessage, workerID uuid.UUID) []workersvc.OutboxEntry {
	if len(raw) == 0 || string(raw) == "null" {
		return nil // no outbox on this tick (older worker, feature off, or drained → clears)
	}
	drop := func(reason string) []workersvc.OutboxEntry {
		slog.Warn("worker reported invalid outbox; dropping", "worker_id", workerID.String(), "reason", reason)
		return nil
	}
	// Whole-report byte cap FIRST, before Unmarshal touches it.
	if len(raw) > maxOutboxReportBytes {
		return drop("oversize")
	}
	// Decode the TOP LEVEL into []json.RawMessage first, then each element into the typed
	// per-entry struct below. Only a top-level shape error (not a JSON array) is a
	// whole-report "malformed" drop; a WRONG-TYPED element (e.g. a numeric run_id or an
	// object-valued blocked_reason) fails only its own per-entry Unmarshal and drops just
	// that entry — a single typed field must never abort the whole array and discard every
	// valid entry with it. Counts and `since` likewise decode as *json.Number so a float /
	// overflow / non-integer value fails per-entry (converted below), the same reason
	// parseWorkerStats decodes its disk fields as *json.Number.
	var rawItems []json.RawMessage
	if err := json.Unmarshal(raw, &rawItems); err != nil {
		return drop("malformed")
	}
	if len(rawItems) > maxOutboxEntries {
		return drop("too many entries")
	}
	out := make([]workersvc.OutboxEntry, 0, len(rawItems))
	for _, rawItem := range rawItems {
		var it struct {
			RunID           string       `json:"run_id"`
			PendingMessages *json.Number `json:"pending_messages"`
			PendingTerminal *json.Number `json:"pending_terminal"`
			StaleRetired    *json.Number `json:"stale_retired"`
			BlockedReason   string       `json:"blocked_reason"`
			Since           *json.Number `json:"since"`
		}
		if err := json.Unmarshal(rawItem, &it); err != nil {
			continue // wrong-typed entry → drop only this entry
		}
		id, err := uuid.Parse(it.RunID)
		if err != nil {
			continue // bad run_id → drop only this entry
		}
		pm, ok := outboxCountOrDrop(it.PendingMessages)
		if !ok {
			continue
		}
		pt, ok := outboxCountOrDrop(it.PendingTerminal)
		if !ok {
			continue
		}
		sr, ok := outboxCountOrDrop(it.StaleRetired)
		if !ok {
			continue
		}
		since, ok := outboxSinceOrDrop(it.Since)
		if !ok {
			continue
		}
		out = append(out, workersvc.OutboxEntry{
			RunID:           id,
			PendingMessages: pm,
			PendingTerminal: pt,
			StaleRetired:    sr,
			// Sanitize (strip control + format chars, bound) rather than reject: the
			// reason reaches a CLI column and the web, and a register/heartbeat must never
			// fail over cosmetic input. Same posture as sanitizeSelfReported.
			BlockedReason: sanitizeSelfReported(it.BlockedReason, maxOutboxBlockedBytes),
			Since:         since,
		})
	}
	return out
}

// maxActiveSnapshotBytes bounds the heartbeat/claim/register active_snapshot body before
// Unmarshal touches it (PRD #1390 M2a). Generous for ACTIVE_SNAPSHOT_MAX_ENTRIES entries (each
// ~150 bytes) while bounding an abusive/garbled worker; the semantic entry caps are enforced
// server-side in ReplaceWorkerActiveRuns.
const maxActiveSnapshotBytes = 128 << 10 // 128 KiB

// parseActiveSnapshot is the defensive JSON-shape parse of the untrusted active_snapshot body
// (PRD #1390 M2a). It NEVER fails the request: an absent/empty/oversize/malformed body returns
// nil (the snapshot is simply not applied) and, for a heartbeat, the 200 stands. It does only
// the shape decode; the SEMANTIC validation (nonce, epoch ordering, phase set, caps, ownership)
// lives in ReplaceWorkerActiveRuns, which needs the worker's stored nonce/epoch from the DB. A
// lenient Unmarshal (not the strict whole-body decoder) is used on purpose, so a future worker
// adding an inner field never 400s a heartbeat that carries the snapshot.
func parseActiveSnapshot(raw json.RawMessage, workerID uuid.UUID) *workersvc.ActiveSnapshot {
	if len(raw) == 0 || string(raw) == "null" {
		return nil // no snapshot on this tick (older worker, feature off/absent)
	}
	if len(raw) > maxActiveSnapshotBytes {
		slog.Warn("worker reported oversize active snapshot; dropping", "worker_id", workerID.String())
		return nil
	}
	var snap workersvc.ActiveSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		slog.Warn("worker reported malformed active snapshot; dropping", "worker_id", workerID.String())
		return nil
	}
	return &snap
}

// outboxCountOrDrop converts a raw count field to a non-negative int within the sane
// ceiling, reporting ok=false (drop the entry) on absent / parse error / overflow /
// negative / absurd. The counts are REQUIRED on the wire, so a nil is a malformed
// entry, not a zero.
func outboxCountOrDrop(n *json.Number) (int, bool) {
	if n == nil {
		return 0, false
	}
	v, err := n.Int64()
	if err != nil || v < 0 || v > maxOutboxCount {
		return 0, false
	}
	return int(v), true
}

// outboxSinceOrDrop converts the epoch-millisecond `since` (a NUMBER on the wire, not
// an RFC3339 string) to a time, reporting ok=false (drop the entry) on absent / parse
// error / overflow / negative. Only the ordering of blocked reasons depends on it.
func outboxSinceOrDrop(n *json.Number) (time.Time, bool) {
	if n == nil {
		return time.Time{}, false
	}
	ms, err := n.Int64()
	if err != nil || ms < 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
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
		// Disk fields decode as *json.Number, NOT *int64, so an out-of-range or
		// malformed disk NUMBER (e.g. an int64-overflow like 99999999999999999999)
		// stores its literal here instead of failing the outer Unmarshal — which would
		// take the whole-object drop() path and null cpu/mem/source too, contradicting
		// PRD #837 M1's "a bad DISK field drops only that field." Each is converted to
		// *int64 below by diskBytesOrNil, which drops (nil) on parse error / overflow /
		// negative. cpu/mem stay *int64 on purpose: their overflow-drops-the-object
		// behavior is pre-existing and out of scope here.
		DiskNixBytes       *json.Number `json:"disk_nix_bytes"`
		DiskNixTotalBytes  *json.Number `json:"disk_nix_total_bytes"`
		DiskDataBytes      *json.Number `json:"disk_data_bytes"`
		DiskDataTotalBytes *json.Number `json:"disk_data_total_bytes"`
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
	// Disk fields (PRD #837 M1) are per-volume used/total, each optional (omitted when
	// the worker's statfs failed or the mount was absent). Unlike mem, a bad DISK value
	// drops ONLY that field — never the whole stats object — so a negative on one volume
	// cannot discard the cpu/mem gauge. A negative is dropped by leaving the pointer nil.
	out.DiskNixBytes = diskBytesOrNil(s.DiskNixBytes)
	out.DiskNixTotalBytes = diskBytesOrNil(s.DiskNixTotalBytes)
	out.DiskDataBytes = diskBytesOrNil(s.DiskDataBytes)
	out.DiskDataTotalBytes = diskBytesOrNil(s.DiskDataTotalBytes)
	return out
}

// diskBytesOrNil converts a raw disk-field JSON number to a non-negative *int64, dropping
// (nil) on absent / parse error / int64-overflow / negative. It is the display-only disk
// fields' tolerant per-field decode: because the field arrives as a *json.Number (its
// literal, never coerced at Unmarshal time), a bad value drops ONLY that field here rather
// than failing the whole stats decode and discarding the cpu/mem gauge with it (PRD #837
// M1). Number.Int64 folds both the overflow and the malformed/non-integer cases into an
// error, and a negative is dropped just as the previous non-negative check did.
func diskBytesOrNil(n *json.Number) *int64 {
	if n == nil {
		return nil
	}
	v, err := n.Int64()
	if err != nil || v < 0 {
		return nil
	}
	return &v
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
		// PRD #1390 M3 (Task 1): the run-lane claim carries the worker's active-run snapshot, so a
		// claim that beats the first post-outage heartbeat (fact 7) still dedupes and pre-locks its
		// own runs. Strict-decode a body with just `active_snapshot`, treating EOF as "no snapshot"
		// (the same !io.EOF guard the heartbeat/register handlers use) so an OLD bodyless worker
		// never 400s. Chat stays bodyless (D10).
		var req struct {
			ActiveSnapshot json.RawMessage `json:"active_snapshot"`
		}
		if err := httpx.DecodeJSON(r, &req); err != nil && !errors.Is(err, io.EOF) {
			httpx.Error(w, http.StatusBadRequest, "invalid request body")
			return
		}
		// Feature gate, mirroring WorkerHeartbeat's D7 rule EXACTLY: a disabled api 400s a claim that
		// still carries the field (the same generic 400 that triggers the worker's strip-and-retry);
		// when enabled, parse the snapshot defensively (a malformed body drops to nil).
		var snapshot *workersvc.ActiveSnapshot
		if h.cfg.ActiveSnapshotDisabled {
			if req.ActiveSnapshot != nil {
				httpx.Error(w, http.StatusBadRequest, "invalid request body")
				return
			}
		} else {
			snapshot = parseActiveSnapshot(req.ActiveSnapshot, wkr.ID)
		}
		payload, err := h.wsvc.Claim(r.Context(), wkr, snapshot)
		if err != nil {
			// An invalid/stale-epoch/wrong-nonce claim snapshot fails the claim CLOSED (D3): 400,
			// no claim, no side effect. Same generic body the worker reads for its strip-and-retry.
			if errors.Is(err, workersvc.ErrActiveSnapshotInvalid) {
				httpx.Error(w, http.StatusBadRequest, "invalid request body")
				return
			}
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
		// ClaimGeneration is the runs.claim_generation the reporting worker believes it holds
		// (PRD #1247 M5, D3). A CAPABILITY worker stamps it on every batch; the fenced append
		// then persists ONLY while the run is still at that generation with an unreleased claim,
		// so a released/reclaimed old flight's batch lands nothing. Nullable + OPTIONAL: a legacy
		// worker omits it and the append is unfenced. The field must exist here because
		// DecodeJSONLimited rejects unknown fields.
		ClaimGeneration *int64 `json:"claim_generation"`
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
	if err := h.wsvc.AppendMessagesForClaim(r.Context(), wkr, runID, req.Messages, req.ClaimGeneration); err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrMissingClaimGeneration):
			// PRD #1247 M5a-1 rework (auditor fail-open finding): a CAPABILITY worker
			// (advertising credential_switch_v1) omitted claim_generation on a message batch, so
			// the per-query fence could not engage. Refuse rather than persist unfenced. 409 is
			// the refuse ack — a protocol violation the worker fixes by stamping the generation,
			// distinct from the 404 (not owned) and 400 (bad/unstorable batch) causes.
			httpx.Error(w, http.StatusConflict, "this worker must stamp claim_generation on every message batch")
		case errors.Is(err, workersvc.ErrStaleClaim):
			// PRD #1247 M5 (BLOCKING-4 rework): the message batch fenced out — a held-state switch
			// RELEASED this claim or a reclaim SUPERSEDED it, so it persisted NOTHING and no usage
			// was folded. Answer the same stale_claim 409 disposition WorkerRunState uses, which
			// the worker reads to STOP the old flight rather than treat a 409 as a permanent
			// per-message reject and bisect the batch. The old flight no longer owns the run, so no
			// run DTO is carried — the disposition is the whole signal.
			httpx.JSON(w, http.StatusConflict, map[string]any{"disposition": "stale_claim"})
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

// forgeParkRefusalReason maps a forge-park precedence refusal (PRD #1392 M1) to the token the
// WorkerRunState 409 {run, reason} body carries, which the worker dispatches on:
//   - "stale_claim": a newer claim superseded this worker's, nothing was mutated — the worker
//     stops silently.
//   - "custody_unsettled": the generation's custody hold could not be settled — the worker falls
//     to today's failed path.
//
// It returns ("", false) for EVERY other error so WorkerRunState keeps routing
// ErrRunNotOwned / ErrInvalidState and the generic 500 through its own switch. Kept as a pure
// function for two reasons: the exact error→reason mapping is unit-tested (a
// stale_claim↔custody_unsettled swap reddens) WITHOUT the live-DB forge-park transaction that
// raises these errors; and, being the single source of the reason token, it makes the two 409
// bodies structurally incapable of the copy-paste mixup two hand-written case arms invited.
func forgeParkRefusalReason(err error) (string, bool) {
	switch {
	case errors.Is(err, workersvc.ErrForgeParkStaleClaim):
		return "stale_claim", true
	case errors.Is(err, workersvc.ErrForgeParkCustodyUnsettled):
		return "custody_unsettled", true
	default:
		return "", false
	}
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
		// PRD #1392 M1: the two forge-park precedence refusals are 409 with a {run, reason}
		// body — the worker dispatches on `reason` (stale_claim stops silently; custody_unsettled
		// falls to today's failed path). recovery_retry_not_before rides on the run (RunDTO), not
		// the top-level body. Both carry the run row (SetState returns it alongside the error).
		// The reason token comes from the single pure forgeParkRefusalReason mapping, so the two
		// bodies cannot drift or swap; a non-forge-park error returns ok=false and falls through.
		if reason, ok := forgeParkRefusalReason(err); ok {
			httpx.JSON(w, http.StatusConflict, map[string]any{
				"run":    runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock()),
				"reason": reason,
			})
			return
		}
		switch {
		case errors.Is(err, workersvc.ErrMessagesPending):
			// PRD #1391 Run B M3c (D3): the terminal fence refused a completed/failed report whose
			// run_messages are not yet contiguous through the reported messages_through_seq. 409 with
			// the SAME {run, reason} shape as the forge-park refusals (top-level reason, NOT the
			// disposition shape) so the worker fills the missing seqs and re-reports. SetState returns
			// the run alongside the error.
			httpx.JSON(w, http.StatusConflict, map[string]any{
				"run":    runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock()),
				"reason": "messages_pending",
			})
		case errors.Is(err, workersvc.ErrGapUnrecoverable):
			// PRD #1391 Run B M3c: the hole below messages_through_seq is larger than
			// WorkerGapFillMax, so it can never be filled — a typed 409 (NOT a 400) telling the worker
			// to stop re-parking on it. Distinct reason token from messages_pending.
			httpx.JSON(w, http.StatusConflict, map[string]any{
				"reason": "gap_unrecoverable",
			})
		case errors.Is(err, workersvc.ErrStaleClaim), errors.Is(err, workersvc.ErrMissingClaimGeneration):
			// PRD #1247 M5 (D3): the generation fence rejected this report — a held-state switch
			// RELEASED this claim, or a reclaim SUPERSEDED it. M5a-1 rework: it ALSO covers a
			// CAPABILITY worker that OMITTED claim_generation on a mutating report
			// (ErrMissingClaimGeneration, fail-closed). Answer 409 with the run PLUS a
			// disposition:"stale_claim" field the new worker reads to STOP the old flight without
			// further reports. The 409 status is shared with an ordinary not-applied ack (an old
			// worker that ignores the extra field still treats it as "changed nothing"); the
			// disposition is what distinguishes a stale claim from a benign no-op.
			httpx.JSON(w, http.StatusConflict, map[string]any{
				"run":         runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock()),
				"disposition": "stale_claim",
			})
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrInvalidState):
			httpx.Error(w, http.StatusBadRequest, "state must be one of running, awaiting_approval, awaiting_input, awaiting_followup, limit_wait, recovery_wait, paused, pause_failed, credential_switch, credential_switch_failed, completed, failed")
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
		// the RETURNED STATUS matching the requested park, never off applied, because
		// the three most common ways a park does not happen (budget spent, park too far,
		// run opted out) are server-side FAILURES delivered as 200s with
		// status: "failed", where applied is TRUE. An applied-keyed branch leaks the
		// disk on exactly those.
		httpx.JSON(w, http.StatusConflict, h.workerStateAck(r, run))
		return
	}
	ack := h.workerStateAck(r, run)
	if req.State == "credential_switch" {
		// PRD #1247 M5b (BLOCKING-2 rework): the held-state credential-switch RELEASE applied — a
		// FRESH requeue (status 'queued') OR an idempotent release after a reclaim (applied, status
		// 'running'). Tell the worker EXPLICITLY the release took, so enterCredentialSwitch accepts
		// it regardless of the run's status. It previously required status == 'queued' and so gave
		// up on the idempotent-after-reclaim success (applied=true, status 'running'), leaving the
		// old flight to continue on a claim the reclaim already owns.
		ack["disposition"] = "released"
	}
	httpx.JSON(w, http.StatusOK, ack)
}

// workerStateAck builds the state-report ack body: the run DTO plus, when a held-state switch is
// pending for the run's CURRENT claim, the worker-facing credential_switch signal (PRD #1247 M5,
// D3/D4). It is shared by the 200 ack and the ORDINARY not-applied 409 ack — both hand back a run
// the worker may still be holding, so both must carry the switch signal. It is deliberately NOT
// used for the stale_claim 409 disposition, which already tells the worker to STOP the flight.
func (h *Handler) workerStateAck(r *http.Request, run store.Run) map[string]any {
	ack := map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock())}
	if sig := workersvc.PendingCredentialSwitchSignal(run); sig != nil {
		ack["credential_switch"] = sig
	}
	return ack
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
	res, err := h.wsvc.ConsumeInputs(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run inputs", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// inputs is ALWAYS an array (never nil) — the consume-nothing path returns an empty slice.
	inputs := res.Inputs
	if inputs == nil {
		inputs = []workersvc.InputDTO{}
	}
	body := map[string]any{"inputs": inputs}
	// PRD #1247 M5 (D3/D4, step 2): surface the held-state switch signal on EVERY inputs
	// response — including an empty-inputs poll (the idle gate/question/follow-up waiters poll
	// this route continuously) — so the worker holding the current claim learns a switch was
	// requested and begins its local release. Omitted when no switch is pending for this claim.
	if res.CredentialSwitch != nil {
		body["credential_switch"] = res.CredentialSwitch
	}
	httpx.JSON(w, http.StatusOK, body)
}

func (h *Handler) workerInputReceipt(w http.ResponseWriter, r *http.Request, applied bool) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		IDs             []int64 `json:"ids"`
		ClaimGeneration *int64  `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil || body.ClaimGeneration == nil {
		httpx.Error(w, http.StatusBadRequest, "invalid input receipt body")
		return
	}
	var res workersvc.InputReceiptResult
	var err error
	if applied {
		res, err = h.wsvc.ApplyInputs(r.Context(), wkr, runID, *body.ClaimGeneration, body.IDs)
	} else {
		res, err = h.wsvc.AckInputs(r.Context(), wkr, runID, *body.ClaimGeneration, body.IDs)
	}
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			// Typed, like the 409: the worker ends its flight only on reason "stale", and keeps
			// retrying an untyped 404 (an older api pod without this route, mid-roll).
			httpx.ErrorReason(w, http.StatusNotFound, "run not found", workersvc.ReceiptStale)
		case errors.Is(err, workersvc.ErrInputReceiptInvalid):
			httpx.Error(w, http.StatusBadRequest, "invalid input ids or capability")
		case errors.Is(err, workersvc.ErrInputReceiptConflict):
			// The inactive reason tells the worker whether to keep polling (switch_pending)
			// or end its old flight (released, stale); empty for a row-state conflict.
			var conflict *workersvc.InputReceiptConflictError
			reason := ""
			if errors.As(err, &conflict) {
				reason = conflict.Reason
			}
			httpx.ErrorReason(w, http.StatusConflict, "input receipt conflicts with claim", reason)
		default:
			slog.Error("worker input receipt", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

func (h *Handler) WorkerRunInputsAck(w http.ResponseWriter, r *http.Request) {
	h.workerInputReceipt(w, r, false)
}

func (h *Handler) WorkerRunInputsApplied(w http.ResponseWriter, r *http.Request) {
	h.workerInputReceipt(w, r, true)
}

// WorkerRunFollowUps returns the already-consumed follow_up inputs of a run this worker owns,
// oldest first, as {"inputs": [...]} in the /inputs shape (issue #1660). READ ONLY. The worker
// reads it on every claim to rehydrate the run's operator constraints, which it attaches to
// every subagent dispatch. A run this worker does not hold is a 404.
func (h *Handler) WorkerRunFollowUps(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	inputs, err := h.wsvc.ConsumedFollowUps(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run follow-ups", "error", err)
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
	status, recoveryRetryNotBefore, claimGeneration, err := h.wsvc.RunOwnership(r.Context(), wkr, runID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run ownership", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	// PRD #1392 M1 (D10): recovery_retry_not_before rides the ownership probe so a worker
	// reconciling an unknown forge-park outcome (a transport failure after the report was sent)
	// can still quote the acknowledged retry time on its feed event. Omitted (nil) for a run
	// that is not recovery-parked.
	//
	// PRD #1391 Run B M4: claim_generation rides the same probe (additive) so the run-lane claim
	// router can proceed ONLY on a claimed/running row AT the claim's generation, and end the
	// attempt (no report) on a terminal status or a DIFFERENT generation.
	body := map[string]any{"status": status, "claim_generation": claimGeneration}
	if recoveryRetryNotBefore != nil {
		body["recovery_retry_not_before"] = recoveryRetryNotBefore
	}
	httpx.JSON(w, http.StatusOK, body)
}

// Message-gaps read bounds (PRD #1391 Run B M3c). maxMessageGapsThrough caps the `through` query
// param well below math.MaxInt32 so the query's `through+1` sentinel and the int32 casts never
// overflow; it is far above any real run's message count. defaultMessageGapsLimit /
// maxMessageGapsLimit bound the page size — the default when `limit` is omitted, the hard ceiling
// a larger value is clamped to.
const (
	maxMessageGapsThrough   = 1 << 30
	defaultMessageGapsLimit = 256
	maxMessageGapsLimit     = 1024
)

// WorkerRunMessageGaps returns the MISSING message-seq ranges in [1..through] for a run this worker
// holds at its current, unreleased claim (PRD #1391 Run B M3c). Worker-authenticated, run-scoped,
// generation-fenced and keyset-paginated: required `claim_generation` (>=0), `through` (>=0, <= a
// cap), `limit` (default 256, hard-capped) and `cursor` (the previous page's next_cursor). A
// foreign worker, or a stale/released flight, is 404 (ErrRunNotOwned) — it must not inspect or fill
// a newer flight's gaps. Response: {"gaps":[{"first":F,"last":L},...], "next_cursor":<seq or omitted>}.
func (h *Handler) WorkerRunMessageGaps(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	// claim_generation: required and non-negative. The journal generation is what prevents an old
	// same-worker flight from inspecting a newer reclaim's gaps; this endpoint and terminal_fence
	// ship together, so there is no legacy generation-less caller to admit.
	claimGeneration, perr := strconv.ParseInt(r.URL.Query().Get("claim_generation"), 10, 64)
	if perr != nil || claimGeneration < 0 {
		httpx.Error(w, http.StatusBadRequest, "claim_generation must be a non-negative integer")
		return
	}
	// through: required, >= 0, <= the cap (a bounded window the keyset walks).
	through := int64(0)
	if raw := r.URL.Query().Get("through"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < 0 || n > maxMessageGapsThrough {
			httpx.Error(w, http.StatusBadRequest, "through must be an integer in [0, 2^30]")
			return
		}
		through = n
	}
	// cursor: the seq keyset value to resume after; >= 0. 0 (the default) starts from the head.
	cursor := int64(0)
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n < 0 || n > maxMessageGapsThrough {
			httpx.Error(w, http.StatusBadRequest, "cursor must be an integer in [0, 2^30]")
			return
		}
		cursor = n
	}
	// limit: default when omitted, clamped to the hard ceiling; a value <= 0 is invalid.
	limit := int64(defaultMessageGapsLimit)
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || n <= 0 {
			httpx.Error(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		if n > maxMessageGapsLimit {
			n = maxMessageGapsLimit
		}
		limit = n
	}
	page, err := h.wsvc.RunMessageGaps(r.Context(), wkr, runID, claimGeneration, through, cursor, limit)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run message gaps", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	body := map[string]any{"gaps": page.Gaps}
	if page.NextCursor != nil {
		body["next_cursor"] = *page.NextCursor
	}
	httpx.JSON(w, http.StatusOK, body)
}

// WorkerRunOrphanClassification classifies a clone orphan's OWNER run (issue #1319): the
// {id} path param is the CLAIMANT run the worker currently holds (the authz anchor + source
// of the current repo), and the ?owner query param is the orphan's owner run id. The service
// scopes the read to the worker's OWNER (user) + the claimant's repo — NOT worker_id — so a
// terminal owner that moved workers is still found. ErrRunNotOwned (claimant not held, a
// repo-less claimant, or no matching owner run) maps to 404, mirroring WorkerRunOwnership.
func (h *Handler) WorkerRunOrphanClassification(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	claimantRunID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	ownerRunID, err := uuid.Parse(r.URL.Query().Get("owner"))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid owner run id")
		return
	}
	id, err := h.wsvc.RunOrphanClassification(r.Context(), wkr, claimantRunID, ownerRunID)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run orphan classification", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{
		"status": id.Status, "repo_id": id.RepoID.String(), "kind": id.Kind,
		"issue_iid": id.IssueIID, "branch": id.Branch, "pipeline_ref": id.PipelineRef, "pipeline_id": id.PipelineID,
	})
}

// WorkerRunCompletionPermit is the completion-interlock permit endpoint (PRD #1226 M2, D4/D5):
// the worker requests a permit bound to the frozen contract revision, the source branch, and the
// EXACT final head. The service applies the claim fence, recomputes the unmet structural
// criteria server-side, and either issues an idempotent permit or returns a structured
// non-terminal denial. Mirrors WorkerRunState's decode/auth pattern.
func (h *Handler) WorkerRunCompletionPermit(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		ContractRevision int    `json:"contract_revision"`
		Branch           string `json:"branch"`
		Head             string `json:"head"`
		// PRD #1247 M5 (D3): the claim generation the worker holds. A CAPABILITY worker stamps it so
		// RequestCompletionPermit's fence refuses to issue a permit for a released/superseded stale
		// flight. Nullable + OPTIONAL (a legacy worker omits it, unfenced), but the field must exist
		// here because httpx.DecodeJSON rejects unknown fields.
		ClaimGeneration *int64 `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	branch := strings.TrimSpace(body.Branch)
	head := strings.TrimSpace(body.Head)
	if body.ContractRevision < 1 || branch == "" || head == "" {
		httpx.Error(w, http.StatusBadRequest, "contract_revision (>=1), branch and head are required")
		return
	}
	res, err := h.wsvc.RequestCompletionPermit(r.Context(), wkr, runID, workersvc.CompletionPermitRequest{
		ContractRevision: body.ContractRevision, Branch: branch, Head: head, ClaimGeneration: body.ClaimGeneration,
	})
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrCompletionStaleClaim):
			// PRD #1247 M5: the generation fence refused — a held-state switch RELEASED this claim or
			// a reclaim SUPERSEDED it, so no permit is issued for the stale flight. 409, non-terminal.
			httpx.Error(w, http.StatusConflict, "run is not in a live claimed state for this worker")
		case errors.Is(err, workersvc.ErrMissingClaimGeneration):
			// PRD #1247 M5: a CAPABILITY worker omitted claim_generation, so the fence could not
			// engage. Refuse (409) rather than issue an unfenced permit, mirroring WorkerRunState.
			httpx.Error(w, http.StatusConflict, "this worker must stamp claim_generation on the completion permit request")
		default:
			slog.Error("worker run completion permit", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// WorkerRunCompletionAttempt is the same-lead nudge endpoint (PRD #1226 M2, D4; M3 caller): the
// worker reports the current head + worktree fingerprint, the server recomputes the unmet set
// and records a bounded attempt, and returns the server-authoritative unmet set + attempt count.
func (h *Handler) WorkerRunCompletionAttempt(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		// PRD #1226 M3: the lead's signal_done declaration. The service subset-validates it
		// against the frozen list and union-merges it into runs.milestones_completed before
		// recomputing unmet. Omitted/null ⇒ nothing declared this attempt (union no-op).
		MilestonesCompleted []string `json:"milestones_completed"`
		Head                string   `json:"head"`
		WorktreeFingerprint string   `json:"worktree_fingerprint"`
		// PRD #1247 M5 (D3): the claim generation the worker holds. A CAPABILITY worker stamps it so
		// the RecordCompletionAttempt fence refuses a released/superseded stale flight's attempt.
		// Nullable + OPTIONAL, but the field must exist because httpx.DecodeJSON rejects unknown fields.
		ClaimGeneration *int64 `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	res, err := h.wsvc.RecordCompletionAttempt(r.Context(), wkr, runID, workersvc.CompletionAttemptRequest{
		MilestonesCompleted: body.MilestonesCompleted,
		Head:                strings.TrimSpace(body.Head),
		WorktreeFingerprint: strings.TrimSpace(body.WorktreeFingerprint),
		ClaimGeneration:     body.ClaimGeneration,
	})
	if err != nil {
		switch {
		case errors.Is(err, workersvc.ErrRunNotOwned):
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
		case errors.Is(err, workersvc.ErrCompletionStaleClaim):
			httpx.Error(w, http.StatusConflict, "run is not in a live claimed state for this worker")
		case errors.Is(err, workersvc.ErrMissingClaimGeneration):
			// PRD #1247 M5: a CAPABILITY worker omitted claim_generation, so the fence could not
			// engage. Refuse (409) rather than record an unfenced attempt, mirroring WorkerRunState.
			httpx.Error(w, http.StatusConflict, "this worker must stamp claim_generation on the completion attempt")
		case errors.Is(err, workersvc.ErrCompletionNotInterlocked):
			httpx.Error(w, http.StatusBadRequest, "run is not interlocked")
		default:
			slog.Error("worker run completion attempt", "error", err)
			httpx.Error(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	httpx.JSON(w, http.StatusOK, res)
}

// WorkerRunCompletionHold is the completion-interlock HOLD endpoint (PRD #1226 M4, D6): the worker
// reports the head it captured at hold time, and the service parks an owned, interlocked run with
// a recorded completion attempt (running/awaiting_input -> paused, hold_reason='completion_blocked').
//
// It mirrors WorkerRunState's applied/409 shape EXACTLY, and that shape is load-bearing for the
// park-order ack contract: on success it returns 200 with the paused run, and on a REFUSED hold it
// returns 409 with the run's ACTUAL status. That status is usually non-paused (a still-live run the
// guard rejected, or one that moved to queued/terminal), but it CAN be `paused`: an idempotent
// retry after a hold already landed — or a run an owner already paused — re-reads as `paused` while
// the guard (source running/awaiting_input) legitimately refuses. The worker keys its cleanup
// (removes the clone / plugin dir / HOME) off a `paused` status; a `paused` 409 is SAFE, not a bug,
// because the run IS already held and cleaning up an already-held run is idempotent (a later resume
// re-clones). A non-paused body means retain the run live. (A reclaim surfaces as ErrRunNotOwned ->
// 404, which the worker never reads as `paused`.)
func (h *Handler) WorkerRunCompletionHold(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		// The exact head the worker captured at hold time. Empty is allowed (the column is
		// nullable); the service NUL-strips + TrimSpaces it, matching the permit path.
		Head string `json:"head"`
		// PRD #1247 M5 (D3): the claim generation the worker holds. A CAPABILITY worker stamps it so
		// the hold's fence refuses to park a released/superseded stale flight's reclaimed run.
		// Nullable + OPTIONAL, but the field must exist because httpx.DecodeJSON rejects unknown fields.
		ClaimGeneration *int64 `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, applied, err := h.wsvc.SetRunCompletionHold(r.Context(), wkr, runID, body.Head, body.ClaimGeneration)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		if errors.Is(err, workersvc.ErrMissingClaimGeneration) {
			// PRD #1247 M5: a CAPABILITY worker omitted claim_generation, so the hold's fence could
			// not engage. Refuse (409) rather than park unfenced, mirroring WorkerRunState.
			httpx.Error(w, http.StatusConflict, "this worker must stamp claim_generation on the completion hold")
			return
		}
		slog.Error("worker run completion hold", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !applied {
		// The hold's guard refused (wrong status, not interlocked, or no recorded completion
		// attempt): 409 with the run's REAL status, which the worker reads off the body and
		// retains the run on (it cleans up ONLY on a `paused` ack).
		httpx.JSON(w, http.StatusConflict, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock())})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock())})
}

// WorkerRunWallPark is the wall-clock PARK endpoint (PRD #1497 M1, D4/D15): the worker reports it
// dropped its turn at the run's deadline and captured the tree, and the service parks the run
// (running -> paused, hold_reason='budget_exhausted') via the fenced SetRunWallPark whose SOLE
// authority is the three-term deadline. It mirrors WorkerRunCompletionHold's applied/409 shape,
// which is load-bearing for the park-order ack contract: 200 with the paused run on success; a
// 409 with the run's ACTUAL status when the park is REFUSED — usually `running` (the owner extended
// in the request-then-park window, so the worker clears its sticky wall mode, lifts its wall from
// the served total, and restarts the turn). An idempotent report after the SERVER already parked the
// row (ParkRunsAtWall beat the worker) is answered 200/`paused` (D16), and a captured head it
// carries is kept via RecordWallParkCapturedHead. A reclaim surfaces as ErrRunNotOwned -> 404.
func (h *Handler) WorkerRunWallPark(w http.ResponseWriter, r *http.Request) {
	wkr, ok := mw.WorkerFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "worker authentication required")
		return
	}
	runID, ok := httpx.PathUUID(w, r, "id", "run")
	if !ok {
		return
	}
	var body struct {
		// The exact head the worker captured at park time. Empty is allowed (the column is
		// nullable; a degraded park reports it null); the service NUL-strips + TrimSpaces it.
		Head string `json:"head"`
		// Published reports whether the checkpoint was published to the forge (informational for the
		// degraded-park feed message, M2). The park transition itself does not depend on it — a
		// verified LOCAL capture makes the park (D4). Present so DecodeJSON does not reject it.
		Published bool `json:"published"`
		// PRD #1497 M1 (D16): the claim generation the worker holds. A wall_park_v1 worker stamps it
		// so the fence refuses a released/superseded stale flight's reclaimed run. Nullable + OPTIONAL.
		ClaimGeneration *int64 `json:"claim_generation"`
	}
	if err := httpx.DecodeJSON(r, &body); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	run, applied, err := h.wsvc.ReportWallPark(r.Context(), wkr, runID, body.Head, body.Published, body.ClaimGeneration)
	if err != nil {
		if errors.Is(err, workersvc.ErrRunNotOwned) {
			httpx.Error(w, http.StatusNotFound, "run not found for this worker")
			return
		}
		slog.Error("worker run wall park", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !applied {
		// The park was refused (the owner extended in the window, or the run moved on): 409 with the
		// run's REAL status, which the worker reads off the body to clear the wall mode and restart.
		httpx.JSON(w, http.StatusConflict, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock())})
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"run": runToDTO(run, h.runPriorityClass(r.Context(), run), h.cfg.RunTimeout, h.runExtensionCapSeconds(r.Context()), h.cfg.RunForgeUnreachableMaxParks, h.clock())})
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
