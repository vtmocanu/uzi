package schedsvc

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4512 (ADR-4007 T3a), scheduler level, against PostgreSQL 17: the real run-create
// admission (run + fire header + ledger row in one transaction) and the real summary
// transaction, driven through Scheduler.process.
//
// RETRY DRIVER (pinned): every retry calls process again on the FIRST attempt's claimed
// store.RunSchedule snapshot (fx.snapshot), never on ClaimDueSchedules, so the retry is a
// deterministic re-fire of the same claimed occurrence. For (c) the retry runs on a fresh
// Scheduler and a fresh workersvc.Service over the same database.
//
// The terminal consumer gate is simulated at the RunCreator seam (onceRetryRuns.busy returns
// ErrTerminalConsumerGateBusy before anything is created, exactly where the real gate refuses);
// the real gate is covered in workersvc.
//
// Letters (a)..(q) match the regression list of #4512 M3. (r), the mismatched-progress identity
// guard, is workersvc's TestOnceFireIdentityLiveDB. (n)'s same-epoch interleavings with the
// pause hook are workersvc's TestOnceFireSummary* tests (the hook is a workersvc field); the
// scheduler-level stale-view half is (n) below. Every name ends LiveDB.

// onceRetryRuns wraps the real Service: it can refuse a candidate with the busy gate error, and
// can fail a scheduled create AFTER it committed (a lost acknowledgement).
type onceRetryRuns struct {
	*workersvc.Service
	mu       sync.Mutex
	busy     map[int64]bool
	lostAck  map[int64]error
	progress []workersvc.OnceFireProgress
}

func (r *onceRetryRuns) gate(iid int64, p *workersvc.OnceFireProgress) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p != nil {
		r.progress = append(r.progress, *p)
	}
	if r.busy[iid] {
		return workersvc.ErrTerminalConsumerGateBusy
	}
	return nil
}

func (r *onceRetryRuns) ack(iid int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lostAck[iid]
}

func (r *onceRetryRuns) CreateScheduledAutopilotRun(ctx context.Context, user, repo uuid.UUID, iid int64, desc string, wait *bool, rework *bool, model *string, sub bool, cred *workersvc.CredentialOverride, h *workersvc.Harness, p *workersvc.OnceFireProgress) (store.Run, error) {
	if err := r.gate(iid, p); err != nil {
		return store.Run{}, err
	}
	run, err := r.Service.CreateScheduledAutopilotRun(ctx, user, repo, iid, desc, wait, rework, model, sub, cred, h, p)
	if err == nil {
		if lost := r.ack(iid); lost != nil {
			return store.Run{}, lost
		}
	}
	return run, err
}

// onceProgressSpy counts the scheduled fire's ledger reads (RunNow must make none).
type onceProgressSpy struct {
	*store.Queries
	mu    sync.Mutex
	lists int
	caps  int
}

func (s *onceProgressSpy) ListOnceFireProgress(ctx context.Context, a store.ListOnceFireProgressParams) ([]store.ListOnceFireProgressRow, error) {
	s.mu.Lock()
	s.lists++
	s.mu.Unlock()
	return s.Queries.ListOnceFireProgress(ctx, a)
}

func (s *onceProgressSpy) GetOnceFireCap(ctx context.Context, a store.GetOnceFireCapParams) (pgtype.Int4, error) {
	s.mu.Lock()
	s.caps++
	s.mu.Unlock()
	return s.Queries.GetOnceFireCap(ctx, a)
}

type onceRetryFx struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	q     *store.Queries
	spy   *onceProgressSpy
	user  uuid.UUID
	repo  uuid.UUID
	token uuid.UUID
	svc   *workersvc.Service
	runs  *onceRetryRuns
	sched *Scheduler
	notif *fakeNotifier
	id    uuid.UUID
	box   *secretbox.Box
}

