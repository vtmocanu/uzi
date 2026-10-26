package workersvc

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Every worker in this matrix can open recovery holds. Null live pointers make
// the prior holds stably counted, independently of heartbeat timing.
type checkerCustodyFixture struct {
	env                        codexTestEnv
	svc                        *Service
	owner, worker, lead, child uuid.UUID
	stage, harness, lane       string
}

func newCheckerCustodyFixture(t *testing.T, stage, harness, lane string, expired bool) checkerCustodyFixture {
	t.Helper()
	var x checkerCustodyFixture
	if stage == "plan" && harness == "claude" {
		f := newClaudeCheckerFix(t, BindModePinned)
		x = checkerCustodyFixture{env: f.env, svc: f.svc, owner: f.userID, worker: f.workerID, lead: f.lead, child: f.kid}
		f.env.exec("UPDATE workers SET protocol_capabilities=array_append(protocol_capabilities,'recovery_archive_v1') WHERE id=$1", f.workerID)
		if lane == "cross_check" {
			f.env.exec("UPDATE workers SET max_cross_check_slots=1,protocol_capabilities=array_append(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1", f.workerID)
		}
	} else {
		var f crossCheckContentionFixture
		if stage == "code" {
			f = codeFixture(t)
			if harness == "claude" {
				f.env.exec("UPDATE runs SET harness='codex' WHERE id=$1", f.lead)
				f.env.exec("UPDATE workers SET protocol_capabilities=array_append(array_append(protocol_capabilities,'cross_check_codex_lead_v1'),'cross_check_pins_v1') WHERE id=$1", f.workerID)
			}
			if expired {
				// InsertCodeCrossCheck freezes its deadline from the service clock.
				past := time.Now().Add(-24 * time.Hour)
				f.svc.SetClock(func() time.Time { return past })
			}
			cc, err := f.svc.SubmitCodeCrossCheck(f.env.ctx, wkrRow(t, f.env, f.workerID), f.lead, 1, strings.Repeat("a", 40), strings.Repeat("b", 40))
			f.svc.SetClock(time.Now)
			if err != nil || !cc.CheckerRunID.Valid {
				t.Fatalf("create code child: %v", err)
			}
			f.runID = uuid.UUID(cc.CheckerRunID.Bytes)
		} else if lane == "cross_check" {
			f = laneFixture(t)
		} else {
			f = newCrossCheckContentionFixture(t)
		}
		x = checkerCustodyFixture{env: f.env, svc: f.svc, owner: f.userID, worker: f.workerID, lead: f.lead, child: f.runID}
	}
	x.stage, x.harness, x.lane = stage, harness, lane
	// Make the selected model deterministic and distinguish the two families.
	x.env.exec("UPDATE users SET default_claude_model='sonnet',default_codex_model=$2 WHERE id=$1", x.owner, customCodexModel)
	return x
}

func (f checkerCustodyFixture) seed(t *testing.T, n int) {
	t.Helper()
	repo := mustRun(t, f.env, f.lead).RepoID
	for range n {
		hold, run := uuid.New(), uuid.New()
		f.env.exec(`INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity)
   VALUES($1,$2,$3,$4,1,'open',$5,'checker-custody-fixture')`, hold, f.owner, repo, run, f.worker)
		f.env.exec(`INSERT INTO recovery_captures(id,hold_id,run_id,user_id,original_worker_identity,source_sha,idempotency_key,state)
   VALUES($1,$2,$3,$4,'checker-custody-fixture','abc',$5,'needs_action')`, uuid.New(), hold, run, f.owner, uuid.NewString())
	}
	t.Cleanup(func() {
		f.env.exec("DELETE FROM recovery_captures WHERE user_id=$1", f.owner)
		f.env.exec("DELETE FROM recovery_custody_holds WHERE user_id=$1", f.owner)
	})
	f.assertCount(t, n)
}

