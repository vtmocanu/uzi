package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// credential_disabled_converge_livedb_test.go pins the convergence of the PRD #1732 D14
// park/promote loop against real Postgres: the parking worker reclaims its own promoted run, a
// resumed completion hold parks without leaking custody, an auto lane never cycles on a
// disabled pooled token, the promoter follows ClaimRun's affinity, and one bad candidate or a
// panicking pass never stops the owner's other runs. Skipped unless UZI_TEST_DATABASE_URL is
// set; run via ./e2e/run-store-it.sh.

// disableOnFirstAssembly disables tok after the first claim's assembly only, the window
// between opening the credential and the finisher's locked decision.
func (fx *cdFix) disableOnFirstAssembly(t *testing.T, tok uuid.UUID) {
	t.Helper()
	fired := false
	fx.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if fired {
			return
		}
		fired = true
		if err != nil || p == nil {
			t.Fatalf("assembly = (%v, %v), want a payload", p != nil, err)
		}
		fx.setEnabled(tok, false)
	}}
}

// TestCredentialDisabledSameWorkerReclaimsAfterPromoteLiveDB (B1): park -> enable -> the SAME
// live worker incarnation claims the run again. The undelivered claim must not record a D19
// released incarnation (which bars exactly that worker forever) and promotion keeps worker_id
// as resume affinity.
func TestCredentialDisabledSameWorkerReclaimsAfterPromoteLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	fx.disableOnFirstAssembly(t, fx.pinTok)
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
		t.Fatalf("first Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	assertHeld(t, fx.env, runID, true)

	fx.setEnabled(fx.pinTok, true)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	r := assertHeld(t, fx.env, runID, false)
	if r.WorkerID != pgconv.UUID(fx.workerID) {
		t.Fatalf("promoted worker_id = %v, want the parking worker %s kept as affinity", r.WorkerID, fx.workerID)
	}

	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload == nil || payload.RunID != runID.String() {
		t.Fatalf("same-worker reclaim = (%v, %v), want a payload for %s", payload != nil, err, runID)
	}
	if r := mustRun(t, fx.env, runID); r.Status != "claimed" || r.WorkerID != pgconv.UUID(fx.workerID) {
		t.Fatalf("reclaim: status=%s worker=%v", r.Status, r.WorkerID)
	}
}

// TestCredentialDisabledParksCompletionBlockedClaimLiveDB (B2): a run the owner resumed out
// of a completion hold is claimed still carrying hold_reason='completion_blocked' (kept until
// its first running report). A disabled credential on that claim parks it, settles THIS
// claim's custody hold in the same transaction, and leaves no open hold behind; the promoted
// run is reclaimable and the captured head survives for SetRunRunning's clear.
func TestCredentialDisabledParksCompletionBlockedClaimLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.queuedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET hold_reason = 'completion_blocked', hold_captured_head = 'cafe01' WHERE id = $1`, runID)
	fx.disableOnFirstAssembly(t, fx.pinTok)

	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	r := assertHeld(t, fx.env, runID, true)
	if r.HoldCapturedHead.String != "cafe01" {
		t.Fatalf("captured head = %v, want kept", r.HoldCapturedHead)
	}
	if n := openCustodyHolds(t, fx.env, runID); n != 0 {
		t.Fatalf("open custody holds after the park = %d, want 0 (the claim's hold is settled)", n)
	}
	var total int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM recovery_custody_holds WHERE run_id = $1`, runID).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("custody holds = %d, want exactly the one this claim opened (released)", total)
	}

	fx.setEnabled(fx.pinTok, true)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	r = assertHeld(t, fx.env, runID, false)
	if r.HoldReason.Valid || r.HoldCapturedHead.String != "cafe01" {
		t.Fatalf("promoted: hold=%v head=%v, want no hold and the head kept", r.HoldReason, r.HoldCapturedHead)
	}
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload == nil || payload.RunID != runID.String() {
		t.Fatalf("reclaim = (%v, %v), want a payload", payload != nil, err)
	}
}

