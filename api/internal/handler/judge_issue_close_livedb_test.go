package handler

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/forgesvc"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Live-DB suite for the PRD #98 M6 Filed→Done sync. The PRD says the TEST is what gates
// this milestone, and it has to be live-DB: every guarantee is a property of the SQL edge
// predicate plus the ON CONFLICT DO NOTHING insert, and a fake store that replayed the
// poller's snapshot as transition events would be testing the model rather than the
// mechanism — the exact failure the edge marker exists to prevent.
//
// It lives in package handler rather than forgesvc only because the store-IT runner sweeps
// ./internal/store/... and ./internal/handler/... for the LiveDB suffix; e2e/ belongs to
// PRD #97 until that merges, so the runner cannot be extended yet. The subject under test
// is forgesvc.Service.SyncFiledIssueCloses, constructed here against the real pool exactly
// as the handler's own live tests already construct a forgesvc.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.

// closeSyncFixture is one user's judged run with one recommendation, filed as an issue into
// a repo. Everything the sync needs, with every knob the tests vary exposed.
type closeSyncFixture struct {
	userID   uuid.UUID
	repoID   uuid.UUID
	reviewID uuid.UUID
	filedID  uuid.UUID
	// recID is the seeded recommendation's own id. It exists so an assertion can scope to
	// THIS fixture's row rather than to its target string — see the self-improve backlog
	// check in TestFiledIssueCloseAutoDonesOnceLiveDB.
	recID    uuid.UUID
	category string
	target   string
	issueIID int64
}

func closeSyncLiveDB(t *testing.T) (*forgesvc.Service, *pgxpool.Pool, *store.Queries) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	q := store.New(pool)
	// No forge client and no label config: this pass never talks to a forge — it reads the
	// issue cache the sync already wrote. A nil LabelConfig is tolerated by New.
	return forgesvc.New(q, newHandlerTestBox(t), time.Second, nil), pool, q
}

// seedCloseSync builds the fixture. issueState seeds the CACHED issue state; filedSettled
// controls whether the link is settled (filed_at set); sameRepo=false files the link into a
// SECOND repo while the closed issue stays in the first, which is the cross-repo iid case.
func seedCloseSync(ctx context.Context, t *testing.T, pool *pgxpool.Pool, category, issueState string, filedSettled bool) closeSyncFixture {
	t.Helper()
	f := closeSyncFixture{
		userID: uuid.New(), repoID: uuid.New(), reviewID: uuid.New(), filedID: uuid.New(),
		recID:    uuid.New(),
		category: category, target: "rg", issueIID: 4242,
	}
	connID, runID := uuid.New(), uuid.New()
	mustExecT(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		f.userID, fmt.Sprintf("closesync-%s@e2e", f.userID))
	mustExecT(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, f.userID, []byte{0x1})
	mustExecT(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, f.repoID, connID)
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'completed')`, runID, f.userID, f.repoID)
	mustExecT(ctx, t, pool,
		`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
		f.reviewID, runID, f.userID)
	mustExecT(ctx, t, pool,
		`INSERT INTO review_recommendations (id, review_id, category, target, rationale_md)
		 VALUES ($1, $2, $3, $4, 'because')`, f.recID, f.reviewID, f.category, f.target)
	// The cached issue — what the poller's sync would have written.
	mustExecT(ctx, t, pool,
		`INSERT INTO issues (repo_id, forge_issue_iid, title, state, web_url, forge_updated_at, synced_at)
		 VALUES ($1, $2, 'filed', $3, 'https://forge.e2e/i/1', now(), now())`,
		f.repoID, f.issueIID, issueState)
	filedAt := "now()"
	if !filedSettled {
		filedAt = "NULL" // an in-flight #68 claim: a row exists, but it is not filed
	}
	mustExecT(ctx, t, pool, fmt.Sprintf(
		`INSERT INTO recommendation_filed_issues
		     (id, review_id, category, target, filed_repo_id, filed_issue_iid, filed_issue_url, filed_at)
		 VALUES ($1, $2, $3, $4, $5, $6, 'https://forge.e2e/i/1', %s)`, filedAt),
		f.filedID, f.reviewID, f.category, f.target, f.repoID, f.issueIID)
	return f
}

