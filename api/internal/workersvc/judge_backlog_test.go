package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// judgeBacklogRows backs ListJudgeRecommendationRowsForUser on the shared fakeStore, and
// backlogUserArg records the id the query was scoped to (the owner-scoping assertion).
// Declared here so the PRD #98 fixture lives next to the tests that use it.
func (f *fakeStore) ListJudgeRecommendationRowsForUser(_ context.Context, userID uuid.UUID) ([]store.ListJudgeRecommendationRowsForUserRow, error) {
	f.backlogUserArg = userID
	return f.judgeBacklogRows, nil
}

// ---- fixture builder ----------------------------------------------------------------

// backlogRow builds one wide backlog row. Every field the grouper reads is explicit, so a
// test reads as the SQL row it stands for.
type backlogRow struct {
	runID       uuid.UUID
	runTitle    string
	reviewID    uuid.UUID
	recID       uuid.UUID
	verdict     string
	category    string
	target      string
	rationale   string
	confidence  string
	disposition string // "" = no disposition row
	reason      string
	filedIID    int64 // 0 = no filed link
	filedURL    string
	filedAt     time.Time // zero = claimed-but-unsettled (or not filed at all)
}

func (b backlogRow) row() store.ListJudgeRecommendationRowsForUserRow {
	txt := func(s string) pgtype.Text {
		if s == "" {
			return pgtype.Text{}
		}
		return pgtype.Text{String: s, Valid: true}
	}
	out := store.ListJudgeRecommendationRowsForUserRow{
		ReviewID:          b.reviewID,
		RunID:             b.runID,
		Verdict:           b.verdict,
		RunTitle:          b.runTitle,
		RecID:             b.recID,
		Category:          b.category,
		Target:            b.target,
		RationaleMd:       b.rationale,
		Confidence:        b.confidence,
		DispositionStatus: txt(b.disposition),
		DismissReason:     txt(b.reason),
		FiledSettled:      !b.filedAt.IsZero(),
	}
	if b.filedIID != 0 {
		out.FiledIssueIid = pgtype.Int8{Int64: b.filedIID, Valid: true}
		out.FiledIssueUrl = txt(b.filedURL)
	}
	if !b.filedAt.IsZero() {
		out.FiledAt = pgtype.Timestamptz{Time: b.filedAt, Valid: true}
	}
	return out
}

func rowsOf(bs ...backlogRow) []store.ListJudgeRecommendationRowsForUserRow {
	out := make([]store.ListJudgeRecommendationRowsForUserRow, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.row())
	}
	return out
}

func groupByCoord(t *testing.T, groups []apitypes.JudgeRecommendationGroupDTO, category, target string) apitypes.JudgeRecommendationGroupDTO {
	t.Helper()
	for _, g := range groups {
		if g.Category == category && g.Target == target {
			return g
		}
	}
	t.Fatalf("no group for (%s, %s) in %+v", category, target, groups)
	return apitypes.JudgeRecommendationGroupDTO{}
}

// ---- dedup (the M1 gating test) ------------------------------------------------------

