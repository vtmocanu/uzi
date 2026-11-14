package workersvc

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3250 M2 live-DB suite: claim-time precedence of a start-time run_override model and effort,
// and the run_override_unavailable hold. Skipped unless UZI_TEST_DATABASE_URL is set; run via
// ./e2e/run-store-it.sh.

func setRunStartOptions(env codexTestEnv, runID uuid.UUID, model, modelSource, effort, effortSource any) {
	env.exec(`UPDATE runs SET model = $2, model_source = $3, effort = $4, effort_source = $5 WHERE id = $1`,
		runID, model, modelSource, effort, effortSource)
}

// overrideMessages returns the payloads of the run's run_override_unavailable feed lines.
func overrideMessages(t *testing.T, env codexTestEnv, runID uuid.UUID) []map[string]string {
	t.Helper()
	rows, err := env.pool.Query(env.ctx, `SELECT payload FROM run_messages WHERE run_id = $1 AND kind = 'run_override_unavailable' ORDER BY seq`, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

func lastSeqCoversMessages(t *testing.T, env codexTestEnv, runID uuid.UUID) {
	t.Helper()
	var last int32
	var max int32
	if err := env.pool.QueryRow(env.ctx, `SELECT r.last_seq, COALESCE((SELECT max(seq) FROM run_messages WHERE run_id = r.id), 0) FROM runs r WHERE r.id = $1`, runID).Scan(&last, &max); err != nil {
		t.Fatal(err)
	}
	if last < max {
		t.Fatalf("runs.last_seq %d is behind the newest message seq %d", last, max)
	}
}

// requireOverrideHeld asserts the exact park shape and the single feed line it wrote.
func requireOverrideHeld(t *testing.T, env codexTestEnv, runID, workerID uuid.UUID, wantMessages int, setting, value string) store.Run {
	t.Helper()
	r := mustRun(t, env, runID)
	if r.Status != "paused" || r.HoldReason.String != HoldReasonRunOverrideUnavailable || !r.ClaimReleasedAt.Valid ||
		r.WorkerID != pgconv.UUID(workerID) || len(r.CodexCapHash) != 0 || r.FailOrigin.Valid || r.Health != "ok" {
		t.Fatalf("park shape: status=%s hold=%v released=%v worker=%v cap=%x origin=%v health=%s",
			r.Status, r.HoldReason, r.ClaimReleasedAt, r.WorkerID, r.CodexCapHash, r.FailOrigin, r.Health)
	}
	msgs := overrideMessages(t, env, runID)
	if len(msgs) != wantMessages {
		t.Fatalf("run_override_unavailable messages = %d, want %d", len(msgs), wantMessages)
	}
	last := msgs[len(msgs)-1]
	if last["setting"] != setting || last["value"] != value || last["harness"] != r.Harness || strings.TrimSpace(last["note"]) == "" {
		t.Fatalf("message payload = %v, want setting=%s value=%q harness=%s and a note", last, setting, value, r.Harness)
	}
	var n int
	if err := env.pool.QueryRow(env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = $2`, runID, harnessModelFallbackKind).Scan(&n); err != nil || n != 0 {
		t.Fatalf("harness_model_fallback notes = %d (%v), want none: an unhonorable override is never a fallback", n, err)
	}
	lastSeqCoversMessages(t, env, runID)
	return r
}

func requeueClaimed(env codexTestEnv, runID uuid.UUID) {
	env.exec(`UPDATE runs SET status = 'queued', status_since = now(), codex_cap_hash = NULL WHERE id = $1`, runID)
}

func (fx *cdFix) claimPayload(t *testing.T) *ClaimPayload {
	t.Helper()
	p, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
	if err != nil || p == nil {
		t.Fatalf("Claim = (%v, %v), want a payload", p != nil, err)
	}
	return p
}

func requireEffort(t *testing.T, p *ClaimPayload, wantLevel, wantSource string) {
	t.Helper()
	if p.Config.DefaultEffort == nil || *p.Config.DefaultEffort != wantLevel ||
		p.Config.DefaultEffortSource == nil || *p.Config.DefaultEffortSource != wantSource {
		t.Fatalf("default_effort = %v / %v, want %s / %s", deref(p.Config.DefaultEffort), deref(p.Config.DefaultEffortSource), wantLevel, wantSource)
	}
}

// TestClaimRunOverrideEffortAndModelClaudeLiveDB: a frozen xhigh wins over the owner's low, survives
// a change of the owner's setting across a requeue and reclaim, and a sibling run without an override
// follows the owner's new setting. A frozen model ships with its run_override source.
func TestClaimRunOverrideEffortAndModelClaudeLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.env.exec(`UPDATE users SET default_effort = 'low', default_claude_model = 'opus' WHERE id = $1`, fx.userID)
	frozen := fx.queuedRun(t, "issue")
	setRunStartOptions(fx.env, frozen, "sonnet", "run_override", "xhigh", "run_override")

	p := fx.claimPayload(t)
	if p.RunID != frozen.String() {
		t.Fatalf("claimed %s, want %s", p.RunID, frozen)
	}
	requireEffort(t, p, "xhigh", "run_override")
	if p.Config.DefaultModel == nil || *p.Config.DefaultModel != "sonnet" || p.Config.DefaultModelSource == nil || *p.Config.DefaultModelSource != "run_override" {
		t.Fatalf("default_model = %v / %v, want sonnet / run_override", deref(p.Config.DefaultModel), deref(p.Config.DefaultModelSource))
	}

	fx.env.exec(`UPDATE users SET default_effort = 'high' WHERE id = $1`, fx.userID)
	requeueClaimed(fx.env, frozen)
	p = fx.claimPayload(t)
	if p.RunID != frozen.String() {
		t.Fatalf("reclaimed %s, want %s", p.RunID, frozen)
	}
	requireEffort(t, p, "xhigh", "run_override")

	// Park the first run so the sibling is the next claim, then claim an unset run.
	fx.env.exec(`UPDATE runs SET status = 'cancelled', status_since = now(), finished_at = now() WHERE id = $1`, frozen)
	sibling := fx.queuedRun(t, "issue")
	p = fx.claimPayload(t)
	if p.RunID != sibling.String() {
		t.Fatalf("claimed %s, want the sibling %s", p.RunID, sibling)
	}
	requireEffort(t, p, "high", "user_default")
	if p.Config.DefaultModelSource == nil || *p.Config.DefaultModelSource != "user_default" {
		t.Fatalf("sibling model source = %v, want the lane's user_default", deref(p.Config.DefaultModelSource))
	}
}

// TestClaimRunOverrideEffortBeatsRuntimeAndUnsetClaudeLiveDB: precedence over a NULL and a `runtime`
// owner choice.
func TestClaimRunOverrideEffortBeatsRuntimeAndUnsetClaudeLiveDB(t *testing.T) {
	for _, user := range []any{nil, "runtime"} {
		fx := newCDFix(t)
		fx.env.exec(`UPDATE users SET default_effort = $2 WHERE id = $1`, fx.userID, user)
		id := fx.queuedRun(t, "issue")
		setRunStartOptions(fx.env, id, nil, nil, "max", "run_override")
		requireEffort(t, fx.claimPayload(t), "max", "run_override")
	}
}

// TestClaimIncompatibleSchedulePinStillFallsBackLiveDB: the schedule-pin lane is unchanged: an
// incompatible pin falls back to the lane with the visible note and the lane's own source.
func TestClaimIncompatibleSchedulePinStillFallsBackLiveDB(t *testing.T) {
	fx := newCDFix(t)
	fx.env.exec(`UPDATE users SET default_claude_model = 'opus' WHERE id = $1`, fx.userID)
	id := fx.queuedRun(t, "issue")
	setRunStartOptions(fx.env, id, "gpt-6-sol", "schedule_pin", nil, nil)
	p := fx.claimPayload(t)
	if p.Config.DefaultModel == nil || *p.Config.DefaultModel != "opus" || p.Config.DefaultModelSource == nil || *p.Config.DefaultModelSource != "user_default" {
		t.Fatalf("default_model = %v / %v, want opus / user_default", deref(p.Config.DefaultModel), deref(p.Config.DefaultModelSource))
	}
	var n int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM run_messages WHERE run_id = $1 AND kind = $2`, id, harnessModelFallbackKind).Scan(&n); err != nil || n != 1 {
		t.Fatalf("harness_model_fallback notes = %d (%v), want 1", n, err)
	}
	if r := mustRun(t, fx.env, id); r.Status != "claimed" {
		t.Fatalf("status = %s, want claimed (a pin falls back, it never holds)", r.Status)
	}
}

// TestClaimRunOverrideUnavailableClaudeLiveDB: an unhonorable run_override model (101 bytes, a bidi
// control) or an out-of-set effort parks the claim paused/run_override_unavailable with exactly one
// message; no promoter or sweeper moves it; the owner's resume requeues it with the hold kept and
// the next claim parks it again.
func TestClaimRunOverrideUnavailableClaudeLiveDB(t *testing.T) {
	bidi := "op" + string(rune(0x202e)) + "us"
	for _, tc := range []struct {
		name, setting, value string
		model, effort        any
	}{
		{"101-byte model", "model", strings.Repeat("a", 101), strings.Repeat("a", 101), nil},
		{"bidi model", "model", bidi, bidi, nil},
		{"codex role id", "model", "gpt-6-sol", "gpt-6-sol", nil},
		{"out-of-set effort", "effort", "ultra", nil, "ultra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCDFix(t)
			id := fx.queuedRun(t, "issue")
			var modelSource, effortSource any
			if tc.model != nil {
				modelSource = "run_override"
			}
			if tc.effort != nil {
				effortSource = "run_override"
			}
			setRunStartOptions(fx.env, id, tc.model, modelSource, tc.effort, effortSource)

			payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil)
			if err != nil || payload != nil {
				t.Fatalf("Claim = (%v, %v), want an idle claim", payload != nil, err)
			}
			requireOverrideHeld(t, fx.env, id, fx.workerID, 1, tc.setting, tc.value)

			// No automatic promoter or sweeper moves the held run.
			fx.svc.RequestCredentialDisabledPromotion(fx.userID)
			if _, err := fx.svc.Sweep(fx.env.ctx); err != nil {
				t.Fatalf("Sweep: %v", err)
			}
			requireOverrideHeld(t, fx.env, id, fx.workerID, 1, tc.setting, tc.value)

			// The owner's resume requeues it, dropping the worker and keeping the hold.
			if _, err := fx.env.q.ResumePausedRun(fx.env.ctx, store.ResumePausedRunParams{
				ID: id, UserID: fx.userID, GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
			}); err != nil {
				t.Fatalf("ResumePausedRun: %v", err)
			}
			r := mustRun(t, fx.env, id)
			if r.Status != "queued" || r.WorkerID.Valid || r.HoldReason.String != HoldReasonRunOverrideUnavailable {
				t.Fatalf("resumed: status=%s worker=%v hold=%v, want queued, no worker, hold kept", r.Status, r.WorkerID, r.HoldReason)
			}
			if payload, err := fx.svc.Claim(fx.env.ctx, fx.worker(t), nil); err != nil || payload != nil {
				t.Fatalf("reclaim = (%v, %v), want an idle claim", payload != nil, err)
			}
			requireOverrideHeld(t, fx.env, id, fx.workerID, 2, tc.setting, tc.value)
		})
	}
}

