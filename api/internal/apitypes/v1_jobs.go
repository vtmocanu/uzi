package apitypes

import "time"

// The /api/v1/jobs wire DTOs (PRD #1908 D10). They are part of the stable /api/v1 contract
// (PRD #1907 D12): changes are additive only, and api/openapi/v1.yaml describes every one of
// them (TestV1OpenAPISchemasMatchDTOs binds the two field by field). Every free-text field
// (title, requested_by_label, failure_reason, report_md, finding text, message text) is
// untrusted display text: it was validated or scrubbed on write, and a client must still
// render it inert.

// V1JobInputDTO is one named text input of a job create request.
type V1JobInputDTO struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

// V1JobCreateRequest is the POST /api/v1/jobs body. Type and Prompt are required. Title is
// derived from the prompt when omitted. EgressProfile is reserved: any value is refused
// with 422 not_supported until PRD #1906 lands site-list egress. The optional fields accept
// an explicit null as "omitted".
type V1JobCreateRequest struct {
	Type   string          `json:"type"`
	Title  *string         `json:"title,omitempty"`
	Prompt string          `json:"prompt"`
	Inputs []V1JobInputDTO `json:"inputs,omitempty"`
	// InputFileIDs are ids returned by POST /api/v1/files, attached to the job as input files
	// (PRD #1909 D5). At most UZI_JOB_INPUTS_MAX_FILES; each id may be listed once.
	InputFileIDs     []string `json:"input_file_ids,omitempty"`
	RequestedByLabel *string  `json:"requested_by_label,omitempty"`
	WallSeconds      *int     `json:"wall_seconds,omitempty"`
	EgressProfile    *string  `json:"egress_profile,omitempty"`
}

// V1JobDTO is one job as the API shows it. Status is the small public vocabulary
// (queued, running, waiting, completed, failed, cancelled), never the internal run status.
// FailureReason is a bounded, secret-scrubbed human message, set only for a failed job.
// WallSeconds is the job's wall-clock limit in seconds, null while the instance default
// applies.
type V1JobDTO struct {
	ID               string     `json:"id"`
	Type             string     `json:"type"`
	Status           string     `json:"status"`
	Title            string     `json:"title"`
	RequestedByLabel *string    `json:"requested_by_label"`
	FailureReason    *string    `json:"failure_reason"`
	WallSeconds      *int       `json:"wall_seconds"`
	CreatedAt        time.Time  `json:"created_at"`
	StartedAt        *time.Time `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at"`
}

// V1JobListDTO is one page of GET /api/v1/jobs, newest first. NextCursor is null on the
// last page; otherwise pass it back as ?cursor= for the next page. The cursor is opaque.
type V1JobListDTO struct {
	Jobs       []V1JobDTO `json:"jobs"`
	NextCursor *string    `json:"next_cursor"`
}

// V1JobFindingDTO is one structured finding of a job result. The location is either a URL or
// a file with an optional line; the unused members are null.
type V1JobFindingDTO struct {
	Severity  string  `json:"severity"`
	MessageMd string  `json:"message_md"`
	URL       *string `json:"url"`
	File      *string `json:"file"`
	Line      *int    `json:"line"`
}

// V1JobResultBodyDTO is a job's stored result.
//
// Sources, Files and RefusedFiles are PRD #1909 M5 (additive): the run's source log as the fetch
// service recorded it, the files of the job (inputs and outputs) and the outputs that were not
// stored. They describe THIS job's run only. All three are present (empty, never null).
type V1JobResultBodyDTO struct {
	Status       string                `json:"status"`
	ReportMd     string                `json:"report_md"`
	Findings     []V1JobFindingDTO     `json:"findings"`
	Sources      []V1JobSourceDTO      `json:"sources"`
	Files        []V1JobFileDTO        `json:"files"`
	RefusedFiles []V1JobRefusedFileDTO `json:"refused_files"`
}

// V1JobResultDTO is the GET /api/v1/jobs/{id}/result response. Result is null until the job
// has reported one (JobStatus says whether it still can: queued, running or waiting).
type V1JobResultDTO struct {
	JobStatus string              `json:"job_status"`
	Result    *V1JobResultBodyDTO `json:"result"`
}

// V1JobMessageDTO is one entry of a job's narrowed progress feed: only human-readable
// agent text, status and error lines, never tool input or output.
type V1JobMessageDTO struct {
	Seq       int       `json:"seq"`
	CreatedAt time.Time `json:"created_at"`
	Type      string    `json:"type"`
	Text      string    `json:"text"`
}

// V1JobMessagesDTO is the GET /api/v1/jobs/{id}/messages response: the messages after the
// requested seq, oldest first, at most one bounded page. Poll again with after set to the
// last returned seq.
type V1JobMessagesDTO struct {
	Messages []V1JobMessageDTO `json:"messages"`
}
