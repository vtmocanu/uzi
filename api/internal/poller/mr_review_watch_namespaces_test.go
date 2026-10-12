package poller

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Per-namespace comment identity in the MR review watcher (ADR-1199): a comment is new against
// its own namespace's mark, never against another namespace's.

// mrwNSComment is an eligible actionable inline comment in the given id namespace.
func mrwNSComment(ns string, id int64, created time.Time) forge.MRComment {
	c := mrwComment(id, created, mrwHeadSHA)
	c.IDNamespace = ns
	return c
}

// A GitHub-like MR whose issue comments run in the billions while its inline review comments run
// in the low billions: a scalar high-water at the issue-comment id would hide every new inline
// comment. The ledger has consumed issue_comment 5_580_000_000; a new review_comment
// 3_960_000_001 must still start a rework.
func TestMRReworkNewCommentInAnotherNamespaceFiresDespiteHigherConsumedID(t *testing.T) {
	const issueMark, newReview = int64(5_580_000_000), int64(3_960_000_001)
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers: map[string]store.GetMRReworkLedgerRow{
			mrwRef: mrwRow(mrwRef, 1, issueMark, mrwWithMarks(map[string]int64{forge.CommentNSIssueComment: issueMark})),
		},
	}
	runs := &mrwRuns{}
	f := landedForge(
		mrwNSComment(forge.CommentNSIssueComment, issueMark, landed()),
		mrwNSComment(forge.CommentNSReviewComment, newReview, landed()),
	)

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d, want the new inline comment to fire a rework", len(runs.calls))
	}
	got := st.ledgers[mrwRef]
	if got.AttemptCount != 2 {
		t.Fatalf("attempt_count = %d, want 2", got.AttemptCount)
	}
	want := map[string]int64{forge.CommentNSIssueComment: issueMark, forge.CommentNSReviewComment: newReview}
	if m := mrwMarks(t, got); !reflect.DeepEqual(m, want) {
		t.Fatalf("marks = %v, want %v: each namespace advances on its own", m, want)
	}
	// The consumed issue comment alone never re-fires.
	f2 := landedForge(mrwNSComment(forge.CommentNSIssueComment, issueMark, landed()), mrwNSComment(forge.CommentNSReviewComment, newReview, landed()))
	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f2)
	if len(runs.calls) != 1 {
		t.Fatalf("runs = %d after a repeat tick, want still 1", len(runs.calls))
	}
}

func TestMRReworkBootstrapIsGivenTheRowAndTheListing(t *testing.T) {
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.GetMRReworkLedgerRow{mrwRef: mrwRow(mrwRef, 1, 100)},
	}
	runs := &mrwRuns{}
	f := landedForge(mrwComment(100, landed(), mrwHeadSHA), mrwComment(140, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.bootstraps) != 1 || runs.bootstraps[0].comments != 2 || runs.bootstraps[0].pre.HighWater != 100 {
		t.Fatalf("bootstrap calls = %+v, want one with the ledger row read before the listing and both comments", runs.bootstraps)
	}
}

func TestMRReworkFailedListingNeverBootstraps(t *testing.T) {
	legacy := store.GetMRReworkLedgerRow{RepoID: mrwRepoID, Ref: mrwRef, AttemptCount: 1, HighWater: 100, Legacy: true}
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.GetMRReworkLedgerRow{mrwRef: legacy},
	}
	runs := &mrwRuns{}
	f := landedForge()
	f.commentsErr = errors.New("forge unavailable")

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.bootstraps) != 0 || len(runs.calls) != 0 || !reflect.DeepEqual(st.ledgers[mrwRef], legacy) {
		t.Fatalf("a failed listing must leave the row legacy: bootstraps=%d runs=%d ledger=%+v", len(runs.bootstraps), len(runs.calls), st.ledgers[mrwRef])
	}
}

