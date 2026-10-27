package workersvc

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// usage_reserve_pause_livedb_test.go pins the weekly usage reserve's running-run side (PRD #3522 M4a)
// against a real Postgres: the sweeper's reservePausePass (request, withdraw, release, exemption
// expiry), the park, the owner's levers and the claim-time floor. Skipped unless
// UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

// pauseRun is one non-queued run's shape for the pause pass.
type pauseRun struct {
	status       string // "" = running
	kind         string // "" = issue
	cred         *uuid.UUID
	completed    int
	pauseMode    string
	hold         string
	notStarted   bool
	interactive  bool
	worker       *uuid.UUID // nil = the fixture worker
	statusSince  string     // SQL interval ago, "" = now
	updatedAgo   string     // SQL interval ago for updated_at, "" = now
	budgetPaused int
}

func (fx *reserveFix) pauseRun(o pauseRun) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	status, kind := o.status, o.kind
	if status == "" {
		status = "running"
	}
	if kind == "" {
		kind = "issue"
	}
	iid := 5000 + int64(id[0])*256 + int64(id[1])
	worker := fx.workerID
	if o.worker != nil {
		worker = *o.worker
	}
	fx.env.exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
	                 worker_id, started_at, status_since, anthropic_secret_id, interactive, budget_paused_seconds,
	                 milestones_completed, pipeline_id, pipeline_ref)
	             VALUES ($1, $2, $3, $4, $5, 't', 'd', $6, $7, now() - interval '3 hours', now() - $8::interval,
	                 $9, $10, $11, (SELECT COALESCE(jsonb_agg(g), '[]'::jsonb) FROM generate_series(1, $12::int) g),
	                 CASE WHEN $4 = 'ci_fix' THEN 1 END, CASE WHEN $4 = 'ci_fix' THEN 'main' END)`,
		id, fx.userID, fx.repoID, kind, iid, status, worker, orNow(o.statusSince), o.cred, o.interactive, o.budgetPaused, o.completed)
	if !o.notStarted {
		fx.env.exec(`UPDATE runs SET first_started_at = now() - interval '3 hours' WHERE id = $1`, id)
	}
	if o.pauseMode != "" {
		fx.env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = $2, pause_after_count = $3 WHERE id = $1`,
			id, o.pauseMode, o.completed)
	}
	if o.hold != "" {
		fx.env.exec(`UPDATE runs SET hold_reason = $2 WHERE id = $1`, id, o.hold)
	}
	if o.updatedAgo != "" {
		fx.env.exec(`UPDATE runs SET updated_at = now() - $2::interval WHERE id = $1`, id, o.updatedAgo)
	}
	return id
}

func orNow(s string) string {
	if s == "" {
		return "0 seconds"
	}
	return s
}

func (fx *reserveFix) pass() int64 {
	fx.t.Helper()
	n, err := fx.svc.reservePausePass(fx.env.ctx, time.Now())
	if err != nil {
		fx.t.Fatalf("reservePausePass: %v", err)
	}
	return n
}

// pausePending is the run's pending pause: (mode, after, requested).
func (fx *reserveFix) pausePending(id uuid.UUID) (string, int32, bool) {
	fx.t.Helper()
	r := mustRun(fx.t, fx.env, id)
	return r.PauseMode.String, r.PauseAfterCount.Int32, r.PauseRequestedAt.Valid
}

func (fx *reserveFix) inputBodies(id uuid.UUID, kind string) []string {
	fx.t.Helper()
	rows, err := fx.env.pool.Query(fx.env.ctx, `SELECT COALESCE(body, '') FROM run_user_inputs WHERE run_id = $1 AND kind = $2 ORDER BY id`, id, kind)
	if err != nil {
		fx.t.Fatalf("query inputs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			fx.t.Fatal(err)
		}
		out = append(out, b)
	}
	return out
}