// TestGroupJudgeRecommendationsDedupsAcrossRuns is the PRD #98 M1 headline: the SAME
// (category, target) appearing in TWO runs collapses into ONE group whose occurrence list
// carries both runs with their own per-run triage state (Decision 2 — the group is a
// display construct; state stays per-coordinate). A different coordinate in the same runs
// stays its own group, so the dedup is by coordinate, not "one group per user".
func TestGroupJudgeRecommendationsDedupsAcrossRuns(t *testing.T) {
	runA, runB := uuid.New(), uuid.New()
	revA, revB := uuid.New(), uuid.New()
	recA, recB := uuid.New(), uuid.New()

	groups := GroupJudgeRecommendations(rowsOf(
		// Most-recent review first (the query's order).
		backlogRow{runID: runB, runTitle: "fix the flake", reviewID: revB, recID: recB,
			verdict: "issues", category: "install_worker_tool", target: "rg",
			rationale: "newest rationale", confidence: "high"},
		backlogRow{runID: runB, runTitle: "fix the flake", reviewID: revB, recID: uuid.New(),
			verdict: "issues", category: "improve_agent", target: "coder", rationale: "read the error"},
		backlogRow{runID: runA, runTitle: "add the endpoint", reviewID: revA, recID: recA,
			verdict: "ok", category: "install_worker_tool", target: "rg",
			rationale: "older rationale", confidence: "medium", disposition: "dismissed", reason: "wont_do"},
	))

	if len(groups) != 2 {
		t.Fatalf("want 2 groups (one per coordinate), got %d: %+v", len(groups), groups)
	}
	g := groupByCoord(t, groups, "install_worker_tool", "rg")
	if g.RunCount != 2 {
		t.Errorf("run_count = %d, want 2 (seen in two runs)", g.RunCount)
	}
	if len(g.Occurrences) != 2 {
		t.Fatalf("want 2 occurrences, got %d: %+v", len(g.Occurrences), g.Occurrences)
	}
	// Occurrence 0 is the most recent (query order preserved); each carries its own run,
	// review, rec, verdict, confidence and bucket.
	newest, oldest := g.Occurrences[0], g.Occurrences[1]
	if newest.RunID != runB.String() || newest.RunTitle != "fix the flake" || newest.ReviewID != revB.String() || newest.RecID != recB.String() {
		t.Errorf("newest occurrence identity wrong: %+v", newest)
	}
	if newest.Verdict != "issues" || newest.Confidence != "high" || newest.Bucket != "todo" {
		t.Errorf("newest occurrence facts wrong: %+v", newest)
	}
	if oldest.RunID != runA.String() || oldest.RunTitle != "add the endpoint" || oldest.RecID != recA.String() {
		t.Errorf("oldest occurrence identity wrong: %+v", oldest)
	}
	if oldest.Verdict != "ok" || oldest.Bucket != "dismissed" {
		t.Errorf("per-run triage state must survive the grouping: %+v", oldest)
	}
	// One open member → the group is To triage, and open_count counts ONLY that member.
	if g.OpenCount != 1 || g.Bucket != "todo" {
		t.Errorf("open_count/bucket = %d/%q, want 1/todo (any open member makes the group To triage)", g.OpenCount, g.Bucket)
	}
	// rationale_preview is the MOST-RECENT occurrence's rationale (Decision 1).
	if g.RationalePreview != "newest rationale" {
		t.Errorf("rationale_preview = %q, want the most-recent occurrence's rationale_md", g.RationalePreview)
	}
	if other := groupByCoord(t, groups, "improve_agent", "coder"); other.RunCount != 1 || len(other.Occurrences) != 1 {
		t.Errorf("a different coordinate must stay its own group: %+v", other)
	}
}

// TestGroupJudgeRecommendationsRunCountIsDistinctRuns: review_recommendations has no
// unique constraint on (review_id, category, target), so a judge CAN emit the same
// coordinate twice in one review. Both rows are occurrences, but "seen in N runs" must
// count the RUN once — otherwise the frequency ranking would be gameable by a repetitive
// judge.
func TestGroupJudgeRecommendationsRunCountIsDistinctRuns(t *testing.T) {
	run, rev := uuid.New(), uuid.New()
	groups := GroupJudgeRecommendations(rowsOf(
		backlogRow{runID: run, reviewID: rev, recID: uuid.New(), verdict: "issues", category: "improve_uzi", target: "docs"},
		backlogRow{runID: run, reviewID: rev, recID: uuid.New(), verdict: "issues", category: "improve_uzi", target: "docs"},
	))
	if len(groups) != 1 {
		t.Fatalf("want 1 group, got %d", len(groups))
	}
	if got := groups[0]; got.RunCount != 1 || len(got.Occurrences) != 2 || got.OpenCount != 2 {
		t.Fatalf("run_count/occurrences/open_count = %d/%d/%d, want 1/2/2", got.RunCount, len(got.Occurrences), got.OpenCount)
	}
}

