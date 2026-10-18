package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// ownerHoldDemand executes both real demand queries on the caller's connection.
// In particular, q must be store.New(tx) while the NULL fixture holds the runs DDL lock.
func ownerHoldDemand(ctx context.Context, q *store.Queries, at time.Time) (map[uuid.UUID]bool, map[uuid.UUID]bool, error, error) {
	ts := func(d time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: at.Add(-d), Valid: true}
	}
	gap, gapErr := q.ListUnplaceableQueuedRunsForEphemeral(ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		BackgroundGraceCutoff: ts(15 * time.Minute), AffinityCutoff: ts(2 * time.Hour),
		EphemeralLease: leaseInterval(0), MaxPerUser: 1000, MaxRows: 10000,
		CrossCheckEvaluatedAt: ts(0), CrossCheckAffinityCutoff: ts(2 * time.Minute),
	})
	sat, satErr := q.ListSaturationQueuedRunsForEphemeral(ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		BackgroundGraceCutoff: ts(15 * time.Minute), AffinityCutoff: ts(2 * time.Hour),
		SaturationDelay: leaseInterval(time.Minute),
		EphemeralLease:  leaseInterval(0), MaxPerUser: 1000, MaxRows: 10000,
		CrossCheckEvaluatedAt: ts(0), CrossCheckAffinityCutoff: ts(2 * time.Minute),
	})
	gaps, sats := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for _, r := range gap {
		gaps[r.ID] = true
	}
	for _, r := range sat {
		sats[r.ID] = true
	}
	return gaps, sats, gapErr, satErr
}

