package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// #2705 M2: the two ephemeral provisioning triggers must not count, as demand, a run that
// ClaimRun is holding for its returning worker (the "effective pin": the stale-requeue grace AND
// the affinity ceiling AND the owner row still existing). Against a REAL Postgres; skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway one (e2e/run-store-it.sh provides it).
//
// Every case runs against BOTH ListUnplaceableQueuedRunsForEphemeral and
// ListSaturationQueuedRunsForEphemeral. No real waits: status_since / updated_at are back-dated
// with plain UPDATEs and the cutoffs are computed from one fixed evaluation time.
//
// The queued run is inserted in the state RequeueRunsOfStaleWorkers leaves behind (queued,
// worker_id kept, stale_requeue_generation = claim_generation); that sweep's own stamping is
// covered by claim_stale_requeue_grace_livedb_test.go, and the sweep is a global one this file
// avoids.

// ephemeralPinKind is one trigger query plus the fixture it needs to select an unpinned run.
type ephemeralPinKind struct {
	name string
	// caps is the run's required_capabilities. The gap query needs a requirement nothing online
	// satisfies; the saturation query takes a zero-capability run.
	caps []string
	// seed adds whatever else the query needs once the owner exists.
	seed func(fx *fleetFixture)
	list func(fx *fleetFixture, evalAt time.Time, grace, ceiling time.Duration) map[uuid.UUID]bool
}

func ephemeralPinCutoffs(evalAt time.Time, grace, ceiling time.Duration) (stale, affinity pgtype.Timestamptz) {
	if grace > 0 {
		stale = pgtype.Timestamptz{Time: evalAt.Add(-grace), Valid: true}
	}
	return stale, pgtype.Timestamptz{Time: evalAt.Add(-ceiling), Valid: true}
}

func ephemeralPinKinds() []ephemeralPinKind {
	return []ephemeralPinKind{
		{
			name: "unplaceable",
			caps: []string{"gh"},
			seed: func(*fleetFixture) {},
			list: func(fx *fleetFixture, evalAt time.Time, grace, ceiling time.Duration) map[uuid.UUID]bool {
				fx.t.Helper()
				stale, affinity := ephemeralPinCutoffs(evalAt, grace, ceiling)
				rows, err := fx.q.ListUnplaceableQueuedRunsForEphemeral(fx.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
					BackgroundGraceCutoff: pgtype.Timestamptz{Time: evalAt.Add(-15 * time.Minute), Valid: true},
					StaleRequeueCutoff:    stale, AffinityCutoff: affinity,
					EphemeralLease: leaseInterval(0), MaxPerUser: 1000, MaxRows: 10000,
					CrossCheckEvaluatedAt:    planCrossCheckTime(evalAt),
					CrossCheckAffinityCutoff: planCrossCheckTime(evalAt.Add(-2 * time.Minute)),
				})
				if err != nil {
					fx.t.Fatalf("ListUnplaceableQueuedRunsForEphemeral: %v", err)
				}
				out := map[uuid.UUID]bool{}
				for _, r := range rows {
					if r.UserID == fx.userID {
						out[r.ID] = true
					}
				}
				return out
			},
		},
		{
			name: "saturation",
			caps: []string{},
			// A capable persistent worker at its run-lane cap: the fleet is saturated.
			seed: func(fx *fleetFixture) {
				c := fx.worker("capable-at-cap", capOf(1), false)
				fx.holdActive(c, 1)
			},
			list: func(fx *fleetFixture, evalAt time.Time, grace, ceiling time.Duration) map[uuid.UUID]bool {
				fx.t.Helper()
				stale, affinity := ephemeralPinCutoffs(evalAt, grace, ceiling)
				rows, err := fx.q.ListSaturationQueuedRunsForEphemeral(fx.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
					BackgroundGraceCutoff: pgtype.Timestamptz{Time: evalAt.Add(-15 * time.Minute), Valid: true},
					StaleRequeueCutoff:    stale, AffinityCutoff: affinity,
					// A 1m debounce: every case's status_since is at least 2m old.
					SaturationDelay: leaseInterval(time.Minute),
					EphemeralLease:  leaseInterval(0), MaxPerUser: 1000, MaxRows: 10000,
					CrossCheckEvaluatedAt:    planCrossCheckTime(evalAt),
					CrossCheckAffinityCutoff: planCrossCheckTime(evalAt.Add(-2 * time.Minute)),
				})
				if err != nil {
					fx.t.Fatalf("ListSaturationQueuedRunsForEphemeral: %v", err)
				}
				out := map[uuid.UUID]bool{}
				for _, r := range rows {
					if r.UserID == fx.userID {
						out[r.ID] = true
					}
				}
				return out
			},
		},
	}
}

