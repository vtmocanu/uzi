package apitypes

// Terminal preservation (issue #4007): POST /api/worker/runs/{id}/terminal-preservation. The worker
// of a terminal run (or an earlier generation of a live one) uploads its final work as one pack and
// the API creates or fast-forwards refs/uzi-terminal/<run-id> with the stored credential. The
// repository, user, worker, ref and credential are derived server-side; the worker names only the
// generation, the tip, the tip the ref is expected to be at, and its coverage manifest.

// TerminalDispositionPreserve is the only disposition a worker may request. Whether the work is
// really pushed is the server's decision: a run whose issue or pull request is proven closed is
// recorded as abandoned and nothing is sent.
const TerminalDispositionPreserve = "preserve"

// Terminal manifest bounds, enforced at the decoder and again by the service.
const (
	TerminalManifestMaxRoots     = 64
	TerminalManifestMaxAbandoned = 256
	TerminalManifestMaxIDLen     = 128
)

// TerminalAbandonedIgnored and TerminalAbandonedColdDirty are the abandoned-extent classes.
const (
	TerminalAbandonedIgnored   = "ignored"
	TerminalAbandonedColdDirty = "cold_dirty"
)

// TerminalPreservationRequest is the strict JSON body of the route. Unknown fields are refused.
// Pack is the standard-base64 raw (non-thin) packfile; it may be empty when the tip is already on
// the forge. PriorTip is the tip the terminal ref is at (empty for the first publication).
type TerminalPreservationRequest struct {
	Generation  int64            `json:"generation"`
	Tip         string           `json:"tip"`
	PriorTip    string           `json:"prior_tip,omitempty"`
	Manifest    TerminalManifest `json:"manifest"`
	Disposition string           `json:"disposition"`
	Pack        string           `json:"pack,omitempty"`
}

// TerminalManifest is the worker's declared coverage: the roots its work descends from and the
// extents it abandoned. RunID, Generation and WorkerID must equal the authenticated values. The
// server canonicalises it (sorted, no credentials) and computes the digest it stores.
type TerminalManifest struct {
	RunID      string                      `json:"run_id"`
	Generation int64                       `json:"generation"`
	WorkerID   string                      `json:"worker_id"`
	Roots      []TerminalManifestRoot      `json:"roots"`
	Abandoned  []TerminalManifestAbandoned `json:"abandoned"`
}

// TerminalManifestRoot is one source's bound root: a commit and the tree it must have.
type TerminalManifestRoot struct {
	SourceID  string `json:"source_id"`
	AttemptID string `json:"attempt_id"`
	Commit    string `json:"commit"`
	Tree      string `json:"tree"`
}

// TerminalManifestAbandoned is one class of work the worker did NOT publish, with its size.
type TerminalManifestAbandoned struct {
	Class    string                  `json:"class"`
	SourceID string                  `json:"source_id"`
	Extents  TerminalManifestExtents `json:"extents"`
}

// TerminalManifestExtents counts what was abandoned.
type TerminalManifestExtents struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// TerminalPreservationResponse is the answer to the route. State is the contribution's state after
// the request (published, invoked, publish_unknown, refused, recorded, prepared). Only published
// means the work is on the forge at Tip. Reason qualifies a non-published state.
type TerminalPreservationResponse struct {
	PreservationID    string `json:"preservation_id"`
	ContributionID    string `json:"contribution_id"`
	Ref               string `json:"ref"`
	Tip               string `json:"tip"`
	State             string `json:"state"`
	PreservationState string `json:"preservation_state"`
	Disposition       string `json:"disposition"`
	Reason            string `json:"reason,omitempty"`
}

// The terminal-preservation refusal vocabulary. The first group is stored: the contribution's
// refusal_reason column (its CHECK). The second group is wire-only: a refusal made before any row
// is written, never stored.
const (
	TerminalReasonCreateRefused = "create_refused"
	TerminalReasonUpdateRefused = "update_refused"

	TerminalReasonUnsupported         = "unsupported"
	TerminalReasonHoldMissing         = "hold_missing"
	TerminalReasonIdentityChanged     = "identity_changed"
	TerminalReasonExcludedOrigin      = "excluded_origin"
	TerminalReasonManifestInvalid     = "manifest_invalid"
	TerminalReasonManifestMismatch    = "manifest_mismatch"
	TerminalReasonGenerationBound     = "generation_bound"
	TerminalReasonPriorTipMismatch    = "prior_tip_mismatch"
	TerminalReasonPreservationRetired = "preservation_retired"
	TerminalReasonPublicationInFlight = "publication_in_flight"
	TerminalReasonForgeUnavailable    = "forge_unavailable"
	TerminalReasonPackInvalid         = "pack_invalid"
	TerminalReasonPackTooLarge        = "pack_too_large"
	TerminalReasonTipMissing          = "tip_missing"
	TerminalReasonCIConfigGuard       = "ci_config_guard"
	TerminalReasonNotDescendant       = "not_descendant"
	TerminalReasonRootUnreachable     = "root_unreachable"
	TerminalReasonTreeMismatch        = "tree_mismatch"
	TerminalReasonWalkTooLarge        = "walk_too_large"
	TerminalReasonForeignTip          = "foreign_tip"
)
