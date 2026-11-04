package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3859: the claimed-never-started cohort locks the owning worker, then waits on the
// qualifying run rows. A running state report holds such a run row (GetRunOwnedByWorkerForUpdate)
// and updates it more than once; the second update re-checks the runs.worker_id FK and takes
// FOR KEY SHARE on the worker. A FOR UPDATE worker lock conflicts with that and closed a
// deadlock cycle (SQLSTATE 40P01); FOR NO KEY UPDATE does not.
//
// The interleaving is forced, not raced: session A holds the run row and has updated it once;
// session B (the real SweepClaimedNeverStartedCohort) is observed blocked on A in
// pg_stat_activity; only then does A update the run a second time and commit.
func TestClaimedNeverStartedCohortRunningReportLockOrderLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	f := newCNCFixture(t, env, 5)
	run, _ := f.qualifying(t, 1)

	ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
	defer cancel()

	a, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Rollback(context.Background()) }()
	if _, err := a.Exec(ctx, "SELECT id FROM runs WHERE id=$1 FOR UPDATE", run); err != nil {
		t.Fatal(err)
	}
	// First update: the FK column is untouched and the tuple is not ours yet, so no worker lock.
	if _, err := a.Exec(ctx, "UPDATE runs SET plan_md = plan_md WHERE id=$1", run); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		now := time.Now()
		_, err := store.New(env.pool).SweepClaimedNeverStartedCohort(ctx, store.SweepClaimedNeverStartedCohortParams{
			Cutoff: pgconv.Time(now.Add(-time.Minute)), WitnessCutoff: pgconv.Time(now.Add(-2 * time.Minute)),
			WitnessWindowSeconds: 120, MaxRequeues: 5,
		})
		done <- err
	}()
	// Unblock and join even if an assertion below fails.
	joined := false
	defer func() {
		_ = a.Rollback(context.Background())
		if !joined {
			<-done
		}
	}()

	apid := a.Conn().PgConn().PID()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var blocked bool
		if err := env.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, apid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cohort never blocked on the running report's run lock: the interleaving was not forced")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Second update: the tuple is now this transaction's own, so the FK is re-checked and
	// FOR KEY SHARE is taken on the worker the cohort has locked.
	_, aErr := a.Exec(ctx, "UPDATE runs SET plan_md = plan_md WHERE id=$1", run)
	if aErr == nil {
		aErr = a.Commit(ctx)
	}
	bErr := <-done
	joined = true
	for who, err := range map[string]error{"running report": aErr, "cohort sweep": bErr} {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "40P01" {
			t.Fatalf("%s hit a deadlock: %v", who, err)
		}
		if err != nil {
			t.Fatalf("%s: %v", who, err)
		}
	}
}
