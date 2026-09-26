package apitypes

import "time"

// Durable run recovery (PRD #1296). This file freezes the OWNER-facing web DTOs (the
// SPA/CLI read side, wire-contract-pinned like every other DTO) and the WORKER-facing
// archive RPC request/response shapes M3 consumes (mirrored in agent/src/protocol.ts).
// Raw archive bytes NEVER appear here — only metadata (D6). Adding these grows no
// existing DTO: RunDTO is untouched, so the run-list wire is unchanged.

// RecoveryArchiveDTO is one owner-visible capture's metadata (PRD #1296 D7). It carries
// NO raw bytes and no idempotency key — only what the run page and `uzi run export`
// render: the capture id, its run, the custody hold it was reserved under (hold_id, so
// `uzi run recovery --json` can list each hold's captures, #1417), lifecycle state, the
// original committed head H (source_sha), the provenance attempted head H' (attempted_head_sha, absent when the run
// never attempted a publish), the byte manifest facts (byte_size/checksum, absent until
// bound), a bounded sanitized reason, the resolved prerequisite closure, and the ready
// expiry (absent until the artifact is available). Every optional field is omitempty, so
// a zero value marshals with no null (the finding.zero.json shape).
type RecoveryArchiveDTO struct {
	ID               string     `json:"id"`
	RunID            string     `json:"run_id"`
	HoldID           string     `json:"hold_id"`
	State            string     `json:"state"`
	SourceSha        string     `json:"source_sha"`
	AttemptedHeadSha *string    `json:"attempted_head_sha,omitempty"`
	ByteSize         *int64     `json:"byte_size,omitempty"`
	Checksum         string     `json:"checksum,omitempty"`
	Reason           string     `json:"reason,omitempty"`
	PrerequisiteShas []string   `json:"prerequisite_shas,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

// RecoveryArchiveStateCountsDTO is the per-state capture tally on a run's recovery
// summary (PRD #1296 D7). It is a fixed struct (not a map) so the wire shape is a closed,
// always-present object of the six capture lifecycle states — M5 renders each count
// without probing for a key.
type RecoveryArchiveStateCountsDTO struct {
	Preparing   int `json:"preparing"`
	Uploading   int `json:"uploading"`
	Available   int `json:"available"`
	NeedsAction int `json:"needs_action"`
	Expired     int `json:"expired"`
	Discarded   int `json:"discarded"`
}

// RecoveryArchiveSummaryDTO is the per-run recovery aggregate (PRD #1296 D7). It is what
// M5 renders WITHOUT gating on captures.length>=1: Supported is true iff a custody hold
// ever existed for the run (its absence is the honest legacy/unsupported case, surfaced as
// Legacy); HasOpenHold is the pending-custody signal shown while capture is still in
// flight; Counts is the closed per-state tally; and Archives is the full retained-capture
// list (all states). Archives is ALWAYS a JSON array on the wire, never null — the M2
// summary endpoint returns [] when the run has none (the ci_run_detail.jobs contract
// shape), so the TS type is a never-null array and the nil-slice zero marshal is exempted
// in apiContract.test.ts.
type RecoveryArchiveSummaryDTO struct {
	Supported   bool                          `json:"supported"`
	Legacy      bool                          `json:"legacy"`
	HasOpenHold bool                          `json:"has_open_hold"`
	Counts      RecoveryArchiveStateCountsDTO `json:"counts"`
	Archives    []RecoveryArchiveDTO          `json:"archives"`
}

// ── Worker-facing archive RPC (PRD #1296 M3 consumes; frozen here, D8) ────────────────
// These are the reserve/upload/status/release shapes the worker exchanges with the API's
// Bearer-authenticated archive endpoints (M2 implements the handlers). They are mirrored
// verbatim in agent/src/protocol.ts. They are NOT web DTOs: an admin/owner never sees
// them, so they are pinned by wire_test.go tag tests, not by the SPA api-contract fixtures.

// RecoveryReserveRequest is the worker's request to reserve (or idempotently re-reserve) a
// capture under its run's open custody hold (D2). SourceSha is the original committed head
// H; AttemptedHeadSha is the provenance H' (omitted when no publish was attempted);
// IdempotencyKey is the worker's durable source-journal identity, so a lost ACK re-reserves
// the SAME capture rather than duplicating it.
type RecoveryReserveRequest struct {
	RunID            string `json:"run_id"`
	IdempotencyKey   string `json:"idempotency_key"`
	SourceSha        string `json:"source_sha"`
	AttemptedHeadSha string `json:"attempted_head_sha,omitempty"`
	// Generation is the exact claim generation this capture belongs to (PRD #1349 M1). A v2
	// worker sends it so the reserve binds to the ONE hold it took at that generation; a v1
	// worker omits it, and the server binds under the caller's SOLE open hold — refusing
	// (ErrAmbiguous) rather than guessing when more than one open hold exists (M4). The call
	// sites that populate it are M2's.
	Generation *int64 `json:"generation,omitempty"`
}

// RecoveryReserveResponse is the reserve ACK: the server-minted capture id and its current
// lifecycle state ('preparing' on a fresh reserve, or the existing state on an idempotent
// retry).
type RecoveryReserveResponse struct {
	CaptureID string `json:"capture_id"`
	State     string `json:"state"`
}

// RecoveryUploadManifest is the byte-manifest the worker binds ONCE (compare-and-set, D2)
// before/at the streaming upload of the verified bundle. ByteSize and Checksum are the
// complete-bundle facts the server verifies; ChunkCount is the expected ordered-chunk
// inventory; PrerequisiteShas is the verified public prerequisite closure the bundle
// imports against (D5). The bundle bytes themselves stream as the request body — they are
// never carried in JSON.
type RecoveryUploadManifest struct {
	ByteSize         int64    `json:"byte_size"`
	Checksum         string   `json:"checksum"`
	ChunkCount       int      `json:"chunk_count"`
	PrerequisiteShas []string `json:"prerequisite_shas,omitempty"`
}

// RecoveryCaptureStatusResponse is the worker's by-id status poll of a capture (D2: by-ID
// inspection plus idempotent upload handle lost ACKs). ManifestBound tells the worker
// whether the byte manifest is already bound (so a restart knows whether to re-bind);
// ByteSize/Checksum/ExpiresAt surface the bound manifest and ready expiry when present;
// Reason carries a bounded sanitized needs_action/error reason.
type RecoveryCaptureStatusResponse struct {
	CaptureID     string     `json:"capture_id"`
	State         string     `json:"state"`
	ManifestBound bool       `json:"manifest_bound"`
	ByteSize      *int64     `json:"byte_size,omitempty"`
	Checksum      string     `json:"checksum,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
}

