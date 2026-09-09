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
//
// DispositionID is the coordinate's finding_dispositions.id — the ALWAYS-PRESENT primary id the
// bulk-dismiss (POST /findings/dismiss {ids}) and undo (DELETE /findings/{id}/dismiss) endpoints
// key on (PRD #1183 M3). It is distinct from FindingID: FindingID is the newest EVIDENCE row's
// id (nil once the evidence was cascaded away with a deleted run), while a dismissed/done/filed
// coordinate always has a disposition id even with no evidence — which is exactly why undo keys
// on this, not on the evidence id. It is NOT omitempty: every real backlog row carries one.
//
// DismissReason (wont_do | not_an_issue, omitempty) and SetVia (issue_close, omitempty) surface
// the disposition's reason and provenance so the state chip can read "Dismissed · Won't do" and
// distinguish a hand-set done from an issue-close auto-done (PRD #1183 M3), the finding twin of
// JudgeOccurrenceDTO.SetVia. EvidencePreview is the newest evidence row's description_md as
// PLAIN TEXT, capped like the judge's rationale_preview (RationalePreviewMaxRunes + the same
// ellipsis), and — like every model-authored field — must be rendered as escaped text by every
// consumer, never markdown/HTML. Occurrences is the per-run evidence, newest-first, capped at 20.
// All four are omitempty: a resolved/dismissed coordinate may carry no evidence, and an open one
// no reason/provenance.
type IncidentalFindingDTO struct {
	DispositionID   string                 `json:"disposition_id"`
	FindingID       *string                `json:"finding_id,omitempty"`
	Location        string                 `json:"location"`
	RepoID          string                 `json:"repo_id"`
	RepoPath        string                 `json:"repo_path"`
	Status          string                 `json:"status"`
	LastTitle       string                 `json:"last_title"`
	SeenInRuns      int                    `json:"seen_in_runs"`
	DismissReason   string                 `json:"dismiss_reason,omitempty"`
	SetVia          string                 `json:"set_via,omitempty"`
	FiledIssueIID   *int64                 `json:"filed_issue_iid,omitempty"`
	FiledIssueURL   string                 `json:"filed_issue_url,omitempty"`
	ResolvedAt      *time.Time             `json:"resolved_at,omitempty"`
	EvidencePreview string                 `json:"evidence_preview,omitempty"`
	Occurrences     []FindingOccurrenceDTO `json:"occurrences,omitempty"`
}

// FindingOccurrenceDTO is one run's report of a finding coordinate (PRD #1183 M3): the finding
// twin of JudgeOccurrenceDTO's per-run slice. RunTitle is the reporting run's issue_title and
// ReportedAt is the evidence row's created_at (both typed exactly as JudgeOccurrenceDTO types
// run_title/judged_at, so a consumer never special-cases the shape). Confidence is the
// agent-supplied confidence string. All four keys are always present.
type FindingOccurrenceDTO struct {
	RunID      string    `json:"run_id"`
	RunTitle   string    `json:"run_title"`
	ReportedAt time.Time `json:"reported_at"`
	Confidence string    `json:"confidence"`
}

// FileFindingRequest is the (all-optional) body of POST /api/findings/{id}/issue (PRD #333 M5,
// exported in PRD #1183 M3 so the wire lives in apitypes beside the response). Every field is a
// user EDIT of the server-rendered draft, so none is trusted: the handler re-runs title/description
// through the field-level sanitisers and UNIONs labels with the server marker.
type FileFindingRequest struct {
	Title       *string  `json:"title"`
	Description *string  `json:"description"`
	Labels      []string `json:"labels"`
}

// DismissFindingRequest is the body of POST /api/findings/{id}/dismiss: a required reason from the
// closed enum {wont_do, not_an_issue}. The DB CHECK ((status='dismissed')=(reason IS NOT NULL)) is
// the backstop; the handler rejects a missing/invalid reason as a 400 first.
type DismissFindingRequest struct {
	Reason string `json:"reason"`
}

// BulkDismissFindingsRequest is the body of POST /api/findings/dismiss (PRD #1183 M3): a set of
// disposition ids and one shared reason. The handler caps Ids at 100 (a 400 above it) and skips a
// foreign or non-open id silently, so the request is idempotent-ish and owner-scoped by the query.
type BulkDismissFindingsRequest struct {
	IDs    []string `json:"ids"`
	Reason string   `json:"reason"`
}

// DismissFindingResultDTO is the typed POST /api/findings/{id}/dismiss response (PRD #1183 M3),
// replacing the untyped map[string]string the handler returned before. Status is always
// "dismissed" on the 200 path; Reason echoes the applied reason (omitempty is harmless — it is
// always present on a successful dismiss).
type DismissFindingResultDTO struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// BulkDismissFindingsResultDTO is the POST /api/findings/dismiss response (PRD #1183 M3). Updated
// is how many coordinates the one statement moved to dismissed (a foreign or non-open id is
// skipped silently, so Updated can be less than len(ids)); Findings re-reads the updated rows so
// the client can reconcile them in place. Findings is never nil on the wire (an empty result
// encodes []), so a client iterates it without a null guard.
type BulkDismissFindingsResultDTO struct {
	Updated  int                    `json:"updated"`
	Findings []IncidentalFindingDTO `json:"findings"`
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
