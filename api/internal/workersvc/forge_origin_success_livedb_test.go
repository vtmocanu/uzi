package workersvc

import (
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeCloneSuccessCASLiveDB(t *testing.T) {
	for _, mode := range []string{"exact", "generation", "incarnation", "version", "observation", "new_failure", "wrong_worker", "mapping", "origin", "recreated", "released", "legacy", "downgraded", "upgraded", "cancel", "broker", "peer", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			w := e.seedWorker(t, []string{ForgeCloneSuccessFeature})
			id := e.fpSeedRunning(t, w, 1)
			bindForgeSuccessReservation(t, e, id, w)
			row, err := e.q.GetForgeOriginReservation(e.ctx, store.GetForgeOriginReservationParams{RunID: id, WorkerID: w, Generation: 1})
			if err != nil {
				t.Fatal(err)
			}
			req := ForgeCloneSuccessRequest{ClaimGeneration: 1, Incarnation: uuid.UUID(row.ReservationIncarnation.Bytes), Version: 7, ObservationVersion: row.ReservationObservationVersion.Int64}
			worker := store.Worker{ID: w, UserID: e.userID, ProtocolCapabilities: []string{ForgeCloneSuccessFeature}}
			switch mode {
			case "generation":
				req.ClaimGeneration++
			case "incarnation":
				req.Incarnation = uuid.New()
			case "version":
				req.Version++
			case "observation":
				req.ObservationVersion++
			case "new_failure":
				e.exec(t, "UPDATE forge_origin_cooldowns SET observation_version=observation_version+1 WHERE worker_id=$1", w)
			case "wrong_worker":
				worker.ID = e.seedWorker(t, []string{ForgeCloneSuccessFeature})
			case "mapping":
				e.exec(t, "UPDATE run_forge_origins SET connection_id=$2 WHERE run_id=$1", id, uuid.New())
			case "origin":
				e.exec(t, "UPDATE run_forge_origins SET origin=$2 WHERE run_id=$1", id, "https://api.example")
			case "recreated":
				fresh := uuid.New()
				e.exec(t, "UPDATE forge_origin_cooldowns SET incarnation=$2,reservation_incarnation=$2 WHERE worker_id=$1", w, fresh)
				e.exec(t, "UPDATE run_forge_origins SET reservation_incarnation=$2 WHERE run_id=$1", id, fresh)
			case "released":
				e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", id)
			case "legacy":
				worker.ProtocolCapabilities = nil
				e.exec(t, "UPDATE workers SET protocol_capabilities=ARRAY[]::text[] WHERE id=$1", w)
			case "downgraded":
				e.exec(t, "UPDATE workers SET protocol_capabilities=ARRAY[]::text[] WHERE id=$1", w)
			case "upgraded":
				worker.ProtocolCapabilities = nil
			case "cancel":
				e.exec(t, "UPDATE runs SET stop_kind='cancelled' WHERE id=$1", id)
			case "broker":
				// Broker admission carries no direct-forge claim snapshot.
				e.exec(t, "DELETE FROM run_forge_origins WHERE run_id=$1", id)
			case "peer":
				peer := e.fpSeedRunning(t, w, 1)
				e.exec(t, "UPDATE forge_origin_cooldowns SET reservation_run_id=$2 WHERE worker_id=$1", w, peer)
			}
			svc := e.forgeParkService(t, 6)
			applied, err := svc.ReportForgeCloneSuccess(e.ctx, worker, id, req)
			want := mode == "exact" || mode == "duplicate" || mode == "upgraded"
			if mode == "wrong_worker" {
				if err != ErrRunNotOwned {
					t.Fatalf("ownership error=%v", err)
				}
			} else if mode == "legacy" || mode == "downgraded" {
				if err != ErrInvalidState {
					t.Fatalf("legacy error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if applied != want {
				t.Fatalf("applied=%v want %v", applied, want)
			}
			if want && e.originCount(t, w) != 0 {
				t.Fatal("matching cooldown retained")
			}
			if !want && e.originCount(t, w) != 1 {
				t.Fatal("nonmatching cooldown removed")
			}
			if mode == "duplicate" {
				applied, err = svc.ReportForgeCloneSuccess(e.ctx, worker, id, req)
				if err != nil || applied {
					t.Fatalf("duplicate=%v,%v", applied, err)
				}
			}
		})
	}
}

func bindForgeSuccessReservation(t *testing.T, e interlockLiveDB, id, w uuid.UUID) {
	t.Helper()
	e.exec(t, `INSERT INTO forge_origin_cooldowns(user_id,worker_id,origin,cooled_until,last_reason,observed_at,
                reservation_run_id,reservation_generation,reservation_incarnation,reservation_version,reservation_observation_version)
                SELECT m.user_id,m.worker_id,m.origin,now(),'transient_clone_fetch_exhausted',now(),NULL,NULL,
                    NULL, NULL, NULL FROM run_forge_origins m WHERE m.run_id=$1`, id)
	e.exec(t, `UPDATE forge_origin_cooldowns SET reservation_run_id=$1,reservation_generation=1,
                reservation_incarnation=incarnation,reservation_version=7,reservation_observation_version=observation_version
                WHERE worker_id=$2`, id, w)
	e.exec(t, `UPDATE run_forge_origins m SET reservation_incarnation=c.incarnation,reservation_version=7
                FROM forge_origin_cooldowns c WHERE m.run_id=$1 AND c.worker_id=m.worker_id AND c.origin=m.origin`, id)
}

func TestForgeCompletionSuccessStampLiveDB(t *testing.T) {
	for _, mode := range []string{"issue", "mr_rework", "self_improve", "new_failure", "wrong_identity", "legacy", "no_head", "rollback", "permit_refused", "later_receipt"} {
		t.Run(mode, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			caps := []string{capability.RecoveryCompletedPublicationV1}
			if mode == "legacy" {
				caps = nil
			}
			w := e.seedWorker(t, caps)
			id := e.fpSeedRunning(t, w, 1)
			if mode == "mr_rework" {
				e.exec(t, "UPDATE runs SET kind='mr_rework',pipeline_ref='agent/rework',mr_iid=7,target_run_id=$1 WHERE id=$1", id)
			}
			if mode == "self_improve" {
				e.exec(t, "UPDATE runs SET kind='self_improve' WHERE id=$1", id)
			}
			hold := uuid.New()
			e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
                VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, id, w)
			bindForgeSuccessReservation(t, e, id, w)
			if mode == "new_failure" {
				e.exec(t, "UPDATE forge_origin_cooldowns SET observation_version=observation_version+1 WHERE worker_id=$1", w)
			}
			if mode == "wrong_identity" {
				e.exec(t, "UPDATE run_forge_origins SET connection_id=$2 WHERE run_id=$1", id, uuid.New())
			}
			if mode == "permit_refused" {
				e.exec(t, "UPDATE runs SET completion_contract_version=1 WHERE id=$1", id)
			}
			tx, err := e.pool.Begin(e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(e.ctx) }()
			head := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			final := &head
			if mode == "no_head" || mode == "later_receipt" {
				final = nil
			}
			tag, err := tx.Exec(e.ctx, "UPDATE runs SET status='completed',completion_final_head=$2,mr_iid=7 WHERE id=$1", id, final)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "permit_refused" && tag.RowsAffected() != 0 {
				t.Fatal("unpermitted completion applied")
			}
			var n int
			if err := tx.QueryRow(e.ctx, "SELECT count(*) FROM forge_origin_cooldowns WHERE worker_id=$1", w).Scan(&n); err != nil {
				t.Fatal(err)
			}
			cleared := mode == "issue" || mode == "mr_rework" || mode == "self_improve" || mode == "rollback"
			if (n == 0) != cleared {
				t.Fatalf("completion clear=%v want %v", n == 0, cleared)
			}
			if mode == "rollback" {
				if err := tx.Rollback(e.ctx); err != nil {
					t.Fatal(err)
				}
				if e.originCount(t, w) != 1 {
					t.Fatal("rollback lost cooldown")
				}
				var unchanged bool
				if err := e.pool.QueryRow(e.ctx, `SELECT r.status='running' AND h.completion_identity IS NULL
                    AND c.reservation_run_id=r.id FROM runs r JOIN recovery_custody_holds h ON h.run_id=r.id
                    JOIN forge_origin_cooldowns c ON c.worker_id=r.worker_id WHERE r.id=$1`, id).Scan(&unchanged); err != nil || !unchanged {
					t.Fatalf("rollback state=%v,%v", unchanged, err)
				}
			} else if err := tx.Commit(e.ctx); err != nil {
				t.Fatal(err)
			}
			if mode == "later_receipt" {
				// A completed-state replacement cannot mint a direct worker identity.
				e.exec(t, "UPDATE runs SET completion_final_head=repeat('a',40) WHERE id=$1", id)
				if e.originCount(t, w) != 1 {
					t.Fatal("later completion update cleared cooldown")
				}
			}
		})
	}
}
