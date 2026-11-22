package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codexPlanningFixture uses the subscription fixture's real linked login and frozen binding.
func codexPlanningFixture(t *testing.T) (*isoFix, codexRunFixture) {
	t.Helper()
	env := setupCodexLiveDB(t)
	cf := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, cf.userID)
	run := mustRun(t, env, cf.runID)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	return &isoFix{env: env, userID: cf.userID, workerID: cf.workerID, repo: uuid.UUID(run.RepoID.Bytes), svc: svc}, cf
}

func codexPlanningCaps(extra ...string) []string {
	return append([]string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CompletionInterlockV1}, extra...)
}

func insertCodexPlanningRun(t *testing.T, f *isoFix, cf codexRunFixture, kind string) uuid.UUID {
	t.Helper()
	id := f.insertPhaseRun(t, kind, "claimed", f.workerID)
	f.env.exec(`UPDATE runs SET harness='codex' WHERE id=$1`, id)
	if err := f.svc.FreezeCodexBinding(f.env.ctx, f.userID, id, cf.aliasID, codexAuthModeSubscription); err != nil {
		t.Fatalf("FreezeCodexBinding: %v", err)
	}
	return id
}

func TestCodexClaimPlanPhasePerKindLiveDB(t *testing.T) {
	f, cf := codexPlanningFixture(t)
	f.env.exec(`UPDATE users SET default_codex_model='gpt-6-sol', default_codex_effort='medium',
		planning_claude_model='opus', planning_claude_effort='max' WHERE id=$1`, f.userID)
	w := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: codexPlanningCaps(capability.CodexPhaseModelsV1)}
	for _, kind := range phaseLeadKinds {
		t.Run(kind, func(t *testing.T) {
			id := insertCodexPlanningRun(t, f, cf, kind)
			for _, tc := range []struct {
				name            string
				model, effort   any
				implementEffort string
				want            map[string]any
			}{
				{"both", "gpt-6-astra", "high", "medium", map[string]any{"model": "gpt-6-astra", "effort": "high", "model_source": "user_planning_default", "effort_source": "user_planning_default"}},
				{"model only", "gpt-6-astra", nil, "medium", map[string]any{"model": "gpt-6-astra", "effort": "medium", "model_source": "user_planning_default", "effort_source": "user_default"}},
				{"effort only", nil, "high", "medium", map[string]any{"model": "gpt-6-sol", "effort": "high", "model_source": "user_default", "effort_source": "user_planning_default"}},
				{"runtime implement", nil, "high", "runtime", map[string]any{"model": "gpt-6-sol", "effort": "high", "model_source": "user_default", "effort_source": "user_planning_default"}},
				{"nothing stored", nil, nil, "medium", nil},
			} {
				t.Run(tc.name, func(t *testing.T) {
					f.env.exec(`UPDATE users SET planning_codex_model=$2, planning_codex_effort=$3,
						default_codex_effort=$4 WHERE id=$1`, f.userID, tc.model, tc.effort, tc.implementEffort)
					worker := w
					if tc.want == nil {
						worker.ProtocolCapabilities = codexPlanningCaps()
					}
					p, err := f.svc.assembleClaim(f.env.ctx, worker, mustRun(t, f.env, id))
					if err != nil {
						t.Fatalf("assembleClaim: %v", err)
					}
					if got := planPhaseOnWire(t, p); !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("plan_phase=%v want %v", got, tc.want)
					}
					if derefOr(p.Config.DefaultModel, "") != "gpt-6-sol" || derefOr(p.Config.DefaultModelSource, "") != "user_default" ||
						derefOr(p.Config.DefaultEffortSource, "") != "user_default" {
						t.Fatalf("implement model/source or effort source changed: %+v", p.Config)
					}
					if tc.implementEffort == "runtime" {
						if p.Config.DefaultEffort != nil {
							t.Fatalf("runtime implement effort=%v, want nil", p.Config.DefaultEffort)
						}
					} else if derefOr(p.Config.DefaultEffort, "") != tc.implementEffort {
						t.Fatalf("implement effort=%v want %s", p.Config.DefaultEffort, tc.implementEffort)
					}
				})
			}
		})
	}
}

type codexPlanningReadErrorStore struct {
	Store
	err   error
	calls int
}

func (q *codexPlanningReadErrorStore) GetUserPlanningCodex(context.Context, uuid.UUID) (store.GetUserPlanningCodexRow, error) {
	q.calls++
	return store.GetUserPlanningCodexRow{}, q.err
}

func TestCodexClaimPlanningReadErrorFailsLiveDB(t *testing.T) {
	f, cf := codexPlanningFixture(t)
	sentinel := errors.New("planning Codex read failed")
	q := &codexPlanningReadErrorStore{Store: f.env.q, err: sentinel}
	f.svc.q = q
	w := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: codexPlanningCaps(capability.CodexPhaseModelsV1)}
	p, err := f.svc.assembleClaim(f.env.ctx, w, mustRun(t, f.env, cf.runID))
	if p != nil || !errors.Is(err, sentinel) || q.calls != 1 {
		t.Fatalf("claim=(%v,%v), reads=%d; want nil, planning sentinel, one read", p != nil, err, q.calls)
	}
}

