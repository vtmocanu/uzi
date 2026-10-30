package workersvc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The mapping can change after target selection but before the run UPDATE's
// trigger executes. Freeze the admitted tuple, not a later trigger query's view.
func TestForgeOriginAdmissionMappingDuringClaimLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	worker := e.seedWorker(t, nil)
	id := e.fpSeedRunning(t, worker, 1)
	e.exec(t, "UPDATE runs SET status='queued' WHERE id=$1", id)
	otherRepo := uuid.New()
	e.exec(t, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
        SELECT $1,connection_id,forge_project_id+100000,'group/other','https://other.example/group/other','main'
        FROM repos WHERE id=$2`, otherRepo, e.repoID)
	e.exec(t, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at)
        VALUES($1,$2,'https://other.example',now()+interval '1 day','transient_clone_fetch_exhausted',now())`, e.userID, worker)
	blocker, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	claimant, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer claimant.Release()
	key := int64(time.Now().UnixNano())
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%x", uuid.New())
	function, trigger := "forge_claim_barrier_"+suffix, "forge_claim_barrier_"+suffix
	e.exec(t, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
        BEGIN IF NEW.id='%s'::uuid AND NEW.status='claimed' THEN PERFORM pg_advisory_xact_lock(%d); END IF;
        RETURN NEW; END $$`, function, id, key))
	e.exec(t, fmt.Sprintf("CREATE TRIGGER %s BEFORE UPDATE ON runs FOR EACH ROW EXECUTE FUNCTION %s()", trigger, function))
	defer func() {
		e.exec(t, "DROP TRIGGER "+trigger+" ON runs")
		e.exec(t, "DROP FUNCTION "+function+"()")
	}()
	type result struct {
		run store.Run
		err error
	}
	results := make(chan result, 1)
	done := false
	defer func() {
		_, _ = blocker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", key)
		if !done {
			<-results
		}
	}()
	go func() {
		run, err := store.New(claimant).ClaimRun(ctx, e.fpRecoveryClaimParams(worker, time.Now().Add(-45*time.Second)))
		results <- result{run, err}
	}()
	for {
		var blocked bool
		err := e.pool.QueryRow(ctx, "SELECT $1::integer = ANY(pg_blocking_pids($2::integer))",
			int(blocker.Conn().PgConn().PID()), int(claimant.Conn().PgConn().PID())).Scan(&blocked)
		if err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
	}
	// Both writers commit while the claim is at the controlled event barrier.
	e.exec(t, "UPDATE repos SET web_url='https://other.example/group/repo' WHERE id=$1", e.repoID)
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", key); err != nil {
		t.Fatal(err)
	}
	got := <-results
	done = true
	if got.err != nil || got.run.ID != id {
		t.Fatalf("admitted old-origin claim: %s, %v", got.run.ID, got.err)
	}
	if _, err := e.q.GetRunClaimContext(ctx, id); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("mapping changed after admission but payload context was not refused: %v", err)
	}
	var origin string
	if err := e.pool.QueryRow(ctx, "SELECT origin FROM run_forge_origins WHERE run_id=$1", id).Scan(&origin); err != nil || origin != "https://forge.e2e" {
		t.Fatalf("stamp did not retain admitted origin: %q, %v", origin, err)
	}
}
