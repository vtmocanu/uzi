package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

// A plain UPDATE of a non-key users column (a settings write) held across the finish does not
// conflict with the finish's FOR KEY SHARE lock: the claim delivers, with no 55P03.
func TestRunEnvFinishIgnoresPlainUsersUpdateLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	fx.put(t, "API_KEY", true, envSecretValue("one"))
	var tx pgx.Tx
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(ctx context.Context, _ store.Run, _ *ClaimPayload, _ error) {
		var err error
		if tx, err = fx.env.pool.Begin(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET default_effort='low' WHERE id=$1`, fx.userID); err != nil {
			t.Fatal(err)
		}
	}}
	p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if tx != nil {
		defer func() { _ = tx.Rollback(fx.env.ctx) }()
	}
	if err != nil || p == nil || runEnvByName(p)["API_KEY"].Value != envSecretValue("one") {
		t.Fatalf("claim = (%v, %v) while a plain users UPDATE was held; want the variable delivered", p != nil, err)
	}
	if r := mustRun(t, fx.env, runID); r.Status != "claimed" || r.FailOrigin.Valid || r.HoldReason.Valid {
		t.Fatalf("status=%s fail=%v hold=%v", r.Status, r.FailOrigin, r.HoldReason)
	}
}

// insertForeignOwnerRow writes a run_env_vars row of another user on the run's own repo
// directly, the way a bug or a manual write could.
func (fx *reFix) insertForeignOwnerRow(t *testing.T, foreign uuid.UUID, name, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	fx.env.exec(`INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at)
	    VALUES ($1,$2,$3,$4,true,$5,'master_aad',10,1,CASE WHEN $6 = 'retired' THEN now() END)`,
		id, foreign, fx.repoID, name, []byte("0123456789abcdef"), state)
	return id
}

// A row of another owner on the run's own repo is never delivered.
func TestRunEnvForeignOwnerLiveRowIsNotDeliveredLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	fx.queuedRun(t, "issue")
	fx.put(t, "MINE", true, envSecretValue("mine"))
	fx.insertForeignOwnerRow(t, fx.env.seedUser(t), "FOREIGN_LIVE", "live")
	p := fx.claim(t)
	if p == nil {
		t.Fatal("claim idle")
	}
	got := runEnvByName(p)
	if len(got) != 1 || got["MINE"].Value != envSecretValue("mine") || len(p.Secrets.RunEnvRedact) != 0 {
		t.Fatalf("delivered %s; want only MINE", payloadShape(p))
	}
}

// A receipt that only a foreign owner's row could satisfy is not redacted from that row: the
// history is unavailable and the run parks.
func TestRunEnvForeignOwnerRowDoesNotSatisfyReceiptLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	foreignVar := fx.insertForeignOwnerRow(t, fx.env.seedUser(t), "FOREIGN_OLD", "retired")
	fx.env.exec(`INSERT INTO run_env_receipts (run_id,user_id,var_id,rev,scope,name,secret,first_claim_generation,last_claim_generation)
	    VALUES ($1,$2,$3,1,'repo','FOREIGN_OLD',true,1,1)`, runID, fx.userID, foreignVar)
	fx.assertParkedHistoryUnavailable(t, runID, fx.claim(t))
}

// redactFix is a Run that claimed with a secret and a plain variable, so it holds receipts.
func redactFix(t *testing.T) (*reFix, uuid.UUID, uuid.UUID) {
	t.Helper()
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	keyVar := fx.put(t, "API_KEY", true, envSecretValue("one"))
	fx.put(t, "API_BASE", false, "https://sandbox.example.test")
	if p := fx.claim(t); p == nil {
		t.Fatal("claim idle")
	}
	return fx, runID, keyVar
}

// The redact route hands the holding worker every secret version the run was delivered, opened
// for the run's owner; a plain variable is not part of it.
func TestRunEnvRedactReturnsEverySecretVersionLiveDB(t *testing.T) {
	fx, runID, keyVar := redactFix(t)
	// Rotating after the claim makes rev 1 non-effective; both versions stay receipted or held.
	fx.rotate(t, "API_KEY", envSecretValue("two"))
	res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Protected || len(res.Versions) != 1 {
		t.Fatalf("response = %s; want protected with the one receipted secret version", redactRespShape(res))
	}
	v := res.Versions[0]
	if v.VarID != keyVar.String() || v.Rev != 1 || v.Name != "API_KEY" || v.Scope != "repo" || v.Value != envSecretValue("one") {
		t.Fatalf("version = %s", redactShape([]ClaimRunEnvRedact{v}))
	}
}

func TestRunEnvRedactUnprotectedWithoutSecretReceiptsLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	runID := fx.queuedRun(t, "issue")
	fx.put(t, "API_BASE", false, "https://sandbox.example.test")
	if p := fx.claim(t); p == nil {
		t.Fatal("claim idle")
	}
	res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID)
	if err != nil || res.Protected || len(res.Versions) != 0 {
		t.Fatalf("response = %s, %v; want {protected:false}", redactRespShape(res), err)
	}
}

// Only the worker that holds or last held the run, of the run's own user, may read it; anything
// else, an unknown run or a deleted one included, is not found.
func TestRunEnvRedactOwnershipLiveDB(t *testing.T) {
	fx, runID, _ := redactFix(t)
	wkr := fx.worker(t)
	otherWorkerOfUser := wkr
	otherWorkerOfUser.ID = uuid.New()
	fx.env.exec(`INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`,
		otherWorkerOfUser.ID, fx.userID, otherWorkerOfUser.ID.String(), otherWorkerOfUser.ID[:])
	otherUser := fx.env.seedUser(t)
	otherUsersWorker := wkr
	otherUsersWorker.ID, otherUsersWorker.UserID = uuid.New(), otherUser
	fx.env.exec(`INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`,
		otherUsersWorker.ID, otherUser, otherUsersWorker.ID.String(), otherUsersWorker.ID[:])

	for name, tc := range map[string]struct {
		w   store.Worker
		run uuid.UUID
	}{
		"another worker of the same user": {otherWorkerOfUser, runID},
		"a worker of another user":        {otherUsersWorker, runID},
		"the owner's worker, unknown run": {wkr, uuid.New()},
	} {
		if _, err := fx.svc.RunEnvRedact(fx.env.ctx, tc.w, tc.run); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("%s: err = %v, want ErrRunNotOwned", name, err)
		}
	}
	// A released claim keeps its worker as affinity: the worker that last held the run reads it.
	fx.env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, runID)
	if res, err := fx.svc.RunEnvRedact(fx.env.ctx, wkr, runID); err != nil || !res.Protected {
		t.Fatalf("last holder: %s, %v", redactRespShape(res), err)
	}
	fx.env.exec(`DELETE FROM runs WHERE id = $1`, runID)
	if _, err := fx.svc.RunEnvRedact(fx.env.ctx, wkr, runID); !errors.Is(err, ErrRunNotOwned) {
		t.Fatalf("deleted run: err = %v, want ErrRunNotOwned", err)
	}
}

func TestRunEnvRedactRefusesWhenHistoryCannotBeOpenedLiveDB(t *testing.T) {
	t.Run("missing version", func(t *testing.T) {
		fx, runID, keyVar := redactFix(t)
		fx.env.exec(`DELETE FROM run_env_vars WHERE var_id = $1`, keyVar)
		if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); !errors.Is(err, ErrRunEnvHistoryUnavailable) {
			t.Fatalf("err = %v, want ErrRunEnvHistoryUnavailable", err)
		}
	})
	t.Run("swapped ciphertext", func(t *testing.T) {
		fx, runID, keyVar := redactFix(t)
		other := fx.put(t, "OTHER_KEY", true, envSecretValue("other"))
		fx.env.exec(`UPDATE run_env_vars d SET ciphertext = s.ciphertext, sealed_with = s.sealed_with
		    FROM run_env_vars s WHERE d.var_id = $1 AND s.var_id = $2`, keyVar, other)
		if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); !errors.Is(err, ErrRunEnvHistoryUnavailable) {
			t.Fatalf("err = %v, want ErrRunEnvHistoryUnavailable", err)
		}
	})
	t.Run("locked vault", func(t *testing.T) {
		fx, runID, keyVar := redactFix(t)
		fx.env.exec(`UPDATE run_env_vars SET sealed_with = 'dek' WHERE var_id = $1`, keyVar)
		fx.env.exec(`UPDATE users SET vault_mode='protected' WHERE id=$1`, fx.userID) // a dek row is a protected owner's
		if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); !errors.Is(err, ErrRunEnvVaultLocked) {
			t.Fatalf("err = %v, want ErrRunEnvVaultLocked", err)
		}
	})
}

// openHold inserts an open custody hold of the fixture's worker on the run, the parent a capture or
// a held publication row needs; closeHold settles it the way the fixture cleanups do.
func (fx *reFix) openHold(runID uuid.UUID) uuid.UUID {
	hold := uuid.New()
	fx.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
	    VALUES($1,$2,$3,$4,1,'open',$5,'redact-fixture')`, hold, fx.userID, fx.repoID, runID, fx.workerID)
	return hold
}