// TestClaimCredentialDisabledPublishesParkLiveDB (N5): the claimed -> paused park is
// published to clients like every other visible status transition.
func TestClaimCredentialDisabledPublishesParkLiveDB(t *testing.T) {
	fx := newCDFix(t)
	rec := newReadmitRecorder()
	fx.svc.SetBroadcaster(rec)
	runID := fx.queuedRun(t, "issue")
	fx.disableOnFirstAssembly(t, fx.pinTok)
	if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
		t.Fatalf("Claim = (%v, %v), want an idle park", payload != nil, err)
	}
	assertHeld(t, fx.env, runID, true)
	if _, states := rec.published(runID); len(states) != 1 || states[0] != "paused" {
		t.Fatalf("published states = %v, want [paused]", states)
	}
}

// TestCredentialDisabledAutoLaneDoesNotCycleLiveDB (M): an auto-bound worker whose pool holds a
// disabled pooled token (the best-ranked one) and an enabled pooled token claims on the
// enabled one. Before the fix the ranker picked the disabled token, the finisher parked the
// run, the promoter's auto branch promoted it unconditionally and the next claim picked the
// same token again, forever.
func TestCredentialDisabledAutoLaneDoesNotCycleLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'auto', anthropic_secret_id = NULL WHERE id = $1`, fx.workerID)
	// pinTok is the ranker's favourite: a fresh, nearly empty reading with the soonest reset.
	fx.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
	    seven_day_pct, seven_day_resets_at, source, synced_at)
	    VALUES ($1, $2, 5, now() + interval '1 hour', 5, now() + interval '1 day', 'usage_endpoint', now())`, fx.pinTok, fx.userID)
	fx.setEnabled(fx.pinTok, false)
	runID := fx.queuedRun(t, "issue")

	payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || payload == nil || payload.RunID != runID.String() {
		r := mustRun(t, fx.env, runID)
		t.Fatalf("Claim = (%v, %v), status=%s hold=%v: want a payload on the enabled pooled token",
			payload != nil, err, r.Status, r.HoldReason)
	}
	if r := mustRun(t, fx.env, runID); uuid.UUID(r.AnthropicSecretID.Bytes) != fx.otherTok {
		t.Fatalf("auto lane spent %v, want the enabled pooled token %s", r.AnthropicSecretID, fx.otherTok)
	}
}

// TestCredentialPromoterAutoLaneFallsToPoolWaitLiveDB (PRD #1732 D2): a held run on an ordinary
// auto lane (a worker bound auto, or a per-run auto override) has no single disabled pin to wait
// for, so the promoter always returns it to queued; with no enabled pooled token its next claim
// holds it in pool_wait (errAutoPoolEmpty), never on the non-pooled default. A Judge-lane auto
// run keeps the #1140 fallback and spends the enabled default.
//
// MUTATION: make the promoter's ordinary auto branches require an enabled pooled token again;
// the two auto runs then stay held on credential_disabled and this test fails.
func TestCredentialPromoterAutoLaneFallsToPoolWaitLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'auto', anthropic_secret_id = NULL WHERE id = $1`, fx.workerID)
	fx.env.exec(`UPDATE users SET judge_anthropic_bind_mode = 'auto', judge_anthropic_secret_id = NULL WHERE id = $1`, fx.userID)
	fx.setEnabled(fx.pinTok, false)
	fx.setEnabled(fx.otherTok, false)

	workerAuto := fx.parkedRun(t, "issue")
	overrideAuto := fx.parkedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET credential_override_mode = 'auto' WHERE id = $1`, overrideAuto)
	judgeAuto := fx.parkedRun(t, "self_improve")

	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	for _, id := range []uuid.UUID{workerAuto, overrideAuto, judgeAuto} {
		assertHeld(t, fx.env, id, false)
	}
	for i := 0; i < 3; i++ {
		if _, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil {
			t.Fatalf("Claim %d: %v", i, err)
		}
	}
	for _, id := range []uuid.UUID{workerAuto, overrideAuto} {
		if r := mustRun(t, fx.env, id); r.Status != "pool_wait" || r.AnthropicSecretID.Valid {
			t.Fatalf("auto run %s: status=%s hold=%v spent=%v, want pool_wait with nothing spent", id, r.Status, r.HoldReason, r.AnthropicSecretID)
		}
	}
	if r := mustRun(t, fx.env, judgeAuto); uuid.UUID(r.AnthropicSecretID.Bytes) != fx.defTok || r.AnthropicSelectReason.String != "pool_empty" {
		t.Fatalf("judge-lane auto run: status=%s spent=%v (%q), want the enabled default (pool_empty)",
			r.Status, r.AnthropicSecretID, r.AnthropicSelectReason.String)
	}
}

