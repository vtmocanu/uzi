package workersvc

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type laneRoundsPinsFixture struct {
	env           codexTestEnv
	svc           *Service
	worker        store.Worker
	lead          uuid.UUID
	first, second store.CrossCheck
}

func newLaneRoundsPinsFixture(t *testing.T, pinned bool) laneRoundsPinsFixture {
	t.Helper()
	env, svc, w, lead, candidate := roundsPinsFixture(t)
	ctx, cancel := context.WithTimeout(env.ctx, 30*time.Second)
	t.Cleanup(cancel)
	env.ctx = ctx
	env.exec(`UPDATE workers SET max_concurrent_runs=1,max_cross_check_slots=1,
		protocol_capabilities=array_append(protocol_capabilities,'cross_check_lane_v1')
		WHERE id=$1`, w.ID)
	f := laneRoundsPinsFixture{env: env, svc: svc, worker: w, lead: lead}
	f.first = roundsPinsSubmit(t, env, svc, w, lead, candidate, 1)
	f.placement(t, uuid.UUID(f.first.CheckerRunID.Bytes), 1, false)
	p := f.claim(t, "cross_check")
	f.claimed(t, f.first, p, "gpt-6-sol", "high", "worker default")
	roundsPinsRevise(t, env, svc, w, f.first)
	if pinned {
		env.exec(`INSERT INTO user_cross_check_pins(user_id,stage,harness,model,effort)
			VALUES($1,'plan','codex','gpt-6-astra','xhigh')`, w.UserID)
		// Distinct defaults make the delivered pin distinguishable from fallback.
		env.exec("UPDATE users SET default_codex_model='gpt-5.6-sol',default_codex_effort='low' WHERE id=$1", w.UserID)
	}
	f.second = roundsPinsSubmit(t, env, svc, w, lead, candidate, 2)
	env.exec("UPDATE runs SET status_since=now()-interval '5 minutes' WHERE id=$1", uuid.UUID(f.second.CheckerRunID.Bytes))
	return f
}

// Each request has its own deadline, including refusals that must return idle.
func (f laneRoundsPinsFixture) claim(t *testing.T, lane string) *ClaimPayload {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.env.ctx, 5*time.Second)
	defer cancel()
	w := wkrRow(t, f.env, f.worker.ID)
	var p *ClaimPayload
	var err error
	switch lane {
	case "run":
		p, err = f.svc.Claim(ctx, w, nil)
	case "cross_check":
		p, err = f.svc.ClaimCrossCheck(ctx, w, nil)
	default:
		t.Fatalf("unknown request lane %q", lane)
	}
	if err != nil {
		t.Fatalf("%s claim: %v", lane, err)
	}
	return p
}

func (f laneRoundsPinsFixture) claimed(t *testing.T, cc store.CrossCheck, p *ClaimPayload, model, effort, source string) {
	t.Helper()
	childID := uuid.UUID(cc.CheckerRunID.Bytes)
	if p == nil || p.RunID != childID.String() || p.Kind != "cross_check" ||
		p.CrossCheck == nil || p.CrossCheck.Round != cc.Round {
		t.Fatalf("round %d dedicated claim: %+v", cc.Round, p)
	}
	if p.Config.DefaultModel == nil || *p.Config.DefaultModel != model ||
		p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != effort ||
		p.CrossCheck.ModelSource == nil || *p.CrossCheck.ModelSource != source ||
		p.CrossCheck.EffortSource == nil || *p.CrossCheck.EffortSource != source {
		t.Fatalf("round %d did not deliver %s/%s from %s", cc.Round, model, effort, source)
	}
	child := mustRun(t, f.env, childID)
	if !child.CrossCheckLane || child.WorkerID.Bytes != f.worker.ID ||
		child.ClaimGeneration != p.ClaimGeneration || child.ClaimReleasedAt.Valid {
		t.Fatalf("dedicated claim custody: %+v", child)
	}
	rows, err := f.env.q.ListWorkersByUser(f.env.ctx, f.worker.UserID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.ID == f.worker.ID {
			found = true
			if !row.Busy || row.ActiveRuns != 1 {
				t.Fatalf("checker inflated the lead's run load: busy=%v active=%d", row.Busy, row.ActiveRuns)
			}
		}
	}
	if !found {
		t.Fatal("claiming worker missing from fleet load")
	}
}

