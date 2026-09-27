package apitypes

import "time"

// PRD #1798: the plain-English PR description artifact. These are the wire shapes the worker
// (agent/src/protocol.ts), the web run view and the CLI consume. JSON is snake_case.

// PrDescriptionFields is the model- or lead-authored part of a PR description. On a stage
// request it is RAW and untrusted; every response carries the api-sanitized form, which is the
// only text the renderer may publish (D7). In a response every slice is non-nil.
type PrDescriptionFields struct {
	Summary        string                      `json:"summary"`
	Changes        []string                    `json:"changes"`
	ScopeNotes     []PrDescriptionScopeNote    `json:"scope_notes"`
	ReviewPointers []string                    `json:"review_pointers"`
	Verification   []PrDescriptionVerification `json:"verification"`
}

// PrDescriptionScopeNote is one difference from the ask. Kind is added|changed|dropped|deferred.
type PrDescriptionScopeNote struct {
	Kind string `json:"kind"`
	Text string `json:"text"`
}

// PrDescriptionVerification is one check the lead REPORTED (D5), with the local HEAD it was
// reported at. Result is pass|fail; VerifiedAtSha is 7-64 hex characters.
type PrDescriptionVerification struct {
	Command       string `json:"command"`
	Result        string `json:"result"`
	VerifiedAtSha string `json:"verified_at_sha"`
}

// PrDescriptionSizeBucket is one size-line bucket's line counts (D3).
type PrDescriptionSizeBucket struct {
	Added   int64 `json:"added"`
	Deleted int64 `json:"deleted"`
}

// PrDescriptionSize is the deterministic, worker-computed size line (D3) as data. Unavailable
// marks the "**Size:** unavailable" case (the attribute lookup failed), in which every bucket
// must be zero (a stage with a non-zero bucket is refused) and none is rendered.
type PrDescriptionSize struct {
	Unavailable bool                    `json:"unavailable"`
	Files       int64                   `json:"files"`
	Code        PrDescriptionSizeBucket `json:"code"`
	Tests       PrDescriptionSizeBucket `json:"tests"`
	Docs        PrDescriptionSizeBucket `json:"docs"`
	Config      PrDescriptionSizeBucket `json:"config"`
	Generated   PrDescriptionSizeBucket `json:"generated"`
	Vendored    PrDescriptionSizeBucket `json:"vendored"`
}

// PrDescriptionVersionDTO is one staged version (D9). MrIid is null until the version names its
// PR: it is set at stage for a refresh (the PR already exists), otherwise at bind.
// RenderedRegionSha256 is null until bound, and a published version always has it.
// PublishedAt is null unless state is published.
// Source is generated|lead_only|deterministic_only; State is pending|published|abandoned.
type PrDescriptionVersionDTO struct {
	ID                   string              `json:"id"`
	RunID                string              `json:"run_id"`
	ClaimGeneration      int64               `json:"claim_generation"`
	MrIid                *int64              `json:"mr_iid"`
	Fields               PrDescriptionFields `json:"fields"`
	Size                 *PrDescriptionSize  `json:"size"`
	BaseSha              string              `json:"base_sha"`
	HeadSha              string              `json:"head_sha"`
	TargetBranch         string              `json:"target_branch"`
	Source               string              `json:"source"`
	RenderedRegionSha256 *string             `json:"rendered_region_sha256"`
	State                string              `json:"state"`
	CreatedAt            time.Time           `json:"created_at"`
	PublishedAt          *time.Time          `json:"published_at"`
}

// PrDescriptionState is the per-PR record (D9): the CAS counter, the version whose region is on
// the forge (null before the first acknowledged publish), and the last write outcome
// (published|skipped_human_edit|skipped_no_region|skipped_malformed|skipped_snapshot_moved|
// write_failed, null before the first ack). It is the bind / lookup / ack response body and the
// claim's pr_description.
type PrDescriptionState struct {
	MrIid            int64                    `json:"mr_iid"`
	LockVersion      int64                    `json:"lock_version"`
	LastOutcome      *string                  `json:"last_outcome"`
	PublishedVersion *PrDescriptionVersionDTO `json:"published_version"`
}

