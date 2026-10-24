package workersvc

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The legacy output was captured by executing FilterProtocol from this SHA.
// Replaying its persisted frame below does not execute an old API process.
func rolloutProtocolCapture(t *testing.T) (advertised, legacy []string) {
	t.Helper()
	data, err := historicalCoordinateFixtures.ReadFile("testdata/task-checkpoint-legacy/legacy-protocol-filter.json")
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		SourceSHA       string   `json:"source_sha"`
		Advertised      []string `json:"advertised"`
		LegacyOutput    []string `json:"legacy_output"`
		CandidateOutput []string `json:"candidate_output"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	if capture.SourceSHA != "03d273fde1901d4185ac7590bae4b6a9f78ef8ea" ||
		!reflect.DeepEqual(capture.Advertised, []string{"task_checkpoint_coordinates_v1", "recovery_archive_v1", "docker", "unknown", "task_checkpoint_coordinates_v1"}) ||
		!reflect.DeepEqual(capture.LegacyOutput, []string{"recovery_archive_v1"}) ||
		!reflect.DeepEqual(capture.CandidateOutput, []string{"task_checkpoint_coordinates_v1", "recovery_archive_v1"}) {
		t.Fatal("historical protocol capture changed")
	}
	if got := capability.FilterProtocol(capture.Advertised); !reflect.DeepEqual(got, capture.CandidateOutput) {
		t.Fatalf("current FilterProtocol=%v want captured candidate output=%v", got, capture.CandidateOutput)
	}
	return capture.Advertised, capture.LegacyOutput
}

func rolloutRegister(t *testing.T, f *custodyContinuationFixture, caps []string, active ...ActiveRunEntry) string {
	t.Helper()
	w, nonce, err := f.svc.Register(f.env.ctx, f.wkr, "rollout-fixture", "base", nil, nil, caps,
		&ActiveSnapshot{Active: active})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := f.env.q.GetWorkerByID(f.env.ctx, f.workerID)
	if err != nil || !reflect.DeepEqual(persisted.ProtocolCapabilities, capability.FilterProtocol(caps)) ||
		!reflect.DeepEqual(w.ProtocolCapabilities, persisted.ProtocolCapabilities) {
		t.Fatalf("Register did not persist filtered protocol frame: worker=%+v err=%v", persisted, err)
	}
	f.wkr = w
	return nonce
}

func rolloutClaim(t *testing.T, f *custodyContinuationFixture, id uuid.UUID, enrolled bool) store.Run {
	t.Helper()
	p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
	if err != nil || p == nil || p.RunID != id.String() || (p.CheckpointCoordinates != nil) != enrolled {
		t.Fatalf("Claim(%s,enrolled=%v)=%+v err=%v", id, enrolled, p, err)
	}
	run := mustRun(t, f.env, id)
	if run.ClaimGeneration != p.ClaimGeneration || run.ClaimGeneration < 1 ||
		!run.WorkerID.Valid || uuid.UUID(run.WorkerID.Bytes) != f.workerID ||
		(len(run.CheckpointCoordinates) != 0) != enrolled {
		t.Fatal("claim response disagrees with persisted generation/worker/enrollment")
	}
	return run
}

func rolloutUnsupportedPublish(t *testing.T, f *custodyContinuationFixture, run store.Run) {
	t.Helper()
	before := coordinateAuthoritySnapshot(t, f.env, run.ID)
	result, err := f.svc.Publish(f.env.ctx, f.wkr, run.ID,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte("unused pack"), &run.ClaimGeneration)
	if err != nil || result.Published || result.Skipped != "unsupported" || result.Ref != "" {
		t.Fatalf("unsupported task Publish=%+v err=%v", result, err)
	}
	if coordinateAuthoritySnapshot(t, f.env, run.ID) != before {
		t.Fatal("unsupported publication changed run or custody")
	}
}

func TestTaskCheckpointRolloutOrdersLiveDB(t *testing.T) {
	advertised, legacy := rolloutProtocolCapture(t)
	for _, order := range []string{"old-api-new-worker-frame", "new-api-old-worker"} {
		for _, upgrade := range []string{"register", "heartbeat"} {
			t.Run(order+"/"+upgrade, func(t *testing.T) {
				f := newCustodyContinuationFixture(t, setupCodexLiveDB(t))
				// Old API/new worker: replay the EXECUTED old filter's output at
				// the persisted registration boundary. New API/old worker sends
				// the same archive-only frame without knowing the new protocol.
				frame := []string{capability.RecoveryArchiveV1}
				if order == "old-api-new-worker-frame" {
					frame = legacy
				}
				nonce := rolloutRegister(t, f, frame)
				id := seedCoordinateTask(f)
				run := rolloutClaim(t, f, id, false)
				rolloutUnsupportedPublish(t, f, run)
				_, applied, err := f.svc.SetState(f.env.ctx, f.wkr, id,
					StateRequest{State: "running", ClaimGeneration: &run.ClaimGeneration})
				if err != nil || !applied {
					t.Fatalf("start legacy generation: applied=%v err=%v", applied, err)
				}
				active := entry(id, run.ClaimGeneration, "running", false)
				if upgrade == "register" {
					nonce = rolloutRegister(t, f, advertised, active)
				} else {
					f.wkr, err = f.svc.Heartbeat(f.env.ctx, f.wkr, nil, nil,
						&ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: nonce, Active: []ActiveRunEntry{active}},
						&ProtocolRefresh{RegisterNonce: nonce, ProtocolCapabilities: advertised, MaxConcurrentRuns: 1})
					if err != nil {
						t.Fatal(err)
					}
				}
				upgraded, err := f.env.q.GetWorkerByID(f.env.ctx, f.workerID)
				wantCaps := capability.FilterProtocol(advertised)
				gotCaps := slices.Clone(upgraded.ProtocolCapabilities)
				slices.Sort(wantCaps)
				slices.Sort(gotCaps)
				if err != nil || !slices.Equal(gotCaps, wantCaps) {
					t.Fatalf("upgrade protocol frame=%v err=%v", upgraded.ProtocolCapabilities, err)
				}
				f.wkr = upgraded
				same := mustRun(t, f.env, id)
				// Register is a new worker process: its orphan pass requeues the
				// old executor. Heartbeat refreshes the live process in place.
				wantStatus := "running"
				if upgrade == "register" {
					wantStatus = "queued"
				}
				if same.Status != wantStatus || same.ClaimGeneration != run.ClaimGeneration ||
					same.WorkerID != run.WorkerID || len(same.CheckpointCoordinates) != 0 {
					t.Fatal("support upgrade converted or replaced the active legacy generation")
				}
				rolloutUnsupportedPublish(t, f, same)

				var oldHold string
				if err := f.env.pool.QueryRow(f.env.ctx, `SELECT to_jsonb(h)::text
					FROM recovery_custody_holds h WHERE run_id=$1 AND generation=$2 AND state='open'`,
					id, run.ClaimGeneration).Scan(&oldHold); err != nil {
					t.Fatal(err)
				}
				// Retire the reported executor through the public snapshot protocol
				// before fresh-claiming the SAME UUID at its next generation.
				f.wkr, err = f.svc.Heartbeat(f.env.ctx, f.wkr, nil, nil,
					&ActiveSnapshot{SnapshotEpoch: 2, RegisterNonce: nonce, Active: []ActiveRunEntry{}})
				if err != nil {
					t.Fatal(err)
				}
				f.env.exec("UPDATE runs SET status='queued' WHERE id=$1", id)
				fresh := rolloutClaim(t, f, id, true)
				var pair CheckpointCoordinates
				if err := json.Unmarshal(fresh.CheckpointCoordinates, &pair); err != nil {
					t.Fatal(err)
				}
				if fresh.ClaimGeneration != run.ClaimGeneration+1 ||
					pair.Version != 1 || pair.RunID != id || pair.UserID != f.userID || pair.RepoID != f.repoID ||
					pair.Kind != "task" || pair.IssueIID != nil ||
					pair.WorkBranch != "uzi/task/"+id.String() || pair.CheckpointKey != "uzi/task/"+id.String() ||
					!reflect.DeepEqual(pair.Provenance, []uuid.UUID{id}) || f.ownOpenHolds(t, id) != 2 {
					t.Fatal("next-generation admission did not persist literal own coordinates and two open holds")
				}
				var oldHoldAfter string
				var newHolds int
				if err := f.env.pool.QueryRow(f.env.ctx, `SELECT to_jsonb(h)::text,
					(SELECT count(*) FROM recovery_custody_holds
					 WHERE run_id=$1 AND generation=$3 AND state='open' AND user_id=$4 AND original_worker_id=$5)
					FROM recovery_custody_holds h WHERE run_id=$1 AND generation=$2 AND state='open'`,
					id, run.ClaimGeneration, fresh.ClaimGeneration, f.userID, f.workerID).Scan(&oldHoldAfter, &newHolds); err != nil ||
					oldHoldAfter != oldHold || newHolds != 1 {
					t.Fatalf("generation custody changed: old=%s new=%d err=%v", oldHoldAfter, newHolds, err)
				}
				_, applied, err = f.svc.SetState(f.env.ctx, f.wkr, fresh.ID,
					StateRequest{State: "running", ClaimGeneration: &fresh.ClaimGeneration})
				if err != nil || !applied {
					t.Fatalf("start enrolled generation: applied=%v err=%v", applied, err)
				}
				freshActive := entry(fresh.ID, fresh.ClaimGeneration, "running", false)
				// A later unsupported registration must retain the enrollment
				// marker while removing execution/publication authority.
				nonce = rolloutRegister(t, f, legacy, freshActive)
				downgraded := mustRun(t, f.env, fresh.ID)
				if downgraded.ClaimGeneration != fresh.ClaimGeneration ||
					string(downgraded.CheckpointCoordinates) != string(fresh.CheckpointCoordinates) {
					t.Fatal("unsupported registration erased frozen enrollment")
				}
				if downgraded.ClaimReleasedAt.Valid {
					// A restarted incompatible worker is parked by Register.
					// Stale-generation fencing precedes protocol eligibility.
					before := coordinateAuthoritySnapshot(t, f.env, fresh.ID)
					res, err := f.svc.Publish(f.env.ctx, f.wkr, fresh.ID,
						"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", []byte("unused pack"), &fresh.ClaimGeneration)
					if !errors.Is(err, ErrStaleClaim) || res.Published || coordinateAuthoritySnapshot(t, f.env, fresh.ID) != before {
						t.Fatalf("released unsupported publication=%+v err=%v", res, err)
					}
				} else {
					rolloutUnsupportedPublish(t, f, downgraded)
				}
				// Remove snapshot deduplication as an alternative refusal cause.
				f.wkr, err = f.svc.Heartbeat(f.env.ctx, f.wkr, nil, nil,
					&ActiveSnapshot{SnapshotEpoch: 1, RegisterNonce: nonce, Active: []ActiveRunEntry{}})
				if err != nil {
					t.Fatal(err)
				}
				var sources int
				if err := f.env.pool.QueryRow(f.env.ctx,
					"SELECT count(*) FROM worker_active_runs WHERE worker_id=$1 AND run_id=$2",
					f.workerID, fresh.ID).Scan(&sources); err != nil || sources != 0 {
					t.Fatalf("snapshot deduplication still active: sources=%d err=%v", sources, err)
				}
				f.env.exec("UPDATE runs SET status='queued',required_capabilities='{}' WHERE id=$1", fresh.ID)
				// Ordinary scheduling requirements are empty. Turning their
				// kill-switch off must still leave the protocol interlock closed.
				for _, scheduling := range []bool{true, false} {
					f.svc.SetCapabilitySettings(fakeCapabilitySettings{on: scheduling})
					before := coordinateAuthoritySnapshot(t, f.env, fresh.ID)
					p, err := f.svc.Claim(f.env.ctx, f.wkr, nil)
					if err != nil || p != nil {
						t.Fatalf("scheduling=%v bypassed protected execution: %+v %v", scheduling, p, err)
					}
					if coordinateAuthoritySnapshot(t, f.env, fresh.ID) != before {
						t.Fatal("refused protected claim changed authority/custody")
					}
				}
				rolloutRegister(t, f, advertised)
				reclaimed := rolloutClaim(t, f, fresh.ID, true)
				if reclaimed.ClaimGeneration != fresh.ClaimGeneration+1 ||
					string(reclaimed.CheckpointCoordinates) != string(fresh.CheckpointCoordinates) {
					t.Fatal("compatible reclaim control did not preserve enrollment at the next generation")
				}
			})
		}
	}
}

// Each archived statement sees BOTH an enrolled task and an unprotected legacy
// task. Refusal is recovered at a savepoint and the outer transaction COMMITTED;
// a whole-transaction rollback cannot conceal a partial peer write.
func TestTaskCheckpointHistoricalMixedBatchLiveDB(t *testing.T) {
	for _, name := range []string{"ReadoptRunsFromSnapshot", "FrozenReadoptRunsFromSnapshot"} {
		t.Run(name, func(t *testing.T) {
			f := newCustodyContinuationFixture(t, setupCodexLiveDB(t))
			rolloutRegister(t, f, []string{capability.RecoveryArchiveV1})
			peer := rolloutClaim(t, f, seedCoordinateTask(f), false)
			// Free the ordinary run slot without enrolling the legacy peer.
			f.env.exec("UPDATE runs SET status='completed',finished_at=now() WHERE id=$1", peer.ID)
			protected := claimCoordinateTask(t, f)
			var err error
			f.wkr, err = f.env.q.GetWorkerByID(f.env.ctx, f.workerID)
			if err != nil || f.wkr.UserID != f.userID ||
				!reflect.DeepEqual(f.wkr.ProtocolCapabilities, []string{capability.TaskCheckpointCoordinatesV1, capability.RecoveryArchiveV1}) {
				t.Fatalf("protected claim worker capabilities/owner: worker=%+v err=%v", f.wkr, err)
			}
			ids := []uuid.UUID{peer.ID, protected.ID}
			for _, run := range []store.Run{peer, protected} {
				f.env.exec("UPDATE runs SET status='queued',finished_at=NULL WHERE id=$1", run.ID)
				f.env.exec(`INSERT INTO worker_active_runs
					(worker_id,run_id,claim_generation,phase,terminal_pending,snapshot_epoch,reported_at)
					VALUES($1,$2,$3,'running',false,1,now())`, f.workerID, run.ID, run.ClaimGeneration)
				rolloutSourceControl(t, f, run)
			}
			if len(peer.CheckpointCoordinates) != 0 || len(protected.CheckpointCoordinates) == 0 ||
				peer.ClaimGeneration != 1 || protected.ClaimGeneration != 1 {
				t.Fatal("mixed batch must have real generation-1 legacy and enrolled tasks")
			}
			before := []string{rolloutAuthoritySnapshot(t, f, peer.ID), rolloutAuthoritySnapshot(t, f, protected.ID)}
			tx, _, params := rolloutRecoveryTx(t, f, ids)
			var historicalParams any = f.workerID
			if name == "FrozenReadoptRunsFromSnapshot" {
				historicalParams = params
			}
			sql, args := historicalCoordinateQuery(t, name, historicalParams)
			sp, err := tx.Begin(f.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = sp.Exec(f.env.ctx, sql, args...)
			requireCoordinateSQLState(t, err, "23514")
			if err := sp.Rollback(f.env.ctx); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(f.env.ctx); err != nil {
				t.Fatal(err)
			}
			for i, id := range ids {
				if rolloutAuthoritySnapshot(t, f, id) != before[i] {
					t.Fatalf("archived mixed-batch refusal changed complete authority/custody for %s", id)
				}
				rolloutSourceControl(t, f, mustRun(t, f.env, id))
			}

			// Explicit generation control: exclude only the protected source
			// generation. The unchanged archived statement MUST readopt the
			// legacy peer, proving it was an eligible write in the failed batch.
			tx, _, params = rolloutRecoveryTx(t, f, ids)
			if _, err := tx.Exec(f.env.ctx, "UPDATE worker_active_runs SET claim_generation=claim_generation+1 WHERE worker_id=$1 AND run_id=$2", f.workerID, protected.ID); err != nil {
				t.Fatal(err)
			}
			if name == "FrozenReadoptRunsFromSnapshot" {
				sql, args = historicalCoordinateQuery(t, name, params)
			}
			tag, err := tx.Exec(f.env.ctx, sql, args...)
			if err != nil || tag.RowsAffected() != 1 {
				t.Fatalf("legacy peer positive control: affected=%d err=%v", tag.RowsAffected(), err)
			}
			var status string
			if err := tx.QueryRow(f.env.ctx, "SELECT status FROM runs WHERE id=$1", peer.ID).Scan(&status); err != nil || status != "running" {
				t.Fatalf("peer-only control status=%s err=%v", status, err)
			}
			if err := tx.QueryRow(f.env.ctx, "SELECT status FROM runs WHERE id=$1", protected.ID).Scan(&status); err != nil || status != "queued" {
				t.Fatalf("generation-mismatched protected source was readopted: status=%s err=%v", status, err)
			}
			// This is a disposable positive control, distinct from the committed
			// refused transaction above. Restore its source and peer via rollback.
			if err := tx.Rollback(f.env.ctx); err != nil {
				t.Fatal(err)
			}

			// Updated production Frozen batch, worker-first locks and API opt-in:
			// both candidates succeed at their original generation.
			tx, q, params := rolloutRecoveryTx(t, f, ids)
			if err := q.OptInCheckpointCoordinates(f.env.ctx); err != nil {
				t.Fatal(err)
			}
			rows, err := q.FrozenReadoptRunsFromSnapshot(f.env.ctx, params)
			if err != nil || len(rows) != 2 {
				t.Fatalf("updated compatible batch rows=%v err=%v", rows, err)
			}
			seen := map[uuid.UUID]bool{}
			for _, row := range rows {
				seen[row.ID] = true
			}
			if !seen[peer.ID] || !seen[protected.ID] {
				t.Fatal("updated compatible batch omitted an original source")
			}
			if err := tx.Commit(f.env.ctx); err != nil {
				t.Fatal(err)
			}
			for _, original := range []store.Run{peer, protected} {
				got := mustRun(t, f.env, original.ID)
				if got.Status != "running" || got.WorkerID != original.WorkerID ||
					got.ClaimGeneration != original.ClaimGeneration ||
					string(got.CheckpointCoordinates) != string(original.CheckpointCoordinates) ||
					f.ownOpenHolds(t, got.ID) != 1 {
					t.Fatal("updated batch changed identity, generation, enrollment or custody")
				}
			}
		})
	}
}

// Include the entire persisted active source as well as all run/custody columns.
func rolloutAuthoritySnapshot(t *testing.T, f *custodyContinuationFixture, id uuid.UUID) string {
	t.Helper()
	var source string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(
		jsonb_agg(to_jsonb(a) ORDER BY a.worker_id),'[]'::jsonb)::text
		FROM worker_active_runs a WHERE run_id=$1`, id).Scan(&source); err != nil {
		t.Fatal(err)
	}
	return coordinateAuthoritySnapshot(t, f.env, id) + "\n" + source
}