func (fx *reserveFix) unappliedPauseInputs(id uuid.UUID) int {
	fx.t.Helper()
	var n int
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = 'pause' AND applied_at IS NULL`, id).Scan(&n); err != nil {
		fx.t.Fatal(err)
	}
	return n
}

// TestReservePauseRequestFilesMilestoneRequestLiveDB: a running run whose recorded credential reads
// Held on a fresh reading gets the system 'reserve' request (and its 'milestone' steering input) with
// pause_after_count at the completed-milestone count, once; every run the pass must not pause is left
// alone (unknown readings, a missing credential, an OK reading, toggle off, a kind outside the pause allowlist).
func TestReservePauseRequestFilesMilestoneRequestLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	hi := fx.token("hi", true, false, 90)
	ok := fx.token("ok", false, false, 10)
	stale := fx.token("stale", false, false, -1)
	fx.reading(stale, 95, 10, "3 hours")
	noReading := fx.token("none", false, false, -1)

	held := fx.pauseRun(pauseRun{cred: &hi, completed: 2})
	unk1 := fx.pauseRun(pauseRun{cred: &stale})
	unk2 := fx.pauseRun(pauseRun{cred: &noReading})
	okRun := fx.pauseRun(pauseRun{cred: &ok})
	noCred := fx.pauseRun(pauseRun{})
	inter := fx.pauseRun(pauseRun{cred: &hi, kind: "ci_fix"})

	if n := fx.pass(); n != 1 {
		t.Fatalf("first pass changed %d runs, want exactly the held one", n)
	}
	mode, after, req := fx.pausePending(held)
	if !req || mode != "reserve" || after != 2 {
		t.Fatalf("held run pause = (%q, after %d, requested %v), want (reserve, 2, true)", mode, after, req)
	}
	if got := fx.inputBodies(held, "pause"); len(got) != 1 || got[0] != "milestone" {
		t.Fatalf("pause inputs = %v, want one 'milestone' steering input", got)
	}
	for name, id := range map[string]uuid.UUID{"stale reading": unk1, "no reading": unk2, "ok reading": okRun, "no recorded credential": noCred, "ci_fix run (no milestone pause contract)": inter} {
		if _, _, req := fx.pausePending(id); req {
			t.Fatalf("%s: a pause was requested", name)
		}
	}
	if n := fx.pass(); n != 0 {
		t.Fatalf("second pass changed %d runs, want 0 (idempotent)", n)
	}
	if got := fx.inputBodies(held, "pause"); len(got) != 1 {
		t.Fatalf("a repeated pass wrote another pause input: %v", got)
	}

	// The pause toggle off: nothing is requested for a started run.
	fx.setReserve(70, false)
	again := fx.pauseRun(pauseRun{cred: &hi})
	fx.pass()
	if _, _, req := fx.pausePending(again); req {
		t.Fatal("a pause was requested with the pause toggle off")
	}
	// The statement repeats the owner guards: toggle off or no threshold requests nothing.
	if rows, err := fx.env.q.RequestReservePauses(fx.env.ctx, []uuid.UUID{again}); err != nil || len(rows) != 0 {
		t.Fatalf("RequestReservePauses with the toggle off = (%d rows, %v), want 0", len(rows), err)
	}
	fx.setReserve(0, true)
	if rows, err := fx.env.q.RequestReservePauses(fx.env.ctx, []uuid.UUID{again}); err != nil || len(rows) != 0 {
		t.Fatalf("RequestReservePauses with no threshold = (%d rows, %v), want 0", len(rows), err)
	}
}

// TestReservePauseNeverOverwritesOwnerOrWallLiveDB: a pending owner or wall request stands; an owner
// or wall request overwrites a pending reserve request.
func TestReservePauseNeverOverwritesOwnerOrWallLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	hi := fx.token("hi", true, false, 90)
	owner := fx.pauseRun(pauseRun{cred: &hi, pauseMode: "milestone", completed: 1})
	wall := fx.pauseRun(pauseRun{cred: &hi, pauseMode: "wall"})
	if n := fx.pass(); n != 0 {
		t.Fatalf("pass changed %d runs, want 0 (existing requests stand)", n)
	}
	if m, _, _ := fx.pausePending(owner); m != "milestone" {
		t.Fatalf("owner request overwritten: %q", m)
	}
	if m, _, _ := fx.pausePending(wall); m != "wall" {
		t.Fatalf("wall request overwritten: %q", m)
	}

	// The statement itself repeats the guard (a request that landed after the list is never overwritten).
	if rows, err := fx.env.q.RequestReservePauses(fx.env.ctx, []uuid.UUID{owner, wall}); err != nil || len(rows) != 0 {
		t.Fatalf("RequestReservePauses over owner/wall requests = (%d rows, %v), want 0", len(rows), err)
	}
	if m, _, _ := fx.pausePending(owner); m != "milestone" {
		t.Fatalf("owner request overwritten by the statement: %q", m)
	}

	// A reserve request is overwritten by the owner's 'now' ...
	res := fx.pauseRun(pauseRun{cred: &hi, pauseMode: "reserve"})
	if _, err := fx.env.q.CreatePauseInput(fx.env.ctx, store.CreatePauseInputParams{ID: res, Mode: pgconv.Text("now")}); err != nil {
		t.Fatalf("CreatePauseInput over a reserve request: %v", err)
	}
	if m, _, _ := fx.pausePending(res); m != "now" {
		t.Fatalf("owner request over reserve = %q, want now", m)
	}
	// ... and by the wall sweep.
	fx.env.exec(`UPDATE workers SET protocol_capabilities = protocol_capabilities || $2::text[] WHERE id = $1`,
		fx.workerID, []string{capability.WallParkV1})
	wres := fx.pauseRun(pauseRun{cred: &hi, pauseMode: "reserve"})
	fx.env.exec(`UPDATE runs SET started_at = now() - interval '30 hours' WHERE id = $1`, wres)
	if _, err := fx.env.q.RequestWallParks(fx.env.ctx, store.RequestWallParksParams{
		Now: pgconv.Time(time.Now()), GlobalTimeoutSeconds: 7200, WorkerStaleCutoff: pgconv.Time(time.Now().Add(-time.Minute)),
	}); err != nil {
		t.Fatalf("RequestWallParks: %v", err)
	}
	if m, _, _ := fx.pausePending(wres); m != "wall" {
		t.Fatalf("wall request over reserve = %q, want wall", m)
	}
}

// TestReserveParkCarriesWeeklyReserveHoldLiveDB: the worker's park report settles a reserve request
// into paused + hold_reason weekly_reserve (owner pause stays hold-less); a failed checkpoint
// (ClearPauseRequest) leaves the run running and the next pass re-requests it; the wall budget is
// preserved across the park and the release; a credential_disabled hold settles into the reserve cause.
func TestReserveParkCarriesWeeklyReserveHoldLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	hi := fx.token("hi", true, false, 90)

	run := fx.pauseRun(pauseRun{cred: &hi, pauseMode: "reserve", completed: 1})
	if n, err := fx.env.q.SetRunPaused(fx.env.ctx, store.SetRunPausedParams{ID: run, WorkerID: pgconv.UUID(fx.workerID)}); err != nil || n != 1 {
		t.Fatalf("SetRunPaused = (%d, %v), want the reserve request to park", n, err)
	}
	r := mustRun(t, fx.env, run)
	if r.Status != "paused" || r.HoldReason.String != "weekly_reserve" || r.PauseMode.Valid || r.PauseRequestedAt.Valid {
		t.Fatalf("parked run = status %s hold %q mode %v requested %v", r.Status, r.HoldReason.String, r.PauseMode, r.PauseRequestedAt)
	}
	own := fx.pauseRun(pauseRun{cred: &hi, pauseMode: "milestone"})
	if n, err := fx.env.q.SetRunPaused(fx.env.ctx, store.SetRunPausedParams{ID: own, WorkerID: pgconv.UUID(fx.workerID)}); err != nil || n != 1 {
		t.Fatalf("owner SetRunPaused = (%d, %v)", n, err)
	}
	if r := mustRun(t, fx.env, own); r.Status != "paused" || r.HoldReason.Valid {
		t.Fatalf("owner park: status %s hold %v, want paused with no hold", r.Status, r.HoldReason)
	}

	// Checkpoint failure: the request clears, the run keeps running, the next pass re-requests.
	failed := fx.pauseRun(pauseRun{cred: &hi, pauseMode: "reserve", completed: 1})
	if _, err := fx.env.q.ClearPauseRequest(fx.env.ctx, store.ClearPauseRequestParams{ID: failed, WorkerID: pgconv.UUID(fx.workerID)}); err != nil {
		t.Fatalf("ClearPauseRequest: %v", err)
	}
	if r := mustRun(t, fx.env, failed); r.Status != "running" || r.PauseMode.Valid {
		t.Fatalf("after a failed checkpoint: status %s mode %v, want running with the request cleared", r.Status, r.PauseMode)
	}
	fx.pass()
	if m, _, req := fx.pausePending(failed); !req || m != "reserve" {
		t.Fatalf("the reserve did not re-request after the checkpoint failure: (%q, %v)", m, req)
	}

	// Wall budget: parked 100s ago, released on a fresh OK reading; started_at kept, the gap banked once.
	fx.reading(hi, 10, 10, "1 minute")
	parked := fx.pauseRun(pauseRun{cred: &hi, status: "paused", hold: "weekly_reserve", statusSince: "100 seconds", budgetPaused: 40})
	before := mustRun(t, fx.env, parked)
	if n := fx.pass(); n < 1 {
		t.Fatalf("the pass released nothing")
	}
	after := mustRun(t, fx.env, parked)
	if after.Status != "queued" || after.HoldReason.Valid {
		t.Fatalf("released run: status %s hold %v", after.Status, after.HoldReason)
	}
	if !after.StartedAt.Time.Equal(before.StartedAt.Time) {
		t.Fatalf("started_at moved %v -> %v: the wall budget must be preserved, not reset", before.StartedAt.Time, after.StartedAt.Time)
	}
	if gained := after.BudgetPausedSeconds - before.BudgetPausedSeconds; gained < 100 || gained > 130 {
		t.Fatalf("banked %ds of the parked gap, want ~100", gained)
	}
	if bodies := fx.inputBodies(parked, "resume"); len(bodies) != 1 || bodies[0] != reserveReleaseAuditBody {
		t.Fatalf("resume audit rows = %v", bodies)
	}

	// credential_disabled hold with a pending reserve request settles into the reserve cause.
	disabled := fx.pauseRun(pauseRun{cred: &hi, status: "paused", hold: "credential_disabled", pauseMode: "reserve"})
	fx.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'pause', 'milestone')`, disabled)
	if _, err := fx.env.q.SettleCredentialDisabledPause(fx.env.ctx, store.SettleCredentialDisabledPauseParams{
		ID: disabled, UserID: fx.userID, ExpectedWorkerID: pgconv.UUID(fx.workerID)}); err != nil {
		t.Fatalf("SettleCredentialDisabledPause: %v", err)
	}
	if r := mustRun(t, fx.env, disabled); r.Status != "paused" || r.HoldReason.String != "weekly_reserve" || r.PauseMode.Valid {
		t.Fatalf("settled credential-disabled reserve pause = status %s hold %q mode %v, want paused/weekly_reserve", r.Status, r.HoldReason.String, r.PauseMode)
	}
	if fx.unappliedPauseInputs(disabled) != 0 {
		t.Fatal("the settle left an unapplied pause input")
	}
}

