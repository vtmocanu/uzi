package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The live-DB half of PRD #1184 M2. The admin cross-user Mark done's whole trust story lives in
// SQL a fake cannot defend: ListRecommendationsForCoordsAll has NO user predicate (so the fan-out
// reaches every owner), UpsertAdminDispositionsForResolvedCoords is ON CONFLICT DO NOTHING (so a
// human verdict is never overwritten) and stamps set_via='admin', and DeleteAdminDispositionsForCoords
// removes only set_via='admin' rows. A fake returns its fixture regardless of predicate, so each of
// those properties is only observable against real Postgres, on whether a specific row changed.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; ./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix. A package that prints `ok` with
// PASS=0 is INVALID, not green.

func adminDispoLiveDB(t *testing.T) (*Service, *store.Queries, *pgxpool.Pool) {
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
	svc := New(q, newBox(t), testParams())
	// Wire the tx beginner so AdminMarkDone can open its coord-lock transaction, exactly as
	// production does in api/cmd/server/main.go (wsvc.SetTxBeginner(pool)).
	svc.SetTxBeginner(pool)
	return svc, q, pool
}

// adminMustExec runs a seed statement, failing the test on error.
func adminMustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// adminSeedOwner creates a fresh owner with a forge connection + repo. Every id is a fresh uuid
// since the store-IT runner shares one DB across the whole suite.
func adminSeedOwner(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tag string) (userID, repoID uuid.UUID) {
	t.Helper()
	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	adminMustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("admindispo-%s-%s@e2e", tag, userID))
	adminMustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	adminMustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	return userID, repoID
}

// adminIID hands out unique issue_iid values so seeded runs never collide on the shared DB.
var adminIID = func() func() int64 {
	n := int64(100000)
	return func() int64 { n++; return n }
}()