func (f laneRoundsPinsFixture) refused(t *testing.T, child uuid.UUID, lane string) {
	t.Helper()
	before := mustRun(t, f.env, child)
	if p := f.claim(t, lane); p != nil {
		t.Fatalf("%s claimed ineligible checker: %+v", lane, p)
	}
	if after := mustRun(t, f.env, child); !reflect.DeepEqual(before, after) {
		t.Fatalf("%s refusal changed queued checker custody or credentials", lane)
	}
}

// Placement mirrors use one production evaluation clock, the two-minute child
// affinity grace and the shipped curated model vocabulary.
func (f laneRoundsPinsFixture) placement(t *testing.T, child uuid.UUID, want int64, gap bool) {
	t.Helper()
	now := time.Now()
	evaluatedAt := pgtype.Timestamptz{Time: now, Valid: true}
	affinityCutoff := pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true}
	n, err := f.env.q.CountOnlineWorkersClaimableForRun(f.env.ctx, store.CountOnlineWorkersClaimableForRunParams{
		RunID: child, HeartbeatCutoff: pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		AffinityCutoff: affinityCutoff, CrossCheckEvaluatedAt: evaluatedAt,
		CrossCheckAffinityCutoff: affinityCutoff, CapabilityAware: true,
		CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
		EphemeralLease: pgtype.Interval{Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n.Claimable != want {
		t.Fatalf("checker %s claimable=%d want=%d", child, n.Claimable, want)
	}
	gaps, err := f.env.q.ListUnplaceableQueuedRunsForEphemeral(f.env.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
		CrossCheckEvaluatedAt: evaluatedAt, CrossCheckAffinityCutoff: affinityCutoff,
		BackgroundGraceCutoff: affinityCutoff, CodexCuratedModels: codexCuratedModelsSlice(),
		DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
		MaxPerUser: 1000, MaxRows: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range gaps {
		found = found || row.ID == child
	}
	if found != gap {
		t.Fatalf("checker %s capability gap=%v want=%v", child, found, gap)
	}
	saturated, err := f.env.q.ListSaturationQueuedRunsForEphemeral(f.env.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
		CrossCheckEvaluatedAt: evaluatedAt, CrossCheckAffinityCutoff: affinityCutoff,
		BackgroundGraceCutoff: affinityCutoff, CodexCuratedModels: codexCuratedModelsSlice(),
		DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
		SaturationDelay: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range saturated {
		if row.ID == child {
			t.Fatalf("checker %s incorrectly treated as run-slot saturation", child)
		}
	}
}

func TestCrossCheckLaneRoundsPinsCombinedLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pinned  bool
		custom  bool
		missing string
	}{
		{name: "later-round-defaults"},
		{name: "later-round-pins", pinned: true},
		{name: "missing-rounds", pinned: true, missing: capability.CrossCheckRoundsV1},
		{name: "missing-pins", pinned: true, missing: capability.CrossCheckPinsV1},
		{name: "later-round-custom-pin", pinned: true, custom: true},
		{name: "missing-custom-model", pinned: true, custom: true, missing: capability.CodexCustomModelV1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneRoundsPinsFixture(t, tc.pinned)
			child := uuid.UUID(f.second.CheckerRunID.Bytes)
			if tc.custom {
				f.env.exec("UPDATE user_cross_check_pins SET model=$2 WHERE user_id=$1 AND stage='plan' AND harness='codex'", f.worker.UserID, customCodexModel)
			}
			if tc.missing != "" {
				// Change the advertisement after Submit, so only claim-time
				// admission and its mirrors decide this queued child's fate.
				f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,$2) WHERE id=$1", f.worker.ID, tc.missing)
				f.placement(t, child, 0, true)
				f.refused(t, child, "run")
				f.refused(t, child, "cross_check")
				return
			}
			lead := mustRun(t, f.env, f.lead)
			w := wkrRow(t, f.env, f.worker.ID)
			if lead.Status != "running" || lead.WorkerID.Bytes != w.ID ||
				lead.ClaimReleasedAt.Valid || !w.MaxConcurrentRuns.Valid || w.MaxConcurrentRuns.Int32 != 1 {
				t.Fatal("lead must occupy the worker's only run slot")
			}
			f.placement(t, child, 1, false)
			f.refused(t, child, "run")
			p := f.claim(t, "cross_check")
			if tc.custom {
				f.claimed(t, f.second, p, customCodexModel, "xhigh", "pin")
			} else if tc.pinned {
				f.claimed(t, f.second, p, "gpt-6-astra", "xhigh", "pin")
			} else {
				f.claimed(t, f.second, p, "gpt-6-sol", "high", "worker default")
			}
		})
	}
}

