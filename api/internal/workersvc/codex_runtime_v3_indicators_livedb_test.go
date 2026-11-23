package workersvc

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Inconsistent legacy indicators cannot be inserted under today's coherence CHECK.
// Confine that exception to a transaction in a newly-created disposable database.
// Each query gets its own fixture: count assertions cannot mask a claimant mutant.
func TestCodexRuntimeV3LegacyIndicatorsLiveDB(t *testing.T) {
	for _, indicator := range []string{"harness", "material", "secret"} {
		for _, query := range []string{"claim", "claimable"} {
			t.Run(indicator+"/"+query, func(t *testing.T) {
				env := isolatedCoordinateDatabase(t)
				env.vlt = vault.NewPostgres(env.box, env.pool)
				binding := newSubscriptionFixture(t, env)
				bound := mustRun(t, env, binding.runID)
				env.exec("UPDATE runs SET status='completed' WHERE id=$1", binding.runID)
				// Keep the composite secret FK valid, and remove the fixture worker from
				// the online fleet so only this test's worker decides count/claim results.
				env.exec("UPDATE workers SET status='offline' WHERE user_id=$1", binding.userID)
				env.exec("UPDATE users SET default_codex_model=NULL,planning_codex_model=NULL,planning_codex_effort=NULL,planning_claude_model=NULL,planning_claude_effort=NULL WHERE id=$1", binding.userID)
				iid := int64(100)
				e := interlockLiveDB{ctx: env.ctx, pool: env.pool, q: env.q, userID: binding.userID, repoID: bound.RepoID.Bytes, nextIID: &iid}
				old := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2}
				worker := e.seedHeartbeatWorker(t, old)
				run := e.seedCodexQueuedRun(t)
				tx, err := env.pool.Begin(env.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := tx.Rollback(env.ctx); err != nil {
						t.Errorf("rollback legacy fixture: %v", err)
					}
				}()
				exec := func(sql string, args ...any) {
					t.Helper()
					if _, err := tx.Exec(env.ctx, sql, args...); err != nil {
						t.Fatal(err)
					}
				}
				exec("ALTER TABLE runs DROP CONSTRAINT runs_codex_harness_coherence_check")
				switch indicator {
				case "material":
					exec("UPDATE runs SET harness='claude',codex_material_revision=1 WHERE id=$1", run)
				case "secret":
					if !bound.CodexSecretID.Valid {
						t.Fatal("subscription fixture lacks secret indicator")
					}
					exec("UPDATE runs SET harness='claude',codex_secret_id=$2 WHERE id=$1", run, bound.CodexSecretID)
				}
				e.q = store.New(tx)
				if query == "claimable" {
					if n := e.claimableForRun(t, run); n != 0 {
						t.Fatalf("%s v2-only claimable=%d want zero", indicator, n)
					}
				} else {
					if _, err := e.q.ClaimRun(e.ctx, e.claimParams(worker, old, false)); !errors.Is(err, pgx.ErrNoRows) {
						t.Fatalf("%s v2-only ClaimRun err=%v want ErrNoRows", indicator, err)
					}
				}
				current := append(append([]string{}, old...), capability.CodexRuntimeV3)
				exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", worker, current)
				if query == "claimable" {
					if n := e.claimableForRun(t, run); n != 1 {
						t.Fatalf("%s v3 claimable=%d want one", indicator, n)
					}
				} else {
					claimed, err := e.q.ClaimRun(e.ctx, e.claimParams(worker, current, false))
					if err != nil || claimed.ID != run {
						t.Fatalf("%s v3 ClaimRun id=%s err=%v", indicator, claimed.ID, err)
					}
				}
			})
		}
	}
}

// Use the existing fully-bound cross-check fixture, but exercise the actual store
// query so credential assembly cannot obscure the runtime admission assertion.
func TestCodexRuntimeV3CrossCheckNoExemptionLiveDB(t *testing.T) {
	f := newCrossCheckContentionFixture(t)
	e := interlockLiveDB{ctx: f.env.ctx, pool: f.env.pool, q: f.env.q, userID: f.userID}
	w := wkrRow(t, f.env, f.workerID)
	old := make([]string, 0, len(w.ProtocolCapabilities))
	for _, protocol := range w.ProtocolCapabilities {
		if protocol != capability.CodexRuntimeV3 {
			old = append(old, protocol)
		}
	}
	f.env.exec("UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now() WHERE id=$1", f.workerID, old)
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(f.workerID, old, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("v2-only cross-check claimed: %v", err)
	}
	current := append(old, capability.CodexRuntimeV3)
	f.env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", f.workerID, current)
	claimed, err := e.q.ClaimRun(e.ctx, e.claimParams(f.workerID, current, false))
	if err != nil || claimed.ID != f.runID {
		t.Fatalf("current cross-check claim id=%s err=%v", claimed.ID, err)
	}
}