// TestReserveAutoReleaseOnlyOnFreshOKLiveDB: a weekly_reserve pause is released only by a fresh OK
// reading (or a cleared threshold / toggle off); an owner pause, a wall park and a credential hold are
// never released by the pass; a hold that changed between the list and the release is not released.
func TestReserveAutoReleaseOnlyOnFreshOKLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	reserve := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "weekly_reserve"})
	ownerPause := fx.pauseRun(pauseRun{cred: &tok, status: "paused"})
	wallPark := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "budget_exhausted"})
	credHold := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "credential_disabled"})
	status := func(id uuid.UUID) string { return mustRun(t, fx.env, id).Status }

	// Held, unknown (stale) and missing readings keep it parked.
	for name, set := range map[string]func(){
		"held":    func() { fx.reading(tok, 90, 10, "1 minute") },
		"stale":   func() { fx.reading(tok, 5, 10, "3 hours") },
		"missing": func() { fx.env.exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id = $1`, tok) },
	} {
		set()
		fx.pass()
		if status(reserve) != "paused" {
			t.Fatalf("%s reading released the reserve pause", name)
		}
	}

	// A fresh OK reading releases exactly the reserve pause.
	fx.reading(tok, 10, 10, "1 minute")
	fx.pass()
	if status(reserve) != "queued" {
		t.Fatalf("a fresh OK reading did not release the reserve pause: %s", status(reserve))
	}
	if r := mustRun(t, fx.env, reserve); r.HoldReason.Valid {
		t.Fatalf("the released run kept hold_reason %q", r.HoldReason.String)
	}
	for name, id := range map[string]uuid.UUID{"owner pause": ownerPause, "wall park": wallPark, "credential hold": credHold} {
		if status(id) != "paused" {
			t.Fatalf("%s was released by the reserve pass", name)
		}
	}
	if r := mustRun(t, fx.env, wallPark); r.HoldReason.String != "budget_exhausted" {
		t.Fatalf("wall park hold changed to %q", r.HoldReason.String)
	}

	// The hold guard at the statement: a hold the owner changed after the list is not released.
	changed := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "weekly_reserve"})
	fx.env.exec(`UPDATE runs SET hold_reason = NULL WHERE id = $1`, changed)
	if _, err := fx.env.q.ResumePausedRun(fx.env.ctx, store.ResumePausedRunParams{
		ID: changed, UserID: fx.userID, GlobalTimeoutSeconds: 7200, RequiredHoldReason: pgconv.Text(holdWeeklyReserve),
	}); err == nil {
		t.Fatal("ResumePausedRun released a run whose hold is no longer weekly_reserve")
	}
	if status(changed) != "paused" {
		t.Fatal("the guarded resume moved the run")
	}
}

// TestReserveReleaseWithNoLaneNeedsFreshOKLiveDB: an owner whose last worker is gone (ephemeral off)
// has no lane that could claim a resumed run, so the lane loop proves nothing: a weekly_reserve pause
// stays while the reading is held or unknown and is released only by a fresh OK reading.
func TestReserveReleaseWithNoLaneNeedsFreshOKLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 90)
	fx.svc.ephemeralSettings = fakeEphemeralSettings{on: false} // no burst lane either
	run := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "weekly_reserve"})
	fx.env.exec(`UPDATE runs SET worker_id = NULL WHERE id = $1`, run)
	fx.env.exec(`DELETE FROM workers WHERE user_id = $1`, fx.userID)
	for name, set := range map[string]func(){
		"held":    func() { fx.reading(tok, 90, 10, "1 minute") },
		"stale":   func() { fx.reading(tok, 5, 10, "3 hours") },
		"missing": func() { fx.env.exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id = $1`, tok) },
	} {
		set()
		fx.pass()
		if mustRun(t, fx.env, run).Status != "paused" {
			t.Fatalf("%s reading with no lane released the reserve pause", name)
		}
	}
	fx.reading(tok, 10, 10, "1 minute")
	fx.pass()
	if mustRun(t, fx.env, run).Status != "queued" {
		t.Fatal("a fresh OK reading with no lane did not release the reserve pause")
	}
}

