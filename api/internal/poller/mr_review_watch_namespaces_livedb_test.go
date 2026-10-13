package poller

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Per-namespace review ledger (ADR-1199) against a real Postgres and the real workersvc create
// and bootstrap: the namespaced regression, the legacy-row upgrade, and the races around it.

const (
	nsIssueMark = int64(5_580_000_000) // a GitHub issue-comment id
	nsReviewOld = int64(3_960_000_000) // a GitHub review-comment id, lower than any issue-comment id
)

var nsOld = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// seedMarks writes a non-legacy ledger as earlier consumed cycles left it.
func (l *mrwLive) seedMarks(t *testing.T, attempts int, highWater int64, marks string) {
	t.Helper()
	if _, err := l.q.UpsertMRReworkLedger(l.ctx, store.UpsertMRReworkLedgerParams{
		RepoID: l.repo, Ref: mrwRef, HighWater: highWater, Marks: []byte(marks),
		PendingAdd: []string{}, PendingRemove: []string{}, PendingSuperseded: []string{}, PendingSupersededBy: []string{},
	}); err != nil {
		t.Fatal(err)
	}
	l.exec(t, "UPDATE mr_rework_ledger SET attempt_count=$3 WHERE repo_id=$1 AND ref=$2", l.repo, mrwRef, attempts)
}

// legacyRow inserts a ledger row exactly as the pre-ADR-1199 binary wrote it: scalar columns
// only, comment_marks left NULL.
func (l *mrwLive) legacyRow(t *testing.T, attempts int, highWater int64, pendingSQL string) {
	t.Helper()
	l.exec(t, "INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, pending_unknown_ids) VALUES ($1,$2,$3,$4,"+pendingSQL+")",
		l.repo, mrwRef, attempts, highWater)
}

// finishReworks completes every rework run so the next cycle may start (one active rework per MR).
func (l *mrwLive) finishReworks(t *testing.T) {
	t.Helper()
	l.exec(t, "UPDATE runs SET status='completed' WHERE kind='mr_rework' AND repo_id=$1", l.repo)
}

func (l *mrwLive) marks(t *testing.T) map[string]int64 {
	t.Helper()
	return mrwMarks(t, l.ledger(t))
}

func TestMRReworkNamespacedRegressionLiveDB(t *testing.T) {
	l := newMRWLive(t)
	l.seedMarks(t, 1, nsIssueMark, fmt.Sprintf(`{"issue_comment":%d}`, nsIssueMark))
	f := landedForge(
		mrwNSComment(forge.CommentNSIssueComment, nsIssueMark, nsOld),
		mrwNSComment(forge.CommentNSReviewComment, 3_960_000_001, nsOld),
	)

	l.detect(5, f)

	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d, want 1: a new inline comment must fire although a higher issue-comment id was consumed", n)
	}
	got := l.ledger(t)
	want := map[string]int64{"issue_comment": nsIssueMark, "review_comment": 3_960_000_001}
	if got.AttemptCount != 2 || got.Legacy || !reflect.DeepEqual(l.marks(t), want) {
		t.Fatalf("ledger = %+v marks=%v, want attempt_count 2, non-legacy, marks %v", got, l.marks(t), want)
	}
	l.finishReworks(t)
	l.detect(5, f)
	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d after a repeat tick, want still 1", n)
	}
}

