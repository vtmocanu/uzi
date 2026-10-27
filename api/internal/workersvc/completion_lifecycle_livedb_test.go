package workersvc

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1232 M1: the structural completion interlock driven as ONE run through the production
// Service entry points, against a real throwaway Postgres (skipped unless UZI_TEST_DATABASE_URL is
// set; run via ./e2e/run-store-it.sh). Every lifecycle state change below goes through a Service
// method (CreateRun, Claim, SetState, SubmitInput, ConsumeInputs, RecordCompletionAttempt,
// RequestCompletionPermit, SetRunCompletionHold, ContinueCompletionDecision, DecideCompletion). The
// only direct writes are fixture inserts (users, repos, issues, workers) and the app_settings
// kill-switch row. Reads go through SQL for assertions only.
//
// The shape is issue #1220: a plan with five milestones, a lead that declares four of them, and a
// server that must not let that run finish. Each case owns a fresh user, so a Claim here can only
// ever take this test's own run (ClaimRun is scoped to the claiming worker's user). The
// completion_interlock_rollout row is instance-global, so every case resets it up front and in
// t.Cleanup (resetInterlockSetting) and none runs in parallel.

const (
	lifecycleBranch = "agent/issue-1"
	lifecycleHead1  = "1111111111111111111111111111111111111111"
	lifecycleHead2  = "2222222222222222222222222222222222222222"
	lifecycleFinger = "worktree-fingerprint-a"
	lifecycleReason = "owner decision recorded by the lifecycle test"
)

// lifecycleMilestones builds the plan's milestone list m1..mN.
func lifecycleMilestones(n int) []Milestone {
	ms := make([]Milestone, 0, n)
	for i := 1; i <= n; i++ {
		ms = append(ms, Milestone{ID: fmt.Sprintf("m%d", i), Title: fmt.Sprintf("Milestone %d", i)})
	}
	return ms
}

// lifecycleFixture is one test's user, repo, Service and issue counter.
type lifecycleFixture struct {
	env     codexTestEnv
	svc     *Service
	userID  uuid.UUID
	repoID  uuid.UUID
	harness Harness
	nextIID int64
}

// newLifecycleFixture seeds a user whose only usable harness is the requested one and wires the production
// create seams: a real settings.Cache as the interlock reader and a transaction beginner.
func newLifecycleFixture(t *testing.T, harness Harness) *lifecycleFixture {
	t.Helper()
	env := setupCodexLiveDB(t)
	resetInterlockSetting(t, env)
	var userID, repoID uuid.UUID
	// Claim assembles a real payload, so the fixture's bot PAT (and, for Claude, a default
	// Anthropic token) must be openable by the master box; the seed helpers store placeholders.
	if harness == HarnessCodex {
		userID, repoID = seedCodexOnlyOriginUser(t, env)
		resealBotPAT(t, env, userID)
	} else {
		userID, repoID = seedClaudeOnlyOriginUser(t, env)
		enableClaimAssembly(t, env, userID)
	}
	svc := New(env.q, env.box, testParams())
	svc.SetVault(env.vlt)
	svc.SetTxBeginner(env.pool)
	svc.SetBackground(func(func()) {})
	f := &lifecycleFixture{env: env, svc: svc, userID: userID, repoID: repoID, harness: harness, nextIID: 1}
	f.freshSettings()
	return f
}

// freshSettings replaces the settings cache so the next read sees the app_settings row as it is NOW.
// The kill-switch case calls it after each flip, so a regression that read the switch at claim or
// permit time could not hide behind a stale cached value.
func (f *lifecycleFixture) freshSettings() {
	f.svc.SetCompletionInterlockSettings(settings.New(f.env.q, time.Minute))
}

