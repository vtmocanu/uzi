package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3252 m4: the phase-switch hold (running | awaiting_approval -> paused,
// hold_reason 'phase_switch_unavailable'). Skipped unless UZI_TEST_DATABASE_URL is set
// (setupCodexLiveDB skips).

// seedPhaseSwitchRun seeds a run owned by o's worker at claim generation 3 in the given status.
func seedPhaseSwitchRun(t *testing.T, env codexTestEnv, o reevalOwner, iid int64, status string) uuid.UUID {
	t.Helper()
	id := seedRunningRun(t, env, o, iid, 0)
	env.exec(`UPDATE runs SET claim_generation = 3, status = $2,
	              health = 'stalled', health_reason = 'x', health_since = now() WHERE id = $1`, id, status)
	return id
}

// approvePlanVia approves the run's plan the way a real approval shows up in GetRunClaimContext.
func approvePlanVia(t *testing.T, env codexTestEnv, id uuid.UUID, how string) {
	t.Helper()
	switch how {
	case "auto":
		env.exec(`UPDATE runs SET auto_approve = true WHERE id = $1`, id)
	case "seeded":
		env.exec(`UPDATE runs SET plan_source = 'seeded' WHERE id = $1`, id)
	case "human":
		env.exec(`INSERT INTO run_user_inputs(run_id, kind, body, applied_at) VALUES ($1, 'approve_plan', '{}', now())`, id)
	case "cross-check", "cross-check-stale":
		// A cross-check-gated run: the approval counts only when bound to the published gate revision.
		env.exec(`UPDATE runs SET plan_cross_check_required = true, plan_md = 'p', gate_revision = 1,
		              gate_presented_payload = '{"plan_md":"p","milestones":null,"required_tools":[],"size_class":null}'::jsonb
		          WHERE id = $1`, id)
		rev := 1
		if how == "cross-check-stale" {
			rev = 2 // approved an older revision than the one now published
		}
		env.exec(`INSERT INTO run_user_inputs(run_id, kind, body, applied_at, gate_binding, gate_revision)
		          VALUES ($1, 'approve_plan', '{}', now(), 'bound', $2)`, id, rev)
	default:
		t.Fatalf("unknown approval %q", how)
	}
}

func TestSetRunPhaseSwitchHoldServiceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	head := "0123456789abcdef0123456789abcdef01234567"
	iid := int64(32520)
	next := func() int64 { iid++; return iid }

	t.Run("an invalid phase or generation is a validation error", func(t *testing.T) {
		id := seedPhaseSwitchRun(t, env, o, next(), "running")
		for _, phase := range []string{"", "plans", "Implement"} {
			if _, _, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, phase); !errors.Is(err, ErrInvalidState) ||
				!errors.Is(err, ErrInvalidPhaseSwitchPhase) || errors.Is(err, ErrMissingClaimGeneration) {
				t.Fatalf("phase %q err = %v, want ErrInvalidPhaseSwitchPhase (and ErrInvalidState) only", phase, err)
			}
		}
		if _, _, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 0, PhaseSwitchPlan); !errors.Is(err, ErrInvalidState) ||
			!errors.Is(err, ErrMissingClaimGeneration) || errors.Is(err, ErrInvalidPhaseSwitchPhase) {
			t.Fatalf("generation 0 err = %v, want ErrMissingClaimGeneration (and ErrInvalidState) only", err)
		}
		if s := statusOf(t, env, id); s != "running" {
			t.Fatalf("status = %q, want running", s)
		}
	})

	t.Run("a plan hold needs no approval, from running or awaiting_approval", func(t *testing.T) {
		for _, status := range []string{"running", "awaiting_approval"} {
			id := seedPhaseSwitchRun(t, env, o, next(), status)
			run, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchPlan)
			if err != nil || !applied {
				t.Fatalf("%s plan hold = (applied=%v, %v), want (true, nil)", status, applied, err)
			}
			if run.Status != "paused" || run.HoldReason.String != HoldReasonPhaseSwitchUnavailable || run.HoldCapturedHead.String != head {
				t.Fatalf("held run = (%q, %q, %q)", run.Status, run.HoldReason.String, run.HoldCapturedHead.String)
			}
			if run.Health != "ok" || run.HealthReason.Valid || run.HealthSince.Valid || run.OpenQuestionID.Valid {
				t.Fatalf("health/question not reset: %q %v %v", run.Health, run.HealthReason, run.OpenQuestionID)
			}
			if run.WorkerID.Bytes != o.workerID || run.ClaimReleasedAt.Valid || run.ClaimGeneration != 3 {
				t.Fatal("the hold keeps the worker and the live claim")
			}
		}
	})

	t.Run("an implement hold needs an approved plan", func(t *testing.T) {
		for _, status := range []string{"running", "awaiting_approval"} {
			for _, how := range []string{"auto", "seeded", "human", "cross-check"} {
				id := seedPhaseSwitchRun(t, env, o, next(), status)
				approvePlanVia(t, env, id, how)
				run, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, nil, 3, PhaseSwitchImplement)
				if err != nil || !applied || run.Status != "paused" || run.HoldCapturedHead.Valid {
					t.Fatalf("%s/%s implement hold = (%q, head %v, applied=%v, %v), want paused with no head", status, how, run.Status, run.HoldCapturedHead, applied, err)
				}
			}
			for _, how := range []string{"", "cross-check-stale"} {
				id := seedPhaseSwitchRun(t, env, o, next(), status)
				if how != "" {
					approvePlanVia(t, env, id, how)
				}
				run, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchImplement)
				if !errors.Is(err, ErrPhaseSwitchPlanNotApproved) || applied || run.Status != status || run.HoldReason.Valid {
					t.Fatalf("%s/unapproved(%q) implement hold = (%q, hold %v, applied=%v, %v), want ErrPhaseSwitchPlanNotApproved and unchanged", status, how, run.Status, run.HoldReason, applied, err)
				}
				if s := statusOf(t, env, id); s != status {
					t.Fatalf("status = %q, want %q unchanged", s, status)
				}
			}
		}
	})

	t.Run("the claim fence refuses a stale generation, a released claim and another worker", func(t *testing.T) {
		id := seedPhaseSwitchRun(t, env, o, next(), "running")
		run, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 2, PhaseSwitchPlan)
		if err != nil || applied || run.Status != "running" {
			t.Fatalf("stale generation = (%q, applied=%v, %v), want (running, false, nil)", run.Status, applied, err)
		}
		env.exec(`UPDATE runs SET claim_released_at = now() WHERE id = $1`, id)
		if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchPlan); err != nil || applied {
			t.Fatalf("released claim = (applied=%v, %v), want (false, nil)", applied, err)
		}
		other := seedPhaseSwitchRun(t, env, o, next(), "running")
		if _, _, err := svc.SetRunPhaseSwitchHold(env.ctx, store.Worker{ID: uuid.New(), UserID: o.userID}, other, &head, 3, PhaseSwitchPlan); !errors.Is(err, ErrRunNotOwned) {
			t.Fatalf("foreign worker err = %v, want ErrRunNotOwned", err)
		}
	})

	t.Run("a codex run is admitted and a non-live phase is refused", func(t *testing.T) {
		id := seedPhaseSwitchRun(t, env, o, next(), "running")
		env.exec(`UPDATE runs SET harness = 'codex' WHERE id = $1`, id)
		if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchPlan); err != nil || !applied {
			t.Fatalf("codex run = (applied=%v, %v), want (true, nil)", applied, err)
		}
		id = seedPhaseSwitchRun(t, env, o, next(), "awaiting_input")
		if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchPlan); err != nil || applied {
			t.Fatalf("awaiting_input = (applied=%v, %v), want (false, nil)", applied, err)
		}
	})

	t.Run("a lost-ACK repeat answers from the held row without touching it", func(t *testing.T) {
		id := seedPhaseSwitchRun(t, env, o, next(), "running")
		first, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchPlan)
		if err != nil || !applied {
			t.Fatalf("hold = (applied=%v, %v)", applied, err)
		}
		time.Sleep(10 * time.Millisecond)
		again, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchPlan)
		if err != nil || !applied {
			t.Fatalf("repeat = (applied=%v, %v), want (true, nil)", applied, err)
		}
		if !again.StatusSince.Time.Equal(first.StatusSince.Time) || again.BudgetPausedSeconds != first.BudgetPausedSeconds ||
			again.BudgetExtensionSeconds != first.BudgetExtensionSeconds || !again.UpdatedAt.Time.Equal(first.UpdatedAt.Time) {
			t.Fatal("a repeat must not reset status_since, updated_at or any budget")
		}
		if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 4, PhaseSwitchPlan); err != nil || applied {
			t.Fatalf("a repeat from another generation = (applied=%v, %v), want (false, nil)", applied, err)
		}
	})
}