// TestReserveClearingThresholdOrToggleReleasesRegardlessOfFreshnessLiveDB: during a poller outage
// (stale reading), clearing the threshold releases the parks and withdraws pending requests; so does
// the toggle; the released run is then claimed above the threshold when the toggle is off.
func TestReserveClearingThresholdOrToggleReleasesRegardlessOfFreshnessLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 95)
	fx.reading(tok, 95, 10, "3 hours") // the poller is down: unknown
	fx.bindWorker(BindModeDefault, nil)
	parked := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "weekly_reserve"})
	pending := fx.pauseRun(pauseRun{cred: &tok, pauseMode: "reserve"})
	ownerPending := fx.pauseRun(pauseRun{cred: &tok, pauseMode: "milestone"})

	// Stale: nothing moves.
	if n := fx.pass(); n != 0 {
		t.Fatalf("a stale reading moved %d runs", n)
	}

	// Toggle off: release + withdraw (the run keeps its owner request), and the claim succeeds.
	fx.setReserve(70, false)
	fx.pass()
	if r := mustRun(t, fx.env, parked); r.Status != "queued" || r.HoldReason.Valid {
		t.Fatalf("toggle off: parked run = %s/%v, want released", r.Status, r.HoldReason)
	}
	if _, _, req := fx.pausePending(pending); req {
		t.Fatal("toggle off: the pending reserve request survived")
	}
	if m, _, req := fx.pausePending(ownerPending); !req || m != "milestone" {
		t.Fatalf("toggle off: the owner request was touched: (%q, %v)", m, req)
	}
	fx.env.exec(`UPDATE runs SET first_started_at = now() - interval '3 hours', worker_id = NULL WHERE id = $1`, parked)
	if p := fx.claim(); p == nil {
		t.Fatal("a released run above the threshold was not claimed with the toggle off")
	}

	// Threshold cleared: a second parked run is released the same way, with the reading still stale.
	fx.setReserve(70, true)
	again := fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "weekly_reserve"})
	fx.setReserve(0, true)
	fx.pass()
	if r := mustRun(t, fx.env, again); r.Status != "queued" {
		t.Fatalf("threshold cleared: status %s, want released", r.Status)
	}
}