func TestMRReworkBootstrapOverflowWarnsOncePerTickAndFiresNothing(t *testing.T) {
	logs := captureLogs(t, slog.LevelDebug)
	legacy := store.GetMRReworkLedgerRow{RepoID: mrwRepoID, Ref: mrwRef, AttemptCount: 1, HighWater: 100, Legacy: true, PendingUnknownIds: []int64{1, 2, 3}}
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.GetMRReworkLedgerRow{mrwRef: legacy},
	}
	runs := &mrwRuns{bootstrapErr: &workersvc.ReviewLedgerBootstrapOverflowError{LegacyIDs: 5001, ExpandedKeys: 10002}}
	f := landedForge(mrwComment(140, landed(), mrwHeadSHA))
	d := newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5})

	d.detect(context.Background(), mrwRepoRow(), f)
	d.detect(context.Background(), mrwRepoRow(), f)

	var warns []map[string]any
	for _, rec := range logRecords(t, logs) {
		if rec["reason"] == "bootstrap_overflow" {
			warns = append(warns, rec)
		}
	}
	if len(warns) != 2 {
		t.Fatalf("bootstrap_overflow records = %d over two ticks, want exactly one per tick; logs: %s", len(warns), logs.String())
	}
	w := warns[0]
	if w["level"] != "WARN" || w["ref"] != mrwRef || w["repo"] != mrwRepoRow().PathWithNamespace ||
		w["legacy_pending"] != float64(5001) || w["expanded_keys"] != float64(10002) {
		t.Fatalf("overflow record = %v", w)
	}
	if strings.Contains(logs.String(), "please tighten this") {
		t.Fatalf("a comment body reached the log: %s", logs.String())
	}
	if len(runs.calls) != 0 || len(st.upserts) != 0 || !reflect.DeepEqual(st.ledgers[mrwRef], legacy) {
		t.Fatalf("an overflow must create nothing and write nothing: runs=%d upserts=%d ledger=%+v", len(runs.calls), len(st.upserts), st.ledgers[mrwRef])
	}
}

func TestMRReworkBootstrapLedgerChangedIsSilentAndOtherErrorsWarn(t *testing.T) {
	legacy := store.GetMRReworkLedgerRow{RepoID: mrwRepoID, Ref: mrwRef, HighWater: 100, Legacy: true}
	for _, tc := range []struct {
		name     string
		err      error
		wantWarn bool
	}{
		{"ledger changed", workersvc.ErrReworkLedgerChanged, false},
		{"other error", errors.New("boom"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t, slog.LevelInfo)
			st := &mrwStore{
				candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
				ledgers:    map[string]store.GetMRReworkLedgerRow{mrwRef: legacy},
			}
			runs := &mrwRuns{bootstrapErr: tc.err}
			f := landedForge(mrwComment(140, landed(), mrwHeadSHA))

			newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

			if len(runs.calls) != 0 || !reflect.DeepEqual(st.ledgers[mrwRef], legacy) {
				t.Fatalf("runs=%d ledger=%+v, want no run and no write", len(runs.calls), st.ledgers[mrwRef])
			}
			if warned := strings.Contains(logs.String(), `"level":"WARN"`); warned != tc.wantWarn || strings.Contains(logs.String(), `"level":"ERROR"`) {
				t.Fatalf("warn=%v (want %v) logs: %s", warned, tc.wantWarn, logs.String())
			}
		})
	}
}

func TestMRReworkCreateLedgerChangedIsSwallowed(t *testing.T) {
	logs := captureLogs(t, slog.LevelInfo)
	row := mrwRow(mrwRef, 1, 100)
	st := &mrwStore{
		candidates: []store.ListMRReworkCandidatesRow{mrwCand("success")},
		ledgers:    map[string]store.GetMRReworkLedgerRow{mrwRef: row},
	}
	runs := &mrwRuns{err: workersvc.ErrReworkLedgerChanged}
	f := landedForge(mrwComment(140, landed(), mrwHeadSHA))

	newMRW(st, runs, nil, mrwSettings{enabled: true, capVal: 5}).detect(context.Background(), mrwRepoRow(), f)

	if len(runs.calls) != 1 || len(st.upserts) != 0 || !reflect.DeepEqual(st.ledgers[mrwRef], row) {
		t.Fatalf("runs=%d upserts=%d ledger=%+v", len(runs.calls), len(st.upserts), st.ledgers[mrwRef])
	}
	if strings.Contains(logs.String(), `"level":"ERROR"`) || strings.Contains(logs.String(), `"level":"WARN"`) {
		t.Fatalf("a ledger-changed refusal must be silent: %s", logs.String())
	}
}