// TestFinishRunOverrideNonExactClaimWritesNothingLiveDB: a stale worker or generation writes neither
// the park nor the message.
func TestFinishRunOverrideNonExactClaimWritesNothingLiveDB(t *testing.T) {
	for _, stale := range []string{"worker", "generation"} {
		t.Run(stale, func(t *testing.T) {
			fx := newCDFix(t)
			id := fx.queuedRun(t, "issue")
			setRunStartOptions(fx.env, id, nil, nil, "ultra", "run_override")
			run, err := fx.env.q.ClaimRun(fx.env.ctx, claimRunParamsFor(fx.worker(t)))
			if err != nil || run.ID != id {
				t.Fatalf("ClaimRun = %v, %v", run.ID, err)
			}
			ident := claimRecoveryIdentity{workerID: fx.workerID}
			if stale == "worker" {
				ident.workerID = uuid.New()
			} else {
				run.ClaimGeneration--
			}
			cause := &runOverrideUnavailableError{Setting: "effort", Value: "ultra", Harness: HarnessClaude}
			if out, err := fx.svc.finishRunClaim(fx.env.ctx, run, nil, cause, ident); out != nil || err != nil {
				t.Fatalf("finishRunClaim = (%v, %v), want idle", out != nil, err)
			}
			if r := mustRun(t, fx.env, id); r.Status != "claimed" || r.HoldReason.Valid || r.ClaimReleasedAt.Valid {
				t.Fatalf("a non-exact claim was parked: status=%s hold=%v released=%v", r.Status, r.HoldReason, r.ClaimReleasedAt)
			}
			if n := len(overrideMessages(t, fx.env, id)); n != 0 {
				t.Fatalf("a non-exact claim wrote %d messages", n)
			}
		})
	}
}

