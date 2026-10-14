package apitypes

import "time"

// Wire types of the owner-confirmed discard of a worker-held `run_not_owned` terminal outcome
// (issue #2984, capability `outcome_discard_v1`). Three audiences share them: the holding worker
// (PUT /api/worker/blocked-outcomes and the /api/worker/outcome-discards/* routes), the owner CLI
// (GET/POST under /api/workers/{id}/...), and the shared fixture fixtures/outcome-discard/.
//
// Every string field of OutcomeLossManifest is ESCAPED ASCII: bytes 0x20-0x7E pass through
// except the backslash, which is written `\\`; every other byte (non-ASCII, control, DEL) is
// written as a lowercase `\xNN`. Consumers print the stored escaped text and never decode it.
// The canonical byte encoding and digest of the manifest are defined by package
// internal/outcomeloss; field order below IS the canonical key order.

// OutcomeLossManifest is the scope-bound loss manifest of one blocked outcome. JSON names:
//
//	version          int, always 1
//	worker_id        string, the reporting worker's uuid
//	run_id           string, the run's uuid
//	run_dir          string (escaped), the physical run directory name on the worker
//	claim_generation int, the journal's claim generation
//	journal_sha256   string, 64 lowercase hex, sha256 of the journal file bytes
//	blocked_reason   string, closed vocabulary (see outcomeloss.BlockedReasons)
//	outcome          object, the journal body's outcome (OutcomeLossOutcome)
//	siblings         array, every other journal and finalize generation of the run, all KEPT
//	clones           array, every clone of the run's branch with its unpublished loss
type OutcomeLossManifest struct {
	Version         int                  `json:"version"`
	WorkerID        string               `json:"worker_id"`
	RunID           string               `json:"run_id"`
	RunDir          string               `json:"run_dir"`
	ClaimGeneration int64                `json:"claim_generation"`
	JournalSHA256   string               `json:"journal_sha256"`
	BlockedReason   string               `json:"blocked_reason"`
	Outcome         OutcomeLossOutcome   `json:"outcome"`
	Siblings        []OutcomeLossSibling `json:"siblings"`
	Clones          []OutcomeLossClone   `json:"clones"`
}

// OutcomeLossOutcome is the outcome recorded in the journal body. JSON names: status, phase
// (phase at journal time), branch, head (40 or 64 lowercase hex, or empty), mr_iid (int or null),
// mr_web_url, fail_origin. Strings are escaped ASCII and may be empty.
type OutcomeLossOutcome struct {
	Status     string `json:"status"`
	Phase      string `json:"phase"`
	Branch     string `json:"branch"`
	Head       string `json:"head"`
	MRIID      *int64 `json:"mr_iid"`
	MRWebURL   string `json:"mr_web_url"`
	FailOrigin string `json:"fail_origin"`
}

// OutcomeLossSibling is a journal or finalize file of the same run that the discard leaves in
// place. JSON names: kind ("journal" or "finalize"), generation, name (escaped file name),
// disposition (always "kept"). The array is sorted by name.
type OutcomeLossSibling struct {
	Kind        string `json:"kind"`
	Generation  int64  `json:"generation"`
	Name        string `json:"name"`
	Disposition string `json:"disposition"`
}

// OutcomeLossClone is one clone of the run's branch on the worker. JSON names: path (escaped),
// branch, head (hex or empty), commit_basis ("worker_bare_tracking_ref" or
// "clone_remote_tracking"), unpublished_commits (sorted hex ids reachable from the clone's local
// branches or HEAD but not from the basis ref), paths (every non-clean path, sorted by path).
type OutcomeLossClone struct {
	Path               string                `json:"path"`
	Branch             string                `json:"branch"`
	Head               string                `json:"head"`
	CommitBasis        string                `json:"commit_basis"`
	UnpublishedCommits []string              `json:"unpublished_commits"`
	Paths              []OutcomeLossPathItem `json:"paths"`
}

