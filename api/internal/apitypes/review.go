package apitypes

import "time"

// RecommendationDTO is one structured judge recommendation for the run-page panel
// (PRD #46 M4). category is the taxonomy enum; target/rationale are the scrubbed
// free-text fields (validated + capped + secret-scrubbed at the review POST), which
// the SPA renders as escaped text (never markdown/HTML).
type RecommendationDTO struct {
	ID          string    `json:"id"`
	Category    string    `json:"category"`
	Target      string    `json:"target"`
	RationaleMd string    `json:"rationale_md"`
	Confidence  string    `json:"confidence"`
	CreatedAt   time.Time `json:"created_at"`
}

// IssueDraftDTO is the templated, human-editable draft the "file an issue from a
// recommendation" flow serves (PRD #68 M2, Decision 2). The body is deterministically
// rendered from rows uzi already holds (no LLM call); every untrusted field is fenced +
// stripped + secret-scanned server-side, but this draft is NOT the security boundary —
// M3 re-applies the write-boundary controls to the client's POST body. default_repo_id
// is a pre-selection ("" when no default resolves, mock state D); labels are assembled
// server-side (PRD + PRDLESS, never autopilot); provenance names whose worker produced
// the (attacker-influencable) text so an admin filing another user's review sees it.
type IssueDraftDTO struct {
	DefaultRepoID string   `json:"default_repo_id"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Labels        []string `json:"labels"`
	Provenance    string   `json:"provenance"`
	// DefaultNote explains the default repo (mock B hint) or its absence (mock D info
	// alert) — the picker copy the category→repo resolution produced.
	DefaultNote string `json:"default_note"`
}

// FiledIssueDTO is a SETTLED recommendation→forge-issue link for the run-page panel
// (PRD #68 M4): the coordinate it covers, the created issue, and when it was filed so the
// panel can render a filed row (with an issue link) instead of the File-issue button and
// flag a stale link (filed_at < review.updated_at → "filed for an earlier version"). Only
// settled links appear; an in-flight claim is transient and omitted.
type FiledIssueDTO struct {
	Category string    `json:"category"`
	Target   string    `json:"target"`
	IssueIID int64     `json:"issue_iid"`
	IssueURL string    `json:"issue_url"`
	FiledAt  time.Time `json:"filed_at"`
}

// DispositionDTO is one user triage verdict on a judge recommendation for the run-page
// panel (PRD #94 Decision 7). Keyed on the same (category, target) coordinate as
// FiledIssueDTO, so the panel matches it to a recommendation and renders the chip + Undo.
// Reason is empty unless status=="dismissed". Stale is computed server-side (the
// rationale-hash compare, Decision 3) — "the recommendation changed since you resolved it"
// — so the browser never sees a hash.
type DispositionDTO struct {
	Category string    `json:"category"`
	Target   string    `json:"target"`
	Status   string    `json:"status"`
	Reason   string    `json:"reason"`
	SetAt    time.Time `json:"set_at"`
	Stale    bool      `json:"stale"`
}

// TriageDTO is the recommendation tally, per-review and global (PRD #94 Decisions 2/7/8).
// Every count is bucketed by the one shared Go ladder (dismissed > done > filed > todo),
// so the per-review bar and the global strip cannot drift. FalsePositives is the
// not_an_issue sub-count of Dismissed. Total is the recommendation-row denominator.
type TriageDTO struct {
	Total          int `json:"total"`
	Todo           int `json:"todo"`
	Filed          int `json:"filed"`
	Done           int `json:"done"`
	Dismissed      int `json:"dismissed"`
	FalsePositives int `json:"false_positives"`
}

// JudgeFiledIssueRefDTO is the forge issue a single occurrence was filed as (PRD #98 M1).
// It is the lean, coordinate-free cousin of FiledIssueDTO: inside an occurrence the
// (category, target) coordinate is the enclosing group's, so only the issue itself is
// carried. Present ONLY on a SETTLED link — an in-flight claim is omitted, matching the
// run-page panel and the "filed means settled" rung of the #94 ladder.
type JudgeFiledIssueRefDTO struct {
	IssueIID int64     `json:"issue_iid"`
	IssueURL string    `json:"issue_url"`
	FiledAt  time.Time `json:"filed_at"`
}

// JudgeOccurrenceDTO is one run's instance of a deduped recommendation (PRD #98
// Decision 2): the group is a display construct, but triage state stays PER-COORDINATE,
// so every occurrence carries its own bucket and its own filed link. run_title is the
// target run's issue_title. Bucket comes from the shared workersvc.BucketOf ladder
// (#94 Decision 2) — never a SQL CASE, never a second implementation.
type JudgeOccurrenceDTO struct {
	RunID      string `json:"run_id"`
	RunTitle   string `json:"run_title"`
	ReviewID   string `json:"review_id"`
	RecID      string `json:"rec_id"`
	Verdict    string `json:"verdict"`
	Confidence string `json:"confidence"`
	Bucket     string `json:"bucket"`
	// FiledIssue is the settled forge issue for this occurrence's coordinate, nil when
	// the coordinate was never filed (or the claim is still in flight).
	FiledIssue *JudgeFiledIssueRefDTO `json:"filed_issue,omitempty"`
}

// JudgeRecommendationGroupDTO is one (category, target) coordinate deduped across every
// run it recurs in (PRD #98 Decisions 1/2) — the Judge menu's row. OpenCount is the
// number of members whose bucket is todo ("open" means todo; a FILED member is not open,
// it is on the filed rung). RunCount is the DISTINCT run count behind the group — the
// "seen in N runs" evidence chip, and the frequency signal the backlog ranks by. Bucket
// is the group rollup: todo whenever OpenCount >= 1, otherwise the HIGHEST member state
// on the #94 ladder (dismissed > done > filed).
//
// RationalePreview is the most-recent occurrence's rationale_md, TRUNCATED to a preview
// cap (workersvc.RationalePreviewMaxRunes) and shipped as PLAIN TEXT — deliberately NOT
// server-side HTML-escaped. It is scrubbed at ingest, and the no-raw-render guarantee is
// CLIENT-side: every consumer must render it as escaped text (React's default, as
// web/src/pages/RunView.tsx does for the same fields), never as markdown or HTML.
// Escaping it here would double-escape in the SPA and print HTML entities into the
// terminal from `uzi review backlog`.
type JudgeRecommendationGroupDTO struct {
	Category         string               `json:"category"`
	Target           string               `json:"target"`
	Bucket           string               `json:"bucket"`
	OpenCount        int                  `json:"open_count"`
	RunCount         int                  `json:"run_count"`
	RationalePreview string               `json:"rationale_preview"`
	Occurrences      []JudgeOccurrenceDTO `json:"occurrences"`
}

// JudgeBacklogDTO is GET /api/me/judge/recommendations (PRD #98 M1): the caller's
// all-time, owner-scoped recommendation backlog, deduped by (category, target).
//
// Bucket echoes the applied ?bucket= filter (default todo) and Run echoes the ?run=
// anchor ("" when absent). Triage is deliberately NOT tallied from Groups: it is the
// aggregate GET /me/judge/stats serves, read from the same query through the same shared
// BucketTriage ladder, so the page's bucket tabs, the nav badge and the strip cannot drift
// no matter which filter is applied or whether the page was truncated.
//
// Truncated says the backlog hit its hard row cap (workersvc.JudgeBacklogMaxRows). Read
// this carefully before rendering a truncated page, because the cut is NOT simply "the
// tail of the list is missing":
//
// The cap bounds ROWS and it applies BEFORE grouping, so a group that SURVIVES the cut can
// still have lost occurrences. When Truncated is true, a surviving group's RunCount and
// OpenCount may be UNDERSTATED and its Bucket rollup may be WRONG — a group whose only
// open occurrence fell outside the cut rolls up done/dismissed instead of todo, and is
// then filtered out of the default ?bucket=todo view entirely. So a truncated page must
// never be presented as authoritative.
//
// What is missing is the LEAST-RECENTLY-JUDGED occurrences: the read is ordered by
// run_reviews.updated_at DESC, so "oldest" means oldest by last judging, and a run
// re-judged today counts as recent no matter when it first ran.
//
// Triage is unaffected — it comes from the #94 stats query over the caller's whole row
// set, so the canonical counts remain correct even here. That asymmetry is deliberate: the
// numbers stay right while the list goes partial.
type JudgeBacklogDTO struct {
	Bucket    string                        `json:"bucket"`
	Run       string                        `json:"run"`
	Groups    []JudgeRecommendationGroupDTO `json:"groups"`
	Truncated bool                          `json:"truncated"`
	Triage    TriageDTO                     `json:"triage"`
}

// JudgeDispositionResultDTO is the response to the bulk group-disposition fan-out
// (PRD #98 M2, Decision 3).
//
// Updated is the number of member COORDINATES actually written. It is deliberately an
// aggregate and never a per-item breakdown: a coordinate that does not exist and one that
// belongs to another user both resolve to zero members, so neither the count nor anything
// else in this DTO can be used to tell them apart (#94 Decision 5's one-404 rule — no
// existence oracle). A caller learning "0 written" learns only that none of THEIR rows
// matched.
//
// Groups are the affected coordinates re-read after the writes (all buckets, so a group
// that just left To triage is still returned with its new rollup), and Triage is the
// recomputed canonical tally — together enough for the page to update its rows AND its
// badge from this one round-trip, with no follow-up GET.
//
// Truncated carries through from that re-read, which is bounded by the same hard row cap as
// the backlog (workersvc.JudgeBacklogMaxRows). It matters because of a specific interaction:
// a user past the cap can settle a coordinate that lies OUTSIDE the read window and get
// Updated > 0 with NO corresponding entry in Groups. Without this flag a consumer cannot
// tell that from "the group is gone", and would make the row vanish mid-interaction —
// exactly what re-reading at bucket=all exists to prevent. When Truncated is true, treat a
// missing group as UNKNOWN, not settled.
type JudgeDispositionResultDTO struct {
	Updated   int                           `json:"updated"`
	Groups    []JudgeRecommendationGroupDTO `json:"groups"`
	Truncated bool                          `json:"truncated"`
	Triage    TriageDTO                     `json:"triage"`
}

// ReviewDTO is the run's judge verdict + recommendations for the run page. summary_md
// and each rationale_md were scrubbed at ingest; the SPA renders them as escaped text.
type ReviewDTO struct {
	ID              string              `json:"id"`
	TargetRunID     string              `json:"target_run_id"`
	Verdict         string              `json:"verdict"`
	SummaryMd       string              `json:"summary_md"`
	JudgeModel      string              `json:"judge_model"`
	Status          string              `json:"status"`
	CreatedAt       time.Time           `json:"created_at"`
	UpdatedAt       time.Time           `json:"updated_at"`
	Recommendations []RecommendationDTO `json:"recommendations"`
	// FiledIssues are the settled recommendation→issue links for this review (PRD #68).
	FiledIssues []FiledIssueDTO `json:"filed_issues"`
	// Dispositions are the user's triage verdicts on this review's recommendations (PRD #94).
	Dispositions []DispositionDTO `json:"dispositions"`
	// Triage is the server-computed per-review tally (PRD #94), rendered directly by the panel.
	Triage TriageDTO `json:"triage"`
}