// RecoveryReleaseResponse is the release ACK for a run's custody (D3): Released is true
// when the call transitioned any open hold to released, and HoldsReleased is how many open
// holds it settled (0 on an idempotent repeat once none remain open).
type RecoveryReleaseResponse struct {
	RunID         string `json:"run_id"`
	Released      bool   `json:"released"`
	HoldsReleased int    `json:"holds_released"`
	// Retained is true when the server LEFT a hold open pending owner attention rather than
	// releasing it (PRD #1349 M1) — the v1/ambiguous case where the worker could not prove
	// its generation's work is durable. Reason is a bounded server reason when retained.
	// Both omitempty, so a clean release marshals neither.
	Retained bool   `json:"retained,omitempty"`
	Reason   string `json:"reason,omitempty"`
	// Generation is the released generation the server ECHOES back on the v2 exact release path
	// (PRD #1392 M1), set only when a hold was actually released (n>0) so the worker can confirm
	// the server settled the exact generation it asked to release. Omitempty and a pointer, so a
	// v1/idempotent-no-op release marshals nothing.
	Generation *int64 `json:"generation,omitempty"`
}

// RecoveryReleaseRequest is the worker's request to settle custody for the EXACT generation
// it names (PRD #1349 M1, D1/D2). A v2 worker sends Generation so the server releases only
// the hold it took at that claim generation, never a newer same-worker generation whose
// source it did not inherit; a v1 worker omits it (the server settles by run+worker). The
// call sites that populate it are M2's — M1 only freezes the shape.
type RecoveryReleaseRequest struct {
	Generation *int64 `json:"generation,omitempty"`
	// ReleaseEvidence is the worker's DECLARATION of WHY this release is warranted (PRD #1392
	// M1, D3). UNTRUSTED: the server allowlists it to {"publication","forge_no_output"} — the
	// two dispositions a worker's release endpoint may legitimately assert — and treats
	// anything else as absent (the release is refused with a 400 in the handler, so a garbled
	// value can never stamp a bogus class). Absent (nil) on a v1 worker; the server then stamps
	// no explicit evidence. The other three classes are server-derived, never worker-supplied:
	// "publication" also on the completed-run release, "archive" on the reconciler,
	// "owner_discard" on the owner discard, and "no_adopted_source" on the pre-clone park.
	ReleaseEvidence *string `json:"release_evidence,omitempty"`
}