// TestCredentialPromoterFollowsAffinityLiveDB (B1): the worker binding is the requirement only
// while that worker will be the next claimant (ClaimRun's affinity pin: draining, or a fresh
// heartbeat). A dead, non-draining worker's disabled pin does not hold the run: it is promoted
// so another worker's claim decides. A draining worker keeps the pin, and so the hold.
func TestCredentialPromoterFollowsAffinityLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.setEnabled(fx.pinTok, false)

	fx.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '1 hour', draining_since = now() WHERE id = $1`, fx.workerID)
	draining := fx.parkedRun(t, "issue")
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, fx.env, draining, true)

	fx.env.exec(`UPDATE workers SET draining_since = NULL WHERE id = $1`, fx.workerID)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, fx.env, draining, false)
}

// TestCredentialPromoterCandidateErrorContinuesLiveDB (N3): one candidate whose transaction
// fails is logged and skipped; the pass still promotes the owner's other runs and reports the
// failure alongside the promoted count.
func TestCredentialPromoterCandidateErrorContinuesLiveDB(t *testing.T) {
	fx := newCDFix(t)
	older := fx.parkedRun(t, "issue")
	fx.env.exec(`UPDATE runs SET status_since = now() - interval '20 minutes' WHERE id = $1`, older)
	newer := fx.parkedRun(t, "issue")
	boom := errors.New("injected candidate failure")
	fx.svc.credPromoteHooks = &credentialPromoteTestHooks{beforePromote: func(_ context.Context, runID uuid.UUID) error {
		if runID == older {
			return boom
		}
		return nil
	}}
	n, err := fx.svc.promoteCredentialDisabledRuns(fx.env.ctx, fx.userID)
	if n != 1 || !errors.Is(err, boom) || !strings.Contains(err.Error(), older.String()) {
		t.Fatalf("pass = (%d, %v), want 1 promoted and the older run's error", n, err)
	}
	assertHeld(t, fx.env, older, true)
	assertHeld(t, fx.env, newer, false)
}

// TestCredentialPromoterPanicDoesNotWedgeOwnerLiveDB (N2): a pass that panics is recovered
// inside the promoter goroutine and releases the owner, so the next request runs a fresh pass
// instead of being swallowed as a dirty flag no goroutine drains.
func TestCredentialPromoterPanicDoesNotWedgeOwnerLiveDB(t *testing.T) {
	fx := newCDFix(t)
	runID := fx.parkedRun(t, "issue")
	var escaped any
	fx.svc.SetBackground(func(fn func()) {
		defer func() { escaped = recover() }()
		fn()
	})
	panicked := false
	fx.svc.credPromoteHooks = &credentialPromoteTestHooks{beforePromote: func(context.Context, uuid.UUID) error {
		if !panicked {
			panicked = true
			panic("injected promoter panic")
		}
		return nil
	}}
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	if !panicked || escaped != nil {
		t.Fatalf("panicked=%t escaped=%v, want the panic recovered inside the pass", panicked, escaped)
	}
	assertHeld(t, fx.env, runID, true)

	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, fx.env, runID, false)
}

// TestCredentialDisabledPendingPauseConvergesLiveDB (PRD #1732 D14, item 0c): a claimed row CAN
// carry a pending pause in production. CreatePauseInput sets it only on a running run, and the
// worker-death requeues (RequeueWorkerRuns, RequeueRunsOfStaleWorkers,
// RequeueRunsMissingFromSnapshot) deliberately keep it so the next claim re-arms it. When that
// next claim parks on credential_disabled, re-enabling the credential must neither promote past
// the pause nor strand the run on a hold ResumePausedRun refuses: an owner request settles into
// the ordinary owner pause (resumable by the owner, with the held interval banked), and a
// system 'wall' request into the budget_exhausted hold (resumed by extend).
//
// MUTATION: drop the promoter's SettleCredentialDisabledPause branch; the run then stays held on
// credential_disabled forever (PromoteCredentialDisabledRun refuses a pending pause).
func TestCredentialDisabledPendingPauseConvergesLiveDB(t *testing.T) {
	for _, tc := range []struct {
		mode, wantHold string
	}{
		{"now", ""},
		{"milestone", ""},
		{"wall", "budget_exhausted"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			fx := newCDFix(t)
			runID := fx.queuedRun(t, "issue")
			if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload == nil {
				t.Fatalf("Claim = (%v, %v), want the payload", payload != nil, err)
			}
			fx.env.exec(`UPDATE runs SET status = 'running', status_since = now() WHERE id = $1`, runID)
			if tc.mode == "wall" {
				// RequestWallParks' shape: the system request plus its steering input.
				fx.env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = 'wall' WHERE id = $1`, runID)
				fx.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'pause', 'wall')`, runID)
			} else if _, err := fx.env.q.CreatePauseInput(fx.env.ctx, store.CreatePauseInputParams{
				ID: runID, Mode: pgconv.TextOrNull(tc.mode),
			}); err != nil {
				t.Fatalf("CreatePauseInput: %v", err)
			}
			// The worker dies: the re-register requeue keeps the pending pause by design.
			if _, err := fx.env.q.RequeueWorkerRuns(fx.env.ctx, store.RequeueWorkerRunsParams{
				WorkerID: pgconv.UUID(fx.workerID), MaxRequeues: 5,
			}); err != nil {
				t.Fatalf("RequeueWorkerRuns: %v", err)
			}
			fx.setEnabled(fx.pinTok, false)
			if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
				t.Fatalf("re-claim = (%v, %v), want an idle park", payload != nil, err)
			}
			if r := assertHeld(t, fx.env, runID, true); !r.PauseRequestedAt.Valid {
				t.Fatal("the park consumed the pending pause")
			}
			fx.env.exec(`UPDATE runs SET status_since = now() - interval '10 minutes' WHERE id = $1`, runID)

			fx.setEnabled(fx.pinTok, true)
			fx.svc.RequestCredentialDisabledPromotion(fx.userID)
			r := mustRun(t, fx.env, runID)
			if r.Status != "paused" || r.HoldReason.String != tc.wantHold || r.PauseRequestedAt.Valid || r.PauseMode.Valid {
				t.Fatalf("settled: status=%s hold=%q pending=%v mode=%v, want paused hold=%q with the request consumed",
					r.Status, r.HoldReason.String, r.PauseRequestedAt.Valid, r.PauseMode, tc.wantHold)
			}
			var unapplied int
			if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM run_user_inputs
			    WHERE run_id = $1 AND kind = 'pause' AND applied_at IS NULL`, runID).Scan(&unapplied); err != nil {
				t.Fatal(err)
			}
			if unapplied != 0 {
				t.Fatalf("unapplied pause inputs = %d, want 0", unapplied)
			}
			if tc.mode == "wall" {
				return // budget_exhausted is resumed by extend, not ResumePausedRun
			}
			// The owner resumes it like any owner pause, banking the whole held interval.
			if _, err := fx.env.q.ResumePausedRun(fx.env.ctx, store.ResumePausedRunParams{
				ID: runID, UserID: fx.userID, GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
			}); err != nil {
				t.Fatalf("ResumePausedRun: %v", err)
			}
			if r := mustRun(t, fx.env, runID); r.Status != "queued" || r.BudgetPausedSeconds < 600 {
				t.Fatalf("resumed: status=%s banked=%ds, want queued with the held interval banked", r.Status, r.BudgetPausedSeconds)
			}
		})
	}
}