// TestClaimRunOverrideCodexLiveDB: the Codex harness freezes the effort and parks a non-curated
// model or out-of-set effort.
func TestClaimRunOverrideCodexLiveDB(t *testing.T) {
	t.Run("frozen effort and curated model", func(t *testing.T) {
		env := setupCodexLiveDB(t)
		fx := newCodexClaimFix(t, env, false)
		env.exec(`UPDATE users SET default_codex_effort = 'low' WHERE id = $1`, fx.userID)
		setRunStartOptions(env, fx.runID, "gpt-6-sol", "run_override", "xhigh", "run_override")
		p, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
		if err != nil || p == nil {
			t.Fatalf("Claim = (%v, %v)", p != nil, err)
		}
		requireEffort(t, p, "xhigh", "run_override")
		if p.Config.DefaultModel == nil || *p.Config.DefaultModel != "gpt-6-sol" || p.Config.DefaultModelSource == nil || *p.Config.DefaultModelSource != "run_override" {
			t.Fatalf("default_model = %v / %v", deref(p.Config.DefaultModel), deref(p.Config.DefaultModelSource))
		}
	})
	for _, tc := range []struct {
		name, setting, value string
		model, effort        any
	}{
		{"non-curated model", "model", "custom-codex-preview", "custom-codex-preview", nil},
		{"claude alias model", "model", "opus", "opus", nil},
		{"out-of-set effort", "effort", "ultra", nil, "ultra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupCodexLiveDB(t)
			fx := newCodexClaimFix(t, env, false)
			var modelSource, effortSource any
			if tc.model != nil {
				modelSource = "run_override"
			}
			if tc.effort != nil {
				effortSource = "run_override"
			}
			setRunStartOptions(env, fx.runID, tc.model, modelSource, tc.effort, effortSource)
			p, err := fx.svc.Claim(env.ctx, fx.claimant(t, true), nil)
			if err != nil || p != nil {
				t.Fatalf("Claim = (%v, %v), want an idle claim", p != nil, err)
			}
			requireOverrideHeld(t, env, fx.runID, fx.workerB, 1, tc.setting, tc.value)
			if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{
				ID: fx.runID, UserID: fx.userID, GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
			}); err != nil {
				t.Fatalf("ResumePausedRun: %v", err)
			}
			r := mustRun(t, env, fx.runID)
			if r.Status != "queued" || r.WorkerID.Valid || r.HoldReason.String != HoldReasonRunOverrideUnavailable {
				t.Fatalf("resumed: status=%s worker=%v hold=%v", r.Status, r.WorkerID, r.HoldReason)
			}
		})
	}
}