// newOnceRetryFx seeds a user with an Anthropic default, an enabled repo and ONE once sweep
// (label uzi, max_issues = maxIssues, due an hour ago, auto-approve).
func newOnceRetryFx(t *testing.T, maxIssues int32) *onceRetryFx {
	t.Helper()
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	user, repo := scheduleFireUser(ctx, t, pool)
	tok := seedAnthropicDefault(ctx, t, pool, box, user)
	fx := &onceRetryFx{t: t, ctx: ctx, pool: pool, q: q, user: user, repo: repo, token: tok, notif: &fakeNotifier{}, box: box}
	due := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	mi := pgtype.Int4{}
	if maxIssues > 0 {
		mi = pgtype.Int4{Int32: maxIssues, Valid: true}
	}
	sc, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID: user, RepoID: repo, Target: "sweep", Labels: []byte(`["uzi"]`), Timing: "once",
		RunAt: pgtype.Timestamptz{Time: due, Valid: true}, NextFireAt: pgtype.Timestamptz{Time: due, Valid: true},
		Timezone: "UTC", AutoApprove: true, Enabled: true, MaxIssues: mi,
	})
	if err != nil {
		t.Fatalf("create once sweep: %v", err)
	}
	fx.id = sc.ID
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM run_schedules WHERE id=$1`, sc.ID) })
	fx.scheduler()
	return fx
}

// scheduler (re)builds a fresh Service and Scheduler over the same database.
func (fx *onceRetryFx) scheduler() {
	fx.t.Helper()
	svc := workersvc.New(fx.q, fx.box, workersvc.Params{})
	svc.SetTxBeginner(fx.pool)
	fx.svc = svc
	fx.runs = &onceRetryRuns{Service: svc, busy: map[int64]bool{}, lostAck: map[int64]error{}}
	fx.spy = &onceProgressSpy{Queries: fx.q}
	fb := &fakeBuilder{f: &fakeForge{issue: forge.Issue{Title: "candidate", Description: "body", Labels: []string{"uzi"}, WebURL: "https://forge.e2e/issue"}}}
	fx.sched = New(fx.spy, fx.runs, fb, &fakeSettings{uziLabel: "uzi"}, fx.notif, nil, time.Minute, nil)
}

func (fx *onceRetryFx) issues(iids ...int64) {
	fx.t.Helper()
	for _, iid := range iids {
		if _, err := fx.pool.Exec(fx.ctx, `INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		          VALUES ($1, $2, $3, 'candidate', 'opened', '["uzi"]', 'https://forge.e2e/issue', false, now(), now())`, uuid.New(), fx.repo, iid); err != nil {
			fx.t.Fatalf("seed issue %d: %v", iid, err)
		}
	}
}

// snapshot is the claimed row a retry re-fires.
func (fx *onceRetryFx) snapshot() store.RunSchedule { return fx.row() }

func (fx *onceRetryFx) row() store.RunSchedule {
	fx.t.Helper()
	sc, err := fx.q.GetRunSchedule(fx.ctx, fx.id)
	if err != nil {
		fx.t.Fatal(err)
	}
	return sc
}

func (fx *onceRetryFx) fire(sc store.RunSchedule) {
	fx.t.Helper()
	pc := &pauseCache{e: fx.sched, seen: map[uuid.UUID]pauseState{}, scoutSlots: scoutTickSlots}
	fx.sched.process(fx.ctx, sc, pc)
}

type onceRunRow struct {
	ID     uuid.UUID
	Status string
}

func (fx *onceRetryFx) runsFor(iid int64) []onceRunRow {
	fx.t.Helper()
	rows, err := fx.pool.Query(fx.ctx, `SELECT id, status FROM runs WHERE repo_id=$1 AND issue_iid=$2 ORDER BY created_at, id`, fx.repo, iid)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer rows.Close()
	var out []onceRunRow
	for rows.Next() {
		var r onceRunRow
		if err := rows.Scan(&r.ID, &r.Status); err != nil {
			fx.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func (fx *onceRetryFx) totalRuns() int {
	fx.t.Helper()
	var n int
	if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM runs WHERE repo_id=$1`, fx.repo).Scan(&n); err != nil {
		fx.t.Fatal(err)
	}
	return n
}

func (fx *onceRetryFx) ledger(epoch int64) []store.ListOnceFireProgressRow {
	fx.t.Helper()
	rows, err := fx.q.ListOnceFireProgress(fx.ctx, store.ListOnceFireProgressParams{ScheduleID: fx.id, FireEpoch: epoch})
	if err != nil {
		fx.t.Fatal(err)
	}
	return rows
}

func (fx *onceRetryFx) lastFire(sc store.RunSchedule) lastFireRecord {
	fx.t.Helper()
	var rec lastFireRecord
	if len(sc.LastFire) == 0 {
		return rec
	}
	if err := json.Unmarshal(sc.LastFire, &rec); err != nil {
		fx.t.Fatalf("last_fire: %v", err)
	}
	return rec
}

func (fx *onceRetryFx) recent(sc store.RunSchedule) int {
	fx.t.Helper()
	var arr []json.RawMessage
	if err := json.Unmarshal(sc.RecentFires, &arr); err != nil {
		fx.t.Fatalf("recent_fires: %v", err)
	}
	return len(arr)
}