// TestPhaseSwitchHoldOwnerResumeAndCancelLiveDB: the owner's resume admits the hold (queued, keeping
// worker, marker and generation) and the owner's cancel still applies to a held run.
func TestPhaseSwitchHoldOwnerResumeAndCancelLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	svc := New(env.q, env.box, testParams())
	o := seedReevalOwner(t, env, BindModeAuto, false)
	wkr := store.Worker{ID: o.workerID, UserID: o.userID}
	head := "abc"

	id := seedPhaseSwitchRun(t, env, o, 32560, "running")
	if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, 3, PhaseSwitchPlan); err != nil || !applied {
		t.Fatalf("hold = (applied=%v, %v)", applied, err)
	}
	if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{ID: id, UserID: o.userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("ResumePausedRun on the phase-switch hold: %v", err)
	}
	r := mustRun(t, env, id)
	if r.Status != "queued" || r.WorkerID.Bytes != o.workerID || r.ClaimGeneration != 3 || r.ClaimReleasedAt.Valid ||
		r.HoldReason.String != HoldReasonPhaseSwitchUnavailable || r.HoldCapturedHead.String != head {
		t.Fatalf("resumed = (%q, gen %d, released %v, hold %q, head %q), want queued keeping worker, generation, marker and head",
			r.Status, r.ClaimGeneration, r.ClaimReleasedAt.Valid, r.HoldReason.String, r.HoldCapturedHead.String)
	}

	held := seedPhaseSwitchRun(t, env, o, 32561, "running")
	if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, held, &head, 3, PhaseSwitchPlan); err != nil || !applied {
		t.Fatalf("hold = (applied=%v, %v)", applied, err)
	}
	res, err := svc.SubmitInputWithOptions(env.ctx, o.userID, held, "cancel", "owner cancel", nil, SubmitInputOptions{})
	if err != nil || !res.ServerSide {
		t.Fatalf("owner cancel = (%+v, %v), want a server-side cancel", res, err)
	}
	if s := statusOf(t, env, held); s != "cancelled" {
		t.Fatalf("status after the owner's cancel = %q, want cancelled", s)
	}
}

