package apitypes

import "time"

// Drift outcomes and reasons (PRD #1230 D5). Drift is a detector: nothing here
// holds, refuses, revises a contract or satisfies a criterion.
const (
	SemanticDriftUnchanged = "unchanged"
	SemanticDriftChanged   = "changed"
	SemanticDriftUnknown   = "unknown"

	SemanticDriftPhasePresentation = "presentation"
	SemanticDriftPhasePublication  = "publication"
)

// SemanticObservationRequest is the worker's POST /worker/runs/{id}/semantic/observation
// body. The worker reads each PRD source at the exact commit through trusted bare git;
// the server cannot. Presentation requires candidate_digest and gate_revision;
// publication requires pushed_sha (40 lowercase hex). Every frozen PRD source of the
// current candidate must appear exactly once (at most 8).
type SemanticObservationRequest struct {
	ClaimGeneration *int64                   `json:"claim_generation"`
	Phase           string                   `json:"phase"`
	CandidateDigest string                   `json:"candidate_digest,omitempty"`
	GateRevision    *int64                   `json:"gate_revision,omitempty"`
	PushedSHA       string                   `json:"pushed_sha,omitempty"`
	PRDSources      []SemanticObservedSource `json:"prd_sources"`
}

// SemanticObservedSource is one worker-reported PRD read. The server recomputes
// changed/unchanged from observed_hash against its frozen hash and refuses a
// contradicting outcome. unknown carries a reason and no hash.
type SemanticObservedSource struct {
	SourceID     string `json:"source_id"`
	ObservedHash string `json:"observed_hash,omitempty"`
	Ref          string `json:"ref"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
}

// SemanticDriftSource records one source's comparison. Hashes, refs and enum
// values only; never source text.
type SemanticDriftSource struct {
	SourceID     string `json:"source_id"`
	FrozenHash   string `json:"frozen_hash"`
	ObservedHash string `json:"observed_hash,omitempty"`
	Ref          string `json:"ref,omitempty"`
	Outcome      string `json:"outcome"`
	Reason       string `json:"reason,omitempty"`
	// CheckboxOnly marks an issue source whose text differs from the frozen text only
	// in markdown task-list checkbox state. It is still a contradiction.
	CheckboxOnly bool `json:"checkbox_only,omitempty"`
}

// SemanticDriftObservation is the stored and returned record. Link fields carry
// clone-relative PRD paths only. LinksTruncated is set when paths were omitted to
// stay inside the fixed bounds (count or unsafe/oversize path).
type SemanticDriftObservation struct {
	Phase           string                `json:"phase"`
	CandidateDigest string                `json:"candidate_digest,omitempty"`
	GateRevision    int64                 `json:"gate_revision,omitempty"`
	PushedSHA       string                `json:"pushed_sha,omitempty"`
	ObservedAt      time.Time             `json:"observed_at"`
	Issue           SemanticDriftSource   `json:"issue"`
	PRDs            []SemanticDriftSource `json:"prds"`
	LinksAdded      []string              `json:"links_added"`
	LinksRemoved    []string              `json:"links_removed"`
	LinksTruncated  bool                  `json:"links_truncated,omitempty"`
}

// SemanticDriftDTO is the owner-only run projection. Contradiction is true when any
// stored observation has a changed or unknown source; it never marks anything satisfied.
type SemanticDriftDTO struct {
	Observations  []SemanticDriftObservation `json:"observations"`
	Dropped       int64                      `json:"dropped"`
	Contradiction bool                       `json:"contradiction"`
}
