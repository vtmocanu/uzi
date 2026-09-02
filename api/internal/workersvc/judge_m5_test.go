package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #69 M5 (Decision 9): Gate 5, the per-user, count-based, best-effort judge spend
// guards — a cooldown and a rolling-24h daily budget — checked in every mode BEFORE the
// idempotency insert. On trip the judge is skipped silently (no CreateJudgeRun), exactly
// like a Gate 3/4 miss. Deliberately FAIL-OPEN: any settings/query read error proceeds
// to enqueue, since the guards are a soft cost backstop, not a correctness gate.

// ts is a Valid pgtype.Timestamptz at t.
func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// TestJudgeCooldownGuard: a judge enqueued within the cooldown window is skipped; one
// outside it proceeds; cooldown=0 disables the guard entirely.
func TestJudgeCooldownGuard(t *testing.T) {
	t.Run("within cooldown skips", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.lastJudgeAt = ts(time.Now().Add(-30 * time.Second)) // 30s ago, inside 60s
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", cooldownSeconds: 60})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun != nil {
			t.Fatal("a judge within the cooldown window must be skipped, no run created")
		}
	})

	t.Run("outside cooldown proceeds", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.lastJudgeAt = ts(time.Now().Add(-5 * time.Minute)) // well outside 60s
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", cooldownSeconds: 60})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("a judge outside the cooldown window must proceed")
		}
	})

	t.Run("no prior judge proceeds", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		// lastJudgeAt zero value → Valid:false (NULL) → no cooldown in effect.
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", cooldownSeconds: 60})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("a user with no prior judge (NULL last) must proceed")
		}
	})

	t.Run("cooldown=0 disables the guard", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.lastJudgeAt = ts(time.Now()) // just now — would trip any positive cooldown
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", cooldownSeconds: 0})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("cooldown=0 disables the guard; the judge must proceed")
		}
	})
}

// TestJudgeDailyBudgetGuard: a user at or above the budget in 24h is skipped; below it
// proceeds; budget=0 disables the guard. The rolling-24h window is asserted via the
// Since arg the guard passes.
func TestJudgeDailyBudgetGuard(t *testing.T) {
	t.Run("at budget skips", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.judgesSince = 5
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", dailyBudget: 5})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun != nil {
			t.Fatal("a user at the daily budget must be skipped")
		}
		// The budget count query asks for the rolling-24h window.
		if len(fs.judgesSinceArgs) != 1 {
			t.Fatalf("CountJudgesSince called %d times, want 1", len(fs.judgesSinceArgs))
		}
		since := fs.judgesSinceArgs[0].Since
		if !since.Valid {
			t.Fatal("CountJudgesSince Since must be a valid timestamp")
		}
		if d := time.Since(since.Time); d < 23*time.Hour || d > 25*time.Hour {
			t.Fatalf("Since is %v ago, want ~24h (rolling window)", d)
		}
		if fs.judgesSinceArgs[0].UserID != run.UserID {
			t.Fatalf("CountJudgesSince UserID = %v, want the run owner %v", fs.judgesSinceArgs[0].UserID, run.UserID)
		}
	})

	t.Run("above budget skips", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.judgesSince = 9
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", dailyBudget: 5})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun != nil {
			t.Fatal("a user above the daily budget must be skipped")
		}
	})

	t.Run("below budget proceeds", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.judgesSince = 4
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", dailyBudget: 5})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("a user below the daily budget must proceed")
		}
	})

	t.Run("budget=0 disables the guard", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.judgesSince = 1000 // would trip any positive budget
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", dailyBudget: 0})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("budget=0 disables the guard; the judge must proceed")
		}
		if len(fs.judgesSinceArgs) != 0 {
			t.Fatalf("budget=0 must not query the count; got %d calls", len(fs.judgesSinceArgs))
		}
	})
}

// TestJudgeSpendGuardsFailOpen pins the deliberate FAIL-OPEN semantics: on ANY read
// error — settings accessor or the count query — the guard does NOT trip, and the
// enqueue proceeds. A transient DB/settings hiccup must never silently disable judging.
func TestJudgeSpendGuardsFailOpen(t *testing.T) {
	t.Run("settings read error proceeds", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.lastJudgeAt = ts(time.Now()) // would trip if the guard were consulted
		fs.judgesSince = 1000           // would trip if the guard were consulted
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", cooldownSeconds: 60, dailyBudget: 5,
			spendGuardErr: errors.New("settings blip")})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("a settings read error must fail OPEN — the judge proceeds")
		}
	})

	t.Run("cooldown query error proceeds", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.lastJudgeAtErr = errors.New("db blip")
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", cooldownSeconds: 60})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("a cooldown query error must fail OPEN — the judge proceeds")
		}
	})

	t.Run("budget query error proceeds", func(t *testing.T) {
		fs, svc, run := eligibleFixture(t)
		fs.judgesSinceErr = errors.New("db blip")
		svc.SetSettings(fakeSettings{enabled: true, model: "haiku", dailyBudget: 5})
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun == nil {
			t.Fatal("a budget query error must fail OPEN — the judge proceeds")
		}
	})
}