func (fx *onceRetryFx) setRunStatus(iid int64, status string) {
	fx.t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET status=$3 WHERE repo_id=$1 AND issue_iid=$2`, fx.repo, iid, status); err != nil {
		fx.t.Fatal(err)
	}
}

func onceStartedIIDs(rec lastFireRecord) []int64 {
	var out []int64
	for _, s := range rec.Started {
		out = append(out, *s.IssueIID)
	}
	return out
}

func (fx *onceRetryFx) wantStarted(rec lastFireRecord, want ...int64) {
	fx.t.Helper()
	got := onceStartedIIDs(rec)
	if len(got) != len(want) {
		fx.t.Fatalf("started = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			fx.t.Fatalf("started = %v, want %v", got, want)
		}
	}
	for _, s := range rec.Started {
		if id, err := uuid.Parse(s.RunID); err != nil || id == uuid.Nil {
			fx.t.Fatalf("started run_id %q is not a real run id", s.RunID)
		}
	}
}

func (fx *onceRetryFx) wantPending(sc store.RunSchedule) {
	fx.t.Helper()
	if sc.Status != "active" || !sc.NextFireAt.Valid || sc.LastFiredAt.Valid {
		fx.t.Fatalf("row must stay active and due: status=%q next=%+v last_fired=%+v", sc.Status, sc.NextFireAt, sc.LastFiredAt)
	}
}

func (fx *onceRetryFx) wantFired(sc store.RunSchedule) {
	fx.t.Helper()
	if sc.Status != "fired" || sc.NextFireAt.Valid || !sc.LastFiredAt.Valid {
		fx.t.Fatalf("row must be fired: status=%q next=%+v last_fired=%+v", sc.Status, sc.NextFireAt, sc.LastFiredAt)
	}
}

// partialAt96 runs the first attempt with the gate busy for busyIID: 96 starts, busyIID refuses.
func (fx *onceRetryFx) partialAt96(busyIID int64) store.RunSchedule {
	fx.t.Helper()
	snap := fx.snapshot()
	fx.runs.busy[busyIID] = true
	fx.fire(snap)
	sc := fx.row()
	fx.wantPending(sc)
	if rows := fx.ledger(snap.FireEpoch); len(rows) != 1 || rows[0].IssueIid != 96 {
		fx.t.Fatalf("ledger after the first attempt = %+v, want only 96", rows)
	}
	return snap
}

// (a) max_issues=2, 96/97/98 eligible. 96 starts, 97 meets the busy gate, the gate is released
// and the SAME claimed snapshot is re-fired with 96's run still active: two runs in all, 96 is
// never created again. Before #4512 the gate-busy fire advanced to fired after one start (the
// second slot lost); the naive hold would replay and start 96 again.
func TestOnceFireRetry_A_ActiveCandidateNeverRecreatedLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	snap := fx.partialAt96(97)
	mid := fx.row()
	fx.wantStarted(fx.lastFire(mid), 96)
	if rec := fx.lastFire(mid); len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipConsumerGateBusy) || rec.Matched != 2 {
		t.Fatalf("partial summary = %+v, want started 96 and the visible consumer_gate_busy skip", rec)
	}
	first := fx.runsFor(96)[0].ID
	delete(fx.runs.busy, 97)
	fx.fire(snap)
	if n := len(fx.runsFor(96)); n != 1 {
		t.Fatalf("96 has %d runs, want 1 (never re-created)", n)
	}
	if fx.totalRuns() != 2 || len(fx.runsFor(97)) != 1 || len(fx.runsFor(98)) != 0 {
		t.Fatalf("total runs = %d, want 96 and 97 only", fx.totalRuns())
	}
	done := fx.row()
	fx.wantFired(done)
	rec := fx.lastFire(done)
	fx.wantStarted(rec, 96, 97)
	if rec.Started[0].RunID != first.String() || len(rec.Skips) != 0 || rec.Matched != 2 {
		t.Fatalf("completed summary = %+v, want 96's original run id and no stale skip", rec)
	}
}

// (b) the same with 96's run already finished and no open MR: HasActiveRunForIssue no longer
// guards it, only the ledger does.
func TestOnceFireRetry_B_FinishedCandidateNeverRecreatedLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	snap := fx.partialAt96(97)
	fx.setRunStatus(96, "completed")
	delete(fx.runs.busy, 97)
	fx.fire(snap)
	if n := len(fx.runsFor(96)); n != 1 {
		t.Fatalf("96 has %d runs, want 1: a finished candidate must not be started again", n)
	}
	if fx.totalRuns() != 2 || len(fx.runsFor(98)) != 0 {
		t.Fatalf("total runs = %d, want 96 and 97 only", fx.totalRuns())
	}
	fx.wantFired(fx.row())
	fx.wantStarted(fx.lastFire(fx.row()), 96, 97)
}

// (c) 96 leaves the selector after its start (the label is removed through the normal forgesvc
// path: forge first, then the cache the sweep reads) and a FRESH scheduler and service retry: 96's slot is still spent, so only 97
// starts (not 97 and 98).
func TestOnceFireRetry_C_SelectorRemovalKeepsSlotSpentLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	snap := fx.partialAt96(97)
	// The normal label path: forgesvc removes the label on the forge first, then, only on forge
	// success, from the cache the sweep reads.
	issue96, err := fx.q.GetIssueByIID(fx.ctx, store.GetIssueByIIDParams{RepoID: fx.repo, ForgeIssueIid: 96})
	if err != nil {
		t.Fatal(err)
	}
	lf := &labelRemovalForge{}
	if _, err := forgesvc.New(fx.q, nil, time.Second, nil).SetIssueLabel(fx.ctx, lf, 1, issue96, "uzi", "", false); err != nil {
		t.Fatal(err)
	}
	if len(lf.removed) != 1 || lf.removed[0] != 96 {
		t.Fatalf("forge label removals = %v, want one for 96", lf.removed)
	}
	cands, err := fx.q.ListSweepCandidateIssues(fx.ctx, store.ListSweepCandidateIssuesParams{
		RepoID: fx.repo, Selector: "label", Labels: []byte(`["uzi"]`), UziLabel: "uzi", MaxIssues: pgtype.Int4{Int32: 12, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cands {
		if c.ForgeIssueIid == 96 {
			t.Fatal("96 must no longer be a sweep candidate after its label was removed")
		}
	}
	fx.scheduler() // a fresh scheduler and a fresh service
	fx.fire(snap)
	if len(fx.runsFor(97)) != 1 || len(fx.runsFor(98)) != 0 || fx.totalRuns() != 2 {
		t.Fatalf("total runs = %d (97: %d, 98: %d), want 96 and 97 only", fx.totalRuns(), len(fx.runsFor(97)), len(fx.runsFor(98)))
	}
	done := fx.row()
	fx.wantFired(done)
	fx.wantStarted(fx.lastFire(done), 96, 97) // 96 is folded from the ledger though it left the selector
}

// (d) the busy gate before ANY start still retries normally: not advanced, no ledger, no summary.
func TestOnceFireRetry_D_BusyBeforeAnyStartRetriesLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	snap := fx.snapshot()
	fx.runs.busy[96], fx.runs.busy[97], fx.runs.busy[98] = true, true, true
	fx.fire(snap)
	sc := fx.row()
	fx.wantPending(sc)
	if len(sc.LastFire) != 0 || fx.totalRuns() != 0 || len(fx.ledger(snap.FireEpoch)) != 0 {
		t.Fatalf("busy before any start must write nothing: last_fire=%s runs=%d", sc.LastFire, fx.totalRuns())
	}
	fx.runs.busy = map[int64]bool{}
	fx.fire(snap)
	fx.wantFired(fx.row())
	fx.wantStarted(fx.lastFire(fx.row()), 96, 97)
	if fx.totalRuns() != 2 {
		t.Fatalf("runs = %d, want 2", fx.totalRuns())
	}
}

// rearm edits the row like the PATCH handler's UpdateRunSchedule does, moving run_at.
func (fx *onceRetryFx) rearm() store.RunSchedule {
	fx.t.Helper()
	cur := fx.row()
	at := pgtype.Timestamptz{Time: time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond), Valid: true}
	sc, err := fx.q.UpdateRunSchedule(fx.ctx, store.UpdateRunScheduleParams{
		Target: cur.Target, RepoID: cur.RepoID, IssueIid: cur.IssueIid, Labels: cur.Labels, Prompt: cur.Prompt, Timing: cur.Timing, CronExpr: cur.CronExpr,
		RunAt: at, Timezone: cur.Timezone, NextFireAt: at, AutoApprove: cur.AutoApprove, WaitOnLimit: cur.WaitOnLimit, MrReworkEnabled: cur.MrReworkEnabled,
		MaxIssues: cur.MaxIssues, Guidance: cur.Guidance, Model: cur.Model, OutputMode: cur.OutputMode, RemoveLabelOnDispatch: cur.RemoveLabelOnDispatch,
		OverrideSubagentModel: cur.OverrideSubagentModel, CapacityLimit: cur.CapacityLimit, CapacityRoomNeeded: cur.CapacityRoomNeeded, Harness: cur.Harness,
		CredentialOverrideMode: cur.CredentialOverrideMode, CredentialOverrideSecretID: cur.CredentialOverrideSecretID, Customized: cur.Customized,
		ID: cur.ID, UserID: cur.UserID,
	})
	if err != nil {
		fx.t.Fatalf("re-arm: %v", err)
	}
	return sc
}

// (e) a PATCH re-arm (UpdateRunSchedule, moving run_at) is a NEW occurrence with the full cap; a
// non-re-arm edit keeps the progress; a stale writer (the old snapshot after the re-arm) can
// neither advance nor write a summary.
func TestOnceFireRetry_E_RearmIsNewOccurrenceStaleWriterRefusedLiveDB(t *testing.T) {
	t.Run("non re-arm edit keeps progress", func(t *testing.T) {
		fx := newOnceRetryFx(t, 2)
		fx.issues(96, 97, 98)
		snap := fx.partialAt96(97)
		if _, err := fx.pool.Exec(fx.ctx, `UPDATE run_schedules SET guidance='edited', enabled=false WHERE id=$1`, fx.id); err != nil {
			t.Fatal(err)
		}
		if _, err := fx.pool.Exec(fx.ctx, `UPDATE run_schedules SET enabled=true WHERE id=$1`, fx.id); err != nil {
			t.Fatal(err)
		}
		if after := fx.row(); after.FireEpoch != snap.FireEpoch {
			t.Fatalf("a non-re-arm edit moved the epoch: %d -> %d", snap.FireEpoch, after.FireEpoch)
		}
		delete(fx.runs.busy, 97)
		fx.fire(snap)
		if fx.totalRuns() != 2 {
			t.Fatalf("runs = %d, want 2: the edit must not restart the fire", fx.totalRuns())
		}
		fx.wantStarted(fx.lastFire(fx.row()), 96, 97)
	})
	t.Run("re-arm", func(t *testing.T) {
		fx := newOnceRetryFx(t, 2)
		fx.issues(96, 97, 98)
		old := fx.partialAt96(97)
		fx.setRunStatus(96, "completed") // the new occurrence may start a finished candidate again
		before := fx.row()
		after := fx.rearm()
		if after.FireEpoch != old.FireEpoch+1 || after.Status != "active" {
			t.Fatalf("re-arm epoch %d -> %d status=%q, want the epoch bumped by the trigger", old.FireEpoch, after.FireEpoch, after.Status)
		}
		// A stale writer: the old snapshot can neither advance nor write a summary.
		delete(fx.runs.busy, 97)
		fx.fire(old)
		stale := fx.row()
		if stale.Status != "active" || stale.LastFiredAt.Valid || string(stale.LastFire) != string(before.LastFire) || fx.totalRuns() != 1 {
			t.Fatalf("stale fire changed the new occurrence: status=%q last_fire=%s runs=%d", stale.Status, stale.LastFire, fx.totalRuns())
		}
		fx.sched.writeOnceSweepSummary(fx.ctx, old, FireOutcome{Matched: 1, Skips: []Skip{{Reason: SkipCredentialDisabled}}}, onceSummaryComplete)
		fx.sched.writeOnceSweepSummary(fx.ctx, old, FireOutcome{Matched: 1, Skips: []Skip{{Reason: SkipCredentialDisabled}}}, onceSummaryHeld)
		fx.sched.writeOnceSweepSummary(fx.ctx, old, FireOutcome{Matched: 1, Skips: []Skip{{Reason: SkipConsumerGateBusy}}}, onceSummaryPartial)
		if again := fx.row(); again.Status != "active" || again.LastFiredAt.Valid || string(again.LastFire) != string(before.LastFire) {
			t.Fatalf("a stale summary writer changed the row: status=%q last_fire=%s", again.Status, again.LastFire)
		}
		// The new occurrence has the full cap again.
		fresh := fx.snapshot()
		fx.fire(fresh)
		if len(fx.ledger(fresh.FireEpoch)) != 2 || len(fx.runsFor(96)) != 2 || len(fx.runsFor(97)) != 1 {
			t.Fatalf("new occurrence did not get its full cap: ledger=%+v 96 runs=%d 97 runs=%d", fx.ledger(fresh.FireEpoch), len(fx.runsFor(96)), len(fx.runsFor(97)))
		}
		fx.wantFired(fx.row())
		fx.wantStarted(fx.lastFire(fx.row()), 96, 97)
	})
}

// (f) a held credential-disabled write keeps the Started entries, writes last_fire only (never
// recent_fires), and leaves the row active and due.
func TestOnceFireRetry_F_HeldWriteKeepsStartedLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	fx.pinCredential()
	snap := fx.partialAt96(97)
	fx.setPin(false)
	fx.fire(snap)
	sc := fx.row()
	fx.wantPending(sc)
	rec := fx.lastFire(sc)
	fx.wantStarted(rec, 96)
	if len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipCredentialDisabled) || rec.Matched != 2 {
		t.Fatalf("held summary = %+v, want started 96 and the credential_disabled skip", rec)
	}
	if fx.recent(sc) != 0 {
		t.Fatalf("recent_fires has %d entries, want none from a partial or held write", fx.recent(sc))
	}
}

func (fx *onceRetryFx) pinCredential() {
	fx.t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE run_schedules SET credential_override_mode='pinned', credential_override_secret_id=$2 WHERE id=$1`, fx.id, fx.token); err != nil {
		fx.t.Fatal(err)
	}
}