// The Fresh controls: ResumePausedRun's one narrow exception for a restored one-claim Fresh grant.

func (p policyLive) overrideResume(id uuid.UUID, userID uuid.UUID) (store.ResumePausedRunRow, error) {
	return p.e.q.ResumePausedRun(p.e.ctx, store.ResumePausedRunParams{
		ID: id, UserID: userID, GlobalTimeoutSeconds: int32(testParams().RunTimeout.Seconds()),
	})
}

func overrideCause() error {
	return &runOverrideUnavailableError{Setting: "effort", Value: "ultra", Harness: HarnessClaude}
}

// TestRunOverrideParkRestoresFreshGrantLiveDB: a preserving run with a resolved episode whose
// consuming claim parks on run_override_unavailable gets its grant back, the owner's resume requeues
// it, the next claim consumes it exactly once, and a re-park restores the same single grant.
func TestRunOverrideParkRestoresFreshGrantLiveDB(t *testing.T) {
	p := newPolicyLive(t)
	id, _ := p.freshHeld(t)
	p.e.exec(t, "UPDATE runs SET harness='claude' WHERE id=$1", id)
	ident := claimRecoveryIdentity{workerID: p.wid}
	consumed := func(want int) {
		t.Helper()
		if n := p.freshAuditCount(t, id, "consumed"); n != want {
			t.Fatalf("consumed audit rows = %d, want %d", n, want)
		}
	}
	restored := func(want int) {
		t.Helper()
		if n := p.freshAuditCount(t, id, "restored"); n != want {
			t.Fatalf("restored audit rows = %d, want %d", n, want)
		}
	}

	claimed := p.reclaim(t, id) // generation 2 consumes the grant
	if claimed.ClaimGeneration != 2 || claimed.PolicyPendingFresh || claimed.PolicyFreshConsumedGeneration.Int64 != 2 {
		t.Fatalf("claim 1: %s", freshStamps(claimed))
	}
	consumed(1)
	if out, err := p.svc.finishRunClaim(p.e.ctx, claimed, nil, overrideCause(), ident); out != nil || err != nil {
		t.Fatalf("finishRunClaim: %v %v", out, err)
	}
	got := p.run(t, id)
	if got.Status != "paused" || got.HoldReason.String != HoldReasonRunOverrideUnavailable || !got.PolicyPendingFresh ||
		got.PolicyFreshConsumedGeneration.Valid || !got.PolicyPreserveRequired || !got.PolicyResolvedAt.Valid {
		t.Fatalf("park 1: %s hold=%v", freshStamps(got), got.HoldReason)
	}
	consumed(1)
	restored(1)

	if _, err := p.overrideResume(id, p.e.userID); err != nil {
		t.Fatalf("ResumePausedRun on the restored grant: %v", err)
	}
	got = p.run(t, id)
	if got.Status != "queued" || !got.PolicyPendingFresh || got.WorkerID.Valid || got.HoldReason.String != HoldReasonRunOverrideUnavailable {
		t.Fatalf("resume: %s worker=%v hold=%v", freshStamps(got), got.WorkerID, got.HoldReason)
	}

	claimed = p.reclaim(t, id) // generation 3 consumes the restored grant exactly once
	if claimed.ClaimGeneration != 3 || claimed.PolicyPendingFresh || claimed.PolicyFreshConsumedGeneration.Int64 != 3 {
		t.Fatalf("claim 2: %s", freshStamps(claimed))
	}
	consumed(2)
	restored(1)
	if out, err := p.svc.finishRunClaim(p.e.ctx, claimed, nil, overrideCause(), ident); out != nil || err != nil {
		t.Fatalf("finishRunClaim 2: %v %v", out, err)
	}
	got = p.run(t, id)
	if !got.PolicyPendingFresh || got.PolicyFreshConsumedGeneration.Valid || got.Status != "paused" {
		t.Fatalf("park 2 did not restore the same grant: %s", freshStamps(got))
	}
	consumed(2)
	restored(2)

	if _, err := p.overrideResume(id, p.e.userID); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	claimed = p.reclaim(t, id) // the following claim may consume it
	if claimed.ClaimGeneration != 4 || claimed.PolicyPendingFresh || claimed.PolicyFreshConsumedGeneration.Int64 != 4 {
		t.Fatalf("claim 3: %s", freshStamps(claimed))
	}
	consumed(3)
	restored(2)
}