// disposition reads the coordinate's row, or reports absence.
func disposition(ctx context.Context, t *testing.T, pool *pgxpool.Pool, f closeSyncFixture) (status, reason, setVia string, setByUser *uuid.UUID, setAt time.Time, found bool) {
	t.Helper()
	var reasonN, setViaN *string
	err := pool.QueryRow(ctx,
		`SELECT status, dismiss_reason, set_via, set_by_user_id, set_at
		   FROM recommendation_dispositions
		  WHERE review_id = $1 AND category = $2 AND target = $3`,
		f.reviewID, f.category, f.target).Scan(&status, &reasonN, &setViaN, &setByUser, &setAt)
	if err == pgx.ErrNoRows {
		return "", "", "", nil, time.Time{}, false
	}
	if err != nil {
		t.Fatalf("read disposition: %v", err)
	}
	if reasonN != nil {
		reason = *reasonN
	}
	if setViaN != nil {
		setVia = *setViaN
	}
	return status, reason, setVia, setByUser, setAt, true
}

func closeSyncedAt(ctx context.Context, t *testing.T, pool *pgxpool.Pool, filedID uuid.UUID) *time.Time {
	t.Helper()
	var ts *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT close_synced_at FROM recommendation_filed_issues WHERE id = $1`, filedID).Scan(&ts); err != nil {
		t.Fatalf("read close_synced_at: %v", err)
	}
	return ts
}

// ---- 1. fires exactly once, with system provenance --------------------------------

// TestFiledIssueCloseAutoDonesOnceLiveDB: a closed filed issue moves its recommendation to
// Done on the FIRST tick — stamped set_via='issue_close' and set_by_user_id NULL so an
// automatic Done is visibly distinct from a hand-marked one — and subsequent ticks are
// no-ops because the edge has been consumed. Also proves the improve_uzi coordinate drops
// out of the self-improvement backlog (#94 Decision 9), which is the PRD's stated
// downstream effect.
func TestFiledIssueCloseAutoDonesOnceLiveDB(t *testing.T) {
	svc, pool, q := closeSyncLiveDB(t)
	ctx := context.Background()
	f := seedCloseSync(ctx, t, pool, "improve_uzi", "closed", true)

	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	status, reason, setVia, setBy, firstSetAt, found := disposition(ctx, t, pool, f)
	if !found {
		t.Fatal("the close did not produce a disposition")
	}
	if status != "done" || reason != "" {
		t.Fatalf("disposition = %q/%q, want done with no reason", status, reason)
	}
	if setVia != "issue_close" {
		t.Fatalf("set_via = %q, want issue_close (an auto-done must be distinguishable)", setVia)
	}
	if setBy != nil {
		t.Fatalf("set_by_user_id = %v, want NULL — the system did this, and the FILER may be "+
			"a different user than the review owner (#68 Decision 8)", *setBy)
	}
	stamp := closeSyncedAt(ctx, t, pool, f.filedID)
	if stamp == nil {
		t.Fatal("close_synced_at was not stamped, so the edge was never consumed")
	}

	// Second tick: nothing changes. If the pass were level-triggered this would re-fire.
	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	_, _, _, _, secondSetAt, _ := disposition(ctx, t, pool, f)
	if !secondSetAt.Equal(firstSetAt) {
		t.Fatalf("set_at moved on the second tick (%v → %v): the pass is re-firing, not edge-triggered",
			firstSetAt, secondSetAt)
	}

	// The improve_uzi coordinate is out of the self-improvement backlog (#94 Decision 9 —
	// row-existence in recommendation_dispositions is the exclusion, so the auto-done
	// keeps the engine from folding an already-handled item into its tracking issue).
	//
	// SCOPED TO THIS FIXTURE'S ROW ID, and that is load-bearing rather than tidiness.
	// ListOpenImproveUziRecommendations selects `WHERE rr.category = 'improve_uzi'` across the
	// WHOLE TABLE — no user scope, no review scope — and the LiveDB packages share one
	// database, so anything any other test leaves behind comes back in this result. This
	// assertion used to filter on `r.Target == f.target`, and `target` is not unique: every
	// seedCloseSync fixture uses "rg", and so did an unrelated M4 badge fixture, which failed
	// this test on its first baseline run for reasons entirely unrelated to issue-close sync.
	// The row's own id is the only value here that cannot collide, which is why the fixture
	// carries recID at all.
	//
	// (The query genuinely has no owner column to scope by — that is PRD #94/#68's design,
	// not an oversight here — so the id is the fix available to the TEST. Narrowing the query
	// itself would change behaviour the self-improve engine depends on.)
	// A DECOY that makes the scoping assertion below discriminating instead of merely
	// correct: a second, unrelated, still-open improve_uzi recommendation on the SAME target,
	// under a different review. It is what any future fixture elsewhere in the suite would
	// look like, seeded deliberately so the hazard is present on EVERY run rather than on the
	// unlucky ones.
	//
	// It is built by hand rather than by seedCloseSync, and the difference is the whole
	// exclusion rule: seedCloseSync always writes a recommendation_filed_issues row, and
	// PRD #68 Decision 12 makes the ROW'S EXISTENCE the backlog exclusion (not filed_at) — so
	// a seedCloseSync decoy is never in the backlog and cannot collide. The precondition
	// below caught exactly that on the first attempt.
	decoyRunID, decoyReviewID, decoyRecID := uuid.New(), uuid.New(), uuid.New()
	mustExecT(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, 99, 't', 'd', 'completed')`, decoyRunID, f.userID, f.repoID)
	mustExecT(ctx, t, pool,
		`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
		decoyReviewID, decoyRunID, f.userID)
	mustExecT(ctx, t, pool,
		`INSERT INTO review_recommendations (id, review_id, category, target, rationale_md)
		 VALUES ($1, $2, 'improve_uzi', $3, 'decoy')`, decoyRecID, decoyReviewID, f.target)

	after, err := q.ListOpenImproveUziRecommendations(ctx, 50)
	if err != nil {
		t.Fatalf("backlog after: %v", err)
	}
	// Precondition, or the assertion below proves nothing: the decoy must actually be IN the
	// unscoped result, sharing this fixture's target. If it is not, the collision this test
	// defends against cannot occur and the scoping is untested.
	var decoyPresent bool
	for _, r := range after {
		if r.ID == decoyRecID {
			decoyPresent = true
			if r.Target != f.target {
				t.Fatalf("fixture broken: decoy target %q != %q, so it cannot collide", r.Target, f.target)
			}
		}
	}
	if !decoyPresent {
		t.Fatal("fixture broken: the decoy improve_uzi row is not in the unscoped backlog, " +
			"so a target-matching assertion would pass here and this test proves nothing")
	}
	for _, r := range after {
		if r.ID == f.recID {
			t.Fatalf("an auto-done improve_uzi recommendation is still in the self-improve backlog: %+v", r)
		}
	}
}

// ---- 2. Undo sticks ----------------------------------------------------------------

// TestFiledIssueCloseUndoSticksLiveDB is the subtle one the PRD singles out. After the
// auto-done, the user hits Undo (#94's disposition delete). The issue is STILL closed in
// the cache, so a level-triggered pass would helpfully re-create the row on the very next
// tick and the user could never undo it. Because the edge was consumed, it must not.
func TestFiledIssueCloseUndoSticksLiveDB(t *testing.T) {
	svc, pool, _ := closeSyncLiveDB(t)
	ctx := context.Background()
	f := seedCloseSync(ctx, t, pool, "improve_agent", "closed", true)

	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if _, _, _, _, _, found := disposition(ctx, t, pool, f); !found {
		t.Fatal("fixture: the auto-done did not land")
	}

	// The user undoes it.
	mustExecT(ctx, t, pool,
		`DELETE FROM recommendation_dispositions WHERE review_id = $1 AND category = $2 AND target = $3`,
		f.reviewID, f.category, f.target)

	// Several more ticks, with the issue still closed.
	for i := 0; i < 3; i++ {
		if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
	}
	if _, _, _, _, _, found := disposition(ctx, t, pool, f); found {
		t.Fatal("the Undo did NOT stick — the sync re-applied a disposition the user removed. " +
			"The edge must be consumed by close_synced_at, not re-derived from the closed state.")
	}
}

// ---- 3. a human verdict is never overwritten ----------------------------------------

// TestFiledIssueCloseDoesNotOverwriteDismissedLiveDB: the user has already DISMISSED the
// coordinate when the filed issue closes. The insert is ON CONFLICT DO NOTHING, so their
// verdict — status, reason, and the NULL set_via that marks it as human — survives intact.
// #94's DO-UPDATE upsert would have replaced it with a system `done`.
func TestFiledIssueCloseDoesNotOverwriteDismissedLiveDB(t *testing.T) {
	svc, pool, _ := closeSyncLiveDB(t)
	ctx := context.Background()
	f := seedCloseSync(ctx, t, pool, "improve_agent", "closed", true)

	mustExecT(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, $3, 'dismissed', 'not_an_issue', 'hash', $4)`,
		f.reviewID, f.category, f.target, f.userID)

	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	status, reason, setVia, setBy, _, found := disposition(ctx, t, pool, f)
	if !found {
		t.Fatal("the user's disposition vanished")
	}
	if status != "dismissed" || reason != "not_an_issue" {
		t.Fatalf("the sync OVERWROTE a human verdict: got %q/%q, want dismissed/not_an_issue", status, reason)
	}
	if setVia != "" {
		t.Fatalf("set_via = %q, want empty — this row is still the human's", setVia)
	}
	if setBy == nil || *setBy != f.userID {
		t.Fatalf("set_by_user_id = %v, want the user who dismissed it", setBy)
	}
	// The edge is still consumed, so the pass does not re-examine this row forever.
	if closeSyncedAt(ctx, t, pool, f.filedID) == nil {
		t.Error("close_synced_at must be stamped even when the insert wrote nothing")
	}
}

