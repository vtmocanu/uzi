package handler

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func stoppedOwnerWorkerSnapshot(t *testing.T, e *applyRouteEnv) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	if err := e.pool.QueryRow(context.Background(),
		"SELECT to_jsonb(w) FROM workers w WHERE id=$1", e.worker).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(raw, &row); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestWorkerCodexStoppedOwnerRouteLiveDB(t *testing.T) {
	for _, status := range []string{"running", "paused"} {
		for _, name := range []string{"matched_positive", "mismatched_owner"} {
			t.Run(status+"/"+name, func(t *testing.T) {
				e, w := setupStoppedGuardsRoute(t)
				withdrawStoppedRoute(t, e, w)
				if status == "paused" {
					reportStoppedRoute(t, e, w, true, true)
				}
				before := stoppedRouteSnapshot(t, e)
				workerBefore := stoppedOwnerWorkerSnapshot(t, e)
				if string(workerBefore["anthropic_secret_id"]) != "null" {
					t.Fatal("owner fixture requires NULL worker Anthropic binding")
				}
				if string(workerBefore["user_id"]) != `"`+e.user.String()+`"` {
					t.Fatal("initial worker owner differs from fixture owner")
				}

				other := uuid.New()
				owner := e.user
				mismatch := name == "mismatched_owner"
				if mismatch {
					owner = other
				}
				ctx := context.Background()
				tx, err := e.pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback(ctx) }()
				if _, err := tx.Exec(ctx, `INSERT INTO users(id,email,password_hash,vault_mode)
 VALUES($1,$2,'x','unprotected')`, other, "stopped-owner-"+other.String()+"@example.com"); err != nil {
					t.Fatal(err)
				}
				// Both cases remove the same composite worker-owner bookkeeping.
				// The run's binding and claim stay on the original owner.
				if _, err := tx.Exec(ctx, "DELETE FROM run_forge_origins WHERE worker_id=$1", e.worker); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "DELETE FROM forge_origin_cooldowns WHERE worker_id=$1", e.worker); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "UPDATE workers SET user_id=$2 WHERE id=$1", e.worker, owner); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, stoppedRouteSnapshot(t, e)) {
					t.Fatal("owner fixture preparation changed run snapshot")
				}
				workerAfter := stoppedOwnerWorkerSnapshot(t, e)
				if string(workerAfter["user_id"]) != `"`+owner.String()+`"` {
					t.Fatal("stored worker owner differs from prepared owner")
				}
				workerBefore["user_id"] = workerAfter["user_id"]
				if !reflect.DeepEqual(workerBefore, workerAfter) {
					t.Fatal("owner fixture preparation changed another worker field")
				}

				var valid bool
				if err := e.pool.QueryRow(ctx, `SELECT
 r.user_id=$2 AND w.user_id=$3 AND (w.user_id<>r.user_id)=$4
 AND r.worker_id=$5 AND r.claim_generation=$6 AND r.status=$7
 AND EXISTS (SELECT 1 FROM users WHERE id=$8)
 FROM runs r JOIN workers w ON w.id=r.worker_id WHERE r.id=$1`,
					e.run, e.user, owner, mismatch, e.worker, e.gen, status, other).Scan(&valid); err != nil {
					t.Fatal(err)
				}
				if !valid {
					t.Fatal("owner fixture lost owner triplet, seed user, or claim identity")
				}
				// e.do keeps the original token and worker ID; authentication reads
				// the stored owner. Each refusal has an independent matched control.
				reportStoppedRoute(t, e, w, !mismatch, !mismatch && status == "running")
				if !reflect.DeepEqual(workerAfter, stoppedOwnerWorkerSnapshot(t, e)) {
					t.Fatal("stopped route changed worker row")
				}
			})
		}
	}
}
