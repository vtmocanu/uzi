package apitypes

import "time"

// The uzi-fetcher wire shapes (PRD #1906 M2, Decisions 3, 7 and 8). Two hops use them:
//
//   - worker -> fetcher, POST /v1/fetch on the fetcher: FetchRequest in; on success the
//     body is the fetched bytes (not JSON), on refusal FetchErrorDTO.
//   - fetcher -> api, POST /api/fetcher/v1/begin and POST /api/fetcher/v1/complete on the
//     api: FetcherBeginRequest / FetcherBeginResponse and FetcherCompleteRequest, with
//     FetcherControlErrorDTO for a refusal. The full contract (statuses, which side
//     authenticates what) is the package doc of api/internal/fetcher.
//
// Every request body here is decoded strictly (unknown fields refused), so one
// definition shared by both ends is what keeps a key mismatch from being writable.

// FetchRequest is the whole body a worker may send the fetcher: a URL and nothing else.
// There is deliberately no method, header or body field: the fetcher performs a GET that
// it builds itself (Decision 3).
type FetchRequest struct {
	URL string `json:"url"`
}

// FetchErrorDTO is the fetcher's refusal body. Reason is a stable code a client branches
// on; Error is for humans. UpstreamStatus is set only for reason "upstream_status" (the
// site answered with a non-2xx, non-redirect status) and AdmissionReason only for reason
// "admission_refused" (the api's own code, e.g. a run total is used up).
type FetchErrorDTO struct {
	Error           string `json:"error"`
	Reason          string `json:"reason"`
	UpstreamStatus  int    `json:"upstream_status,omitempty"`
	AdmissionReason string `json:"admission_reason,omitempty"`
}

// FetcherBeginRequest asks the api to admit one fetch. Credential is the per-run fetch
// credential the worker presented, forwarded verbatim: the api derives the run from it
// and from nothing else. URL is the URL the worker asked for, as received.
type FetcherBeginRequest struct {
	Credential string `json:"credential"`
	URL        string `json:"url"`
}

// FetcherBeginResponse admits one fetch. ReservationID names the api-side reservation
// (the per-file maximum bytes, one file slot and one concurrency slot) that the matching
// Complete settles. Entries is the run's EFFECTIVE site list snapshot (overrides already
// applied: a multi-publisher entry without an override is not in it). MaxBytes is the
// per-file cap in bytes.
type FetcherBeginResponse struct {
	ReservationID string   `json:"reservation_id"`
	Entries       []string `json:"entries"`
	MaxBytes      int64    `json:"max_bytes"`
}

// FetcherCompleteRequest reports one attempt that Begin admitted, allowed or refused.
// Credential is forwarded again so the api keys the row from it, never from a run id the
// fetcher could choose. URL, FinalURL, ContentType and Reason are site- or
// agent-controlled text: the api must check them for control and bidi characters at
// write time. SHA256 is lowercase hex and empty unless Verdict is "allowed".
type FetcherCompleteRequest struct {
	Credential    string    `json:"credential"`
	ReservationID string    `json:"reservation_id"`
	URL           string    `json:"url"`
	FinalURL      string    `json:"final_url"`
	Verdict       string    `json:"verdict"`
	Reason        string    `json:"reason"`
	HTTPStatus    int       `json:"http_status"`
	ContentType   string    `json:"content_type"`
	Bytes         int64     `json:"bytes"`
	SHA256        string    `json:"sha256"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
}

// FetcherControlErrorDTO is the api's refusal body on the fetcher control routes.
// Reason is "credential_invalid" (the run credential is unknown, revoked or its run is
// terminal) or an admission code on a 429.
type FetcherControlErrorDTO struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
}

// RunFetchDTO is one row of a run's source log (PRD #1906 M3, Decision 8), as the run's
// owner reads it from GET /api/runs/{id}/fetches and `uzi run fetches`. URL, FinalURL,
// ContentType and Reason are site- or agent-controlled: the api stored them escaped
// (control and format runes as \u{XXXX}, invalid bytes as \xNN, a backslash as \\) and
// length-capped, and a renderer must still treat them as untrusted text.
type RunFetchDTO struct {
	ID          string    `json:"id"`
	URL         string    `json:"url"`
	FinalURL    string    `json:"final_url"`
	Verdict     string    `json:"verdict"`
	Reason      string    `json:"reason"`
	HTTPStatus  int       `json:"http_status"`
	ContentType string    `json:"content_type"`
	Bytes       int64     `json:"bytes"`
	SHA256      string    `json:"sha256"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	CreatedAt   time.Time `json:"created_at"`
}

// RunFetchesDTO is one page of the owner read, oldest first. NextCursor is set when more
// rows follow: pass it back as ?after= to read the next page. It is the id of this page's
// last row, and omitted on the last page.
type RunFetchesDTO struct {
	Fetches    []RunFetchDTO `json:"fetches"`
	NextCursor string        `json:"next_cursor,omitempty"`
}
