package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1867 M2: the live-DB proof of the run_salvage table (migration 00264) and its
// queries (queries/salvage.sql): the CHECKs, the RESTRICT live pointer, each state
// transition, and the sweep's candidate selection. A fake store cannot exhibit any of it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres;
// ./e2e/run-store-it.sh provides one and sweeps this package for the LiveDB suffix.

const salvageTip = "0123456789abcdef0123456789abcdef01234567"

type salvageFixture struct {
	ctx  context.Context
	t    *testing.T
	pool *pgxpool.Pool
	q    *store.Queries
	user uuid.UUID
	runs []uuid.UUID
	n    int64
}

func openSalvageLiveDB(t *testing.T) *salvageFixture {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &salvageFixture{ctx: ctx, t: t, pool: pool, q: store.New(pool), user: uuid.New()}
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		f.user, fmt.Sprintf("salvage-%s@e2e", f.user))
	// Registered after pool.Close, so it runs BEFORE it (LIFO). The salvage rows go first:
	// a live pointer would RESTRICT the user cascade onto the runs.
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM run_salvage WHERE run_id = ANY($1) OR user_id = $2`, f.runs, f.user); err != nil {
			t.Errorf("cleanup run_salvage: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, f.user); err != nil {
			t.Errorf("cleanup user: %v", err)
		}
	})
	return f
}

// repo seeds a forge connection of forgeType and one repo on it.
func (f *salvageFixture) repo(forgeType string) uuid.UUID {
	f.t.Helper()
	f.n++
	conn, repo := uuid.New(), uuid.New()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, $3, $4, 'bot', 1, $5)`, conn, f.user, forgeType, "https://"+conn.String()+".e2e", []byte{0x1})
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch)
		 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', 'main')`, repo, conn, f.n, "g/r-"+repo.String())
	return repo
}

type salvageRunSpec struct {
	kind       string
	status     string
	issueIID   *int64
	tip        *string
	failOrigin *string
	finishedAt time.Time
}