func (fx *reFix) closeHold(hold uuid.UUID) {
	fx.env.exec(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,
		released_at=now(),updated_at=now() WHERE id=$1`, hold)
}

// Each arm of fn_run_env_publication_pending alone, with no open custody hold except where the arm
// is the hold: a terminal run is served while the arm's in-flight state holds, and closed once the
// arm reaches a settled state. Rows that need a hold parent get one, then the hold is closed.
func TestRunEnvRedactPublicationPendingArmsAloneLiveDB(t *testing.T) {
	setup := func(t *testing.T) (*reFix, uuid.UUID) {
		fx, runID, _ := redactFix(t)
		fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, runID)
		return fx, runID
	}
	served := func(t *testing.T, fx *reFix, runID uuid.UUID, why string) {
		t.Helper()
		if res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); err != nil || !res.Protected {
			t.Fatalf("%s: %s, %v; want served", why, redactRespShape(res), err)
		}
	}
	closed := func(t *testing.T, fx *reFix, runID uuid.UUID, why string) {
		t.Helper()
		if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); !errors.Is(err, ErrRunEnvRedactClosed) {
			t.Fatalf("%s: err = %v, want ErrRunEnvRedactClosed", why, err)
		}
	}
	t.Run("open custody hold", func(t *testing.T) {
		fx, runID := setup(t)
		hold := fx.openHold(runID)
		served(t, fx, runID, "open hold")
		fx.closeHold(hold)
		closed(t, fx, runID, "released hold")
	})
	t.Run("capture", func(t *testing.T) {
		fx, runID := setup(t)
		hold := fx.openHold(runID)
		fx.env.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state)
		    VALUES($1,$2,$3,$4,$5,'redact-fixture','sha','redact-capture','uploading')`, uuid.New(), hold, runID, fx.userID, fx.workerID)
		fx.closeHold(hold)
		served(t, fx, runID, "capture uploading, hold closed")
		fx.env.exec(`UPDATE recovery_captures SET state='available' WHERE hold_id=$1`, hold)
		closed(t, fx, runID, "capture available")
		fx.env.exec(`UPDATE recovery_captures SET state='needs_action' WHERE hold_id=$1`, hold)
		closed(t, fx, runID, "capture needs_action")
	})
	t.Run("held publication", func(t *testing.T) {
		fx, runID := setup(t)
		hold := fx.openHold(runID)
		// The insert guard admits only a prepared row; invoked and created follow through the
		// transitions held_publication_guard allows.
		fx.env.exec(`INSERT INTO run_held_publications(run_id,generation,hold_id,user_id,repo_id,worker_id,live_run_id,ref,tip,coverage_digest,state)
		    VALUES($1,1,$2,$3,$4,$5,$1,$6,$7,$8,'prepared')`, runID, hold, fx.userID, fx.repoID, fx.workerID,
			"refs/uzi-held/"+runID.String()+"/1", strings.Repeat("a", 40), strings.Repeat("b", 64))
		fx.closeHold(hold)
		served(t, fx, runID, "held publication prepared")
		fx.env.exec(`UPDATE run_held_publications SET state='invoked', create_invoked_at=now() WHERE run_id=$1`, runID)
		served(t, fx, runID, "held publication invoked")
		fx.env.exec(`UPDATE run_held_publications SET state='create_unknown' WHERE run_id=$1`, runID)
		served(t, fx, runID, "held publication create_unknown")
		fx.env.exec(`UPDATE run_held_publications SET state='created' WHERE run_id=$1`, runID)
		closed(t, fx, runID, "held publication created")
	})
	t.Run("terminal preservation", func(t *testing.T) {
		fx, runID := setup(t)
		tip := strings.Repeat("c", 40)
		// A row is born bound; publishing, then live follow through the transitions
		// run_terminal_preservation_guard allows, and live needs a published contribution at its tip.
		fx.env.exec(`INSERT INTO run_terminal_preservations(run_id,user_id,repo_id,ref,lifetime_at_bind)
		    VALUES($1,$2,$3,$4,'issue')`, runID, fx.userID, fx.repoID, "refs/uzi-terminal/"+runID.String())
		served(t, fx, runID, "preservation bound")
		fx.env.exec(`UPDATE run_terminal_preservations SET state='publishing', live_run_id=run_id WHERE run_id=$1`, runID)
		served(t, fx, runID, "preservation publishing")
		fx.env.exec(`INSERT INTO run_terminal_contributions(preservation_id,run_id,generation,worker_id,user_id,authority,tip,coverage_digest,manifest,disposition,state)
		    VALUES((SELECT id FROM run_terminal_preservations WHERE run_id=$1),$1,1,$2,$3,'current_flight',$4,$5,'{}'::jsonb,'preserved','prepared')`,
			runID, fx.workerID, fx.userID, tip, strings.Repeat("d", 64))
		fx.env.exec(`UPDATE run_terminal_contributions SET state='invoked', create_invoked_at=now() WHERE run_id=$1`, runID)
		fx.env.exec(`UPDATE run_terminal_contributions SET state='published', published_at=now() WHERE run_id=$1`, runID)
		fx.env.exec(`UPDATE run_terminal_preservations SET state='live', tip=$2 WHERE run_id=$1`, runID, tip)
		closed(t, fx, runID, "preservation live")
	})
}

