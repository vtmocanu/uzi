package store_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

const witnessRollbackMessage = "same issue witness audit exists; refusing destructive rollback"
const witnessDiscardSQL = `UPDATE recovery_custody_holds SET state='discarded',
    live_worker_id=NULL,live_run_id=NULL,release_evidence='merged_predecessor_discard',
    merged_predecessor_discard_binding=$2::jsonb WHERE id=$1`

func witnessRollbackFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (heldFixture, uuid.UUID, string) {
	t.Helper()
	f := heldNewHoldFixture(ctx, t, pool, true, "cancelled", nil)
	w := uuid.New()
	mustExec(ctx, t, pool, `UPDATE runs SET issue_iid=101,claim_generation=3,
        finished_at='2025-01-02T03:04:05.123456Z' WHERE id=$1`, f.run)
	mustExec(ctx, t, pool, `INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,
        issue_description,status,claim_generation,mr_iid,created_at)
        VALUES($1,$2,$3,'issue',101,'witness','body','completed',1,7,'2025-01-02T03:04:06.654321Z')`, w, f.user, f.repo)
	var binding string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
        'basis','same_issue_witness','hold_id',$1::uuid,'generation',1,
        'target_run_id',a.id,'target_terminal_generation',a.claim_generation,'target_terminal_status',a.status,
        'witness_run_id',$2::uuid,'witness_completed_generation',1,
        'owner_id',a.user_id,'repo_id',a.repo_id,'issue_iid',a.issue_iid,
        'target_finished_at','2025-01-02T03:04:05.123456Z',
        'witness_created_at','2025-01-02T03:04:06.654321Z',
        'connection_id',c.id,'forge_type',c.forge_type,'base_url',c.base_url,
        'project_id',p.forge_project_id,'witness_mr_iid',7,
        'verified_at','2025-01-02T03:04:07Z')::text
        FROM runs a JOIN repos p ON p.id=a.repo_id JOIN forge_connections c ON c.id=p.connection_id
        WHERE a.id=$3`, f.hold, w, f.run).Scan(&binding); err != nil {
		t.Fatal(err)
	}
	return f, w, binding
}

func witnessSchema(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_object(
        'inventory',pg_get_functiondef('recovery_inventory_hold_guard()'::regprocedure),
        'checks',(SELECT jsonb_agg(pg_get_constraintdef(oid) ORDER BY conname)
            FROM pg_constraint WHERE conrelid='recovery_custody_holds'::regclass AND contype='c'))::text`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func witnessGuard(t *testing.T, ctx context.Context, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, "SELECT prosrc FROM pg_proc WHERE oid='merged_predecessor_discard_guard()'::regprocedure").Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func witnessRetained(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hold uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT jsonb_build_object('hold',(SELECT to_jsonb(h)
        FROM recovery_custody_holds h WHERE id=$1),'versions',(SELECT jsonb_agg(to_jsonb(v) ORDER BY id)
        FROM goose_db_version v),'guard',pg_get_functiondef('merged_predecessor_discard_guard()'::regprocedure))::text`, hold).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func requireWitnessRollbackRefusal(t *testing.T, err error) {
	t.Helper()
	var p *pgconn.PgError
	if !errors.As(err, &p) || p.Code != "P0001" || p.Message != witnessRollbackMessage {
		t.Fatalf("wrong Down refusal: %v", err)
	}
}

func TestCrossRunDiscardRollbackRoundTripLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "witness_roundtrip_")
	if err := store.MigrateTo(ctx, dsn, 385); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	original, schema := witnessGuard(t, ctx, pool), witnessSchema(t, ctx, pool)
	source, err := os.ReadFile("migrations/00338_merged_predecessor_discard.sql")
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(string(source), "CREATE FUNCTION merged_predecessor_discard_guard()", 2)
	if len(first) != 2 {
		t.Fatal("original universal guard absent")
	}
	body := strings.SplitN(strings.SplitN(first[1], "AS $$", 2)[1], "$$;", 2)[0]
	if original != body {
		t.Fatal("385 universal guard differs from exact 338 body")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	if witnessSchema(t, ctx, pool) != schema {
		t.Fatal("Up changed inventory guard or CHECK definitions")
	}
	if !strings.Contains(witnessGuard(t, ctx, pool), "same_issue_witness") {
		t.Fatal("new branch missing")
	}
	if err := store.MigrateDownTo(ctx, dsn, 385); err != nil {
		t.Fatal(err)
	}
	if witnessGuard(t, ctx, pool) != original {
		t.Fatal("Down did not restore exact original universal guard body")
	}
	if witnessSchema(t, ctx, pool) != schema {
		t.Fatal("Down changed inventory guard or CHECK definitions")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	f, _, binding := witnessRollbackFixture(t, ctx, pool)
	tag, err := pool.Exec(ctx, witnessDiscardSQL, f.hold, binding)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("direct database transition: rows=%d err=%v", tag.RowsAffected(), err)
	}
	before := witnessRetained(t, ctx, pool, f.hold)
	requireWitnessRollbackRefusal(t, store.MigrateDownTo(ctx, dsn, 385))
	if witnessRetained(t, ctx, pool, f.hold) != before || witnessSchema(t, ctx, pool) != schema {
		t.Fatal("refused Down changed rows, versions, functions or CHECKs")
	}
}

// Down must wait for the exact writer backend, then see its newly committed audit.
// Every goroutine is joined before pool closure, including failure paths.
func TestCrossRunDiscardRollbackRefusesWitnessLiveDB(t *testing.T) {
	ctx, dsn, pool := heldMigratedPool(t, "witness_down_race_")
	f, _, binding := witnessRollbackFixture(t, ctx, pool)
	bounded, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	tx, err := pool.Begin(bounded)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if tag, err := tx.Exec(bounded, witnessDiscardSQL, f.hold, binding); err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("discard rows=%d err=%v", tag.RowsAffected(), err)
	}
	var writerPID int32
	if err := tx.QueryRow(bounded, "SELECT pg_backend_pid()").Scan(&writerPID); err != nil {
		t.Fatal(err)
	}
	marker := "witness_down_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	// ConnConfig.ConnString returns the original DSN, ignoring RuntimeParams edits.
	// Encode the marker into the actual migration DSN, for URI and keyword forms.
	migrationDSN := dsn + " application_name=" + marker
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		query := parsed.Query()
		query.Set("application_name", marker)
		parsed.RawQuery = query.Encode()
		migrationDSN = parsed.String()
	}
	result := make(chan error, 1)
	go func() { result <- store.MigrateDownTo(bounded, migrationDSN, 385) }()
	joined := false
	defer func() {
		if !joined {
			_ = tx.Rollback(context.Background())
			cancel()
			if cleanupErr := <-result; cleanupErr != nil {
				t.Log("migration returned an error after cleanup cancellation")
			}
		}
	}()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var pid int32
		err := pool.QueryRow(bounded, `SELECT pid FROM pg_stat_activity
            WHERE application_name=$1 AND wait_event_type='Lock'
            AND $2::int=ANY(pg_blocking_pids(pid))
            AND EXISTS (SELECT 1 FROM pg_locks l WHERE l.pid=pg_stat_activity.pid
                AND l.relation='recovery_custody_holds'::regclass
                AND l.mode='AccessExclusiveLock' AND NOT l.granted)`, marker, writerPID).Scan(&pid)
		if err == nil && pid != writerPID {
			break
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case err := <-result:
			joined = true
			t.Fatalf("Down returned before writer committed: %v", err)
		case <-deadline.C:
			t.Fatal("Down did not wait on the exact writer backend")
		case <-tick.C:
		}
	}
	if err := tx.Commit(bounded); err != nil {
		t.Fatal(err)
	}
	err = <-result
	joined = true
	requireWitnessRollbackRefusal(t, err)
	var valid bool
	if err := pool.QueryRow(ctx, `SELECT state='discarded' AND
        merged_predecessor_discard_binding->>'basis'='same_issue_witness'
        FROM recovery_custody_holds WHERE id=$1`, f.hold).Scan(&valid); err != nil || !valid {
		t.Fatalf("audit lost: valid=%v err=%v", valid, err)
	}
}

// Holding A/B row locks elsewhere must not block the universal trigger's reads.
func TestCrossRunDiscardNoTriggerRunLocksLiveDB(t *testing.T) {
	ctx, _, pool := heldMigratedPool(t, "witness_no_run_locks_")
	f, w, binding := witnessRollbackFixture(t, ctx, pool)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	blocker, err := pool.Begin(bounded)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = blocker.Rollback(context.Background()) }()
	rows, err := blocker.Query(bounded, "SELECT id FROM runs WHERE id=ANY($1::uuid[]) ORDER BY id FOR UPDATE", []uuid.UUID{f.run, w})
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	writer, err := pool.Begin(bounded)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(context.Background()) }()
	if _, err := writer.Exec(bounded, "SET LOCAL lock_timeout='250ms'"); err != nil {
		t.Fatal(err)
	}
	tag, err := writer.Exec(bounded, witnessDiscardSQL, f.hold, binding)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("trigger attempted run locking: rows=%d err=%v", tag.RowsAffected(), err)
	}
	if err := writer.Commit(bounded); err != nil {
		t.Fatal(err)
	}
}
