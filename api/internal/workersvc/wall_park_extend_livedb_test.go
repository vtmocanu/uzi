package workersvc

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file pins issue #2734: the owner's Extend on a budget_exhausted wall park must not report
// success when ExtendAndResumeWallPark matched no row, and the deadline it returns must come from the
// post-update row (the statement banks the overrun and the parked interval into
// budget_paused_seconds).

// recordingWallParkObserver records every PublishState and Notify the service fans out.
type recordingWallParkObserver struct {
	mu       sync.Mutex
	states   []string
	notifies []string
}

func (r *recordingWallParkObserver) PublishMessage(uuid.UUID, int32, string, string, string, string, []byte, time.Time) {
}
func (r *recordingWallParkObserver) PublishHealth(uuid.UUID, string, string, bool) {}
func (r *recordingWallParkObserver) PublishInput(uuid.UUID)                        {}
func (r *recordingWallParkObserver) PublishState(_ uuid.UUID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, status)
}
func (r *recordingWallParkObserver) Notify(_ uuid.UUID, status string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifies = append(r.notifies, status)
}
func (r *recordingWallParkObserver) events() (states, notifies []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.states...), append([]string(nil), r.notifies...)
}

type wallParkExtendFixture struct {
	env    codexTestEnv
	svc    *Service
	obs    *recordingWallParkObserver
	userID uuid.UUID
	worker uuid.UUID
	repoID uuid.UUID
}

func newWallParkExtendFixture(t *testing.T, capSeconds int) *wallParkExtendFixture {
	t.Helper()
	env := setupCodexLiveDB(t)
	userID, workerID, repoID := env.seedCodexInfra(t)
	svc := New(env.q, env.box, testParams())
	svc.SetHealthSettings(fakeHealthSettings{enabled: true, runExtensionCap: capSeconds})
	obs := &recordingWallParkObserver{}
	svc.SetBroadcaster(obs)
	svc.SetLifecycle(obs)
	return &wallParkExtendFixture{env: env, svc: svc, obs: obs, userID: userID, worker: workerID, repoID: repoID}
}

// wallPark inserts an issue run parked at its wall budget: budget_wall 3600, started 5h ago, parked
// 30m ago, with the unsettled wall 'pause' input the park leaves behind.
func (f *wallParkExtendFixture) wallPark(t *testing.T, extension int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
		worker_id, hold_reason, budget_wall_seconds, budget_extension_seconds, started_at, status_since, claim_generation)
		VALUES ($1,$2,$3,'issue',1,'t','d','paused',$4,'budget_exhausted',3600,$5,
		now()-interval '5 hours',now()-interval '30 minutes',1)`, id, f.userID, f.repoID, f.worker, extension)
	f.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, id)
	return id
}

// assertRefusedUntouched checks a refused extend changed nothing and told nobody: the run row is
// identical, no extend/resume audit rows exist, the wall pause input is unsettled, and neither the
// broadcaster nor the lifecycle saw a transition.
func (f *wallParkExtendFixture) assertRefusedUntouched(t *testing.T, id uuid.UUID, before store.Run) {
	t.Helper()
	after := mustRun(t, f.env, id)
	if after.Status != before.Status || after.HoldReason != before.HoldReason ||
		after.BudgetExtensionSeconds != before.BudgetExtensionSeconds ||
		after.BudgetPausedSeconds != before.BudgetPausedSeconds ||
		after.StatusSince != before.StatusSince {
		t.Fatalf("refused extend mutated the run: before=%s/%v/ext%d/paused%d after=%s/%v/ext%d/paused%d",
			before.Status, before.HoldReason, before.BudgetExtensionSeconds, before.BudgetPausedSeconds,
			after.Status, after.HoldReason, after.BudgetExtensionSeconds, after.BudgetPausedSeconds)
	}
	var audits int
	if err := f.env.pool.QueryRow(f.env.ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind IN ('extend','resume')`, id).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 0 {
		t.Fatalf("refused extend wrote %d extend/resume audit rows, want 0", audits)
	}
	var unsettled int
	if err := f.env.pool.QueryRow(f.env.ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id=$1 AND kind='pause' AND body='wall' AND applied_at IS NULL`, id).Scan(&unsettled); err != nil {
		t.Fatal(err)
	}
	if unsettled != 1 {
		t.Fatalf("unsettled wall pause inputs = %d, want 1", unsettled)
	}
	if states, notifies := f.obs.events(); len(states) != 0 || len(notifies) != 0 {
		t.Fatalf("refused extend broadcast=%v notified=%v, want none", states, notifies)
	}
}

// TestWallParkExtendPastOldCumulativeCapResumesLiveDB: the cap is a PER-REQUEST maximum (#2735).
// 50000s already extended with cap 57600 used to refuse an extend of 10000 (the cumulative rule); it
// now applies, resumes the run, and returns the new total.
func TestWallParkExtendPastOldCumulativeCapResumesLiveDB(t *testing.T) {
	f := newWallParkExtendFixture(t, 57600)
	id := f.wallPark(t, 50000)

	res, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "10000", nil)
	if err != nil {
		t.Fatalf("SubmitInput extend past the old cumulative cap: %v", err)
	}
	if !res.Resumed || res.ExtensionSeconds == nil || *res.ExtensionSeconds != 60000 {
		t.Fatalf("result = %+v (ext %v), want Resumed with a 60000s total", res, res.ExtensionSeconds)
	}
	after := mustRun(t, f.env, id)
	if after.Status != "queued" || after.HoldReason.Valid || after.BudgetExtensionSeconds != 60000 {
		t.Fatalf("run after extend = %s hold=%v ext=%d, want queued / no hold / 60000", after.Status, after.HoldReason, after.BudgetExtensionSeconds)
	}
	var audit string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT body FROM run_user_inputs WHERE run_id=$1 AND kind='extend'`, id).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if audit != "extend and resume +2h46m (max 16h per extension)" {
		t.Fatalf("extend audit body = %q", audit)
	}
}

