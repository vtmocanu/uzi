package workersvc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Comment identity is (namespace, id) (ADR-1199). These tests pin the per-namespace invariant
// of the ledger seam: marks and pending keys never compare ids across namespaces.

const (
	nsIssue  = forge.CommentNSIssueComment
	nsReview = forge.CommentNSReviewComment
)

// rcNS is rc in an explicit namespace.
func rcNS(ns string, id, author int64, body string) forge.MRComment {
	c := rc(id, author, body)
	c.IDNamespace = ns
	return c
}

func key(ns string, id int64) CommentKey { return CommentKey{Namespace: ns, ID: id} }

func stateOf(marks map[string]int64, pending ...CommentKey) ReviewLedgerState {
	return ReviewLedgerState{Marks: marks, Pending: pending}
}

func TestCommentKeyRoundTripAndStrictParse(t *testing.T) {
	k := key(nsReview, 3_960_000_001)
	if got := k.String(); got != "review_comment:3960000001" {
		t.Fatalf("String = %q", got)
	}
	back, err := ParseCommentKey(k.String())
	if err != nil || back != k {
		t.Fatalf("ParseCommentKey = %+v, %v, want %+v", back, err, k)
	}
	// The key splits at the LAST colon, and the empty namespace is well formed (the ledger accepts it).
	if got, err := ParseCommentKey(":7"); err != nil || got != key("", 7) {
		t.Fatalf("empty namespace = %+v, %v", got, err)
	}
	if got, err := ParseCommentKey("0"); err == nil {
		t.Fatalf("no colon parsed as %+v", got)
	}
	for _, bad := range []string{"", "note", "note:", "note:007", "note:-1", "note:+1", "note: 1", "note:1x", "note:9223372036854775808", "Note:1", "no te:1", "a:b:1"} {
		if got, err := ParseCommentKey(bad); err == nil {
			t.Errorf("ParseCommentKey(%q) = %+v, want an error", bad, got)
		}
	}
	if _, err := ParseCommentKey("note:9223372036854775807"); err != nil {
		t.Errorf("max int64 id: %v", err)
	}
}