// adminSeedJudgedRun creates a completed run + review + one recommendation per coordinate, and
// returns the review id (the disposition's key half). rationale is per (review, coordinate) so a
// stale-hash regression would be observable.
func adminSeedJudgedRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, coords ...[2]string) uuid.UUID {
	t.Helper()
	runID, reviewID := uuid.New(), uuid.New()
	adminMustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
		 VALUES ($1, $2, $3, $4, 'run', 'd', 'completed')`, runID, userID, repoID, adminIID())
	adminMustExec(ctx, t, pool,
		`INSERT INTO run_reviews (id, target_run_id, user_id, verdict) VALUES ($1, $2, $3, 'issues')`,
		reviewID, runID, userID)
	for _, c := range coords {
		adminMustExec(ctx, t, pool,
			`INSERT INTO review_recommendations (review_id, category, target, rationale_md) VALUES ($1, $2, $3, $4)`,
			reviewID, c[0], c[1], fmt.Sprintf("rationale for %s/%s in %s", c[0], c[1], reviewID))
	}
	return reviewID
}

// adminDispoRow reads back one coordinate's disposition on a specific review: status, set_via
// (nil when NULL), set_by_user_id (nil when NULL), and whether a row exists at all.
func adminDispoRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reviewID uuid.UUID, category, target string) (exists bool, status string, setVia *string, setBy *uuid.UUID) {
	t.Helper()
	err := pool.QueryRow(ctx,
		`SELECT status, set_via, set_by_user_id FROM recommendation_dispositions
		  WHERE review_id = $1 AND category = $2 AND target = $3`,
		reviewID, category, target).Scan(&status, &setVia, &setBy)
	if err != nil {
		return false, "", nil, nil
	}
	return true, status, setVia, setBy
}

// TestAdminMarkDoneFansOutOpenMembersLiveDB is (a) + (c): the admin done reaches every owner's
// OPEN member of the coordinate, skips a FILED member and an already-DISPOSED member, and stamps
// each written row set_via='admin' + set_by_user_id=<the admin>.
func TestAdminMarkDoneFansOutOpenMembersLiveDB(t *testing.T) {
	svc, _, pool := adminDispoLiveDB(t)
	ctx := context.Background()

	target := "rg-" + uuid.NewString()
	rg := [2]string{"install_worker_tool", target}

	admin, _ := adminSeedOwner(ctx, t, pool, "admin")
	ownerA, repoA := adminSeedOwner(ctx, t, pool, "a")
	ownerB, repoB := adminSeedOwner(ctx, t, pool, "b")

	// ownerA: two OPEN members.
	revA1 := adminSeedJudgedRun(ctx, t, pool, ownerA, repoA, rg)
	revA2 := adminSeedJudgedRun(ctx, t, pool, ownerA, repoA, rg)
	// ownerB: one OPEN member.
	revB1 := adminSeedJudgedRun(ctx, t, pool, ownerB, repoB, rg)
	// ownerA: a FILED member (settled filed link, no disposition) — must be SKIPPED (not todo).
	revFiled := adminSeedJudgedRun(ctx, t, pool, ownerA, repoA, rg)
	adminMustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues (review_id, category, target, filed_issue_iid, filed_issue_url, filed_at, filing_since)
		 VALUES ($1, $2, $3, 7, 'https://forge.e2e/i/7', now(), now())`, revFiled, rg[0], rg[1])
	// ownerB: an already-DISPOSED member (human dismissed) — must be SKIPPED, and survive.
	revDisposed := adminSeedJudgedRun(ctx, t, pool, ownerB, repoB, rg)
	adminMustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, $3, 'dismissed', 'not_an_issue', 'h', $4)`, revDisposed, rg[0], rg[1], ownerB)

	res, err := svc.AdminMarkDone(ctx, admin, []JudgeDispositionCoord{{Category: rg[0], Target: rg[1]}})
	if err != nil {
		t.Fatalf("AdminMarkDone: %v", err)
	}
	if res.Updated != 3 {
		t.Fatalf("updated = %d, want 3 (ownerA x2 + ownerB x1 OPEN members; filed and disposed skipped)", res.Updated)
	}

	// Each OPEN member now carries an admin done with the admin as setter.
	for _, rev := range []uuid.UUID{revA1, revA2, revB1} {
		exists, status, setVia, setBy := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1])
		if !exists {
			t.Fatalf("review %s: no disposition after admin done, want one", rev)
		}
		if status != "done" {
			t.Errorf("review %s: status = %q, want done", rev, status)
		}
		if setVia == nil || *setVia != "admin" {
			t.Errorf("review %s: set_via = %v, want 'admin' — the provenance the chip reads", rev, setVia)
		}
		if setBy == nil || *setBy != admin {
			t.Errorf("review %s: set_by_user_id = %v, want the acting admin %s (accountability in the row)", rev, setBy, admin)
		}
	}

	// The FILED member was skipped: no disposition row was written on it.
	if exists, status, _, _ := adminDispoRow(ctx, t, pool, revFiled, rg[0], rg[1]); exists {
		t.Errorf("the filed member got a %q disposition — an admin done must skip a filed member (not todo)", status)
	}
	// The already-DISPOSED member's human verdict survives untouched (DO NOTHING + the todo filter).
	exists, status, setVia, setBy := adminDispoRow(ctx, t, pool, revDisposed, rg[0], rg[1])
	if !exists || status != "dismissed" {
		t.Errorf("the human-dismissed member = exists %v status %q, want a surviving 'dismissed'", exists, status)
	}
	if setVia != nil {
		t.Errorf("the human-dismissed member's set_via = %v, want NULL — an admin done must not stamp a human's row", setVia)
	}
	if setBy == nil || *setBy != ownerB {
		t.Errorf("the human-dismissed member's set_by_user_id = %v, want ownerB (untouched)", setBy)
	}
}

// TestAdminMarkDoneDoNothingProtectsHumanVerdictLiveDB is (b) at the SQL layer: the todo filter in
// Go never hands the write a member that already carries a disposition, so DO NOTHING is only
// OBSERVABLE by calling the store query directly with a conflicting review — which is exactly the
// race the DO NOTHING is the durable backstop for. This is the test that fails if DO NOTHING is
// ever relaxed to DO UPDATE: a DO UPDATE would overwrite the human 'dismissed' to an admin 'done'.
func TestAdminMarkDoneDoNothingProtectsHumanVerdictLiveDB(t *testing.T) {
	_, q, pool := adminDispoLiveDB(t)
	ctx := context.Background()

	target := "conflict-" + uuid.NewString()
	rg := [2]string{"improve_uzi", target}

	admin, _ := adminSeedOwner(ctx, t, pool, "admin")
	owner, repo := adminSeedOwner(ctx, t, pool, "o")
	rev := adminSeedJudgedRun(ctx, t, pool, owner, repo, rg)

	// A human dismissal already stands on the coordinate (set_via NULL, set_by = owner).
	adminMustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, dismiss_reason, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, $3, 'dismissed', 'wont_do', 'h', $4)`, rev, rg[0], rg[1], owner)

	// Call the fan-out write DIRECTLY with this review in the arrays (bypassing the Go todo filter,
	// modelling the race). DO NOTHING must skip it: zero rows written, the human verdict intact.
	n, err := q.UpsertAdminDispositionsForResolvedCoords(ctx, store.UpsertAdminDispositionsForResolvedCoordsParams{
		AdminUserID:     pgconv.UUID(admin),
		ReviewIds:       []uuid.UUID{rev},
		Categories:      []string{rg[0]},
		Targets:         []string{rg[1]},
		RationaleHashes: []string{"newhash"},
	})
	if err != nil {
		t.Fatalf("UpsertAdminDispositionsForResolvedCoords: %v", err)
	}
	if n != 0 {
		t.Fatalf("execrows = %d, want 0 — ON CONFLICT DO NOTHING must not write over an existing verdict", n)
	}

	exists, status, setVia, setBy := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1])
	if !exists || status != "dismissed" {
		t.Fatalf("row = exists %v status %q, want the human 'dismissed' untouched — DO NOTHING was relaxed to DO UPDATE", exists, status)
	}
	if setVia != nil {
		t.Errorf("set_via = %v, want NULL — the admin write must not stamp its provenance over a human verdict", setVia)
	}
	if setBy == nil || *setBy != owner {
		t.Errorf("set_by_user_id = %v, want the owner %s (unchanged)", setBy, owner)
	}
}

