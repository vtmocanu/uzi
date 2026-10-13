package apitypes

import "time"

// HeldPublicationResponse is the answer to a worker's held-work publication upload (step A,
// POST /api/worker/runs/{id}/held-publication, issue #2545). State is the publication's state
// after the request (created, refused, create_unknown, invoked, ...). A non-empty Reason qualifies
// it: create_refused for a remote that refused the create, forge_unavailable when the ref could
// not be listed. Only created means the work is on the forge at Tip; every other state sends the
// worker to the archive path.
type HeldPublicationResponse struct {
	PublicationID string `json:"publication_id"`
	Ref           string `json:"ref"`
	Tip           string `json:"tip"`
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
}

// HeldPublicationDTO is one held-work publication of a failed run, as the owner sees it
// (GET /api/runs/{id}/held-publications, issue #2545): the forge ref the run's work was published
// to, the tip it holds, and where the row is in its lifecycle. State is one of prepared, invoked,
// created, create_unknown, refused, acknowledged, delete_unknown, deleted or abandoned.
// ExpiresAt is when the retention delete is due; OwnerExpiredAt is set when the owner asked for it
// early. LastError is bounded, sanitized text and never a credential. Every optional field is
// omitempty, so a zero value carries no null.
type HeldPublicationDTO struct {
	ID             string     `json:"id"`
	RunID          string     `json:"run_id"`
	Generation     int64      `json:"generation"`
	Ref            string     `json:"ref"`
	Tip            string     `json:"tip"`
	State          string     `json:"state"`
	CreatedAt      time.Time  `json:"created_at"`
	RefCreatedAt   *time.Time `json:"ref_created_at,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"`
	OwnerExpiredAt *time.Time `json:"owner_expired_at,omitempty"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	LastError      string     `json:"last_error,omitempty"`
	RefusalReason  string     `json:"refusal_reason,omitempty"`
}

// HeldPublicationsDTO is the answer to GET /api/runs/{id}/held-publications: the run's held
// publications, newest generation first (empty, never null, when it has none).
type HeldPublicationsDTO struct {
	Publications []HeldPublicationDTO `json:"publications"`
}

// HeldPublicationsExpireDTO is the answer to POST /api/runs/{id}/held-publications/expire: the
// acknowledged publications that became due now (the retention sweep deletes their refs on its
// next pass). Expired counts them; it is 0 when none was acknowledged.
type HeldPublicationsExpireDTO struct {
	Expired      int                  `json:"expired"`
	Publications []HeldPublicationDTO `json:"publications"`
}