// RecoverySettleRequest is the worker's request that the api settle ONE older-generation
// custody hold on a completed run by ANCESTRY (issue #1582 M1): the predecessor generation's
// work was adopted by a same-worker successor that then completed. The worker supplies
// CANDIDATE SHAs only: PushedSha is the SUCCESSOR generation's acknowledged pushed head (the
// head the completing generation pushed and landed, persisted by the worker before its terminal
// report); SourceSha is the PREDECESSOR generation's journaled source; AdoptedSha is the tip the
// successor adopted. The api proves, through the forge compare API alone, that each is an
// ancestor of (or equal to) the completed branch head. There is deliberately NO field for the
// worker's own ancestry verdict: the handler decodes strictly, so an extra field (for example
// "ancestry":"ancestor") is a 400. Every SHA must be a 40-char lowercase hex commit id.
type RecoverySettleRequest struct {
	PredecessorGeneration int64  `json:"predecessor_generation"`
	SuccessorGeneration   int64  `json:"successor_generation"`
	PushedSha             string `json:"pushed_sha"`
	SourceSha             string `json:"source_sha"`
	AdoptedSha            string `json:"adopted_sha"`
}

// Settle outcomes and retained reasons (issue #1582 M1). Outcome is released only when the
// named hold is (now, or already by an identical earlier settle) released with 'ancestry'
// evidence; every other answer is retained with one bounded reason.
const (
	RecoverySettleReleased = "released"
	RecoverySettleRetained = "retained"

	// RecoverySettleAncestryUnknown: the forge could not prove ancestry (branch head
	// unreadable, a rate limit, a redirect, an error, or an inconclusive answer). Transient:
	// the worker may retry later.
	RecoverySettleAncestryUnknown = "ancestry_unknown"
	// RecoverySettleNotAncestor: the forge explicitly reported a candidate NOT contained in
	// the completed branch head.
	RecoverySettleNotAncestor = "not_ancestor"
	// RecoverySettleStateChanged: the run or hold changed between the proof and the guarded
	// release, so nothing was released.
	RecoverySettleStateChanged = "state_changed"
	// RecoverySettleNotEligible: the run/hold is not an older-generation hold of this worker
	// on a run this worker completed at the named successor generation, the run's branch is
	// not a valid git branch name, an interlocked run has no consumed completion permit to
	// bind to (or that permit's head is not a 40-char lowercase hex commit id), or the hold
	// was already settled with a different identity.
	RecoverySettleNotEligible = "not_eligible"
	// RecoverySettleCandidateMismatch: a candidate SHA contradicts a fact the server already
	// holds for this hold or run (issue #1582 M1 rework): source_sha is not the source_sha of
	// any recovery capture registered under the hold before the successor generation claimed
	// (a later capture is ignored), or pushed_sha is not the head of the
	// completion permit the run's (interlocked) completion consumed. Terminal: retrying the
	// same candidates can never succeed.
	RecoverySettleCandidateMismatch = "candidate_mismatch"
	// RecoverySettleBranchMissing: the forge reports the completed branch does not exist (a
	// 404 on the branch read), so there is no head to prove against. Terminal for the worker.
	RecoverySettleBranchMissing = "branch_missing"
)

// RecoveryLiveSettleRequest is the worker's request that the api settle ONE older-generation
// custody hold while the SUCCESSOR generation of the same run is still LIVE on the same worker
// (issue #1751 M2), POSTed to /api/worker/runs/{id}/recovery-holds/{holdID}/settle-live.
// PublishedSha is the head the successor generation published to Target; SourceSha is the
// PREDECESSOR generation's journaled source; AdoptedSha is the tip the successor adopted.
// Target names the published target the api reads the head of: RecoverySettleTargetCheckpoint
// (the run's forge checkpoint ref, refs/uzi-checkpoints/<branch the api derives from the run
// row>) or RecoverySettleTargetBranch (the run's branch). The api proves, through the forge
// alone, that each SHA is an ancestor of (or equal to) that head. Like RecoverySettleRequest
// there is NO field for the worker's own verdict (strict decode: an extra field is a 400), and
// every SHA must be a 40-char lowercase hex commit id. The answer is a RecoverySettleResponse.
type RecoveryLiveSettleRequest struct {
	PredecessorGeneration int64  `json:"predecessor_generation"`
	SuccessorGeneration   int64  `json:"successor_generation"`
	PublishedSha          string `json:"published_sha"`
	SourceSha             string `json:"source_sha"`
	AdoptedSha            string `json:"adopted_sha"`
	Target                string `json:"target"`
}