func TestCodexClaimPlanningSettingsRaceRequeuesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []string
		want error
	}{
		{"phase missing", codexPlanningCaps(capability.CodexCustomModelV1), errPhaseModelsCapabilityMissing},
		{"custom missing", codexPlanningCaps(capability.CodexPhaseModelsV1), errCustomModelCapabilityMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cf := codexPlanningFixture(t)
			incapable := f.seedWorker(t, laneWorkerSpec{protoCaps: tc.caps})
			f.env.exec(`UPDATE runs SET status='queued',worker_id=NULL WHERE id=$1`, cf.runID)
			claim := func(w store.Worker) (store.Run, error) {
				params := laneClaimParams(f.userID, w)
				params.CodexCuratedModels = codexCuratedModelsSlice()
				return f.env.q.ClaimRun(f.env.ctx, params)
			}
			run, err := claim(incapable)
			if err != nil || run.ID != cf.runID {
				t.Fatalf("initial claim=(%s,%v), want run %s while planning is NULL", run.ID, err, cf.runID)
			}
			// Commit the settings write after ClaimRun, before assembly. No timing assumption.
			f.env.exec(`UPDATE users SET planning_codex_model='private-plan',planning_codex_effort='high' WHERE id=$1`, f.userID)
			p, aerr := f.svc.assembleClaim(f.env.ctx, incapable, run)
			if p != nil || !errors.Is(aerr, tc.want) {
				t.Fatalf("assembly=(%v,%v), want %v", p != nil, aerr, tc.want)
			}
			if got, err := f.svc.finishRunClaim(f.env.ctx, mustRun(t, f.env, cf.runID), p, aerr, claimRecoveryIdentity{workerID: incapable.ID}); err != nil || got != nil {
				t.Fatalf("finishRunClaim=(%v,%v), want idle", got != nil, err)
			}
			queued := mustRun(t, f.env, cf.runID)
			if queued.Status != "queued" || queued.FailOrigin.Valid {
				t.Fatalf("after race: status=%s fail_origin=%v", queued.Status, queued.FailOrigin)
			}
			if _, err := claim(incapable); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("second incapable ClaimRun=%v, want exclusion", err)
			}
			// Exclusion, rather than another claim/requeue, is the non-looping guarantee.
			// Codex claim provenance is intentionally retained (00493): it is not ordinary
			// expiring affinity and must not be cleared to admit a different worker here.
			// Fresh mixed-fleet capable placement is covered by the store mirror matrix.
			after := mustRun(t, f.env, cf.runID)
			if after.RequeueCount != queued.RequeueCount || after.ClaimGeneration != queued.ClaimGeneration {
				t.Fatal("excluded claimant advanced the requeue count or claim generation")
			}
		})
	}
}

func TestCodexClaimPlanPhaseExcludedLanesLiveDB(t *testing.T) {
	t.Run("task review", func(t *testing.T) {
		f, cf := codexPlanningFixture(t)
		target := insertCodexPlanningRun(t, f, cf, "task")
		f.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, target)
		review := insertCodexPlanningRun(t, f, cf, "task")
		f.env.exec(`UPDATE runs SET review_target_run_id=$2,base_branch='main',dispatched_at=now() WHERE id=$1`, review, target)
		f.env.exec(`UPDATE users SET planning_codex_model='private-plan',planning_codex_effort='high' WHERE id=$1`, f.userID)
		w := store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: codexPlanningCaps()}
		p, err := f.svc.assembleClaim(f.env.ctx, w, mustRun(t, f.env, review))
		if err != nil {
			t.Fatalf("review without planning/custom capabilities: %v", err)
		}
		if planPhaseOnWire(t, p) != nil {
			t.Fatal("review carries plan_phase")
		}
	})
	t.Run("checker", func(t *testing.T) {
		f := laneFixture(t)
		f.env.exec(`UPDATE users SET default_codex_model='gpt-6-sol',planning_codex_model='private-plan',planning_codex_effort='high' WHERE id=$1`, f.userID)
		f.env.exec(`UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,$2) WHERE id=$1`, f.workerID, capability.CodexCustomModelV1)
		p := laneClaim(t, f, "cross_check", nil)
		if p == nil || p.CrossCheck == nil {
			t.Fatal("Codex checker was not claimed")
		}
		if planPhaseOnWire(t, p) != nil {
			t.Fatal("checker carries plan_phase")
		}
	})
	t.Run("egress bound", func(t *testing.T) {
		// Egress-bound runs must be Claude: the schema forbids Codex in this lane.
		f := newIsoFix(t)
		f.env.exec(`UPDATE users SET planning_codex_model='private-plan',planning_codex_effort='high',
			planning_claude_model='opus',planning_claude_effort='high' WHERE id=$1`, f.userID)
		id := f.queuedRun(t, int64(f.nextIID()), true)
		f.env.exec(`UPDATE runs SET status='claimed',worker_id=$2 WHERE id=$1`, id, f.workerID)
		w := phaseWorker(f, false)
		w.IsolatedLane = true
		p, err := f.svc.assembleClaim(f.env.ctx, w, mustRun(t, f.env, id))
		if err != nil {
			t.Fatalf("egress assembly: %v", err)
		}
		if planPhaseOnWire(t, p) != nil {
			t.Fatal("egress-bound claim carries plan_phase")
		}
	})
}
