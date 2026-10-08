package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func setCheckerPin(f crossCheckContentionFixture, harness string, model, effort any) {
	f.env.exec(`INSERT INTO user_cross_check_pins (user_id,stage,harness,model,effort)
		VALUES ($1,'plan',$2,$3,$4) ON CONFLICT (user_id,stage,harness)
		DO UPDATE SET model=EXCLUDED.model,effort=EXCLUDED.effort`, f.userID, harness, model, effort)
}

func checkerPinCaps(pins, custom bool) []string {
	caps := []string{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}
	if pins {
		caps = append(caps, capability.CrossCheckPinsV1)
	}
	if custom {
		caps = append(caps, capability.CodexCustomModelV1)
	}
	return caps
}

// Reuse the production-shaped SQL parameter and fleet helpers with the authenticated
// checker fixture; no second user, pool or credential fixture is needed.
func checkerPlacementEnv(t *testing.T, f crossCheckContentionFixture) interlockLiveDB {
	t.Helper()
	r := mustRun(t, f.env, f.runID)
	next := int64(100)
	return interlockLiveDB{ctx: f.env.ctx, pool: f.env.pool, q: f.env.q,
		userID: f.userID, repoID: uuid.UUID(r.RepoID.Bytes), nextIID: &next}
}

func assertCheckerNoMint(t *testing.T, f crossCheckContentionFixture, expectedEpoch int64) {
	t.Helper()
	r := mustRun(t, f.env, f.runID)
	if len(r.CodexCapHash) != 0 || r.CodexClaimEpoch != expectedEpoch {
		t.Fatal("pin refusal minted a capability")
	}
	cc, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
	if err != nil {
		t.Fatal(err)
	}
	if cc.CheckerModel.Valid || cc.CheckerEffort.Valid || cc.CheckerModelSource.Valid || cc.CheckerEffortSource.Valid {
		t.Fatal("pin refusal recorded an execution snapshot")
	}
}

func TestCrossCheckPinsResolutionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		model, effort                                    any
		lane                                             any
		laneEffort                                       string
		wantModel, wantEffort, modelSource, effortSource string
	}{
		{"both", "gpt-6-astra", "xhigh", "gpt-6-sol", "high", "gpt-6-astra", "xhigh", "pin", "pin"},
		{"model-only", "gpt-6-astra", nil, "gpt-6-sol", "high", "gpt-6-astra", "high", "pin", "worker default"},
		{"effort-only", nil, "xhigh", "gpt-6-sol", "high", "gpt-6-sol", "xhigh", "worker default", "pin"},
		{"default-null-model", nil, nil, nil, "high", "gpt-6.1-sol", "high", "worker default", "worker default"},
		{"default-later-lane-edit", nil, nil, "gpt-6-astra", "low", "gpt-6-astra", "low", "worker default", "worker default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			f.env.exec(`UPDATE users SET default_codex_model='gpt-6-sol',default_codex_effort='high' WHERE id=$1`, f.userID)
			f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, checkerPinCaps(true, false))
			setCheckerPin(f, "codex", tc.model, tc.effort)
			// The inactive family is deliberately different, including an invalid stored
			// value: assembly must read the actual check's family, not the first pin.
			setCheckerPin(f, "claude", "sonnet", "invalid-inactive-effort")
			f.svc.claimHooks = &claimTestHooks{beforeAssembly: func(_ context.Context, _ store.Run) {
				f.env.exec(`UPDATE users SET default_codex_model=$2,default_codex_effort=$3 WHERE id=$1`, f.userID, tc.lane, tc.laneEffort)
			}}
			p := f.claim(t, false, "", uuid.Nil)
			if p == nil || p.Config.DefaultModel == nil || p.Config.DefaultEffort == nil || p.CrossCheck == nil {
				t.Fatal("checker claim missing model, effort or cross-check input")
			}
			if *p.Config.DefaultModel != tc.wantModel || *p.Config.DefaultEffort != tc.wantEffort ||
				p.CrossCheck.ModelSource == nil || *p.CrossCheck.ModelSource != tc.modelSource ||
				p.CrossCheck.EffortSource == nil || *p.CrossCheck.EffortSource != tc.effortSource {
				t.Fatal("delivered checker resolution/provenance differs from pin and worker defaults")
			}
			cc, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
			if err != nil {
				t.Fatal(err)
			}
			if cc.CheckerModel.String != tc.wantModel || cc.CheckerEffort.String != tc.wantEffort ||
				cc.CheckerModelSource.String != tc.modelSource || cc.CheckerEffortSource.String != tc.effortSource {
				t.Fatal("recorded checker snapshot differs from payload")
			}
		})
	}
}

