package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func roundsPinsFixture(t *testing.T) (codexTestEnv, *Service, store.Worker, uuid.UUID, PlanCrossCheckCandidate) {
	t.Helper()
	env, svc, w, lead, c := newAutomaticRoundsFixture(t, 2)
	resealBotPAT(t, env, w.UserID)
	seedDefaultAnthropicToken(t, env, w.UserID)
	env.exec("UPDATE users SET default_codex_model='gpt-6-sol',default_codex_effort='high',ephemeral_workers_enabled=true WHERE id=$1", w.UserID)
	env.exec("UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now(),max_concurrent_runs=3 WHERE id=$1", w.ID, append(checkerPinCaps(true, true), capability.CrossCheckRoundsV1))
	return env, svc, w, lead, c
}

func roundsPinsSubmit(t *testing.T, env codexTestEnv, svc *Service, w store.Worker, lead uuid.UUID, c PlanCrossCheckCandidate, round int32) store.CrossCheck {
	t.Helper()
	cc, err := svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, round)
	if err != nil {
		t.Fatal(err)
	}
	if !cc.AutomaticRoundsEnabled || cc.AutomaticRevisionLimit != 2 || cc.Round != round {
		t.Fatalf("attempt snapshot: %+v", cc)
	}
	return cc
}

func roundsPinsClaim(t *testing.T, env codexTestEnv, svc *Service, w store.Worker, cc store.CrossCheck) *ClaimPayload {
	t.Helper()
	p, err := svc.Claim(env.ctx, wkrRow(t, env, w.ID), nil)
	if err != nil || p == nil || p.CrossCheck == nil || p.CrossCheck.Round != cc.Round {
		t.Fatalf("round %d claim: payload=%v err=%v", cc.Round, p != nil, err)
	}
	return p
}

func roundsPinsRevise(t *testing.T, env codexTestEnv, svc *Service, w store.Worker, cc store.CrossCheck) {
	t.Helper()
	child := mustRun(t, env, uuid.UUID(cc.CheckerRunID.Bytes))
	if _, err := svc.DecidePlanCrossCheck(env.ctx, w, child.ID, child.ClaimGeneration, "revise", "revise", []byte(`{"summary":"revise","items":[]}`)); err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE runs SET status='completed',claim_released_at=now() WHERE id=$1", child.ID)
}

func TestCrossCheckRoundsPinsAttemptProvenanceLiveDB(t *testing.T) {
	env, svc, w, lead, c := roundsPinsFixture(t)
	first := roundsPinsSubmit(t, env, svc, w, lead, c, 1)
	p := roundsPinsClaim(t, env, svc, w, first)
	if p.Config.DefaultModel == nil || *p.Config.DefaultModel != "gpt-6-sol" || p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != "high" || p.CrossCheck.ModelSource == nil || *p.CrossCheck.ModelSource != "worker default" || p.CrossCheck.EffortSource == nil || *p.CrossCheck.EffortSource != "worker default" {
		t.Fatal("round 1 defaults not delivered")
	}
	roundsPinsRevise(t, env, svc, w, first)
	before, err := env.q.GetExactPlanCrossCheck(env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: lead, Round: 1})
	if err != nil {
		t.Fatal(err)
	}
	env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort) VALUES($1,'plan','codex','gpt-6-astra','xhigh')", w.UserID)
	env.exec("UPDATE users SET default_codex_model='gpt-5.6-sol',default_codex_effort='low' WHERE id=$1", w.UserID)
	second := roundsPinsSubmit(t, env, svc, w, lead, c, 2)
	p = roundsPinsClaim(t, env, svc, w, second)
	if p.Config.DefaultModel == nil || *p.Config.DefaultModel != "gpt-6-astra" || p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != "xhigh" ||
		p.CrossCheck.ModelSource == nil || *p.CrossCheck.ModelSource != "pin" || p.CrossCheck.EffortSource == nil || *p.CrossCheck.EffortSource != "pin" {
		t.Fatal("round 2 pin resolution not delivered")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Config struct {
			Model  *string `json:"default_model"`
			Effort *string `json:"default_effort"`
		} `json:"config"`
		Check struct {
			Round        int32   `json:"round"`
			ModelSource  *string `json:"model_source"`
			EffortSource *string `json:"effort_source"`
		} `json:"cross_check"`
	}
	if err = json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Config.Model == nil || *wire.Config.Model != "gpt-6-astra" || wire.Config.Effort == nil || *wire.Config.Effort != "xhigh" ||
		wire.Check.Round != 2 || wire.Check.ModelSource == nil || *wire.Check.ModelSource != "pin" || wire.Check.EffortSource == nil || *wire.Check.EffortSource != "pin" {
		t.Fatal("marshaled round 2 lost delivered model/effort/provenance")
	}
	latest, err := svc.PlanCrossCheckSummary(env.ctx, w.UserID, lead)
	if err != nil || latest == nil || latest.CheckerModel == nil || *latest.CheckerModel != "gpt-6-astra" || latest.CheckerEffort == nil || *latest.CheckerEffort != "xhigh" ||
		latest.CheckerModelSource == nil || *latest.CheckerModelSource != "pin" || latest.CheckerEffortSource == nil || *latest.CheckerEffortSource != "pin" {
		t.Fatalf("latest summary did not select round 2: %+v err=%v", latest, err)
	}
	after, err := env.q.GetExactPlanCrossCheck(env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: lead, Round: 1})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("round 2 overwrote round 1 provenance: %v", err)
	}
}