func TestLedgerStateFromRow(t *testing.T) {
	if _, err := LedgerStateFromRow(store.GetMRReworkLedgerRow{Legacy: true}); err == nil {
		t.Fatal("a legacy row must not decode to a state")
	}
	// The zero row (no ledger row) is the empty non-legacy state; so are null, empty and {} marks.
	for name, marks := range map[string][]byte{"nil": nil, "empty": {}, "object": []byte("{}"), "null": []byte("null")} {
		st, err := LedgerStateFromRow(store.GetMRReworkLedgerRow{CommentMarks: marks})
		if err != nil || len(st.Marks) != 0 || st.Marks == nil || st.Pending == nil || len(st.Pending) != 0 {
			t.Errorf("%s: state = %+v, %v, want empty non-nil maps/slices", name, st, err)
		}
	}
	st, err := LedgerStateFromRow(store.GetMRReworkLedgerRow{
		CommentMarks:       []byte(`{"issue_comment":5580000000,"review_comment":3960000000}`),
		PendingUnknownKeys: []string{"review_comment:7", "issue_comment:7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if st.Marks[nsIssue] != 5_580_000_000 || st.Marks[nsReview] != 3_960_000_000 {
		t.Fatalf("marks = %v", st.Marks)
	}
	if !slices.Equal(st.Pending, []CommentKey{key(nsReview, 7), key(nsIssue, 7)}) {
		t.Fatalf("pending = %v, want the stored order", st.Pending)
	}
	for _, bad := range []store.GetMRReworkLedgerRow{
		{CommentMarks: []byte(`[1]`)},
		{CommentMarks: []byte(`{"a":"1"}`)},
		{CommentMarks: []byte(`{"a":1.5}`)},
		{CommentMarks: []byte(`{"a":-1}`)},
		{CommentMarks: []byte(`{"a":01}`)},
		{PendingUnknownKeys: []string{"nocolon"}},
	} {
		if _, err := LedgerStateFromRow(bad); err == nil {
			t.Errorf("row %+v decoded, want an error", bad)
		}
	}
}

func TestEncodeNeverNull(t *testing.T) {
	if string(encodeMarks(nil)) != "{}" || string(encodeMarks(map[string]int64{})) != "{}" {
		t.Fatalf("empty marks must encode as {}")
	}
	if got := string(encodeMarks(map[string]int64{"note": 5})); got != `{"note":5}` {
		t.Fatalf("marks = %s", got)
	}
	if got := encodeKeys(nil); got == nil || len(got) != 0 {
		t.Fatalf("encodeKeys(nil) = %#v, want a non-nil empty slice", got)
	}
}

// (a) The reported bug: a consumed issue comment with a huge id must not hide a later,
// numerically lower review comment from another id space.
func TestHighIssueCommentMarkDoesNotHideLowerReviewComment(t *testing.T) {
	state := stateOf(map[string]int64{nsIssue: 5_580_000_000})
	res := assessedResultState(t, state,
		rcNS(nsIssue, 5_580_000_000, eligibleAuthor, "already consumed"),
		rcNS(nsReview, 3_960_000_001, eligibleAuthor, "fresh inline finding"),
	)
	if !res.HasNewAgainst(state) {
		t.Fatal("review_comment 3960000001 is new: its own namespace has no mark")
	}
	plan := res.Plan(state)
	if !plan.HasNew || plan.MaxActionable[nsReview] != 3_960_000_001 || plan.MaxActionable[nsIssue] != 5_580_000_000 {
		t.Fatalf("plan = %+v, want new with a mark per namespace", plan)
	}
	if plan.MaxActionableID != 5_580_000_000 {
		t.Fatalf("legacy MaxActionableID = %d, want the overall max", plan.MaxActionableID)
	}

	// The manual door admits it end to end.
	fs, svc, user, runID := reworkFixture(t, 0, nil)
	fs.mrReworkLedger = store.GetMRReworkLedgerRow{Ref: "agent/issue-7", AttemptCount: 5, CommentMarks: []byte(`{"issue_comment":5580000000}`)}
	run, err := svc.StartMRReworkForRun(context.Background(), user, runID, "", res)
	if err != nil || run.ID == uuid.Nil {
		t.Fatalf("StartMRReworkForRun = %v, %v, want the review comment admitted", run.ID, err)
	}
	marks, err := decodeMarks(fs.mrReworkAndAdvanceParams.Marks)
	if err != nil || marks[nsReview] != 3_960_000_001 || marks[nsIssue] != 5_580_000_000 {
		t.Fatalf("advance marks = %v, %v", marks, err)
	}
}

// (b) Equal numeric ids in two namespaces are two comments.
func TestEqualIDsInTwoNamespacesAreTrackedIndependently(t *testing.T) {
	// Consuming issue_comment:100 does not consume review_comment:100.
	state := stateOf(map[string]int64{nsIssue: 100})
	only := assessedResultState(t, state, rcNS(nsReview, 100, eligibleAuthor, "inline"))
	if !only.HasNewAgainst(state) || !only.Plan(state).HasNew {
		t.Fatal("review_comment:100 must be new when only issue_comment has a mark of 100")
	}
	both := assessedResultState(t, stateOf(map[string]int64{}),
		rcNS(nsIssue, 100, eligibleAuthor, "conversation"),
		rcNS(nsReview, 100, eligibleAuthor, "inline"),
	)
	plan := both.Plan(stateOf(map[string]int64{}))
	if plan.MaxActionable[nsIssue] != 100 || plan.MaxActionable[nsReview] != 100 {
		t.Fatalf("plan marks = %v, want 100 in both namespaces", plan.MaxActionable)
	}
	if both.HasNewAgainst(stateOf(plan.MaxActionable)) {
		t.Fatal("nothing is new once both namespaces are advanced")
	}

	// A pending review_comment:7 does not make issue_comment:7 new.
	pend := stateOf(map[string]int64{nsIssue: 50, nsReview: 50}, key(nsReview, 7))
	issueOnly := assessedResultState(t, pend, rcNS(nsIssue, 7, eligibleAuthor, "old conversation"))
	if issueOnly.HasNewAgainst(pend) {
		t.Fatal("issue_comment:7 is below its mark and not pending: not new")
	}
	reviewToo := assessedResultState(t, pend,
		rcNS(nsIssue, 7, eligibleAuthor, "old conversation"),
		rcNS(nsReview, 7, eligibleAuthor, "now eligible"),
	)
	if !reviewToo.HasNewAgainst(pend) {
		t.Fatal("pending review_comment:7 must be new")
	}
}

// (c) A pending unknown-author key resolves by its source-qualified identity, never by a
// colliding id in another namespace.
func TestPendingKeyResolvesBySourceQualifiedIdentity(t *testing.T) {
	// issue_comment:7 is pending for an author still unknown; an ELIGIBLE author posted
	// review_comment:7. A bare-id match would see the eligible comment and drop the pending key.
	state := stateOf(map[string]int64{nsIssue: 50, nsReview: 50}, key(nsIssue, 7))
	res := assessedResultState(t, state,
		rcNS(nsIssue, 7, unknownAuthor, "still unverified"),
		rcNS(nsReview, 7, eligibleAuthor, "eligible, below its mark"),
	)
	plan := res.Plan(state)
	if slices.Contains(plan.PendingRemove, key(nsIssue, 7)) {
		t.Fatalf("PendingRemove = %v: issue_comment:7 must stay while its author is unknown", plan.PendingRemove)
	}
	if !slices.Contains(plan.PendingAdd, key(nsIssue, 7)) {
		t.Fatalf("PendingAdd = %v, want the still-pending issue_comment:7 retained", plan.PendingAdd)
	}

	// The key whose own comment resolved eligible and was kept is the one consumed, and only it.
	state = stateOf(map[string]int64{nsIssue: 50, nsReview: 50}, key(nsReview, 7), key(nsIssue, 7))
	res = assessedResultState(t, state,
		rcNS(nsIssue, 7, unknownAuthor, "still unverified"),
		rcNS(nsReview, 7, eligibleAuthor, "now eligible"),
	)
	plan = res.Plan(state)
	if !slices.Equal(plan.PendingRemove, []CommentKey{key(nsReview, 7)}) {
		t.Fatalf("PendingRemove = %v, want only review_comment:7 consumed", plan.PendingRemove)
	}

	// A key whose comment is gone from ITS namespace is dropped even though another namespace
	// still has that numeric id.
	state = stateOf(map[string]int64{nsIssue: 50, nsReview: 50}, key(nsIssue, 7))
	res = assessedResultState(t, state, rcNS(nsReview, 7, unknownAuthor, "different comment, same number"))
	if plan = res.Plan(state); !slices.Contains(plan.PendingRemove, key(nsIssue, 7)) {
		t.Fatalf("PendingRemove = %v, want the vanished issue_comment:7 dropped", plan.PendingRemove)
	}
}

// (f) The wire snapshot carries bare ids, so the namespace of each kept comment is recovered by
// position. After the caps cut the head, every kept comment must still pair with its own key.
func TestSnapshotKeysPairWithKeptCommentsAfterTheCap(t *testing.T) {
	big := strings.Repeat("x", 4096)
	var comments []forge.MRComment
	for id := int64(1); id <= 6; id++ {
		// Equal ids in both namespaces; the body names the comment's true identity.
		comments = append(comments,
			rcNS(nsIssue, id, eligibleAuthor, nsIssue+":"+big[:4000]+string(rune('0'+id))),
			rcNS(nsReview, id, eligibleAuthor, nsReview+":"+big[:4000]+string(rune('0'+id))),
		)
		comments[len(comments)-2].CreatedAt = time.Unix(2*id, 0)
		comments[len(comments)-1].CreatedAt = time.Unix(2*id+1, 0)
	}
	res := assessedResultState(t, stateOf(map[string]int64{}), comments...)
	if !res.Snapshot.Truncated || len(res.Snapshot.Comments) == 0 || len(res.Snapshot.Comments) >= len(comments) {
		t.Fatalf("precondition: the byte cap must cut the head (kept %d of %d, truncated=%t)", len(res.Snapshot.Comments), len(comments), res.Snapshot.Truncated)
	}
	if len(res.eligibleActionable) != len(res.Snapshot.Comments) {
		t.Fatalf("%d keys for %d kept comments", len(res.eligibleActionable), len(res.Snapshot.Comments))
	}
	for i, c := range res.Snapshot.Comments {
		ns, _, _ := strings.Cut(c.Body, ":")
		want := key(ns, c.ID)
		if got := res.eligibleActionable[i]; got != want {
			t.Fatalf("kept comment %d (%s:%d) paired with %v", i, ns, c.ID, got)
		}
		if !res.inSnapshot[want] {
			t.Fatalf("%v kept but not marked inSnapshot", want)
		}
	}
	// The cut comments are not in the snapshot: the same numeric id of the OTHER namespace may be.
	kept := map[CommentKey]bool{}
	for _, k := range res.eligibleActionable {
		kept[k] = true
	}
	if len(res.inSnapshot) != len(kept) {
		t.Fatalf("inSnapshot has %d entries for %d kept comments", len(res.inSnapshot), len(kept))
	}
}

func TestBootstrapLedgerScalarSpansNamespaces(t *testing.T) {
	const scalar = 5_580_000_000
	listing := []forge.MRComment{
		{ID: 5_580_000_000, IDNamespace: nsIssue},
		{ID: 3_960_000_000, IDNamespace: nsReview},
	}
	marks, pending := bootstrapLedger(scalar, nil, listing)
	if len(marks) != 2 || marks[nsIssue] != 5_580_000_000 || marks[nsReview] != 3_960_000_000 || len(pending) != 0 {
		t.Fatalf("marks = %v pending = %v", marks, pending)
	}

	// A review comment that arrives later (3_970_000_000) is new exactly once.
	state := stateOf(marks)
	later := append(slices.Clone(listing), rcNS(nsReview, 3_970_000_000, eligibleAuthor, "late inline"))
	res := assessedResultState(t, state, later...)
	if !res.HasNewAgainst(state) {
		t.Fatal("a review comment above its namespace's mark must be new after bootstrap")
	}
	advanced := stateOf(res.Plan(state).MaxActionable)
	if res.HasNewAgainst(advanced) {
		t.Fatal("after the advance the comment must not be new again")
	}

	// Already present at bootstrap: it is at or below the scalar, so it is consumed.
	marks, _ = bootstrapLedger(scalar, nil, later)
	if marks[nsReview] != 3_970_000_000 {
		t.Fatalf("marks = %v, want review_comment 3970000000 (present at bootstrap)", marks)
	}
	if res := assessedResultState(t, stateOf(marks), later...); res.HasNewAgainst(stateOf(marks)) {
		t.Fatal("a comment present at bootstrap and at or below the scalar stays consumed")
	}

	// Above the scalar stays new, whatever its namespace.
	above := append(slices.Clone(listing), rcNS(nsIssue, 5_580_000_001, eligibleAuthor, "newer conversation"), rcNS(nsReview, 5_590_000_000, eligibleAuthor, "newer inline"))
	marks, _ = bootstrapLedger(scalar, nil, above)
	if marks[nsIssue] != 5_580_000_000 || marks[nsReview] != 3_960_000_000 {
		t.Fatalf("marks = %v: ids above the scalar must not be consumed", marks)
	}
	if res := assessedResultState(t, stateOf(marks), above...); !res.HasNewAgainst(stateOf(marks)) {
		t.Fatal("comments above the scalar must stay new")
	}
}

func TestBootstrapLedgerNamespaceWithNoCommentAtOrBelowScalarHasNoMark(t *testing.T) {
	marks, _ := bootstrapLedger(100, nil, []forge.MRComment{{ID: 500, IDNamespace: nsReview}, {ID: 40, IDNamespace: nsIssue}})
	if _, ok := marks[nsReview]; ok || marks[nsIssue] != 40 {
		t.Fatalf("marks = %v, want issue_comment 40 and no review_comment entry", marks)
	}
}

func TestBootstrapLedgerPendingIDsBecomeKeysPerMatchingNamespace(t *testing.T) {
	listing := []forge.MRComment{
		{ID: 7, IDNamespace: nsIssue},
		{ID: 7, IDNamespace: nsReview}, // ambiguous: the legacy id exists in two namespaces
		{ID: 9, IDNamespace: nsReview},
		{ID: 12, IDNamespace: nsIssue},
	}
	_, pending := bootstrapLedger(10, []int64{9, 7, 8, 12}, listing)
	// Legacy order, then namespace order; 8 matches no comment and is dropped.
	want := []CommentKey{key(nsReview, 9), key(nsIssue, 7), key(nsReview, 7), key(nsIssue, 12)}
	if !slices.Equal(pending, want) {
		t.Fatalf("pending = %v, want %v", pending, want)
	}
}

func TestBootstrapLedgerKeyBoundary(t *testing.T) {
	build := func(ids int) ([]int64, []forge.MRComment) {
		var legacy []int64
		var comments []forge.MRComment
		for i := 1; i <= ids; i++ {
			legacy = append(legacy, int64(i))
			comments = append(comments, forge.MRComment{ID: int64(i), IDNamespace: nsIssue}, forge.MRComment{ID: int64(i), IDNamespace: nsReview})
		}
		return legacy, comments
	}
	legacy, comments := build(ReviewPendingCap / 2)
	if _, pending := bootstrapLedger(0, legacy, comments); len(pending) != ReviewPendingCap {
		t.Fatalf("5000 ids in two namespaces = %d keys, want exactly the cap", len(pending))
	}
}

// Overflow is refused before any write: the nil transaction beginner would otherwise be the
// error, so reaching ErrReviewLedgerBootstrapOverflow proves the check precedes the store.
func TestBootstrapReviewLedgerOverflowRefusesBeforeAnyWrite(t *testing.T) {
	var legacy []int64
	var comments []forge.MRComment
	for i := 1; i <= ReviewPendingCap/2+1; i++ {
		legacy = append(legacy, int64(i))
		comments = append(comments, forge.MRComment{ID: int64(i), IDNamespace: nsIssue}, forge.MRComment{ID: int64(i), IDNamespace: nsReview})
	}
	fs := &fakeStore{}
	svc := New(fs, newBox(t), testParams())
	pre := store.GetMRReworkLedgerRow{Legacy: true, PendingUnknownIds: legacy}
	_, err := svc.BootstrapReviewLedger(context.Background(), uuid.New(), "agent/issue-7", pre, comments)
	if !errors.Is(err, ErrReviewLedgerBootstrapOverflow) {
		t.Fatalf("err = %v, want ErrReviewLedgerBootstrapOverflow", err)
	}
	var oe *ReviewLedgerBootstrapOverflowError
	if !errors.As(err, &oe) || oe.LegacyIDs != 5001 || oe.ExpandedKeys != 10002 {
		t.Fatalf("overflow error = %+v, want 5001 legacy ids expanding to 10002 keys", oe)
	}
	if err.Error() != ErrReviewLedgerBootstrapOverflow.Error() {
		t.Fatalf("typed error text %q differs from the sentinel", err.Error())
	}
}

func TestBootstrapReviewLedgerNonLegacyIsDecodedWithoutAWrite(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	st, err := svc.BootstrapReviewLedger(context.Background(), uuid.New(), "r", store.GetMRReworkLedgerRow{
		CommentMarks: []byte(`{"note":9}`), PendingUnknownKeys: []string{"note:3"},
	}, nil)
	if err != nil || st.Marks["note"] != 9 || !slices.Equal(st.Pending, []CommentKey{key("note", 3)}) {
		t.Fatalf("state = %+v, %v", st, err)
	}
}

// A ledger-writing create on a legacy row writes nothing and says so.
func TestCreateOnLegacyRowRefusesWithoutWriting(t *testing.T) {
	legacy := store.GetMRReworkLedgerRow{Legacy: true, HighWater: 100}
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{mrReworkLedger: legacy, mrReworkRunResult: store.Run{ID: uuid.New()}, runByIDPlain: reworkableSourceRun(runID, user, repo), repoRow: aValidRepoRow()}
	svc := New(fs, newBox(t), testParams())
	res := reviewResultOf(sampleReviewSnapshot())
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(context.Background(), user, repo, "agent/issue-7", 55, runID, "t", "d", res, 5); !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("auto on a legacy row = %v, want ErrReworkLedgerChanged", err)
	}
	if _, err := svc.CreateManualMRReworkRun(context.Background(), user, repo, "agent/issue-7", 55, runID, "t", "d", nil, ReviewPlan{}, nil); !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("manual on a legacy row = %v, want ErrReworkLedgerChanged", err)
	}
	if fs.mrReworkRunParams != nil || fs.mrReworkAndAdvanceParams != nil || len(fs.ledgerUpserts) != 0 {
		t.Fatal("a refused create wrote a run or a ledger row")
	}
	// The unadvanced create writes no ledger, so it does not care.
	if _, err := svc.CreateAutoMRReworkRun(context.Background(), user, repo, "agent/issue-7", 55, runID, "t", "d", sampleReviewSnapshot()); err != nil {
		t.Fatalf("unadvanced create on a legacy row: %v", err)
	}
}

