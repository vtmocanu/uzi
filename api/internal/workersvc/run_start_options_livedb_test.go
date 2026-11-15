package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3250 M1: every run-INSERT path stamps model provenance, and a run_override model makes the
// run ineligible for the automatic harness. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres; run via ./e2e/run-store-it.sh.

func modelPair(r store.Run) (model, source pgtype.Text) { return r.Model, r.ModelSource }

func wantPair(t *testing.T, label string, r store.Run, model, source string) {
	t.Helper()
	gm, gs := modelPair(r)
	if model == "" {
		if gm.Valid || gs.Valid {
			t.Errorf("%s: model=%v source=%v, want both NULL", label, gm, gs)
		}
		return
	}
	if !gm.Valid || gm.String != model || !gs.Valid || gs.String != source {
		t.Errorf("%s: model=%v source=%v, want %q/%q", label, gm, gs, model, source)
	}
}

// TestScheduleOriginRunsStampSchedulePinLiveDB covers all four schedule-origin INSERT paths:
// a schedule pin when a model is written, NULL provenance when none is.
func TestScheduleOriginRunsStampSchedulePinLiveDB(t *testing.T) {
	model := "opus"
	t.Run("CreateScheduledRun", func(t *testing.T) {
		fx := newOverflowFix(t)
		with, err := fx.svc.CreateScheduledRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "d", nil, nil, &model, false, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "with model", mustRun(t, fx.env, with.ID), "opus", "schedule_pin")
		without, err := fx.svc.CreateScheduledRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "d", nil, nil, nil, false, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := mustRun(t, fx.env, without.ID)
		wantPair(t, "without model", got, "", "")
		if got.Effort.Valid || got.EffortSource.Valid {
			t.Errorf("a schedule never sets effort: %v/%v", got.Effort, got.EffortSource)
		}
	})
	t.Run("CreateScheduledAutopilotRun", func(t *testing.T) {
		fx := newOverflowFix(t)
		with, err := fx.svc.CreateScheduledAutopilotRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "d", nil, nil, &model, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "with model", mustRun(t, fx.env, with.ID), "opus", "schedule_pin")
		without, err := fx.svc.CreateScheduledAutopilotRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "d", nil, nil, nil, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "without model", mustRun(t, fx.env, without.ID), "", "")
	})
	t.Run("CreateAutopilotRun", func(t *testing.T) {
		fx := newOverflowFix(t)
		run, err := fx.svc.CreateAutopilotRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "d", nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "autopilot", mustRun(t, fx.env, run.ID), "", "")
	})
	t.Run("CreatePromptRun", func(t *testing.T) {
		fx := newOverflowFix(t)
		sched := seedPromptScheduleRow(t, fx.env, fx.userID, fx.repoID)
		with, err := fx.svc.CreatePromptRun(fx.env.ctx, fx.userID, fx.repoID, sched, "t", "p", false, false, nil, &model, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "with model", mustRun(t, fx.env, with.ID), "opus", "schedule_pin")
		fx.env.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, with.ID)
		without, err := fx.svc.CreatePromptRun(fx.env.ctx, fx.userID, fx.repoID, sched, "t", "p", false, false, nil, nil, false, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "without model", mustRun(t, fx.env, without.ID), "", "")
	})
	t.Run("CreateSelfImproveRun", func(t *testing.T) {
		fx := newOverflowFix(t)
		with, err := fx.svc.CreateSelfImproveRun(fx.env.ctx, fx.userID, fx.repoID, 9001, "t", "d", nil, &model, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "with model", mustRun(t, fx.env, with.ID), "opus", "schedule_pin")
		fx.env.exec(`UPDATE runs SET status = 'cancelled' WHERE id = $1`, with.ID)
		without, err := fx.svc.CreateSelfImproveRun(fx.env.ctx, fx.userID, fx.repoID, 9002, "t", "d", nil, nil, false, nil)
		if err != nil {
			t.Fatal(err)
		}
		wantPair(t, "without model", mustRun(t, fx.env, without.ID), "", "")
	})
}

// TestRunOverrideModelExcludesAutomaticHarnessLiveDB (D22): with the default harness unset and both
// harnesses usable, choosing a model records the "excluded" decision, while an effort-only start
// stays eligible for the automatic decision and its effort pair is stamped.
func TestRunOverrideModelExcludesAutomaticHarnessLiveDB(t *testing.T) {
	setup := func() *overflowFix {
		fx := newAutoFix(t, false)
		// Codex resets sooner, so an eligible automatic create lands on Codex.
		fx.claudeWeekly(fx.token, 20, "72 hours")
		fx.codexWeekly(fx.codex, 30, 86400)
		return fx
	}
	model, effort := "opus", "high"

	fx := setup()
	run, err := fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "d", nil, nil, false, nil, nil, nil, nil, &model, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := mustRun(t, fx.env, run.ID)
	if got.Harness != "claude" || got.HarnessDecisionReason.String != "excluded" {
		t.Fatalf("model-chosen create: harness=%s reason=%v, want claude/excluded", got.Harness, got.HarnessDecisionReason)
	}
	wantPair(t, "model-chosen", got, "opus", "run_override")

	fx = setup()
	run, err = fx.svc.CreateRun(fx.env.ctx, fx.userID, fx.repoID, fx.nextIssue(), "d", nil, nil, false, nil, nil, nil, nil, nil, &effort)
	if err != nil {
		t.Fatal(err)
	}
	got = mustRun(t, fx.env, run.ID)
	if got.Harness != "codex" || got.HarnessDecisionReason.String != "auto_soonest_reset" {
		t.Fatalf("effort-only create: harness=%s reason=%v, want an eligible automatic decision (codex/auto_soonest_reset)", got.Harness, got.HarnessDecisionReason)
	}
	wantPair(t, "effort-only", got, "", "")
	if !got.Effort.Valid || got.Effort.String != "high" || !got.EffortSource.Valid || got.EffortSource.String != "run_override" {
		t.Errorf("effort pair = %v/%v, want high/run_override", got.Effort, got.EffortSource)
	}
}
