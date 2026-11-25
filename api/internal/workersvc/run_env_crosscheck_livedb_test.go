package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// The cross-check half of PRD #4416 M2's masking contract: a checker child is never given run_env
// and writes no receipts, but it masks every secret version its target (lead) run was given.
// Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

// seedLeadSecret stores a secret version for the lead's repo and records the lead's receipt for
// it, as a delivered claim would have. It returns the variable id.
func seedLeadSecret(t *testing.T, f crossCheckContentionFixture, name, value string) uuid.UUID {
	t.Helper()
	repo := uuid.UUID(mustRun(t, f.env, f.lead).RepoID.Bytes)
	varID := uuid.New()
	kind, err := runenv.AADFor(runenv.AADVersion2, f.userID, runenv.ScopeRepo, &repo, true, name, varID, 1)
	if err != nil {
		t.Fatal(err)
	}
	m, err := f.env.vlt.Seal(f.env.ctx, f.userID, kind, []byte(value), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G115: fixture values are a few bytes.
	valueBytes := int32(len(value))
	p := store.InsertRunEnvVarParams{
		VarID: varID, UserID: f.userID, Name: name, Secret: true, Ciphertext: m.Ciphertext, SealedWith: m.SealedWith,
		ValueBytes: valueBytes, Rev: 1, AadVersion: runenv.AADVersion2,
	}
	p.RepoID.Bytes, p.RepoID.Valid = repo, true
	if _, err := f.env.q.InsertRunEnvVar(f.env.ctx, p); err != nil {
		t.Fatal(err)
	}
	f.env.exec(`INSERT INTO run_env_receipts (run_id,user_id,var_id,rev,scope,name,secret,first_claim_generation,last_claim_generation)
	    VALUES ($1,$2,$3,1,'repo',$4,true,1,1)`, f.lead, f.userID, varID, name)
	return varID
}

func checkerReceiptCount(t *testing.T, f crossCheckContentionFixture, run uuid.UUID) int {
	t.Helper()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_env_receipts WHERE run_id=$1`, run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func crossCheckEnvCapable(f crossCheckContentionFixture, capable bool) {
	if capable {
		f.env.exec(`UPDATE workers SET protocol_capabilities = array_append(protocol_capabilities, $2) WHERE id = $1`,
			f.workerID, capability.RunEnvV1)
	}
}

// A checker child whose target holds a secret receipt gets exactly that redact list, no run_env,
// and writes no receipt of its own; the redact route answers the child with the target's versions.
func TestRunEnvCrossCheckChildGetsTargetRedactOnlyLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage, func(t *testing.T) {
			f, w := admissionCheckerBase(t, stage)
			f, _ = admissionCheckerSubmit(t, f, w, stage, 0, nil)
			crossCheckEnvCapable(f, true)
			varID := seedLeadSecret(t, f, "API_KEY", envSecretValue("lead"))
			p := laneClaim(t, f, "cross_check", nil)
			if p == nil || p.RunID != f.runID.String() {
				t.Fatalf("checker not claimed: %s", payloadShape(p))
			}
			if len(p.Secrets.RunEnv) != 0 {
				t.Fatalf("checker was delivered run_env: %s", envShape(p.Secrets.RunEnv))
			}
			got := p.Secrets.RunEnvRedact
			if len(got) != 1 || got[0].VarID != varID.String() || got[0].Rev != 1 || got[0].Name != "API_KEY" ||
				got[0].Scope != "repo" || got[0].Value != envSecretValue("lead") {
				t.Fatalf("run_env_redact = %s, want the lead's one secret version", redactShape(got))
			}
			if n := checkerReceiptCount(t, f, f.runID); n != 0 {
				t.Fatalf("checker child has %d receipts, want none", n)
			}
			if n := checkerReceiptCount(t, f, f.lead); n != 1 {
				t.Fatalf("lead has %d receipts, want its one", n)
			}
			res, err := f.svc.RunEnvRedact(f.env.ctx, wkrRow(t, f.env, f.workerID), f.runID)
			if err != nil || !res.Protected || len(res.Versions) != 1 || res.Versions[0] != got[0] {
				t.Fatalf("redact route for the child = %s, %v; want the target's version", redactRespShape(res), err)
			}
		})
	}
}

// A checker with a target that holds no secret receipt gets neither list.
func TestRunEnvCrossCheckChildWithoutSecretReceiptsGetsNothingLiveDB(t *testing.T) {
	f, w := admissionCheckerBase(t, "plan")
	f, _ = admissionCheckerSubmit(t, f, w, "plan", 0, nil)
	crossCheckEnvCapable(f, true)
	p := laneClaim(t, f, "cross_check", nil)
	if p == nil || len(p.Secrets.RunEnv) != 0 || len(p.Secrets.RunEnvRedact) != 0 {
		t.Fatalf("payload = %s, want a claim with no run environment", payloadShape(p))
	}
	res, err := f.svc.RunEnvRedact(f.env.ctx, wkrRow(t, f.env, f.workerID), f.runID)
	if err != nil || res.Protected || len(res.Versions) != 0 {
		t.Fatalf("redact route = %s, %v; want {protected:false}", redactRespShape(res), err)
	}
}

// A checker that cannot mask the lead's history never runs unmasked and never passes: the check
// ends failed/checker_unavailable, the child fails (it is not parked), and no payload is delivered.
func TestRunEnvCrossCheckUnmaskableHistoryIsCheckerUnavailableLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, tc := range []struct {
			name    string
			capable bool
			mutate  func(f crossCheckContentionFixture, varID uuid.UUID)
		}{
			{"missing version", true, func(f crossCheckContentionFixture, v uuid.UUID) {
				f.env.exec(`DELETE FROM run_env_vars WHERE var_id=$1`, v)
			}},
			{"swapped ciphertext", true, func(f crossCheckContentionFixture, v uuid.UUID) {
				other := seedLeadSecret(t, f, "OTHER_KEY", envSecretValue("other"))
				f.env.exec(`UPDATE run_env_vars d SET ciphertext = s.ciphertext, sealed_with = s.sealed_with
				    FROM run_env_vars s WHERE d.var_id = $1 AND s.var_id = $2`, v, other)
			}},
			{"worker without run_env_v1", false, func(crossCheckContentionFixture, uuid.UUID) {}},
		} {
			t.Run(stage+"/"+tc.name, func(t *testing.T) {
				f, w := admissionCheckerBase(t, stage)
				f, _ = admissionCheckerSubmit(t, f, w, stage, 0, nil)
				crossCheckEnvCapable(f, tc.capable)
				varID := seedLeadSecret(t, f, "API_KEY", envSecretValue("lead"))
				tc.mutate(f, varID)
				if p := laneClaim(t, f, "cross_check", nil); p != nil {
					t.Fatalf("an unmaskable checker was delivered a payload: %s", redactShape(p.Secrets.RunEnvRedact))
				}
				var verdict, outcome, reason string
				if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(verdict,''), COALESCE(outcome,''), COALESCE(reason_class,'')
				    FROM cross_checks WHERE checker_run_id=$1`, f.runID).Scan(&verdict, &outcome, &reason); err != nil {
					t.Fatal(err)
				}
				decided := verdict
				if stage == "code" {
					decided = outcome
				}
				if decided != "failed" || reason != "checker_unavailable" {
					t.Fatalf("check = verdict %q outcome %q reason %q, want failed/checker_unavailable", verdict, outcome, reason)
				}
				child := mustRun(t, f.env, f.runID)
				if child.Status != "failed" || child.HoldReason.Valid {
					t.Fatalf("child status=%s hold=%v, want failed and not parked", child.Status, child.HoldReason)
				}
				if n := checkerReceiptCount(t, f, f.runID); n != 0 {
					t.Fatalf("checker child has %d receipts", n)
				}
			})
		}
	}
}

