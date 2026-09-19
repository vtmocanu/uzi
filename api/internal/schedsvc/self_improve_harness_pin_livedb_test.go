package schedsvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// self_improve_harness_pin_livedb_test.go is the BLOCKING-fix regression for the M4a review:
// fireSelfImprove (schedsvc/self_improve.go) previously called CreateSelfImproveRun WITHOUT the
// schedule's pinned harness, unlike the other three fire seams (firePrompt / the two scheduled-issue
// paths), which all thread scheduleHarness(sched). A self_improve schedule PINNED to Codex therefore
// silently fired a Claude run whenever the owner also had a usable Anthropic credential (D11 rule 4:
// both usable, no default set -> Claude) — violating D2/D4 ("pinned schedules never fall back"). It
// builds a real *workersvc.Service over a real Postgres (the exact RunCreator production fires
// through), so D11's credential-availability read and the atomic Codex freeze are genuine, not a
// fake. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// ./e2e/run-store-it.sh. Every name ends LiveDB so the store-IT sweep selects it.

// selfImproveSchedulePinned builds an in-memory (unpersisted) self_improve schedule row RunNow can
// fire directly, optionally pinned to harness ("" leaves it NULL, i.e. unpinned/implicit). Unlike a
// prompt/issue schedule fire, a self_improve fire needs NO persisted run_schedules row first:
// CreateSelfImproveRunParams carries no schedule_id column, so the run insert has no FK to satisfy.
func selfImproveSchedulePinned(userID, repoID uuid.UUID, harness string) store.RunSchedule {
	sc := store.RunSchedule{
		ID:          uuid.New(),
		UserID:      userID,
		RepoID:      repoID,
		Target:      "self_improve",
		Timing:      "recurring",
		CronExpr:    pgconv.Text("0 4 */2 * *"),
		Timezone:    "UTC",
		Origin:      "default",
		CatalogSlug: pgconv.Text("self-improve"),
		AutoApprove: true,
		Status:      "active",
		Enabled:     true,
	}
	if harness != "" {
		sc.Harness = pgconv.Text(harness)
	}
	return sc
}

// selfImproveFireBuilder wires the fakeForge/fakeBuilder (scheduler_test.go, same package) as the
// self_improve fire path's ForgeBuilder: fireSelfImprove needs a real forge to list/create the
// tracking issue (unlike firePrompt, which needs none), so a nil ForgeBuilder would nil-deref.
func selfImproveFireBuilder() *fakeBuilder {
	return &fakeBuilder{f: &fakeForge{createdIID: 501}}
}

// TestScheduleFireSelfImproveHarnessPinWinsOverImplicitLiveDB is the BLOCKING regression: a
// self_improve schedule PINNED to codex, for a user who has BOTH harnesses usable (so implicit D11
// would resolve Claude per rule 4 — both usable, no default set), must fire a run with
// harness='codex' and a frozen Codex binding. Before the fix, fireSelfImprove dropped the pin and
// this test reddens on Claude.
func TestScheduleFireSelfImproveHarnessPinWinsOverImplicitLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	userID, repoID := scheduleFireUser(ctx, t, pool)
	seedAnthropicDefault(ctx, t, pool, box, userID)      // Claude usable
	seedCodexAPIKeyDefault(ctx, t, q, pool, box, userID) // Codex ALSO usable -> implicit D11 would pick Claude (rule 4)

	runs := workersvc.New(q, box, workersvc.Params{})
	runs.SetTxBeginner(pool) // REQUIRED: a Codex freeze fails closed (errCodexCreateRequiresTx) without one
	sched := New(q, runs, selfImproveFireBuilder(), nil, nil, nil, time.Minute, nil)

	sc := selfImproveSchedulePinned(userID, repoID, "codex")
	out, err := sched.RunNow(ctx, sc)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(out.Skips) != 0 {
		t.Fatalf("Skips = %+v, want none", out.Skips)
	}
	if len(out.Started) != 1 {
		t.Fatalf("Started = %+v, want exactly one run", out.Started)
	}

	got, err := q.GetRunByID(ctx, out.Started[0].RunID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if got.Harness != "codex" {
		t.Fatalf("run harness = %q, want %q (a pinned self_improve schedule must never fall back to the implicit resolution)", got.Harness, "codex")
	}
	if !got.CodexSecretID.Valid {
		t.Fatalf("run harness=codex but codex_secret_id is not frozen")
	}
	if !got.CodexMaterialRevision.Valid {
		t.Fatalf("run harness=codex but codex_material_revision is not frozen")
	}
}

// TestScheduleFireSelfImproveNullHarnessResolvesImplicitlyLiveDB is the control: the IDENTICAL
// both-usable-credentials user with a NULL-harness (unpinned) self_improve schedule resolves
// implicitly to Claude (D11 rule 4), proving the pinned case above is attributable to the pin, not a
// fixture that always yields Codex regardless.
func TestScheduleFireSelfImproveNullHarnessResolvesImplicitlyLiveDB(t *testing.T) {
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	userID, repoID := scheduleFireUser(ctx, t, pool)
	seedAnthropicDefault(ctx, t, pool, box, userID)      // Claude usable
	seedCodexAPIKeyDefault(ctx, t, q, pool, box, userID) // Codex ALSO usable, no default set

	runs := workersvc.New(q, box, workersvc.Params{})
	runs.SetTxBeginner(pool) // REQUIRED: a Codex freeze fails closed (errCodexCreateRequiresTx) without one
	sched := New(q, runs, selfImproveFireBuilder(), nil, nil, nil, time.Minute, nil)

	sc := selfImproveSchedulePinned(userID, repoID, "") // NULL harness: implicit D11
	out, err := sched.RunNow(ctx, sc)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(out.Started) != 1 {
		t.Fatalf("Started = %+v, want exactly one run", out.Started)
	}

	got, err := q.GetRunByID(ctx, out.Started[0].RunID)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if got.Harness != "claude" {
		t.Fatalf("run harness = %q, want %q (D11 rule 4: both usable, no default set -> Claude)", got.Harness, "claude")
	}
}