// TestGroupJudgeRecommendationsRationalePreviewIsCappedPlainText: the preview is
// TRUNCATED at the rune cap (PRD #98 Decision 1 — the payload bound on an all-time
// backlog), cuts on a rune boundary rather than a byte one, and is NOT server-side
// HTML-escaped: markup-looking characters come through verbatim for the consumer to render
// as escaped text (the client-side no-raw-render rule).
func TestGroupJudgeRecommendationsRationalePreviewIsCappedPlainText(t *testing.T) {
	// Multi-byte runes: a byte-wise truncation would produce invalid UTF-8 here.
	long := strings.Repeat("ă", RationalePreviewMaxRunes+50)
	short := `<script>alert("x")</script> & "quotes"`
	groups := GroupJudgeRecommendations(rowsOf(
		backlogRow{runID: uuid.New(), reviewID: uuid.New(), recID: uuid.New(), category: "improve_uzi", target: "long", rationale: long},
		backlogRow{runID: uuid.New(), reviewID: uuid.New(), recID: uuid.New(), category: "improve_uzi", target: "raw", rationale: short},
	))

	got := groupByCoord(t, groups, "improve_uzi", "long").RationalePreview
	wantRunes := RationalePreviewMaxRunes + 1 // the cap plus the ellipsis
	if n := len([]rune(got)); n != wantRunes {
		t.Fatalf("preview is %d runes, want %d (cap + ellipsis)", n, wantRunes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation must cut on a rune boundary, not a byte one")
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated preview must be marked with an ellipsis, got %q", got)
	}

	if raw := groupByCoord(t, groups, "improve_uzi", "raw").RationalePreview; raw != short {
		t.Errorf("preview = %q, want the rationale VERBATIM (plain text, not server-escaped)", raw)
	}
}

// ---- rollup (Decision 2's documented display quirk) ----------------------------------

// TestGroupJudgeRecommendationsRollup pins Decision 2's rollup: a group with ANY open
// member is To triage; a FULLY-SETTLED group shows at the HIGHEST member state on the #94
// ladder (dismissed > done > filed). The 3-done + 1-dismissed case is the quirk the PRD
// documents explicitly.
func TestGroupJudgeRecommendationsRollup(t *testing.T) {
	filed := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	run := func() uuid.UUID { return uuid.New() }
	cases := []struct {
		name       string
		rows       []store.ListJudgeRecommendationRowsForUserRow
		wantBucket string
		wantOpen   int
	}{
		{
			name: "any open member wins over a settled one",
			rows: rowsOf(
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", disposition: "dismissed", reason: "not_an_issue"},
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x"},
			),
			wantBucket: "todo", wantOpen: 1,
		},
		{
			name: "fully settled: 3 done + 1 dismissed rolls up dismissed (highest wins)",
			rows: rowsOf(
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", disposition: "done"},
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", disposition: "done"},
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", disposition: "done"},
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", disposition: "dismissed", reason: "wont_do"},
			),
			wantBucket: "dismissed", wantOpen: 0,
		},
		{
			name: "fully settled: done beats filed",
			rows: rowsOf(
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", filedIID: 7, filedURL: "https://f/7", filedAt: filed},
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", disposition: "done"},
			),
			wantBucket: "done", wantOpen: 0,
		},
		{
			name: "a settled filed link alone rolls up filed — filed is NOT open",
			rows: rowsOf(
				backlogRow{runID: run(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "x", filedIID: 7, filedURL: "https://f/7", filedAt: filed},
			),
			wantBucket: "filed", wantOpen: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups := GroupJudgeRecommendations(tc.rows)
			if len(groups) != 1 {
				t.Fatalf("want 1 group, got %d", len(groups))
			}
			if groups[0].Bucket != tc.wantBucket || groups[0].OpenCount != tc.wantOpen {
				t.Fatalf("bucket/open_count = %q/%d, want %q/%d", groups[0].Bucket, groups[0].OpenCount, tc.wantBucket, tc.wantOpen)
			}
		})
	}
}

// TestGroupJudgeRecommendationsFiledIssueOnlySettled: an UNSETTLED claim (#68 writes the
// row before the forge call settles it) is not a filed occurrence — it buckets todo and
// carries NO filed_issue, exactly as the run-page panel treats it. A settled link carries
// the iid/url/filed_at through.
func TestGroupJudgeRecommendationsFiledIssueOnlySettled(t *testing.T) {
	filed := time.Date(2026, 7, 2, 9, 30, 0, 0, time.UTC)
	groups := GroupJudgeRecommendations(rowsOf(
		backlogRow{runID: uuid.New(), reviewID: uuid.New(), recID: uuid.New(), category: "improve_uzi", target: "settled",
			filedIID: 42, filedURL: "https://forge/issues/42", filedAt: filed},
		// A claim with no filed_at: the row exists, but it is not settled.
		backlogRow{runID: uuid.New(), reviewID: uuid.New(), recID: uuid.New(), category: "improve_uzi", target: "claimed"},
	))
	settled := groupByCoord(t, groups, "improve_uzi", "settled")
	if settled.Occurrences[0].Bucket != "filed" {
		t.Errorf("a settled link must bucket filed, got %q", settled.Occurrences[0].Bucket)
	}
	ref := settled.Occurrences[0].FiledIssue
	if ref == nil || ref.IssueIID != 42 || ref.IssueURL != "https://forge/issues/42" || !ref.FiledAt.Equal(filed) {
		t.Errorf("filed_issue = %+v, want the settled issue", ref)
	}
	claimed := groupByCoord(t, groups, "improve_uzi", "claimed")
	if claimed.Occurrences[0].Bucket != "todo" || claimed.Occurrences[0].FiledIssue != nil {
		t.Errorf("an unsettled claim must be todo with no filed_issue, got %+v", claimed.Occurrences[0])
	}
}

// TestGroupJudgeRecommendationsRanksByRecurrence: frequency is the priority signal
// (Solution Overview), so the group seen in more runs sorts first regardless of the
// judge's self-reported confidence or of arrival order.
func TestGroupJudgeRecommendationsRanksByRecurrence(t *testing.T) {
	rare := backlogRow{runID: uuid.New(), reviewID: uuid.New(), recID: uuid.New(), category: "add_agent", target: "rare", confidence: "high"}
	common := func() backlogRow {
		return backlogRow{runID: uuid.New(), reviewID: uuid.New(), recID: uuid.New(), category: "improve_agent", target: "common", confidence: "low"}
	}
	groups := GroupJudgeRecommendations(rowsOf(rare, common(), common(), common()))
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	if groups[0].Target != "common" || groups[0].RunCount != 3 {
		t.Fatalf("groups[0] = %+v, want the 3-run 'common' group first (frequency ranks the backlog)", groups[0])
	}
}

// ---- service: filters, owner scoping, and the shared-ladder triage -------------------

// backlogFixture is one owner's backlog spanning three coordinates and four runs, with a
// member in every rung of the ladder — enough to exercise every bucket filter and the
// run anchor from ONE fixture.
func backlogFixture() (rows []store.ListJudgeRecommendationRowsForUserRow, anchorRun uuid.UUID) {
	filed := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)
	anchorRun = uuid.New()
	runOld, runDone, runFiled := uuid.New(), uuid.New(), uuid.New()
	return rowsOf(
		// (install_worker_tool, rg): open in the anchor run, dismissed in an older run → todo.
		backlogRow{runID: anchorRun, runTitle: "anchor", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "issues", category: "install_worker_tool", target: "rg", rationale: "need rg"},
		backlogRow{runID: runOld, runTitle: "older", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "ok", category: "install_worker_tool", target: "rg", rationale: "needed rg then",
			disposition: "dismissed", reason: "not_an_issue"},
		// (improve_uzi, docs): a single done member → rolls up done. Not in the anchor run.
		backlogRow{runID: runDone, runTitle: "done run", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "ok", category: "improve_uzi", target: "docs", disposition: "done"},
		// (improve_agent, coder): a settled filed link → rolls up filed. Not in the anchor run.
		backlogRow{runID: runFiled, runTitle: "filed run", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "issues", category: "improve_agent", target: "coder",
			filedIID: 11, filedURL: "https://forge/11", filedAt: filed},
	), anchorRun
}

