package apitypes

import "time"

// Durable run recovery (PRD #1296). This file freezes the OWNER-facing web DTOs (the
// SPA/CLI read side, wire-contract-pinned like every other DTO) and the WORKER-facing
// archive RPC request/response shapes M3 consumes (mirrored in agent/src/protocol.ts).
// Raw archive bytes NEVER appear here — only metadata (D6). Adding these grows no
// existing DTO: RunDTO is untouched, so the run-list wire is unchanged.

// RecoveryArchiveDTO is one owner-visible capture's metadata (PRD #1296 D7). It carries
// NO raw bytes and no idempotency key — only what the run page and `uzi run export`
// render: the capture id, its run, lifecycle state, the original committed head H
// (source_sha), the provenance attempted head H' (attempted_head_sha, absent when the run
// never attempted a publish), the byte manifest facts (byte_size/checksum, absent until
// bound), a bounded sanitized reason, the resolved prerequisite closure, and the ready
// expiry (absent until the artifact is available). Every optional field is omitempty, so
// a zero value marshals with no null (the finding.zero.json shape).
type RecoveryArchiveDTO struct {
	ID               string     `json:"id"`
	RunID            string     `json:"run_id"`
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
}
