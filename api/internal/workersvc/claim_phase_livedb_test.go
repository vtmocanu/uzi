package workersvc

import (
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// claim_phase_livedb_test.go pins PRD #3252 M2 on the REAL claim path against Postgres: the
// plan_phase claim key appears on every lead-lane Claude kind when the owner has a planning value
// and on no excluded kind, the wire is unchanged when nothing is set, a worker without
// claude_phase_models_v1 never receives a plan_phase (ClaimRun excludes it, assembly requeues it),
// and the requeue cannot loop. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh).

var phaseLeadKinds = []string{"issue", "ci_fix", "self_improve", "prompt", "mr_rework", "task"}

// phaseCaps is the protocol capability set of a worker that can take a Claude planning phase.
func phaseCaps(withPhase bool) []string {
	caps := []string{capability.CompletionInterlockV1}
	if withPhase {
		caps = append(caps, capability.ClaudePhaseModelsV1)
	}
	return caps
}

// insertPhaseRun inserts a Claude run of kind in the given status, shaped to satisfy the kind's
// row constraints, and returns its id. worker is the claimed-by worker (uuid.Nil = none).
func (f *isoFix) insertPhaseRun(t *testing.T, kind, status string, worker uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var w any
	if worker != uuid.Nil {
		w = worker
	}
	switch kind {
	case "issue", "self_improve":
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id, harness)
		            VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, 'claude')`, id, f.userID, f.repo, kind, f.nextIID(), status, w)
	case "ci_fix":
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_id, pipeline_ref, status, worker_id, harness)
		            VALUES ($1, $2, $3, 'ci_fix', 't', 'd', $4, $5, $6, $7, 'claude')`, id, f.userID, f.repo, f.nextIID(), "ref-"+id.String(), status, w)
	case "mr_rework":
		source, mrIID := uuid.New(), f.nextIID()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status, harness)
		            VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, 'opened', 'completed', 'claude')`, source, f.userID, f.repo, f.nextIID(), "agent/issue-"+source.String(), mrIID)
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, target_run_id, status, worker_id, harness)
		            VALUES ($1, $2, $3, 'mr_rework', 't', 'd', $4, $5, $6, $7, $8, 'claude')`, id, f.userID, f.repo, "agent/issue-"+source.String(), mrIID, source, status, w)
	default: // prompt, self_improve, task
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, branch, status, worker_id, harness)
		            VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, $7, 'claude')`, id, f.userID, f.repo, kind, "uzi/"+kind+"/"+id.String(), status, w)
	}
	return id
}

// nextIID hands out issue, pipeline and MR numbers unique within the fixture's repo.
func (f *isoFix) nextIID() int { return 90000 + int(phaseIID.Add(1)) }

var phaseIID atomic.Int64

func (f *isoFix) setPlanning(model, effort any) {
	f.env.exec(`UPDATE users SET planning_claude_model = $2, planning_claude_effort = $3 WHERE id = $1`, f.userID, model, effort)
}

func phaseWorker(f *isoFix, withPhase bool) store.Worker {
	return store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: phaseCaps(withPhase)}
}

// planPhaseOnWire returns the claim config's plan_phase object, or nil when the key is absent.
func planPhaseOnWire(t *testing.T, p *ClaimPayload) map[string]any {
	t.Helper()
	raw, ok := claimConfigKeys(t, p)["plan_phase"]
	if !ok {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("plan_phase is not an object: %s", raw)
	}
	return out
}

func TestClaimPlanPhasePerKindLiveDB(t *testing.T) {
	f := newIsoFix(t)
	f.env.exec(`UPDATE users SET default_claude_model = 'sonnet', default_effort = 'medium' WHERE id = $1`, f.userID)
	f.setPlanning("opus", "high")
	for _, kind := range phaseLeadKinds {
		t.Run(kind, func(t *testing.T) {
			run := f.insertPhaseRun(t, kind, "claimed", f.workerID)
			p, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, true), mustRun(t, f.env, run))
			if err != nil {
				t.Fatalf("assembleClaim: %v", err)
			}
			got := planPhaseOnWire(t, p)
			want := map[string]any{"model": "opus", "effort": "high", "model_source": "user_planning_default", "effort_source": "user_planning_default"}
			if len(got) != len(want) {
				t.Fatalf("plan_phase = %v, want %v", got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("plan_phase = %v, want %v", got, want)
				}
			}
			// The implement pair is unchanged next to it.
			if derefOr(p.Config.DefaultModel, "") != "sonnet" || derefOr(p.Config.DefaultEffort, "") != "medium" {
				t.Fatalf("implement pair = %v/%v, want sonnet/medium", p.Config.DefaultModel, p.Config.DefaultEffort)
			}
		})
	}
}

func TestClaimPlanPhaseAbsentOnExcludedClaimsLiveDB(t *testing.T) {
	f := newIsoFix(t)
	f.setPlanning("opus", "high")

	t.Run("task review", func(t *testing.T) {
		target := f.insertPhaseRun(t, "task", "completed", uuid.Nil)
		review := uuid.New()
		f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, worker_id, harness)
		            VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), 't', 'd', 'claimed', $6, 'claude')`, review, f.userID, f.repo, "uzi/task/"+review.String(), target, f.workerID)
		p, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, false), mustRun(t, f.env, review))
		if err != nil {
			t.Fatalf("assembleClaim (worker without the capability must still take a review): %v", err)
		}
		if planPhaseOnWire(t, p) != nil {
			t.Fatal("a task review claim carries plan_phase")
		}
	})

	t.Run("egress-bound run", func(t *testing.T) {
		bound := f.queuedRun(t, 801, true)
		f.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2 WHERE id = $1`, bound, f.workerID)
		lane := phaseWorker(f, false)
		lane.IsolatedLane = true
		p, err := f.svc.assembleClaim(f.env.ctx, lane, mustRun(t, f.env, bound))
		if err != nil {
			t.Fatalf("assembleClaim: %v", err)
		}
		if planPhaseOnWire(t, p) != nil {
			t.Fatal("an egress-bound claim carries plan_phase")
		}
	})

	t.Run("cross-check checker", func(t *testing.T) {
		cf, _ := newClaudeCodeCheckerFix(t)
		cf.env.exec(`UPDATE users SET planning_claude_model = 'opus', planning_claude_effort = 'high' WHERE id = $1`, cf.userID)
		p := laneClaim(t, cf, "cross_check", nil)
		if p == nil || p.CrossCheck == nil {
			t.Fatalf("checker claim not delivered: %+v", p)
		}
		if planPhaseOnWire(t, p) != nil {
			t.Fatal("a checker claim carries plan_phase")
		}
	})
}

func TestClaimPlanPhaseNothingSetIsByteIdenticalLiveDB(t *testing.T) {
	f := newIsoFix(t)
	f.env.exec(`UPDATE users SET default_claude_model = 'sonnet' WHERE id = $1`, f.userID)
	for _, kind := range phaseLeadKinds {
		run := f.insertPhaseRun(t, kind, "claimed", f.workerID)
		// Worker without the capability: nothing set means no requirement and no key.
		p, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, false), mustRun(t, f.env, run))
		if err != nil {
			t.Fatalf("%s: assembleClaim: %v", kind, err)
		}
		if _, ok := claimConfigKeys(t, p)["plan_phase"]; ok {
			t.Fatalf("%s: plan_phase present with no planning value stored", kind)
		}
	}
	// A blank-after-trim stored value is unset in Go, so it ships nothing and needs no capability.
	f.setPlanning("   ", nil)
	run := f.insertPhaseRun(t, "issue", "claimed", f.workerID)
	p, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, false), mustRun(t, f.env, run))
	if err != nil || planPhaseOnWire(t, p) != nil {
		t.Fatalf("blank planning model: err=%v plan_phase=%v, want none", err, planPhaseOnWire(t, p))
	}
}

func TestClaimPlanPhaseCapabilityRequeueLiveDB(t *testing.T) {
	f := newIsoFix(t)
	f.setPlanning(nil, "high")
	run := f.insertPhaseRun(t, "issue", "claimed", f.workerID)

	p, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, false), mustRun(t, f.env, run))
	if !errors.Is(err, errPhaseModelsCapabilityMissing) || p != nil {
		t.Fatalf("assembleClaim (incapable) = (%v, %v), want errPhaseModelsCapabilityMissing", p != nil, err)
	}
	if got, rerr := f.svc.finishRunClaim(f.env.ctx, mustRun(t, f.env, run), p, err, claimRecoveryIdentity{workerID: f.workerID}); rerr != nil || got != nil {
		t.Fatalf("finishRunClaim = (%v, %v), want idle", got != nil, rerr)
	}
	if s := runStatusOf(t, f.env, run); s != "queued" {
		t.Fatalf("run status = %q, want queued (requeued, never failed)", s)
	}

	// The capable worker assembles it and gets the planning effort with the implement model inherited.
	f.env.exec(`UPDATE runs SET status = 'claimed', worker_id = $2 WHERE id = $1`, run, f.workerID)
	p, err = f.svc.assembleClaim(f.env.ctx, phaseWorker(f, true), mustRun(t, f.env, run))
	if err != nil {
		t.Fatalf("assembleClaim (capable): %v", err)
	}
	if got := planPhaseOnWire(t, p); got == nil || got["effort"] != "high" || got["effort_source"] != "user_planning_default" {
		t.Fatalf("plan_phase = %v, want the planning effort", got)
	}
}

// TestClaimPlanPhaseRequeueDoesNotLoopLiveDB: both workers online; the owner sets a planning value
// between ClaimRun and assembly on the incapable worker. The requeue happens once, ClaimRun then
// excludes the incapable worker, and the capable worker claims.
func TestClaimPlanPhaseRequeueDoesNotLoopLiveDB(t *testing.T) {
	f := newIsoFix(t)
	incapable := f.seedWorker(t, laneWorkerSpec{protoCaps: phaseCaps(false)})
	capable := f.seedWorker(t, laneWorkerSpec{protoCaps: phaseCaps(true)})
	run := f.insertPhaseRun(t, "issue", "queued", uuid.Nil)

	// ClaimRun commits before assembly: the incapable worker takes the run while nothing is set.
	claimed, err := f.env.q.ClaimRun(f.env.ctx, laneClaimParams(f.userID, incapable))
	if err != nil || claimed.ID != run {
		t.Fatalf("raw ClaimRun = (%v, %v), want run %s", claimed.ID, err, run)
	}
	f.setPlanning("opus", nil) // the settings write between the claim and its assembly

	p, aerr := f.svc.assembleClaim(f.env.ctx, incapable, mustRun(t, f.env, run))
	if !errors.Is(aerr, errPhaseModelsCapabilityMissing) {
		t.Fatalf("assembleClaim = (%v, %v), want errPhaseModelsCapabilityMissing", p != nil, aerr)
	}
	if got, rerr := f.svc.finishRunClaim(f.env.ctx, mustRun(t, f.env, run), p, aerr, claimRecoveryIdentity{workerID: incapable.ID}); rerr != nil || got != nil {
		t.Fatalf("finishRunClaim = (%v, %v), want idle", got != nil, rerr)
	}
	if s := runStatusOf(t, f.env, run); s != "queued" {
		t.Fatalf("run status = %q, want queued", s)
	}
	// Second pass: the incapable worker is excluded by ClaimRun itself (no second requeue).
	if got, cerr := f.svc.Claim(f.env.ctx, incapable, nil); cerr != nil || got != nil {
		t.Fatalf("Claim(incapable) = (%v, %v), want idle", got != nil, cerr)
	}
	// The requeue keeps the prior worker's bounded resume affinity (as the custom-Codex requeue
	// does); let it lapse so the capable peer may claim.
	f.env.exec(`UPDATE runs SET updated_at = now() - interval '1 hour' WHERE id = $1`, run)
	got, cerr := f.svc.Claim(f.env.ctx, capable, nil)
	if cerr != nil || got == nil || got.RunID != run.String() {
		t.Fatalf("Claim(capable) = (%v, %v), want run %s", got, cerr, run)
	}
	if pp := planPhaseOnWire(t, got); pp == nil || pp["model"] != "opus" {
		t.Fatalf("plan_phase = %v, want the planning model", pp)
	}
}

// TestClaimPlanPhaseReclaimResolvesStoredPairLiveDB: a reclaim resolves the pair stored at that
// claim, not the one of the first claim.
func TestClaimPlanPhaseReclaimResolvesStoredPairLiveDB(t *testing.T) {
	f := newIsoFix(t)
	f.setPlanning("opus", "high")
	run := f.insertPhaseRun(t, "issue", "claimed", f.workerID)
	first, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, true), mustRun(t, f.env, run))
	if err != nil || planPhaseOnWire(t, first)["model"] != "opus" {
		t.Fatalf("first claim: %v %v", err, planPhaseOnWire(t, first))
	}
	f.setPlanning("fable", nil)
	again, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, true), mustRun(t, f.env, run))
	if err != nil {
		t.Fatal(err)
	}
	if pp := planPhaseOnWire(t, again); pp["model"] != "fable" || pp["effort_source"] != "builtin" {
		t.Fatalf("reclaim plan_phase = %v, want fable with the inherited builtin effort", pp)
	}
	f.setPlanning(nil, nil)
	cleared, err := f.svc.assembleClaim(f.env.ctx, phaseWorker(f, false), mustRun(t, f.env, run))
	if err != nil || planPhaseOnWire(t, cleared) != nil {
		t.Fatalf("cleared reclaim: err=%v plan_phase=%v, want none", err, planPhaseOnWire(t, cleared))
	}
}

// TestClaimPlanPhaseCodexClaimUnaffectedLiveDB: a Claude planning value changes nothing on a Codex
// claim, which needs no capability and carries no plan_phase.
func TestClaimPlanPhaseCodexClaimUnaffectedLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cf := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, cf.userID)
	env.exec(`UPDATE users SET planning_claude_model = 'opus', planning_claude_effort = 'high' WHERE id = $1`, cf.userID)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	w := store.Worker{ID: cf.workerID, UserID: cf.userID, ProtocolCapabilities: []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3, capability.CodexCustomModelV1}}
	p, err := svc.assembleClaim(env.ctx, w, mustRun(t, env, cf.runID))
	if err != nil {
		t.Fatalf("Codex claim with Claude planning set: %v", err)
	}
	if _, ok := claimConfigKeys(t, p)["plan_phase"]; ok {
		t.Fatal("a Codex claim carries plan_phase")
	}
}

// TestCheckProviderModelsRequiresMappedPlanningModelLiveDB: a provider-bound Claude lead-lane run
// whose owner's planning model would apply must also map that model (refused as "planning default"),
// and a run that excludes the planning lane (a task review) does not.
func TestCheckProviderModelsRequiresMappedPlanningModelLiveDB(t *testing.T) {
	f := newIsoFix(t)
	f.env.exec(`UPDATE users SET default_claude_model = 'sonnet' WHERE id = $1`, f.userID)
	// Independent of a shared database: a global allocation row left by another suite can enable a
	// builtin template (e.g. architect, opus) whose model would also need mapping. A user-scoped
	// allocation overrides the global one, so disable every non-user template for this user.
	f.env.exec(`INSERT INTO agent_template_allocations (template_id, user_id, enabled)
	            SELECT id, $1, false FROM agent_templates WHERE scope <> 'user' ON CONFLICT DO NOTHING`, f.userID)
	src := providerSource{ModelMap: providers.ModelMap{"sonnet": providers.ModelEntry{Model: "vendor-sonnet"}}}
	run := f.insertPhaseRun(t, "issue", "queued", uuid.Nil)

	check := func(id uuid.UUID) error {
		return f.svc.checkProviderModels(f.env.ctx, f.env.q, mustRun(t, f.env, id), src)
	}
	f.setPlanning(nil, "high") // an effort alone adds no model to map
	if err := check(run); err != nil {
		t.Fatalf("planning effort only: %v", err)
	}
	f.setPlanning("opus", nil)
	var unmapped *ProviderModelUnmappedError
	if err := check(run); !errors.As(err, &unmapped) || unmapped.Model != "opus" || unmapped.Source != "planning default" {
		t.Fatalf("unmapped planning model: err = %v, want ProviderModelUnmappedError{opus, planning default}", err)
	}
	f.setPlanning("sonnet", nil)
	if err := check(run); err != nil {
		t.Fatalf("mapped planning model: %v", err)
	}

	f.setPlanning("opus", nil)
	target := f.insertPhaseRun(t, "task", "completed", uuid.Nil)
	review := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, branch, base_branch, review_target_run_id, dispatched_at, issue_title, issue_description, status, harness)
	            VALUES ($1, $2, $3, 'task', $4, 'main', $5, now(), 't', 'd', 'queued', 'claude')`, review, f.userID, f.repo, "uzi/task/"+review.String(), target)
	if err := check(review); err != nil {
		t.Fatalf("a task review never plans, so the planning model needs no mapping: %v", err)
	}
}

func TestCodexClaimPlanCapabilitiesLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	cf := newSubscriptionFixture(t, env)
	resealBotPAT(t, env, cf.userID)
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	env.exec(`UPDATE users SET planning_codex_model='private-plan', planning_codex_effort='high' WHERE id=$1`, cf.userID)
	base := []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}
	for _, tc := range []struct {
		name  string
		extra []string
		want  error
	}{
		{"missing phase", []string{capability.CodexCustomModelV1}, errPhaseModelsCapabilityMissing},
		{"missing custom", []string{capability.CodexPhaseModelsV1}, errCustomModelCapabilityMissing},
		{"both", []string{capability.CodexPhaseModelsV1, capability.CodexCustomModelV1}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := store.Worker{ID: cf.workerID, UserID: cf.userID, ProtocolCapabilities: append(append([]string{}, base...), tc.extra...)}
			p, err := svc.assembleClaim(env.ctx, w, mustRun(t, env, cf.runID))
			if tc.want != nil {
				if !errors.Is(err, tc.want) || p != nil {
					t.Fatalf("claim=(%v,%v) want %v", p != nil, err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			plan := planPhaseOnWire(t, p)
			if plan["model"] != "private-plan" || plan["effort"] != "high" || plan["model_source"] != "user_planning_default" {
				t.Fatalf("plan=%v", plan)
			}
		})
	}
}