// worker inserts an online worker for the fixture's user with the given protocol capabilities and
// returns the row Claim and the worker-side calls take.
func (f *lifecycleFixture) worker(t *testing.T, protocolCaps []string) store.Worker {
	t.Helper()
	if protocolCaps == nil {
		protocolCaps = []string{}
	}
	id := uuid.New()
	f.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, protocol_capabilities)
	            VALUES ($1, $2, $3, $4, 'online', $5)`, id, f.userID, "lc-"+id.String()[:8], id[:], protocolCaps)
	return wkrRow(t, f.env, id)
}

// createRun creates an issue run through Service.CreateRun (a human-gated, non-seeded run).
func (f *lifecycleFixture) createRun(t *testing.T) *lifecycleRun {
	t.Helper()
	iid := f.nextIID
	f.nextIID++
	seedEligibleIssue(t, f.env, f.repoID, iid)
	run, err := f.svc.CreateRun(f.env.ctx, f.userID, f.repoID, iid, "desc", nil, nil, false, nil, nil, nil)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	assertRunHarness(t, f.env, run.ID, f.harness)
	return &lifecycleRun{f: f, id: run.ID}
}

// lifecycleRun is one run plus the worker flight currently holding it.
type lifecycleRun struct {
	f   *lifecycleFixture
	id  uuid.UUID
	w   store.Worker
	gen int64
}

func (r *lifecycleRun) row(t *testing.T) store.Run {
	t.Helper()
	return mustRun(t, r.f.env, r.id)
}

func (r *lifecycleRun) status(t *testing.T) string { return r.row(t).Status }

func (r *lifecycleRun) genPtr() *int64 { g := r.gen; return &g }

// claim has w claim the run through Service.Claim and adopts the returned flight.
func (r *lifecycleRun) claim(t *testing.T, w store.Worker) {
	t.Helper()
	p, err := r.f.svc.Claim(r.f.env.ctx, w, nil)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if p == nil || p.RunID != r.id.String() {
		t.Fatalf("Claim = %+v, want run %s", p, r.id)
	}
	if p.ClaimGeneration <= r.gen {
		t.Fatalf("claim_generation = %d, want it above the previous flight's %d", p.ClaimGeneration, r.gen)
	}
	r.w, r.gen = w, p.ClaimGeneration
}

// mustNotClaim asserts Service.Claim hands w nothing and leaves the run queued.
func (r *lifecycleRun) mustNotClaim(t *testing.T, w store.Worker) {
	t.Helper()
	p, err := r.f.svc.Claim(r.f.env.ctx, w, nil)
	if err != nil || p != nil {
		t.Fatalf("a worker without the completion protocol must claim nothing; payload=%+v err=%v", p, err)
	}
	if got := r.status(t); got != "queued" {
		t.Fatalf("run status after the refused claim = %q, want queued", got)
	}
}

func (r *lifecycleRun) setState(t *testing.T, req StateRequest) store.Run {
	t.Helper()
	req.ClaimGeneration = r.genPtr()
	run, applied, err := r.f.svc.SetState(r.f.env.ctx, r.w, r.id, req)
	if err != nil || !applied {
		t.Fatalf("SetState %s: applied=%v err=%v", req.State, applied, err)
	}
	return run
}

func (r *lifecycleRun) consume(t *testing.T) {
	t.Helper()
	if _, err := r.f.svc.ConsumeInputs(r.f.env.ctx, r.w, r.id, r.genPtr()); err != nil {
		t.Fatalf("ConsumeInputs: %v", err)
	}
}

// planAndApprove walks the human plan gate: running, awaiting_approval with the milestone list,
// the owner's approve_plan (which freezes the contract), the worker consuming the verdict and
// reporting running again.
func (r *lifecycleRun) planAndApprove(t *testing.T, ms []Milestone) {
	t.Helper()
	r.setState(t, StateRequest{State: "running"})
	r.setState(t, StateRequest{State: "awaiting_approval", PlanMd: strPtr("# Plan\n\nFive milestones.\n"), Milestones: &ms})
	if _, err := r.f.svc.SubmitInput(r.f.env.ctx, r.f.userID, r.id, "approve_plan", "", &AgentSelection{Source: AgentSourceOwn}); err != nil {
		t.Fatalf("SubmitInput approve_plan: %v", err)
	}
	r.consume(t)
	run := r.setState(t, StateRequest{State: "running"})
	if !run.ContractRevision.Valid || run.ContractRevision.Int32 != 1 {
		t.Fatalf("post-approval running row contract_revision = %+v, want 1", run.ContractRevision)
	}
	if got := r.row(t); string(got.MilestonesFrozen) == "" || got.Status != "running" {
		t.Fatalf("after approval: status=%q frozen=%s", got.Status, got.MilestonesFrozen)
	}
}

func (r *lifecycleRun) attempt(t *testing.T, head string, declared ...string) CompletionAttemptResult {
	t.Helper()
	res, err := r.f.svc.RecordCompletionAttempt(r.f.env.ctx, r.w, r.id, CompletionAttemptRequest{
		MilestonesCompleted: declared, Head: head, WorktreeFingerprint: lifecycleFingerprintFor(head), ClaimGeneration: r.genPtr(),
	})
	if err != nil {
		t.Fatalf("RecordCompletionAttempt: %v", err)
	}
	return res
}

func lifecycleFingerprintFor(head string) string { return lifecycleFinger + "-" + head[:6] }

func (r *lifecycleRun) permit(t *testing.T, rev int, head string) CompletionPermitResult {
	t.Helper()
	res, err := r.f.svc.RequestCompletionPermit(r.f.env.ctx, r.w, r.id, CompletionPermitRequest{
		ContractRevision: rev, Branch: lifecycleBranch, Head: head, ClaimGeneration: r.genPtr(),
	})
	if err != nil {
		t.Fatalf("RequestCompletionPermit(rev %d, head %s): %v", rev, head, err)
	}
	return res
}

// complete reports `completed` for head with the flight's generation; the caller asserts the outcome.
func (r *lifecycleRun) complete(head string, gen *int64) (store.Run, bool, error) {
	return r.f.svc.SetState(r.f.env.ctx, r.w, r.id, StateRequest{
		State: "completed", Head: strPtr(head), Branch: strPtr(lifecycleBranch), ClaimGeneration: gen,
	})
}

func (r *lifecycleRun) attemptCount(t *testing.T) int {
	t.Helper()
	return int(r.row(t).CompletionAttempts)
}

// permitCounts returns (rows, consumed rows) of run_completion_permits.
func (r *lifecycleRun) permitCounts(t *testing.T) (total, consumed int) {
	t.Helper()
	if err := r.f.env.pool.QueryRow(r.f.env.ctx,
		`SELECT count(*), count(consumed_at) FROM run_completion_permits WHERE run_id = $1`, r.id).Scan(&total, &consumed); err != nil {
		t.Fatalf("count permits: %v", err)
	}
	return total, consumed
}

func (r *lifecycleRun) inputCount(t *testing.T, kind string) int {
	t.Helper()
	var n int
	if err := r.f.env.pool.QueryRow(r.f.env.ctx,
		`SELECT count(*) FROM run_user_inputs WHERE run_id = $1 AND kind = $2`, r.id, kind).Scan(&n); err != nil {
		t.Fatalf("count %s inputs: %v", kind, err)
	}
	return n
}

// rowJSON is the whole runs row as text, for "nothing changed" comparisons.
func (r *lifecycleRun) rowJSON(t *testing.T) string {
	t.Helper()
	var s string
	if err := r.f.env.pool.QueryRow(r.f.env.ctx, `SELECT to_jsonb(runs)::text FROM runs WHERE id = $1`, r.id).Scan(&s); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	return s
}

func (r *lifecycleRun) stopKind(t *testing.T) (string, bool) {
	t.Helper()
	sk := r.row(t).StopKind
	return sk.String, sk.Valid
}

func (r *lifecycleRun) contract(t *testing.T) completionContract {
	t.Helper()
	c, err := parseCompletionContract(r.row(t).CompletionContract)
	if err != nil {
		t.Fatalf("parse completion contract: %v", err)
	}
	return c
}

func (r *lifecycleRun) completedIDs(t *testing.T) []string {
	t.Helper()
	ids, err := DecodeMilestoneIDs(r.row(t).MilestonesCompleted)
	if err != nil {
		t.Fatalf("decode milestones_completed: %v", err)
	}
	slices.Sort(ids)
	return ids
}

func lifecycleWantIDs(t *testing.T, got []string, want ...string) {
	t.Helper()
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

// reachFrozenRunning is the shared opening of cases (a) to (c) and (f): a default-on interlocked
// run, an incapable worker that cannot claim it, a capable worker that does, and the five-milestone
// plan approved so m1..m5 are frozen.
func (f *lifecycleFixture) reachFrozenRunning(t *testing.T, incapableCaps, capableCaps []string, milestones int) *lifecycleRun {
	t.Helper()
	r := f.createRun(t)
	assertContractVersion(t, f.env, r.id, true)
	r.mustNotClaim(t, f.worker(t, incapableCaps))
	r.claim(t, f.worker(t, capableCaps))
	r.planAndApprove(t, lifecycleMilestones(milestones))
	return r
}

// reachHold is the #1220 shape from the frozen running run to the owner-facing hold: four of five
// milestones declared, the server naming the fifth, the permit denied, a bare `completed` refused,
// identical attempts, then the worker's hold at head H1. It asserts every server answer on the way.
func (r *lifecycleRun) reachHold(t *testing.T) {
	t.Helper()

	res := r.attempt(t, lifecycleHead1, "m1", "m2", "m3", "m4")
	lifecycleWantIDs(t, res.Unmet, "m5")
	if res.AttemptCount != 1 {
		t.Fatalf("first attempt count = %d, want 1", res.AttemptCount)
	}

	denied := r.permit(t, 1, lifecycleHead1)
	if denied.Granted || denied.DenyReason != CompletionDenyMissingMilestones {
		t.Fatalf("permit with m5 undone: %+v, want denied %q", denied, CompletionDenyMissingMilestones)
	}
	lifecycleWantIDs(t, denied.Unmet, "m5")
	if total, _ := r.permitCounts(t); total != 0 {
		t.Fatalf("a denied permit request wrote %d permit rows", total)
	}

	run, applied, err := r.complete(lifecycleHead1, r.genPtr())
	if err != nil {
		t.Fatalf("SetState completed without a permit: %v", err)
	}
	if applied || run.Status == "completed" || r.status(t) != "running" {
		t.Fatalf("completed without a permit must leave the run running; applied=%v returned=%q db=%q", applied, run.Status, r.status(t))
	}

	// The lead re-attempts with the same head and the same worktree: the stall the worker turns into a hold.
	for i := 0; i < 2; i++ {
		again := r.attempt(t, lifecycleHead1, "m1", "m2", "m3", "m4")
		lifecycleWantIDs(t, again.Unmet, "m5")
	}
	attempts := r.attemptCount(t)
	if attempts < 3 {
		t.Fatalf("completion_attempts = %d after identical attempts, want at least 3", attempts)
	}

	held, applied, err := r.f.svc.SetRunCompletionHold(r.f.env.ctx, r.w, r.id, lifecycleHead1, r.genPtr())
	if err != nil || !applied {
		t.Fatalf("SetRunCompletionHold: applied=%v err=%v", applied, err)
	}
	if held.Status != "paused" || !held.HoldReason.Valid || held.HoldReason.String != "completion_blocked" {
		t.Fatalf("held run = status %q hold_reason %+v, want paused/completion_blocked", held.Status, held.HoldReason)
	}
	if got := r.row(t); got.HoldCapturedHead.String != lifecycleHead1 {
		t.Fatalf("hold_captured_head = %q, want %q", got.HoldCapturedHead.String, lifecycleHead1)
	}

	// A lost ACK makes the worker repeat the hold: the server answers not-applied with the real
	// status (paused) and records nothing new.
	again, applied, err := r.f.svc.SetRunCompletionHold(r.f.env.ctx, r.w, r.id, lifecycleHead1, r.genPtr())
	if err != nil {
		t.Fatalf("repeat SetRunCompletionHold: %v", err)
	}
	if applied || again.Status != "paused" {
		t.Fatalf("repeat hold: applied=%v status=%q, want applied=false status=paused", applied, again.Status)
	}
	if got := r.attemptCount(t); got != attempts {
		t.Fatalf("repeat hold changed completion_attempts %d -> %d", attempts, got)
	}
}

// reclaimAfterDecision has the capable worker claim the requeued run again, asserts it is a NEW
// generation, and reports running so the run is live under it.
func (r *lifecycleRun) reclaimAfterDecision(t *testing.T) (oldGen int64) {
	t.Helper()
	if got := r.status(t); got != "queued" {
		t.Fatalf("status after the owner decision = %q, want queued", got)
	}
	oldGen = r.gen
	r.claim(t, r.w)
	if r.gen == oldGen {
		t.Fatalf("reclaim kept claim_generation %d", oldGen)
	}
	r.consume(t)
	r.setState(t, StateRequest{State: "running"})
	return oldGen
}

// TestCompletionLifecycleContinueLiveDB (PRD #1232 M1 (a)): #1220 end to end on the owner's
// "continue" path, including the stale-flight fences after a real reclaim.
func TestCompletionLifecycleContinueLiveDB(t *testing.T) {
	f := newLifecycleFixture(t, HarnessClaude)
	incapable := []string{}
	capable := []string{capability.CompletionInterlockV1}
	r := f.reachFrozenRunning(t, incapable, capable, 5)
	r.reachHold(t)

	// A foreign user's continue is refused and changes nothing.
	before := r.rowJSON(t)
	if _, err := f.svc.ContinueCompletionDecision(f.env.ctx, uuid.New(), r.id, ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("a foreign user's continue: err=%v, want ErrRunNotFound", err)
	}
	if after := r.rowJSON(t); after != before {
		t.Fatalf("a refused foreign continue changed the run row:\nbefore %s\nafter  %s", before, after)
	}
	if got := r.status(t); got != "paused" {
		t.Fatalf("status after the foreign continue = %q, want paused", got)
	}

	// The owner's continue requeues it.
	requeued, err := f.svc.ContinueCompletionDecision(f.env.ctx, f.userID, r.id, "m5 is still owed; finish it")
	if err != nil {
		t.Fatalf("owner's continue: %v", err)
	}
	if requeued.Status != "queued" {
		t.Fatalf("owner's continue left status %q, want queued", requeued.Status)
	}

	// Reclaim gives a new generation; the first flight's writes are all refused.
	oldGen := r.reclaimAfterDecision(t)
	attemptsBefore := r.attemptCount(t)
	stale := oldGen
	if _, err := f.svc.RecordCompletionAttempt(f.env.ctx, r.w, r.id, CompletionAttemptRequest{
		MilestonesCompleted: []string{"m5"}, Head: lifecycleHead2, WorktreeFingerprint: "stale", ClaimGeneration: &stale,
	}); !errors.Is(err, ErrCompletionStaleClaim) {
		t.Fatalf("old-generation attempt: err=%v, want ErrCompletionStaleClaim", err)
	}
	if got := r.attemptCount(t); got != attemptsBefore {
		t.Fatalf("an old-generation attempt changed completion_attempts %d -> %d", attemptsBefore, got)
	}
	lifecycleWantIDs(t, r.completedIDs(t), "m1", "m2", "m3", "m4")
	if _, err := f.svc.RequestCompletionPermit(f.env.ctx, r.w, r.id, CompletionPermitRequest{
		ContractRevision: 1, Branch: lifecycleBranch, Head: lifecycleHead2, ClaimGeneration: &stale,
	}); !errors.Is(err, ErrCompletionStaleClaim) {
		t.Fatalf("old-generation permit: err=%v, want ErrCompletionStaleClaim", err)
	}
	if total, _ := r.permitCounts(t); total != 0 {
		t.Fatalf("an old-generation permit request wrote %d permit rows", total)
	}
	if _, applied, err := f.svc.SetState(f.env.ctx, r.w, r.id, StateRequest{State: "running", ClaimGeneration: &stale}); !errors.Is(err, ErrStaleClaim) || applied {
		t.Fatalf("old-generation state write: applied=%v err=%v, want ErrStaleClaim", applied, err)
	}

	// The new flight declares m5: the server clears it, grants the permit at H2 and re-serves it.
	res := r.attempt(t, lifecycleHead2, "m5")
	lifecycleWantIDs(t, res.Unmet)
	granted := r.permit(t, 1, lifecycleHead2)
	if !granted.Granted || granted.Permit == nil || granted.Permit.Head != lifecycleHead2 || granted.Permit.ContractRevision != 1 || granted.Permit.Branch != lifecycleBranch {
		t.Fatalf("permit at H2: %+v, want granted for (rev 1, %s, %s)", granted, lifecycleBranch, lifecycleHead2)
	}
	repeat := r.permit(t, 1, lifecycleHead2)
	if !repeat.Granted || repeat.Permit == nil || repeat.Permit.ID != granted.Permit.ID {
		t.Fatalf("repeat permit request: %+v, want the same permit id %s", repeat, granted.Permit.ID)
	}
	if total, consumed := r.permitCounts(t); total != 1 || consumed != 0 {
		t.Fatalf("permit rows = %d (consumed %d), want exactly 1 unconsumed", total, consumed)
	}

	// The superseded flight cannot spend the new flight's permit.
	if _, applied, err := r.complete(lifecycleHead2, &stale); !errors.Is(err, ErrStaleClaim) || applied {
		t.Fatalf("old-generation completed: applied=%v err=%v, want ErrStaleClaim", applied, err)
	}
	if got := r.status(t); got != "running" {
		t.Fatalf("status after the refused old-generation completed = %q, want running", got)
	}

	done, applied, err := r.complete(lifecycleHead2, r.genPtr())
	if err != nil || !applied || done.Status != "completed" {
		t.Fatalf("completed at H2: applied=%v status=%q err=%v", applied, done.Status, err)
	}
	if total, consumed := r.permitCounts(t); total != 1 || consumed != 1 {
		t.Fatalf("after completion: permit rows=%d consumed=%d, want 1 and 1", total, consumed)
	}
	if sk, valid := r.stopKind(t); valid {
		t.Fatalf("a full-scope completion must leave stop_kind NULL; got %q", sk)
	}
	lifecycleWantIDs(t, r.completedIDs(t), "m1", "m2", "m3", "m4", "m5")

	// A replayed `completed` (lost response) succeeds and changes nothing.
	settled := r.rowJSON(t)
	replay, applied, err := r.complete(lifecycleHead2, r.genPtr())
	if err != nil || !applied || replay.Status != "completed" {
		t.Fatalf("replayed completed: applied=%v status=%q err=%v", applied, replay.Status, err)
	}
	if after := r.rowJSON(t); after != settled {
		t.Fatalf("a replayed completed changed the run row:\nbefore %s\nafter  %s", settled, after)
	}
	if total, consumed := r.permitCounts(t); total != 1 || consumed != 1 {
		t.Fatalf("after the replay: permit rows=%d consumed=%d, want 1 and 1", total, consumed)
	}
}

// TestCompletionLifecyclePartialLiveDB (PRD #1232 M1 (b)): the owner keeps m1..m4 and defers m5.
func TestCompletionLifecyclePartialLiveDB(t *testing.T) {
	f := newLifecycleFixture(t, HarnessClaude)
	r := f.reachFrozenRunning(t, []string{}, []string{capability.CompletionInterlockV1}, 5)
	r.reachHold(t)

	decided, err := f.svc.DecideCompletion(f.env.ctx, f.userID, r.id, CompletionDecisionInput{
		Decision: "partial", Keep: []string{"m1", "m2", "m3", "m4"}, Reason: lifecycleReason, ContractRevision: 1,
	})
	if err != nil {
		t.Fatalf("DecideCompletion partial: %v", err)
	}
	if decided.Status != "queued" {
		t.Fatalf("status after the partial decision = %q, want queued", decided.Status)
	}
	if got := r.row(t).ContractRevision; !got.Valid || got.Int32 != 2 {
		t.Fatalf("contract_revision = %+v, want 2", got)
	}

	r.reclaimAfterDecision(t)
	// The attempt declares nothing new; the deferred m5 is no longer unmet.
	res := r.attempt(t, lifecycleHead1)
	lifecycleWantIDs(t, res.Unmet)

	// The revision-1 permit is gone; the revision-2 permit is granted without m5 and names it as deferred.
	if stale := r.permit(t, 1, lifecycleHead1); stale.Granted || stale.DenyReason != CompletionDenyRevisionDrift {
		t.Fatalf("permit at the superseded revision 1: %+v, want denied %q", stale, CompletionDenyRevisionDrift)
	}
	granted := r.permit(t, 2, lifecycleHead1)
	if !granted.Granted || granted.Permit == nil || granted.Permit.ContractRevision != 2 || granted.Permit.Head != lifecycleHead1 {
		t.Fatalf("revision-2 permit: %+v, want granted at (rev 2, %s)", granted, lifecycleHead1)
	}
	scope := granted.Permit.CompletionScope
	if scope == nil || len(scope.Deferred) != 1 || scope.Deferred[0].MilestoneID != "m5" || scope.Deferred[0].Reason != lifecycleReason || len(scope.Accepted) != 0 {
		t.Fatalf("completion_scope = %+v, want exactly m5 deferred with the owner's reason and nothing accepted", scope)
	}

	done, applied, err := r.complete(lifecycleHead1, r.genPtr())
	if err != nil || !applied || done.Status != "completed" {
		t.Fatalf("completed: applied=%v status=%q err=%v", applied, done.Status, err)
	}
	if sk, valid := r.stopKind(t); !valid || sk != "scope_reduced" {
		t.Fatalf("stop_kind = (%q, valid=%v), want scope_reduced", sk, valid)
	}
	lifecycleWantIDs(t, r.completedIDs(t), "m1", "m2", "m3", "m4")
}

// TestCompletionLifecycleAcceptLiveDB (PRD #1232 M1 (c)): the owner accepts m5.c1 as delivered.
// The criterion id is "<milestone id>.c1", the id buildCompletionContract mints per milestone.
func TestCompletionLifecycleAcceptLiveDB(t *testing.T) {
	f := newLifecycleFixture(t, HarnessClaude)
	r := f.reachFrozenRunning(t, []string{}, []string{capability.CompletionInterlockV1}, 5)
	r.reachHold(t)

	decided, err := f.svc.DecideCompletion(f.env.ctx, f.userID, r.id, CompletionDecisionInput{
		Decision: "accept", Criteria: []string{"m5.c1"}, Reason: lifecycleReason, ContractRevision: 1,
	})
	if err != nil {
		t.Fatalf("DecideCompletion accept: %v", err)
	}
	if decided.Status != "queued" {
		t.Fatalf("status after the accept decision = %q, want queued", decided.Status)
	}

	r.reclaimAfterDecision(t)
	// The lead still declares only m1..m4; the accepted m5.c1 is not unmet.
	res := r.attempt(t, lifecycleHead1, "m1", "m2", "m3", "m4")
	lifecycleWantIDs(t, res.Unmet)

	if stale := r.permit(t, 1, lifecycleHead1); stale.Granted || stale.DenyReason != CompletionDenyRevisionDrift {
		t.Fatalf("permit at revision 1 after the accept: %+v, want denied %q", stale, CompletionDenyRevisionDrift)
	}
	granted := r.permit(t, 2, lifecycleHead1)
	if !granted.Granted || granted.Permit == nil || granted.Permit.ContractRevision != 2 || granted.Permit.Head != lifecycleHead1 || granted.Permit.Branch != lifecycleBranch {
		t.Fatalf("revision-2 permit: %+v, want granted at exactly (rev 2, %s, %s)", granted, lifecycleBranch, lifecycleHead1)
	}
	scope := granted.Permit.CompletionScope
	if scope == nil || len(scope.Deferred) != 0 || len(scope.Accepted) != 1 ||
		scope.Accepted[0].ID != "m5.c1" || scope.Accepted[0].MilestoneID != "m5" || scope.Accepted[0].Reason != lifecycleReason {
		t.Fatalf("completion_scope = %+v, want m5.c1 accepted with the owner's reason and nothing deferred", scope)
	}

	done, applied, err := r.complete(lifecycleHead1, r.genPtr())
	if err != nil || !applied || done.Status != "completed" {
		t.Fatalf("completed: applied=%v status=%q err=%v", applied, done.Status, err)
	}
	if sk, valid := r.stopKind(t); valid {
		t.Fatalf("an accepted-criterion completion must leave stop_kind NULL (not scope_reduced); got %q", sk)
	}
	// Owner-accepted is not worker-completed: m5 never enters milestones_completed.
	lifecycleWantIDs(t, r.completedIDs(t), "m1", "m2", "m3", "m4")
	c := r.contract(t)
	if len(c.Accepted) != 1 || c.Accepted[0].ID != "m5.c1" || c.Accepted[0].Reason != lifecycleReason || c.Accepted[0].Revision != 2 {
		t.Fatalf("contract accepted block = %+v, want m5.c1 at revision 2 with the owner's reason", c.Accepted)
	}
	if c.Scope != nil && len(c.Scope.Out) != 0 {
		t.Fatalf("an accept decision deferred milestones: %+v", c.Scope.Out)
	}
}

// TestCompletionLifecycleCodexParityLiveDB (PRD #1232 M1 (d)): a Codex-harness run takes the same
// path. The old Codex worker (completion_interlock_v1 but not the Codex completion protocol) claims
// nothing; the four-capability worker claims, is denied with m5 undone, and is granted once m5 is declared.
func TestCompletionLifecycleCodexParityLiveDB(t *testing.T) {
	f := newLifecycleFixture(t, HarnessCodex)
	oldCodex := []string{capability.CompletionInterlockV1, capability.CodexHarnessV1, capability.CodexRuntimeV2}
	r := f.reachFrozenRunning(t, oldCodex, codexCompletionCaps(), 5)

	res := r.attempt(t, lifecycleHead1, "m1", "m2", "m3", "m4")
	lifecycleWantIDs(t, res.Unmet, "m5")
	denied := r.permit(t, 1, lifecycleHead1)
	if denied.Granted || denied.DenyReason != CompletionDenyMissingMilestones {
		t.Fatalf("Codex permit with m5 undone: %+v, want denied %q", denied, CompletionDenyMissingMilestones)
	}
	lifecycleWantIDs(t, denied.Unmet, "m5")
	if _, applied, err := r.complete(lifecycleHead1, r.genPtr()); err != nil || applied || r.status(t) != "running" {
		t.Fatalf("Codex completed without a permit: applied=%v err=%v status=%q, want a refused report on a running run", applied, err, r.status(t))
	}

	res = r.attempt(t, lifecycleHead2, "m5")
	lifecycleWantIDs(t, res.Unmet)
	granted := r.permit(t, 1, lifecycleHead2)
	if !granted.Granted || granted.Permit == nil || granted.Permit.Head != lifecycleHead2 || granted.Permit.ContractRevision != 1 {
		t.Fatalf("Codex permit at H2: %+v, want granted at (rev 1, %s)", granted, lifecycleHead2)
	}
	done, applied, err := r.complete(lifecycleHead2, r.genPtr())
	if err != nil || !applied || done.Status != "completed" {
		t.Fatalf("Codex completed at H2: applied=%v status=%q err=%v", applied, done.Status, err)
	}
	if total, consumed := r.permitCounts(t); total != 1 || consumed != 1 {
		t.Fatalf("Codex permit rows=%d consumed=%d, want 1 and 1", total, consumed)
	}
	if sk, valid := r.stopKind(t); valid {
		t.Fatalf("a full-scope Codex completion must leave stop_kind NULL; got %q", sk)
	}
}

// TestCompletionLifecycleKillSwitchAfterCreateLiveDB (PRD #1232 M1 (e), D3): the
// completion_interlock_rollout switch is read once, at create. Flipping it afterwards changes
// neither a stamped run's claim clause and permit requirement nor a legacy run's pass-through.
func TestCompletionLifecycleKillSwitchAfterCreateLiveDB(t *testing.T) {
	f := newLifecycleFixture(t, HarnessClaude)
	incapable := f.worker(t, nil)
	capable := f.worker(t, []string{capability.CompletionInterlockV1})

	t.Run("stamped run keeps its clause and permit requirement after the switch goes false", func(t *testing.T) {
		r := f.createRun(t)
		assertContractVersion(t, f.env, r.id, true)

		setInterlockSetting(t, f.env, "false")
		f.freshSettings()

		r.mustNotClaim(t, incapable)
		r.claim(t, capable)
		r.planAndApprove(t, lifecycleMilestones(2))

		// No permit: the report is refused and the run stays running.
		if _, applied, err := r.complete(lifecycleHead1, r.genPtr()); err != nil || applied || r.status(t) != "running" {
			t.Fatalf("completed without a permit after the switch went false: applied=%v err=%v status=%q", applied, err, r.status(t))
		}
		res := r.attempt(t, lifecycleHead1, "m1")
		lifecycleWantIDs(t, res.Unmet, "m2")
		if denied := r.permit(t, 1, lifecycleHead1); denied.Granted || denied.DenyReason != CompletionDenyMissingMilestones {
			t.Fatalf("permit with m2 undone after the switch went false: %+v, want denied %q", denied, CompletionDenyMissingMilestones)
		}
		res = r.attempt(t, lifecycleHead2, "m2")
		lifecycleWantIDs(t, res.Unmet)
		if granted := r.permit(t, 1, lifecycleHead2); !granted.Granted || granted.Permit == nil {
			t.Fatalf("permit after m2 declared: %+v, want granted", granted)
		}
		if done, applied, err := r.complete(lifecycleHead2, r.genPtr()); err != nil || !applied || done.Status != "completed" {
			t.Fatalf("permitted completed: applied=%v status=%q err=%v", applied, done.Status, err)
		}
		assertContractVersion(t, f.env, r.id, true)
		if total, consumed := r.permitCounts(t); total != 1 || consumed != 1 {
			t.Fatalf("permit rows=%d consumed=%d, want 1 and 1", total, consumed)
		}
	})

	t.Run("legacy run stays legacy after the switch goes true", func(t *testing.T) {
		// Set the switch false here so this subtest passes when run alone.
		setInterlockSetting(t, f.env, "false")
		f.freshSettings()
		r := f.createRun(t)
		assertContractVersion(t, f.env, r.id, false)

		setInterlockSetting(t, f.env, "true")
		f.freshSettings()

		// Any worker claims it, including the one with no protocol capabilities.
		r.claim(t, incapable)
		r.setState(t, StateRequest{State: "running"})
		assertContractVersion(t, f.env, r.id, false)

		// It completes with no permit, byte-for-byte the legacy path.
		done, applied, err := r.complete(lifecycleHead1, r.genPtr())
		if err != nil || !applied || done.Status != "completed" {
			t.Fatalf("legacy completed: applied=%v status=%q err=%v", applied, done.Status, err)
		}
		if total, _ := r.permitCounts(t); total != 0 {
			t.Fatalf("a legacy completion touched %d permit rows", total)
		}
		assertContractVersion(t, f.env, r.id, false)
		if r.row(t).CompletionContract != nil {
			t.Fatalf("a legacy run froze a completion contract: %s", r.row(t).CompletionContract)
		}
	})
}

// TestCompletionLifecycleAuthorizeMaintainerRefusedLiveDB (PRD #1232 M1 (f)): authorize_maintainer
// is a semantic-profile decision. On a structural held run DecideCompletion refuses it with the
// bare ErrCompletionDecisionInvalid and writes nothing. The sentinel alone does not show which
// check refused it (an empty Maintainers list yields the same one).
func TestCompletionLifecycleAuthorizeMaintainerRefusedLiveDB(t *testing.T) {
	f := newLifecycleFixture(t, HarnessClaude)
	r := f.reachFrozenRunning(t, []string{}, []string{capability.CompletionInterlockV1}, 5)
	r.reachHold(t)

	before := r.rowJSON(t)
	_, err := f.svc.DecideCompletion(f.env.ctx, f.userID, r.id, CompletionDecisionInput{
		Decision: "authorize_maintainer", Reason: lifecycleReason, ContractRevision: 1,
	})
	if !errors.Is(err, ErrCompletionDecisionInvalid) {
		t.Fatalf("authorize_maintainer on a structural held run: err=%v, want ErrCompletionDecisionInvalid", err)
	}
	if errors.Is(err, ErrCompletionRevisionConflict) {
		t.Fatalf("the refusal must be the bare decision-invalid sentinel, not the revision fence: %v", err)
	}
	if got := r.row(t).ContractRevision; !got.Valid || got.Int32 != 1 {
		t.Fatalf("contract_revision = %+v after the refused decision, want 1", got)
	}
	if n := r.inputCount(t, "completion_decision"); n != 0 {
		t.Fatalf("the refused decision wrote %d completion_decision inputs, want 0", n)
	}
	if after := r.rowJSON(t); after != before {
		var b, a map[string]json.RawMessage
		_ = json.Unmarshal([]byte(before), &b)
		_ = json.Unmarshal([]byte(after), &a)
		for k := range b {
			if string(a[k]) != string(b[k]) {
				t.Fatalf("the refused decision changed runs.%s: %s -> %s", k, b[k], a[k])
			}
		}
		t.Fatal("the refused decision changed the run row")
	}
	if got := r.status(t); got != "paused" {
		t.Fatalf("status after the refused decision = %q, want paused", got)
	}
}
