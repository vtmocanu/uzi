package workersvc

import (
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Bootstrap of a legacy ledger row (ADR-1199) and the legacy refusal of the ledger-writing
// creates, against a real Postgres. Skipped without UZI_TEST_DATABASE_URL.

// insertLegacyLedger writes a row the way a pre-ADR-1199 binary did: only the scalar columns,
// so comment_marks stays NULL.
func insertLegacyLedger(t *testing.T, env codexTestEnv, repoID uuid.UUID, ref string, highWater int64, pending []int64) {
	t.Helper()
	if pending == nil {
		pending = []int64{} // the column is NOT NULL
	}
	env.exec(`INSERT INTO mr_rework_ledger (repo_id, ref, attempt_count, high_water, pending_unknown_ids) VALUES ($1, $2, 2, $3, $4)`,
		repoID, ref, highWater, pending)
}

func readLedger(t *testing.T, env codexTestEnv, repoID uuid.UUID, ref string) store.GetMRReworkLedgerRow {
	t.Helper()
	row, err := env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: ref})
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return row
}

func countReworkRuns(t *testing.T, env codexTestEnv, repoID uuid.UUID, mrIID int64) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM runs WHERE kind = 'mr_rework' AND repo_id = $1 AND mr_iid = $2`, repoID, mrIID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// bootstrapListing is the issue's scenario: a scalar of 5.58e9 spanning two id spaces.
func bootstrapListing() []forge.MRComment {
	return []forge.MRComment{
		{ID: 5_580_000_000, IDNamespace: nsIssue},
		{ID: 3_960_000_000, IDNamespace: nsReview},
		{ID: 7, IDNamespace: nsIssue},
		{ID: 7, IDNamespace: nsReview},
	}
}

func TestBootstrapReviewLedgerAppliesOnceLiveDB(t *testing.T) {
	env, svc, _, repoID := branchSerializeEnv(t)
	const ref = "agent/issue-119901"
	insertLegacyLedger(t, env, repoID, ref, 5_580_000_000, []int64{7, 8})
	pre := readLedger(t, env, repoID, ref)
	if !pre.Legacy {
		t.Fatalf("precondition: a raw scalar-only row must read as legacy, got %+v", pre)
	}

	st, err := svc.BootstrapReviewLedger(env.ctx, repoID, ref, pre, bootstrapListing())
	if err != nil {
		t.Fatalf("BootstrapReviewLedger: %v", err)
	}
	wantPending := []CommentKey{key(nsIssue, 7), key(nsReview, 7)} // 8 matches no comment
	if st.Marks[nsIssue] != 5_580_000_000 || st.Marks[nsReview] != 3_960_000_000 || !slices.Equal(st.Pending, wantPending) {
		t.Fatalf("returned state = %+v", st)
	}
	row := readLedger(t, env, repoID, ref)
	if row.Legacy {
		t.Fatal("the row is still legacy after the bootstrap")
	}
	stored, err := LedgerStateFromRow(row)
	if err != nil || !reflect.DeepEqual(stored, st) {
		t.Fatalf("stored state = %+v, %v, want the returned %+v", stored, err, st)
	}
	if row.AttemptCount != 2 || row.HighWater != 5_580_000_000 {
		t.Fatalf("bootstrap must not touch the cycle counter or legacy mark: %+v", row)
	}

	// A second bootstrap from the same pre row is a lost race: refused, row unchanged.
	if _, err := svc.BootstrapReviewLedger(env.ctx, repoID, ref, pre, bootstrapListing()); !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("second bootstrap = %v, want ErrReworkLedgerChanged", err)
	}
	if after := readLedger(t, env, repoID, ref); !reflect.DeepEqual(after, row) {
		t.Fatalf("a refused bootstrap changed the row:\n before %+v\n after  %+v", row, after)
	}
	// A non-legacy row is simply decoded.
	if st2, err := svc.BootstrapReviewLedger(env.ctx, repoID, ref, row, nil); err != nil || !reflect.DeepEqual(st2, st) {
		t.Fatalf("bootstrap of a non-legacy row = %+v, %v", st2, err)
	}
}

func TestBootstrapReviewLedgerOverflowLeavesRowUntouchedLiveDB(t *testing.T) {
	env, svc, _, repoID := branchSerializeEnv(t)
	const ref = "agent/issue-119902"
	ids := make([]int64, 0, ReviewPendingCap/2+1)
	var comments []forge.MRComment
	for i := int64(1); i <= ReviewPendingCap/2+1; i++ {
		ids = append(ids, i)
		comments = append(comments, forge.MRComment{ID: i, IDNamespace: nsIssue}, forge.MRComment{ID: i, IDNamespace: nsReview})
	}
	insertLegacyLedger(t, env, repoID, ref, 0, ids)
	pre := readLedger(t, env, repoID, ref)

	_, err := svc.BootstrapReviewLedger(env.ctx, repoID, ref, pre, comments)
	if !errors.Is(err, ErrReviewLedgerBootstrapOverflow) {
		t.Fatalf("err = %v, want ErrReviewLedgerBootstrapOverflow", err)
	}
	if after := readLedger(t, env, repoID, ref); !reflect.DeepEqual(after, pre) {
		t.Fatalf("an overflowing bootstrap changed the row:\n before %+v\n after  %+v", pre, after)
	}
}

func TestBootstrapReviewLedgerExactCapSucceedsLiveDB(t *testing.T) {
	env, svc, _, repoID := branchSerializeEnv(t)
	const ref = "agent/issue-119903"
	ids := make([]int64, 0, ReviewPendingCap/2)
	var comments []forge.MRComment
	for i := int64(1); i <= ReviewPendingCap/2; i++ {
		ids = append(ids, i)
		comments = append(comments, forge.MRComment{ID: i, IDNamespace: nsIssue}, forge.MRComment{ID: i, IDNamespace: nsReview})
	}
	insertLegacyLedger(t, env, repoID, ref, 0, ids)
	st, err := svc.BootstrapReviewLedger(env.ctx, repoID, ref, readLedger(t, env, repoID, ref), comments)
	if err != nil || len(st.Pending) != ReviewPendingCap {
		t.Fatalf("5000 ids in two namespaces: %d keys, %v, want exactly the cap", len(st.Pending), err)
	}
	row := readLedger(t, env, repoID, ref)
	if got, err := LedgerStateFromRow(row); err != nil || len(got.Pending) != ReviewPendingCap || row.Legacy {
		t.Fatalf("stored pending = %d, legacy=%t, %v", len(got.Pending), row.Legacy, err)
	}
}

// Auto and manual creates refuse a legacy row: no run, and the row is not touched.
func TestCreateOnLegacyLedgerRowRefusesLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 119904
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 1199040)
	insertLegacyLedger(t, env, repoID, branch, 50, nil)
	before := readLedger(t, env, repoID, branch)

	res := reviewResultOf(sampleReviewSnapshot())
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 1199040, source, "t", "d", res, 5); !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("auto create on a legacy row = %v, want ErrReworkLedgerChanged", err)
	}
	// Manual with guidance (no freshness recheck) must refuse too.
	if _, err := svc.CreateManualMRReworkRun(env.ctx, userID, repoID, branch, 1199040, source, "t", "d", res.Snapshot, res.PlanAssessed(), nil); !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("manual create on a legacy row = %v, want ErrReworkLedgerChanged", err)
	}
	if got := countReworkRuns(t, env, repoID, 1199040); got != 0 {
		t.Fatalf("%d mr_rework run(s) created on a legacy row, want 0", got)
	}
	if after := readLedger(t, env, repoID, branch); !reflect.DeepEqual(after, before) {
		t.Fatalf("a refused create changed the legacy row:\n before %+v\n after  %+v", before, after)
	}

	// After the bootstrap the same create succeeds.
	if _, err := svc.BootstrapReviewLedger(env.ctx, repoID, branch, before, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 1199040, source, "t", "d", res, 5); err != nil {
		t.Fatalf("auto create after the bootstrap = %v", err)
	}
}

// (e) A fresh MR with no ledger row: the first auto create succeeds and writes a non-legacy row
// carrying a mark per namespace.
func TestFirstAutoCreateWritesNamespacedMarksLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 119905
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 1199050)
	res := &ReviewSnapshotResult{
		Snapshot: sampleReviewSnapshot(),
		eligibleActionable: []CommentKey{
			key(nsIssue, 5_580_000_000), key(nsReview, 3_960_000_001),
		},
		class:      map[CommentKey]reviewClass{},
		inSnapshot: map[CommentKey]bool{},
	}
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 1199050, source, "t", "d", res, 5); err != nil {
		t.Fatalf("first create with no ledger row = %v", err)
	}
	row := readLedger(t, env, repoID, branch)
	st, err := LedgerStateFromRow(row)
	if row.Legacy || err != nil || st.Marks[nsIssue] != 5_580_000_000 || st.Marks[nsReview] != 3_960_000_001 {
		t.Fatalf("row = %+v (legacy=%t), state = %+v, %v", row, row.Legacy, st, err)
	}
	if row.AttemptCount != 1 || row.HighWater != 5_580_000_000 {
		t.Fatalf("row = %+v, want attempt_count 1 and the legacy high_water at the overall max", row)
	}
}

// (R3) Two callers read the same legacy row. The first bootstrap wins (here followed by an
// advance); the second must neither write nor create, and a fresh read plans correctly.
func TestConcurrentBootstrapAndAdvanceLoserWritesNothingLiveDB(t *testing.T) {
	env, svc, userID, repoID := branchSerializeEnv(t)
	const n int64 = 119906
	branch := agentIssueBranch(n)
	seedEligibleIssue(t, env, repoID, n)
	source := seedCompletedSourceRun(t, env, userID, repoID, n, 1199060)
	insertLegacyLedger(t, env, repoID, branch, 5_580_000_000, nil)
	preA := readLedger(t, env, repoID, branch)
	preB := readLedger(t, env, repoID, branch) // the loser's earlier, now stale, read

	listing := bootstrapListing()
	if _, err := svc.BootstrapReviewLedger(env.ctx, repoID, branch, preA, listing); err != nil {
		t.Fatalf("winner bootstrap: %v", err)
	}
	// The winner also advances: a review comment the bootstrap marks did not cover.
	fresh := assessedResultState(t, stateOf(map[string]int64{nsIssue: 5_580_000_000, nsReview: 3_960_000_000}),
		append(listing, rcNS(nsReview, 3_960_000_001, eligibleAuthor, "fresh inline"))...)
	if _, err := svc.CreateAutoMRReworkRunAndAdvance(env.ctx, userID, repoID, branch, 1199060, source, "t", "d", fresh, 5); err != nil {
		t.Fatalf("winner advance: %v", err)
	}
	winner := readLedger(t, env, repoID, branch)
	runs := countReworkRuns(t, env, repoID, 1199060)

	if _, err := svc.BootstrapReviewLedger(env.ctx, repoID, branch, preB, listing); !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("loser bootstrap = %v, want ErrReworkLedgerChanged", err)
	}
	if got := readLedger(t, env, repoID, branch); !reflect.DeepEqual(got, winner) {
		t.Fatalf("the loser changed the row:\n winner %+v\n after  %+v", winner, got)
	}
	if got := countReworkRuns(t, env, repoID, 1199060); got != runs {
		t.Fatalf("mr_rework runs went %d -> %d around the loser", runs, got)
	}

	// A fresh read plans correctly: the advanced comment is consumed, a later one is new.
	st, err := LedgerStateFromRow(readLedger(t, env, repoID, branch))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.HasNewAgainst(st) {
		t.Fatal("the advanced comment must be consumed on the fresh read")
	}
	later := assessedResultState(t, st, append(listing, rcNS(nsReview, 3_960_000_001, eligibleAuthor, "fresh inline"), rcNS(nsReview, 3_960_000_002, eligibleAuthor, "newer"))...)
	if !later.HasNewAgainst(st) {
		t.Fatal("a later review comment must be new on the fresh read")
	}
}

// A legacy-scalar change between the read and the bootstrap (an old binary still running) is a
// lost race too.
func TestBootstrapAfterLegacyScalarMovedIsRefusedLiveDB(t *testing.T) {
	env, svc, _, repoID := branchSerializeEnv(t)
	const ref = "agent/issue-119907"
	insertLegacyLedger(t, env, repoID, ref, 100, []int64{1})
	pre := readLedger(t, env, repoID, ref)
	env.exec(`UPDATE mr_rework_ledger SET high_water = 200 WHERE repo_id = $1 AND ref = $2`, repoID, ref)
	before := readLedger(t, env, repoID, ref)
	if _, err := svc.BootstrapReviewLedger(env.ctx, repoID, ref, pre, bootstrapListing()); !errors.Is(err, ErrReworkLedgerChanged) {
		t.Fatalf("bootstrap from a stale scalar = %v, want ErrReworkLedgerChanged", err)
	}
	if after := readLedger(t, env, repoID, ref); !reflect.DeepEqual(after, before) {
		t.Fatalf("row changed: %+v -> %+v", before, after)
	}
}

// Two real concurrent bootstraps from the same pre row: exactly one wins.
func TestConcurrentBootstrapExactlyOneWinsLiveDB(t *testing.T) {
	env, svc, _, repoID := branchSerializeEnv(t)
	const ref = "agent/issue-119908"
	insertLegacyLedger(t, env, repoID, ref, 5_580_000_000, []int64{7})
	pre := readLedger(t, env, repoID, ref)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = svc.BootstrapReviewLedger(env.ctx, repoID, ref, pre, bootstrapListing())
		}()
	}
	close(start)
	wg.Wait()
	wins, losses := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrReworkLedgerChanged):
			losses++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("wins=%d losses=%d (%v), want exactly one of each", wins, losses, errs)
	}
	if _, err := env.q.GetMRReworkLedger(env.ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: ref}); errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("ledger row missing")
	}
}
