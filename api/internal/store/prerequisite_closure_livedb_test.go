package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type prerequisiteFixture struct {
	heldFixture
	capture uuid.UUID
	closure store.RecoveryPrerequisiteClosure
}

func prerequisiteUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
func prerequisiteText(s string) pgtype.Text     { return pgtype.Text{String: s, Valid: true} }

func prerequisiteNew(t *testing.T, ctx context.Context, pool *pgxpool.Pool) prerequisiteFixture {
	t.Helper()
	f := prerequisiteFixture{heldFixture: heldNewHoldFixture(ctx, t, pool, true, "failed", nil), capture: uuid.New()}
	mustExec(ctx, t, pool, `INSERT INTO recovery_captures
        (id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,
         source_sha,idempotency_key,state,manifest_bound,byte_size,checksum,chunk_count,
         prerequisite_shas,reserved_bytes,coverage_digest,expires_at)
        VALUES ($1,$2,$3,$4,$5,'held-worker',repeat('a',40),$1::uuid::text,'available',true,
            3,repeat('f',64),1,ARRAY[repeat('b',40),repeat('c',40)],3,repeat('a',64),now()+interval '1 day')`,
		f.capture, f.hold, f.run, f.user, f.worker)
	var err error
	f.closure, err = store.New(pool).InsertPrerequisiteClosure(ctx, f.capture)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func prerequisiteRows(t *testing.T, want int64, n int64, err error) {
	t.Helper()
	if err != nil || n != want {
		t.Fatalf("rows=%d want=%d err=%v", n, want, err)
	}
}
func prerequisiteError(t *testing.T, err error) {
	t.Helper()
	var p *pgconn.PgError
	if !errors.As(err, &p) || (p.Code != "55000" && p.Code != "23514" && p.Code != "23503" && p.Code != "23502") {
		t.Fatalf("want guard/check/FK refusal, got %v", err)
	}
}
func prerequisiteRead(t *testing.T, ctx context.Context, f prerequisiteFixture) store.RecoveryPrerequisiteClosure {
	t.Helper()
	c, err := store.New(f.pool).GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: f.capture, UserID: f.user})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func prerequisiteReserve(t *testing.T, ctx context.Context, f prerequisiteFixture, lease float64) store.RecoveryPrerequisiteClosure {
	t.Helper()
	c, err := store.New(f.pool).ReservePrerequisitePreparation(ctx, store.ReservePrerequisitePreparationParams{ID: f.closure.ID, LeaseSeconds: lease})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func prerequisiteInvoke(t *testing.T, ctx context.Context, f prerequisiteFixture, send float64) {
	t.Helper()
	prerequisiteReserve(t, ctx, f, 60)
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	r, err := q.LockPrerequisiteRun(ctx, f.run)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != f.run {
		t.Fatal("wrong run")
	}
	h, err := q.LockPrerequisiteHold(ctx, f.hold)
	if err != nil || h.State != "open" {
		t.Fatalf("hold: %v", err)
	}
	c, err := q.LockPrerequisiteCapture(ctx, f.capture)
	if err != nil || c.State != "available" {
		t.Fatalf("capture: %v", err)
	}
	p, err := q.LockPrerequisiteClosure(ctx, f.closure.ID)
	if err != nil {
		t.Fatal(err)
	}
	var conn uuid.UUID
	if err = tx.QueryRow(ctx, "SELECT connection_id FROM repos WHERE id=$1", f.repo).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	args := store.MarkPrerequisiteInvokedParams{ID: p.ID, Generation: p.Attempts, SendSeconds: send,
		PinTip: prerequisiteText(heldTip), ConnectionID: prerequisiteUUID(conn), ForgeKind: prerequisiteText("github"),
		NormalizedBaseUrl: prerequisiteText("https://example.com"), ProjectID: pgtype.Int8{Int64: 1, Valid: true},
		DefaultBranch: prerequisiteText("main")}
	got, err := q.MarkPrerequisiteInvoked(ctx, args)
	if err != nil || got.State != "invoked" || !got.LiveRunID.Valid {
		t.Fatalf("marker: %v %+v", err, got)
	}
	if _, err = q.MarkPrerequisiteInvoked(ctx, args); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate marker: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
func prerequisitePin(t *testing.T, ctx context.Context, f prerequisiteFixture) {
	t.Helper()
	prerequisiteInvoke(t, ctx, f, 60)
	n, err := store.New(f.pool).RecordPrerequisitePinned(ctx, store.RecordPrerequisitePinnedParams{ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
}

// Each database is disposable. Explicit search_path makes SQL and the unchanged
// production trigger resolve now() to this schema-only clock. No trigger is
// disabled, and no lifecycle row is rewritten to simulate elapsed time.
func prerequisiteClockPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.ConnConfig.RuntimeParams["search_path"] = "public,pg_catalog"
	return pgxpool.NewWithConfig(ctx, config)
}

func prerequisiteMigratedPool(t *testing.T, prefix string) (context.Context, string, *pgxpool.Pool) {
	t.Helper()
	ctx, dsn, original := heldMigratedPool(t, prefix)
	mustExec(ctx, t, original, "CREATE TABLE prerequisite_test_clock (singleton boolean PRIMARY KEY CHECK(singleton), observed_at timestamptz NOT NULL)")
	mustExec(ctx, t, original, "INSERT INTO prerequisite_test_clock VALUES (true,pg_catalog.now())")
	mustExec(ctx, t, original, `CREATE FUNCTION public.now() RETURNS timestamptz LANGUAGE sql STABLE
        AS 'SELECT observed_at FROM public.prerequisite_test_clock WHERE singleton'`)
	pool, err := prerequisiteClockPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return ctx, dsn, pool
}

func prerequisiteAdvance(t *testing.T, ctx context.Context, pool *pgxpool.Pool, elapsed time.Duration) {
	t.Helper()
	mustExec(ctx, t, pool, "UPDATE prerequisite_test_clock SET observed_at=observed_at+make_interval(secs=>$1::float8)", elapsed.Seconds())
}

func prerequisiteElapsedAbsence(t *testing.T, ctx context.Context, f prerequisiteFixture) {
	t.Helper()
	prerequisiteAdvance(t, ctx, f.pool, 11*time.Minute)
}

func prerequisiteAudit(t *testing.T, ctx context.Context, f prerequisiteFixture) pgtype.UUID {
	t.Helper()
	mustExec(ctx, t, f.pool, "UPDATE recovery_prerequisite_closures SET audit_next_at=now() WHERE id=$1", f.closure.ID)
	pass := prerequisiteUUID(uuid.New())
	if _, err := store.New(f.pool).ReservePrerequisiteAudit(ctx, store.ReservePrerequisiteAuditParams{ID: f.closure.ID, PassID: pass}); err != nil {
		t.Fatal(err)
	}
	return pass
}

func TestPrerequisiteClosureSchemaLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_schema_")
	f := prerequisiteNew(t, ctx, pool)
	q := store.New(pool)
	c := prerequisiteRead(t, ctx, f)
	if c.State != "pending" || c.LiveRunID.Valid || c.PinTip.Valid || c.Attempts != 0 ||
		c.PinRef != "refs/uzi-prereq/"+f.capture.String() {
		t.Fatalf("pending shape: %+v", c)
	}
	if _, err := q.InsertPrerequisiteClosure(ctx, f.capture); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("duplicate insert: %v", err)
	}
	// Multiple captures may use the same hold.
	second := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO recovery_captures
        SELECT (jsonb_populate_record(NULL::recovery_captures,to_jsonb(c) ||
            jsonb_build_object('id',$2::uuid,'idempotency_key',$2::uuid::text))).*
        FROM recovery_captures c WHERE id=$1`, f.capture, second)
	if _, err := q.InsertPrerequisiteClosure(ctx, second); err != nil {
		t.Fatalf("second closure for hold: %v", err)
	}
	for _, set := range []string{
		"capture_id=gen_random_uuid()", "hold_id=gen_random_uuid()", "run_id=gen_random_uuid()",
		"user_id=gen_random_uuid()", "repo_id=gen_random_uuid()", "checksum=repeat('e',64)",
		"prerequisite_shas=ARRAY[repeat('c',40),repeat('b',40)]",
		"pin_ref='refs/uzi-prereq/other'", "live_run_id=run_id", "state='pinned'",
		"state=NULL", "reason=NULL", "attempts=9", "cleanup_attempts=9",
	} {
		_, err := pool.Exec(ctx, "UPDATE recovery_prerequisite_closures SET "+set+" WHERE id=$1", c.ID)
		prerequisiteError(t, err)
	}
	_, err := pool.Exec(ctx, "DELETE FROM recovery_prerequisite_closures WHERE id=$1", c.ID)
	prerequisiteError(t, err)
	// Trigger validates insert provenance and exact ordered manifest, not only the guarded query.
	for _, change := range []string{
		"'state','pinned'", "'attempts',1", "'run_id',gen_random_uuid()", "'repo_id',gen_random_uuid()",
		"'hold_id',gen_random_uuid()", "'checksum',repeat('e',64)",
		"'prerequisite_shas',jsonb_build_array(repeat('c',40),repeat('b',40))",
		"'prerequisite_shas','[]'::jsonb", "'prerequisite_shas',jsonb_build_array(NULL)",
	} {
		_, err = pool.Exec(ctx, `INSERT INTO recovery_prerequisite_closures
            SELECT (jsonb_populate_record(NULL::recovery_prerequisite_closures,to_jsonb(p) ||
                jsonb_build_object('id',gen_random_uuid(),`+change+`))).*
            FROM recovery_prerequisite_closures p WHERE id=$1`, c.ID)
		prerequisiteError(t, err)
	}
	prerequisiteInvoke(t, ctx, f, 60)
	for _, set := range []string{"pin_tip=repeat('0',40)", "pin_tip=upper(repeat('a',40))",
		"connection_id=NULL", "forge_kind=NULL", "normalized_base_url=NULL", "project_id=NULL",
		"default_branch=NULL", "invoked_at=NULL", "live_run_id=NULL", "live_run_id=gen_random_uuid()",
		"pin_tip=repeat('d',40)", "connection_id=gen_random_uuid()", "forge_kind='gitlab'",
		"normalized_base_url='https://other.example.com'", "project_id=2", "default_branch='other'",
		"invoked_at=invoked_at-interval '1 second'", "invoked_deadline=invoked_deadline+interval '1 second'"} {
		_, err = pool.Exec(ctx, "UPDATE recovery_prerequisite_closures SET "+set+" WHERE id=$1", c.ID)
		prerequisiteError(t, err)
	}
	// Isolate the named live FK from the older custody-hold FK.
	mustExec(ctx, t, pool, "UPDATE recovery_custody_holds SET state='discarded',release_evidence='owner_discard',live_run_id=NULL,live_worker_id=NULL WHERE id=$1", f.hold)
	_, err = pool.Exec(ctx, "DELETE FROM runs WHERE id=$1", f.run)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.ConstraintName != "recovery_prerequisite_closures_live_run_id_fkey" {
		t.Fatalf("named live FK: %v", err)
	}
	var conn uuid.UUID
	if err = pool.QueryRow(ctx, "SELECT connection_id FROM repos WHERE id=$1", f.repo).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	n, err := q.CountLivePrerequisiteClosuresForRun(ctx, store.CountLivePrerequisiteClosuresForRunParams{RunID: prerequisiteUUID(f.run), UserID: f.user})
	prerequisiteRows(t, 1, n, err)
	n, err = q.CountLivePrerequisiteClosuresForRepo(ctx, store.CountLivePrerequisiteClosuresForRepoParams{RepoID: f.repo, UserID: f.user})
	prerequisiteRows(t, 1, n, err)
	n, err = q.CountLivePrerequisiteClosuresForConnection(ctx, store.CountLivePrerequisiteClosuresForConnectionParams{ConnectionID: conn, UserID: f.user})
	prerequisiteRows(t, 1, n, err)
	n, err = q.CountLivePrerequisiteClosuresForRun(ctx, store.CountLivePrerequisiteClosuresForRunParams{RunID: prerequisiteUUID(f.run), UserID: uuid.New()})
	prerequisiteRows(t, 0, n, err)
}

func TestPrerequisiteClosureTransitionsLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_transitions_")
	states := []string{"pending", "invoked", "create_unknown", "pinned", "refused", "foreign", "lost", "deleted"}
	// Raw SQL checks every edge independently, including forbidden terminal exits.
	permitted := map[string]bool{
		"pending/invoked": true, "pending/refused": true,
		"invoked/pinned": true, "invoked/refused": true, "invoked/create_unknown": true, "invoked/foreign": true,
		"create_unknown/pinned": true, "create_unknown/foreign": true,
		"pinned/foreign": true, "pinned/lost": true, "pinned/deleted": true,
	}
	for _, from := range states {
		for _, to := range states {
			if from == to {
				continue
			}
			t.Run(from+"/"+to, func(t *testing.T) {
				f := prerequisiteNew(t, ctx, pool)
				prerequisiteSetState(t, ctx, f, from)
				c := prerequisiteRead(t, ctx, f)
				set := "state=$2"
				if to != "pinned" {
					set += ",absent_since=NULL,absence_pass_id=NULL"
				}
				if to == "refused" || to == "foreign" || to == "lost" || to == "deleted" {
					set += ",live_run_id=NULL"
				}
				if to == "lost" {
					set += ",last_audit_pass_id=gen_random_uuid()"
				}
				// pending->invoked uses the production marker (and required locks) below.
				if from == "pending" && to == "invoked" {
					prerequisiteInvoke(t, ctx, f, 60)
					return
				}
				if from == "pinned" && to == "deleted" {
					mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='expired' WHERE id=$1", f.capture)
				}
				tag, err := pool.Exec(ctx, "UPDATE recovery_prerequisite_closures SET "+set+" WHERE id=$1", c.ID, to)
				if permitted[from+"/"+to] {
					if err != nil || tag.RowsAffected() != 1 {
						t.Fatalf("permitted edge: rows=%d err=%v", tag.RowsAffected(), err)
					}
				} else {
					prerequisiteError(t, err)
				}
			})
		}
	}
}

func prerequisiteSetState(t *testing.T, ctx context.Context, f prerequisiteFixture, state string) {
	t.Helper()
	q := store.New(f.pool)
	if state == "pending" {
		return
	}
	if state == "refused" {
		prerequisiteReserve(t, ctx, f, 60)
		n, err := q.RefusePrerequisitePreparation(ctx, store.RefusePrerequisitePreparationParams{ID: f.closure.ID, Generation: 1, Reason: "archive_unavailable"})
		prerequisiteRows(t, 1, n, err)
		return
	}
	prerequisiteInvoke(t, ctx, f, 60)
	if state == "invoked" {
		return
	}
	if state == "create_unknown" {
		n, err := q.MarkPrerequisiteCreateUnknown(ctx, store.MarkPrerequisiteCreateUnknownParams{ID: f.closure.ID, Generation: 1})
		prerequisiteRows(t, 1, n, err)
		return
	}
	n, err := q.RecordPrerequisitePinned(ctx, store.RecordPrerequisitePinnedParams{ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
	if state == "pinned" || state == "lost" {
		// Seed only the elapsed clock for a positive lost edge; production results are tested below.
		pass := prerequisiteAudit(t, ctx, f)
		n, err = q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: pass})
		prerequisiteRows(t, 1, n, err)
		prerequisiteElapsedAbsence(t, ctx, f)
		if state == "pinned" {
			return
		}
		n, err = q.ConfirmPrerequisiteLost(ctx, store.ConfirmPrerequisiteLostParams{ID: f.closure.ID, PassID: prerequisiteAudit(t, ctx, f)})
		prerequisiteRows(t, 1, n, err)
		return
	}
	if state == "foreign" {
		n, err = q.RecordPrerequisitePinnedForeign(ctx, store.RecordPrerequisitePinnedForeignParams{ID: f.closure.ID, Generation: 0, PassID: prerequisiteAudit(t, ctx, f)})
		prerequisiteRows(t, 1, n, err)
		return
	}
	mustExec(ctx, t, f.pool, "UPDATE recovery_captures SET state='discarded' WHERE id=$1", f.capture)
	_, err = q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{PassID: prerequisiteUUID(uuid.New()), ID: f.closure.ID, LeaseSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	n, err = q.RecordPrerequisiteDeleted(ctx, store.RecordPrerequisiteDeletedParams{PassID: prerequisiteRead(t, ctx, f).LastAuditPassID, ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
}

func TestPrerequisiteClosureQueryExecutionLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_queries_")
	q := store.New(pool)
	f := prerequisiteNew(t, ctx, pool)
	due := pgtype.Timestamptz{Time: time.Now().Add(48 * time.Hour), Valid: true}
	rows, err := q.ListPrerequisitePendingDue(ctx, store.ListPrerequisitePendingDueParams{DueAt: due, AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, Lim: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != f.closure.ID {
		t.Fatalf("pending: %v %v", rows, err)
	}
	prerequisiteInvoke(t, ctx, f, 0.5)
	rows, err = q.ListPrerequisiteStaleInvokedDue(ctx, store.ListPrerequisiteStaleInvokedDueParams{DueAt: due, AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, Lim: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("invoked: %v %v", rows, err)
	}
	prerequisiteAdvance(t, ctx, pool, time.Second)
	n, err := q.SettleStalePrerequisiteInvocation(ctx, f.closure.ID)
	prerequisiteRows(t, 1, n, err)
	rows, err = q.ListPrerequisiteReconcileDue(ctx, store.ListPrerequisiteReconcileDueParams{DueAt: due, AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, Lim: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("unknown: %v %v", rows, err)
	}
	n, err = q.DeferPrerequisiteReconciliation(ctx, store.DeferPrerequisiteReconciliationParams{ID: f.closure.ID, Generation: 1, LastError: strings.Repeat("x", 600)})
	prerequisiteRows(t, 1, n, err)
	n, err = q.RecordPrerequisiteCreateRefused(ctx, store.RecordPrerequisiteCreateRefusedParams{ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 0, n, err) // listing-only unknown must never become refused
	n, err = q.RecordPrerequisitePinned(ctx, store.RecordPrerequisitePinnedParams{ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
	rows, err = q.ListPrerequisiteAuditDue(ctx, store.ListPrerequisiteAuditDueParams{DueAt: due, AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, Lim: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit: %v %v", rows, err)
	}
	mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET audit_next_at=now() WHERE id=$1", f.closure.ID)
	pass := prerequisiteUUID(uuid.New())
	if _, err = q.ReservePrerequisiteAudit(ctx, store.ReservePrerequisiteAuditParams{ID: f.closure.ID, PassID: pass}); err != nil {
		t.Fatal(err)
	}
	if _, err = q.ReservePrerequisiteAudit(ctx, store.ReservePrerequisiteAuditParams{ID: f.closure.ID, PassID: pass}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("same-pass audit: %v", err)
	}
	n, err = q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: pass})
	prerequisiteRows(t, 1, n, err)
	n, err = q.ConfirmPrerequisiteLost(ctx, store.ConfirmPrerequisiteLostParams{ID: f.closure.ID, PassID: prerequisiteUUID(uuid.New())})
	prerequisiteRows(t, 0, n, err)
	n, err = q.ClearPrerequisiteAbsence(ctx, store.ClearPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: pass})
	prerequisiteRows(t, 0, n, err)
	n, err = q.ClearPrerequisiteAbsence(ctx, store.ClearPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: prerequisiteAudit(t, ctx, f)})
	prerequisiteRows(t, 1, n, err)
	// A definitive first-send refusal is distinct from unknown reconciliation.
	refused := prerequisiteNew(t, ctx, pool)
	prerequisiteInvoke(t, ctx, refused, 60)
	n, err = q.RecordPrerequisiteCreateRefused(ctx, store.RecordPrerequisiteCreateRefusedParams{ID: refused.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
	// Every cleanup query executes after capture eligibility changes.
	mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='discarded' WHERE id=$1", f.capture)
	rows, err = q.ListPrerequisiteCleanupDue(ctx, store.ListPrerequisiteCleanupDueParams{DueAt: due, AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, Lim: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("cleanup: %v %v", rows, err)
	}
	c, err := q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{PassID: prerequisiteUUID(uuid.New()), ID: f.closure.ID, LeaseSeconds: 60})
	if err != nil || c.CleanupAttempts != 1 {
		t.Fatalf("cleanup reserve: %+v %v", c, err)
	}
	n, err = q.RecordPrerequisiteCleanupRetry(ctx, store.RecordPrerequisiteCleanupRetryParams{PassID: prerequisiteRead(t, ctx, f).LastAuditPassID, ID: f.closure.ID, Generation: 1, LastError: "unknown delete"})
	prerequisiteRows(t, 1, n, err)
	n, err = q.RecordPrerequisiteDeleted(ctx, store.RecordPrerequisiteDeletedParams{PassID: prerequisiteRead(t, ctx, f).LastAuditPassID, ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 0, n, err)
	n, err = q.ExpirePrerequisiteCleanup(ctx, store.ExpirePrerequisiteCleanupParams{PassID: prerequisiteRead(t, ctx, f).LastAuditPassID, ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 0, n, err)
	if !prerequisiteRead(t, ctx, f).LiveRunID.Valid {
		t.Fatal("ambiguous deletion lost live pointer")
	}
	n, err = q.ConfirmPrerequisiteCleanupAbsent(ctx, store.ConfirmPrerequisiteCleanupAbsentParams{ID: f.closure.ID, Generation: 1, PassID: uuid.UUID(prerequisiteAudit(t, ctx, f).Bytes)})
	prerequisiteRows(t, 1, n, err)

}

func TestPrerequisiteClosureReservationsLiveDB(t *testing.T) {
	ctx, dsn, pool := prerequisiteMigratedPool(t, "prereq_reserve_")
	second, err := prerequisiteClockPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	f := prerequisiteNew(t, ctx, pool)
	n, err := store.New(pool).DeferPrerequisitePreparation(ctx, store.DeferPrerequisitePreparationParams{ID: f.closure.ID, Generation: 0})
	prerequisiteRows(t, 1, n, err)
	if got := prerequisiteRead(t, ctx, f); got.Attempts != 0 || got.State != "pending" || got.Reason != "" {
		t.Fatalf("before-S1 deferral: %+v", got)
	}
	mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", f.closure.ID)
	// Two independent pools race one durable reservation. Neither retries: one wins, one loses.
	race := func(cleanup bool) {
		t.Helper()
		start := make(chan struct{})
		results := make(chan error, 2)
		var wg sync.WaitGroup
		for _, p := range []*pgxpool.Pool{pool, second} {
			wg.Add(1)
			go func(p *pgxpool.Pool) {
				defer wg.Done()
				<-start
				var err error
				if cleanup {
					_, err = store.New(p).ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{PassID: prerequisiteUUID(uuid.New()), ID: f.closure.ID, LeaseSeconds: 60})
				} else {
					_, err = store.New(p).ReservePrerequisitePreparation(ctx, store.ReservePrerequisitePreparationParams{ID: f.closure.ID, LeaseSeconds: 60})
				}
				results <- err
			}(p)
		}
		close(start)
		wg.Wait()
		close(results)
		wins, loses := 0, 0
		for err := range results {
			if err == nil {
				wins++
			} else if errors.Is(err, pgx.ErrNoRows) {
				loses++
			} else {
				t.Fatal(err)
			}
		}
		if wins != 1 || loses != 1 {
			t.Fatalf("reservation winners=%d losers=%d", wins, loses)
		}
	}
	race(false)
	q := store.New(pool)
	n, err = q.RecordPrerequisitePreparationRetry(ctx, store.RecordPrerequisitePreparationRetryParams{ID: f.closure.ID, Generation: 1, BudgetShortfall: true})
	prerequisiteRows(t, 1, n, err)
	got := prerequisiteRead(t, ctx, f)
	if got.Reason != "" || got.Attempts != 1 || got.AttemptDeadline.Valid || !got.NextAttemptAt.Time.After(time.Now()) {
		t.Fatalf("pending budget retry: %+v", got)
	}
	n, err = q.RecordPrerequisitePreparationRetry(ctx, store.RecordPrerequisitePreparationRetryParams{ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 0, n, err)
	// Repeat at the finite ceiling, not merely on the first reservation.
	for gen := int32(2); gen <= 7; gen++ {
		mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", f.closure.ID)
		prerequisiteReserve(t, ctx, f, 60)
		n, err = q.RecordPrerequisitePreparationRetry(ctx, store.RecordPrerequisitePreparationRetryParams{ID: f.closure.ID, Generation: gen, BudgetShortfall: true})
		prerequisiteRows(t, 1, n, err)
	}
	mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", f.closure.ID)
	race(false)
	if c := prerequisiteRead(t, ctx, f); c.Attempts != 8 {
		t.Fatalf("ceiling reservation: %+v", c)
	}
	n, err = q.RecordPrerequisitePreparationRetry(ctx, store.RecordPrerequisitePreparationRetryParams{ID: f.closure.ID, Generation: 7, BudgetShortfall: true})
	prerequisiteRows(t, 0, n, err)
	n, err = q.RecordPrerequisitePreparationRetry(ctx, store.RecordPrerequisitePreparationRetryParams{ID: f.closure.ID, Generation: 8, BudgetShortfall: true})
	prerequisiteRows(t, 1, n, err)
	// Separate fixture for cleanup race, after a real marker and pinned result.
	f = prerequisiteNew(t, ctx, pool)
	prerequisitePin(t, ctx, f)
	mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='discarded' WHERE id=$1", f.capture)
	race(true)
	if prerequisiteRead(t, ctx, f).CleanupAttempts != 1 {
		t.Fatal("double cleanup reservation")
	}
	for gen := int32(1); gen <= 7; gen++ {
		if gen > 1 {
			mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", f.closure.ID)
			if _, err = q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{PassID: prerequisiteUUID(uuid.New()), ID: f.closure.ID, LeaseSeconds: 60}); err != nil {
				t.Fatal(err)
			}
		}
		c := prerequisiteRead(t, ctx, f)
		n, err = q.RecordPrerequisiteCleanupRetry(ctx, store.RecordPrerequisiteCleanupRetryParams{ID: c.ID, Generation: gen, PassID: c.LastAuditPassID})
		prerequisiteRows(t, 1, n, err)
	}
	mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", f.closure.ID)
	race(true)
	c := prerequisiteRead(t, ctx, f)
	if c.CleanupAttempts != 8 {
		t.Fatalf("cleanup ceiling reservation: %+v", c)
	}
	n, err = q.RecordPrerequisiteCleanupRetry(ctx, store.RecordPrerequisiteCleanupRetryParams{ID: c.ID, Generation: 7, PassID: c.LastAuditPassID})
	prerequisiteRows(t, 0, n, err)
	n, err = q.RecordPrerequisiteCleanupRetry(ctx, store.RecordPrerequisiteCleanupRetryParams{ID: c.ID, Generation: 8, PassID: c.LastAuditPassID})
	prerequisiteRows(t, 1, n, err)
	if c = prerequisiteRead(t, ctx, f); c.State != "pinned" || !c.LiveRunID.Valid || !c.CleanupExhaustedAt.Valid {
		t.Fatalf("cleanup ceiling custody: %+v", c)
	}
}

func TestPrerequisiteClosureAttemptEightLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_eight_")
	q := store.New(pool)
	for _, mode := range []string{"budget", "ancestry", "crash"} {
		t.Run(mode, func(t *testing.T) {
			f := prerequisiteNew(t, ctx, pool)
			for gen := int32(1); gen <= 8; gen++ {
				mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", f.closure.ID)
				lease := 60.0
				if mode == "crash" && gen == 8 {
					lease = 30
				}
				c := prerequisiteReserve(t, ctx, f, lease)
				if c.Attempts != gen {
					t.Fatalf("generation=%d want=%d", c.Attempts, gen)
				}
				if gen == 8 && mode == "crash" {
					break
				}
				n, err := q.RecordPrerequisitePreparationRetry(ctx, store.RecordPrerequisitePreparationRetryParams{ID: c.ID, Generation: gen, BudgetShortfall: mode == "budget"})
				prerequisiteRows(t, 1, n, err)
			}
			if mode == "crash" {
				prerequisiteAdvance(t, ctx, pool, time.Minute)
				n, err := q.ExpirePrerequisitePreparation(ctx, store.ExpirePrerequisitePreparationParams{ID: f.closure.ID, Generation: 7})
				prerequisiteRows(t, 0, n, err)
				n, err = q.ExpirePrerequisitePreparation(ctx, store.ExpirePrerequisitePreparationParams{ID: f.closure.ID, Generation: 8})
				prerequisiteRows(t, 1, n, err)
			}
			c := prerequisiteRead(t, ctx, f)
			reason := "prerequisite_ancestry_unknown"
			if mode == "budget" {
				reason = "prerequisite_prepare_budget_exhausted"
			}
			if c.State != "refused" || c.Reason != reason || c.LiveRunID.Valid || c.PinTip.Valid {
				t.Fatalf("attempt eight: %+v", c)
			}
		})
	}
	for _, crash := range []bool{false, true} {
		f := prerequisiteNew(t, ctx, pool)
		prerequisitePin(t, ctx, f)
		mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='discarded' WHERE id=$1", f.capture)
		for gen := int32(1); gen <= 8; gen++ {
			mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", f.closure.ID)
			lease := 60.0
			if crash && gen == 8 {
				lease = 30
			}
			c, err := q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{PassID: prerequisiteUUID(uuid.New()), ID: f.closure.ID, LeaseSeconds: lease})
			if err != nil || c.CleanupAttempts != gen {
				t.Fatalf("cleanup generation: %+v %v", c, err)
			}
			n, err := q.RecordPrerequisiteDeleted(ctx, store.RecordPrerequisiteDeletedParams{PassID: prerequisiteRead(t, ctx, f).LastAuditPassID, ID: c.ID, Generation: gen - 1})
			prerequisiteRows(t, 0, n, err)
			if crash && gen == 8 {
				break
			}
			n, err = q.RecordPrerequisiteCleanupRetry(ctx, store.RecordPrerequisiteCleanupRetryParams{PassID: prerequisiteRead(t, ctx, f).LastAuditPassID, ID: c.ID, Generation: gen, LastError: "unavailable"})
			prerequisiteRows(t, 1, n, err)
		}
		if crash {
			prerequisiteAdvance(t, ctx, pool, time.Minute)
			n, err := q.ExpirePrerequisiteCleanup(ctx, store.ExpirePrerequisiteCleanupParams{PassID: prerequisiteRead(t, ctx, f).LastAuditPassID, ID: f.closure.ID, Generation: 8})
			prerequisiteRows(t, 1, n, err)
		}
		c := prerequisiteRead(t, ctx, f)
		if c.State != "pinned" || !c.LiveRunID.Valid || !c.CleanupExhaustedAt.Valid || c.Reason != "pin_cleanup_exhausted" {
			t.Fatalf("cleanup exhaustion: %+v", c)
		}
		if _, err := q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{PassID: prerequisiteUUID(uuid.New()), ID: c.ID, LeaseSeconds: 60}); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("ninth cleanup: %v", err)
		}
		prerequisiteAdvance(t, ctx, pool, 2*time.Hour)
		pass := prerequisiteUUID(uuid.New())
		if _, err := q.ReservePrerequisiteAudit(ctx, store.ReservePrerequisiteAuditParams{ID: c.ID, PassID: pass}); err != nil {
			t.Fatal(err)
		}
		n, err := q.DeferPrerequisiteCleanupListing(ctx, store.DeferPrerequisiteCleanupListingParams{
			ID: c.ID, Generation: 8, PassID: pass, LastError: strings.Repeat("x", 600)})
		prerequisiteRows(t, 1, n, err)
		exhausted := prerequisiteRead(t, ctx, f)
		if exhausted.NextAttemptAt.Time.Sub(prerequisiteNow(t, ctx, pool)) != time.Hour || exhausted.CleanupAttempts != 8 ||
			!exhausted.LiveRunID.Valid || len(exhausted.LastError) != 512 {
			t.Fatalf("exhausted listing backoff: %+v", exhausted)
		}
		n, err = q.ConfirmPrerequisiteCleanupAbsent(ctx, store.ConfirmPrerequisiteCleanupAbsentParams{
			ID: c.ID, Generation: 8, PassID: uuid.UUID(pass.Bytes)})
		prerequisiteRows(t, 1, n, err)
		if got := prerequisiteRead(t, ctx, f); got.State != "deleted" || got.LiveRunID.Valid {
			t.Fatalf("exhausted absence: %+v", got)
		}
	}
}

func prerequisiteSchemaVersion(t *testing.T, ctx context.Context, pool *pgxpool.Pool, expected int64) {
	t.Helper()
	applied, head, atHead, err := store.SchemaVersionStatus(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if applied != expected || head != 387 || atHead != (expected == 387) {
		t.Fatalf("schema version: applied=%d head=%d atHead=%t; want applied=%d head=387 atHead=%t",
			applied, head, atHead, expected, expected == 387)
	}
}

func TestPrerequisiteClosureMigrationRoundTripLiveDB(t *testing.T) {
	ctx, dsn, pool := prerequisiteMigratedPool(t, "prereq_roundtrip_")
	// Empty Down/Up, then dependent evidence blocks without CASCADE.
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	if err := store.MigrateDownTo(ctx, dsn, 386); err != nil {
		t.Fatal(err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 386)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	mustExec(ctx, t, pool, "CREATE VIEW prerequisite_closure_dependency AS SELECT id FROM recovery_prerequisite_closures")
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	if err := store.MigrateDownTo(ctx, dsn, 386); err == nil {
		t.Fatal("Down removed dependent evidence")
	}
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	mustExec(ctx, t, pool, "DROP VIEW prerequisite_closure_dependency")
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	if err := store.MigrateDownTo(ctx, dsn, 386); err != nil {
		t.Fatal(err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 386)
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	for _, state := range []string{"pending", "invoked", "create_unknown", "pinned", "refused", "foreign", "lost", "deleted"} {
		t.Run(state, func(t *testing.T) {
			// A fresh database prevents an earlier pending row masking later states.
			ctx, dsn, pool := prerequisiteMigratedPool(t, "prereq_down_state_")
			f := prerequisiteNew(t, ctx, pool)
			prerequisiteSetState(t, ctx, f, state)
			prerequisiteSchemaVersion(t, ctx, pool, 387)
			if err := store.MigrateDownTo(ctx, dsn, 386); err == nil {
				t.Fatalf("Down forgot %s evidence", state)
			}
			prerequisiteSchemaVersion(t, ctx, pool, 387)
			if c := prerequisiteRead(t, ctx, f); c.State != state {
				t.Fatalf("failed Down changed %s to %s", state, c.State)
			}
			if err := store.Migrate(ctx, dsn); err != nil {
				t.Fatal(err)
			}
			prerequisiteSchemaVersion(t, ctx, pool, 387)
		})
	}
}

func TestPrerequisitePendingForeignLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_foreign_")
	f := prerequisiteNew(t, ctx, pool)
	prerequisiteReserve(t, ctx, f, 60)
	n, err := store.New(pool).RefusePrerequisitePreparation(ctx, store.RefusePrerequisitePreparationParams{
		ID: f.closure.ID, Generation: 1, Reason: "pin_foreign"})
	prerequisiteRows(t, 1, n, err)
	if c := prerequisiteRead(t, ctx, f); c.State != "refused" || c.Reason != "pin_foreign" || c.InvokedAt.Valid || c.LiveRunID.Valid {
		t.Fatalf("preexisting target: %+v", c)
	}
}

func TestPrerequisiteCleanupListingFencesLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_fences_")
	q := store.New(pool)
	// Initial absence is sufficient only once the archive is ineligible.
	f := prerequisiteNew(t, ctx, pool)
	prerequisitePin(t, ctx, f)
	pass := prerequisiteAudit(t, ctx, f)
	args := store.ConfirmPrerequisiteCleanupAbsentParams{ID: f.closure.ID, Generation: 0, PassID: uuid.UUID(pass.Bytes)}
	n, err := q.ConfirmPrerequisiteCleanupAbsent(ctx, args)
	prerequisiteRows(t, 0, n, err)
	_, err = pool.Exec(ctx, "UPDATE recovery_prerequisite_closures SET state='deleted',live_run_id=NULL WHERE id=$1", f.closure.ID)
	prerequisiteError(t, err)
	// The delete outcome must also refuse an active capture, independently of
	// its generation/deadline predicates. This valid-shaped raw reservation is
	// deliberately outside ReservePrerequisiteCleanup's capture filter.
	active := prerequisiteNew(t, ctx, pool)
	prerequisitePin(t, ctx, active)
	activePass := prerequisiteAudit(t, ctx, active)
	mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures SET cleanup_attempts=1,
        cleanup_deadline=now()+interval '1 minute',next_attempt_at=now()+interval '2 minutes'
        WHERE id=$1`, active.closure.ID)
	n, err = q.RecordPrerequisiteDeleted(ctx, store.RecordPrerequisiteDeletedParams{ID: active.closure.ID, Generation: 1, PassID: activePass})
	prerequisiteRows(t, 0, n, err)
	mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='expired' WHERE id=$1", f.capture)
	n, err = q.DeferPrerequisiteCleanupListing(ctx, store.DeferPrerequisiteCleanupListingParams{ID: f.closure.ID, Generation: 0, PassID: pass})
	prerequisiteRows(t, 1, n, err)
	initial := prerequisiteRead(t, ctx, f)
	if initial.CleanupAttempts != 0 || initial.NextAttemptAt.Time.Sub(prerequisiteNow(t, ctx, pool)) != 30*time.Second {
		t.Fatalf("initial listing backoff: %+v", initial)
	}
	n, err = q.ConfirmPrerequisiteCleanupAbsent(ctx, args)
	prerequisiteRows(t, 1, n, err)
	if c := prerequisiteRead(t, ctx, f); c.CleanupAttempts != 0 || c.LiveRunID.Valid || c.State != "deleted" {
		t.Fatalf("initial absent: %+v", c)
	}

	for _, outcome := range []string{"absence", "foreign", "cleanup_absent", "deleted", "retry", "expire"} {
		t.Run(outcome, func(t *testing.T) {
			f := prerequisiteNew(t, ctx, pool)
			prerequisitePin(t, ctx, f)
			oldPass := prerequisiteAudit(t, ctx, f)
			mustExec(ctx, t, pool, "UPDATE recovery_captures SET state='discarded' WHERE id=$1", f.capture)
			latestPass := prerequisiteUUID(uuid.New())
			c, err := q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{ID: f.closure.ID, PassID: latestPass, LeaseSeconds: 60})
			if err != nil {
				t.Fatal(err)
			}
			// The old audit generation and pass cannot overwrite the cleanup reservation.
			n, err := q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: c.ID, Generation: 0, PassID: oldPass})
			prerequisiteRows(t, 0, n, err)
			n, err = q.RecordPrerequisitePinnedForeign(ctx, store.RecordPrerequisitePinnedForeignParams{ID: c.ID, Generation: 0, PassID: oldPass})
			prerequisiteRows(t, 0, n, err)
			n, err = q.ConfirmPrerequisiteCleanupAbsent(ctx, store.ConfirmPrerequisiteCleanupAbsentParams{ID: c.ID, Generation: 0, PassID: uuid.UUID(oldPass.Bytes)})
			prerequisiteRows(t, 0, n, err)
			// Current generation with an old audit reservation is also stale.
			n, err = q.RecordPrerequisitePinnedForeign(ctx, store.RecordPrerequisitePinnedForeignParams{ID: c.ID, Generation: 1, PassID: oldPass})
			prerequisiteRows(t, 0, n, err)
			n, err = q.RecordPrerequisiteDeleted(ctx, store.RecordPrerequisiteDeletedParams{ID: c.ID, Generation: 1, PassID: oldPass})
			prerequisiteRows(t, 0, n, err)
			n, err = q.RecordPrerequisiteCleanupRetry(ctx, store.RecordPrerequisiteCleanupRetryParams{ID: c.ID, Generation: 1, PassID: oldPass})
			prerequisiteRows(t, 0, n, err)
			// A listing cannot settle absence while another delete lease is live.
			n, err = q.ConfirmPrerequisiteCleanupAbsent(ctx, store.ConfirmPrerequisiteCleanupAbsentParams{ID: c.ID, Generation: 1, PassID: uuid.UUID(latestPass.Bytes)})
			prerequisiteRows(t, 0, n, err)
			prerequisiteAdvance(t, ctx, pool, 2*time.Minute)
			mustExec(ctx, t, pool, "UPDATE recovery_prerequisite_closures SET next_attempt_at=now() WHERE id=$1", c.ID)
			newerPass := prerequisiteUUID(uuid.New())
			_, err = q.ReservePrerequisiteCleanup(ctx, store.ReservePrerequisiteCleanupParams{ID: c.ID, PassID: newerPass, LeaseSeconds: 60})
			if err != nil {
				t.Fatal(err)
			}
			switch outcome {
			case "absence":
				n, err = q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: c.ID, Generation: 1, PassID: latestPass})
			case "foreign":
				n, err = q.RecordPrerequisitePinnedForeign(ctx, store.RecordPrerequisitePinnedForeignParams{ID: c.ID, Generation: 1, PassID: latestPass})
			case "cleanup_absent":
				n, err = q.ConfirmPrerequisiteCleanupAbsent(ctx, store.ConfirmPrerequisiteCleanupAbsentParams{ID: c.ID, Generation: 1, PassID: uuid.UUID(latestPass.Bytes)})
			case "deleted":
				n, err = q.RecordPrerequisiteDeleted(ctx, store.RecordPrerequisiteDeletedParams{ID: c.ID, Generation: 1, PassID: latestPass})
			case "retry":
				n, err = q.RecordPrerequisiteCleanupRetry(ctx, store.RecordPrerequisiteCleanupRetryParams{ID: c.ID, Generation: 1, PassID: latestPass})
			case "expire":
				n, err = q.ExpirePrerequisiteCleanup(ctx, store.ExpirePrerequisiteCleanupParams{ID: c.ID, Generation: 1, PassID: latestPass})
			}
			prerequisiteRows(t, 0, n, err)
			if c := prerequisiteRead(t, ctx, f); c.State != "pinned" || !c.LiveRunID.Valid || c.CleanupAttempts != 2 {
				t.Fatalf("stale result changed row: %+v", c)
			}
		})
	}
}

func TestPrerequisiteAuditReservationFencesLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_audit_fence_")
	q := store.New(pool)
	for _, candidate := range []bool{false, true} {
		f := prerequisiteNew(t, ctx, pool)
		prerequisitePin(t, ctx, f)
		old := prerequisiteAudit(t, ctx, f)
		if candidate {
			n, err := q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: old})
			prerequisiteRows(t, 1, n, err)
			prerequisiteElapsedAbsence(t, ctx, f)
			old = prerequisiteAudit(t, ctx, f)
		}
		current := prerequisiteAudit(t, ctx, f)
		n, err := q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: old})
		prerequisiteRows(t, 0, n, err)
		n, err = q.ClearPrerequisiteAbsence(ctx, store.ClearPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: old})
		prerequisiteRows(t, 0, n, err)
		n, err = q.ConfirmPrerequisiteLost(ctx, store.ConfirmPrerequisiteLostParams{ID: f.closure.ID, PassID: old})
		prerequisiteRows(t, 0, n, err)
		n, err = q.RecordPrerequisitePinnedForeign(ctx, store.RecordPrerequisitePinnedForeignParams{ID: f.closure.ID, PassID: old})
		prerequisiteRows(t, 0, n, err)
		if candidate {
			n, err = q.ClearPrerequisiteAbsence(ctx, store.ClearPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: current})
		} else {
			n, err = q.RecordPrerequisiteAbsence(ctx, store.RecordPrerequisiteAbsenceParams{ID: f.closure.ID, PassID: current})
		}
		prerequisiteRows(t, 1, n, err)
	}
}

func TestPrerequisiteReconciliationBackoffLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_backoff_")
	q := store.New(pool)
	f := prerequisiteNew(t, ctx, pool)
	prerequisiteInvoke(t, ctx, f, 60)
	n, err := q.MarkPrerequisiteCreateUnknown(ctx, store.MarkPrerequisiteCreateUnknownParams{ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
	// S4 remains writable long after the preparation lease expired.
	prerequisiteAdvance(t, ctx, pool, time.Hour)
	delays := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, delay := range delays {
		before := prerequisiteNow(t, ctx, pool)
		gen := int32(i)
		if gen > 7 {
			gen = 7
		}
		n, err = q.DeferPrerequisiteReconciliation(ctx, store.DeferPrerequisiteReconciliationParams{
			ID: f.closure.ID, Generation: 1, ReconciliationGeneration: gen, LastError: strings.Repeat("x", 600)})
		prerequisiteRows(t, 1, n, err)
		c := prerequisiteRead(t, ctx, f)
		if c.NextAttemptAt.Time.Sub(before) != delay || c.Attempts != 1 || c.State != "create_unknown" || !c.LiveRunID.Valid || len(c.LastError) != 512 {
			t.Fatalf("retry %d: %+v; delay=%v want=%v", i, c, c.NextAttemptAt.Time.Sub(before), delay)
		}
		n, err = q.DeferPrerequisiteReconciliation(ctx, store.DeferPrerequisiteReconciliationParams{ID: c.ID, Generation: 1, ReconciliationGeneration: gen})
		prerequisiteRows(t, 0, n, err)
		prerequisiteAdvance(t, ctx, pool, delay)
	}
	n, err = q.RecordPrerequisiteForeign(ctx, store.RecordPrerequisiteForeignParams{ID: f.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
	if c := prerequisiteRead(t, ctx, f); c.State != "foreign" || c.LiveRunID.Valid {
		t.Fatalf("S4 outcome: %+v", c)
	}
	pinned := prerequisiteNew(t, ctx, pool)
	prerequisiteInvoke(t, ctx, pinned, 60)
	n, err = q.MarkPrerequisiteCreateUnknown(ctx, store.MarkPrerequisiteCreateUnknownParams{ID: pinned.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
	prerequisiteAdvance(t, ctx, pool, time.Hour)
	n, err = q.RecordPrerequisitePinned(ctx, store.RecordPrerequisitePinnedParams{ID: pinned.closure.ID, Generation: 1})
	prerequisiteRows(t, 1, n, err)
}

func prerequisiteNow(t *testing.T, ctx context.Context, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(ctx, "SELECT now()").Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func TestPrerequisiteDueCursorLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_cursor_")
	q := store.New(pool)
	want := make([]uuid.UUID, 0, 15)
	for i := 0; i < 15; i++ {
		want = append(want, prerequisiteNew(t, ctx, pool).closure.ID)
	}
	sort.Slice(want, func(i, j int) bool { return want[i].String() < want[j].String() })
	args := store.ListPrerequisitePendingDueParams{DueAt: pgtype.Timestamptz{Time: prerequisiteNow(t, ctx, pool), Valid: true},
		AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, Lim: 4}
	got := make([]uuid.UUID, 0, len(want))
	// Four nonempty pages plus the empty end page; no retry or wall-clock wait.
	for page := 0; page < 5; page++ {
		rows, err := q.ListPrerequisitePendingDue(ctx, args)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 4 {
			t.Fatal("page limit ignored")
		}
		for _, c := range rows {
			got = append(got, c.ID)
			args.AfterDueAt = c.NextAttemptAt
			args.AfterID = c.ID
		}
		if len(rows) == 0 {
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("cursor returned %d rows want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cursor first mismatch %d: %s want %s", i, got[i], want[i])
		}
	}
	args.AfterDueAt = pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}
	args.AfterID = uuid.Nil
	rows, err := q.ListPrerequisitePendingDue(ctx, args)
	if err != nil || len(rows) != 4 || rows[0].ID != want[0] {
		t.Fatalf("wrap: %v %v", rows, err)
	}
}

// Capture the exact generated query that successfully executes, then explain
// prepared custom and generic plans for that same SQL and parameter shape.
type prerequisitePlanDB struct {
	store.DBTX
	sql  string
	args []interface{}
}

func (p *prerequisitePlanDB) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	p.sql, p.args = sql, args
	return p.DBTX.Query(ctx, sql, args...)
}
func (p *prerequisitePlanDB) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	p.sql, p.args = sql, args
	return p.DBTX.QueryRow(ctx, sql, args...)
}

func prerequisitePlanLiteral(t *testing.T, v interface{}) string {
	t.Helper()
	switch v := v.(type) {
	case uuid.UUID:
		return "'" + v.String() + "'::uuid"
	case pgtype.UUID:
		return "'" + uuid.UUID(v.Bytes).String() + "'::uuid"
	case int32:
		return fmt.Sprint(v)
	case pgtype.Timestamptz:
		if v.InfinityModifier == pgtype.NegativeInfinity {
			return "'-infinity'::timestamptz"
		}
		return "'" + v.Time.Format(time.RFC3339Nano) + "'::timestamptz"
	default:
		t.Fatalf("unsupported plan parameter %T", v)
		return ""
	}
}

func prerequisiteExplain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args []interface{}, index string, limited bool, alternatives ...string) {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	literals := make([]string, len(args))
	for i, arg := range args {
		literals[i] = prerequisitePlanLiteral(t, arg)
	}
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			if _, err := conn.Exec(ctx, "SET plan_cache_mode="+mode); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, "PREPARE prerequisite_plan AS "+sql); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			err := conn.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE prerequisite_plan("+strings.Join(literals, ",")+")").Scan(&raw)
			if _, deallocateErr := conn.Exec(ctx, "DEALLOCATE prerequisite_plan"); deallocateErr != nil {
				t.Fatal(deallocateErr)
			}
			if err != nil {
				t.Fatal(err)
			}
			var plans []struct{ Plan map[string]interface{} }
			if err = json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
				t.Fatalf("plan JSON: %v", err)
			}
			found := false
			var visit func(map[string]interface{})
			visit = func(node map[string]interface{}) {
				if name, ok := node["Index Name"].(string); ok {
					for _, expected := range append([]string{index}, alternatives...) {
						if name == expected {
							found = true
						}
					}
				}
				if children, ok := node["Plans"].([]interface{}); ok {
					for _, child := range children {
						if node, ok := child.(map[string]interface{}); ok {
							visit(node)
						}
					}
				}
			}
			visit(plans[0].Plan)
			if !found {
				t.Fatalf("normal %s plan omitted %s: %s", mode, index, raw)
			}
			// A generic plan cannot estimate a parameterized LIMIT exactly. Check the
			// executed bound, rather than treating its estimate as a result count.
			if limited && (plans[0].Plan["Node Type"] != "Limit" || plans[0].Plan["Actual Rows"].(float64) > 10) {
				t.Fatalf("unbounded due plan: %s", raw)
			}
		})
	}
	if _, err := conn.Exec(ctx, "RESET plan_cache_mode"); err != nil {
		t.Fatal(err)
	}
}