// ---- 4. a reopen does not re-open ----------------------------------------------------

// TestFiledIssueCloseReopenDoesNotReopenLiveDB: after the auto-done, the issue is REOPENED
// and later re-closed. Neither event touches the disposition — there is no auto-reopen (a
// flapping issue must not ping-pong a user's backlog) and the consumed edge makes the
// re-close a no-op.
func TestFiledIssueCloseReopenDoesNotReopenLiveDB(t *testing.T) {
	svc, pool, _ := closeSyncLiveDB(t)
	ctx := context.Background()
	f := seedCloseSync(ctx, t, pool, "enable_tool", "closed", true)

	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	_, _, _, _, setAt, found := disposition(ctx, t, pool, f)
	if !found {
		t.Fatal("fixture: the auto-done did not land")
	}

	// Reopen, as a later sync would have cached it.
	mustExecT(ctx, t, pool, `UPDATE issues SET state = 'opened' WHERE repo_id = $1 AND forge_issue_iid = $2`,
		f.repoID, f.issueIID)
	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("tick after reopen: %v", err)
	}
	if _, _, _, _, _, stillThere := disposition(ctx, t, pool, f); !stillThere {
		t.Fatal("a reopen must NOT auto-reopen the recommendation (no un-done on reopen)")
	}

	// Re-close: the edge is already consumed, so nothing re-fires.
	mustExecT(ctx, t, pool, `UPDATE issues SET state = 'closed' WHERE repo_id = $1 AND forge_issue_iid = $2`,
		f.repoID, f.issueIID)
	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("tick after re-close: %v", err)
	}
	_, _, _, _, finalSetAt, _ := disposition(ctx, t, pool, f)
	if !finalSetAt.Equal(setAt) {
		t.Fatalf("a re-close re-fired the sync (set_at %v → %v); close_synced_at must make it a no-op",
			setAt, finalSetAt)
	}
}

