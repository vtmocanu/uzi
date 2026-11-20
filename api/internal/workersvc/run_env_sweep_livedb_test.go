package workersvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// PRD #4416 M2: the retired repo variable sweep against a real PostgreSQL (skipped without
// UZI_TEST_DATABASE_URL). Each case builds a secret variable a run received at its first claim, rotates it
// (the first version is retired), makes the run terminal for longer than the grace window, adds one open
// record of the kind under test, and asserts the retired version survives a sweep; closing that record
// lets the next sweep delete it. The live version is never touched.

// bypass runs one statement with triggers and foreign keys off for this connection only, so a test can
// place a hold row, or move it to its final state, without rebuilding the whole recovery fixture the
// table's guards expect. CHECK constraints still apply.
func (fx *cdFix) bypass(t *testing.T, sql string, args ...any) {
	t.Helper()
	conn, err := fx.env.pool.Acquire(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(fx.env.ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	_, execErr := conn.Exec(fx.env.ctx, sql, args...)
	if _, err := conn.Exec(fx.env.ctx, `RESET session_replication_role`); err != nil {
		t.Fatal(err)
	}
	if execErr != nil {
		t.Fatalf("%s: %v", strings.Fields(sql)[0]+" "+strings.Fields(sql)[2], execErr)
	}
}

// sweepAll runs the registered pass until its user cursor wraps, so rows left by other tests in the
// shared database cannot hide this test's user behind the per-tick user cap.
func (fx *cdFix) sweepAll(t *testing.T) {
	t.Helper()
	for i := 0; i < 20; i++ {
		if _, err := fx.svc.SweepRunEnvRetired(fx.env.ctx); err != nil {
			t.Fatalf("SweepRunEnvRetired: %v", err)
		}
		fx.svc.runEnvSweepMu.Lock()
		wrapped := fx.svc.runEnvSweepCursor == uuid.Nil
		fx.svc.runEnvSweepMu.Unlock()
		if wrapped {
			return
		}
	}
	t.Fatal("the sweep cursor never wrapped")
}

func (fx *cdFix) envVersionExists(t *testing.T, varID uuid.UUID, rev int32) bool {
	t.Helper()
	var n int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM run_env_vars WHERE var_id = $1 AND rev = $2`, varID, rev).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// receiptedRetiredVersion claims a run over a secret variable, rotates it so revision 1 is retired,
// and returns the run, the variable and the claiming worker. The run is claimed, so it is held.
func (fx *cdFix) receiptedRetiredVersion(t *testing.T) (run, varID uuid.UUID) {
	t.Helper()
	run = fx.queuedRun(t, "issue")
	varID = fx.putEnv(t, "SECRET_"+strings.ToUpper(strings.ReplaceAll(uuid.NewString()[:8], "-", "")), "sekret-1", true)
	if p := fx.claim(t); p == nil || mustRun(t, fx.env, run).Status != "claimed" {
		t.Fatal("setup claim delivered nothing")
	}
	var name string
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT name FROM run_env_vars WHERE var_id = $1`, varID).Scan(&name); err != nil {
		t.Fatal(err)
	}
	fx.rotateEnv(t, varID, name, "sekret-2", true)
	return run, varID
}

func (fx *cdFix) finishRun(t *testing.T, run uuid.UUID, ago string) {
	t.Helper()
	fx.env.exec(`UPDATE runs SET status = 'completed', status_since = now(), finished_at = now() - $2::interval WHERE id = $1`, run, ago)
}

// settleClaimCustody ends the custody hold the setup claim opened, so a case's own record is the only
// reason left to keep the version.
func (fx *cdFix) settleClaimCustody(t *testing.T, run uuid.UUID) {
	t.Helper()
	fx.releaseCustody(t, run)
}

func TestRunEnvRetiredSweepHoldsEveryKindOfOpenRecordLiveDB(t *testing.T) {
	hash := strings.Repeat("a", 40)
	digest := strings.Repeat("b", 64)
	type hold struct {
		name  string
		open  func(t *testing.T, fx *cdFix, run uuid.UUID) // adds one open record of the kind under test
		close func(t *testing.T, fx *cdFix, run uuid.UUID) // moves it to its final state
	}
	holds := []hold{
		{"non-terminal run", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.env.exec(`UPDATE runs SET status = 'queued', finished_at = NULL WHERE id = $1`, run)
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) { fx.finishRun(t, run, "25 hours") }},
		{"terminal for under 24 hours", func(t *testing.T, fx *cdFix, run uuid.UUID) { fx.finishRun(t, run, "23 hours") },
			func(t *testing.T, fx *cdFix, run uuid.UUID) { fx.finishRun(t, run, "25 hours") }},
		{"terminal with no finish time", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.env.exec(`UPDATE runs SET status = 'failed', finished_at = NULL WHERE id = $1`, run)
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) { fx.finishRun(t, run, "25 hours") }},
		{"open custody hold", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
				VALUES ($1,$2,$3,$4,9,'open',$5,'sweep-fixture')`, uuid.New(), fx.userID, fx.repoID, run, fx.workerID)
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE recovery_custody_holds SET state = 'released' WHERE run_id = $1 AND generation = 9`, run)
		}},
		{"recovery capture", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
				VALUES ($1,$2,$3,$4,'sweep-fixture','abc',$5,'needs_action')`, uuid.New(), uuid.New(), run, fx.userID, uuid.NewString())
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE recovery_captures SET state = 'expired' WHERE run_id = $1`, run)
		}},
		{"held-ref publication", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO run_held_publications(id,run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
				VALUES (gen_random_uuid(),$1,1,$2,$3,$4,$5,$1,$8,$6,$7,'prepared')`,
				run, uuid.New(), fx.userID, fx.repoID, fx.workerID, hash, digest, "refs/uzi-held/"+run.String()+"/1")
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE run_held_publications SET state = 'refused', live_run_id = NULL, create_invoked_at = now() WHERE run_id = $1`, run)
		}},
		{"terminal preservation", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO run_terminal_preservations(run_id,user_id,repo_id,ref,lifetime_at_bind) VALUES ($1,$2,$3,$4,'issue')`,
				run, fx.userID, fx.repoID, "refs/uzi-terminal/"+run.String())
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE run_terminal_preservations SET state = 'not_published' WHERE run_id = $1`, run)
		}},
		{"salvage", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO run_salvage(run_id,user_id,repo_id,forge_type,branch,tip,state,live_run_id) VALUES ($1,$2,$3,'github','agent/issue-1',$4,'pending',$1)`,
				run, fx.userID, fx.repoID, hash)
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE run_salvage SET state = 'unavailable', live_run_id = NULL WHERE run_id = $1`, run)
		}},
		{"prerequisite closure", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			capture := uuid.New()
			fx.bypass(t, `INSERT INTO recovery_prerequisite_closures(capture_id,hold_id,run_id,user_id,repo_id,checksum,prerequisite_shas,pin_ref,next_attempt_at)
				VALUES ($1,$2,$3,$4,$5,$6,ARRAY[$7]::text[],$8,now())`, capture, uuid.New(), run, fx.userID, fx.repoID, digest, hash, "refs/uzi-prereq/"+capture.String())
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE recovery_prerequisite_closures SET state = 'refused', reason = 'closure_unsupported' WHERE run_id = $1`, run)
		}},
		{"publication resume", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO run_publication_resume(run_id,enrollment_seq,claim_generation,intent,state) VALUES ($1,1,1,'{}','active')`, run)
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE run_publication_resume SET state = 'retired' WHERE run_id = $1`, run)
		}},
		{"checkpoint retention", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO checkpoint_retentions(run_id,user_id,repo_id,branch,ref,tip,state) VALUES ($1,$2,$3,'agent/issue-1','refs/uzi-checkpoints/agent/issue-1',$4,'retained')`,
				run, fx.userID, fx.repoID, hash)
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `UPDATE checkpoint_retentions SET state = 'deleted' WHERE run_id = $1`, run)
		}},
		{"unresolved checkpoint publish attempt", func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `INSERT INTO checkpoint_publish_attempts(run_id,branch,ref,tip) VALUES ($1,'agent/issue-1','refs/uzi-checkpoints/agent/issue-1',$2)`, run, hash)
		}, func(t *testing.T, fx *cdFix, run uuid.UUID) {
			fx.bypass(t, `DELETE FROM checkpoint_publish_attempts WHERE run_id = $1`, run)
		}},
	}
	for _, h := range holds {
		t.Run(h.name, func(t *testing.T) {
			fx := newRunEnvFix(t)
			run, varID := fx.receiptedRetiredVersion(t)
			// Everything else is released: terminal for over the grace window, claim custody settled.
			fx.finishRun(t, run, "25 hours")
			fx.settleClaimCustody(t, run)
			h.open(t, fx, run)
			fx.sweepAll(t)
			if !fx.envVersionExists(t, varID, 1) {
				t.Fatalf("the retired version was deleted while %s is open", h.name)
			}
			if !fx.envVersionExists(t, varID, 2) {
				t.Fatal("the live version was deleted")
			}
			h.close(t, fx, run)
			fx.sweepAll(t)
			if fx.envVersionExists(t, varID, 1) {
				t.Fatalf("the retired version survived after %s was released", h.name)
			}
			if !fx.envVersionExists(t, varID, 2) {
				t.Fatal("the live version was deleted by the sweep")
			}
		})
	}
}