// TestJudgeSpendGuardsApplyUnderEnforceAll pins that Gate 5 applies in enforced mode
// too (a runaway loop is a footgun even for an admin-enforced user): enforceAll=true
// still respects the cooldown and budget.
func TestJudgeSpendGuardsApplyUnderEnforceAll(t *testing.T) {
	// Opted-out owner, admin-enforced, but inside the cooldown → still skipped.
	fs, svc, run := eligibleFixture(t)
	fs.userByID = store.User{JudgeEnabled: false}
	fs.lastJudgeAt = ts(time.Now().Add(-10 * time.Second))
	svc.SetSettings(fakeSettings{enabled: true, enforceAll: true, model: "haiku", cooldownSeconds: 60})
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun != nil {
		t.Fatal("Gate 5 must apply under enforceAll: a cooldown-throttled judge stays skipped")
	}

	// Same enforced owner, over budget → still skipped.
	fs, svc, run = eligibleFixture(t)
	fs.userByID = store.User{JudgeEnabled: false}
	fs.judgesSince = 3
	svc.SetSettings(fakeSettings{enabled: true, enforceAll: true, model: "haiku", dailyBudget: 3})
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun != nil {
		t.Fatal("Gate 5 must apply under enforceAll: an over-budget judge stays skipped")
	}
}

// TestJudgeLoopThrottledByGuards is the runaway-loop scenario Decision 9 exists for: a
// user's rapid stream of failed runs is throttled by the cooldown (only the first of a
// burst enqueues while the window is warm) and hard-capped by the daily budget (once the
// 24h count reaches it, every subsequent terminal run is skipped). Modeled by driving
// maybeEnqueueJudge repeatedly and advancing the fake's recorded state as a real run
// stream would.
func TestJudgeLoopThrottledByGuards(t *testing.T) {
	// Cooldown throttle: with a warm last-judge stamp, a burst of terminal runs enqueues
	// nothing; once the stamp is old, the next one proceeds.
	fs, svc, _ := eligibleFixture(t)
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku", cooldownSeconds: 60})
	// One user's runaway: a fixed owner across the burst, since the cooldown/budget are
	// per-user (the queries key on run.UserID). Varying the id would model unrelated
	// users, not the loop Decision 9 exists to throttle.
	burstUser := uuid.New()
	fs.lastJudgeAt = ts(time.Now()) // a judge just went out for this user
	for i := 0; i < 5; i++ {
		fs.createdJudgeRun = nil
		run := store.Run{ID: uuid.New(), UserID: burstUser, Kind: runkind.Issue, Status: "failed"}
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun != nil {
			t.Fatalf("burst run %d enqueued a judge while the cooldown was warm", i)
		}
	}
	// Window elapsed → the same user's next terminal run judges.
	fs.createdJudgeRun = nil
	fs.lastJudgeAt = ts(time.Now().Add(-2 * time.Minute))
	run := store.Run{ID: uuid.New(), UserID: burstUser, Kind: runkind.Issue, Status: "failed"}
	svc.maybeEnqueueJudge(context.Background(), run)
	if fs.createdJudgeRun == nil {
		t.Fatal("after the cooldown window elapsed, the next run must judge")
	}

	// Budget cap: with no cooldown, once one user's rolling-24h count reaches the budget
	// every further run of theirs is skipped.
	fs, svc, _ = eligibleFixture(t)
	svc.SetSettings(fakeSettings{enabled: true, model: "haiku", dailyBudget: 3})
	budgetUser := uuid.New()
	fs.judgesSince = 3 // this user is already at cap
	for i := 0; i < 4; i++ {
		fs.createdJudgeRun = nil
		run := store.Run{ID: uuid.New(), UserID: budgetUser, Kind: runkind.Issue, Status: "failed"}
		svc.maybeEnqueueJudge(context.Background(), run)
		if fs.createdJudgeRun != nil {
			t.Fatalf("run %d enqueued a judge despite the daily budget being reached", i)
		}
	}
}