// TestResumePausedRunRefusesPendingFreshOutsideTheExceptionLiveDB: the exception is exactly a
// run_override_unavailable hold on a RESOLVED episode. Every other pending-Fresh hold is refused with
// no row and no change.
func TestResumePausedRunRefusesPendingFreshOutsideTheExceptionLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, hold string
		unresolved bool
	}{
		{"codex_policy_hold", "codex_policy_hold", false},
		{"run_override_unavailable with an unresolved episode", HoldReasonRunOverrideUnavailable, true},
		{"provider_unavailable", "provider_unavailable", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPolicyLive(t)
			id, _ := p.freshHeld(t)
			p.e.exec(t, `UPDATE runs SET status='paused', status_since=now(), hold_reason=$2,
				policy_resolved_at = CASE WHEN $3 THEN NULL ELSE policy_resolved_at END WHERE id=$1`, id, tc.hold, tc.unresolved)
			before := p.run(t, id)
			if !before.PolicyPendingFresh {
				t.Fatalf("setup: %s", freshStamps(before))
			}
			_, err := p.overrideResume(id, p.e.userID)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("ResumePausedRun = %v, want no row", err)
			}
			after := p.run(t, id)
			if after.Status != "paused" || after.HoldReason.String != tc.hold || !after.PolicyPendingFresh {
				t.Fatalf("a refused resume changed the run: %s hold=%v", freshStamps(after), after.HoldReason)
			}
		})
	}
}