// setPin enables or disables the pinned secret.
func (fx *onceRetryFx) setPin(enabled bool) {
	fx.t.Helper()
	q := `UPDATE user_secrets SET disabled_at=now(), enablement_rev=enablement_rev+1 WHERE id=$1`
	if enabled {
		q = `UPDATE user_secrets SET disabled_at=NULL, enablement_rev=enablement_rev+1 WHERE id=$1`
	}
	if _, err := fx.pool.Exec(fx.ctx, q, fx.token); err != nil {
		fx.t.Fatal(err)
	}
}

// (g) RunNow on a once sweep: no ledger read, no fold, no last_fire write, the base error
// mapping, and the scheduled fire's slots untouched.
func TestOnceFireRetry_G_RunNowIsADistinctFireLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	snap := fx.partialAt96(97)
	ledgerBefore := fx.ledger(snap.FireEpoch)
	before := fx.row()
	listsBefore, capsBefore := fx.spy.lists, fx.spy.caps
	fx.setRunStatus(96, "completed")

	// Busy gate: the base error mapping (returned unchanged), nothing recorded.
	fx.runs.busy[96], fx.runs.busy[97], fx.runs.busy[98] = true, true, true
	if _, err := fx.sched.RunNow(fx.ctx, snap); !errors.Is(err, workersvc.ErrTerminalConsumerGateBusy) {
		t.Fatalf("RunNow err = %v, want the busy error returned as is", err)
	}
	fx.runs.busy = map[int64]bool{}
	out, err := fx.sched.RunNow(fx.ctx, snap)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if len(out.Started) != 2 || out.Matched != 2 {
		t.Fatalf("RunNow outcome = %+v, want a fresh two-start fire (it does not fold the scheduled fire's slots)", out)
	}
	if fx.spy.lists != listsBefore || fx.spy.caps != capsBefore {
		t.Fatal("RunNow read the once-fire ledger")
	}
	after := fx.row()
	if string(after.LastFire) != string(before.LastFire) || after.Status != "active" || after.LastFiredAt.Valid {
		t.Fatalf("RunNow wrote the row: last_fire %s -> %s status=%q", before.LastFire, after.LastFire, after.Status)
	}
	if got := fx.ledger(snap.FireEpoch); len(got) != len(ledgerBefore) || got[0].RunUuid != ledgerBefore[0].RunUuid {
		t.Fatalf("RunNow touched the scheduled fire's ledger: %+v -> %+v", ledgerBefore, got)
	}
}

