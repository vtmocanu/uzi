package apitypes

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