// TestReserveReleaseIsAffinityAwareLiveDB: a parked run whose resume affinity pins it to a worker on a
// blocked lane is NOT released just because another worker's lane is open; pinned to the open lane it
// is. The run is evaluated as it will be queued (updated_at refreshed by the resume), so a long park
// does not read as "past the affinity ceiling".
func TestReserveReleaseIsAffinityAwareLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	hi := fx.token("hi", true, false, 90)
	lo := fx.token("lo", false, false, 5)
	fx.bindWorker(BindModeDefault, nil) // worker A: the blocked default
	b := fx.extraWorker(BindModePinned, &lo)

	run := fx.pauseRun(pauseRun{cred: &hi, status: "paused", hold: "weekly_reserve", updatedAgo: "30 days"})
	if n := fx.pass(); n != 0 {
		t.Fatalf("the pass changed %d runs, want 0: the run is pinned to the blocked worker A", n)
	}
	if mustRun(t, fx.env, run).Status != "paused" {
		t.Fatal("a run pinned to a blocked worker was released through another worker's open lane")
	}
	fx.env.exec(`UPDATE runs SET worker_id = $2 WHERE id = $1`, run, b)
	fx.pass()
	if mustRun(t, fx.env, run).Status != "queued" {
		t.Fatal("a run pinned to the open worker B was not released")
	}
}

