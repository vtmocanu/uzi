package workersvc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// All coordination waits observe PostgreSQL lock state, never an assumed delay.
// Each async operation is joined even on Fatal, before its connections are closed.
func coordinateAsync(fn func() error) func() error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	var once sync.Once
	var err error
	return func() error { once.Do(func() { err = <-done }); return err }
}

func waitCoordinateLock(t *testing.T, ctx context.Context, env codexTestEnv, pid uint32, kind, mode string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := env.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
   WHERE pid=$1 AND locktype=$2 AND ($3='' OR mode=$3) AND NOT granted
   AND ($2<>'relation' OR relation='runs'::regclass))`, int64(pid), kind, mode).Scan(&waiting)
		if err != nil {
			t.Fatalf("observe %s/%s wait: %v", kind, mode, err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("never observed pid %d waiting for %s/%s: %v", pid, kind, mode, ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestCheckpointCoordinatesCommittedLineageDriftLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	for _, mutation := range []string{"branch", "association"} {
		t.Run(mutation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
			defer cancel()
			f := newCustodyContinuationFixture(t, env)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			root, child, other := uuid.New(), uuid.New(), uuid.New()
			branch := "uzi/task/" + root.String()
			env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,issue_title,issue_description,status)
    VALUES($1,$2,$3,'task',$4,'ancestor','fixture','completed'),
    ($5,$2,$3,'task',$6,'other','fixture','completed')`, root, f.userID, f.repoID, branch, other, "uzi/task/"+other.String())
			env.exec(`INSERT INTO runs(id,user_id,repo_id,kind,branch,then_fix_of_run_id,dispatched_at,issue_title,issue_description,status)
    VALUES($1,$2,$3,'task',$4,$5,now(),'child','fixture','queued')`, child, f.userID, f.repoID, branch, root)
			before := coordinateAuthoritySnapshot(t, env, child)
			schema := "coord_drift_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			ident := pgx.Identifier{schema}.Sanitize()
			env.exec("CREATE SCHEMA " + ident)
			t.Cleanup(func() { env.exec("DROP SCHEMA " + ident + " CASCADE") })
			// Capture the real discovery result BEFORE blocking, then return that old
			// JSON after the mutator commits. Only the first resolver call is delayed.
			env.exec(fmt.Sprintf(`CREATE FUNCTION %s.fn_checkpoint_coordinates(subject public.runs)
    RETURNS jsonb LANGUAGE plpgsql VOLATILE AS $wrapper$
    DECLARE discovered jsonb;
    BEGIN
     discovered := public.fn_checkpoint_coordinates(subject);
     IF current_setting('uzi.coordinate_test_discovered',true) IS DISTINCT FROM 'yes' THEN
      PERFORM set_config('uzi.coordinate_test_discovered','yes',true);
      PERFORM pg_advisory_xact_lock(734352, pg_backend_pid());
     END IF;
     RETURN discovered;
    END $wrapper$`, ident))
			claim, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = claim.Rollback(env.ctx) }()
			q := store.New(claim)
			if _, err := q.GetWorkerForUpdate(ctx, f.workerID); err != nil {
				t.Fatal(err)
			}
			if _, err := claim.Exec(ctx, "SET LOCAL search_path TO "+ident+",public"); err != nil {
				t.Fatal(err)
			}
			mutator, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = mutator.Rollback(env.ctx) }()
			pid := claim.Conn().PgConn().PID()
			if _, err := mutator.Exec(ctx, "SELECT pg_advisory_xact_lock(734352,$1)", int64(pid)); err != nil {
				t.Fatal(err)
			}
			sp, err := claim.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			sq := store.New(sp)
			if err := sq.OptInCheckpointCoordinates(ctx); err != nil {
				t.Fatal(err)
			}
			run, err := sq.ClaimRun(ctx, coordinateClaimParams(f))
			if err != nil || run.ID != child {
				t.Fatalf("claim=%s err=%v", run.ID, err)
			}
			wait := coordinateAsync(func() error {
				_, err := sq.EnrollCheckpointCoordinates(ctx, store.EnrollCheckpointCoordinatesParams{RunID: child, WorkerID: f.workerID, Generation: run.ClaimGeneration})
				return err
			})
			defer func() { cancel(); _ = wait() }()
			waitCoordinateLock(t, ctx, env, pid, "advisory", "ExclusiveLock")
			if mutation == "branch" {
				_, err = mutator.Exec(ctx, "UPDATE runs SET branch='feature/committed-drift' WHERE id=$1", root)
			} else {
				_, err = mutator.Exec(ctx, "UPDATE runs SET then_fix_of_run_id=$2 WHERE id=$1", root, other)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := mutator.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = wait()
			requireCoordinateSQLState(t, err, "40001")
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Message != "checkpoint lineage changed" {
				t.Fatalf("wrong revalidation failure: %v", err)
			}
			if err := sp.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			// Probe the actual worker ROW lock from another connection. A table
			// RowShareLock alone would not prove the row survived savepoint rollback.
			_, err = env.pool.Exec(ctx, "SELECT id FROM workers WHERE id=$1 FOR UPDATE NOWAIT", f.workerID)
			requireCoordinateSQLState(t, err, "55P03")
			if err := claim.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if coordinateAuthoritySnapshot(t, env, child) != before {
				t.Fatal("40001 left claim, descriptor, generation or custody residue")
			}
		})
	}
}