func (f checkerCustodyFixture) assertCount(t *testing.T, n int) {
	t.Helper()
	adm, err := f.env.q.GetCustodyAdmissionForRun(f.env.ctx, store.GetCustodyAdmissionForRunParams{
		UserID: f.owner, RunID: f.child, CustodyHoldLimit: testCustodyLimit, HeartbeatCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if adm.OpenHolds != int64(n) || adm.AdmissionCountedHolds != int64(n) || adm.ContinuationExempt {
		t.Fatalf("canonical count/continuation: %+v, want %d/%d/false", adm, n, n)
	}
	var own int
	if err := f.env.pool.QueryRow(f.env.ctx, "SELECT count(*) FROM recovery_custody_holds WHERE run_id=$1", f.child).Scan(&own); err != nil {
		t.Fatal(err)
	}
	if own != 0 {
		t.Fatalf("hold-free checker has %d holds", own)
	}
}

func (f checkerCustodyFixture) snapshot(t *testing.T) string {
	t.Helper()
	var s string
	err := f.env.pool.QueryRow(f.env.ctx, `SELECT jsonb_build_object(
 'holds',(SELECT coalesce(jsonb_agg(to_jsonb(h) ORDER BY id),'[]') FROM recovery_custody_holds h WHERE user_id=$1),
 'captures',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY id),'[]') FROM recovery_captures c WHERE user_id=$1))::text`, f.owner).Scan(&s)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f checkerCustodyFixture) claim(t *testing.T) *ClaimPayload {
	t.Helper()
	w := wkrRow(t, f.env, f.worker)
	var p *ClaimPayload
	var err error
	if f.lane == "cross_check" {
		p, err = f.svc.ClaimCrossCheck(f.env.ctx, w, nil)
	} else {
		p, err = f.svc.Claim(f.env.ctx, w, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaimCrossCheckCustodyRefusalsLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, harness := range []string{"claude", "codex"} {
			refusals := []string{"parent ineligible", "owner mismatch", "stale generation", "expired", "missing protocol", "zero capacity", "exhausted capacity", "vault locked", "credential unavailable"}
			if stage == "plan" {
				refusals = append(refusals, "family protocol", "latest round")
			} else if harness == "codex" {
				refusals = append(refusals, "family protocol")
			}
			for _, refusal := range refusals {
				t.Run(stage+"/"+harness+"/"+refusal, func(t *testing.T) {
					f := newCheckerCustodyFixture(t, stage, harness, "cross_check", refusal == "expired")
					f.seed(t, 8)
					wantStatus := "queued"
					switch refusal {
					case "parent ineligible":
						// Code rejects report-only parents; plan requires an active parent.
						if stage == "code" {
							f.env.exec("UPDATE runs SET report_only=true WHERE id=$1", f.lead)
						} else {
							f.env.exec("UPDATE runs SET status='completed' WHERE id=$1", f.lead)
							wantStatus = "cancelled"
						}
					case "owner mismatch":
						foreign, _, _ := f.env.seedCodexInfra(t)
						t.Cleanup(func() { f.env.exec("DELETE FROM users WHERE id=$1", foreign) })
						f.env.exec("UPDATE runs SET user_id=$2 WHERE id=$1", f.lead, foreign)
					case "stale generation":
						f.env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", f.lead)
						wantStatus = "cancelled"
					case "expired":
						if stage == "plan" {
							f.env.exec("UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE checker_run_id=$1", f.child)
						}
					case "latest round":
						// A decided later attempt leaves this pending child stale without
						// violating the single-pending-attempt constraint.
						f.env.exec(`INSERT INTO cross_checks SELECT (jsonb_populate_record(NULL::cross_checks,
       to_jsonb(cc) || jsonb_build_object('id',$2::uuid,'round',2,'verdict','approve',
        'checker_run_id',NULL,'decided_at',now()))).* FROM cross_checks cc WHERE checker_run_id=$1`, f.child, uuid.New())
					case "missing protocol":
						protocol := "cross_check_v1"
						if stage == "code" {
							protocol = "cross_check_code_v1"
						}
						f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,$2) WHERE id=$1", f.worker, protocol)
					case "family protocol":
						protocol := "codex_runtime_v2"
						if harness == "claude" {
							protocol = "cross_check_codex_lead_v1"
						}
						f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,$2) WHERE id=$1", f.worker, protocol)
					case "zero capacity":
						f.env.exec("UPDATE workers SET max_cross_check_slots=0 WHERE id=$1", f.worker)
					case "exhausted capacity":
						sibling := crossCheckContentionFixture{env: f.env, lead: f.lead}
						sibling.runID = f.child
						other := cloneLaneChild(t, sibling)
						f.env.exec("UPDATE runs SET status='running',worker_id=$2,cross_check_lane=true WHERE id=$1", other, f.worker)
					case "vault locked":
						f.svc.SetVault(vault.NewPostgres(f.env.box, f.env.pool))
						if harness == "claude" {
							f.env.exec("UPDATE user_secrets SET sealed_with='dek' WHERE user_id=$1 AND kind='anthropic_token'", f.owner)
						} else {
							f.env.exec("UPDATE codex_provider_account SET sealed_with='dek' WHERE user_id=$1", f.owner)
						}
					case "credential unavailable":
						if harness == "claude" {
							f.env.exec("UPDATE user_secrets SET disabled_at=now(),enablement_rev=enablement_rev+1 WHERE user_id=$1 AND kind='anthropic_token'", f.owner)
							wantStatus = "failed"
						} else {
							// Corrupt ciphertext is an unavailable credential at real assembly.
							f.env.exec("UPDATE codex_provider_account SET sealed_login=$2 WHERE user_id=$1", f.owner, []byte("unavailable-local-fixture"))
							wantStatus = "failed"
						}
					}
					before := f.snapshot(t)
					if p := f.claim(t); p != nil {
						t.Fatal("ineligible checker delivered credentials at custody cap")
					}
					child := mustRun(t, f.env, f.child)
					if child.Status != wantStatus {
						t.Fatalf("refused child status=%s want %s", child.Status, wantStatus)
					}
					f.assertCount(t, 8)
					if f.snapshot(t) != before {
						t.Fatal("refusal changed prior custody rows")
					}
				})
			}
		}
	}
}

func TestClaimCrossCheckCustodyLiveDB(t *testing.T) {
	for _, stage := range []string{"plan", "code"} {
		for _, harness := range []string{"claude", "codex"} {
			lanes := []string{"cross_check"}
			if stage == "plan" {
				lanes = append(lanes, "run")
			}
			for _, lane := range lanes {
				for _, n := range []int{7, 8, 9} {
					t.Run(fmt.Sprintf("%s/%s/%s/holds-%d", stage, harness, lane, n), func(t *testing.T) {
						f := newCheckerCustodyFixture(t, stage, harness, lane, false)
						f.seed(t, n)
						before := f.snapshot(t)
						p := f.claim(t)
						if p == nil {
							t.Fatal("hold-free checker was blocked by custody admission")
						}
						r := mustRun(t, f.env, f.child)
						if p.RunID != f.child.String() || p.Kind != "cross_check" || p.ClaimGeneration != 1 ||
							p.CrossCheck == nil || p.CrossCheck.Stage != stage || p.CrossCheck.LeadRunID != f.lead.String() ||
							r.Status != "claimed" || r.ClaimGeneration != 1 || r.Harness != harness || r.CrossCheckLane != (lane == "cross_check") ||
							!r.WorkerID.Valid || uuid.UUID(r.WorkerID.Bytes) != f.worker {
							t.Fatalf("checker identity/lane/stage mismatch: payload=%+v run=%+v", p, r)
						}
						model := customCodexModel
						if harness == "claude" {
							model = "sonnet"
						}
						if p.Config.DefaultModel == nil || *p.Config.DefaultModel != model {
							t.Fatalf("model=%v want %s", p.Config.DefaultModel, model)
						}
						if harness == "claude" {
							if p.Secrets.AnthropicOAuthToken == "" || p.Secrets.Codex != nil || !r.AnthropicSecretID.Valid || r.CodexSecretID.Valid {
								t.Fatal("Claude credential family mismatch")
							}
						} else if p.Secrets.Codex == nil || p.Secrets.AnthropicOAuthToken != "" || !r.CodexSecretID.Valid || r.AnthropicSecretID.Valid {
							t.Fatal("Codex credential family mismatch")
						}
						f.assertCount(t, n)
						if after := f.snapshot(t); after != before {
							t.Fatal("checker changed prior custody hold/capture rows")
						}
					})
				}
			}
		}
	}
}