// OutcomeLossPathItem is one non-clean path bound by content. JSON names: path (escaped),
// status (two porcelain XY characters; "!!" is an ignored path), orig_path (rename source or
// empty), kind ("file", "symlink" or "deleted"), size (content bytes; 0 for deleted), sha256
// (64 lowercase hex of the file bytes or the symlink target bytes; empty for deleted),
// link_target (escaped symlink target, empty otherwise).
type OutcomeLossPathItem struct {
	Path       string `json:"path"`
	Status     string `json:"status"`
	OrigPath   string `json:"orig_path"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	LinkTarget string `json:"link_target"`
}

// BlockedOutcomeReport is one blocked journal a worker reports. JSON names: run_id,
// claim_generation, journal_sha256, blocked_reason, loss_complete, loss_sha256 (the worker's
// digest of the canonical manifest; empty when incomplete), incomplete_reason (escaped, empty
// when complete), loss (the manifest; null when incomplete). The api recomputes the digest and
// rejects a mismatch.
type BlockedOutcomeReport struct {
	RunID            string               `json:"run_id"`
	ClaimGeneration  int64                `json:"claim_generation"`
	JournalSHA256    string               `json:"journal_sha256"`
	BlockedReason    string               `json:"blocked_reason"`
	LossComplete     bool                 `json:"loss_complete"`
	LossSHA256       string               `json:"loss_sha256"`
	IncompleteReason string               `json:"incomplete_reason"`
	Loss             *OutcomeLossManifest `json:"loss"`
}

// BlockedOutcomesPutRequest is the body of PUT /api/worker/blocked-outcomes: a wholesale
// replacement of the worker's reported set. JSON name: outcomes (at most 8; an empty array clears).
type BlockedOutcomesPutRequest struct {
	Outcomes []BlockedOutcomeReport `json:"outcomes"`
}

// BlockedOutcomesPutResponse is the body of a successful PUT /api/worker/blocked-outcomes.
// JSON name: stored (the number of outcomes now held for the worker).
type BlockedOutcomesPutResponse struct {
	Stored int `json:"stored"`
}

// OwnerBlockedOutcome is one reported outcome as its owner sees it. JSON names: run_id,
// claim_generation, journal_sha256, blocked_reason, loss_complete, loss_sha256 (recomputed by
// the api from the stored manifest), incomplete_reason, loss (manifest or null), reported_at.
type OwnerBlockedOutcome struct {
	RunID            string               `json:"run_id"`
	ClaimGeneration  int64                `json:"claim_generation"`
	JournalSHA256    string               `json:"journal_sha256"`
	BlockedReason    string               `json:"blocked_reason"`
	LossComplete     bool                 `json:"loss_complete"`
	LossSHA256       string               `json:"loss_sha256"`
	IncompleteReason string               `json:"incomplete_reason"`
	Loss             *OutcomeLossManifest `json:"loss"`
	ReportedAt       time.Time            `json:"reported_at"`
}

// OwnerBlockedOutcomesResponse is the body of GET /api/workers/{id}/blocked-outcomes.
// JSON name: outcomes.
type OwnerBlockedOutcomesResponse struct {
	Outcomes []OwnerBlockedOutcome `json:"outcomes"`
}

// OutcomeDiscardEvidenceInformed is the only accepted evidence value: the owner reviewed the
// displayed loss manifest and decided. Server-held journal bytes are NOT available as evidence.
const OutcomeDiscardEvidenceInformed = "informed_decision"

// OutcomeDiscardRequest is the body of POST /api/workers/{id}/blocked-outcomes/discard
// (with ?confirm=discard). JSON names: run_id, claim_generation, journal_sha256, loss_sha256
// (the digest the owner was shown), evidence (must be "informed_decision").
type OutcomeDiscardRequest struct {
	RunID           string `json:"run_id"`
	ClaimGeneration int64  `json:"claim_generation"`
	JournalSHA256   string `json:"journal_sha256"`
	LossSHA256      string `json:"loss_sha256"`
	Evidence        string `json:"evidence"`
}

// OutcomeDiscard states.
const (
	OutcomeDiscardAuthorized       = "authorized"
	OutcomeDiscardExecuting        = "executing"
	OutcomeDiscardCompleted        = "completed"
	OutcomeDiscardCompletedUnknown = "completed_unknown"
	OutcomeDiscardRefused          = "refused"
	OutcomeDiscardExpired          = "expired"
	OutcomeDiscardCancelled        = "cancelled"
)

// OutcomeDiscard is a discard directive, returned by the owner routes, the worker's directive
// list, /begin, /complete and /reconcile. JSON names: id, worker_id, run_id, claim_generation,
// journal_sha256, loss_sha256, state (see the OutcomeDiscard* constants), reason (empty while
// authorized or executing, a closed machine reason in every final state), expires_at (the
// authorization deadline), lease_until (the executing lease end; null unless executing),
// remaining_ms (milliseconds of lease left at the server's clock when the response was built;
// 0 unless executing; never extended by a repeated /begin), created_at, updated_at.
// A state of "completed" or "completed_unknown" is never proof that the journal is gone.
type OutcomeDiscard struct {
	ID              string     `json:"id"`
	WorkerID        string     `json:"worker_id"`
	RunID           string     `json:"run_id"`
	ClaimGeneration int64      `json:"claim_generation"`
	JournalSHA256   string     `json:"journal_sha256"`
	LossSHA256      string     `json:"loss_sha256"`
	State           string     `json:"state"`
	Reason          string     `json:"reason"`
	ExpiresAt       time.Time  `json:"expires_at"`
	LeaseUntil      *time.Time `json:"lease_until"`
	RemainingMS     int64      `json:"remaining_ms"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// WorkerOutcomeDiscardsResponse is the body of GET /api/worker/outcome-discards: this worker's
// authorized and executing directives. JSON name: discards.
type WorkerOutcomeDiscardsResponse struct {
	Discards []OutcomeDiscard `json:"discards"`
}

// OutcomeDiscard /complete results and /reconcile observations.
const (
	OutcomeDiscardResultDeleted  = "deleted"
	OutcomeDiscardResultRefused  = "refused"
	OutcomeDiscardResultUnsynced = "unsynced"

	OutcomeDiscardObservationAbsent            = "absent"
	OutcomeDiscardObservationPresentUnverified = "present_unverifiable"
)

// OutcomeDiscardCompleteRequest is the body of POST /api/worker/outcome-discards/{id}/complete.
// JSON names: result ("deleted", "refused" or "unsynced"), reason (closed machine reason of at
// most 64 bytes of [a-z0-9_]; required for refused and unsynced, empty for deleted).
type OutcomeDiscardCompleteRequest struct {
	Result string `json:"result"`
	Reason string `json:"reason"`
}

// OutcomeDiscardReconcileRequest is the body of POST /api/worker/outcome-discards/{id}/reconcile.
// JSON name: observation ("absent" or "present_unverifiable"). It never carries deletion authority.
type OutcomeDiscardReconcileRequest struct {
	Observation string `json:"observation"`
}

// OutcomeDiscardRefusal is the 409 body of the owner discard route. JSON names: error (text),
// code (closed machine code: not_blocked, reason_not_discardable, attributed, journal_changed,
// loss_changed, manifest_incomplete, custody_hold_open, execution_unresolved).
type OutcomeDiscardRefusal struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