func TestPrerequisiteQueryPlansLiveDB(t *testing.T) {
	ctx, _, pool := prerequisiteMigratedPool(t, "prereq_plans_")
	background := prerequisiteNew(t, ctx, pool)
	// 4096 rows on one repository: half future pending, half live pinned.
	// The requested repository and due lane are sparse in this normal skew.
	mustExec(ctx, t, pool, `INSERT INTO recovery_captures
        SELECT (jsonb_populate_record(NULL::recovery_captures,to_jsonb(c) ||
            jsonb_build_object('id',gen_random_uuid(),'idempotency_key',gen_random_uuid()::text))).*
        FROM recovery_captures c CROSS JOIN generate_series(1,4096) WHERE c.id=$1`, background.capture)
	mustExec(ctx, t, pool, `INSERT INTO recovery_prerequisite_closures
        (capture_id,hold_id,run_id,user_id,repo_id,checksum,prerequisite_shas,pin_ref,next_attempt_at)
        SELECT c.id,c.hold_id,c.run_id,c.user_id,$2,c.checksum,c.prerequisite_shas,
            'refs/uzi-prereq/'||c.id::text,now()
        FROM recovery_captures c WHERE c.hold_id=$1 AND c.id<>$3`, background.hold, background.repo, background.capture)
	// Select a deterministic half through UUID ordering, with the reservation
	// and marker shape enforced by the unmodified production trigger.
	mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures
        SET attempts=1,attempt_deadline=now()+interval '1 minute',next_attempt_at=now()+interval '2 minutes'
        WHERE id IN (SELECT id FROM recovery_prerequisite_closures WHERE hold_id=$1 ORDER BY id LIMIT 2048)`, background.hold)
	mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures
        SET state='invoked',live_run_id=run_id,invoked_at=now(),invoked_deadline=now()+interval '30 seconds',
            pin_tip=$2,connection_id=(SELECT connection_id FROM repos WHERE id=$3),
            forge_kind='github',normalized_base_url='https://example.com',project_id=1,default_branch='main'
        WHERE hold_id=$1 AND attempts=1`, background.hold, heldTip, background.repo)
	mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures
        SET state='pinned' WHERE hold_id=$1 AND state='invoked'`, background.hold)
	mustExec(ctx, t, pool, `UPDATE recovery_prerequisite_closures
        SET next_attempt_at=now()+interval '1 day' WHERE hold_id=$1`, background.hold)
	target := prerequisiteNew(t, ctx, pool)
	pinned := prerequisiteNew(t, ctx, pool)
	prerequisitePin(t, ctx, pinned)
	mustExec(ctx, t, pool, "ANALYZE recovery_prerequisite_closures")
	mustExec(ctx, t, pool, "ANALYZE recovery_captures")
	mustExec(ctx, t, pool, "ANALYZE runs")
	mustExec(ctx, t, pool, "ANALYZE repos")
	mustExec(ctx, t, pool, "ANALYZE forge_connections")
	capture := &prerequisitePlanDB{DBTX: pool}
	q := store.New(capture)
	rows, err := q.ListPrerequisitePendingDue(ctx, store.ListPrerequisitePendingDueParams{
		DueAt:      pgtype.Timestamptz{Time: prerequisiteNow(t, ctx, pool), Valid: true},
		AfterDueAt: pgtype.Timestamptz{InfinityModifier: pgtype.NegativeInfinity, Valid: true}, Lim: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != target.closure.ID {
		t.Fatalf("sparse due query: %v %v", rows, err)
	}
	prerequisiteExplain(t, ctx, pool, capture.sql, capture.args, "prerequisite_closure_pending_due", true)
	n, err := q.CountLivePrerequisiteClosuresForRepo(ctx, store.CountLivePrerequisiteClosuresForRepoParams{
		RepoID: pinned.repo, UserID: pinned.user})
	prerequisiteRows(t, 1, n, err)
	prerequisiteExplain(t, ctx, pool, capture.sql, capture.args, "prerequisite_closure_live_repo", false)
}

// The uncommitted S0 row is invisible to an unlocked emptiness check. Down must
// wait before that check, rather than discover the row only when DROP takes its lock.
func TestPrerequisiteClosureDownConcurrentInsertLiveDB(t *testing.T) {
	ctx, dsn, pool := prerequisiteMigratedPool(t, "prereq_down_race_")
	f := heldNewHoldFixture(ctx, t, pool, true, "failed", nil)
	capture := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO recovery_captures
		(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,
		 state,manifest_bound,byte_size,checksum,chunk_count,prerequisite_shas,expires_at)
		VALUES($1,$2,$3,$4,$5,'held-worker',repeat('a',40),$1::uuid::text,'available',true,3,repeat('f',64),1,ARRAY[repeat('b',40)],now()+interval '1 day')`,
		capture, f.hold, f.run, f.user, f.worker)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = store.New(tx).InsertPrerequisiteClosure(ctx, capture); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	go func() { done <- store.MigrateDownTo(bounded, dsn, 386) }()
	joined := false
	defer func() {
		_ = tx.Rollback(ctx)
		if !joined {
			if err := <-done; err != nil {
				t.Errorf("join Down after rollback: %v", err)
			}
		}
	}()
	for {
		select {
		case err = <-done:
			joined = true
			t.Fatalf("Down finished before S0 committed: %v", err)
		default:
		}
		var waiting bool
		err = pool.QueryRow(bounded, `SELECT EXISTS (
			SELECT 1 FROM pg_locks l JOIN pg_class c ON c.oid=l.relation
			WHERE c.relname='recovery_prerequisite_closures' AND l.mode='AccessExclusiveLock' AND NOT l.granted
			AND l.database=(SELECT oid FROM pg_database WHERE datname=current_database()))`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		runtime.Gosched()
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	joined = true
	if err == nil || !strings.Contains(err.Error(), "rollback refused") {
		t.Fatalf("Down failed to preserve concurrent S0 evidence: %v", err)
	}
	prerequisiteSchemaVersion(t, ctx, pool, 387)
	c, err := store.New(pool).GetPrerequisiteClosure(ctx, store.GetPrerequisiteClosureParams{CaptureID: capture, UserID: f.user})
	if err != nil || c.State != "pending" {
		t.Fatalf("concurrent S0 evidence lost: state=%s err=%v", c.State, err)
	}
}