func TestCrossCheckRoundsPinsCapabilityRaceLiveDB(t *testing.T) {
	env, svc, w, lead, c := roundsPinsFixture(t)
	first := roundsPinsSubmit(t, env, svc, w, lead, c, 1)
	roundsPinsClaim(t, env, svc, w, first)
	roundsPinsRevise(t, env, svc, w, first)
	env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, append(checkerPinCaps(false, false), capability.CrossCheckRoundsV1))
	second := roundsPinsSubmit(t, env, svc, w, lead, c, 2)
	childID := uuid.UUID(second.CheckerRunID.Bytes)
	before := mustRun(t, env, childID)
	assembled := false
	svc.claimHooks = &claimTestHooks{
		beforeAssembly: func(_ context.Context, _ store.Run) {
			env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model) VALUES($1,'plan','codex','gpt-6-astra')", w.UserID)
		},
		afterMint: func(_ context.Context, _ store.Run) { t.Fatal("capability race minted credentials") },
		afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
			assembled = true
			during := mustRun(t, env, childID)
			if len(during.CodexCapHash) != 0 || during.CodexClaimEpoch != before.CodexClaimEpoch {
				t.Fatal("preflight refusal minted credentials")
			}
			if p != nil || !errors.Is(err, errCrossCheckPinsCapabilityMissing) {
				t.Fatalf("race assembly payload=%v err=%v", p != nil, err)
			}
		},
	}
	p, err := svc.Claim(env.ctx, wkrRow(t, env, w.ID), nil)
	if err != nil || p != nil || !assembled {
		t.Fatalf("race claim payload=%v assembled=%v err=%v", p != nil, assembled, err)
	}
	after := mustRun(t, env, childID)
	cc, err := env.q.GetExactPlanCrossCheck(env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: lead, Round: 2})
	if err != nil || after.Status != "queued" || len(after.CodexCapHash) != 0 || after.CodexClaimEpoch != before.CodexClaimEpoch+1 ||
		cc.CheckerModel.Valid || cc.CheckerEffort.Valid || cc.CheckerModelSource.Valid || cc.CheckerEffortSource.Valid {
		t.Fatalf("race changed credential/snapshot state: status=%s epoch=%d err=%v", after.Status, after.CodexClaimEpoch, err)
	}
}