// TestReserveWithdrawSettlesIdleRunsLiveDB: a pending reserve request on a queued, claimed or
// limit_wait run is withdrawn when the toggle goes off, its unapplied pause inputs settle, a claimed run
// also gets a pause_cancel input, and an owner request on the same states is untouched.
func TestReserveWithdrawSettlesIdleRunsLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 90)
	mk := func(status, mode string) uuid.UUID {
		id := fx.pauseRun(pauseRun{cred: &tok, status: status, pauseMode: mode})
		fx.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'pause', 'milestone')`, id)
		return id
	}
	queued, claimed, limit := mk("queued", "reserve"), mk("claimed", "reserve"), mk("limit_wait", "reserve")
	ownerQueued := mk("queued", "milestone")

	fx.setReserve(70, false)
	fx.pass()
	for name, id := range map[string]uuid.UUID{"queued": queued, "claimed": claimed, "limit_wait": limit} {
		if _, _, req := fx.pausePending(id); req {
			t.Fatalf("%s: the reserve request survived", name)
		}
		if fx.unappliedPauseInputs(id) != 0 {
			t.Fatalf("%s: an unapplied pause input survived the withdrawal", name)
		}
	}
	if got := fx.inputBodies(claimed, "pause_cancel"); len(got) != 1 {
		t.Fatalf("claimed run pause_cancel inputs = %v, want one", got)
	}
	if got := fx.inputBodies(queued, "pause_cancel"); len(got) != 0 {
		t.Fatalf("queued run got a pause_cancel input: %v", got)
	}
	if m, _, req := fx.pausePending(ownerQueued); !req || m != "milestone" || fx.unappliedPauseInputs(ownerQueued) != 1 {
		t.Fatal("an owner request on a queued run was touched")
	}
}

// TestReserveWithdrawRunningOnFreshOKLiveDB: a pending reserve request on a RUNNING run is withdrawn
// (CancelPauseInput's shape plus a pause_cancel input) when the credential reads fresh OK, and kept
// while the reading is unknown.
func TestReserveWithdrawRunningOnFreshOKLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 90)
	run := fx.pauseRun(pauseRun{cred: &tok, pauseMode: "reserve", completed: 1})
	fx.reading(tok, 5, 10, "3 hours") // unknown: kept
	fx.pass()
	if _, _, req := fx.pausePending(run); !req {
		t.Fatal("an unknown reading withdrew the request")
	}
	fx.reading(tok, 5, 10, "1 minute")
	fx.pass()
	if _, _, req := fx.pausePending(run); req {
		t.Fatal("a fresh OK reading did not withdraw the request")
	}
	if got := fx.inputBodies(run, "pause_cancel"); len(got) != 1 {
		t.Fatalf("pause_cancel inputs = %v, want one", got)
	}
}

// TestReserveOwnerLeversOnPendingRequestLiveDB: the owner cannot cancel the reserve's request (409
// sentinel, request untouched) and "pause now" converts it into an owner pause.
func TestReserveOwnerLeversOnPendingRequestLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 90)
	run := fx.pauseRun(pauseRun{cred: &tok, pauseMode: "reserve", completed: 1})

	if _, err := fx.svc.SubmitInput(fx.env.ctx, fx.userID, run, "pause_cancel", "", nil); !errors.Is(err, ErrReservePausePending) {
		t.Fatalf("pause_cancel on a reserve request = %v, want ErrReservePausePending", err)
	}
	if m, _, req := fx.pausePending(run); !req || m != "reserve" {
		t.Fatalf("the refused cancel changed the request: (%q, %v)", m, req)
	}
	// The statement itself refuses too (a request that landed after the service read).
	if _, err := fx.env.q.CancelPauseInput(fx.env.ctx, run); err == nil {
		t.Fatal("CancelPauseInput withdrew a reserve request")
	}
	if _, err := fx.svc.SubmitInput(fx.env.ctx, fx.userID, run, "pause", "reserve", nil); !errors.Is(err, ErrInvalidPauseMode) {
		t.Fatalf("an owner-supplied 'reserve' pause = %v, want ErrInvalidPauseMode", err)
	}
	if _, err := fx.svc.SubmitInput(fx.env.ctx, fx.userID, run, "pause", "now", nil); err != nil {
		t.Fatalf("pause now: %v", err)
	}
	if m, _, req := fx.pausePending(run); !req || m != "now" {
		t.Fatalf("after pause now: (%q, %v), want an owner 'now' request", m, req)
	}
	// The pass never withdraws it, whatever the reading says.
	fx.reading(tok, 5, 10, "1 minute")
	fx.pass()
	if m, _, req := fx.pausePending(run); !req || m != "now" {
		t.Fatalf("the reserve pass touched the owner request: (%q, %v)", m, req)
	}
}

// TestReserveRequeuedRequestWithToggleOffClaimsWithoutPauseLiveDB: a run requeued with a pending
// reserve request is claimed with no pause delivered (and the request withdrawn) once the toggle is
// off; with the reserve still enforced and the credential OK the request is served as 'milestone'.
func TestReserveRequeuedRequestWithToggleOffClaimsWithoutPauseLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("only", true, false, 10) // below the line: the claim is admitted either way
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{firstStarted: true})
	fx.env.exec(`UPDATE runs SET pause_requested_at = now(), pause_mode = 'reserve', pause_after_count = 0 WHERE id = $1`, id)
	fx.env.exec(`INSERT INTO run_user_inputs (run_id, kind, body) VALUES ($1, 'pause', 'milestone')`, id)

	// Still enforced: the worker is told 'milestone', never 'reserve'.
	p := fx.claim()
	if p == nil || !p.PausePending || p.PauseMode != "milestone" {
		t.Fatalf("enforced claim = %+v, want a pending 'milestone' pause", p)
	}
	fx.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, claim_generation = 0 WHERE id = $1`, id)

	fx.setReserve(70, false)
	p = fx.claim()
	if p == nil {
		t.Fatal("the requeued run was not claimed with the toggle off")
	}
	if p.PausePending || p.PauseMode != "" {
		t.Fatalf("claim delivered a pause (%v, %q) for a reserve request the reserve no longer enforces", p.PausePending, p.PauseMode)
	}
	if _, _, req := fx.pausePending(id); req {
		t.Fatal("the stale reserve request was not withdrawn at the claim")
	}
	if fx.unappliedPauseInputs(id) != 0 {
		t.Fatal("the stale pause input stayed unapplied")
	}
}