// PrDescriptionStageRequest is POST /api/worker/runs/{id}/pr-description/stage. Fields are RAW
// (the api sanitizes them). MrIid is set when the PR already exists (a refresh); it must be the
// run's own PR (runs.mr_iid when set, else the PR the run first staged or bound for). A
// deterministic_only stage stores no text: its fields are cleared.
type PrDescriptionStageRequest struct {
	ClaimGeneration *int64              `json:"claim_generation"`
	Source          string              `json:"source"`
	Fields          PrDescriptionFields `json:"fields"`
	Size            *PrDescriptionSize  `json:"size"`
	BaseSha         string              `json:"base_sha"`
	HeadSha         string              `json:"head_sha"`
	TargetBranch    string              `json:"target_branch"`
	MrIid           *int64              `json:"mr_iid,omitempty"`
}

// PrDescriptionStageResponse carries the stored pending version, whose Fields are the sanitized
// fields the renderer publishes.
type PrDescriptionStageResponse struct {
	Version PrDescriptionVersionDTO `json:"version"`
}

// PrDescriptionBindRequest is POST /api/worker/runs/{id}/pr-description/bind: bind the run's
// pending version to its PR and record the sha256 (64 lowercase hex) of the exact region text the
// renderer will write. MrIid must be the run's own PR (as for stage). Idempotent for the same
// (version, mr_iid).
type PrDescriptionBindRequest struct {
	ClaimGeneration      *int64 `json:"claim_generation"`
	VersionID            string `json:"version_id"`
	MrIid                int64  `json:"mr_iid"`
	RenderedRegionSha256 string `json:"rendered_region_sha256"`
}

// PrDescriptionBindResponse is the bound version plus the PR's current state, so the worker can
// compare the forge region with the published version's rendered_region_sha256 before writing.
type PrDescriptionBindResponse struct {
	Version PrDescriptionVersionDTO `json:"version"`
	PR      PrDescriptionState      `json:"pr"`
}

// PrDescriptionLookupRequest is POST /api/worker/runs/{id}/pr-description/lookup: classify a
// region hash the worker read from the forge against the PR's versions.
type PrDescriptionLookupRequest struct {
	ClaimGeneration *int64 `json:"claim_generation"`
	MrIid           int64  `json:"mr_iid"`
	RegionSha256    string `json:"region_sha256"`
}

// PrDescriptionLookupResponse: Match is "published" (a published version rendered that region),
// "pending" (a pending version did: a lost ack), or "none" (an unknown protected region).
// MatchedVersionID names the matched version (null for none). PR is null when the PR has no
// pr_descriptions row yet.
type PrDescriptionLookupResponse struct {
	Match            string              `json:"match"`
	MatchedVersionID *string             `json:"matched_version_id"`
	PR               *PrDescriptionState `json:"pr"`
}

// PrDescriptionAckRequest is POST /api/worker/runs/{id}/pr-description/ack: the outcome of the
// forge write for a bound version, compare-and-swapped on ExpectedLockVersion. Outcome is
// published|skipped_human_edit|skipped_no_region|skipped_malformed|skipped_snapshot_moved|
// write_failed. ObservedRegionSha256, when set, is the hash of the region the worker read on the
// forge before writing; a pending version with that hash is acknowledged as published first
// (lost-ack recovery).
type PrDescriptionAckRequest struct {
	ClaimGeneration      *int64  `json:"claim_generation"`
	VersionID            string  `json:"version_id"`
	Outcome              string  `json:"outcome"`
	ExpectedLockVersion  int64   `json:"expected_lock_version"`
	ObservedRegionSha256 *string `json:"observed_region_sha256,omitempty"`
}

// PrDescriptionAckResponse is the PR's state after the ack. RecoveredVersionID names the pending
// version lost-ack recovery published first (null when none).
type PrDescriptionAckResponse struct {
	PR                 PrDescriptionState `json:"pr"`
	RecoveredVersionID *string            `json:"recovered_version_id"`
}

// RunPrDescriptionDTO is RunDTO.pr_description: the PUBLISHED version of the run's PR (the text
// that is on the forge), for the run view and `uzi run get`. Fields are api-sanitized but still
// untrusted display text: render them as plain text.
type RunPrDescriptionDTO struct {
	MrIid        int64               `json:"mr_iid"`
	Source       string              `json:"source"`
	Fields       PrDescriptionFields `json:"fields"`
	Size         *PrDescriptionSize  `json:"size"`
	BaseSha      string              `json:"base_sha"`
	HeadSha      string              `json:"head_sha"`
	TargetBranch string              `json:"target_branch"`
	PublishedAt  *time.Time          `json:"published_at"`
}