func TestEphemeralTriggersExcludeStaleRequeuePinLiveDB(t *testing.T) {
	const (
		grace   = 10 * time.Minute
		ceiling = 30 * time.Minute
	)
	for _, k := range ephemeralPinKinds() {
		t.Run(k.name, func(t *testing.T) {
			fx := newFleetFixture(t)
			optInEphemeral(fx)
			evalAt := time.Now()

			// The previous worker: persistent, heartbeat-stale, offline, so it satisfies neither
			// query's capable-worker test and the run stays selectable when unpinned.
			owner := uuid.New()
			mustExec(fx.ctx, t, fx.pool,
				`INSERT INTO workers (id, user_id, name, token_hash, status, last_heartbeat_at, max_concurrent_runs, docker_enabled)
				 VALUES ($1, $2, 'owner', $3, 'offline', now() - interval '10 minutes', 2, false)`,
				owner, fx.userID, tokenHash())
			k.seed(fx)

			run := uuid.New()
			mustExec(fx.ctx, t, fx.pool,
				`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, required_capabilities, claim_generation, stale_requeue_generation)
				 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', $5, $6, 1, 1)`,
				run, fx.userID, fx.repoID, fx.nextIID(), owner, k.caps)
			// The database is shared across tests: delete the run so it cannot crowd a later
			// run's fixture out of a LIMIT window (the lists above take MaxRows 10000 for the same reason).
			t.Cleanup(func() { _, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id = $1`, run) })

			// pinned restores the effective-pin state: generations match, status_since and
			// updated_at 5m ago (inside the 10m grace and the 30m ceiling).
			pinned := func() {
				t.Helper()
				mustExec(fx.ctx, t, fx.pool,
					`UPDATE runs SET worker_id = $2, claim_generation = 1, stale_requeue_generation = 1,
					        status_since = now() - interval '5 minutes', updated_at = now() - interval '5 minutes' WHERE id = $1`,
					run, owner)
			}
			expect := func(why string, g, c time.Duration, want bool) {
				t.Helper()
				if got := k.list(fx, evalAt, g, c)[run]; got != want {
					t.Fatalf("%s: listed=%v, want %v", why, got, want)
				}
			}

			pinned()
			expect("effectively pinned to a stale persistent owner", grace, ceiling, false)

			// A NULL cutoff (grace 0) is today's behaviour: the run is demand.
			expect("grace 0 (NULL cutoff)", 0, ceiling, true)

			// The grace expired (status_since 11m ago, still inside the ceiling).
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since = now() - interval '11 minutes', updated_at = now() - interval '11 minutes' WHERE id = $1`, run)
			expect("grace expired", grace, ceiling, true)

			// The ceiling is shorter than the grace: updated_at is past it, status_since is not, and
			// ClaimRun lets peers claim, so the run is demand again.
			pinned()
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since = now() - interval '2 minutes', updated_at = now() - interval '6 minutes' WHERE id = $1`, run)
			expect("inside the grace, past a 5m ceiling", grace, 5*time.Minute, true)
			expect("control: the same run inside a 30m ceiling is pinned", grace, ceiling, false)

			// The regression the dba flagged: an ordinary requeued run (NULL or mismatched
			// stale_requeue_generation) with an owner and a recent status_since must stay visible.
			pinned()
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET stale_requeue_generation = NULL WHERE id = $1`, run)
			expect("NULL stale_requeue_generation", grace, ceiling, true)
			pinned()
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
			expect("mismatched stale_requeue_generation", grace, ceiling, true)

			// The owner row is deleted (worker_id is ON DELETE SET NULL): no pin.
			pinned()
			expect("control before the owner row is deleted", grace, ceiling, false)
			mustExec(fx.ctx, t, fx.pool, `DELETE FROM workers WHERE id = $1`, owner)
			expect("owner row deleted", grace, ceiling, true)
		})
	}
}