// TestJudgeRecommendationBacklogFilters walks the ?bucket= enum against the group ROLLUP
// (PRD #98 M1): todo is exactly "open_count >= 1", the settled rungs are mutually
// exclusive with it, and all returns everything. The echoed bucket always names the
// applied filter.
func TestJudgeRecommendationBacklogFilters(t *testing.T) {
	rows, _ := backlogFixture()
	fs := &fakeStore{judgeBacklogRows: rows}
	svc := New(fs, newBox(t), testParams())
	owner := uuid.New()

	cases := []struct {
		bucket  string
		targets []string
	}{
		{"todo", []string{"rg"}},
		{"done", []string{"docs"}},
		{"filed", []string{"coder"}},
		{"dismissed", nil}, // no group is FULLY dismissed — rg still has an open member
		{"all", []string{"rg", "docs", "coder"}},
	}
	for _, tc := range cases {
		t.Run(tc.bucket, func(t *testing.T) {
			got, err := svc.JudgeRecommendationBacklog(context.Background(), owner, tc.bucket, uuid.Nil)
			if err != nil {
				t.Fatalf("backlog: %v", err)
			}
			if got.Bucket != tc.bucket {
				t.Errorf("echoed bucket = %q, want %q", got.Bucket, tc.bucket)
			}
			if len(got.Groups) != len(tc.targets) {
				t.Fatalf("bucket=%s returned %d groups, want %d: %+v", tc.bucket, len(got.Groups), len(tc.targets), got.Groups)
			}
			seen := map[string]bool{}
			for _, g := range got.Groups {
				seen[g.Target] = true
			}
			for _, want := range tc.targets {
				if !seen[want] {
					t.Errorf("bucket=%s missing the %q group: %+v", tc.bucket, want, got.Groups)
				}
			}
			// Owner scoping: the query is always called with the caller's id.
			if fs.backlogUserArg != owner {
				t.Errorf("backlog query scoped to %v, want the caller %v", fs.backlogUserArg, owner)
			}
		})
	}
}

