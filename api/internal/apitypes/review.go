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

// JudgeCategoryStatsDTO is GET /me/judge/category-stats (PRD #270): the caller's Judge
// filter-chip counts, scoped to the selected triage tab. CountsByBucket is a
// bucket → category → count MATRIX: the outer key is a bucket rollup name
// (todo, filed, done, dismissed, all) and the inner key is the raw rr.category column,
// one entry per category with at least one group IN THAT BUCKET. `all` is always present
// as a key so the frontend indexes uniformly — CountsByBucket[tab][cat] ?? 0 per chip,
// with tab == "all" the whole-backlog fallback.
//
// BOTH levels are maps so the taxonomy AND the bucket ladder can grow without a wire
// break: an unknown category simply has no chip to render on, and an absent bucket serves
// an empty inner map.
//
// The count is computed by running the SHARED Go rollup (workersvc.GroupJudgeRecommendations,
// "any open member ⇒ todo, else the group's highest settled rung") over an UNCAPPED
// whole-backlog row load, then tallying each group's rollup Bucket — PRD #94 Decision 2
// forbids re-expressing the bucket ladder in SQL, so there is no GROUP BY disposition_status.
// It is therefore TAB-SCOPED and TRIAGE-VARIANT (a triage action moves a group between
// buckets and so between chip tallies), unlike the whole-backlog invariant it replaced.
//
// It is a NEW DTO rather than a widening of TriageDTO: the count is a GROUP count (a card
// per group), not the ROW count TriageDTO carries, and keeping it on its own endpoint keeps
// the category dimension out of the polled nav-badge payload, which reads only
// TriageCounts.todo.
type JudgeCategoryStatsDTO struct {
	CountsByBucket map[string]map[string]int `json:"counts_by_bucket"`
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
	// SetVia is the disposition's PROVENANCE (PRD #98 Decision 6): "" (omitted) means a
	// PERSON set it, "issue_close" means the M6 poller sync did when the filed issue closed.
	// A client MUST render the two differently — an auto-done reads "done via #IID" — because
	// "I decided this was done" and "the system inferred it from a closed issue" are
	// different claims, and only one of them is the user's.
	//
	// The entire set_via mechanism exists for this one visible distinction, guarded from both
	// directions in SQL: the sync writes set_via='issue_close' with set_by_user_id NULL so a
	// system action is never attributed to a person, and every human write clears set_via
	// back to a literal NULL so a person's action is never attributed to the system. None of
	// that reached a client until this field existed, and the chip rendered both identically.
	//
	// omitempty because the overwhelmingly common case is a hand-set (or absent) disposition:
	// the field carries meaning only when present.
	SetVia string `json:"set_via,omitempty"`
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
// Updated counts member COORDINATES — (review_id, category, target) triples — not
// recommendation rows. The two differ: a review may carry the same coordinate on two
// recommendations, and since recommendation_dispositions is keyed on the coordinate, both
// share ONE verdict and contribute ONE to this count. So Updated can be lower than the
// number of recommendations a group visibly spans, and that is correct.
//
// It is deliberately an aggregate and never a per-item breakdown: a coordinate that does
// not exist and one that belongs to another user both resolve to zero members, so neither
// the count nor anything else in this DTO can be used to tell them apart (#94 Decision 5's
// one-404 rule — no existence oracle). A caller learning "0 written" learns only that none
// of THEIR rows matched.
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
// Settled names the coordinates this call actually wrote, as addresses a caller can undo
// through. It exists because the client CANNOT compute that set (PRD #98 review BLK-UNDO):
// with scope=open, membership is decided SERVER-SIDE at write time, so any member settled
// between the client's last read and this write is in the client's view of "open" and
// outside the action. Undoing from that stale view deletes dispositions the action never
// created — and for an M6 issue-close auto-done that is IRREVERSIBLE, because
// close_synced_at is already stamped and the poller is edge-triggered, so the auto-done
// never re-fires and the set_via='issue_close' provenance is gone.
//
// Updated cannot stand in for it: it is a bare count, and a count cannot say WHICH.
type JudgeDispositionResultDTO struct {
	Updated   int                           `json:"updated"`
	Settled   []JudgeSettledMemberDTO       `json:"settled"`
	Groups    []JudgeRecommendationGroupDTO `json:"groups"`
	Truncated bool                          `json:"truncated"`
	Triage    TriageDTO                     `json:"triage"`
}

// JudgeSettledMemberDTO is one coordinate a bulk disposition wrote, addressed as the
// (run, recommendation) pair the per-recommendation disposition route takes (PRD #98 review
// BLK-UNDO). It is an ADDRESS, not a grain: dispositions are keyed on the (review_id,
// category, target) coordinate, so two recommendations sharing a coordinate share ONE
// disposition row and either one of them names it. Exactly one member per settled
// coordinate is returned, so len(Settled) is the coordinate count Updated reports — a
// consumer that finds them disagreeing has found a bug, not a subtlety.
//
// A caller undoes by DELETEing each pair's disposition. Doing so is safe even if the
// coordinate was settled twice, because that route treats "no disposition" as already-undone
// rather than an error.
type JudgeSettledMemberDTO struct {
	RunID string `json:"run_id"`
	RecID string `json:"rec_id"`
}

// JudgeDispositionCoordDTO is one (category, target) coordinate in a bulk group-disposition
// request (PRD #98 M2/M7). It is the DISPLAY grain the Judge menu groups by, deliberately
// NOT a recommendation id, because one group spans many runs and therefore many ids. It is
// the caller's REQUEST, never a resolved row: nothing here is written to the database — the
// values only match against review_recommendations, and the disposition is written from the
// resolved row's own columns.
//
// It lives in apitypes rather than in workersvc so the CLI ENCODES the same struct the
// handler DECODES: workersvc.JudgeDispositionCoord is a type alias of this, so there is one
// set of JSON tags and a client/server key mismatch is not expressible. That matters more
// than usual here — the handler decodes with DisallowUnknownFields, so a client-side key
// typo would be a 400 rather than a quietly-ignored field. uzicli could not have referenced
// the workersvc type in any case: importing workersvc drags pgx into the CLI binary and
// turns cmd/uzi's TestNoServerDeps red.
type JudgeDispositionCoordDTO struct {
	Category string `json:"category"`
	Target   string `json:"target"`
}

// JudgeBulkDispositionRequest is the PUT /api/me/judge/recommendations/disposition body
// (PRD #98 M2, Decision 3): one triage verdict fanned out to every member coordinate of the
// listed groups.
//
// Status ∈ {done, dismissed}; Reason ∈ {wont_do, not_an_issue} and is legal only on a
// dismissal. Scope ∈ {open, all}, and EMPTY IS MEANINGFUL: the handler reads "" as the
// default open scope (settle what is open, never re-assert a settled member). A client that
// does not expose a scope choice therefore sends the zero value rather than naming a scope,
// which is why the CLI spells neither wire value — see uzicli.Client.BulkSetDispositions.
type JudgeBulkDispositionRequest struct {
	Items  []JudgeDispositionCoordDTO `json:"items"`
	Status string                     `json:"status"`
	Reason string                     `json:"reason"`
	Scope  string                     `json:"scope"`
}

// PendingJudgeDTO is the ACTIVE judge run for a target (PRD #119 M1) — "a verdict is
// already on its way", the fact the run page could not previously tell apart from "this
// run was never judged".
//
// State is the NORMALIZED display value, never the raw runs.status: "scheduled" (the
// judge is enqueued and unclaimed) or "running" (a worker has it). The mapping is done by
// a TOTAL mapper server-side (handler.pendingJudgeState) so a client can treat the field
// as the closed union "scheduled" | "running" — the raw status set is wider than those
// two and is allowed to grow, and a client is the wrong place to learn that. EnqueuedAt
// is the judge run's created_at.
//
// It is a SIBLING key on the review response — {"review": …, "pending_judge": …} — and
// deliberately NOT a field of ReviewDTO. A pending judge is orthogonal to a verdict and
// is present precisely when there may be no review at all (the common case: an
// auto-judge enqueued the moment the run finished), so hanging it off ReviewDTO would
// make it unreachable in the one state it exists to describe.
type PendingJudgeDTO struct {
	State      string    `json:"state"`
	EnqueuedAt time.Time `json:"enqueued_at"`
}

// JudgeRunDTO is the judge run's own timing + token/cost usage for the reviewed run's
// panel (PRD #69 M6, Decision 10). Judge spend is the owner's own token spend: the judge
// run posts its terminal result frame like any work run, so foldRunUsage writes a
// run_usage row and it also rolls into the owner's usage totals — the CHOSEN behavior.
//
// ClaimedAt/StartedAt/FinishedAt are the judge run's lifecycle stamps; StartedAt is set
// once the worker reports `running`, so it is null for a pre-feature judge that never
// did (leaving the panel to show no duration). Usage is nil when the judge posted no
// result frame (every pre-feature judge) — the panel then renders NO cost/time strip,
// never a fabricated 0.
type JudgeRunDTO struct {
	JudgeRunID string     `json:"judge_run_id"`
	ClaimedAt  *time.Time `json:"claimed_at"`
	StartedAt  *time.Time `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Usage      *UsageDTO  `json:"usage"`
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
	// JudgeRun is the judge run's own timing + usage (PRD #69 M6), for the panel's
	// time/tokens/cost strip. Omitted when there is no judge-run detail to surface.
	JudgeRun *JudgeRunDTO `json:"judge_run,omitempty"`
}