func TestOwnerHoldNullUpdatedAtDemandLiveDB(t *testing.T) {
	for _, ownerState := range []string{"draining", "requested", "ready", "stopping", "recycling", "fenced"} {
		t.Run(ownerState, func(t *testing.T) {
			fx := newFleetFixture(t)
			optInEphemeral(fx)
			at := time.Now().UTC().Truncate(time.Microsecond)
			owner, capped := uuid.New(), uuid.New()
			gapRun, satRun, active := uuid.New(), uuid.New(), uuid.New()
			t.Cleanup(func() {
				for _, stmt := range []string{
					"DELETE FROM runs WHERE user_id=$1",
					"DELETE FROM workers WHERE user_id=$1",
					"UPDATE users SET ephemeral_workers_enabled=false WHERE id=$1",
				} {
					if _, err := fx.pool.Exec(fx.ctx, stmt, fx.userID); err != nil {
						t.Errorf("cleanup: %v", err)
					}
				}
			})
			phase := ownerState
			if ownerState == "draining" || ownerState == "fenced" {
				phase = ""
			}
			var draining any
			if ownerState == "draining" {
				draining = at.Add(-time.Hour)
			}
			mustExec(fx.ctx, t, fx.pool, `INSERT INTO workers
				(id,user_id,name,token_hash,status,last_heartbeat_at,draining_since,maintenance_phase,
				 maintenance_fenced,created_at,updated_at)
				VALUES ($1,$2,'owner',$3,'offline',$4,$5,$6,$7,$4,$4)`,
				owner, fx.userID, owner[:], at.Add(-10*time.Minute), draining, phase, ownerState == "fenced")
			mustExec(fx.ctx, t, fx.pool, `INSERT INTO workers
				(id,user_id,name,token_hash,status,last_heartbeat_at,max_concurrent_runs,created_at,updated_at)
				VALUES ($1,$2,'capable-at-cap',$3,'online',$4,1,$4,$4)`, capped, fx.userID, capped[:], at)
			for i, r := range []struct {
				id, worker uuid.UUID
				status     string
				caps       []string
			}{
				{gapRun, owner, "queued", []string{"gh"}},
				{satRun, owner, "queued", []string{}},
				{active, capped, "running", []string{}},
			} {
				mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs
					(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,worker_id,
					 required_capabilities,status_since,updated_at,created_at)
					VALUES ($1,$2,$3,$4,'t','d',$5,$6,$7,$8,$9,$8)`,
					r.id, fx.userID, fx.repoID, i+1, r.status, r.worker, r.caps, at.Add(-20*time.Minute), at.Add(-2*time.Hour))
			}

			tx, err := fx.pool.Begin(fx.ctx)
			if err != nil {
				t.Fatal(err)
			}
			// Registered before any DDL: Fatal/error paths roll back too, then verify the
			// original schema from a fresh pool read before fixture cleanup can read runs.
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				if err := tx.Rollback(ctx); err != nil {
					t.Errorf("rollback: %v", err)
				}
				var notNull bool
				err := fx.pool.QueryRow(ctx, `SELECT attnotnull FROM pg_attribute
					WHERE attrelid='runs'::regclass AND attname='updated_at' AND NOT attisdropped`).Scan(&notNull)
				if err != nil || !notNull {
					t.Errorf("updated_at NOT NULL not restored: attnotnull=%v err=%v", notNull, err)
				}
			}()
			if _, err := tx.Exec(fx.ctx, `ALTER TABLE runs ALTER COLUMN updated_at DROP NOT NULL`); err != nil {
				t.Fatal(err)
			}
			q := store.New(tx)
			check := func(label string, want bool) {
				t.Helper()
				gap, sat, gapErr, satErr := ownerHoldDemand(fx.ctx, q, at)
				if gapErr != nil || satErr != nil {
					t.Fatalf("%s: gap err=%v saturation err=%v", label, gapErr, satErr)
				}
				if gap[gapRun] != want || sat[satRun] != want {
					t.Errorf("%s: gap=%v saturation=%v, want both %v", label, gap[gapRun], sat[satRun], want)
				}
			}
			// Positive demand controls leave the owner, capacity and capability fixtures intact.
			check("nonnull exact ceiling held", false)
			if _, err := tx.Exec(fx.ctx, `UPDATE runs SET updated_at=$2 WHERE id=ANY($1)`,
				[]uuid.UUID{gapRun, satRun}, at.Add(-2*time.Hour-time.Microsecond)); err != nil {
				t.Fatal(err)
			}
			check("nonnull one microsecond past ceiling released", true)
			if _, err := tx.Exec(fx.ctx, `UPDATE runs SET updated_at=NULL WHERE id=ANY($1)`,
				[]uuid.UUID{gapRun, satRun}); err != nil {
				t.Fatal(err)
			}
			check("NULL updated_at is demand (NULL-safe hold exclusion)", true)
		})
	}
}

func TestOwnerHoldReasonsExcludedFromAlarmQueriesLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	at := time.Now().UTC().Truncate(time.Microsecond)
	reasons := []any{
		"waiting until " + at.Format(time.RFC3339) + " for its previous worker previous-owner to finish draining or maintenance; another worker may take it after that",
		"waiting until " + at.Format(time.RFC3339) + " for its previous worker to finish draining or maintenance; another worker may take it after that",
		"no worker is online to pick up this run",
		nil,
	}
	ids := make([]uuid.UUID, len(reasons))
	t.Cleanup(func() {
		if _, err := fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id=ANY($1)`, ids); err != nil {
			t.Errorf("cleanup alarms: %v", err)
		}
	})
	for i, reason := range reasons {
		ids[i] = uuid.New()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO runs
			(id,user_id,repo_id,issue_iid,issue_title,issue_description,status,health,health_reason,
			 health_since,status_since,updated_at,created_at)
			VALUES ($1,$2,$3,$4,'t','d','queued','waiting_worker',$5,$6,$6,$6,$6)`,
			ids[i], fx.userID, fx.repoID, i+1, reason, at.Add(-3*time.Hour))
	}
	waiting, err := fx.q.ListWaitingWorkerRuns(fx.ctx)
	if err != nil {
		t.Fatal(err)
	}
	capacity, err := fx.q.ListOwnersWaitingNoCapacity(fx.ctx, store.ListOwnersWaitingNoCapacityParams{
		HeartbeatCutoff: pgtype.Timestamptz{Time: at.Add(-45 * time.Second), Valid: true},
		RollReason:      "workers are upgrading",
	})
	if err != nil {
		t.Fatal(err)
	}
	gotWaiting, gotCapacity := map[uuid.UUID]bool{}, map[uuid.UUID]bool{}
	for _, r := range waiting {
		gotWaiting[r.RunID] = true
	}
	for _, r := range capacity {
		gotCapacity[r.RunID] = true
	}
	for i, id := range ids {
		want := i >= 2
		if gotWaiting[id] != want || gotCapacity[id] != want {
			t.Errorf("reason %v: queue.waiting=%v fleet.capacity=%v, want both %v",
				reasons[i], gotWaiting[id], gotCapacity[id], want)
		}
	}
}
