package apitypes

import "time"

// IncidentalFindingDTO is one coordinate in the per-repo Findings backlog (PRD #333 M4,
// D7): a (repo, location) coordinate deduped across every run it recurs in, carrying its
// disposition status, the "seen in N runs" occurrence count, and the actionable evidence id
// the file/dismiss actions drive on.
//
// FindingID is the latest evidence row's id — the id POST /findings/{id}/issue|dismiss act
// on (M5). It is omitempty and nil for a filed/dismissed coordinate whose evidence rows were
// cascaded away with a deleted run (a display-only, non-actionable row, D12): the coordinate
// still appears (the read is disposition-driven), last_title keeps it legible, but there is no
// evidence row to act on. A client MUST treat a nil finding_id as "not actionable from here".
//
// LastTitle is a disposition snapshot (D12), refreshed on each report, so a coordinate stays
// legible after its evidence is gone. It is agent-authored, already-sanitised text (inert at
// rest); like the judge's rationale_preview every consumer renders it as escaped text, never
// markdown/HTML. FiledIssueIID/ResolvedAt are omitempty — present only on a filed/resolved
// coordinate.
//
// FiledIssueURL is the stored web URL of the forge issue a filed coordinate produced (D12),
// stamped at settle time alongside FiledIssueIID. It is omitempty — empty until filed — and is
// what lets the backlog render a filed coordinate as a click-through link even for a coordinate
// filed from the CLI or revisited in a later session (no session-local file result to link
// from). It is a forge-produced URL, not agent text; the web renders it only when it is https.
type IncidentalFindingDTO struct {
	FindingID     *string    `json:"finding_id,omitempty"`
	Location      string     `json:"location"`
	RepoID        string     `json:"repo_id"`
	RepoPath      string     `json:"repo_path"`
	Status        string     `json:"status"`
	LastTitle     string     `json:"last_title"`
	SeenInRuns    int        `json:"seen_in_runs"`
	FiledIssueIID *int64     `json:"filed_issue_iid,omitempty"`
	FiledIssueURL string     `json:"filed_issue_url,omitempty"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
}

// IncidentalFindingBacklogDTO is GET /api/findings (PRD #333 M4, D7/D8): the caller's
// owner-scoped Findings backlog, deduped by (repo, location).
//
// Bucket echoes the applied ?bucket= filter (default to_file); Repo echoes the ?repo= filter
// ("" when absent) and Run echoes ?run= ("" when absent), the same echo pattern as
// JudgeBacklogDTO. OpenCount is the D8 nav-badge count (CountOpenFindingsForUser) — a NEW
// count source, separate from the shared bell unread, and it RIDES ON THIS RESPONSE META
// rather than a standalone route (the judge pattern), scoped by the same ?repo= filter as the
// list. Findings is never nil on the wire (an empty backlog encodes []), so a client iterates
// it without a null guard.
type IncidentalFindingBacklogDTO struct {
	Bucket    string                 `json:"bucket"`
	Repo      string                 `json:"repo"`
	Run       string                 `json:"run"`
	OpenCount int                    `json:"open_count"`
	Findings  []IncidentalFindingDTO `json:"findings"`
}

// IncidentalFindingFiledIssueDTO is the real forge issue POST /api/findings/{id}/issue created
// (PRD #333 M5/M6): the iid and web_url `uzi findings file` prints so a user can open it. It
// carries no forge project id or other coordinates — only what the click produced.
type IncidentalFindingFiledIssueDTO struct {
	IID    int64  `json:"iid"`
	WebURL string `json:"web_url"`
	Title  string `json:"title"`
}

// IncidentalFindingFileResultDTO is the POST /api/findings/{id}/issue response (PRD #333 M5):
// the created forge issue, plus a non-empty Warning when the issue WAS created but its local
// disposition could not settle (created-with-warning — a success, never a retry signal, so a
// CLI prints it as a note and still exits 0). Warning is omitempty: absent on a clean file.
type IncidentalFindingFileResultDTO struct {
	Issue   IncidentalFindingFiledIssueDTO `json:"issue"`
	Warning string                         `json:"warning,omitempty"`
}

// IncidentalFindingIssueDraftDTO is GET /api/findings/{id}/issue-draft (PRD #333 M4, D4): the
// deterministic, human-editable draft for filing a forge issue from one finding. It is built
// by issuedraft.RenderFinding — the finding-specific template that CALLS the field-level
// sanitisers (title→SanitizeTitle; description→FenceBlock+SanitizeFiledBody;
// location→SafeInlineCode), NOT issuedraft.Render (which is judge-hardcoded). Every field is
// already inert; like IssueDraftDTO this draft is a UX convenience, and the M5 file POST
// re-applies the write-boundary controls to the (possibly edited) body. Labels seed the
// editable selection; the server-mandated marker is added at file time (D5), never here.
type IncidentalFindingIssueDraftDTO struct {
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Location    string   `json:"location"`
	Labels      []string `json:"labels"`
	Provenance  string   `json:"provenance"`
}