// The published targets a live settle may be proven against (issue #1751 M2).
const (
	RecoverySettleTargetCheckpoint = "checkpoint"
	RecoverySettleTargetBranch     = "branch"
)

// RecoverySettleResponse is the api's answer to a RecoverySettleRequest (issue #1582 M1).
// FinalHeadSha is the branch head the api proved against, set only on a release.
type RecoverySettleResponse struct {
	RunID        string `json:"run_id"`
	HoldID       string `json:"hold_id"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
	FinalHeadSha string `json:"final_head_sha,omitempty"`
}

// RecoveryHoldDTO is one open custody hold this worker holds on a run, in the worker-facing
// post-clone inventory (PRD #1349 M1, D3). HoldID + Generation are the exact hold identity;
// HasAvailableCapture is true when a ready archive already covers this hold's source, and
// CaptureState is the latest capture's lifecycle state (empty when the hold has no capture yet).
// The worker uses this to decide, per generation, whether its source is already durable
// before it re-attempts capture/release.
type RecoveryHoldDTO struct {
	HoldID              string `json:"hold_id"`
	Generation          int64  `json:"generation"`
	HasAvailableCapture bool   `json:"has_available_capture"`
	CaptureState        string `json:"capture_state,omitempty"`
}

// RecoveryHoldsResponse is the worker's post-clone hold inventory for one run (PRD #1349 M1,
// D3). Holds is ALWAYS a JSON array, never null — the service initializes it to
// []RecoveryHoldDTO{} so the worker iterates it unconditionally.
type RecoveryHoldsResponse struct {
	RunID string            `json:"run_id"`
	Holds []RecoveryHoldDTO `json:"holds"`
}

// ── Owner-facing custody-hold DTOs (PRD #1349 M1 D7; SPA/CLI read side, api-contract
// fixture-pinned like the archive DTOs above, mirrored in web/src/lib/apiTypes.ts) ─────────

// RecoveryCustodyHoldDTO is one owner-visible custody hold. It NEVER carries
// original_worker_identity (raw provenance is owner-hidden, D7) — only the OPAQUE worker id
// and a bounded owner-safe display name. Attention is a SERVER-DERIVED action/attention state
// DISTINCT from any capture State: its intended vocabulary is
//
//	active | capturing | archive_ready | needs_action | source_only | released | discarded
//
// M4/M5 compute it; in M1 it is a documented placeholder that stays "" (the zero fixture
// carries ""). HasAvailableCapture is true when a ready archive already covers this hold's
// source; CaptureState is the latest capture's lifecycle state (empty when the hold has none).
// ReleasedAt is null while the hold is open.
type RecoveryCustodyHoldDTO struct {
	ID                  string     `json:"id"`
	RunID               string     `json:"run_id"`
	Generation          int64      `json:"generation"`
	State               string     `json:"state"`
	Attention           string     `json:"attention"`
	WorkerID            string     `json:"worker_id"`
	WorkerName          string     `json:"worker_name,omitempty"`
	HasAvailableCapture bool       `json:"has_available_capture"`
	CaptureState        string     `json:"capture_state,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	ReleasedAt          *time.Time `json:"released_at,omitempty"`
}

// RecoveryCustodyAggregateDTO is the owner-level custody summary the board alert and the
// one-per-episode Slack DM read (PRD #1349 M1 D6/D10). OpenHolds is the owner's unresolved
// (state='open') hold count; CustodyHoldLimit is the configured per-owner admission ceiling;
// DecisionNeeded is how many holds await an owner decision; BlockedRuns is the count of the
// owner's queued code-publishing runs blocked by the custody-admission gate. All four are
// plain ints (0 is meaningful), always on the wire.
type RecoveryCustodyAggregateDTO struct {
	OpenHolds        int `json:"open_holds"`
	CustodyHoldLimit int `json:"custody_hold_limit"`
	DecisionNeeded   int `json:"decision_needed"`
	BlockedRuns      int `json:"blocked_runs"`
}

// RecoveryCustodyHoldsDTO is the owner GET /api/recovery/holds response (PRD #1349 M1 D7). M5
// populates the endpoint; the shape is frozen here. Holds is ALWAYS a JSON array, never null.
type RecoveryCustodyHoldsDTO struct {
	Aggregate RecoveryCustodyAggregateDTO `json:"aggregate"`
	Holds     []RecoveryCustodyHoldDTO    `json:"holds"`
}