func TestCrossCheckRoundsPinsStaleVerdictLiveDB(t *testing.T) {
	env, svc, w, lead, c := roundsPinsFixture(t)
	first := roundsPinsSubmit(t, env, svc, w, lead, c, 1)
	roundsPinsClaim(t, env, svc, w, first)
	roundsPinsRevise(t, env, svc, w, first)
	env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort) VALUES($1,'plan','codex','gpt-6-astra','xhigh')", w.UserID)
	second := roundsPinsSubmit(t, env, svc, w, lead, c, 2)
	roundsPinsClaim(t, env, svc, w, second)
	child := mustRun(t, env, uuid.UUID(second.CheckerRunID.Bytes))
	env.exec("UPDATE runs SET claim_generation=2 WHERE id=$1", lead)
	before, err := env.q.GetPlanCrossCheck(env.ctx, lead)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.DecidePlanCrossCheck(env.ctx, w, child.ID, child.ClaimGeneration, "approve", "approve", []byte(`{"summary":"late","items":[]}`)); err == nil {
		t.Fatal("interrupted checker verdict accepted")
	}
	after, err := env.q.GetPlanCrossCheck(env.ctx, lead)
	if err != nil || !reflect.DeepEqual(before, after) || after.Verdict != "failed" || after.ReasonClass.String != "superseded" ||
		after.CheckerModelSource.String != "pin" || !after.WaitCredited {
		t.Fatalf("late verdict rewrote pinned evidence: %+v err=%v", after, err)
	}
}

func TestCrossCheckRoundsPinsUnavailableHumanFallbackLiveDB(t *testing.T) {
	env, svc, w, lead, c := roundsPinsFixture(t)
	first := roundsPinsSubmit(t, env, svc, w, lead, c, 1)
	roundsPinsClaim(t, env, svc, w, first)
	roundsPinsRevise(t, env, svc, w, first)
	second := roundsPinsSubmit(t, env, svc, w, lead, c, 2)
	svc.claimHooks = &claimTestHooks{
		beforeAssembly: func(_ context.Context, _ store.Run) {
			env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort) VALUES($1,'plan','codex','gpt-6-astra','invalid-effort')", w.UserID)
		},
		afterMint: func(_ context.Context, _ store.Run) { t.Fatal("invalid round 2 pin minted credentials") },
	}
	p, err := svc.Claim(env.ctx, wkrRow(t, env, w.ID), nil)
	if err != nil || p != nil {
		t.Fatalf("invalid pin claim: payload=%v err=%v", p != nil, err)
	}
	cc, err := env.q.GetPlanCrossCheck(env.ctx, lead)
	if err != nil || cc.ID != second.ID || cc.ReasonClass.String != "checker_unavailable" || !cc.WaitCredited {
		t.Fatalf("invalid later pin settlement: %+v err=%v", cc, err)
	}
	if _, err = svc.SubmitPlanCrossCheck(env.ctx, w, lead, 1, c, 3); !errors.Is(err, ErrCrossCheckRefused) {
		t.Fatalf("checker-unavailable attempted round 3: %v", err)
	}
	generation := int64(1)
	size := "s"
	caps, tools, ms := []string{}, []string{}, []Milestone{}
	presentation := uuid.New()
	_, applied, err := svc.SetState(env.ctx, w, lead, StateRequest{State: "awaiting_approval", ClaimGeneration: &generation,
		PlanMd: &c.PlanMd, SizeClass: &size, Milestones: &ms, RequiredCapabilities: &caps, RequiredTools: &tools, PresentationID: &presentation})
	r := mustRun(t, env, lead)
	if err != nil || !applied || r.Status != "awaiting_approval" || r.AutoApprove || r.PlanCrossCheckGateReason.String != "checker_unavailable" {
		t.Fatalf("human fallback: applied=%v status=%s reason=%s err=%v", applied, r.Status, r.PlanCrossCheckGateReason.String, err)
	}
}

