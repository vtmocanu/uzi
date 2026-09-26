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

// TestCredentialPromoterAutoLaneNeedsEnabledPooledTokenLiveDB (M): a held run on an ordinary
// auto lane (a worker bound auto, or a per-run auto override) stays held while no pooled token
// is enabled, even with an enabled non-pooled default, and resumes once one is. A Judge-lane
// auto run keeps the #1140 fallback: the enabled default serves it.
func TestCredentialPromoterAutoLaneNeedsEnabledPooledTokenLiveDB(t *testing.T) {
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
	assertHeld(t, fx.env, workerAuto, true)
	assertHeld(t, fx.env, overrideAuto, true)
	assertHeld(t, fx.env, judgeAuto, false)

	fx.setEnabled(fx.otherTok, true)
	fx.svc.RequestCredentialDisabledPromotion(fx.userID)
	assertHeld(t, fx.env, workerAuto, false)
	assertHeld(t, fx.env, overrideAuto, false)
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
