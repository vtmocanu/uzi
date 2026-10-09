package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCrossCheckAffinityReportsLiveDB(t *testing.T) {
	for _, state := range []string{"failed", "awaiting_input"} {
		t.Run(state, func(t *testing.T) {
			f := laneFixture(t)
			w := wkrRow(t, f.env, f.workerID)
			zero := int64(0)
			question := "question"
			_, applied, _, err := f.svc.SetStateReport(f.env.ctx, w, f.runID,
				StateRequest{State: state, ClaimGeneration: &zero, OpenQuestionID: &question})
			if !errors.Is(err, ErrRunNotOwned) || applied {
				t.Fatalf("unclaimed affinity report: applied=%v err=%v", applied, err)
			}
			child := mustRun(t, f.env, f.runID)
			if child.Status != "queued" || child.ClaimGeneration != 0 {
				t.Fatalf("affinity report mutated child: status=%s generation=%d", child.Status, child.ClaimGeneration)
			}
			// Exercise the worker-scoped writers independently of the service read.
			var n int64
			if state == "failed" {
				n, err = f.env.q.SetRunFailed(f.env.ctx, store.SetRunFailedParams{
					ID: f.runID, WorkerID: pgconv.UUID(w.ID), ClaimGeneration: pgtype.Int8{Int64: 0, Valid: true}})
			} else {
				n, err = f.env.q.SetRunAwaitingInput(f.env.ctx, store.SetRunAwaitingInputParams{
					ID: f.runID, WorkerID: pgconv.UUID(w.ID), OpenQuestionID: pgtype.Text{String: question, Valid: true}})
			}
			if err != nil || n != 0 {
				t.Fatalf("unclaimed direct report: rows=%d err=%v", n, err)
			}
		})
	}
}

func TestCrossCheckAffinityReadersLiveDB(t *testing.T) {
	f := laneFixture(t)
	ctx, q := f.env.ctx, f.env.q
	w := wkrRow(t, f.env, f.workerID)
	workerID := pgconv.UUID(w.ID)
	for _, tc := range []struct {
		name       string
		generation int64
		released   bool
	}{
		{"unclaimed affinity", 0, false},
		{"claimed then requeued", 1, false},
		{"released recovery", 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.env.exec("UPDATE runs SET claim_generation=$2,claim_released_at=CASE WHEN $3 THEN now() ELSE NULL END WHERE id=$1",
				f.runID, tc.generation, tc.released)
			wantOwned := tc.generation > 0
			_, err := q.GetRunOwnedByWorker(ctx, store.GetRunOwnedByWorkerParams{ID: f.runID, WorkerID: workerID})
			if (err == nil) != wantOwned || (err != nil && !errors.Is(err, pgx.ErrNoRows)) {
				t.Fatalf("ownership read: %v, want owned=%v", err, wantOwned)
			}
			_, err = f.svc.runOwnedByWorker(ctx, f.runID, w)
			if (err == nil) != wantOwned || (err != nil && !errors.Is(err, ErrRunNotOwned)) {
				t.Fatalf("service ownership: %v, want owned=%v", err, wantOwned)
			}
			_, err = q.GetRunOwnedByWorkerForUpdate(ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: f.runID, WorkerID: workerID})
			if (err == nil) != wantOwned || (err != nil && !errors.Is(err, pgx.ErrNoRows)) {
				t.Fatalf("locked ownership: %v, want owned=%v", err, wantOwned)
			}
			_, err = q.GetRunForgeConnForWorker(ctx, store.GetRunForgeConnForWorkerParams{RunID: f.runID, WorkerID: workerID})
			if (err == nil) != wantOwned || (err != nil && !errors.Is(err, pgx.ErrNoRows)) {
				t.Fatalf("forge read: %v, want owned=%v", err, wantOwned)
			}
			gaps, err := q.RunMessageGaps(ctx, store.RunMessageGapsParams{
				RunID: f.runID, WorkerID: workerID, ClaimGeneration: tc.generation, Through: 1, Lim: 10})
			if err != nil || (len(gaps) > 0) != (wantOwned && !tc.released) {
				t.Fatalf("message gaps: rows=%d err=%v", len(gaps), err)
			}
			locks, err := q.LockOwnedRunsByIDs(ctx, store.LockOwnedRunsByIDsParams{RunIds: []uuid.UUID{f.runID}, WorkerID: workerID})
			if err != nil || (len(locks) == 1) != wantOwned {
				t.Fatalf("snapshot ownership locks: rows=%d err=%v", len(locks), err)
			}
			n, err := q.UpsertWorkerActiveRun(ctx, store.UpsertWorkerActiveRunParams{
				RunID: f.runID, WorkerID: w.ID, ClaimGeneration: tc.generation, Phase: "running", SnapshotEpoch: 1})
			if err != nil || (n == 1) != (wantOwned && !tc.released) {
				t.Fatalf("snapshot admission: rows=%d err=%v", n, err)
			}
		})
	}
}

