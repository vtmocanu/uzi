package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestJudgeCategoryStatsMatrixRollup pins PRD #270's chip-count contract at the service seam:
// the bucket→category matrix is built by running the SHARED GroupJudgeRecommendations rollup
// over the whole-backlog rows and tallying each group's rollup Bucket, NOT by reading member
// states. It asserts the three properties the PRD calls out:
//
//	(a) per category, todo+filed+done+dismissed == all;
//	(b) a group with ONE open member counts under `todo` only (never its members' settled
//	    rungs);
//	(c) a fully-settled group counts under its highest rung.
func TestJudgeCategoryStatsMatrixRollup(t *testing.T) {
	filed := time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC)

	// (install_worker_tool, rg): TWO members — one dismissed, one OPEN. The rollup is todo
	// because any open member wins, so property (b): it must count under todo and NOWHERE
	// else, even though it has a dismissed member.
	open := uuid.New()
	dismissedRun := uuid.New()
	// (improve_uzi, docs): a single DONE member → fully settled, rolls up done — property (c).
	doneRun := uuid.New()
	// (improve_uzi, api): a settled filed link → rolls up filed.
	filedRun := uuid.New()
	// (adjust_template, ci): a lone dismissed member → rolls up dismissed.
	dismRun := uuid.New()

	rows := rowsOf(
		backlogRow{runID: open, runTitle: "open run", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "issues", category: "install_worker_tool", target: "rg", rationale: "need rg"},
		backlogRow{runID: dismissedRun, runTitle: "old run", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "ok", category: "install_worker_tool", target: "rg", rationale: "needed rg then",
			disposition: "dismissed", reason: "not_an_issue"},
		backlogRow{runID: doneRun, runTitle: "done run", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "ok", category: "improve_uzi", target: "docs", disposition: "done"},
		backlogRow{runID: filedRun, runTitle: "filed run", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "issues", category: "improve_uzi", target: "api",
			filedIID: 11, filedURL: "https://forge/11", filedAt: filed},
		backlogRow{runID: dismRun, runTitle: "dism run", reviewID: uuid.New(), recID: uuid.New(),
			verdict: "issues", category: "adjust_template", target: "ci", disposition: "dismissed", reason: "wont_do"},
	)
	svc := New(backlogStoreFor(rows), newBox(t), testParams())

	dto, err := svc.JudgeCategoryStats(context.Background(), uuid.New(), uuid.Nil)
	if err != nil {
		t.Fatalf("JudgeCategoryStats: %v", err)
	}
	m := dto.CountsByBucket

	// All five bucket keys present and non-nil (an absent bucket must serialize as {} not null).
	for _, b := range []string{BucketTodo, BucketFiled, BucketDone, BucketDismissed, BucketAll} {
		if m[b] == nil {
			t.Fatalf("bucket %q missing from matrix: %+v", b, m)
		}
	}

	// (b) the two-member install_worker_tool/rg group rolls up todo ONLY — its dismissed
	// member must NOT leak into the dismissed tally.
	if m[BucketTodo]["install_worker_tool"] != 1 {
		t.Errorf("todo[install_worker_tool] = %d, want 1 (an open member makes the whole group todo)", m[BucketTodo]["install_worker_tool"])
	}
	if got := m[BucketDismissed]["install_worker_tool"]; got != 0 {
		t.Errorf("dismissed[install_worker_tool] = %d, want 0 — a group's members' settled rungs must not count when the group rolls up todo", got)
	}

	// (c) fully-settled groups count under their highest rung.
	if m[BucketDone]["improve_uzi"] != 1 {
		t.Errorf("done[improve_uzi] = %d, want 1 (a fully-settled done group)", m[BucketDone]["improve_uzi"])
	}
	if m[BucketFiled]["improve_uzi"] != 1 {
		t.Errorf("filed[improve_uzi] = %d, want 1 (a settled filed group)", m[BucketFiled]["improve_uzi"])
	}
	if m[BucketDismissed]["adjust_template"] != 1 {
		t.Errorf("dismissed[adjust_template] = %d, want 1", m[BucketDismissed]["adjust_template"])
	}

	// `all` folds every group in regardless of bucket.
	wantAll := map[string]int{"install_worker_tool": 1, "improve_uzi": 2, "adjust_template": 1}
	for cat, n := range wantAll {
		if m[BucketAll][cat] != n {
			t.Errorf("all[%q] = %d, want %d", cat, m[BucketAll][cat], n)
		}
	}

	// (a) the tab-partition invariant, per category.
	cats := map[string]bool{}
	for _, inner := range m {
		for cat := range inner {
			cats[cat] = true
		}
	}
	for cat := range cats {
		sum := m[BucketTodo][cat] + m[BucketFiled][cat] + m[BucketDone][cat] + m[BucketDismissed][cat]
		if sum != m[BucketAll][cat] {
			t.Errorf("category %q: todo+filed+done+dismissed = %d, want == all = %d", cat, sum, m[BucketAll][cat])
		}
	}
}

// TestJudgeCategoryStatsLoadsUncapped proves the whole-backlog load passes the UNCAPPED
// sentinel (Lim: 0) and never applies the category filter (facet independence, Decision 4):
// a capped load mis-rolls a group whose open member fell past the cut, and a category filter
// would make each chip count only itself.
func TestJudgeCategoryStatsLoadsUncapped(t *testing.T) {
	rows := rowsOf(backlogRow{runID: uuid.New(), runTitle: "r", reviewID: uuid.New(), recID: uuid.New(),
		verdict: "issues", category: "improve_uzi", target: "docs"})
	fs := backlogStoreFor(rows)
	svc := New(fs, newBox(t), testParams())

	if _, err := svc.JudgeCategoryStats(context.Background(), uuid.New(), uuid.Nil); err != nil {
		t.Fatalf("JudgeCategoryStats: %v", err)
	}
	if fs.backlogArg == nil {
		t.Fatal("the row load never ran")
	}
	if fs.backlogArg.Lim != 0 {
		t.Errorf("Lim = %d, want 0 (uncapped whole-backlog load)", fs.backlogArg.Lim)
	}
	if fs.backlogArg.Categories != nil {
		t.Errorf("Categories = %v, want nil — the chip counts must never apply the category filter", fs.backlogArg.Categories)
	}
	// No anchor → SQL NULL (no-op).
	if fs.backlogArg.RunAnchor.Valid {
		t.Errorf("run anchor = %+v, want an invalid (NULL) anchor when uuid.Nil is passed", fs.backlogArg.RunAnchor)
	}
}

// TestJudgeCategoryStatsThreadsAnchor: a non-nil runAnchor is pushed down as the query's
// owner-scoped anchor param (the ?run= deep-link), scoping the chips to that run's groups.
func TestJudgeCategoryStatsThreadsAnchor(t *testing.T) {
	fs := backlogStoreFor(nil)
	svc := New(fs, newBox(t), testParams())
	run := uuid.New()

	if _, err := svc.JudgeCategoryStats(context.Background(), uuid.New(), run); err != nil {
		t.Fatalf("JudgeCategoryStats: %v", err)
	}
	if fs.backlogArg == nil || !fs.backlogArg.RunAnchor.Valid || uuid.UUID(fs.backlogArg.RunAnchor.Bytes) != run {
		t.Fatalf("run anchor not pushed down: %+v", fs.backlogArg)
	}
}