// TestWallParkExtendOverCapRequestRefusedLiveDB: a request above the per-request cap is refused as
// ErrExtensionCapExceeded naming the maximum, and the run stays parked and untouched.
func TestWallParkExtendOverCapRequestRefusedLiveDB(t *testing.T) {
	f := newWallParkExtendFixture(t, 57600)
	id := f.wallPark(t, 0)
	before := mustRun(t, f.env, id)

	res, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "57660", nil)
	if !errors.Is(err, ErrExtensionCapExceeded) || !strings.Contains(err.Error(), "+16h1m is more than the 16h maximum per extension") {
		t.Fatalf("err = %v (res %+v), want ErrExtensionCapExceeded naming the per-extension maximum", err, res)
	}
	if res.Resumed {
		t.Fatal("a refused extend reported Resumed:true")
	}
	f.assertRefusedUntouched(t, id, before)
}

// TestWallParkExtendOverRoomRefusedLiveDB: a request within the cap but beyond the room under the
// technical total-time bound is ErrExtensionLimitReached, and the run is still parked. The same
// refusal comes from the statement (not only the pre-check) when the caller's snapshot is stale.
func TestWallParkExtendOverRoomRefusedLiveDB(t *testing.T) {
	// testParams: RunTimeout 2h, RunWallCeiling 8h -> wall upper 259200 (72h maximum ceiling), room = 2147483 - 259200 - 1800 - ext.
	const used = 2147483 - 259200 - 1800 - 3600 // 3600s of room left
	f := newWallParkExtendFixture(t, 604800)
	id := f.wallPark(t, used)
	before := mustRun(t, f.env, id)

	res, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "3660", nil)
	if !errors.Is(err, ErrExtensionLimitReached) {
		t.Fatalf("err = %v (res %+v), want ErrExtensionLimitReached", err, res)
	}
	if res.Resumed {
		t.Fatal("a refused extend reported Resumed:true")
	}
	f.assertRefusedUntouched(t, id, before)

	// Stale snapshot: the caller read the run with room to spare, then another extend consumed it;
	// the statement refuses and the re-read names the limit.
	stale := before
	stale.BudgetExtensionSeconds = 0
	res, err = f.svc.extendAndResumeWallPark(f.env.ctx, f.userID, stale, 3660, 604800)
	if !errors.Is(err, ErrExtensionLimitReached) || res.Resumed {
		t.Fatalf("stale-snapshot err = %v (res %+v), want ErrExtensionLimitReached", err, res)
	}
	f.assertRefusedUntouched(t, id, before)

	// Exactly the room applies.
	res, err = f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "3600", nil)
	if err != nil || !res.Resumed {
		t.Fatalf("exact-room extend = (%+v, %v), want applied and resumed", res, err)
	}
}