func TestCrossCheckLaneSaturationPolicyLiveDB(t *testing.T) {
	f := laneFixture(t)
	f.env.exec("UPDATE runs SET worker_id=$2 WHERE id=$1", f.lead, f.workerID)
	waiting := cloneLaneChild(t, f)
	f.env.exec("UPDATE runs SET status_since=now()-interval '1 hour' WHERE id=$1", waiting)
	f.env.exec("UPDATE users SET ephemeral_workers_enabled=true WHERE id=$1", f.userID)
	// Pin the waiting child behind the first child, then consume the only lane slot.
	f.env.exec("UPDATE runs SET created_at=now()-interval '2 hours' WHERE id=$1", f.runID)
	f.claimed(t, laneClaim(t, f, "cross_check", nil))
	now := time.Now()
	gap, err := f.env.q.ListUnplaceableQueuedRunsForEphemeral(f.env.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		MaxRows: 100, MaxPerUser: 100, CrossCheckEvaluatedAt: pgconv.Time(now),
		CrossCheckAffinityCutoff: pgconv.Time(now.Add(-2 * time.Minute)),
		CodexCuratedModels:       []string{customCodexModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range gap {
		if row.ID == waiting {
			t.Fatal("full capable lane became a capability gap")
		}
	}
	sat, err := f.env.q.ListSaturationQueuedRunsForEphemeral(f.env.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		MaxRows: 100, MaxPerUser: 100, SaturationDelay: pgtype.Interval{Valid: true},
		CrossCheckEvaluatedAt: pgconv.Time(now), CrossCheckAffinityCutoff: pgconv.Time(now.Add(-2 * time.Minute)),
		CodexCuratedModels: []string{customCodexModel},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range sat {
		if row.ID == waiting {
			return
		}
	}
	t.Fatal("full capable lane missing from saturation")
}

func TestCrossCheckAffinityInputReceiptsLiveDB(t *testing.T) {
	f := laneFixture(t)
	f.env.exec("UPDATE workers SET protocol_capabilities=protocol_capabilities || $2::text[] WHERE id=$1",
		f.workerID, []string{capability.InputReceiptsV1, capability.InputInclusionV1})
	w := wkrRow(t, f.env, f.workerID)
	var id int64
	if err := f.env.pool.QueryRow(f.env.ctx,
		"INSERT INTO run_user_inputs (run_id,kind,body,consumed_at,consumed_claim_generation,consumed_worker_id) VALUES ($1,'follow_up','steering',now(),0,$2) RETURNING id",
		f.runID, w.ID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.IncludeInputs(f.env.ctx, w, f.runID, 0, []int64{id})
	if err != nil || res.Active || res.Reason != ReceiptStale {
		t.Fatalf("unclaimed inclusion: %+v %v", res, err)
	}
	_, err = f.svc.ApplyInputs(f.env.ctx, w, f.runID, 0, []int64{id})
	if !errors.Is(err, ErrInputReceiptConflict) {
		t.Fatalf("unclaimed settlement: %v", err)
	}
	var stamped bool
	if err := f.env.pool.QueryRow(f.env.ctx,
		"SELECT included_at IS NOT NULL OR applied_at IS NOT NULL FROM run_user_inputs WHERE id=$1", id).Scan(&stamped); err != nil || stamped {
		t.Fatalf("unclaimed receipt stamped input=%v err=%v", stamped, err)
	}
	// Inclusion is intentionally accepted after release for a real historical claim.
	f.env.exec("UPDATE runs SET claim_generation=1,claim_released_at=now() WHERE id=$1", f.runID)
	res, err = f.svc.IncludeInputs(f.env.ctx, w, f.runID, 1, []int64{id})
	if err != nil || !res.Active || len(res.Inputs) != 1 {
		t.Fatalf("released claimed inclusion: %+v %v", res, err)
	}
}