// (k) partial progress is visible while the row waits, recent_fires carries no partial or held
// duplicate, and only the completion appends one entry.
func TestOnceFireRetry_K_PartialVisibleNoRecentFiresDuplicationLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	snap := fx.partialAt96(97)
	sc := fx.row()
	fx.wantStarted(fx.lastFire(sc), 96)
	if fx.recent(sc) != 0 {
		t.Fatalf("recent_fires after the partial write = %d, want 0", fx.recent(sc))
	}
	// Still busy: the same partial summary is not rewritten (updated_at is stable).
	fx.fire(snap)
	again := fx.row()
	if !again.UpdatedAt.Time.Equal(sc.UpdatedAt.Time) || fx.recent(again) != 0 {
		t.Fatalf("an unchanged partial summary was rewritten: updated_at %v -> %v", sc.UpdatedAt.Time, again.UpdatedAt.Time)
	}
	delete(fx.runs.busy, 97)
	fx.fire(snap)
	if done := fx.row(); fx.recent(done) != 1 {
		t.Fatalf("recent_fires after completion = %d, want exactly 1", fx.recent(done))
	}
}

// (l) a lost acknowledgement inside the fire (the create committed, the call errored) appears
// once in Started and consumes its slot.
func TestOnceFireRetry_L_LostAckStartedOnceLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	fx.runs.lostAck[96] = errors.New("connection reset after commit")
	snap := fx.snapshot()
	fx.fire(snap)
	done := fx.row()
	fx.wantFired(done)
	rec := fx.lastFire(done)
	fx.wantStarted(rec, 96, 97)
	if len(rec.Skips) != 0 || rec.Matched != 2 || fx.totalRuns() != 2 || len(fx.runsFor(98)) != 0 {
		t.Fatalf("lost-ack summary = %+v runs=%d, want 96 once, no fetch_failed skip, cap respected", rec, fx.totalRuns())
	}
}