// TestWallParkExtendStateChangedLiveDB: the caller's snapshot says "budget_exhausted wall park" but
// the row has since been owner-paused (no hold) or already resumed; the statement matches no row and
// the service names the change instead of claiming a resume.
func TestWallParkExtendStateChangedLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
	}{
		{"owner-paused (hold cleared)", `UPDATE runs SET hold_reason=NULL WHERE id=$1`},
		{"already resumed (queued)", `UPDATE runs SET status='queued', hold_reason=NULL WHERE id=$1`},
		// Out of room AND no longer a budget_exhausted park: the state-change refusal must win over
		// the limit wording, since the room arithmetic on a run that is not parked is meaningless.
		{"owner-paused and out of room", `UPDATE runs SET hold_reason=NULL, budget_extension_seconds=2116000 WHERE id=$1`},
		{"queued and out of room", `UPDATE runs SET status='queued', hold_reason=NULL, budget_extension_seconds=2116000 WHERE id=$1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWallParkExtendFixture(t, 57600)
			id := f.wallPark(t, 0)
			stale := mustRun(t, f.env, id) // paused / budget_exhausted: what the caller read
			f.env.exec(tc.setup, id)
			before := mustRun(t, f.env, id)

			res, err := f.svc.extendAndResumeWallPark(f.env.ctx, f.userID, stale, 3600, 57600)
			if !errors.Is(err, ErrWallParkChanged) || errors.Is(err, ErrExtensionCapExceeded) || errors.Is(err, ErrExtensionLimitReached) {
				t.Fatalf("extendAndResumeWallPark err = %v (res %+v), want ErrWallParkChanged", err, res)
			}
			if res.Resumed {
				t.Fatal("a refused extend reported Resumed:true")
			}
			// The wall pause input stays unsettled in both cases; only the run row differs.
			f.assertRefusedUntouched(t, id, before)
		})
	}
}

// TestWallParkExtendCrossCheckIneligibleLiveDB: a wall-parked plan cross-check whose cross_checks row
// has already expired cannot resume (the statement's eligibility guard), so the extend is refused
// with the cross-check sentinel.
func TestWallParkExtendCrossCheckIneligibleLiveDB(t *testing.T) {
	for _, ext := range []int{0, 60000} { // 60000 is above the old cumulative cap (57600)
		t.Run(fmt.Sprintf("extension_%d", ext), func(t *testing.T) { crossCheckIneligible(t, ext) })
	}
}

func crossCheckIneligible(t *testing.T, ext int) {
	f := newWallParkExtendFixture(t, 57600)
	leadID := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, worker_id, kind, issue_iid, issue_title, issue_description,
		status, harness, plan_cross_check_required, claim_generation)
		VALUES ($1,$2,$3,$4,'issue',1,'t','d','running','claude',true,1)`, leadID, f.userID, f.repoID, f.worker)
	checkerID := uuid.New()
	f.env.exec(`INSERT INTO runs (id, user_id, repo_id, worker_id, kind, target_run_id, harness, report_only,
		budget_wall_seconds, budget_extension_seconds, status, hold_reason, claim_generation, issue_title, issue_description,
		started_at, status_since)
		VALUES ($1,$2,$3,$4,'cross_check',$5,'codex',true,1800,$6,'paused','budget_exhausted',1,'check','check',
		now()-interval '5 hours',now()-interval '30 minutes')`, checkerID, f.userID, f.repoID, f.worker, leadID, ext)
	f.env.exec(`INSERT INTO cross_checks (lead_run_id,stage,round,lead_claim_generation,plan_md,milestones,
		required_capabilities,required_tools,size_class,base_commit,planning_diff,candidate_digest,
		checker_run_id,checker_harness,created_at,deadline_at)
		VALUES ($1,'plan',1,1,'plan','[]','{}','{}','s',$2,'',$3,$4,'codex',
		now()-interval '2 hours',now()-interval '1 hour')`, leadID, strings.Repeat("a", 40), []byte("digest"), checkerID)
	f.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1,'pause','wall')`, checkerID)
	before := mustRun(t, f.env, checkerID)

	res, err := f.svc.SubmitInput(f.env.ctx, f.userID, checkerID, "extend", "3600", nil)
	if !errors.Is(err, ErrWallParkCrossCheckIneligible) {
		t.Fatalf("SubmitInput extend err = %v (res %+v), want ErrWallParkCrossCheckIneligible", err, res)
	}
	if res.Resumed {
		t.Fatal("a refused extend reported Resumed:true")
	}
	f.assertRefusedUntouched(t, checkerID, before)
}

// TestWallParkExtendDeadlineUsesPostUpdateRowLiveDB: wall 3600, started 5h ago, parked 30m ago, extend
// 3600. The statement banks the overrun and the parked interval into budget_paused_seconds, so the
// resumed deadline is ~3600s away. Computed from the pre-update snapshot it landed in the past.
func TestWallParkExtendDeadlineUsesPostUpdateRowLiveDB(t *testing.T) {
	f := newWallParkExtendFixture(t, 57600)
	id := f.wallPark(t, 0)

	now := time.Now()
	res, err := f.svc.SubmitInput(f.env.ctx, f.userID, id, "extend", "3600", nil)
	if err != nil {
		t.Fatalf("SubmitInput extend: %v", err)
	}
	if !res.Resumed || res.ExtensionSeconds == nil || *res.ExtensionSeconds != 3600 || res.DeadlineAt == nil {
		t.Fatalf("result = %+v, want Resumed with ExtensionSeconds 3600 and a DeadlineAt", res)
	}
	got := mustRun(t, f.env, id)
	if got.Status != "queued" {
		t.Fatalf("status = %s, want queued", got.Status)
	}
	want := RunDeadline(got.StartedAt, got.BudgetWallSeconds, got.BudgetPausedSeconds, got.Kind, got.Interactive,
		"running", f.svc.p.RunTimeout, got.BudgetExtensionSeconds, got.BudgetFinalizeSeconds)
	if want == nil || !res.DeadlineAt.Equal(*want) {
		t.Fatalf("DeadlineAt = %v, want RunDeadline over the post-update row = %v", res.DeadlineAt, want)
	}
	if d := res.DeadlineAt.Sub(now).Seconds(); d < 3595 || d > 3605 {
		t.Fatalf("DeadlineAt is %.0fs from now, want ~3600 (the overrun and parked wait must be banked)", d)
	}
	if states, notifies := f.obs.events(); len(states) != 1 || states[0] != "queued" || len(notifies) != 1 || notifies[0] != "queued" {
		t.Fatalf("success broadcast=%v notified=%v, want one queued each", states, notifies)
	}
}
