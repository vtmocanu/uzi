package apitypes

// Fresh resume (issue #4628): an owner starts a run held at a worker-exhaustion recovery_wait on a
// separate persistent hosted worker from durable evidence. These are the owner-facing wire types of
// GET and POST /api/runs/{id}/fresh-resume.

// The preview statuses. Only FreshResumeStatusAvailable carries a selection token; every other status
// names why starting is not possible right now, and the run stays held.
const (
	FreshResumeStatusAvailable      = "available"
	FreshResumeStatusDisabled       = "disabled"
	FreshResumeStatusNoDestination  = "no_destination"
	FreshResumeStatusCustodyLimit   = "custody_limit"
	FreshResumeStatusNoEvidence     = "no_evidence"
	FreshResumeStatusExpired        = "expired"
	FreshResumeStatusUnbound        = "unbound"
	FreshResumeStatusPrereqUnproven = "prereq_unproven"
	FreshResumeStatusAmbiguous      = "ambiguous"
	FreshResumeStatusInProgress     = "in_progress"
	FreshResumeStatusIneligible     = "ineligible"
)

// The 409 reasons a start adds to the preview statuses it can also answer with.
const (
	FreshResumeReasonStaleSelection   = "stale_selection"
	FreshResumeReasonRequestCompleted = "request_completed"
)

// The evidence kinds of a fresh resume.
const (
	FreshResumeKindCapture       = "capture"
	FreshResumeKindCheckpointRef = "checkpoint_ref"
)

// FreshResumePreviewDTO is GET /api/runs/{id}/fresh-resume: what a start would seed from, or why it
// cannot start. The selection token binds the start to exactly this selection: it changes with the run's
// claim generation, the hold version and the chosen evidence, so a confirm made on an old preview is
// refused (409 stale_selection).
type FreshResumePreviewDTO struct {
	Status         string `json:"status"`
	SelectionToken string `json:"selection_token,omitempty"`
	// EvidenceGeneration is the claim generation the evidence was saved under; ClaimGeneration is the
	// run's latest. UncoveredGenerations lists the generations after the evidence's that saved nothing,
	// so any edits made in them are not included.
	EvidenceGeneration   int64   `json:"evidence_generation,omitempty"`
	ClaimGeneration      int64   `json:"claim_generation,omitempty"`
	Kind                 string  `json:"kind,omitempty"`
	CaptureID            string  `json:"capture_id,omitempty"`
	CheckpointRef        string  `json:"checkpoint_ref,omitempty"`
	Sha                  string  `json:"sha,omitempty"`
	UncoveredGenerations []int64 `json:"uncovered_generations"`
	// Notes is the cutoff text the owner confirms against: what a fresh resume may lose and what it
	// keeps and costs.
	Notes []string `json:"notes"`
}

// FreshResumeStartRequest is the body of POST /api/runs/{id}/fresh-resume. RequestID is a client
// generated UUID, one per confirm action: replaying a request that already completed is refused, never
// queued again.
type FreshResumeStartRequest struct {
	SelectionToken string `json:"selection_token"`
	RequestID      string `json:"request_id"`
}

// The 409 reasons of the destination worker's fresh resume routes (/api/worker/runs/{id}/fresh-resume/...).
const (
	// FreshResumeReasonNotClaimed: the transfer is not claimed by this worker at the claim generation it
	// sent, or it was closed (refused, cancelled), so the call is fenced out.
	FreshResumeReasonNotClaimed = "fresh_resume_not_claimed"
	// FreshResumeReasonAttemptMismatch: the receipt names an attempt or generation other than the one the
	// transfer was seeded under (or an attempt id that does not carry the claim generation).
	FreshResumeReasonAttemptMismatch = "fresh_resume_attempt_mismatch"
	// FreshResumeReasonNoBundle: the transfer's evidence is a checkpoint ref, which has no bundle to stream.
	FreshResumeReasonNoBundle = "fresh_resume_no_bundle"
	// FreshResumeReasonPreSeed: the worker's park or fail report was refused because its transfer is
	// claimed and not yet seeded; the worker abandons the transfer instead of parking the run.
	FreshResumeReasonPreSeed = "fresh_resume_pre_seed"
)

// FreshResumeSeededRequest is the body of POST /api/worker/runs/{id}/fresh-resume/{frid}/seeded: the
// destination's receipt that its first fenced commit landed. ClaimGeneration is G' (the transfer's claim
// generation), AttemptID the destination attempt, LiveGeneration the generation the worker holds now
// (the one CurrentAuthority is judged against).
type FreshResumeSeededRequest struct {
	ClaimGeneration int64  `json:"claim_generation"`
	AttemptID       string `json:"attempt_id"`
	LiveGeneration  int64  `json:"live_generation"`
}

// FreshResumeAbandonRequest is the body of POST /api/worker/runs/{id}/fresh-resume/{frid}/abandon.
type FreshResumeAbandonRequest struct {
	ClaimGeneration int64 `json:"claim_generation"`
}

// FreshResumeStateDTO is the reply of the seeded receipt and of GET .../fresh-resume/{frid}/state: the
// stored transfer state and whether the calling worker still holds the run's live claim. SeededAt is
// null (omitted from the seeded fields) until the receipt landed; a transfer refused before seeding
// answers state "refused" with SeededAt null. CurrentAuthority is judged under the run lock against the
// live generation the caller sent.
type FreshResumeStateDTO struct {
	State                string  `json:"state"`
	SeededAt             *string `json:"seeded_at"`
	DestinationAttemptID *string `json:"destination_attempt_id"`
	SeededGeneration     *int64  `json:"seeded_generation"`
	CurrentAuthority     bool    `json:"current_authority"`
}

// FreshResumeSeededDTO is the reply of the seeded receipt: the same fields as the state route.
type FreshResumeSeededDTO = FreshResumeStateDTO

// FreshResumeAbandonDTO is the reply of the abandon route: the transfer closed refused (the run is back
// at its held recovery_wait).
type FreshResumeAbandonDTO struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// FreshResumeStartDTO is the disposition of a start: the transfer row it created, or, for a replay of a
// request that is still active, the row that request already created.
type FreshResumeStartDTO struct {
	ID                 string `json:"id"`
	RunID              string `json:"run_id"`
	RequestID          string `json:"request_id"`
	State              string `json:"state"`
	Replay             bool   `json:"replay"`
	EvidenceGeneration int64  `json:"evidence_generation"`
	Kind               string `json:"kind"`
	CaptureID          string `json:"capture_id,omitempty"`
	CheckpointRef      string `json:"checkpoint_ref,omitempty"`
	Sha                string `json:"sha"`
	AdmissionDeadline  string `json:"admission_deadline"`
}