// TestPhaseSwitchHoldResumeRaceLiveDB walks the lost-ACK race: the hold applied, the owner resumed
// (queued, marker kept) before the worker's retry or heartbeat arrived. Nothing the old flight sends
// may revive, re-park or fail the row; a fresh claim issues a new generation and fences the old one.
func TestPhaseSwitchHoldResumeRaceLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	svc := snapshotSvc(env, testParams())
	nonce := "phase-hold-" + uuid.NewString()
	wk := seedSnapshotWorker(t, env, userID, nonce)
	wkr := hbWorker(t, env, wk)
	const generation int64 = 3
	head := "0123456789abcdef0123456789abcdef01234567"

	id := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", generation, 0)
	if _, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, generation, PhaseSwitchPlan); err != nil || !applied {
		t.Fatalf("hold = (applied=%v, %v)", applied, err)
	}
	if _, err := env.q.ResumePausedRun(env.ctx, store.ResumePausedRunParams{ID: id, UserID: userID, GlobalTimeoutSeconds: 3600}); err != nil {
		t.Fatalf("ResumePausedRun: %v", err)
	}
	queued := mustRun(t, env, id)
	if queued.Status != "queued" || queued.HoldReason.String != HoldReasonPhaseSwitchUnavailable {
		t.Fatalf("resumed = (%q, %q), want queued with the marker", queued.Status, queued.HoldReason.String)
	}
	assertUnchanged := func(step string) {
		t.Helper()
		r := mustRun(t, env, id)
		if r.Status != "queued" || r.ClaimGeneration != generation || r.WorkerID != queued.WorkerID ||
			r.HoldReason != queued.HoldReason || r.HoldCapturedHead != queued.HoldCapturedHead ||
			!r.StatusSince.Time.Equal(queued.StatusSince.Time) || r.ClaimReleasedAt.Valid {
			t.Fatalf("%s changed the resumed row: status=%q gen=%d hold=%q head=%q", step, r.Status, r.ClaimGeneration, r.HoldReason.String, r.HoldCapturedHead.String)
		}
	}

	// The heartbeat snapshot still lists the run live at the old generation: it must not be readopted.
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, &ActiveSnapshot{
		SnapshotEpoch: 1, RegisterNonce: nonce,
		Active: []ActiveRunEntry{entry(id, generation, "running", false)},
	}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	assertUnchanged("a heartbeat listing the run live at the old generation")

	// The retried hold report is refused with the real status and the marker, changing nothing.
	run, applied, err := svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, generation, PhaseSwitchPlan)
	if err != nil || applied || run.Status != "queued" || run.HoldReason.String != HoldReasonPhaseSwitchUnavailable {
		t.Fatalf("retried hold = (%q, %q, applied=%v, %v), want (queued, marker, false, nil)", run.Status, run.HoldReason.String, applied, err)
	}
	assertUnchanged("the retried hold report")

	// The ownership probe reports the queued marker at the same generation.
	status, _, gen, hold, released, _, _, err := svc.RunOwnership(env.ctx, wkr, id)
	if err != nil || status != "queued" || gen != generation || released || hold == nil || *hold != HoldReasonPhaseSwitchUnavailable {
		t.Fatalf("ownership = (%q, gen %d, hold %v, released %v, %v), want queued/marker at generation %d", status, gen, hold, released, err, generation)
	}

	// Late reports of the old flight at the old generation.
	wid := pgtype.UUID{Bytes: wk, Valid: true}
	if n, err := env.q.SetRunAwaitingInput(env.ctx, store.SetRunAwaitingInputParams{OpenQuestionID: pgtype.Text{String: "q", Valid: true}, ID: id, WorkerID: wid}); err != nil || n != 0 {
		t.Fatalf("late SetRunAwaitingInput = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := env.q.SetRunAwaitingFollowup(env.ctx, store.SetRunAwaitingFollowupParams{ID: id, WorkerID: wid}); err != nil || n != 0 {
		t.Fatalf("late SetRunAwaitingFollowup = (%d, %v), want (0, nil)", n, err)
	}
	if n, err := env.q.SupersedeRunByWorker(env.ctx, store.SupersedeRunByWorkerParams{StopReason: "branch moved", ID: id, WorkerID: wid}); err != nil || n != 0 {
		t.Fatalf("late SupersedeRunByWorker = (%d, %v), want (0, nil)", n, err)
	}
	assertUnchanged("late old-flight reports")

	// The worker learned from the 409 to drop the flight: its next snapshot no longer lists the run
	// (a queued run a fresh snapshot still lists live is held back from claiming). A fresh claim then
	// issues a new generation, and the old generation is refused.
	if _, err := svc.Heartbeat(env.ctx, hbWorker(t, env, wk), nil, nil, &ActiveSnapshot{
		SnapshotEpoch: 2, RegisterNonce: nonce, Active: []ActiveRunEntry{},
	}); err != nil {
		t.Fatalf("Heartbeat (flight dropped): %v", err)
	}
	claimed, err := env.q.ClaimRun(env.ctx, claimRunParams(wkr))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	if claimed.ID != id || claimed.ClaimGeneration != generation+1 {
		t.Fatalf("claimed = (%s, gen %d), want %s at generation %d", claimed.ID, claimed.ClaimGeneration, id, generation+1)
	}
	run, applied, err = svc.SetRunPhaseSwitchHold(env.ctx, wkr, id, &head, generation, PhaseSwitchPlan)
	if err != nil || applied {
		t.Fatalf("old-generation hold after a new claim = (%q, applied=%v, %v), want refused", run.Status, applied, err)
	}
	if s := statusOf(t, env, id); s == "paused" {
		t.Fatal("an old-generation hold parked the re-claimed run")
	}
}