// TestAdminMarkDoneFiledRecheckSkipsFiledCoordLiveDB is the twin of the DO-NOTHING test above, at
// the SQL layer, for the OTHER way a coordinate leaves `todo` between the resolve and the write: it
// gets FILED. Filing writes NO disposition (SettleRecommendationFiledIssue only stamps
// recommendation_filed_issues.filed_at), so ON CONFLICT DO NOTHING — which only guards a conflict
// on an existing disposition — would NOT skip a member that became filed after the Go `todo` filter
// resolved it, and an admin 'done' would land on a now-filed coordinate. The write's WHERE NOT
// EXISTS filed recheck is the backstop for that race. As with the DO-NOTHING test, the Go filter
// never hands the write a filed member, so this is observable only by calling the store query
// DIRECTLY with a filed-but-undisposed review in the arrays. This is the test that fails if the
// NOT EXISTS anti-join is ever removed: n == 1, a 'done'/'admin' row lands on the filed coordinate.
func TestAdminMarkDoneFiledRecheckSkipsFiledCoordLiveDB(t *testing.T) {
	_, q, pool := adminDispoLiveDB(t)
	ctx := context.Background()

	target := "filed-race-" + uuid.NewString()
	rg := [2]string{"install_worker_tool", target}

	admin, _ := adminSeedOwner(ctx, t, pool, "admin")
	owner, repo := adminSeedOwner(ctx, t, pool, "o")
	rev := adminSeedJudgedRun(ctx, t, pool, owner, repo, rg)

	// The coordinate is SETTLED-FILED (filed_at set) with NO disposition row — exactly the state a
	// filing produces, and the state DO NOTHING cannot see (there is no disposition to conflict on).
	adminMustExec(ctx, t, pool,
		`INSERT INTO recommendation_filed_issues (review_id, category, target, filed_issue_iid, filed_issue_url, filed_at, filing_since)
		 VALUES ($1, $2, $3, 9, 'https://forge.e2e/i/9', now(), now())`, rev, rg[0], rg[1])

	// Call the fan-out write DIRECTLY with this filed review in the arrays (bypassing the Go todo
	// filter, modelling the resolve→filed→write race). The NOT EXISTS filed recheck must skip it:
	// zero rows written, no disposition created on the filed coordinate.
	n, err := q.UpsertAdminDispositionsForResolvedCoords(ctx, store.UpsertAdminDispositionsForResolvedCoordsParams{
		AdminUserID:     pgconv.UUID(admin),
		ReviewIds:       []uuid.UUID{rev},
		Categories:      []string{rg[0]},
		Targets:         []string{rg[1]},
		RationaleHashes: []string{"newhash"},
	})
	if err != nil {
		t.Fatalf("UpsertAdminDispositionsForResolvedCoords: %v", err)
	}
	if n != 0 {
		t.Fatalf("execrows = %d, want 0 — WHERE NOT EXISTS must skip a filed coordinate (filing writes no disposition, so DO NOTHING alone would miss it)", n)
	}

	// No disposition landed on the filed coordinate: an admin 'done' must not overwrite a filing.
	if exists, status, setVia, _ := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1]); exists {
		t.Fatalf("row = exists %v status %q set_via %v, want NO disposition — the NOT EXISTS filed recheck was removed and an admin 'done' landed on a filed coordinate", exists, status, setVia)
	}
}