// An automatic cycle and an on-demand cycle both see the same new inline comment: exactly one
// rework runs and the ledger advances each namespace once.
func TestMRReworkNamespacedAutoAndManualRaceLiveDB(t *testing.T) {
	l := newMRWLive(t)
	l.seedMarks(t, 1, nsIssueMark, fmt.Sprintf(`{"issue_comment":%d,"review_comment":%d}`, nsIssueMark, nsReviewOld))
	f := landedForge(
		mrwNSComment(forge.CommentNSIssueComment, nsIssueMark, nsOld),
		mrwNSComment(forge.CommentNSReviewComment, nsReviewOld+1, nsOld),
	)
	f.onList = func() {
		f.onList = nil
		snap := &workersvc.ReviewCommentsSnapshot{Comments: []workersvc.ReviewCommentSnapshot{{ID: nsReviewOld + 1, AuthorUsername: "human", Body: "please tighten this", ReviewState: "inline"}}}
		plan := workersvc.ReviewPlan{MaxActionableID: nsReviewOld + 1, MaxActionable: map[string]int64{forge.CommentNSReviewComment: nsReviewOld + 1}}
		if _, err := l.svc.CreateManualMRReworkRun(l.ctx, l.owner, l.repo, mrwRef, mrwMrIID, l.source, "t", "d", snap, plan, nil); err != nil {
			t.Error(err)
			return
		}
		l.finishReworks(t)
	}

	l.detect(5, f)

	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d, want exactly the manual one", n)
	}
	got := l.ledger(t)
	want := map[string]int64{"issue_comment": nsIssueMark, "review_comment": nsReviewOld + 1}
	if got.AttemptCount != 1 || got.Legacy || !reflect.DeepEqual(l.marks(t), want) {
		t.Fatalf("ledger = %+v marks=%v, want attempt_count 1 (manual does not count) and marks %v", got, l.marks(t), want)
	}
}

func TestMRReworkLegacyLedgerBootstrapsThenFiresOnceLiveDB(t *testing.T) {
	l := newMRWLive(t)
	l.legacyRow(t, 1, nsIssueMark, "'{}'::bigint[]")
	base := []forge.MRComment{
		mrwNSComment(forge.CommentNSIssueComment, nsIssueMark, nsOld),
		mrwNSComment(forge.CommentNSReviewComment, nsReviewOld, nsOld),
	}
	f := landedForge(base...)

	// Tick 1: the first complete listing bootstraps the row; nothing in it is new.
	l.detect(5, f)
	if n := l.reworkRuns(t); n != 0 {
		t.Fatalf("mr_rework runs = %d after the bootstrap tick, want 0", n)
	}
	got := l.ledger(t)
	want := map[string]int64{"issue_comment": nsIssueMark, "review_comment": nsReviewOld}
	if got.Legacy || got.CommentMarks == nil || got.AttemptCount != 1 || !reflect.DeepEqual(l.marks(t), want) {
		t.Fatalf("ledger = %+v marks=%v, want a non-legacy row with marks %v and attempt_count unchanged", got, l.marks(t), want)
	}

	// Tick 2: a new inline comment, past the quiet period and on the current head.
	f.comments = append(append([]forge.MRComment{}, base...), mrwNSComment(forge.CommentNSReviewComment, 3_970_000_000, nsOld))
	l.detect(5, f)
	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d after the new comment, want exactly 1", n)
	}
	if got := l.ledger(t); got.AttemptCount != 2 || l.marks(t)["review_comment"] != 3_970_000_000 {
		t.Fatalf("ledger = %+v marks=%v, want attempt_count 2 and review mark 3970000000", got, l.marks(t))
	}

	// Tick 3: nothing new.
	l.finishReworks(t)
	l.detect(5, f)
	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d after a repeat tick, want still 1", n)
	}
}

func TestMRReworkLegacyLedgerStaysLegacyOnListingErrorLiveDB(t *testing.T) {
	l := newMRWLive(t)
	l.legacyRow(t, 1, nsIssueMark, "'{}'::bigint[]")
	f := landedForge()
	f.commentsErr = fmt.Errorf("forge unavailable")

	l.detect(5, f)

	got := l.ledger(t)
	if !got.Legacy || got.CommentMarks != nil || got.HighWater != nsIssueMark || l.reworkRuns(t) != 0 {
		t.Fatalf("ledger = %+v runs=%d, want the row untouched and still legacy, no run", got, l.reworkRuns(t))
	}
}