// ---- 5. the coordinate join is (repo_id, iid), never iid alone -----------------------

// TestFiledIssueCloseIsRepoScopedLiveDB is PF-3. A forge issue iid is PER-PROJECT: `issues`
// is keyed (repo_id, forge_issue_iid). If the sync joined on iid alone, closing issue #4242
// in some OTHER repo would auto-Done a recommendation filed as #4242 into this one —
// cross-repo, and since reviews are owner-scoped while filed rows are not, possibly
// cross-USER. Here the OTHER repo's #4242 is closed while this repo's is open; nothing may
// fire.
func TestFiledIssueCloseIsRepoScopedLiveDB(t *testing.T) {
	svc, pool, _ := closeSyncLiveDB(t)
	ctx := context.Background()
	// This user's filed issue is still OPEN.
	f := seedCloseSync(ctx, t, pool, "add_agent", "opened", true)

	// A different user's different repo has a CLOSED issue with the SAME iid.
	other := seedCloseSync(ctx, t, pool, "add_agent", "closed", true)
	if other.issueIID != f.issueIID {
		t.Fatalf("fixture: the two repos must reuse the same iid (%d vs %d)", other.issueIID, f.issueIID)
	}

	if err := svc.SyncFiledIssueCloses(ctx, f.repoID); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, _, _, _, _, found := disposition(ctx, t, pool, f); found {
		t.Fatal("a CLOSED issue with the same iid in ANOTHER repo auto-Doned this recommendation — " +
			"the join must be on (repo_id, forge_issue_iid), never iid alone")
	}
	if closeSyncedAt(ctx, t, pool, f.filedID) != nil {
		t.Error("no edge occurred, so close_synced_at must remain NULL")
	}
	// Positive control: the OTHER repo's own closed issue does fire, so the negative above
	// is repo scoping and not a broken fixture.
	if err := svc.SyncFiledIssueCloses(ctx, other.repoID); err != nil {
		t.Fatalf("tick (other repo): %v", err)
	}
	if _, _, _, _, _, found := disposition(ctx, t, pool, other); !found {
		t.Fatal("positive control failed: the other repo's own closed issue should have auto-Doned")
	}
}