// TestReserveOverrideEndsAtObservedResetLiveDB: a manual-resume exemption ends at the first of the
// clock, an observed early reset, or (no anchor) the first fresh below-threshold reading; the sweeper
// drops the ended exemption and ClaimRun's @reserve_override_ended list reaches the SQL gate.
func TestReserveOverrideEndsAtObservedResetLiveDB(t *testing.T) {
	fx := newReserveFix(t, 50, true)
	tok := fx.token("only", true, false, 60)
	fx.bindWorker(BindModeDefault, nil)
	anchor := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_resets_at = $2 WHERE user_secret_id = $1`, tok, anchor)

	granted := time.Now().Add(-2 * time.Hour)
	mk := func(withAnchor bool, until time.Time) uuid.UUID {
		id := fx.queueRun(reserveRun{firstStarted: true})
		var wr, prior any
		if withAnchor {
			wr, prior = anchor, 60 // a real grant records the utilization it read with the anchor
		}
		fx.env.exec(`UPDATE runs SET reserve_exempt_until = $2, reserve_exempt_granted_at = $3, reserve_exempt_credential_id = $4,
		             reserve_exempt_window_reset = $5, reserve_exempt_prior_pct = $6, anthropic_secret_id = $4 WHERE id = $1`, id, until, granted, tok, wr, prior)
		return id
	}
	run := mk(true, anchor)

	// Reading synced after the grant, boundary unmoved, still 60%: the exemption stands, claim admitted.
	if !fx.admitsWithEnded(run) {
		t.Fatal("an active exemption did not admit the run")
	}
	// The week resets EARLY: boundary moves a week out, usage already 60% again (>= 50): the exemption
	// has ended, so the run is held again even though its clock window has not elapsed.
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_resets_at = $2, seven_day_pct = 60, synced_at = now() WHERE user_secret_id = $1`,
		tok, anchor.Add(7*24*time.Hour))
	if fx.admitsWithEnded(run) {
		t.Fatal("an observed early reset did not end the exemption at the claim gate")
	}
	// The whole claim (claimLane wires the ended list into ClaimRun) holds the run too.
	if p := fx.claim(); p != nil {
		t.Fatal("svc.Claim admitted a run whose exemption an observed early reset has ended")
	}
	// ... and ClaimRun itself withheld it (generation untouched), rather than the assembly backstop
	// claiming it and requeueing it afterwards.
	if g := mustRun(t, fx.env, run).ClaimGeneration; g != 0 {
		t.Fatalf("claim_generation = %d: ClaimRun admitted the run and only the assembly backstop held it", g)
	}
	if n := fx.pass(); n < 1 {
		t.Fatal("the pass did not drop the ended exemption")
	}
	if r := mustRun(t, fx.env, run); r.ReserveExemptUntil.Valid || r.ReserveExemptGrantedAt.Valid || r.ReserveExemptCredentialID.Valid || r.ReserveExemptWindowReset.Valid || r.ReserveExemptPriorPct.Valid {
		t.Fatalf("an ended exemption kept columns: %+v", r)
	}

	// No metadata at the grant: the first fresh reading below the threshold ends it.
	fx.reading(tok, 60, 10, "1 minute")
	noAnchor := mk(false, time.Now().Add(6*24*time.Hour))
	if !fx.admitsWithEnded(noAnchor) {
		t.Fatal("an anchor-less exemption on a held reading did not admit")
	}
	fx.reading(tok, 20, 10, "1 minute")
	if r := mustRun(t, fx.env, noAnchor); overrideActive(r, fx.snapshot(), time.Now()) {
		t.Fatal("overrideActive still true after a fresh below-threshold reading")
	}
	fx.pass()
	if r := mustRun(t, fx.env, noAnchor); r.ReserveExemptUntil.Valid {
		t.Fatal("the anchor-less exemption survived a fresh below-threshold reading")
	}

	// The clock: an elapsed exemption is dropped even with no reading movement.
	elapsed := mk(true, time.Now().Add(-time.Minute))
	fx.pass()
	if mustRun(t, fx.env, elapsed).ReserveExemptUntil.Valid {
		t.Fatal("an elapsed exemption was not dropped")
	}
}

func (fx *reserveFix) snapshot() *reserveSnap {
	fx.t.Helper()
	snap, err := fx.svc.reserveSnapshot(fx.env.ctx, fx.userID, time.Now())
	if err != nil {
		fx.t.Fatalf("reserveSnapshot: %v", err)
	}
	return snap
}

// admitsWithEnded is admits() with ClaimRun's @reserve_override_ended list computed as claimLane does.
func (fx *reserveFix) admitsWithEnded(runID uuid.UUID) bool {
	fx.t.Helper()
	now := time.Now()
	p := fx.claimParams(fx.workerID)
	ended, err := fx.svc.reserveEndedOverrides(fx.env.ctx, fx.snapshot(), now)
	if err != nil {
		fx.t.Fatalf("reserveEndedOverrides: %v", err)
	}
	p.ReserveOverrideEnded = ended
	run, err := fx.env.q.ClaimRun(fx.env.ctx, p)
	if err != nil {
		return false
	}
	if run.ID != runID {
		fx.t.Fatalf("ClaimRun claimed %s, want %s", run.ID, runID)
	}
	fx.env.exec(`UPDATE runs SET status = 'queued', worker_id = NULL, claim_generation = 0 WHERE id = $1`, runID)
	return true
}