func TestCrossCheckLaneRoundsPinsExplicitZeroLiveDB(t *testing.T) {
	for _, laneCap := range []bool{true, false} {
		for _, lane := range []string{"run", "cross_check"} {
			name := lane + "-no-lane-cap"
			if laneCap {
				name = lane + "-lane-cap"
			}
			t.Run(name, func(t *testing.T) {
				f := newLaneRoundsPinsFixture(t, true)
				// The live lead keeps its custody, but ordinary capacity is free:
				// explicit zero must deny independently of run-slot occupancy.
				f.env.exec("UPDATE workers SET max_cross_check_slots=0,max_concurrent_runs=2 WHERE id=$1", f.worker.ID)
				if !laneCap {
					f.env.exec("UPDATE workers SET protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1", f.worker.ID)
				}
				child := uuid.UUID(f.second.CheckerRunID.Bytes)
				f.placement(t, child, 0, true)
				f.refused(t, child, lane)
				// A legacy fallback is the positive control on the same free
				// run slot; only NULL plus no lane capability permits it.
				if !laneCap && lane == "run" {
					f.env.exec("UPDATE workers SET max_cross_check_slots=NULL WHERE id=$1", f.worker.ID)
					f.placement(t, child, 1, false)
					p := f.claim(t, "run")
					if p == nil || p.RunID != child.String() || p.CrossCheck == nil || p.CrossCheck.Round != 2 ||
						p.Config.DefaultModel == nil || *p.Config.DefaultModel != "gpt-6-astra" ||
						p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != "xhigh" {
						t.Fatalf("free run-slot legacy control: %+v", p)
					}
					if mustRun(t, f.env, child).CrossCheckLane {
						t.Fatal("legacy control consumed a dedicated lane slot")
					}
				}
			})
		}
	}
}

func TestCrossCheckLaneRoundsPinsStaleEarlierRoundLiveDB(t *testing.T) {
	f := newLaneRoundsPinsFixture(t, true)
	later := uuid.UUID(f.second.CheckerRunID.Bytes)
	f.placement(t, later, 1, false)
	f.claimed(t, f.second, f.claim(t, "cross_check"), "gpt-6-astra", "xhigh", "pin")
	roundsPinsRevise(t, f.env, f.svc, f.worker, f.second)

	// cross_checks_one_pending permits only one pending row. Decide round 2
	// first, then restore round 1 without changing immutable budget snapshots
	// or the lead's custody. Its future deadline eliminates expiry as a cause.
	f.env.exec(`UPDATE cross_checks SET verdict='pending',reason_class=NULL,
		decided_at=NULL,wait_credited=false,interrupted_at=NULL,deadline_at=now()+interval '10 minutes'
		WHERE id=$1`, f.first.ID)
	earlier := uuid.UUID(f.first.CheckerRunID.Bytes)
	f.env.exec(`UPDATE runs SET status='queued',claim_released_at=NULL,finished_at=NULL,
		status_since=now()-interval '5 minutes' WHERE id=$1`, earlier)
	cc, err := f.env.q.GetExactPlanCrossCheck(f.env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
	if err != nil || cc.Verdict != "pending" || cc.WaitCredited || cc.LeadClaimGeneration != mustRun(t, f.env, f.lead).ClaimGeneration {
		t.Fatalf("stale attempt setup: %+v err=%v", cc, err)
	}
	f.placement(t, earlier, 0, true)
	f.refused(t, earlier, "cross_check")
	// Give the run request a genuine legacy route and a free ordinary slot,
	// so its refusal also isolates stale-round admission.
	f.env.exec(`UPDATE workers SET max_cross_check_slots=NULL,max_concurrent_runs=2,
		protocol_capabilities=array_remove(protocol_capabilities,'cross_check_lane_v1') WHERE id=$1`, f.worker.ID)
	f.placement(t, earlier, 0, true)
	f.refused(t, earlier, "run")
	after, err := f.env.q.GetExactPlanCrossCheck(f.env.ctx, store.GetExactPlanCrossCheckParams{LeadRunID: f.lead, Round: 1})
	if err != nil || !reflect.DeepEqual(cc, after) {
		t.Fatalf("stale claim rewrote earlier-round evidence: %v", err)
	}
}