// TestPhaseSwitchProcessDeathSweepLiveDB: a running pre-publication run that is NOT held (the worker
// died before reporting) follows the ordinary stale-worker routes: requeued with budget left, the
// existing over-cap route when the budget is spent. Neither route is a phase hold.
func TestPhaseSwitchProcessDeathSweepLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	o := seedReevalOwner(t, env, BindModeAuto, false)
	env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '3 hours' WHERE id = $1`, o.workerID)

	budget := seedPhaseSwitchRun(t, env, o, 32570, "running")
	spent := seedPhaseSwitchRun(t, env, o, 32571, "running")
	env.exec(`UPDATE runs SET requeue_count = 5 WHERE id = $1`, spent)

	rows, err := env.q.RequeueRunsOfStaleWorkers(env.ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 5, Cutoff: pgconv.Time(time.Now().Add(-time.Hour)),
	})
	if err != nil {
		t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, row := range rows {
		got[row.ID] = true
	}
	if !got[budget] || got[spent] {
		t.Fatalf("requeued = %v, want the run with budget left and not the spent one", got)
	}
	if s := statusOf(t, env, budget); s != "queued" {
		t.Fatalf("run with budget left: status %q, want queued", s)
	}
	if _, err := env.q.FailRunsOfStaleWorkersOverCap(env.ctx, store.FailRunsOfStaleWorkersOverCapParams{
		FailureReason: pgconv.TextOrNull("worker lost; exceeded re-queue budget"),
		MaxRequeues:   5,
		FailCutoff:    pgconv.Time(time.Now().Add(-time.Hour)),
	}); err != nil {
		t.Fatalf("FailRunsOfStaleWorkersOverCap: %v", err)
	}
	r := mustRun(t, env, spent)
	if r.Status == "running" || r.Status == "paused" || r.HoldReason.String == HoldReasonPhaseSwitchUnavailable {
		t.Fatalf("spent run: status %q hold %q, want the existing over-cap route (failed or recovery_wait), not a phase hold", r.Status, r.HoldReason.String)
	}
}

func TestCodexPhaseSwitchHoldEachLeadKindLiveDB(t *testing.T) {
	f := newIsoFix(t)
	w := store.Worker{ID: f.workerID, UserID: f.userID}
	for _, kind := range phaseLeadKinds {
		t.Run(kind, func(t *testing.T) {
			id := f.insertPhaseRun(t, kind, "awaiting_approval", f.workerID)
			f.env.exec(`UPDATE runs SET harness='codex', claim_generation=3, auto_approve=true WHERE id=$1`, id)
			r, applied, err := f.svc.SetRunPhaseSwitchHold(f.env.ctx, w, id, nil, 3, PhaseSwitchImplement)
			if err != nil || !applied || r.Status != "paused" || r.HoldReason.String != HoldReasonPhaseSwitchUnavailable {
				t.Fatalf("Codex %s hold=(%s,%v,%v)", kind, r.Status, applied, err)
			}
		})
	}
}