// TestAdminMarkDoneFiledRaceSerializedLiveDB is the REAL two-transaction race the advisory lock
// exists for (issue #1184 rework), the counterpart to the pure-SQL guard above. It proves the lock
// makes the admin cross-user Mark-done serialize behind a concurrent, in-flight filing settle on the
// same coordinate, so the admin write re-snapshots under READ COMMITTED and skips the now-filed
// coordinate.
//
// Fails WITHOUT the fix / passes WITH it: on unfixed code AdminMarkDone takes no lock, so it never
// blocks — it runs its INSERT while T1's settled filed row is still UNCOMMITTED (invisible to its
// snapshot), its NOT EXISTS passes, and an admin 'done' lands on a coordinate that is about to be
// filed (Updated==1, a 'done'/'admin' row). With the fix, AdminMarkDone takes the per-coordinate
// advisory lock as the tx's first statement, blocks behind T1 (which holds the same lock), and only
// after T1 commits does its next statement re-snapshot, see the committed filed row, and skip it
// (Updated==0, no disposition). T1 is held uncommitted for the whole poll window precisely so the
// unfixed write has its chance to do the damage before the lock is released.
func TestAdminMarkDoneFiledRaceSerializedLiveDB(t *testing.T) {
	svc, _, pool := adminDispoLiveDB(t)
	ctx := context.Background()

	target := "filed-live-race-" + uuid.NewString()
	rg := [2]string{"install_worker_tool", target}

	admin, _ := adminSeedOwner(ctx, t, pool, "admin")
	owner, repo := adminSeedOwner(ctx, t, pool, "o")
	rev := adminSeedJudgedRun(ctx, t, pool, owner, repo, rg) // one OPEN (todo) member

	// T1: acquire the coord lock and insert a SETTLED filed row, then HOLD the tx open (do not
	// commit). This models a filing settle mid-flight, exactly as settleFiledIssue does: lock first,
	// then stamp recommendation_filed_issues.filed_at. Its filed row stays invisible to any other
	// snapshot until commit.
	t1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin T1: %v", err)
	}
	defer func() { _ = t1.Rollback(ctx) }() // no-op after commit; safety net if an assert fails early
	if err := store.LockJudgeCoord(ctx, t1, rg[0], rg[1]); err != nil {
		t.Fatalf("T1 LockJudgeCoord: %v", err)
	}
	if _, err := t1.Exec(ctx,
		`INSERT INTO recommendation_filed_issues (review_id, category, target, filed_issue_iid, filed_issue_url, filed_at, filing_since)
		 VALUES ($1, $2, $3, 11, 'https://forge.e2e/i/11', now(), now())`, rev, rg[0], rg[1]); err != nil {
		t.Fatalf("T1 insert settled filed row: %v", err)
	}

	// Launch the admin Mark-done concurrently. Its resolve runs on the pool (outside T1's tx) under
	// READ COMMITTED, so it sees the coordinate as `todo` (T1's filed row is uncommitted) and reaches
	// the write — where, fixed, it must block on the lock T1 holds.
	type result struct {
		res apitypes.JudgeAdminDispositionResultDTO
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := svc.AdminMarkDone(ctx, admin, []JudgeDispositionCoord{{Category: rg[0], Target: rg[1]}})
		done <- result{res, err}
	}()

	// Poll pg_locks (on the pool, NOT T1) for a session WAITING on the advisory lock for this
	// coordinate. classid/objid for the two-int pg_advisory_xact_lock form are exposed as oid, so
	// compare against the UNSIGNED 32-bit values of the two int4 keys. Bounded: up to ~4s at ~50ms.
	// T1 stays uncommitted until this resolves (waiter seen OR timeout) — that is what lets the
	// unfixed write insert its 'done' before the lock is ever released.
	wantClassID := int64(uint32(store.JudgeDispositionCoordLockClass))  // a positive constant; classid is oid (unsigned)
	wantObjID := int64(uint32(store.JudgeCoordLockObjID(rg[0], rg[1]))) //nolint:gosec // objid is oid (unsigned): wraparound is fine
	waiterSeen := false
	for i := 0; i < 80; i++ {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_locks
			  WHERE locktype = 'advisory' AND NOT granted
			    AND classid::bigint = $1 AND objid::bigint = $2`,
			wantClassID, wantObjID).Scan(&n); err != nil {
			t.Fatalf("poll pg_locks: %v", err)
		}
		if n > 0 {
			waiterSeen = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Commit T1: the settled filed row becomes visible, and the advisory lock releases so a blocked
	// admin write unblocks and re-snapshots.
	if err := t1.Commit(ctx); err != nil {
		t.Fatalf("commit T1: %v", err)
	}

	got := <-done
	if got.err != nil {
		t.Fatalf("AdminMarkDone: %v", got.err)
	}
	// The core assertion: the admin write skipped the now-filed coordinate. On unfixed code it did
	// not block, ran while T1 was uncommitted, and wrote a 'done' (Updated==1).
	if got.res.Updated != 0 {
		t.Fatalf("Updated = %d, want 0 — the admin done must skip a coordinate a concurrent filing settled; without the coord lock it races in an admin 'done' (waiterSeen=%v)", got.res.Updated, waiterSeen)
	}
	// No 'done'/'admin' disposition landed on the coordinate.
	if exists, status, setVia, _ := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1]); exists {
		t.Fatalf("row = exists %v status %q set_via %v, want NO disposition — an admin 'done' raced past the concurrent filing (waiterSeen=%v)", exists, status, setVia, waiterSeen)
	}
	// The filed row is still settled (the filing won the race, as it must).
	var filedSettled bool
	if err := pool.QueryRow(ctx,
		`SELECT filed_at IS NOT NULL FROM recommendation_filed_issues
		  WHERE review_id = $1 AND category = $2 AND target = $3`,
		rev, rg[0], rg[1]).Scan(&filedSettled); err != nil {
		t.Fatalf("read filed row: %v", err)
	}
	if !filedSettled {
		t.Fatalf("filed row is not settled after the race, want it still settled")
	}
	// With the fix the waiter is observable; a timeout here (waiterSeen=false) on green code would
	// mean the write never blocked — surface it rather than passing silently on a lucky schedule.
	if !waiterSeen {
		t.Errorf("never observed the admin write WAITING on the coord advisory lock — the lock is not being taken before the write (this is the unfixed shape)")
	}
}

// TestAdminUndoRemovesOnlyAdminRowsLiveDB is (d): AdminUndoDone deletes the set_via='admin' rows
// across users but leaves a human 'done' on the SAME coordinate standing.
func TestAdminUndoRemovesOnlyAdminRowsLiveDB(t *testing.T) {
	svc, _, pool := adminDispoLiveDB(t)
	ctx := context.Background()

	target := "undo-" + uuid.NewString()
	rg := [2]string{"improve_agent", target}

	admin, _ := adminSeedOwner(ctx, t, pool, "admin")
	owner1, repo1 := adminSeedOwner(ctx, t, pool, "1")
	owner2, repo2 := adminSeedOwner(ctx, t, pool, "2")
	owner3, repo3 := adminSeedOwner(ctx, t, pool, "3")

	rev1 := adminSeedJudgedRun(ctx, t, pool, owner1, repo1, rg) // open
	rev2 := adminSeedJudgedRun(ctx, t, pool, owner2, repo2, rg) // open
	rev3 := adminSeedJudgedRun(ctx, t, pool, owner3, repo3, rg) // human done already
	adminMustExec(ctx, t, pool,
		`INSERT INTO recommendation_dispositions (review_id, category, target, status, rationale_hash, set_by_user_id)
		 VALUES ($1, $2, $3, 'done', 'h', $4)`, rev3, rg[0], rg[1], owner3)

	if res, err := svc.AdminMarkDone(ctx, admin, []JudgeDispositionCoord{{Category: rg[0], Target: rg[1]}}); err != nil {
		t.Fatalf("AdminMarkDone: %v", err)
	} else if res.Updated != 2 {
		t.Fatalf("mark-done updated = %d, want 2 (owner1 + owner2 open; owner3 already done)", res.Updated)
	}
	// Precondition: rev1/rev2 admin done, rev3 human done.
	for _, rev := range []uuid.UUID{rev1, rev2} {
		if _, _, setVia, _ := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1]); setVia == nil || *setVia != "admin" {
			t.Fatalf("precondition: review %s should carry an admin row, set_via=%v", rev, setVia)
		}
	}

	res, err := svc.AdminUndoDone(ctx, []JudgeDispositionCoord{{Category: rg[0], Target: rg[1]}})
	if err != nil {
		t.Fatalf("AdminUndoDone: %v", err)
	}
	if res.Updated != 2 {
		t.Fatalf("undo updated = %d, want 2 (the two admin rows removed; the human done is not touched)", res.Updated)
	}

	// The two admin rows are gone.
	for _, rev := range []uuid.UUID{rev1, rev2} {
		if exists, status, _, _ := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1]); exists {
			t.Errorf("review %s still has a %q disposition after undo — the admin row must be deleted", rev, status)
		}
	}
	// The human done SURVIVES: the undo is scoped to set_via='admin'.
	exists, status, setVia, setBy := adminDispoRow(ctx, t, pool, rev3, rg[0], rg[1])
	if !exists || status != "done" {
		t.Errorf("owner3's human done = exists %v status %q, want a surviving 'done' — the undo must leave human rows alone", exists, status)
	}
	if setVia != nil {
		t.Errorf("owner3's set_via = %v, want NULL (still a human row)", setVia)
	}
	if setBy == nil || *setBy != owner3 {
		t.Errorf("owner3's set_by_user_id = %v, want owner3", setBy)
	}
}

// TestOwnerWriteClearsAdminProvenanceLiveDB is (e): after an admin done stamps set_via='admin' on
// an owner's coordinate, the owner's OWN write (the existing single-coordinate upsert) clears
// set_via back to NULL — so the chip reads the owner's own verdict, never "Done by an admin". The
// owner keeps control of their own coordinate.
func TestOwnerWriteClearsAdminProvenanceLiveDB(t *testing.T) {
	svc, q, pool := adminDispoLiveDB(t)
	ctx := context.Background()

	target := "ownerclear-" + uuid.NewString()
	rg := [2]string{"improve_uzi", target}

	admin, _ := adminSeedOwner(ctx, t, pool, "admin")
	owner, repo := adminSeedOwner(ctx, t, pool, "o")
	rev := adminSeedJudgedRun(ctx, t, pool, owner, repo, rg)

	if res, err := svc.AdminMarkDone(ctx, admin, []JudgeDispositionCoord{{Category: rg[0], Target: rg[1]}}); err != nil {
		t.Fatalf("AdminMarkDone: %v", err)
	} else if res.Updated != 1 {
		t.Fatalf("mark-done updated = %d, want 1", res.Updated)
	}
	if _, _, setVia, _ := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1]); setVia == nil || *setVia != "admin" {
		t.Fatalf("precondition: set_via = %v, want 'admin' after the admin done", setVia)
	}

	// The owner overrides on their own coordinate via the existing human upsert. set_via must clear.
	if _, err := q.UpsertRecommendationDisposition(ctx, store.UpsertRecommendationDispositionParams{
		ReviewID:      rev,
		Category:      rg[0],
		Target:        rg[1],
		Status:        "dismissed",
		DismissReason: pgtype.Text{String: "not_an_issue", Valid: true},
		RationaleHash: "h",
		SetByUserID:   pgconv.UUID(owner),
	}); err != nil {
		t.Fatalf("owner UpsertRecommendationDisposition: %v", err)
	}

	exists, status, setVia, setBy := adminDispoRow(ctx, t, pool, rev, rg[0], rg[1])
	if !exists || status != "dismissed" {
		t.Fatalf("row = exists %v status %q, want the owner's 'dismissed'", exists, status)
	}
	if setVia != nil {
		t.Errorf("set_via = %v, want NULL — a human write clears the admin provenance, or the chip mis-labels the owner's own verdict", setVia)
	}
	if setBy == nil || *setBy != owner {
		t.Errorf("set_by_user_id = %v, want the owner %s", setBy, owner)
	}
}