// TestReserveOverrideGrantComputationLiveDB: ComputeReserveOverride anchors the exemption on the
// owner's current weekly reset, caps it at seven days, and falls back to the cap with no usable reset.
func TestReserveOverrideGrantComputationLiveDB(t *testing.T) {
	fx := newReserveFix(t, 50, true)
	tok := fx.token("only", true, false, 60)
	resets := time.Now().Add(30 * time.Hour).UTC().Truncate(time.Second)
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_resets_at = $2 WHERE user_secret_id = $1`, tok, resets)
	run := mustRun(t, fx.env, fx.pauseRun(pauseRun{cred: &tok, status: "paused", hold: "weekly_reserve"}))
	iv := ReserveIntervals{Claude: 5 * time.Minute, Codex: 5 * time.Minute}
	now := time.Now()

	g, err := ComputeReserveOverride(fx.env.ctx, fx.env.q, run, iv, now)
	if err != nil || g == nil {
		t.Fatalf("ComputeReserveOverride = (%v, %v)", g, err)
	}
	if g.WindowReset == nil || !g.WindowReset.Equal(resets) || !g.Until.Equal(resets) || g.CredentialID == nil || *g.CredentialID != tok {
		t.Fatalf("grant = %+v, want anchored on %v for %s", g, resets, tok)
	}
	// A reset further than a week away is capped.
	far := now.Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_resets_at = $2 WHERE user_secret_id = $1`, tok, far)
	if g, _ = ComputeReserveOverride(fx.env.ctx, fx.env.q, run, iv, now); !g.Until.Equal(now.Add(ReserveOverrideWindow)) {
		t.Fatalf("until = %v, want the %v cap", g.Until, ReserveOverrideWindow)
	}
	// A fresh reading BELOW the threshold at the grant does not establish "read at or above the
	// threshold", so no anchor is recorded either: seven days, the first-fresh-OK arm.
	fx.reading(tok, 10, 10, "1 minute")
	if g, _ = ComputeReserveOverride(fx.env.ctx, fx.env.q, run, iv, now); g.WindowReset != nil || !g.Until.Equal(now.Add(ReserveOverrideWindow)) {
		t.Fatalf("fresh-OK grant = %+v, want no anchor and the seven-day cap", g)
	}
	// A stale reading above the threshold is no better.
	fx.reading(tok, 90, 10, "3 hours")
	if g, _ = ComputeReserveOverride(fx.env.ctx, fx.env.q, run, iv, now); g.WindowReset != nil {
		t.Fatalf("stale-held grant = %+v, want no anchor", g)
	}
	// No reading: no anchor, seven days.
	fx.env.exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id = $1`, tok)
	g, _ = ComputeReserveOverride(fx.env.ctx, fx.env.q, run, iv, now)
	if g.WindowReset != nil || !g.Until.Equal(now.Add(ReserveOverrideWindow)) {
		t.Fatalf("no-metadata grant = %+v, want no anchor and the seven-day cap", g)
	}
	// Threshold cleared: nothing to override.
	fx.setReserve(0, true)
	if g, _ = ComputeReserveOverride(fx.env.ctx, fx.env.q, run, iv, now); g != nil {
		t.Fatalf("grant with the reserve off = %+v, want nil", g)
	}
}

// TestReservePauseUsesCodexAliasForCodexRunsLiveDB: a Codex run is judged on its recorded alias, not
// on the Anthropic token column, and an API-key alias (structurally unmetered) is never paused.
func TestReservePauseUsesCodexAliasForCodexRunsLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	okTok := fx.token("ok", true, false, 5)
	hot := fx.subAlias(weeklyBuckets(codexWindow(fiveHourSecs, 5), codexWindow(weekSecs, 90)))
	cool := fx.subAlias(weeklyBuckets(codexWindow(weekSecs, 10), codexWindow(fiveHourSecs, 99)))
	key := fx.env.seedStaticAPIKey(t, fx.userID, "key", "fixture-"+uuid.NewString())
	start := func(alias uuid.UUID) uuid.UUID {
		id := fx.queueRun(reserveRun{})
		mode := codexAuthModeSubscription
		if alias == key {
			mode = codexAuthModeAPIKey
		}
		if err := fx.svc.FreezeCodexBinding(fx.env.ctx, fx.userID, id, alias, mode); err != nil {
			t.Fatalf("FreezeCodexBinding: %v", err)
		}
		// The Anthropic column names an OK token: only the alias may decide.
		fx.env.exec(`UPDATE runs SET status = 'running', worker_id = $2, started_at = now() - interval '1 hour',
		             first_started_at = now() - interval '1 hour', anthropic_secret_id = $3 WHERE id = $1`, id, fx.workerID, okTok)
		return id
	}
	hotRun, coolRun, keyRun := start(hot), start(cool), start(key)
	if n := fx.pass(); n != 1 {
		t.Fatalf("pass changed %d runs, want only the run on the 90%% alias", n)
	}
	if m, _, req := fx.pausePending(hotRun); !req || m != "reserve" {
		t.Fatalf("hot codex alias run pause = (%q, %v), want reserve", m, req)
	}
	for name, id := range map[string]uuid.UUID{"below-threshold alias": coolRun, "API-key alias": keyRun} {
		if _, _, req := fx.pausePending(id); req {
			t.Fatalf("%s: a pause was requested", name)
		}
	}
}