func TestCrossCheckPinsMintSnapshotLiveDB(t *testing.T) {
	f := newCrossCheckContentionFixture(t)
	f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, checkerPinCaps(true, true))
	setCheckerPin(f, "codex", "gpt-6-astra", "xhigh")
	minted := false
	f.svc.claimHooks = &claimTestHooks{afterMint: func(_ context.Context, _ store.Run) {
		minted = true
		setCheckerPin(f, "codex", "gpt-6-sol", "low")
		f.env.exec(`UPDATE users SET default_codex_model='gpt-5.6-sol',default_codex_effort='medium' WHERE id=$1`, f.userID)
	}}
	p := f.claim(t, false, "", uuid.Nil)
	if !minted || p == nil || p.Config.DefaultModel == nil || p.Config.DefaultEffort == nil ||
		*p.Config.DefaultModel != "gpt-6-astra" || *p.Config.DefaultEffort != "xhigh" {
		t.Fatal("claim recomputed pins after mint instead of carrying the preflight snapshot")
	}
	cc, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
	if err != nil {
		t.Fatal(err)
	}
	if cc.CheckerModel.String != "gpt-6-astra" || cc.CheckerEffort.String != "xhigh" ||
		cc.CheckerModelSource.String != "pin" || cc.CheckerEffortSource.String != "pin" ||
		p.CrossCheck == nil || p.CrossCheck.ModelSource == nil || *p.CrossCheck.ModelSource != "pin" ||
		p.CrossCheck.EffortSource == nil || *p.CrossCheck.EffortSource != "pin" {
		t.Fatal("mint-time edit changed execution snapshot or provenance")
	}
}

func TestCrossCheckPinsLateCapabilityRequeueLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		caps  []string
		model string
		want  error
	}{
		{"pin-added-after-placement", checkerPinCaps(false, false), "gpt-6-astra", errCrossCheckPinsCapabilityMissing},
		{"custom-pin-after-curated-placement", checkerPinCaps(true, false), customCodexModel, errCustomModelCapabilityMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			f.env.exec(`UPDATE users SET default_codex_model='gpt-6-sol' WHERE id=$1`, f.userID)
			f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, tc.caps)
			before := mustRun(t, f.env, f.runID)
			var claimed store.Run
			sawAssembly := false
			f.svc.claimHooks = &claimTestHooks{
				beforeAssembly: func(_ context.Context, r store.Run) {
					claimed = r
					setCheckerPin(f, "codex", tc.model, nil)
				},
				afterMint: func(_ context.Context, _ store.Run) { t.Fatal("capability refusal reached mint") },
				afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
					sawAssembly = true
					if p != nil || !errors.Is(err, tc.want) {
						t.Fatalf("assembly payload present=%v error=%v, want %v", p != nil, err, tc.want)
					}
					assertCheckerNoMint(t, f, before.CodexClaimEpoch)
				},
			}
			if p := f.claim(t, false, "", uuid.Nil); p != nil || !sawAssembly {
				t.Fatal("late capability check did not return idle after assembly")
			}
			r := mustRun(t, f.env, f.runID)
			cc, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
			if err != nil {
				t.Fatal(err)
			}
			if claimed.ClaimGeneration != 1 || r.Status != "queued" || r.WorkerID != claimed.WorkerID ||
				r.ClaimGeneration != claimed.ClaimGeneration || cc.Verdict != "pending" {
				t.Fatal("late capability refusal did not requeue the exact claim")
			}
			assertCheckerNoMint(t, f, before.CodexClaimEpoch+1)
		})
	}
}

func TestCrossCheckPinsInvalidSettlementLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name          string
		model, effort any
	}{
		{"syntax", "bad model!", nil},
		{"family", "sonnet", nil},
		{"effort", nil, "ultra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, checkerPinCaps(true, true))
			f.env.exec(`UPDATE cross_checks SET created_at=now()-interval '120 seconds' WHERE lead_run_id=$1`, f.lead)
			before := mustRun(t, f.env, f.runID)
			var claimed store.Run
			sawAssembly := false
			f.svc.claimHooks = &claimTestHooks{
				beforeAssembly: func(_ context.Context, r store.Run) {
					claimed = r
					setCheckerPin(f, "codex", tc.model, tc.effort)
				},
				afterMint: func(_ context.Context, _ store.Run) { t.Fatal("invalid pin reached mint") },
				afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
					sawAssembly = true
					if p != nil || !errors.Is(err, errCheckerPinUnavailable) {
						t.Fatalf("invalid pin assembly payload present=%v err=%v", p != nil, err)
					}
					assertCheckerNoMint(t, f, before.CodexClaimEpoch)
				},
			}
			if p := f.claim(t, false, "", uuid.Nil); p != nil || !sawAssembly {
				t.Fatal("invalid pin did not settle without credentials")
			}
			cc, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
			if err != nil {
				t.Fatal(err)
			}
			child, lead := mustRun(t, f.env, f.runID), mustRun(t, f.env, f.lead)
			var credit int32
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT CEIL(EXTRACT(EPOCH FROM (decided_at-created_at)))::int
				FROM cross_checks WHERE lead_run_id=$1`, f.lead).Scan(&credit); err != nil {
				t.Fatal(err)
			}
			if cc.Verdict != "failed" || cc.ReasonClass.String != "checker_unavailable" || !cc.DecidedAt.Valid ||
				child.Status != "failed" || child.FailOrigin.String != "guardrail_blocked" ||
				child.ClaimGeneration != claimed.ClaimGeneration || lead.Status != "running" ||
				credit < 120 || lead.BudgetPausedSeconds != credit || lead.LastSeq != 1 {
				t.Fatal("invalid checker pin did not atomically fail child, decide check and bank wait")
			}
			var verdict, reason string
			var gen int64
			if err := f.env.pool.QueryRow(f.env.ctx, `SELECT payload->>'verdict',payload->>'reason_class',claim_generation
				FROM run_messages WHERE run_id=$1 AND seq=1 AND kind='cross_check'`, f.lead).Scan(&verdict, &reason, &gen); err != nil {
				t.Fatal(err)
			}
			if verdict != "failed" || reason != "checker_unavailable" || gen != 1 {
				t.Fatal("missing exact-generation checker-unavailable event")
			}
			assertCheckerNoMint(t, f, before.CodexClaimEpoch+1)
			p, err := f.svc.finishRunClaim(f.env.ctx, claimed, nil, errCheckerPinUnavailable,
				claimRecoveryIdentity{workerID: f.workerID})
			if err != nil || p != nil {
				t.Fatalf("repeat settlement payload present=%v err=%v", p != nil, err)
			}
			if after := mustRun(t, f.env, f.lead); !reflect.DeepEqual(lead, after) {
				t.Fatal("repeat pin failure changed lead or double-banked wait")
			}
			ccAfter, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
			if err != nil || !reflect.DeepEqual(cc, ccAfter) {
				t.Fatal("repeat failure changed decided cross-check")
			}
		})
	}
}

func TestCrossCheckPinsPlacementMirrorsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                                                 string
		model, effort                                        any
		lane                                                 string
		pinsCap, customCap, allowed, customRoot, pinRequired bool
	}{
		{"curated-pin-custom-lane-no-custom-cap", "gpt-6-astra", nil, customCodexModel, true, false, true, false, true},
		{"custom-pin-curated-lane-no-custom-cap", customCodexModel, nil, curatedCodexModel, true, false, false, true, true},
		{"custom-pin-capable", customCodexModel, nil, curatedCodexModel, true, true, true, true, true},
		{"model-pin-old-worker", "gpt-6-astra", nil, curatedCodexModel, false, false, false, false, true},
		{"effort-pin-old-worker", nil, "xhigh", curatedCodexModel, false, false, false, false, true},
		{"effort-pin-aware-worker", nil, "xhigh", curatedCodexModel, true, false, true, false, true},
		{"unpinned-old-worker", nil, nil, curatedCodexModel, false, false, true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			e := checkerPlacementEnv(t, f)
			caps := checkerPinCaps(tc.pinsCap, tc.customCap)
			setCheckerPin(f, "codex", tc.model, tc.effort)
			setCheckerPin(f, "claude", "opus", "max")
			f.env.exec(`UPDATE users SET default_codex_model=$2,ephemeral_workers_enabled=true WHERE id=$1`, f.userID, tc.lane)
			f.env.exec(`UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now(),max_concurrent_runs=1 WHERE id=$1`, f.workerID, caps)
			f.env.exec(`UPDATE runs SET status_since=now()-interval '5 minutes' WHERE id=$1`, f.runID)
			h := e.healthRowFor(t, f.runID)
			if h.CodexCustomRoot != tc.customRoot || h.CrossCheckPinRequired != tc.pinRequired {
				t.Fatalf("SQL health custom=%v pins=%v, want custom=%v pins=%v", h.CodexCustomRoot, h.CrossCheckPinRequired, tc.customRoot, tc.pinRequired)
			}
			wantCount := int64(0)
			if tc.allowed {
				wantCount = 1
			}
			if n := e.claimableForRun(t, f.runID); n != wantCount {
				t.Fatalf("SQL claimable count=%d want=%d", n, wantCount)
			}
			assertTriggers := func(wantUnplaceable, wantSaturated bool) {
				t.Helper()
				now := time.Now()
				u, err := e.q.ListUnplaceableQueuedRunsForEphemeral(e.ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
					CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
					CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
					CodexCuratedModels:       codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
					EphemeralLease: pgtype.Interval{Valid: true}, MaxPerUser: 1000, MaxRows: 10000,
				})
				if err != nil {
					t.Fatal(err)
				}
				unplaceable := false
				for _, r := range u {
					unplaceable = unplaceable || r.ID == f.runID
				}
				s, err := e.q.ListSaturationQueuedRunsForEphemeral(e.ctx, store.ListSaturationQueuedRunsForEphemeralParams{
					CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
					CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-2 * time.Minute), Valid: true},
					CodexCuratedModels:       codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{},
					EphemeralLease: pgtype.Interval{Valid: true}, SaturationDelay: pgtype.Interval{Valid: true},
					MaxPerUser: 1000, MaxRows: 10000,
				})
				if err != nil {
					t.Fatal(err)
				}
				saturated := false
				for _, r := range s {
					saturated = saturated || r.ID == f.runID
				}
				if unplaceable != wantUnplaceable || saturated != wantSaturated {
					t.Fatalf("SQL triggers unplaceable=%v saturation=%v want=%v/%v", unplaceable, saturated, wantUnplaceable, wantSaturated)
				}
			}
			// First a free worker, then the same worker at cap. An incapable
			// worker remains unplaceable even when it is busy, never saturated.
			assertTriggers(!tc.allowed, false)
			active := e.seedActiveRunOwnedBy(t, f.workerID)
			assertTriggers(!tc.allowed, tc.allowed)
			f.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, active)
			run, err := e.q.ClaimRun(e.ctx, e.claimParams(f.workerID, caps, false))
			if tc.allowed {
				if err != nil || run.ID != f.runID {
					t.Fatalf("SQL pin-aware claim failed: id=%v err=%v", run.ID, err)
				}
			} else {
				if !errors.Is(err, pgx.ErrNoRows) {
					t.Fatalf("SQL admitted incapable worker: err=%v", err)
				}
				if r := mustRun(t, f.env, f.runID); r.Status != "queued" {
					t.Fatal("blocked checker left queued state")
				}
			}
		})
	}
}

func TestCrossCheckPinsCapabilityTimeoutLiveDB(t *testing.T) {
	f := newCrossCheckContentionFixture(t)
	e := checkerPlacementEnv(t, f)
	caps := checkerPinCaps(true, false)
	setCheckerPin(f, "codex", customCodexModel, "xhigh")
	f.env.exec(`UPDATE users SET default_codex_model=$2,default_codex_effort='high' WHERE id=$1`,
		f.userID, curatedCodexModel)
	f.env.exec(`UPDATE workers SET protocol_capabilities=$2,last_heartbeat_at=now(),max_concurrent_runs=1 WHERE id=$1`,
		f.workerID, caps)
	before := mustRun(t, f.env, f.runID)
	pending, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
	if err != nil || pending.Verdict != "pending" {
		t.Fatalf("queued checker setup: verdict=%s err=%v", pending.Verdict, err)
	}
	if _, err := e.q.ClaimRun(e.ctx, e.claimParams(f.workerID, caps, false)); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("custom pin admitted worker without custom-model capability: err=%v", err)
	}
	child := mustRun(t, f.env, f.runID)
	if child.Status != "queued" || child.WorkerID.Valid || child.ClaimGeneration != before.ClaimGeneration {
		t.Fatalf("blocked checker changed claim: status=%s worker=%v generation=%d",
			child.Status, child.WorkerID.Valid, child.ClaimGeneration)
	}
	assertCheckerNoMint(t, f, before.CodexClaimEpoch)

	// Expire the same pending check that placement could not dispatch.
	f.env.exec(`UPDATE cross_checks SET created_at=now()-interval '120 seconds',
		deadline_at=now()-interval '1 second' WHERE id=$1 AND verdict='pending'`, pending.ID)
	var credit int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT CEIL(EXTRACT(EPOCH FROM (deadline_at-created_at)))::int
		FROM cross_checks WHERE id=$1`, pending.ID).Scan(&credit); err != nil {
		t.Fatal(err)
	}
	if credit <= 0 {
		t.Fatalf("expired check has no wait credit: %d", credit)
	}
	leadBefore := mustRun(t, f.env, f.lead)
	worker := store.Worker{ID: uuid.UUID(leadBefore.WorkerID.Bytes), UserID: f.userID}
	cc, seq, err := f.svc.PlanCrossCheckStatus(f.env.ctx, worker, f.lead, leadBefore.ClaimGeneration, pending.Round)
	if err != nil || cc.ID != pending.ID || cc.Verdict != "failed" ||
		!cc.ReasonClass.Valid || cc.ReasonClass.String != "timed_out" || seq != leadBefore.LastSeq+1 {
		t.Fatalf("capability timeout status: verdict=%s reason=%s seq=%d err=%v",
			cc.Verdict, cc.ReasonClass.String, seq, err)
	}
	child = mustRun(t, f.env, f.runID)
	lead := mustRun(t, f.env, f.lead)
	if child.Status != "cancelled" || !child.ClaimReleasedAt.Valid ||
		lead.LastSeq != seq || int64(lead.BudgetPausedSeconds) != int64(leadBefore.BudgetPausedSeconds)+int64(credit) {
		t.Fatalf("capability timeout settlement: child=%s released=%v seq=%d budget=%d credit=%d",
			child.Status, child.ClaimReleasedAt.Valid, lead.LastSeq, lead.BudgetPausedSeconds, credit)
	}
	assertCheckerNoMint(t, f, before.CodexClaimEpoch)
	assertPinUnchanged := func() {
		t.Helper()
		var model, effort pgtype.Text
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT model,effort FROM user_cross_check_pins
			WHERE user_id=$1 AND stage='plan' AND harness='codex'`, f.userID).Scan(&model, &effort); err != nil {
			t.Fatal(err)
		}
		if !model.Valid || model.String != customCodexModel || !effort.Valid || effort.String != "xhigh" {
			t.Fatalf("capability timeout changed pin: model=%v effort=%v", model, effort)
		}
	}
	assertPinUnchanged()
	var eventSeq int32
	var generation int64
	var verdict, reason, author string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT seq,claim_generation,payload->>'verdict',
		payload->>'reason_class',payload->>'findings_author' FROM run_messages
		WHERE run_id=$1 AND kind='cross_check'`, f.lead).Scan(&eventSeq, &generation, &verdict, &reason, &author); err != nil {
		t.Fatal(err)
	}
	if eventSeq != seq || generation != leadBefore.ClaimGeneration ||
		verdict != "failed" || reason != "timed_out" || author != "server" {
		t.Fatalf("capability timeout event: seq=%d generation=%d verdict=%s reason=%s author=%s",
			eventSeq, generation, verdict, reason, author)
	}

	repeated, repeatedSeq, err := f.svc.PlanCrossCheckStatus(f.env.ctx, worker, f.lead, leadBefore.ClaimGeneration, pending.Round)
	if err != nil || !reflect.DeepEqual(cc, repeated) || repeatedSeq != seq {
		t.Fatalf("repeat capability timeout status: seq=%d err=%v", repeatedSeq, err)
	}
	after := mustRun(t, f.env, f.lead)
	var events int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM run_messages
		WHERE run_id=$1 AND kind='cross_check'`, f.lead).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 || after.LastSeq != lead.LastSeq || after.BudgetPausedSeconds != lead.BudgetPausedSeconds ||
		!reflect.DeepEqual(child, mustRun(t, f.env, f.runID)) {
		t.Fatal("repeat capability timeout changed child, emitted another event or double-banked wait")
	}
	assertCheckerNoMint(t, f, before.CodexClaimEpoch)
	assertPinUnchanged()
}

func TestCrossCheckPinsSpreadPeerMirrorsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		model, effort                     any
		lane                              string
		peerPins, peerCustom, deferToPeer bool
	}{
		{"curated-pin-custom-lane-peer-no-custom-cap", "gpt-6-astra", nil, customCodexModel, true, false, true},
		{"custom-pin-curated-lane-peer-no-custom-cap", customCodexModel, nil, curatedCodexModel, true, false, false},
		{"custom-pin-capable-peer", customCodexModel, nil, curatedCodexModel, true, true, true},
		{"model-pin-old-peer", "gpt-6-astra", nil, curatedCodexModel, false, false, false},
		{"effort-pin-old-peer", nil, "xhigh", curatedCodexModel, false, false, false},
		{"unpinned-old-peer", nil, nil, curatedCodexModel, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			e := checkerPlacementEnv(t, f)
			setCheckerPin(f, "codex", tc.model, tc.effort)
			f.env.exec(`UPDATE users SET default_codex_model=$2 WHERE id=$1`, f.userID, tc.lane)
			// Only the peer's capability set varies. The claimant clears all
			// gates and has one of two slots busy; the peer has zero of two.
			f.env.exec(`UPDATE workers SET protocol_capabilities=$2,max_concurrent_runs=2,last_heartbeat_at=now() WHERE id=$1`,
				f.workerID, checkerPinCaps(true, true))
			e.seedActiveRunOwnedBy(t, f.workerID)
			e.seedSpreadWorker(t, checkerPinCaps(tc.peerPins, tc.peerCustom), 2)
			run, err := e.q.ClaimRun(e.ctx, e.claimParams(f.workerID, checkerPinCaps(true, true), false))
			if tc.deferToPeer {
				if !errors.Is(err, pgx.ErrNoRows) || mustRun(t, f.env, f.runID).Status != "queued" {
					t.Fatalf("busy claimant did not defer to eligible idle peer: err=%v", err)
				}
			} else if err != nil || run.ID != f.runID {
				t.Fatalf("busy claimant deferred to incapable peer: id=%v err=%v", run.ID, err)
			}
		})
	}
}

func TestCrossCheckPinsOrdinaryClaimUnaffectedLiveDB(t *testing.T) {
	f := newCrossCheckContentionFixture(t)
	setCheckerPin(f, "codex", "gpt-6-astra", "xhigh")
	setCheckerPin(f, "claude", "opus", "max")
	f.env.exec(`UPDATE users SET default_codex_model='gpt-6-sol',default_codex_effort='high' WHERE id=$1`, f.userID)
	f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, checkerPinCaps(false, false))
	f.env.exec(`DELETE FROM cross_checks WHERE lead_run_id=$1`, f.lead)
	f.env.exec(`UPDATE runs SET kind='issue',target_run_id=NULL,report_only=false,issue_iid=43 WHERE id=$1`, f.runID)
	p := f.claim(t, false, "", uuid.Nil)
	if p == nil || p.Kind != "issue" || p.CrossCheck != nil || p.Config.DefaultModel == nil ||
		*p.Config.DefaultModel != "gpt-6-sol" || p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != "high" {
		t.Fatal("checker pins changed ordinary claim or required pins capability")
	}
}

func TestCrossCheckPinsFailureFencesLiveDB(t *testing.T) {
	for _, fence := range []string{"worker", "claim", "epoch", "hash", "released", "lead-generation", "deadline", "pending"} {
		t.Run(fence, func(t *testing.T) {
			f := newCrossCheckContentionFixture(t)
			f.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, f.workerID, checkerPinCaps(true, true))
			var childBefore, leadBefore store.Run
			var checkBefore store.CrossCheck
			sawAssembly := false
			f.svc.claimHooks = &claimTestHooks{
				beforeAssembly: func(_ context.Context, _ store.Run) {
					setCheckerPin(f, "codex", "sonnet", nil)
				},
				afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
					sawAssembly = true
					if p != nil || !errors.Is(err, errCheckerPinUnavailable) {
						t.Fatalf("fence setup: payload present=%v err=%v", p != nil, err)
					}
					switch fence {
					case "worker":
						lead := mustRun(t, f.env, f.lead)
						f.env.exec(`UPDATE runs SET worker_id=$2 WHERE id=$1`, f.runID, lead.WorkerID)
					case "claim":
						f.env.exec(`UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1`, f.runID)
					case "epoch":
						f.env.exec(`UPDATE runs SET codex_claim_epoch=codex_claim_epoch+1 WHERE id=$1`, f.runID)
					case "hash":
						f.env.exec(`UPDATE runs SET codex_cap_hash=$2 WHERE id=$1`, f.runID, []byte("newer-mint-hash"))
					case "released":
						f.env.exec(`UPDATE runs SET claim_released_at=now() WHERE id=$1`, f.runID)
					case "lead-generation":
						f.env.exec(`UPDATE runs SET claim_generation=claim_generation+1 WHERE id=$1`, f.lead)
					case "deadline":
						f.env.exec(`UPDATE cross_checks SET deadline_at=now()-interval '1 second' WHERE lead_run_id=$1`, f.lead)
					case "pending":
						f.env.exec(`UPDATE cross_checks SET verdict='failed',reason_class='timed_out',decided_at=now() WHERE lead_run_id=$1`, f.lead)
					}
					childBefore, leadBefore = mustRun(t, f.env, f.runID), mustRun(t, f.env, f.lead)
					checkBefore, err = f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
					if err != nil {
						t.Fatal(err)
					}
				},
			}
			if p := f.claim(t, false, "", uuid.Nil); p != nil || !sawAssembly {
				t.Fatal("stale failure delivered a payload or bypassed assembly")
			}
			after, err := f.env.q.GetPlanCrossCheck(f.env.ctx, f.lead)
			if err != nil || !reflect.DeepEqual(checkBefore, after) ||
				!reflect.DeepEqual(childBefore, mustRun(t, f.env, f.runID)) ||
				!reflect.DeepEqual(leadBefore, mustRun(t, f.env, f.lead)) {
				t.Fatal("pin failure crossed a stale settlement fence")
			}
		})
	}
}