// A locked vault is the claim lane's transient requeue, not a check outcome.
func TestRunEnvCrossCheckLockedVaultRequeuesLiveDB(t *testing.T) {
	f, w := admissionCheckerBase(t, "plan")
	f, _ = admissionCheckerSubmit(t, f, w, "plan", 0, nil)
	crossCheckEnvCapable(f, true)
	varID := seedLeadSecret(t, f, "API_KEY", envSecretValue("lead"))
	f.env.exec(`UPDATE run_env_vars SET sealed_with = 'dek' WHERE var_id = $1`, varID)
	// A dek row means a protected owner: the owner turns protected after the claim gate and
	// before the values are opened.
	f.svc.claimHooks = &claimTestHooks{beforeAssembly: func(context.Context, store.Run) {
		f.env.exec(`UPDATE users SET vault_mode='protected' WHERE id=$1`, f.userID)
	}}
	if p := laneClaim(t, f, "cross_check", nil); p != nil {
		t.Fatal("a checker was delivered with the vault locked")
	}
	if child := mustRun(t, f.env, f.runID); child.Status != "queued" {
		t.Fatalf("child status = %s, want queued", child.Status)
	}
	var verdict string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT COALESCE(verdict,'') FROM cross_checks WHERE checker_run_id=$1`, f.runID).Scan(&verdict); err != nil {
		t.Fatal(err)
	}
	if verdict == "failed" {
		t.Fatal("a locked vault decided the check")
	}
}

// The route authorizes the worker that holds or last held the run, or one whose open custody hold
// it took; an unrelated worker of the same user stays 404.
func TestRunEnvRedactCustodyHoldAuthorizesLiveDB(t *testing.T) {
	fx, runID, _ := redactFix(t)
	wkr := fx.worker(t)
	other := uuid.New()
	fx.env.exec(`INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`,
		other, fx.userID, other.String(), other[:])
	otherWkr := wkr
	otherWkr.ID = other
	// The run has moved on to another worker: the first worker holds it no longer.
	fx.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, runID, other)
	notOwned := func(w store.Worker, why string) {
		t.Helper()
		if _, err := fx.svc.RunEnvRedact(fx.env.ctx, w, runID); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("%s: err = %v, want ErrRunNotOwned", why, err)
		}
	}
	owned := func(w store.Worker, why string) {
		t.Helper()
		if res, err := fx.svc.RunEnvRedact(fx.env.ctx, w, runID); err != nil || !res.Protected || len(res.Versions) != 1 {
			t.Fatalf("%s: %s, %v; want the run's versions", why, redactRespShape(res), err)
		}
	}
	notOwned(wkr, "a worker with no hold on a run that moved")
	hold := uuid.New()
	fx.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
	    VALUES($1,$2,$3,$4,1,'open',$5,'redact-fixture')`, hold, fx.userID, fx.repoID, runID, fx.workerID)
	t.Cleanup(func() {
		fx.env.exec(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,
			released_at=now(),updated_at=now() WHERE id=$1`, hold)
	})
	owned(wkr, "the worker that took an open hold")
	owned(otherWkr, "the worker the run is on")
	third := uuid.New()
	fx.env.exec(`INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`,
		third, fx.userID, third.String(), third[:])
	thirdWkr := wkr
	thirdWkr.ID = third
	notOwned(thirdWkr, "an unrelated worker of the same user")
	fx.env.exec(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,
		released_at=now(),updated_at=now() WHERE id=$1`, hold)
	notOwned(wkr, "a settled hold")
}

// An inventory-guarded hold authorizes only the worker that is still its live holder on this run.
func TestRunEnvRedactGuardedCustodyHoldNeedsLiveHolderLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name       string
		liveIsHost bool
	}{{"live holder is the hold's worker", true}, {"live holder is another worker", false}} {
		t.Run(tc.name, func(t *testing.T) {
			fx, runID, _ := redactFix(t)
			wkr := fx.worker(t)
			other := uuid.New()
			fx.env.exec(`INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`,
				other, fx.userID, other.String(), other[:])
			fx.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, runID, other)
			live := other
			if tc.liveIsHost {
				live = fx.workerID
			}
			fx.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
			    VALUES($1,$2,$3,$4,1,'open',$5,'redact-fixture',$6,$4,true)`, uuid.New(), fx.userID, fx.repoID, runID, fx.workerID, live)
			res, err := fx.svc.RunEnvRedact(fx.env.ctx, wkr, runID)
			if tc.liveIsHost {
				if err != nil || !res.Protected || len(res.Versions) != 1 {
					t.Fatalf("%s, %v; want the run's versions", redactRespShape(res), err)
				}
			} else if !errors.Is(err, ErrRunNotOwned) {
				t.Fatalf("err = %v, want ErrRunNotOwned", err)
			}
		})
	}
}

// A run of the worker's user that is neither a lead-lane run nor a cross-check child has no
// governing run: the route answers {protected:false}, not 404.
func TestRunEnvRedactNonLeadLaneRunIsUnprotectedLiveDB(t *testing.T) {
	fx := newREFix(t, true)
	fx.put(t, "API_KEY", true, envSecretValue("one"))
	// A chat run is outside the lead lane, whatever the repo holds.
	runID := uuid.New()
	// The chat column set mirrors credential_switch_chat_exempt_livedb_test.go's chat-run fixture.
	fx.env.exec(`INSERT INTO runs (id, user_id, kind, issue_title, issue_description, title, trigger_source, harness,
	             status, status_since, worker_id, claim_generation, budget_paused_seconds, budget_wall_seconds)
	    VALUES ($1, $2, 'chat', 'chat title', 'chat prompt', 'chat title', 'chat', 'claude',
	            'queued', now(), $3, 0, 0, 3600)`, runID, fx.userID, fx.workerID)
	res, err := fx.svc.RunEnvRedact(fx.env.ctx, fx.worker(t), runID)
	if err != nil || res.Protected || len(res.Versions) != 0 {
		t.Fatalf("%s, %v; want {protected:false}", redactRespShape(res), err)
	}
}

// A cross-check child is served while its check is open; once it is terminal, or its check has
// passed its deadline or been decided, the route is closed unless the child itself has recovery
// work in flight.
func TestRunEnvRedactCrossCheckChildBoundedLiveDB(t *testing.T) {
	setup := func(t *testing.T, stage string) crossCheckContentionFixture {
		t.Helper()
		f, w := admissionCheckerBase(t, stage)
		f, _ = admissionCheckerSubmit(t, f, w, stage, 0, nil)
		crossCheckEnvCapable(f, true)
		seedLeadSecret(t, f, "API_KEY", envSecretValue("lead"))
		if p := laneClaim(t, f, "cross_check", nil); p == nil {
			t.Fatal("checker not claimed")
		}
		return f
	}
	redact := func(f crossCheckContentionFixture) (RunEnvRedactResponse, error) {
		return f.svc.RunEnvRedact(f.env.ctx, wkrRow(t, f.env, f.workerID), f.runID)
	}
	for _, stage := range []string{"plan", "code"} {
		t.Run(stage+"/open check is served", func(t *testing.T) {
			f := setup(t, stage)
			if res, err := redact(f); err != nil || !res.Protected || len(res.Versions) != 1 {
				t.Fatalf("%s, %v; want the lead's version", redactRespShape(res), err)
			}
		})
		t.Run(stage+"/terminal child is closed", func(t *testing.T) {
			f := setup(t, stage)
			f.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, f.runID)
			if _, err := redact(f); !errors.Is(err, ErrRunEnvRedactClosed) {
				t.Fatalf("err = %v, want ErrRunEnvRedactClosed", err)
			}
		})
		t.Run(stage+"/terminal child with its own uploading capture is served", func(t *testing.T) {
			f := setup(t, stage)
			f.env.exec(`UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, f.runID)
			hold, capture := uuid.New(), uuid.New()
			f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
			    VALUES($1,$2,(SELECT repo_id FROM runs WHERE id=$3),$3,1,'open',$4,'redact-fixture')`, hold, f.userID, f.runID, f.workerID)
			f.env.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state)
			    VALUES($1,$2,$3,$4,$5,'redact-fixture','sha','redact-capture','uploading')`, capture, hold, f.runID, f.userID, f.workerID)
			// The hold is only the capture's parent: settle it so the capture is the sole arm.
			f.env.exec(`UPDATE recovery_custody_holds SET state='discarded',live_worker_id=NULL,live_run_id=NULL,
				released_at=now(),updated_at=now() WHERE id=$1`, hold)
			if res, err := redact(f); err != nil || !res.Protected {
				t.Fatalf("%s, %v; want protected", redactRespShape(res), err)
			}
			f.env.exec(`UPDATE recovery_captures SET state='available' WHERE id=$1`, capture)
			if _, err := redact(f); !errors.Is(err, ErrRunEnvRedactClosed) {
				t.Fatalf("available capture: err = %v, want ErrRunEnvRedactClosed", err)
			}
		})
		t.Run(stage+"/non-terminal child with a decided check is closed", func(t *testing.T) {
			f := setup(t, stage)
			if stage == "plan" {
				f.env.exec(`UPDATE cross_checks SET verdict='approve', reason_class='approve', decided_at=now() WHERE checker_run_id=$1`, f.runID)
			} else {
				f.env.exec(`UPDATE cross_checks SET outcome='failed', reason_class='timed_out', decided_at=now() WHERE checker_run_id=$1`, f.runID)
			}
			if _, err := redact(f); !errors.Is(err, ErrRunEnvRedactClosed) {
				t.Fatalf("err = %v, want ErrRunEnvRedactClosed", err)
			}
		})
	}
	// A code cross-check snapshot, deadline included, is immutable, so only a plan check is aged.
	t.Run("plan/child past its check deadline is closed", func(t *testing.T) {
		f := setup(t, "plan")
		f.env.exec(`UPDATE cross_checks SET deadline_at = now() - interval '1 minute' WHERE checker_run_id = $1`, f.runID)
		if _, err := redact(f); !errors.Is(err, ErrRunEnvRedactClosed) {
			t.Fatalf("err = %v, want ErrRunEnvRedactClosed", err)
		}
	})
}