func (f *salvageFixture) run(repo uuid.UUID, s salvageRunSpec) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	mustExec(f.ctx, f.t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		                   checkpoint_tip, fail_origin, finished_at)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, $7, $8, $9)`,
		id, f.user, repo, s.issueIID, s.status, s.kind, s.tip, s.failOrigin, s.finishedAt)
	f.runs = append(f.runs, id)
	return id
}

// failedRun seeds a failed issue run with a recorded tip.
func (f *salvageFixture) failedRun(repo uuid.UUID) uuid.UUID {
	f.t.Helper()
	iid, tip := int64(7), salvageTip
	return f.run(repo, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, tip: &tip, finishedAt: time.Now()})
}

// insert records a salvage row: 'pending' with live_run_id = run, anything else with NULL.
func (f *salvageFixture) insert(run, repo uuid.UUID, state string) (int64, error) {
	live := pgtype.UUID{}
	if state == "pending" {
		live = pgtype.UUID{Bytes: run, Valid: true}
	}
	return f.q.InsertRunSalvage(f.ctx, store.InsertRunSalvageParams{
		RunID: run, UserID: f.user, RepoID: repo, ForgeType: "gitlab", Branch: "agent/issue-7",
		Tip: salvageTip, LiveRunID: live, State: state,
	})
}

func (f *salvageFixture) mustInsert(run, repo uuid.UUID, state string) {
	f.t.Helper()
	if n, err := f.insert(run, repo, state); err != nil || n != 1 {
		f.t.Fatalf("InsertRunSalvage(%s) = (%d, %v), want (1, nil)", state, n, err)
	}
}

func (f *salvageFixture) get(run uuid.UUID) store.RunSalvage {
	f.t.Helper()
	row, err := f.q.GetRunSalvage(f.ctx, run)
	if err != nil {
		f.t.Fatalf("GetRunSalvage: %v", err)
	}
	return row
}

func (f *salvageFixture) created(run uuid.UUID, at, expires time.Time) int64 {
	f.t.Helper()
	n, err := f.q.RecordSalvageCreated(f.ctx, store.RecordSalvageCreatedParams{
		RunID: run, CreatedAt: pgtype.Timestamptz{Time: at, Valid: true}, ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		f.t.Fatalf("RecordSalvageCreated: %v", err)
	}
	return n
}

func salvagePgErr(err error) (code, constraint string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	return "", ""
}

// The CHECKs: unknown state, malformed tip, a live state without its pointer, a
// secret-skipped row with one, an over-long error, and the created-salvage invariant.
func TestRunSalvageChecksLiveDB(t *testing.T) {
	f := openSalvageLiveDB(t)
	repo := f.repo("gitlab")

	// Positive control: a well-formed pending row inserts.
	ok := f.failedRun(repo)
	f.mustInsert(ok, repo, "pending")

	run := f.failedRun(repo)
	if _, err := f.insert(run, repo, "bogus"); err == nil {
		t.Fatalf("an unknown state must be rejected by the state CHECK")
	} else if code, _ := salvagePgErr(err); code != "23514" {
		t.Fatalf("unknown state: %v, want 23514", err)
	}
	// 'pending' without its pointer.
	_, err := f.q.InsertRunSalvage(f.ctx, store.InsertRunSalvageParams{
		RunID: run, UserID: f.user, RepoID: repo, ForgeType: "gitlab", Branch: "b", Tip: salvageTip, State: "pending",
	})
	if code, c := salvagePgErr(err); code != "23514" || c != "run_salvage_live_states_pointer_check" {
		t.Fatalf("pending without live_run_id: %v, want 23514 on run_salvage_live_states_pointer_check", err)
	}
	// 'skipped_secret' with a pointer.
	_, err = f.q.InsertRunSalvage(f.ctx, store.InsertRunSalvageParams{
		RunID: run, UserID: f.user, RepoID: repo, ForgeType: "gitlab", Branch: "b", Tip: salvageTip,
		LiveRunID: pgtype.UUID{Bytes: run, Valid: true}, State: "skipped_secret",
	})
	if code, c := salvagePgErr(err); code != "23514" || c != "run_salvage_skipped_secret_no_pointer_check" {
		t.Fatalf("skipped_secret with live_run_id: %v, want 23514 on run_salvage_skipped_secret_no_pointer_check", err)
	}
	// A malformed tip.
	_, err = f.q.InsertRunSalvage(f.ctx, store.InsertRunSalvageParams{
		RunID: run, UserID: f.user, RepoID: repo, ForgeType: "gitlab", Branch: "b", Tip: "HEAD",
		LiveRunID: pgtype.UUID{Bytes: run, Valid: true}, State: "pending",
	})
	if code, _ := salvagePgErr(err); code != "23514" {
		t.Fatalf("malformed tip: %v, want 23514", err)
	}
	// An over-long last_error written directly (the queries bound it themselves).
	_, err = f.pool.Exec(f.ctx, `UPDATE run_salvage SET last_error = $2 WHERE run_id = $1`, ok, strings.Repeat("e", 513))
	if code, _ := salvagePgErr(err); code != "23514" {
		t.Fatalf("513-char last_error: %v, want 23514", err)
	}

	// The invariant: once salvage_created_at is set, only 'expired' may drop the pointer.
	now := time.Now()
	if n := f.created(ok, now, now.Add(time.Hour)); n != 1 {
		t.Fatalf("RecordSalvageCreated = %d rows, want 1", n)
	}
	for _, st := range []string{"pending", "promoted"} {
		mustExec(f.ctx, t, f.pool, `UPDATE run_salvage SET state = $2 WHERE run_id = $1`, ok, st)
		_, err = f.pool.Exec(f.ctx, `UPDATE run_salvage SET live_run_id = NULL WHERE run_id = $1`, ok)
		if code, _ := salvagePgErr(err); code != "23514" {
			t.Fatalf("clearing live_run_id on a created %s row: %v, want 23514", st, err)
		}
	}
	mustExec(f.ctx, t, f.pool, `UPDATE run_salvage SET state = 'pending' WHERE run_id = $1`, ok)
	// SettleSalvage to a no-ref state is refused by the invariant; 'expired' is accepted.
	_, err = f.q.SettleSalvage(f.ctx, store.SettleSalvageParams{RunID: ok, State: "unavailable"})
	if code, c := salvagePgErr(err); code != "23514" || c != "run_salvage_created_keeps_live_check" {
		t.Fatalf("settling a created row to unavailable: %v, want 23514 on run_salvage_created_keeps_live_check", err)
	}
	if n, err := f.q.SettleSalvage(f.ctx, store.SettleSalvageParams{RunID: ok, State: "expired"}); err != nil || n != 1 {
		t.Fatalf("SettleSalvage(expired) on a created row = (%d, %v), want (1, nil)", n, err)
	}
	if row := f.get(ok); row.State != "expired" || row.LiveRunID.Valid {
		t.Fatalf("after expiry: state=%s live=%v, want expired + NULL", row.State, row.LiveRunID.Valid)
	}
}

// The RESTRICT live pointer: a live row blocks the run delete (23503 on the named FK); a
// settled row does not, and survives the run as provenance; an insert after the run was
// deleted fails with 23503 and leaves no row.
func TestRunSalvageRestrictLiveDB(t *testing.T) {
	f := openSalvageLiveDB(t)
	repo := f.repo("gitlab")

	live := f.failedRun(repo)
	f.mustInsert(live, repo, "pending")
	_, err := f.pool.Exec(f.ctx, `DELETE FROM runs WHERE id = $1`, live)
	if code, c := salvagePgErr(err); code != "23503" || c != "run_salvage_live_run_id_fkey" {
		t.Fatalf("deleting a run with a live salvage row: %v, want 23503 on run_salvage_live_run_id_fkey", err)
	}

	settled := f.failedRun(repo)
	f.mustInsert(settled, repo, "skipped_secret")
	mustExec(f.ctx, t, f.pool, `DELETE FROM runs WHERE id = $1`, settled)
	if row := f.get(settled); row.State != "skipped_secret" || row.RepoID != repo {
		t.Fatalf("a settled row must survive its run as provenance, got %+v", row)
	}

	gone := f.failedRun(repo)
	mustExec(f.ctx, t, f.pool, `DELETE FROM runs WHERE id = $1`, gone)
	_, err = f.insert(gone, repo, "pending")
	if code, c := salvagePgErr(err); code != "23503" || c != "run_salvage_live_run_id_fkey" {
		t.Fatalf("inserting pending for a deleted run: %v, want 23503 on run_salvage_live_run_id_fkey", err)
	}
	if _, err := f.q.GetRunSalvage(f.ctx, gone); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("the refused insert must leave no row, GetRunSalvage err = %v", err)
	}
}

// Every transition query, including the attempt cap applying only without a created salvage.
func TestRunSalvageTransitionsLiveDB(t *testing.T) {
	f := openSalvageLiveDB(t)
	repo := f.repo("gitlab")
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Insert is idempotent on run_id.
	a := f.failedRun(repo)
	f.mustInsert(a, repo, "pending")
	if n, err := f.insert(a, repo, "pending"); err != nil || n != 0 {
		t.Fatalf("second InsertRunSalvage = (%d, %v), want (0, nil)", n, err)
	}
	if row := f.get(a); row.State != "pending" || uuid.UUID(row.LiveRunID.Bytes) != a || row.Attempts != 0 || row.Tip != salvageTip {
		t.Fatalf("fresh row = %+v", row)
	}

	fail := func(run uuid.UUID, msg string, attemptCap int32) int64 {
		t.Helper()
		n, err := f.q.RecordSalvageAttemptFailed(f.ctx, store.RecordSalvageAttemptFailedParams{RunID: run, LastError: msg, AttemptCap: attemptCap})
		if err != nil {
			t.Fatalf("RecordSalvageAttemptFailed: %v", err)
		}
		return n
	}

	// Attempt cap WITHOUT a created salvage: pending until the cap, then failed + pointer cleared.
	fail(a, "boom-1", 3)
	fail(a, strings.Repeat("x", 600), 3)
	if row := f.get(a); row.State != "pending" || row.Attempts != 2 || !row.LiveRunID.Valid || len(row.LastError.String) != 512 {
		t.Fatalf("below the cap: state=%s attempts=%d live=%v errlen=%d, want pending/2/live/512",
			row.State, row.Attempts, row.LiveRunID.Valid, len(row.LastError.String))
	}
	fail(a, "boom-3", 3)
	if row := f.get(a); row.State != "failed" || row.Attempts != 3 || row.LiveRunID.Valid || row.LastError.String != "boom-3" {
		t.Fatalf("at the cap: %+v, want failed/3/no pointer/boom-3", row)
	}
	// A failed row takes no further transition.
	if n := fail(a, "late", 3); n != 0 {
		t.Fatalf("RecordSalvageAttemptFailed on a failed row = %d rows, want 0", n)
	}
	if n, _ := f.q.MarkSalvagePromoted(f.ctx, store.MarkSalvagePromotedParams{RunID: a, PromotedAt: pgtype.Timestamptz{Time: now, Valid: true}}); n != 0 {
		t.Fatalf("MarkSalvagePromoted on a failed row = %d rows, want 0", n)
	}

	// Attempt cap WITH a created salvage: stays pending and live, whatever the count.
	b := f.failedRun(repo)
	f.mustInsert(b, repo, "pending")
	// MarkSalvagePromoted needs a created salvage ref.
	if n, _ := f.q.MarkSalvagePromoted(f.ctx, store.MarkSalvagePromotedParams{RunID: b, PromotedAt: pgtype.Timestamptz{Time: now, Valid: true}}); n != 0 {
		t.Fatalf("MarkSalvagePromoted before RecordSalvageCreated = %d rows, want 0", n)
	}
	exp := now.Add(time.Hour)
	if n := f.created(b, now, exp); n != 1 {
		t.Fatalf("RecordSalvageCreated = %d, want 1", n)
	}
	// A repeat keeps the first values.
	if n := f.created(b, now.Add(time.Minute), now.Add(2*time.Hour)); n != 1 {
		t.Fatalf("repeat RecordSalvageCreated = %d, want 1", n)
	}
	if row := f.get(b); !row.SalvageCreatedAt.Time.Equal(now) || !row.ExpiresAt.Time.Equal(exp) {
		t.Fatalf("repeat RecordSalvageCreated moved the values: created=%v expires=%v", row.SalvageCreatedAt.Time, row.ExpiresAt.Time)
	}
	for i := 0; i < 4; i++ {
		fail(b, "delete failed", 3)
	}
	if row := f.get(b); row.State != "pending" || row.Attempts != 4 || !row.LiveRunID.Valid {
		t.Fatalf("created salvage past the cap: %+v, want pending/4/live", row)
	}
	if n, err := f.q.MarkSalvagePromoted(f.ctx, store.MarkSalvagePromotedParams{RunID: b, PromotedAt: pgtype.Timestamptz{Time: now, Valid: true}}); err != nil || n != 1 {
		t.Fatalf("MarkSalvagePromoted = (%d, %v), want (1, nil)", n, err)
	}
	if row := f.get(b); row.State != "promoted" || !row.PromotedAt.Time.Equal(now) || row.LastError.Valid || !row.LiveRunID.Valid {
		t.Fatalf("promoted row = %+v", row)
	}
	// RecordSalvageCreated is pending-only.
	if n := f.created(b, now, exp); n != 0 {
		t.Fatalf("RecordSalvageCreated on a promoted row = %d, want 0", n)
	}
	// A failed expiry delete keeps state and pointer, bumps attempts, bounds the error.
	if n, err := f.q.RecordSalvageExpireFailed(f.ctx, store.RecordSalvageExpireFailedParams{RunID: b, LastError: strings.Repeat("y", 700)}); err != nil || n != 1 {
		t.Fatalf("RecordSalvageExpireFailed = (%d, %v), want (1, nil)", n, err)
	}
	if row := f.get(b); row.State != "promoted" || row.Attempts != 5 || !row.LiveRunID.Valid || len(row.LastError.String) != 512 {
		t.Fatalf("after a failed expiry: %+v", row)
	}

	// SettleSalvage: only the four no-ref targets, only from a live row.
	c := f.failedRun(repo)
	f.mustInsert(c, repo, "pending")
	if n, _ := f.q.RecordSalvageExpireFailed(f.ctx, store.RecordSalvageExpireFailedParams{RunID: c, LastError: "x"}); n != 0 {
		t.Fatalf("RecordSalvageExpireFailed without a created salvage = %d rows, want 0", n)
	}
	for _, bad := range []string{"failed", "pending", "promoted", "skipped_secret", "bogus"} {
		if n, err := f.q.SettleSalvage(f.ctx, store.SettleSalvageParams{RunID: c, State: bad}); err != nil || n != 0 {
			t.Fatalf("SettleSalvage(%s) = (%d, %v), want (0, nil)", bad, n, err)
		}
	}
	if n, err := f.q.SettleSalvage(f.ctx, store.SettleSalvageParams{RunID: c, State: "unavailable"}); err != nil || n != 1 {
		t.Fatalf("SettleSalvage(unavailable) = (%d, %v), want (1, nil)", n, err)
	}
	if row := f.get(c); row.State != "unavailable" || row.LiveRunID.Valid {
		t.Fatalf("settled row = %+v", row)
	}
	if n, _ := f.q.SettleSalvage(f.ctx, store.SettleSalvageParams{RunID: c, State: "expired"}); n != 0 {
		t.Fatalf("re-settling a settled row = %d rows, want 0", n)
	}
	for _, st := range []string{"refused", "disabled"} {
		r := f.failedRun(repo)
		f.mustInsert(r, repo, "pending")
		if n, err := f.q.SettleSalvage(f.ctx, store.SettleSalvageParams{RunID: r, State: st}); err != nil || n != 1 {
			t.Fatalf("SettleSalvage(%s) = (%d, %v), want (1, nil)", st, n, err)
		}
	}

	// The due lists, filtered to this test's rows (the database is shared).
	d := f.failedRun(repo) // created, expiry passed, pending
	f.mustInsert(d, repo, "pending")
	f.created(d, now.Add(-2*time.Hour), now.Add(-time.Hour))
	e := f.failedRun(repo) // created, expiry in the future
	f.mustInsert(e, repo, "pending")
	f.created(e, now, now.Add(24*time.Hour))
	mine := map[uuid.UUID]bool{}
	for _, id := range f.runs {
		mine[id] = true
	}
	pending, err := f.q.ListSalvageDuePending(f.ctx, 10000)
	if err != nil {
		t.Fatalf("ListSalvageDuePending: %v", err)
	}
	gotPending := map[uuid.UUID]bool{}
	for _, row := range pending {
		if mine[row.RunID] {
			gotPending[row.RunID] = true
		}
	}
	if len(gotPending) != 2 || !gotPending[d] || !gotPending[e] {
		t.Fatalf("ListSalvageDuePending (this test's rows) = %v, want exactly {d, e}", gotPending)
	}
	due, err := f.q.ListSalvageDueExpiry(f.ctx, store.ListSalvageDueExpiryParams{Now: pgtype.Timestamptz{Time: now, Valid: true}, Lim: 10000})
	if err != nil {
		t.Fatalf("ListSalvageDueExpiry: %v", err)
	}
	gotDue := map[uuid.UUID]bool{}
	for _, row := range due {
		if mine[row.RunID] {
			gotDue[row.RunID] = true
		}
	}
	// d (pending, past expiry) is due; e (future) is not; b (promoted, expires in an hour) is not.
	if len(gotDue) != 1 || !gotDue[d] {
		t.Fatalf("ListSalvageDueExpiry(now) (this test's rows) = %v, want exactly {d}", gotDue)
	}
	due, err = f.q.ListSalvageDueExpiry(f.ctx, store.ListSalvageDueExpiryParams{Now: pgtype.Timestamptz{Time: now.Add(2 * time.Hour), Valid: true}, Lim: 10000})
	if err != nil {
		t.Fatalf("ListSalvageDueExpiry: %v", err)
	}
	gotDue = map[uuid.UUID]bool{}
	for _, row := range due {
		if mine[row.RunID] {
			gotDue[row.RunID] = true
		}
	}
	if len(gotDue) != 2 || !gotDue[d] || !gotDue[b] {
		t.Fatalf("ListSalvageDueExpiry(now+2h) (this test's rows) = %v, want exactly {d, b}", gotDue)
	}
}

// ListSalvageCandidates: only failed, published, eligible-kind, non-plan-rejected runs on a
// listed forge inside the window, without a row yet, oldest first.
func TestListSalvageCandidatesLiveDB(t *testing.T) {
	f := openSalvageLiveDB(t)
	gl := f.repo("gitlab")
	fj := f.repo("forgejo")
	// A window no other test's runs reach.
	since := time.Now().AddDate(150, 0, 0).UTC().Truncate(time.Second)
	at := func(m int) time.Time { return since.Add(time.Duration(m) * time.Minute) }
	iid, tip := int64(9), salvageTip
	planRejected, secret := "plan_rejected", "push_secret_blocked"

	issue := f.run(gl, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, tip: &tip, finishedAt: at(3)})
	self := f.run(gl, salvageRunSpec{kind: "self_improve", status: "failed", issueIID: &iid, tip: &tip, finishedAt: at(1)})
	withSecret := f.run(gl, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, tip: &tip, failOrigin: &secret, finishedAt: at(2)})
	onForgejo := f.run(fj, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, tip: &tip, finishedAt: at(4)})
	rowed := f.run(gl, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, tip: &tip, finishedAt: at(5)})
	f.mustInsert(rowed, gl, "skipped_secret")
	// Excluded shapes.
	f.run(gl, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, tip: &tip, failOrigin: &planRejected, finishedAt: at(6)})
	f.run(gl, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, finishedAt: at(7)}) // never published
	f.run(gl, salvageRunSpec{kind: "issue", status: "completed", issueIID: &iid, tip: &tip, finishedAt: at(8)})
	f.run(gl, salvageRunSpec{kind: "issue", status: "cancelled", issueIID: &iid, tip: &tip, finishedAt: at(8)})
	f.run(gl, salvageRunSpec{kind: "prompt", status: "failed", tip: &tip, finishedAt: at(9)})                 // non-eligible kind
	f.run(gl, salvageRunSpec{kind: "issue", status: "failed", issueIID: &iid, tip: &tip, finishedAt: at(-1)}) // outside the window

	list := func(forges []string, lim int32) []uuid.UUID {
		t.Helper()
		rows, err := f.q.ListSalvageCandidates(f.ctx, store.ListSalvageCandidatesParams{
			Forges: forges, Since: pgtype.Timestamptz{Time: since, Valid: true}, Lim: lim,
		})
		if err != nil {
			t.Fatalf("ListSalvageCandidates: %v", err)
		}
		ids := make([]uuid.UUID, 0, len(rows))
		for _, r := range rows {
			ids = append(ids, r.ID)
			if r.CheckpointTip != salvageTip || r.UserID != f.user || r.ForgeType == "" {
				t.Fatalf("candidate row %+v", r)
			}
		}
		return ids
	}
	eq := func(got, want []uuid.UUID) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	if got, want := list([]string{"gitlab"}, 100), []uuid.UUID{self, withSecret, issue}; !eq(got, want) {
		t.Fatalf("candidates(gitlab) = %v, want %v (self, secret, issue by finished_at)", got, want)
	}
	if got, want := list([]string{"gitlab", "forgejo"}, 100), []uuid.UUID{self, withSecret, issue, onForgejo}; !eq(got, want) {
		t.Fatalf("candidates(gitlab,forgejo) = %v, want %v", got, want)
	}
	if got := list([]string{"github"}, 100); len(got) != 0 {
		t.Fatalf("candidates(github) = %v, want none", got)
	}
	if got := list([]string{}, 100); len(got) != 0 {
		t.Fatalf("candidates(no forge) = %v, want none", got)
	}
	if got, want := list([]string{"gitlab"}, 2), []uuid.UUID{self, withSecret}; !eq(got, want) {
		t.Fatalf("candidates(gitlab, lim 2) = %v, want %v", got, want)
	}
	rows, err := f.q.ListSalvageCandidates(f.ctx, store.ListSalvageCandidatesParams{
		Forges: []string{"gitlab"}, Since: pgtype.Timestamptz{Time: since, Valid: true}, Lim: 100,
	})
	if err != nil {
		t.Fatalf("ListSalvageCandidates: %v", err)
	}
	for _, r := range rows {
		if r.ID == self && (r.Kind != "self_improve" || r.RepoID != gl || r.ForgeType != "gitlab") {
			t.Fatalf("self_improve candidate = %+v", r)
		}
		if r.ID == withSecret && r.FailOrigin.String != "push_secret_blocked" {
			t.Fatalf("secret candidate must carry its fail_origin, got %+v", r)
		}
	}
	// Recording the issue run removes it.
	f.mustInsert(issue, gl, "pending")
	if got, want := list([]string{"gitlab"}, 100), []uuid.UUID{self, withSecret}; !eq(got, want) {
		t.Fatalf("after recording issue: %v, want %v", got, want)
	}
}

// The delete-guard reads: count and ref names per repo and per (owner-scoped) connection.
func TestLiveSalvageRefsLiveDB(t *testing.T) {
	f := openSalvageLiveDB(t)
	repo := f.repo("gitlab")
	var conn uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT connection_id FROM repos WHERE id = $1`, repo).Scan(&conn); err != nil {
		t.Fatalf("connection: %v", err)
	}
	pending := f.failedRun(repo)
	f.mustInsert(pending, repo, "pending")
	created := f.failedRun(repo)
	f.mustInsert(created, repo, "pending")
	now := time.Now()
	f.created(created, now, now.Add(time.Hour))
	settled := f.failedRun(repo)
	f.mustInsert(settled, repo, "skipped_secret")

	if n, err := f.q.CountLiveSalvageForRepo(f.ctx, repo); err != nil || n != 2 {
		t.Fatalf("CountLiveSalvageForRepo = (%d, %v), want (2, nil)", n, err)
	}
	if n, err := f.q.CountLiveSalvageForConnection(f.ctx, store.CountLiveSalvageForConnectionParams{ConnectionID: conn, UserID: f.user}); err != nil || n != 2 {
		t.Fatalf("CountLiveSalvageForConnection = (%d, %v), want (2, nil)", n, err)
	}
	if n, err := f.q.CountLiveSalvageForConnection(f.ctx, store.CountLiveSalvageForConnectionParams{ConnectionID: conn, UserID: uuid.New()}); err != nil || n != 0 {
		t.Fatalf("CountLiveSalvageForConnection(foreign owner) = (%d, %v), want (0, nil)", n, err)
	}
	want := map[string]bool{"refs/uzi-checkpoints/agent/issue-7": true, "refs/uzi-salvage/" + created.String(): true}
	byRepo, err := f.q.ListLiveSalvageRefsForRepo(f.ctx, store.ListLiveSalvageRefsForRepoParams{RepoID: repo, Lim: 5})
	if err != nil || len(byRepo) != 2 {
		t.Fatalf("ListLiveSalvageRefsForRepo = (%v, %v), want 2 rows", byRepo, err)
	}
	for _, r := range byRepo {
		if !want[r.Ref] {
			t.Fatalf("unexpected repo ref %q (want %v)", r.Ref, want)
		}
	}
	byConn, err := f.q.ListLiveSalvageRefsForConnection(f.ctx, store.ListLiveSalvageRefsForConnectionParams{ConnectionID: conn, UserID: f.user, Lim: 1})
	if err != nil || len(byConn) != 1 || !want[byConn[0].Ref] {
		t.Fatalf("ListLiveSalvageRefsForConnection(lim 1) = (%v, %v), want 1 known ref", byConn, err)
	}
}