// TestJudgeRecommendationBacklogRunAnchor: ?run= (the notification deep-link) keeps only
// groups that recur in that run — but never trims the occurrence list, because seeing that
// the recommendation ALSO recurs elsewhere is the reason to land here. An unknown run id
// simply matches nothing (no ownership oracle).
func TestJudgeRecommendationBacklogRunAnchor(t *testing.T) {
	rows, anchor := backlogFixture()
	svc := New(&fakeStore{judgeBacklogRows: rows}, newBox(t), testParams())
	owner := uuid.New()

	got, err := svc.JudgeRecommendationBacklog(context.Background(), owner, "all", anchor)
	if err != nil {
		t.Fatalf("backlog: %v", err)
	}
	if got.Run != anchor.String() {
		t.Errorf("echoed run = %q, want %q", got.Run, anchor)
	}
	if len(got.Groups) != 1 || got.Groups[0].Target != "rg" {
		t.Fatalf("run anchor returned %+v, want only the group occurring in that run", got.Groups)
	}
	if len(got.Groups[0].Occurrences) != 2 {
		t.Errorf("the anchor must not trim the occurrence list, got %d occurrences", len(got.Groups[0].Occurrences))
	}

	got, err = svc.JudgeRecommendationBacklog(context.Background(), owner, "all", uuid.New())
	if err != nil {
		t.Fatalf("backlog (unknown run): %v", err)
	}
	if len(got.Groups) != 0 {
		t.Fatalf("an unknown run anchor must match nothing, got %+v", got.Groups)
	}
}

// TestJudgeRecommendationBacklogTriageIgnoresFilters: Triage is the ONE canonical tally
// (PRD #98 Decision 1 / Success Criteria — the nav badge, the notification and the To
// triage tab must show the same number). It is computed over the caller's WHOLE row set
// through the shared BucketTriage, so narrowing ?bucket=/?run= never moves it.
func TestJudgeRecommendationBacklogTriageIgnoresFilters(t *testing.T) {
	rows, anchor := backlogFixture()
	svc := New(&fakeStore{judgeBacklogRows: rows}, newBox(t), testParams())
	owner := uuid.New()

	want := apitypes.TriageDTO{Total: 4, Todo: 1, Filed: 1, Done: 1, Dismissed: 1, FalsePositives: 1}
	for _, tc := range []struct {
		name   string
		bucket string
		run    uuid.UUID
	}{
		{"unfiltered", "all", uuid.Nil},
		{"bucket filter", "done", uuid.Nil},
		{"run anchor", "all", anchor},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := svc.JudgeRecommendationBacklog(context.Background(), owner, tc.bucket, tc.run)
			if err != nil {
				t.Fatalf("backlog: %v", err)
			}
			if got.Triage != want {
				t.Fatalf("triage = %+v, want %+v (the canonical tally must not follow the filter)", got.Triage, want)
			}
		})
	}
}