// (n) scheduler level: a summary writer holding a STALE view (it saw only 96) still leaves 96
// and 97 stored, because Started is rebuilt from the ledger under the lock and never taken from
// the caller. (The same-epoch interleavings with the pause hook are in workersvc.)
func TestOnceFireRetry_N_StaleViewWriterKeepsLedgerStartsLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 3)
	fx.issues(96, 97, 98)
	fx.runs.busy[98] = true
	snap := fx.snapshot()
	fx.fire(snap) // 96 and 97 start, 98 busy: partial summary lists both
	staleView := FireOutcome{Matched: 2, Started: []Started{{IssueIID: ptrI64(96), RunID: uuid.New(), Title: "candidate"}}, Skips: []Skip{{IssueIID: ptrI64(98), Reason: SkipConsumerGateBusy}}}
	for _, mode := range []onceSummaryMode{onceSummaryPartial, onceSummaryHeld} {
		fx.sched.writeOnceSweepSummary(fx.ctx, snap, staleView, mode)
		fx.wantStarted(fx.lastFire(fx.row()), 96, 97)
	}
	// The completion likewise: it lists 96 and 97 (with their ledger run ids), not the stale view.
	fx.sched.writeOnceSweepSummary(fx.ctx, snap, staleView, onceSummaryComplete)
	done := fx.row()
	fx.wantFired(done)
	fx.wantStarted(fx.lastFire(done), 96, 97)
	for i, r := range fx.ledger(snap.FireEpoch) {
		if fx.lastFire(done).Started[i].RunID != r.RunUuid.String() {
			t.Fatalf("started[%d].run_id is not the ledger's run uuid", i)
		}
	}
}