// ---- 6. rows that must never fire ----------------------------------------------------

// TestFiledIssueCloseSkipsUnsettledAndOrphanedLiveDB covers the two documented no-ops:
// an in-flight #68 claim (filed_at NULL) is not a filed issue, and a link whose repo was
// disabled/disconnected (filed_repo_id NULL, ON DELETE SET NULL) is unobservable. The
// second is what makes the PRD's "won't auto-Done" limitation SAFE rather than merely
// silent — a NULL-repo row can never be matched against some other repo's issue.
func TestFiledIssueCloseSkipsUnsettledAndOrphanedLiveDB(t *testing.T) {
	svc, pool, _ := closeSyncLiveDB(t)
	ctx := context.Background()

	// (a) unsettled claim, issue closed.
	claim := seedCloseSync(ctx, t, pool, "adjust_template", "closed", false)
	if err := svc.SyncFiledIssueCloses(ctx, claim.repoID); err != nil {
		t.Fatalf("tick (claim): %v", err)
	}
	if _, _, _, _, _, found := disposition(ctx, t, pool, claim); found {
		t.Fatal("an UNSETTLED claim (filed_at NULL) is not a filed issue and must not auto-Done")
	}

	// (b) settled, closed, but the repo link was severed.
	orphan := seedCloseSync(ctx, t, pool, "adjust_template", "closed", true)
	mustExecT(ctx, t, pool, `UPDATE recommendation_filed_issues SET filed_repo_id = NULL WHERE id = $1`, orphan.filedID)
	if err := svc.SyncFiledIssueCloses(ctx, orphan.repoID); err != nil {
		t.Fatalf("tick (orphan): %v", err)
	}
	if _, _, _, _, _, found := disposition(ctx, t, pool, orphan); found {
		t.Fatal("a link with filed_repo_id NULL is unobservable and must be a safe no-op")
	}
}