// Only this newly-created database is migrated/downed/dropped. The supplied DSN
// is used as an admin connection; CI and run-store-it use POSTGRES_USER=uzi
// (the image-created superuser). Missing CREATEDB/event-trigger privilege is a
// fixture failure, not a skip or permission to alter the shared database.
func isolatedCoordinateDatabase(t *testing.T) codexTestEnv {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	name := "coord_down_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("isolated database fixture: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE "+ident); err != nil {
			t.Errorf("drop owned database %s: %v", name, err)
		}
	})
	isolatedDSN := dsn + " dbname=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		parsed.Path = "/" + name
		query := parsed.Query()
		query.Del("dbname")
		parsed.RawQuery = query.Encode()
		isolatedDSN = parsed.String()
	}
	marker, err := os.ReadFile("../../../e2e/test-database-marker.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, isolatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := conn.Exec(ctx, string(marker))
	closeErr := conn.Close(ctx)
	if execErr != nil {
		t.Fatal(execErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if err := store.Migrate(ctx, isolatedDSN); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, isolatedDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	box, err := secretbox.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return codexTestEnv{ctx: ctx, pool: pool, q: store.New(pool), box: box, exec: func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("isolated fixture SQL: %v", err)
		}
	}}
}

func coordinateDownProvider(t *testing.T, env codexTestEnv) (*goose.Provider, uint32) {
	t.Helper()
	// A single dedicated connection makes the lock-wait PID unambiguous.
	db := stdlib.OpenDB(*env.pool.Config().ConnConfig)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	var pid uint32
	if err := db.QueryRowContext(env.ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, os.DirFS("../store/migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return provider, pid
}

func TestCheckpointCoordinatesDownAdmissionRaceLiveDB(t *testing.T) {
	for _, order := range []string{"admission first", "Down first"} {
		t.Run(order, func(t *testing.T) {
			env := isolatedCoordinateDatabase(t)
			ctx, cancel := context.WithTimeout(env.ctx, 45*time.Second)
			defer cancel()
			f := newCustodyContinuationFixture(t, env)
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID,
				[]string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1})
			id := seedCoordinateTask(f)
			provider, downPID := coordinateDownProvider(t, env)
			if order == "admission first" {
				admission, err := env.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = admission.Rollback(env.ctx) }()
				q := store.New(admission)
				if _, err := q.GetWorkerForUpdate(ctx, f.workerID); err != nil {
					t.Fatal(err)
				}
				run, err := f.svc.claimCheckpointRun(ctx, admission, coordinateClaimParams(f))
				if err != nil || run.ID != id || len(run.CheckpointCoordinates) == 0 {
					t.Fatalf("enrollment=%s err=%v", run.CheckpointCoordinates, err)
				}
				wait := coordinateAsync(func() error { _, err := provider.ApplyVersion(ctx, 352, false); return err })
				defer func() { cancel(); _ = wait() }()
				waitCoordinateLock(t, ctx, env, downPID, "relation", "AccessExclusiveLock")
				if err := admission.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				err = wait()
				if err == nil || !strings.Contains(err.Error(), "cannot downgrade with enrolled checkpoint coordinates") {
					t.Fatalf("Down failed to see committed enrollment: %v", err)
				}
				got := mustRun(t, env, id)
				if !bytes.Equal(got.CheckpointCoordinates, run.CheckpointCoordinates) || got.ClaimGeneration != 1 ||
					got.WorkerID != run.WorkerID || f.ownOpenHolds(t, id) != 1 {
					t.Fatal("guarded Down changed descriptor/binding/generation/custody")
				}
				if _, err := env.q.ResolveCheckpointCoordinates(ctx, id); err != nil {
					t.Fatal(err)
				}
				return
			}
			// Pause the REAL Down at its first DROP TRIGGER, after its exclusion
			// lock and guard. No production SQL is replaced. This isolated DB alone
			// gets the event trigger, and dropping the database removes it.
			gate, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = gate.Rollback(env.ctx) }()
			if _, err := gate.Exec(ctx, "SELECT pg_advisory_xact_lock(734353,$1)", int64(downPID)); err != nil {
				t.Fatal(err)
			}
			env.exec(`CREATE FUNCTION coordinate_down_pause() RETURNS event_trigger LANGUAGE plpgsql AS $$
    BEGIN PERFORM pg_advisory_xact_lock(734353,pg_backend_pid()); END $$;
    CREATE EVENT TRIGGER coordinate_down_pause ON ddl_command_start
    WHEN TAG IN ('DROP TRIGGER') EXECUTE FUNCTION coordinate_down_pause()`)
			downWait := coordinateAsync(func() error { _, err := provider.ApplyVersion(ctx, 352, false); return err })
			defer func() { cancel(); _ = downWait() }()
			waitCoordinateLock(t, ctx, env, downPID, "advisory", "ExclusiveLock")
			var exclusion bool
			if err := env.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1
    AND relation='runs'::regclass AND mode='AccessExclusiveLock' AND granted)`, int64(downPID)).Scan(&exclusion); err != nil || !exclusion {
				t.Fatalf("Down exclusion not held: %v", err)
			}
			admission, err := env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = admission.Rollback(env.ctx) }()
			q := store.New(admission)
			if _, err := q.GetWorkerForUpdate(ctx, f.workerID); err != nil {
				t.Fatal(err)
			}
			if err := q.OptInCheckpointCoordinates(ctx); err != nil {
				t.Fatal(err)
			}
			admissionPID := admission.Conn().PgConn().PID()
			admissionWait := coordinateAsync(func() error {
				run, err := q.ClaimRun(ctx, coordinateClaimParams(f))
				if err != nil {
					return err
				}
				_, err = q.EnrollCheckpointCoordinates(ctx, store.EnrollCheckpointCoordinatesParams{RunID: run.ID, WorkerID: f.workerID, Generation: run.ClaimGeneration})
				return err
			})
			defer func() { cancel(); _ = admissionWait() }()
			waitCoordinateLock(t, ctx, env, admissionPID, "relation", "")
			var blockedByDown bool
			if err := env.pool.QueryRow(ctx, "SELECT $1=ANY(pg_blocking_pids($2))", int64(downPID), int64(admissionPID)).Scan(&blockedByDown); err != nil || !blockedByDown {
				t.Fatalf("admission not blocked by Down: %v", err)
			}
			if err := gate.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := downWait(); err != nil {
				t.Fatalf("empty isolated Down: %v", err)
			}
			err = admissionWait()
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || (pgerr.Code != "42703" && pgerr.Code != "42883") {
				t.Fatalf("admission after schema removal: %v", err)
			}
			if err := admission.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			var status string
			var generation, holds int64
			var unbound, removed bool
			if err := env.pool.QueryRow(ctx, `SELECT status,claim_generation,worker_id IS NULL,
    (SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1),
    NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='runs'::regclass
      AND attname='checkpoint_coordinates' AND NOT attisdropped)
    FROM runs WHERE id=$1`, id).Scan(&status, &generation, &unbound, &holds, &removed); err != nil {
				t.Fatal(err)
			}
			if status != "queued" || generation != 0 || !unbound || holds != 0 || !removed {
				t.Fatalf("Down-first residue: status=%s gen=%d unbound=%v holds=%d removed=%v", status, generation, unbound, holds, removed)
			}
		})
	}
}