// (o) delayed permanent errors from a replaced occurrence: a re-arm before a delayed permanent
// error leaves the new occurrence active and sends no notification; a recurring permanent error
// still parks and notifies; a recurring row converted to once mid-fire is neither parked nor
// advanced by the old fire.
func TestOnceFireRetry_O_GuardedParkAndAdvanceLiveDB(t *testing.T) {
	t.Run("re-armed once sweep is not parked", func(t *testing.T) {
		fx := newOnceRetryFx(t, 2)
		old := fx.snapshot()
		fx.rearm()
		fx.sched.advance(fx.ctx, old, FireOutcome{}, workersvc.ErrRepoNotFound)
		if sc := fx.row(); sc.Status != "active" || len(fx.notif.notifications) != 0 {
			t.Fatalf("status=%q notifications=%d, want the new occurrence active and no notification", sc.Status, len(fx.notif.notifications))
		}
		fx.sched.advance(fx.ctx, old, FireOutcome{Matched: 1}, nil)
		if sc := fx.row(); sc.Status != "active" || sc.LastFiredAt.Valid {
			t.Fatalf("a stale advance changed the new occurrence: status=%q", sc.Status)
		}
	})
	t.Run("current once sweep still parks and notifies", func(t *testing.T) {
		fx := newOnceRetryFx(t, 2)
		fx.sched.advance(fx.ctx, fx.snapshot(), FireOutcome{}, workersvc.ErrRepoNotFound)
		if sc := fx.row(); sc.Status != "error" || len(fx.notif.notifications) != 1 {
			t.Fatalf("status=%q notifications=%d, want parked with one notification", sc.Status, len(fx.notif.notifications))
		}
	})
	t.Run("recurring permanent error still parks and notifies", func(t *testing.T) {
		fx := newOnceRetryFx(t, 2)
		fx.toRecurring()
		fx.sched.advance(fx.ctx, fx.snapshot(), FireOutcome{}, workersvc.ErrRepoNotFound)
		if sc := fx.row(); sc.Status != "error" || len(fx.notif.notifications) != 1 {
			t.Fatalf("status=%q notifications=%d, want parked with one notification", sc.Status, len(fx.notif.notifications))
		}
	})
	t.Run("recurring converted to once mid-fire", func(t *testing.T) {
		fx := newOnceRetryFx(t, 2)
		fx.toRecurring()
		old := fx.snapshot()
		fx.toOnce()
		converted := fx.row()
		if converted.FireEpoch != old.FireEpoch+1 {
			t.Fatalf("conversion epoch %d -> %d, want bumped", old.FireEpoch, converted.FireEpoch)
		}
		fx.sched.advance(fx.ctx, old, FireOutcome{}, workersvc.ErrRepoNotFound)
		fx.sched.advance(fx.ctx, old, FireOutcome{Matched: 1}, nil)
		got := fx.row()
		if got.Status != "active" || got.Timing != "once" || got.LastFiredAt.Valid || len(got.LastFire) != 0 ||
			!got.NextFireAt.Time.Equal(converted.NextFireAt.Time) || len(fx.notif.notifications) != 0 {
			t.Fatalf("the old fire touched the converted row: status=%q timing=%q last_fire=%s next=%v notifications=%d",
				got.Status, got.Timing, got.LastFire, got.NextFireAt, len(fx.notif.notifications))
		}
	})
	t.Run("recurring advance keeps its cadence", func(t *testing.T) {
		fx := newOnceRetryFx(t, 2)
		fx.toRecurring()
		old := fx.snapshot()
		fx.sched.advance(fx.ctx, old, FireOutcome{Matched: 1}, nil)
		got := fx.row()
		if got.Status != "active" || !got.LastFiredAt.Valid || !got.NextFireAt.Valid || !got.NextFireAt.Time.After(old.NextFireAt.Time) || got.FireEpoch != old.FireEpoch {
			t.Fatalf("recurring advance: status=%q next=%v epoch %d -> %d", got.Status, got.NextFireAt, old.FireEpoch, got.FireEpoch)
		}
	})
}

