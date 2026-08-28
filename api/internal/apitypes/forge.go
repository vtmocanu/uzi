package apitypes

// Forge read DTOs (PRD #158). These are the WORKER-facing projections of the forge
// driver structs in internal/forge, exposed by the /api/worker/runs/{id}/forge/*
// read routes.
//
// The Go type names carry a Forge* prefix (the package already has a WebURL-bearing
// PipelineDTO for the CI badge — a DIFFERENT, coordinate-carrying shape that must not
// be confused with these). The JSON field names are the wire contract and are exact.
//
// CRITICAL (Success Criterion 3): every one of these DROPS the fields that would
// leak forge coordinates to the agent — no WebURL, no numeric forge project id, no
// forge base URL, no token. The handler mappers pre-format timestamps as RFC3339
// strings (not time.Time) so the wire contract is exact for the TS client: no
// nanoseconds, no zero-value marshaling surprises, and this stays a stdlib-only leaf
// with no time dependency.

// ForgeIssueDTO is a single issue with its (possibly truncated) description and its
// bounded, bot/system-filtered human comments (PRD #381 M4). Returned by
// GET /worker/runs/{id}/forge/issues/{iid}.
type ForgeIssueDTO struct {
	IID                  int64                  `json:"iid"`
	Title                string                 `json:"title"`
	State                string                 `json:"state"`
	Labels               []string               `json:"labels"`
	Author               string                 `json:"author"`
	UpdatedAt            string                 `json:"updated_at"`
	Description          string                 `json:"description"`
	DescriptionTruncated bool                   `json:"description_truncated"`
	Comments             []ForgeIssueCommentDTO `json:"comments"`
	CommentsTruncated    bool                   `json:"comments_truncated"`
}

// ForgeIssueCommentDTO is one HUMAN issue comment (PRD #381). Bot-authored and forge
// system notes are excluded server-side; no forge coordinates are exposed (the author
// forge user id used for the bot filter is deliberately NOT on the wire).
type ForgeIssueCommentDTO struct {
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"` // RFC3339
	Body      string `json:"body"`
}

// ForgeIssueSummaryDTO is a list-row issue: no description (the list never carries
// issue bodies — only the single-issue GET does).
type ForgeIssueSummaryDTO struct {
	IID       int64    `json:"iid"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	Labels    []string `json:"labels"`
	Author    string   `json:"author"`
	UpdatedAt string   `json:"updated_at"`
}

// ForgeIssueListDTO is the capped issue list. Truncated is true when the forge held
// more than the server cap; Returned is the item count after the cap was applied.
type ForgeIssueListDTO struct {
	Items     []ForgeIssueSummaryDTO `json:"items"`
	Truncated bool                   `json:"truncated"`
	Returned  int                    `json:"returned"`
}

// ForgeLabelEventDTO is one resource label event on an issue.
type ForgeLabelEventDTO struct {
	ID        int64  `json:"id"`
	Action    string `json:"action"`
	LabelName string `json:"label_name"`
	Username  string `json:"username"`
	CreatedAt string `json:"created_at"`
}

// ForgeLabelEventListDTO is the capped label-event list.
type ForgeLabelEventListDTO struct {
	Items     []ForgeLabelEventDTO `json:"items"`
	Truncated bool                 `json:"truncated"`
	Returned  int                  `json:"returned"`
}

// ForgeMergeRequestDTO is a single merge request. Only iid + state cross the wire.
type ForgeMergeRequestDTO struct {
	IID   int64  `json:"iid"`
	State string `json:"state"`
}

// Forge WRITE DTOs (PRD #700 M4). The mr_rework run's write-back: reply in and resolve
// the MR review threads it addressed. The request carries ONLY the thread anchor (and a
// body for a reply) — never the mr_iid or any forge coordinate; the endpoint derives the
// mr_iid from the OWNED run and validates the anchor against THIS run's review snapshot
// server-side (Decision 11), so an out-of-snapshot id is rejected.

// ForgeMRThreadReplyRequest is the body of POST /worker/runs/{id}/forge/mr-threads/reply.
// ReplyID is an MRComment.ReplyID from this run's review snapshot.
type ForgeMRThreadReplyRequest struct {
	ReplyID string `json:"reply_id"`
	Body    string `json:"body"`
}

// ForgeMRThreadReplyDTO is the reply write's result. Replied is true when the driver
// posted the reply.
type ForgeMRThreadReplyDTO struct {
	Replied bool `json:"replied"`
}

// ForgeMRThreadResolveRequest is the body of POST /worker/runs/{id}/forge/mr-threads/
// resolve. ResolveID is an MRComment.ResolveID from this run's review snapshot.
type ForgeMRThreadResolveRequest struct {
	ResolveID string `json:"resolve_id"`
}

// ForgeMRThreadResolveDTO is the resolve write's result. Resolved is true when the driver
// resolved the thread; false is the tolerated Forgejo no-op (the forge has no resolvable-
// thread concept, so reply-only is the documented contract and the run does not fail).
type ForgeMRThreadResolveDTO struct {
	Resolved bool `json:"resolved"`
}

// ForgeJobDTO is one pipeline job.
type ForgeJobDTO struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Stage  string `json:"stage"`
	Status string `json:"status"`
}

// ForgeJobListDTO is the capped job list.
type ForgeJobListDTO struct {
	Items     []ForgeJobDTO `json:"items"`
	Truncated bool          `json:"truncated"`
	Returned  int           `json:"returned"`
}

// ForgePipelineDTO is a single pipeline. Distinct from the repo-badge PipelineDTO in
// this package: this one carries NO web_url and no forge coordinates.
type ForgePipelineDTO struct {
	ID        int64  `json:"id"`
	Ref       string `json:"ref"`
	SHA       string `json:"sha"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ForgeLatestPipelineDTO wraps the latest pipeline. Pipeline is null (not an error)
// when the ref/MR has never run CI — the CI-never-ran case must be distinguishable
// from a forge-down error.
type ForgeLatestPipelineDTO struct {
	Pipeline *ForgePipelineDTO `json:"pipeline"`
}