// A pending-deletion row is released by the same rule, a version nobody holds a receipt for goes at
// once, and the variable identity goes only when no row and no receipt names it.
func TestRunEnvRetiredSweepPendingDeletionAndIdentitiesLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	run := fx.queuedRun(t, "issue")
	pending := fx.putEnv(t, "PENDING_ONE", "sekret-1", true)
	if p := fx.claim(t); p == nil {
		t.Fatal("setup claim delivered nothing")
	}
	fx.env.exec(`UPDATE run_env_vars SET pending_delete_at = now() WHERE var_id = $1`, pending)
	orphan := fx.putEnv(t, "RETIRED_NO_RECEIPT", "sekret-3", true)
	fx.env.exec(`UPDATE run_env_vars SET retired_at = now() WHERE var_id = $1`, orphan)
	// An identity whose rows are all gone but that a receipt still names must stay.
	var identityCount = func(id uuid.UUID) int {
		var n int
		if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM run_env_var_ids WHERE var_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// The run is not terminal long enough: the pending row stays, the unreceipted retired row goes.
	fx.settleClaimCustody(t, run)
	fx.finishRun(t, run, "1 hour")
	fx.sweepAll(t)
	if !fx.envVersionExists(t, pending, 1) {
		t.Fatal("a pending-deletion row was deleted inside the grace window")
	}
	if fx.envVersionExists(t, orphan, 1) {
		t.Fatal("a retired row no run holds a receipt for survived the sweep")
	}
	if identityCount(orphan) != 0 {
		t.Fatal("the identity of a fully deleted variable survived the sweep")
	}

	fx.finishRun(t, run, "25 hours")
	fx.sweepAll(t)
	if fx.envVersionExists(t, pending, 1) {
		t.Fatal("a pending-deletion row survived after its run was released")
	}
	if identityCount(pending) != 1 {
		t.Fatal("the identity was deleted while a receipt still names the variable")
	}
	// Deleting the run drops its receipts; the identity is then unreferenced and the next sweep,
	// which needs a retired or pending row to visit the user, drops it with that row.
	fx.env.exec(`DELETE FROM runs WHERE id = $1`, run)
	again := fx.putEnv(t, "RETIRED_AGAIN", "sekret-4", true)
	fx.env.exec(`UPDATE run_env_vars SET retired_at = now() WHERE var_id = $1`, again)
	fx.sweepAll(t)
	if identityCount(pending) != 0 {
		t.Fatal("an unreferenced identity survived a sweep that visited its user")
	}
}

// A sweep that finds the owner row busy skips that user and changes nothing.
func TestRunEnvRetiredSweepSkipsABusyOwnerLiveDB(t *testing.T) {
	fx := newRunEnvFix(t)
	orphan := fx.putEnv(t, "RETIRED_NO_RECEIPT", "sekret-3", true)
	fx.env.exec(`UPDATE run_env_vars SET retired_at = now() WHERE var_id = $1`, orphan)
	tx, err := fx.env.pool.Begin(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(fx.env.ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, fx.userID); err != nil {
		t.Fatal(err)
	}
	n, err := fx.svc.sweepRunEnvUser(context.Background(), fx.userID)
	if err != nil || n != 0 || !fx.envVersionExists(t, orphan, 1) {
		t.Fatalf("busy owner: deleted %d, err %v, row present %v; want a clean skip", n, err, fx.envVersionExists(t, orphan, 1))
	}
	if err := tx.Rollback(fx.env.ctx); err != nil {
		t.Fatal(err)
	}
	fx.sweepAll(t)
	if fx.envVersionExists(t, orphan, 1) {
		t.Fatal("the row survived the sweep once the owner row was free")
	}
}