func (fx *onceRetryFx) toRecurring() {
	fx.t.Helper()
	next := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE run_schedules SET timing='recurring', cron_expr='0 * * * *', run_at=NULL, next_fire_at=$2 WHERE id=$1`, fx.id, next); err != nil {
		fx.t.Fatal(err)
	}
}

func (fx *onceRetryFx) toOnce() {
	fx.t.Helper()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if _, err := fx.pool.Exec(fx.ctx, `UPDATE run_schedules SET timing='once', cron_expr=NULL, run_at=$2, next_fire_at=$2 WHERE id=$1`, fx.id, at); err != nil {
		fx.t.Fatal(err)
	}
}

// (p) a delayed hold writer that saw zero progress (its snapshot predates every start) leaves
// the stored 96/97 summary's Started entries intact: the hold is composed under the lock from
// the ledger, never as a no-progress record.
func TestOnceFireRetry_P_DelayedHoldWithoutProgressKeepsStartsLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 3)
	fx.issues(96, 97, 98)
	stale := fx.snapshot() // zero progress view
	fx.runs.busy[98] = true
	fx.fire(stale)
	fx.wantStarted(fx.lastFire(fx.row()), 96, 97)
	fx.sched.holdOnceCredentialDisabled(fx.ctx, stale, SkipCredentialDisabled)
	sc := fx.row()
	fx.wantStarted(fx.lastFire(sc), 96, 97)
	if rec := fx.lastFire(sc); len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipCredentialDisabled) {
		t.Fatalf("held summary = %+v", rec)
	}
	fx.wantPending(sc)
	if fx.recent(sc) != 0 {
		t.Fatalf("recent_fires = %d, want 0", fx.recent(sc))
	}
}

// (q) 96 is recorded, then its run row is DELETED and the schedule's pinned credential disabled:
// the retry is held (holdOnceCredentialDisabled) and writes a held summary that still carries
// 96's ORIGINAL run uuid; after re-enable the next retry starts 97 and completes. The spent
// count never changes, no run is created for 96, and no Started.RunID is ever the nil UUID.
func TestOnceFireRetry_Q_DeletedRunKeepsLedgerIdentityLiveDB(t *testing.T) {
	fx := newOnceRetryFx(t, 2)
	fx.issues(96, 97, 98)
	fx.pinCredential()
	snap := fx.partialAt96(97)
	orig := fx.runsFor(96)[0].ID
	if _, err := fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id=$1`, orig); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	fx.setPin(false)
	delete(fx.runs.busy, 97)
	fx.fire(snap)
	held := fx.row()
	fx.wantPending(held)
	rec := fx.lastFire(held)
	fx.wantStarted(rec, 96)
	if rec.Started[0].RunID != orig.String() || len(rec.Skips) != 1 || rec.Skips[0].Reason != string(SkipCredentialDisabled) {
		t.Fatalf("held summary = %+v, want 96 under its original run id %s and the credential_disabled skip", rec, orig)
	}
	if rows := fx.ledger(snap.FireEpoch); len(rows) != 1 || rows[0].RunUuid != orig {
		t.Fatalf("ledger = %+v, want 96 with the original run uuid", rows)
	}
	fx.setPin(true)
	fx.fire(snap)
	done := fx.row()
	fx.wantFired(done)
	rec = fx.lastFire(done)
	fx.wantStarted(rec, 96, 97)
	if rec.Started[0].RunID != orig.String() || len(rec.Skips) != 0 || rec.Matched != 2 {
		t.Fatalf("completed summary = %+v", rec)
	}
	if len(fx.runsFor(96)) != 0 || len(fx.runsFor(97)) != 1 || fx.totalRuns() != 1 {
		t.Fatalf("runs: 96=%d 97=%d total=%d, want no run for 96 and one for 97", len(fx.runsFor(96)), len(fx.runsFor(97)), fx.totalRuns())
	}
	if rows := fx.ledger(snap.FireEpoch); len(rows) != 2 {
		t.Fatalf("ledger rows = %d, want 2", len(rows))
	}
}

// labelRemovalForge is the forge half of regression (c): it records the label removals the
// normal forgesvc path sends before it touches the cache.
type labelRemovalForge struct {
	forge.IssueTracker
	removed []int64
}

func (f *labelRemovalForge) UpdateIssueLabels(_ context.Context, _ int64, iid int64, _, remove []string) error {
	if len(remove) == 1 && remove[0] == "uzi" {
		f.removed = append(f.removed, iid)
	}
	return nil
}
