package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func TestWorkerCodexStoppedPreservationRouteLiveDB(t *testing.T) {
	// Require the exact seeded row in the full-row snapshot before granting
	// preservation credit to reportStoppedRoute.
	assertSnapshotRow := func(t *testing.T, e *applyRouteEnv, key, id string) {
		t.Helper()
		var rows []struct {
			ID json.RawMessage `json:"id"`
		}
		if err := json.Unmarshal(stoppedRouteSnapshot(t, e)[key], &rows); err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			t.Fatalf("pre-stop %s snapshot is empty", key)
		}
		for _, row := range rows {
			if string(row.ID) == id {
				return
			}
		}
		t.Fatalf("pre-stop %s snapshot lacks seeded row", key)
	}

	t.Run("applied_input", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		ctx := context.Background()
		var inputID int64
		if err := e.pool.QueryRow(ctx, `INSERT INTO run_user_inputs(
 run_id,kind,body,consumed_at,consumed_claim_generation,consumed_worker_id,applied_at,disposition)
 VALUES($1,'pause','already applied pause',now(),$2,$3,now(),'applied') RETURNING id`,
			e.run, e.gen, e.worker).Scan(&inputID); err != nil {
			t.Fatal(err)
		}
		var valid bool
		if err := e.pool.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM run_user_inputs i JOIN runs r ON r.id=i.run_id
 WHERE i.id=$1 AND i.run_id=$2 AND r.user_id=$3
 AND r.claim_generation=$4 AND r.worker_id=$5
 AND i.consumed_claim_generation=r.claim_generation AND i.consumed_worker_id=r.worker_id
 AND i.kind='pause' AND i.body='already applied pause'
 AND i.consumed_at IS NOT NULL AND i.applied_at IS NOT NULL AND i.disposition='applied'
 AND i.gate_binding IS NULL AND i.gate_revision IS NULL)`,
			inputID, e.run, e.user, e.gen, e.worker).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatal("applied input seed missing or not bound to the current owner/claim with expected fields")
		}
		assertSnapshotRow(t, e, "inputs", fmt.Sprint(inputID))
		reportStoppedRoute(t, e, w, true, true)
		reportStoppedRoute(t, e, w, true, false)
	})

	t.Run("open_custody", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		ctx := context.Background()
		holdID := uuid.New()
		var insertedID uuid.UUID
		if err := e.pool.QueryRow(ctx, `INSERT INTO recovery_custody_holds(
 id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,
 live_worker_id,live_run_id,inventory_guarded)
 SELECT $1,r.user_id,r.repo_id,r.id,r.claim_generation,'open',r.worker_id,$3,r.worker_id,r.id,true
 FROM runs r WHERE r.id=$2 RETURNING id`,
			holdID, e.run, e.worker.String()).Scan(&insertedID); err != nil {
			t.Fatal(err)
		}
		if insertedID != holdID {
			t.Fatal("custody insert returned a different hold")
		}
		var valid bool
		if err := e.pool.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM recovery_custody_holds h JOIN runs r ON r.id=h.run_id
 WHERE h.id=$1 AND h.run_id=$2 AND h.user_id=$3 AND r.user_id=h.user_id
 AND h.repo_id IS NOT DISTINCT FROM r.repo_id
 AND r.claim_generation=$4 AND h.generation=r.claim_generation
 AND r.worker_id=$5 AND h.original_worker_id=r.worker_id
 AND h.original_worker_identity=$6 AND h.live_worker_id=r.worker_id AND h.live_run_id=r.id
 AND h.state='open' AND h.inventory_guarded AND h.released_at IS NULL
 AND h.release_evidence IS NULL AND h.final_disposition IS NULL AND h.final_capture_id IS NULL
 AND h.final_source_sha IS NULL AND h.final_coverage_digest IS NULL
 AND h.final_publication_id IS NULL AND h.final_closure_id IS NULL)`,
			holdID, e.run, e.user, e.gen, e.worker, e.worker.String()).Scan(&valid); err != nil {
			t.Fatal(err)
		}
		if !valid {
			t.Fatal("open custody seed missing or not bound to the current owner/claim with expected fields")
		}
		assertSnapshotRow(t, e, "custody", fmt.Sprintf("%q", holdID.String()))
		reportStoppedRoute(t, e, w, true, true)
		reportStoppedRoute(t, e, w, true, false)
	})
}
