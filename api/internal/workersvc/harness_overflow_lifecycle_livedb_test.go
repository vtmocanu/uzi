package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHarnessOverflowClaimParkPromoteReclaimFrozenLiveDB(t *testing.T) {
	fx := newOverflowFix(t)
	env := fx.env
	cleanupOverflowLifecycleUser(t, env, fx.userID)
	resealBotPAT(t, env, fx.userID)
	// The creation-only fixture has a gauge placeholder; make Claude usable too,
	// so a harness reevaluation cannot hide behind a credential assembly failure.
	sealed, err := env.box.Seal([]byte("overflow-claude-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	env.exec("UPDATE user_secrets SET ciphertext=$1 WHERE id=$2", sealed, fx.token)

	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetForges(originDriftForges{f: &originDriftForge{}})
	svc.p.Autoselect = fx.svc.p.Autoselect
	svc.p.UsagePollInterval = fx.svc.p.UsagePollInterval
	svc.p.CodexUsagePollInterval = fx.svc.p.CodexUsagePollInterval
	svc.p.RunLimitMaxWaits = 5
	svc.p.RunLimitMaxPark = 8 * 24 * time.Hour
	now := time.Now().UTC().Truncate(time.Millisecond)
	svc.now = func() time.Time { return now }
	fx.svc = svc

	workerID := env.seedCapableCodexWorker(t, fx.userID)
	env.exec("UPDATE workers SET anthropic_bind_mode='auto', protocol_capabilities=$2 WHERE id=$1",
		workerID, []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.RecoveryArchiveV1})
	w := wkrRow(t, env, workerID)
	wait := true
	run, err := svc.CreateRun(env.ctx, fx.userID, fx.repoID, fx.nextIssue(),
		"overflow lifecycle", &wait, nil, false, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	created := mustRun(t, env, run.ID)
	if created.Status != "queued" || !created.WaitOnLimit || created.Harness != "codex" ||
		created.HarnessPreferred.String != "claude" || created.HarnessDecisionReason.String != "overflow" ||
		!created.CodexSecretID.Valid || uuid.UUID(created.CodexSecretID.Bytes) != fx.codex.alias ||
		!created.CodexAuthMode.Valid || created.CodexAuthMode.String != codexAuthModeSubscription ||
		!created.CodexAccountKey.Valid || created.CodexAccountKey.String != accountKeyOf(fx.poolFix, fx.codex) {
		t.Fatal("creation did not queue an overflow-selected subscription Codex run")
	}

	// Compare all eight decision columns, including nullable evidence, and the
	// existing harness/binding columns after every real lifecycle transition.
	type frozenDecision struct {
		harness                          string
		preferred, reason                pgtype.Text
		preferredScore, otherScore       pgtype.Float8
		preferredMeasured, otherMeasured pgtype.Float8
		preferredHeld, otherHeld         pgtype.Bool
		codexSecretID                    pgtype.UUID
		codexAuthMode, codexAccountKey   pgtype.Text
	}
	snapshot := func(r store.Run) frozenDecision {
		return frozenDecision{
			harness: r.Harness, preferred: r.HarnessPreferred, reason: r.HarnessDecisionReason,
			preferredScore: r.HarnessPreferredScore, otherScore: r.HarnessOtherScore,
			preferredMeasured: r.HarnessPreferredMeasured, otherMeasured: r.HarnessOtherMeasured,
			preferredHeld: r.HarnessPreferredHeld, otherHeld: r.HarnessOtherHeld,
			codexSecretID: r.CodexSecretID, codexAuthMode: r.CodexAuthMode, codexAccountKey: r.CodexAccountKey,
		}
	}
	frozen := snapshot(created)
	assertFrozen := func(stage, status string) store.Run {
		t.Helper()
		got := mustRun(t, env, run.ID)
		if got.Status != status {
			t.Fatalf("%s: status=%s want %s", stage, got.Status, status)
		}
		if after := snapshot(got); after != frozen {
			t.Fatalf("%s changed frozen decision/binding: got %+v want %+v", stage, after, frozen)
		}
		return got
	}
	// Reverse both usage evidence and the default harness BEFORE the first
	// claim. With Codex preferred but nearly exhausted, a new run selects Claude.
	fx.claudeReading(fx.token, 0, "1 minute")
	fx.reading(fx.codex, 99)
	setDefaultHarness(t, env, fx.userID, "codex")
	assertFrozen("before initial claim", "queued")

	first, err := svc.Claim(env.ctx, w, nil)
	if err != nil || first == nil {
		t.Fatalf("initial claim: payload=%v err=%v", first != nil, err)
	}
	if first.RunID != run.ID.String() || first.Secrets.Codex == nil ||
		first.Secrets.Codex.AuthMode != codexAuthModeSubscription ||
		first.Secrets.Codex.AccessToken == "" || first.Secrets.Codex.Capability == "" ||
		first.Secrets.AnthropicOAuthToken != "" || first.ClaimGeneration != created.ClaimGeneration+1 {
		t.Fatal("initial claim did not deliver the overflow run with subscription credentials and a new generation")
	}
	claimed := assertFrozen("initial claim", "claimed")
	if claimed.ClaimGeneration != first.ClaimGeneration {
		t.Fatal("initial claim payload generation differs from stored generation")
	}
	gen := first.ClaimGeneration
	if _, applied, err := svc.SetState(env.ctx, w, run.ID, StateRequest{State: "running", ClaimGeneration: &gen}); err != nil || !applied {
		t.Fatalf("start: applied=%v err=%v", applied, err)
	}
	assertFrozen("before park", "running")
	reset := now.Add(2 * time.Minute).UnixMilli()
	if _, applied, err := svc.SetState(env.ctx, w, run.ID, StateRequest{
		State: "limit_wait", ClaimGeneration: &gen, LimitResetsAt: &reset, RateLimitType: strPtr("seven_day"),
	}); err != nil || !applied {
		t.Fatalf("usage-limit park: applied=%v err=%v", applied, err)
	}
	parked := assertFrozen("usage-limit park", "limit_wait")
	if !parked.RetryNotBefore.Valid || !parked.RetryNotBefore.Time.After(now) ||
		parked.LimitWaitCount != created.LimitWaitCount+1 || parked.ClaimGeneration != gen {
		t.Fatal("park did not preserve the generation and record a future timed retry")
	}

	// Drive the existing timer query with its explicit clock, without SQL status
	// writes or sleeping. The run must stay parked until its recorded deadline.
	if _, err := env.q.PromoteLimitWaitRuns(env.ctx, pgconv.Time(parked.RetryNotBefore.Time.Add(-time.Microsecond))); err != nil {
		t.Fatal(err)
	}
	assertFrozen("before timed promotion", "limit_wait")
	now = parked.RetryNotBefore.Time
	promoted, err := env.q.PromoteLimitWaitRuns(env.ctx, pgconv.Time(now))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range promoted {
		if r.ID == run.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("timer promotion missed the overflow run")
	}
	queued := assertFrozen("timed promotion / before reclaim", "queued")
	if queued.ClaimGeneration != gen {
		t.Fatal("promotion changed the claim generation")
	}
	fresh, err := svc.Claim(env.ctx, w, nil)
	if err != nil || fresh == nil {
		t.Fatalf("reclaim: payload=%v err=%v", fresh != nil, err)
	}
	if fresh.RunID != run.ID.String() || fresh.Secrets.Codex == nil ||
		fresh.Secrets.Codex.AuthMode != codexAuthModeSubscription ||
		fresh.Secrets.Codex.AccessToken == "" || fresh.Secrets.Codex.Capability == "" ||
		fresh.Secrets.Codex.Capability == first.Secrets.Codex.Capability ||
		fresh.Secrets.AnthropicOAuthToken != "" || fresh.ClaimGeneration != gen+1 {
		t.Fatal("reclaim did not deliver the same Codex run with a fresh capability and generation")
	}
	reclaimed := assertFrozen("reclaim", "claimed")
	if reclaimed.ClaimGeneration != fresh.ClaimGeneration {
		t.Fatal("reclaim payload generation differs from stored generation")
	}

	// Positive control through creation's public API: the changed settings and
	// gauges really reverse selection, while the existing run stayed frozen.
	control, err := fx.issue()
	if err != nil {
		t.Fatal(err)
	}
	if control.Harness != "claude" || control.HarnessPreferred.String != "codex" ||
		control.HarnessDecisionReason.String != "overflow" {
		t.Fatalf("changed evidence did not reverse a new decision: harness=%s preferred=%v reason=%v",
			control.Harness, control.HarnessPreferred, control.HarnessDecisionReason)
	}
	assertFrozen("after reverse-choice control", "claimed")
}

// cleanupOverflowLifecycleUser disposes of this test's own recovery custody
// before the user is deleted. Claim opens custody holds whose live-run and
// live-worker FKs are ON DELETE RESTRICT, so the inherited pool-fixture cleanup
// (an unchecked user delete) would fail and leave the user, its runs and holds
// in the shared LiveDB sweep. Registered after the fixture, so it runs first.
func cleanupOverflowLifecycleUser(t *testing.T, env codexTestEnv, userID uuid.UUID) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		tx, err := env.pool.Begin(ctx)
		if err != nil {
			t.Errorf("cleanup begin: %v", err)
			return
		}
		defer func() { _ = tx.Rollback(ctx) }()
		holds := `SELECT id FROM recovery_custody_holds WHERE user_id = $1`
		for _, q := range []string{
			`DELETE FROM recovery_captures WHERE hold_id IN (` + holds + `)`,
			`DELETE FROM run_held_publications WHERE hold_id IN (` + holds + `)`,
			`DELETE FROM recovery_prerequisite_closures WHERE hold_id IN (` + holds + `)`,
			`DELETE FROM run_salvage WHERE live_run_id IN (SELECT id FROM runs WHERE user_id = $1)`,
			`DELETE FROM recovery_custody_holds WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, userID); err != nil {
				t.Errorf("cleanup %q: %v", q, err)
				return
			}
		}
		var users, runs, holdsLeft int
		if err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM users WHERE id = $1),
			(SELECT count(*) FROM runs WHERE user_id = $1),
			(SELECT count(*) FROM recovery_custody_holds WHERE user_id = $1)`, userID).Scan(&users, &runs, &holdsLeft); err != nil {
			t.Errorf("cleanup verify: %v", err)
			return
		}
		if users+runs+holdsLeft != 0 {
			t.Errorf("cleanup left users=%d runs=%d holds=%d", users, runs, holdsLeft)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			t.Errorf("cleanup commit: %v", err)
		}
	})
}
