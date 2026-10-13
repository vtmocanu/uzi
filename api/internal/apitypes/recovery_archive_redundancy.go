package apitypes

// RecoveryArchiveRedundancyRequest is the worker's claim, POSTed to
// /api/worker/runs/{id}/archives/{captureID}/redundancy (issue #2625), that every object a
// completed run's guarded recovery archive retains is already reachable from the run's published
// branch. The worker supplies the archive's inventory; the api proves publication itself through
// the forge and trusts nothing the worker says about it.
//
// Every SHA is a 40-char lowercase hex commit/tree id. Roots are strictly sorted and unique.
// CoverageDigest is sha256 of the exact JSON {"roots":[...],"currentSha":"...","tree":"..."} the
// worker's coverage builder hashes. AggregateObjects are the raw commit bodies (standard base64,
// unpadded forms refused) of the synthetic aggregate and round commits whose root is the archived
// source_sha; CurrentObject is the raw body of CurrentSha. WIP is present only when CurrentSha is
// a parked work-in-progress commit that may not be on the published branch.
type RecoveryArchiveRedundancyRequest struct {
	Generation       int64                         `json:"generation"`
	CoverageDigest   string                        `json:"coverage_digest"`
	Roots            []string                      `json:"roots"`
	CurrentSha       string                        `json:"current_sha"`
	Tree             string                        `json:"tree"`
	AggregateObjects []string                      `json:"aggregate_objects"`
	CurrentObject    string                        `json:"current_object"`
	WIP              *RecoveryArchiveRedundancyWIP `json:"wip,omitempty"`
}

// RecoveryArchiveRedundancyWIP carries the witness for a parked WIP commit: the raw body of a
// commit on the published branch whose tree equals the archive's tree, so the WIP content is
// reachable even though the WIP commit itself is not.
type RecoveryArchiveRedundancyWIP struct {
	WitnessObject string `json:"witness_object"`
}

// Outcomes and bounded reasons of a redundancy claim. Outcome is expired only when this call (or
// an identical earlier one) recorded the proof and removed the archive bytes; every other answer
// is retained with one reason and leaves the archive on its own retention window.
const (
	RecoveryRedundancyExpired  = "expired"
	RecoveryRedundancyRetained = "retained"

	RecoveryRedundancyNotCompleted      = "not_completed"
	RecoveryRedundancyBindingMismatch   = "binding_mismatch"
	RecoveryRedundancyDigestMismatch    = "digest_mismatch"
	RecoveryRedundancySourceMismatch    = "source_mismatch"
	RecoveryRedundancyTreeMismatch      = "tree_mismatch"
	RecoveryRedundancyBadObject         = "bad_object"
	RecoveryRedundancyNotAvailable      = "not_available"
	RecoveryRedundancyIdentityMissing   = "identity_missing"
	RecoveryRedundancyIdentityChanged   = "identity_changed"
	RecoveryRedundancyMRMissing         = "mr_missing"
	RecoveryRedundancyBranchMissing     = "branch_missing"
	RecoveryRedundancyBranchMismatch    = "branch_mismatch"
	RecoveryRedundancyHeadMismatch      = "head_mismatch"
	RecoveryRedundancyNotAncestor       = "not_ancestor"
	RecoveryRedundancyUncoveredRoot     = "uncovered_root"
	RecoveryRedundancyUncoveredWIP      = "uncovered_wip"
	RecoveryRedundancyUncoveredPrereq   = "uncovered_prerequisite"
	RecoveryRedundancyAncestryUnknown   = "ancestry_unknown"
	RecoveryRedundancyForgeTimeout      = "forge_timeout"
	RecoveryRedundancyInventoryTooLarge = "inventory_too_large"
	RecoveryRedundancyCoolingDown       = "cooling_down"
	RecoveryRedundancyNotExpired        = "not_expired"
)

// RecoveryArchiveRedundancyResponse is the api's answer to a RecoveryArchiveRedundancyRequest.
type RecoveryArchiveRedundancyResponse struct {
	CaptureID string `json:"capture_id"`
	Outcome   string `json:"outcome"`
	Reason    string `json:"reason,omitempty"`
}