// A terminal run is served only while it has recovery custody or a publication in flight; a
// non-terminal run is always served.
func TestRunEnvRedactBoundedByRunStateLiveDB(t *testing.T) {
	terminal := func(fx *reFix, runID uuid.UUID) {
		fx.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, runID)
	}
	t.Run("non-terminal run is served", func(t *testing.T) {
		fx, runID, _ := redactFix(t)
		if res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); err != nil || !res.Protected {
			t.Fatalf("%s, %v; want protected", redactRespShape(res), err)
		}
	})
	t.Run("terminal run with nothing pending is closed", func(t *testing.T) {
		fx, runID, _ := redactFix(t)
		terminal(fx, runID)
		if _, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); !errors.Is(err, ErrRunEnvRedactClosed) {
			t.Fatalf("err = %v, want ErrRunEnvRedactClosed", err)
		}
	})
	t.Run("terminal run without secret receipts answers unprotected", func(t *testing.T) {
		fx := newREFix(t, true)
		runID := fx.queuedRun(t, "issue")
		fx.put(t, "API_BASE", false, "https://sandbox.example.test")
		if p := fx.claim(t); p == nil {
			t.Fatal("claim idle")
		}
		terminal(fx, runID)
		res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID)
		if err != nil || res.Protected {
			t.Fatalf("%s, %v; want {protected:false}", redactRespShape(res), err)
		}
	})
	t.Run("terminal run with an open custody hold of this worker is served", func(t *testing.T) {
		fx, runID, _ := redactFix(t)
		terminal(fx, runID)
		hold := uuid.New()
		fx.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
		    VALUES($1,$2,$3,$4,1,'open',$5,'redact-fixture')`, hold, fx.userID, fx.repoID, runID, fx.workerID)
		t.Cleanup(func() {
			fx.env.exec(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,
				released_at=now(),updated_at=now() WHERE id=$1`, hold)
		})
		if res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID); err != nil || !res.Protected {
			t.Fatalf("%s, %v; want protected", redactRespShape(res), err)
		}
	})
}