func TestMRReworkLegacyBootstrapOverflowWarnsOncePerTickLiveDB(t *testing.T) {
	logs := captureLogs(t, slog.LevelDebug)
	l := newMRWLive(t)
	// 5001 pending ids, each present in two namespaces: 10002 keys > the 10000 cap.
	l.legacyRow(t, 1, 0, "ARRAY(SELECT generate_series(1,5001))::bigint[]")
	var comments []forge.MRComment
	for id := int64(1); id <= 5001; id++ {
		comments = append(comments,
			mrwNSComment(forge.CommentNSIssueComment, id, nsOld),
			mrwNSComment(forge.CommentNSReviewComment, id, nsOld))
	}
	f := landedForge(comments...)
	before := l.ledger(t)

	l.detect(5, f)
	l.detect(5, f)

	var warns int
	for _, rec := range logRecords(t, logs) {
		if rec["reason"] == "bootstrap_overflow" {
			warns++
			if rec["level"] != "WARN" || rec["ref"] != mrwRef || rec["legacy_pending"] != float64(5001) || rec["expanded_keys"] != float64(10002) {
				t.Fatalf("overflow record = %v", rec)
			}
		}
	}
	if warns != 2 {
		t.Fatalf("bootstrap_overflow warnings = %d over two ticks, want exactly one per tick", warns)
	}
	if strings.Contains(logs.String(), "please tighten this") {
		t.Fatal("a comment body reached the log")
	}
	if after := l.ledger(t); !reflect.DeepEqual(after, before) || !after.Legacy || l.reworkRuns(t) != 0 {
		t.Fatalf("row changed or a run started: before=%+v after=%+v runs=%d", before, after, l.reworkRuns(t))
	}
}

// R3: a concurrent writer moves the legacy scalars between the watcher's ledger read and its
// bootstrap. The bootstrap writes nothing and no run starts; the next tick, from a fresh read,
// bootstraps and fires exactly once.
func TestMRReworkBootstrapRecoversFromAConcurrentLedgerChangeLiveDB(t *testing.T) {
	logs := captureLogs(t, slog.LevelInfo)
	l := newMRWLive(t)
	l.legacyRow(t, 1, nsIssueMark, "'{}'::bigint[]")
	fresh := mrwNSComment(forge.CommentNSIssueComment, nsIssueMark+100, nsOld)
	f := landedForge(
		mrwNSComment(forge.CommentNSIssueComment, nsIssueMark, nsOld),
		mrwNSComment(forge.CommentNSReviewComment, nsReviewOld, nsOld),
		fresh,
	)
	f.onList = func() {
		f.onList = nil
		// An old-binary replica advances the legacy mark after our read.
		l.exec(t, "UPDATE mr_rework_ledger SET high_water=$3 WHERE repo_id=$1 AND ref=$2", l.repo, mrwRef, nsIssueMark+50)
	}

	l.detect(5, f)

	got := l.ledger(t)
	if !got.Legacy || got.CommentMarks != nil || got.HighWater != nsIssueMark+50 || l.reworkRuns(t) != 0 {
		t.Fatalf("ledger = %+v runs=%d, want the concurrent writer's row untouched, still legacy, no run", got, l.reworkRuns(t))
	}
	if strings.Contains(logs.String(), `"level":"WARN"`) || strings.Contains(logs.String(), `"level":"ERROR"`) {
		t.Fatalf("a changed-under-us bootstrap must be silent: %s", logs.String())
	}

	l.detect(5, f)
	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d after the stable tick, want exactly 1", n)
	}
	got = l.ledger(t)
	if got.Legacy || got.AttemptCount != 2 || l.marks(t)["issue_comment"] != nsIssueMark+100 {
		t.Fatalf("ledger = %+v marks=%v, want non-legacy, attempt_count 2, issue mark %d", got, l.marks(t), nsIssueMark+100)
	}

	l.finishReworks(t)
	l.detect(5, f)
	if n := l.reworkRuns(t); n != 1 {
		t.Fatalf("mr_rework runs = %d after a repeat tick, want still 1", n)
	}
}