// Verify the persisted source row, rather than merely assuming the INSERT made
// the run eligible. Both batch members must name this worker and exact generation.
func rolloutSourceControl(t *testing.T, f *custodyContinuationFixture, run store.Run) {
	t.Helper()
	var valid bool
	err := f.env.pool.QueryRow(f.env.ctx, `SELECT EXISTS(
		SELECT 1 FROM worker_active_runs a JOIN runs r ON r.id=a.run_id
		WHERE a.worker_id=$1 AND r.worker_id=$1 AND r.id=$2
		AND a.claim_generation=$3 AND r.claim_generation=$3
		AND r.status='queued' AND r.claim_released_at IS NULL
		AND a.phase='running' AND NOT a.terminal_pending)`,
		f.workerID, run.ID, run.ClaimGeneration).Scan(&valid)
	if err != nil || !valid {
		t.Fatalf("missing eligible exact-generation source for %s: %v", run.ID, err)
	}
}

func rolloutRecoveryTx(t *testing.T, f *custodyContinuationFixture, ids []uuid.UUID) (pgx.Tx, *store.Queries, store.FrozenReadoptRunsFromSnapshotParams) {
	t.Helper()
	tx, err := f.env.pool.Begin(f.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(f.env.ctx) })
	q := store.New(tx)
	if _, err := q.GetWorkerForUpdate(f.env.ctx, f.workerID); err != nil {
		t.Fatal(err)
	}
	entries := make([]validatedEntry, len(ids))
	for i, id := range ids {
		entries[i] = validatedEntry{runID: id}
	}
	locks, err := captureWorkerRecoveryLocks(f.env.ctx, q, f.workerID, entries, validatedFinalizeResume{})
	if err != nil {
		t.Fatal(err)
	}
	ledger, parents, err := locks.parameters()
	if err != nil {
		t.Fatal(err)
	}
	return tx, q, store.FrozenReadoptRunsFromSnapshotParams{WorkerID: f.workerID, FrozenTargets: ledger, LockedParentIds: parents}
}
