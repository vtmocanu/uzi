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

// V1JobFileDTO is one file of a job, an input attached to it or an output it produced
// (GET /api/v1/jobs/{id}/files and the result's files). Direction is input or output; download the
// bytes with GET /api/v1/files/{id}. DisplayName is untrusted display text (see V1FileDTO). SourceURL
// is non-null only for an output whose SHA-256 equals the SHA-256 of a fetch the fetch service
// recorded as allowed in this same job: the URL it was fetched from. It is never taken from
// anything the agent claimed. ExpiresAt is null while the file is attached to a job that has not
// finished.
type V1JobFileDTO struct {
	ID          string     `json:"id"`
	DisplayName string     `json:"display_name"`
	ContentType string     `json:"content_type"`
	ByteSize    int64      `json:"byte_size"`
	Sha256      string     `json:"sha256"`
	Direction   string     `json:"direction"`
	State       string     `json:"state"`
	ExpiresAt   *time.Time `json:"expires_at"`
	SourceURL   *string    `json:"source_url"`
}

// V1JobRefusedFileDTO is an output that was not stored: the job still completed, and this says
// which file was left out and why. Reason is a short machine-readable token or sentence; treat it
// and DisplayName as untrusted text. A reason of generation_pending means a generated file
// (report.md or findings.json) is still being stored and is not a refusal.
type V1JobRefusedFileDTO struct {
	DisplayName string `json:"display_name"`
	ByteSize    int64  `json:"byte_size"`
	Reason      string `json:"reason"`
}

// V1JobFilesDTO is the GET /api/v1/jobs/{id}/files response.
type V1JobFilesDTO struct {
	Files        []V1JobFileDTO        `json:"files"`
	RefusedFiles []V1JobRefusedFileDTO `json:"refused_files"`
}

// V1JobSourceDTO is one fetch of the job's run, as the fetch service (not the agent) recorded it.
// Verdict is allowed or refused; a refused fetch has an empty Sha256 and zero ByteSize. URL and
// FinalURL (after redirects) are untrusted text taken from the agent's request and the site.
type V1JobSourceDTO struct {
	URL         string    `json:"url"`
	FinalURL    string    `json:"final_url"`
	Verdict     string    `json:"verdict"`
	Reason      string    `json:"reason"`
	HTTPStatus  int       `json:"http_status"`
	ContentType string    `json:"content_type"`
	ByteSize    int64     `json:"byte_size"`
	Sha256      string    `json:"sha256"`
	FetchedAt   time.Time `json:"fetched_at"`
}
