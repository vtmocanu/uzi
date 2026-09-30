package apitypes

import "time"

// The /api/v1/files wire DTO (PRD #1909 D5). It is part of the stable /api/v1 contract (PRD
// #1907 D12): changes are additive only, and api/openapi/v1.yaml describes it
// (TestV1OpenAPISchemasMatchDTOs binds the two field by field). DisplayName is untrusted display
// text taken from the uploader's file name: it is reduced to a basename and stripped of control and
// invisible characters on write, and a client must still render it inert.

// V1FileDTO is one stored file. StorageName is `<sha256>.<ext>`, derived from the file's content
// and its DETECTED type, never from the uploader's name. State is unattached (uploaded, not yet
// used by a job), attached, available or expired. ExpiresAt is null while the file is attached to
// a running job.
type V1FileDTO struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	StorageName string     `json:"storage_name"`
	ContentType string     `json:"content_type"`
	ByteSize    int64      `json:"byte_size"`
	Sha256      string     `json:"sha256"`
	State       string     `json:"state"`
	ExpiresAt   *time.Time `json:"expires_at"`
}
