package workersvc

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// waitForNamedLockWaiter polls pg_stat_activity until a backend with the given application_name is
// waiting on a heavyweight lock, so the test proceeds only once the writer is blocked on the run
// row (no sleeps standing in for ordering).
func waitForNamedLockWaiter(t *testing.T, f *publicationResumeFixture, app string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx,
			`SELECT count(*) FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock'`, app).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend named %q reached a Lock wait", app)
}

// A state report reads "not enrolled" before it locks the run, so it takes no worker lock. If an
// enrollment commits while it waits for the run row, applying the write would be an authority
// write in the wrong lock order (run before worker): SetState must refuse with
// ErrPublicationEnrollmentChanged, write nothing, and succeed on the retry.
func TestPublicationResumeSetStateEnrollmentRaceLiveDB(t *testing.T) {
	cases := []struct {
		name string
		req  func(g int64) StateRequest
	}{
		{"running progress report", func(g int64) StateRequest { return StateRequest{State: "running", ClaimGeneration: &g} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicationResumeFixture(t)
			ctx := f.env.ctx
			// A second service on its own pool, so its backend is identifiable by application_name.
			app := "cdr1324-" + f.runID.String()
			racePool, err := store.OpenPool(ctx, os.Getenv("UZI_TEST_DATABASE_URL"), func(c *pgxpool.Config) {
				c.ConnConfig.RuntimeParams["application_name"] = app
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(racePool.Close)
			svc := New(store.New(racePool), f.env.box, testParams())
			svc.SetVault(f.env.vlt)
			svc.SetTxBeginner(racePool)

			runRow := func() string {
				var s string
				if err := f.env.pool.QueryRow(ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE id=$1`, f.runID).Scan(&s); err != nil {
					t.Fatal(err)
				}
				return s
			}
			before := runRow()

			// The holder enrolls the run the way RegisterPublicationIntent does: worker lock, run
			// lock, opt-in, then the side-table insert, all uncommitted.
			holder, err := f.env.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			hq := store.New(holder)
			if _, err = hq.GetWorkerForUpdate(ctx, f.workerID); err != nil {
				t.Fatal(err)
			}
			if err = hq.OptInRunFences(ctx); err != nil {
				t.Fatal(err)
			}
			var locked string
			if err = holder.QueryRow(ctx, `SELECT id::text FROM runs WHERE id=$1 FOR UPDATE`, f.runID).Scan(&locked); err != nil {
				t.Fatal(err)
			}
			if _, err = hq.EnrollPublicationResume(ctx, store.EnrollPublicationResumeParams{
				RunID: f.runID, ClaimGeneration: 1, WorkerID: f.workerID, Intent: f.intent("agent/issue-1", pubHeadA, nil),
			}); err != nil {
				t.Fatal(err)
			}

			type result struct {
				applied bool
				err     error
			}
			done := make(chan result, 1)
			go func() {
				cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				_, applied, err := svc.SetState(cctx, f.wkr, f.runID, tc.req(1))
				done <- result{applied, err}
			}()
			waitForNamedLockWaiter(t, f, app)
			select {
			case r := <-done:
				t.Fatalf("SetState finished while the enrollment held the run lock: %+v", r)
			default:
			}
			if err = holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-done:
				if !errors.Is(r.err, ErrPublicationEnrollmentChanged) || r.applied {
					t.Fatalf("racing SetState = %+v, want ErrPublicationEnrollmentChanged and not applied", r)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("SetState did not return after the enrollment committed")
			}
			if after := runRow(); after != before {
				t.Fatalf("the refused report changed the run row:\nbefore=%s\nafter =%s", before, after)
			}

			// Retry: now enrolled, SetState takes the worker lock first and applies.
			_, applied, err := svc.SetState(ctx, f.wkr, f.runID, tc.req(1))
			if err != nil || !applied {
				t.Fatalf("retry applied=%v err=%v, want applied", applied, err)
			}
		})
	}
}