func TestCrossCheckRoundsPinsFleetPlacementLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                string
		round               int32
		pin, custom, rounds bool
		want                int64
	}{
		{"round1 unpinned legacy", 1, false, false, false, 1},
		{"round2 unpinned legacy", 2, false, false, false, 0},
		{"round2 unpinned capable", 2, false, false, true, 1},
		{"round2 pinned capable", 2, true, false, true, 1},
		{"round2 custom pinned capable", 2, true, true, true, 1},
		{"round2 split rounds pins", 2, true, true, true, 0},
		{"round2 split rounds custom", 2, true, true, true, 0},
		{"round2 maintenance fenced", 2, true, true, true, 0},
		{"round2 maintenance requested", 2, true, true, true, 0},
		{"round2 runtime missing", 2, true, true, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, svc, w, lead, c := roundsPinsFixture(t)
			first := roundsPinsSubmit(t, env, svc, w, lead, c, 1)
			cc := first
			if tc.round == 2 {
				roundsPinsClaim(t, env, svc, w, first)
				roundsPinsRevise(t, env, svc, w, first)
				cc = roundsPinsSubmit(t, env, svc, w, lead, c, 2)
			}
			if tc.pin {
				model := curatedCodexModel
				if tc.custom {
					model = customCodexModel
				}
				env.exec("INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort) VALUES($1,'plan','codex',$2,'high')", w.UserID, model)
			}
			caps := checkerPinCaps(tc.pin, tc.custom)
			if tc.rounds {
				caps = append(caps, capability.CrossCheckRoundsV1)
			}
			switch tc.name {
			case "round2 split rounds pins", "round2 split rounds custom":
				if tc.name == "round2 split rounds pins" {
					caps = append(checkerPinCaps(false, true), capability.CrossCheckRoundsV1)
				} else {
					caps = append(checkerPinCaps(true, false), capability.CrossCheckRoundsV1)
				}
				sibling := uuid.New()
				env.exec("INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at,protocol_capabilities) VALUES($1,$2,$3,$4,'online',now(),$5)",
					sibling, w.UserID, sibling.String(), sibling[:], checkerPinCaps(true, true))
			case "round2 maintenance fenced":
				env.exec("UPDATE workers SET maintenance_fenced=true WHERE id=$1", w.ID)
			case "round2 maintenance requested":
				env.exec("UPDATE workers SET maintenance_phase='requested' WHERE id=$1", w.ID)
				// Test refusal of new foreign work, rather than draining this worker's pinned child.
				env.exec("UPDATE runs SET worker_id=NULL WHERE id=$1 AND status='queued'", uuid.UUID(cc.CheckerRunID.Bytes))
			case "round2 runtime missing":
				caps = []string{capability.CrossCheckV1, capability.CrossCheckRoundsV1, capability.CrossCheckPinsV1, capability.CodexCustomModelV1, capability.CodexHarnessV1}
			}
			env.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, caps)
			child := uuid.UUID(cc.CheckerRunID.Bytes)
			env.exec("UPDATE runs SET status_since=now()-interval '5 minutes' WHERE id=$1", child)
			e := interlockLiveDB{ctx: env.ctx, pool: env.pool, q: env.q, userID: w.UserID, repoID: uuid.UUID(mustRun(t, env, lead).RepoID.Bytes)}
			if n := e.claimableForRun(t, child); n != tc.want {
				t.Fatalf("claimable=%d want=%d", n, tc.want)
			}
			now := time.Now()
			unplaceable, err := env.q.ListUnplaceableQueuedRunsForEphemeral(env.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
				CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
				CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
				CodexCuratedModels:       codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, r := range unplaceable {
				found = found || r.ID == child
			}
			if found != (tc.want == 0) {
				t.Fatalf("ephemeral unplaceable=%v want=%v", found, tc.want == 0)
			}
			// The lead occupies this worker's only slot; eligibility must remain
			// distinct from saturation even for the combined protocol requirements.
			env.exec("UPDATE workers SET max_concurrent_runs=1 WHERE id=$1", w.ID)
			saturated, err := env.q.ListSaturationQueuedRunsForEphemeral(env.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
				CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
				CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
				CodexCuratedModels:       codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true}, SaturationDelay: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000})
			if err != nil {
				t.Fatal(err)
			}
			found = false
			for _, r := range saturated {
				found = found || r.ID == child
			}
			if found != (tc.want == 1) {
				t.Fatalf("ephemeral saturation=%v want=%v", found, tc.want == 1)
			}
		})
	}
}