// An upsert that matches no row (the row turned legacy after the in-transaction read) fails the
// create instead of leaving a run behind an unadvanced ledger.
func TestAutoAdvanceZeroRowUpsertIsLedgerChanged(t *testing.T) {
	user, repo, runID := uuid.New(), uuid.New(), uuid.New()
	fs := &fakeStore{ledgerUpsertZeroRows: true, mrReworkRunResult: store.Run{ID: uuid.New()}, runByIDPlain: reworkableSourceRun(runID, user, repo), repoRow: aValidRepoRow()}
	svc := New(fs, newBox(t), testParams())
	_, err := svc.CreateAutoMRReworkRunAndAdvance(context.Background(), user, repo, "agent/issue-7", 55, runID, "t", "d", reviewResultOf(sampleReviewSnapshot()), 5)
	if !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("err = %v, want ErrReworkLedgerChanged", err)
	}
}

func TestLedgerStateIsNew(t *testing.T) {
	st := stateOf(map[string]int64{nsIssue: 100}, key(nsReview, 7))
	for _, tc := range []struct {
		c    forge.MRComment
		want bool
	}{
		{forge.MRComment{ID: 100, IDNamespace: nsIssue}, false},
		{forge.MRComment{ID: 101, IDNamespace: nsIssue}, true},
		{forge.MRComment{ID: 100, IDNamespace: nsReview}, true}, // no mark in its own namespace
		{forge.MRComment{ID: 7, IDNamespace: nsReview}, true},   // pending
		{forge.MRComment{ID: 7, IDNamespace: nsIssue}, false},   // same number, not pending, below its mark
	} {
		if got := st.IsNew(tc.c); got != tc.want {
			t.Errorf("IsNew(%v) = %t, want %t", KeyOf(tc.c), got, tc.want)
		}
	}
}
