package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
)

// usage_reserve_mark_livedb_test.go pins the persisted, visible side of the weekly usage reserve
// (PRD #3522 M3): the sweeper's markReserveHolds pass, queue_reason, and the ephemeral provisioner's
// demand filter. Skipped unless UZI_TEST_DATABASE_URL is set; run via ./e2e/run-store-it.sh.

func (fx *reserveFix) mark() int64 {
	fx.t.Helper()
	n, err := fx.svc.markReserveHolds(fx.env.ctx, time.Now())
	if err != nil {
		fx.t.Fatalf("markReserveHolds: %v", err)
	}
	return n
}

func (fx *reserveFix) reasonOf(id uuid.UUID) (string, bool) {
	fx.t.Helper()
	r := mustRun(fx.t, fx.env, id)
	return r.ReserveHoldReason.String, r.ReserveHoldReason.Valid
}

// extraWorker adds a second online worker with a fresh heartbeat and the given lane.
func (fx *reserveFix) extraWorker(mode string, secret *uuid.UUID) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	fx.env.exec(`INSERT INTO workers (id, user_id, name, token_hash, status, anthropic_bind_mode, anthropic_secret_id, last_heartbeat_at)
	             VALUES ($1, $2, $3, $4, 'online', $5, $6, now())`, id, fx.userID, "w-"+id.String(), id[:], mode, secret)
	return id
}

// TestReserveMarkerWritesAndClearsOnChangeLiveDB: the pass writes the reason of a held queued run, is a
// no-op (no write, updated_at untouched) when nothing changed, rewrites it when the reading
// moves, names an unreadable credential "weekly usage unknown", and clears it when the line is
// cleared or lifted, regardless of reading freshness.
func TestReserveMarkerWritesAndClearsOnChangeLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 72)
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})
	other := fx.queueRun(reserveRun{kind: runkind.Judge}) // also on the blocked default

	updatedBefore := mustRun(t, fx.env, id).UpdatedAt
	if n := fx.mark(); n != 2 {
		t.Fatalf("first pass changed %d runs, want 2", n)
	}
	if !mustRun(t, fx.env, id).UpdatedAt.Time.Equal(updatedBefore.Time) {
		t.Fatal("a WRITING marker pass bumped updated_at, which feeds the affinity ceiling and spread grace")
	}
	for _, rid := range []uuid.UUID{id, other} {
		if got, ok := fx.reasonOf(rid); !ok || got != usagereserve.HeldReason(72, 70) {
			t.Fatalf("reason = (%q, %v), want %q", got, ok, usagereserve.HeldReason(72, 70))
		}
	}
	updated := mustRun(t, fx.env, id).UpdatedAt
	if n := fx.mark(); n != 0 {
		t.Fatalf("second pass changed %d runs, want 0 (write only on change)", n)
	}
	if !mustRun(t, fx.env, id).UpdatedAt.Time.Equal(updated.Time) {
		t.Fatal("the marker pass bumped updated_at, which feeds the affinity ceiling and spread grace")
	}

	fx.reading(tok, 95, 10, "1 minute")
	if n := fx.mark(); n != 2 {
		t.Fatalf("a moved reading changed %d runs, want 2", n)
	}
	if got, _ := fx.reasonOf(id); got != usagereserve.HeldReason(95, 70) {
		t.Fatalf("reason = %q after the reading moved", got)
	}

	fx.env.exec(`DELETE FROM anthropic_rate_limits WHERE user_secret_id = $1`, tok)
	fx.mark()
	if got, _ := fx.reasonOf(id); got != usagereserve.UnknownReason {
		t.Fatalf("reason = %q, want the unknown reason for a token with no reading", got)
	}

	// The week resets and the poller reads 3%: released.
	fx.reading(tok, 3, 10, "1 minute")
	if n := fx.mark(); n != 2 {
		t.Fatalf("release changed %d runs, want 2", n)
	}
	if _, ok := fx.reasonOf(id); ok {
		t.Fatal("marker survived a below-threshold reading")
	}
	if !mustRun(t, fx.env, id).UpdatedAt.Time.Equal(updatedBefore.Time) {
		t.Fatal("a CLEARING marker pass bumped updated_at")
	}

	// Clearing the threshold drops markers even when the reading is stale (a poller outage).
	fx.reading(tok, 95, 10, "3 hours")
	fx.mark()
	if got, _ := fx.reasonOf(id); got != usagereserve.UnknownReason {
		t.Fatalf("stale reading: reason = %q, want unknown", got)
	}
	fx.setReserve(0, true)
	if n := fx.mark(); n != 2 {
		t.Fatalf("clearing the threshold dropped %d markers, want 2", n)
	}
	if _, ok := fx.reasonOf(id); ok {
		t.Fatal("marker survived a cleared threshold")
	}
}

// TestReserveMarkerIgnoresExemptAndClaimedRunsLiveDB: a started run with the toggle off, an overridden
// run and chat-free queues are never marked; a claim clears the marker; the write is guarded on
// status = 'queued'.
func TestReserveMarkerIgnoresExemptAndClaimedRunsLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, false)
	fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	started := fx.queueRun(reserveRun{firstStarted: true})
	overridden := fx.queueRun(reserveRun{exemptFor: time.Hour})
	fresh := fx.queueRun(reserveRun{})
	if n := fx.mark(); n != 1 {
		t.Fatalf("pass changed %d runs, want only the never-started run", n)
	}
	for _, rid := range []uuid.UUID{started, overridden} {
		if _, ok := fx.reasonOf(rid); ok {
			t.Fatalf("exempt run %s was marked", rid)
		}
	}
	if _, ok := fx.reasonOf(fresh); !ok {
		t.Fatal("the never-started run was not marked")
	}
	// A late write on a run that left queued is a no-op.
	fx.env.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, fresh)
	n, err := fx.env.q.SetRunReserveHoldReason(fx.env.ctx, store.SetRunReserveHoldReasonParams{ID: fresh, Reason: pgtype.Text{String: "x", Valid: true}})
	if err != nil || n != 0 {
		t.Fatalf("SetRunReserveHoldReason on a running run = (%d, %v), want 0 rows", n, err)
	}
	// ClaimRun clears the marker of a run that was marked and then claimed (reading dropped).
	fx.env.exec(`UPDATE runs SET status = 'queued' WHERE id = $1`, fresh)
	fx.env.exec(`UPDATE anthropic_rate_limits SET seven_day_pct = 1 WHERE user_id = $1`, fx.userID)
	fx.env.exec(`DELETE FROM runs WHERE id = ANY($1)`, []uuid.UUID{started, overridden})
	if p := fx.claim(); p == nil {
		t.Fatal("the marked run was not claimed after the reading dropped")
	}
	if _, ok := fx.reasonOf(fresh); ok {
		t.Fatal("a claimed run kept its reserve marker")
	}
}

// TestReserveMarkerAffinityAndLanesLiveDB: a queued run is marked only when EVERY lane that could
// claim it is blocked: a pinned run follows its worker, an unpinned one any worker, and a fresh
// ephemeral worker's lane (auto with a pooled token, else default) counts too.
func TestReserveMarkerAffinityAndLanesLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 85) // blocked
	okTok := fx.token("ok", false, false, 5)
	fx.bindWorker(BindModeDefault, nil) // W1: blocked default
	w2 := fx.extraWorker(BindModePinned, &okTok)

	onW1 := fx.queueRun(reserveRun{workerPin: &fx.workerID})
	onW2 := fx.queueRun(reserveRun{workerPin: &w2})
	free := fx.queueRun(reserveRun{})
	fx.mark()
	if _, ok := fx.reasonOf(onW1); !ok {
		t.Fatal("a run pinned to the blocked worker must be marked (only that worker may claim it)")
	}
	if _, ok := fx.reasonOf(onW2); ok {
		t.Fatal("a run pinned to the worker with an ok token must not be marked")
	}
	if _, ok := fx.reasonOf(free); ok {
		t.Fatal("an unpinned run that W2 can claim must not be marked")
	}

	// Both workers blocked, nothing pooled: the ephemeral `default` lane is blocked too -> marked.
	fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'default', anthropic_secret_id = NULL WHERE id = $1`, w2)
	fx.mark()
	if _, ok := fx.reasonOf(free); !ok {
		t.Fatal("an unpinned run blocked on every worker and on the ephemeral default lane must be marked")
	}
	// A pooled token below the threshold gives the ephemeral worker an `auto` lane that can serve it.
	pool := fx.token("pool", false, true, 10)
	fx.mark()
	if _, ok := fx.reasonOf(free); ok {
		t.Fatal("an ephemeral auto lane with a permitted pooled token can serve the run: no marker")
	}
	if _, ok := fx.reasonOf(onW1); !ok {
		t.Fatal("the run pinned to the blocked default worker stays marked")
	}
	_ = pool
}

// TestReserveQueueReasonPrecedenceLiveDB: queue_reason reads weekly_reserve for a marked queued run,
// maintenance wins when the intake pause also holds it, and a non-queued or unmarked run reads none.
func TestReserveQueueReasonPrecedenceLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})
	plain := fx.queueRun(reserveRun{exemptFor: time.Hour})
	fx.mark()

	reasons := func() map[uuid.UUID]string {
		t.Helper()
		rows, err := fx.env.q.ListRunQueueReasons(fx.env.ctx, []uuid.UUID{id, plain})
		if err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]string{}
		for _, r := range rows {
			out[r.RunID] = r.QueueReason
		}
		return out
	}
	got := reasons()
	if got[id] != "weekly_reserve" || len(got) != 1 {
		t.Fatalf("queue reasons = %v, want only %s=weekly_reserve", got, id)
	}

	setPause := func(active bool) {
		t.Helper()
		tx, err := fx.env.pool.Begin(fx.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.SetInstanceIntakePause(fx.env.ctx, store.New(tx), active, "m3 test", pgtype.UUID{}, "test"); err != nil {
			_ = tx.Rollback(fx.env.ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(fx.env.ctx); err != nil {
			t.Fatal(err)
		}
	}
	setPause(true)
	t.Cleanup(func() { setPause(false) })
	got = reasons()
	if got[id] != "maintenance" || got[plain] != "maintenance" {
		t.Fatalf("queue reasons under the pause = %v, want maintenance to win for both", got)
	}
	setPause(false)
	fx.env.exec(`UPDATE runs SET status = 'running' WHERE id = $1`, id)
	if got = reasons(); len(got) != 0 {
		t.Fatalf("queue reasons for a running run = %v, want none", got)
	}
}

// TestReserveEphemeralDemandFiltersHeldRunsLiveDB: the unplaceable-run trigger query drops a run the
// reserve marks but still lists an exempt run in the same position.
func TestReserveEphemeralDemandFiltersHeldRunsLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, false)
	ctx := fx.env.ctx
	fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	fx.env.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, fx.userID)
	fresh := fx.queueRun(reserveRun{})
	exempt := fx.queueRun(reserveRun{firstStarted: true}) // started + toggle off: not gated
	for _, id := range []uuid.UUID{fresh, exempt} {
		// Needs a capability no worker of the owner has: unplaceable, so ephemeral demand exists.
		fx.env.exec(`UPDATE runs SET required_capabilities = ARRAY['docker'], status_since = now() - interval '1 hour' WHERE id = $1`, id)
	}
	listed := func() map[uuid.UUID]bool {
		t.Helper()
		now := time.Now()
		cutoff := pgtype.Timestamptz{Time: now.Add(time.Hour), Valid: true}
		rows, err := fx.env.q.ListUnplaceableQueuedRunsForEphemeral(ctx, store.ListUnplaceableQueuedRunsForEphemeralParams{
			CrossCheckEvaluatedAt: cutoff, CrossCheckAffinityCutoff: cutoff, BackgroundGraceCutoff: cutoff,
			CodexCuratedModels: codexCuratedModelsSlice(), DockerRepoAllowlist: []uuid.UUID{}, EphemeralLease: pgtype.Interval{Valid: true},
			MaxPerUser: 1000, MaxRows: 10000,
		})
		if err != nil {
			t.Fatal(err)
		}
		out := map[uuid.UUID]bool{}
		for _, r := range rows {
			out[r.ID] = true
		}
		return out
	}
	if l := listed(); !l[fresh] || !l[exempt] {
		t.Fatalf("before marking, demand = %v, want both runs", l)
	}
	fx.mark()
	l := listed()
	if l[fresh] {
		t.Fatal("a reserve-held run still drives ephemeral provisioning")
	}
	if !l[exempt] {
		t.Fatal("an exempt run (started, toggle off) must still drive ephemeral provisioning")
	}
}

// TestReserveSweepRunsMarkerPassLiveDB: Sweep wires the pass in and reports its count.
func TestReserveSweepRunsMarkerPassLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})
	res, err := fx.svc.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if res.ReserveMarked < 1 {
		t.Fatalf("ReserveMarked = %d, want >= 1", res.ReserveMarked)
	}
	if got, ok := fx.reasonOf(id); !ok || got != usagereserve.HeldReason(90, 70) {
		t.Fatalf("reason = (%q, %v)", got, ok)
	}
}

// TestReserveHealthRowCarriesMarkerLiveDB: ListActiveRunsForHealth projects the marker only for a
// queued run, and healthTargetFor reports it immediately for a young run.
func TestReserveHealthRowCarriesMarkerLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})
	fx.mark()
	row := healthRowForRun(t, fx.env, id)
	want := usagereserve.HeldReason(90, 70)
	if row.ReserveHoldReason != want {
		t.Fatalf("health row marker = %q, want %q", row.ReserveHoldReason, want)
	}
	row.StatusSince = pgtype.Timestamptz{Time: time.Now(), Valid: true} // a brand-new queued run
	th := thWithLimit(testCustodyLimit)
	th.queued = 10 * time.Minute // the age gate a young run normally waits behind
	health, reason := fx.svc.healthTargetFor(fx.env.ctx, time.Now(), row, th)
	if health != healthWaitingWorker || reason != want {
		t.Fatalf("healthTargetFor = (%q, %q), want waiting_worker with the reserve reason from the first tick", health, reason)
	}
}

// TestReserveClaudeCheckerHeldStaysQueuedLiveDB: a Claude cross_check checker is gated on its OWN
// (opposite-harness) credential. While held it stays queued and its check stays pending (it
// settles through its deadline like any unclaimed check, driven below through the existing expiry
// path); an assembly-time hold (driven with a fresh snapshot read, not a production race) requeues
// it instead of failing it checker_unavailable.
func TestReserveClaudeCheckerHeldStaysQueuedLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModePinned)
	fx.env.exec(`UPDATE users SET weekly_reserve_pct = 70, weekly_reserve_pause_running = true WHERE id = $1`, fx.userID)
	fx.svc.p.UsagePollInterval = 5 * time.Minute
	reading := func(pct int) {
		fx.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
		                 seven_day_pct, seven_day_resets_at, source, synced_at)
		             VALUES ($1, $2, 10, now() + interval '1 hour', $3, now() + interval '48 hours', 'usage_endpoint', now() - interval '1 minute')
		             ON CONFLICT (user_secret_id) DO UPDATE SET seven_day_pct = EXCLUDED.seven_day_pct, synced_at = EXCLUDED.synced_at`,
			fx.tok, fx.userID, pct)
	}

	reading(80)
	if p := fx.claim(t); p != nil {
		t.Fatal("a checker on a credential above the threshold was claimed")
	}
	if child := mustRun(t, fx.env, fx.kid); child.Status != "queued" || child.FailureReason.Valid || child.ClaimGeneration != 0 {
		t.Fatalf("held checker: status=%s reason=%v gen=%d, want an untouched queued child", child.Status, child.FailureReason, child.ClaimGeneration)
	}
	var verdict string
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT verdict FROM cross_checks WHERE lead_run_id = $1`, fx.lead).Scan(&verdict); err != nil || verdict != "pending" {
		t.Fatalf("check verdict = %q (%v), want pending: a held checker waits for its deadline, it is not decided", verdict, err)
	}

	// Requeue mechanics of a hold found at assembly, driven with a fresh snapshot read (bare ctx):
	// it requeues, never decides the check.
	reading(10)
	w := wkrRow(t, fx.env, fx.workerID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, claimRunParamsForChecker(w))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	reading(85)
	if _, err := fx.svc.assembleAndFinishRunClaim(fx.env.ctx, w, run, false); err != nil {
		t.Fatalf("assembleAndFinishRunClaim: %v", err)
	}
	child := mustRun(t, fx.env, fx.kid)
	if child.Status != "queued" || child.FailOrigin.Valid || child.ReserveHoldReason.String != usagereserve.HeldReason(85, 70) {
		t.Fatalf("status=%s origin=%v reason=%q, want a requeued checker with the reserve reason", child.Status, child.FailOrigin, child.ReserveHoldReason.String)
	}
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT verdict FROM cross_checks WHERE lead_run_id = $1`, fx.lead).Scan(&verdict); err != nil || verdict != "pending" {
		t.Fatalf("check verdict = %q (%v), want pending", verdict, err)
	}

	// Below the threshold the same checker is admitted.
	reading(10)
	p := fx.claim(t)
	child = mustRun(t, fx.env, fx.kid)
	if p == nil || p.Secrets.AnthropicOAuthToken == "" || child.Status != "claimed" || child.ReserveHoldReason.Valid {
		t.Fatalf("below the threshold the checker must be claimed with its credential and a cleared marker: payload=%v status=%s marker=%v",
			p != nil, child.Status, child.ReserveHoldReason)
	}
}

// TestReserveClaudeCheckerHeldExpiresThroughDeadlineLiveDB: a marked, held Claude checker never
// decides its check by itself; when the check's deadline passes the existing expiry path
// (PlanCrossCheckStatus) settles it failed/timed_out and cancels the still-queued child.
func TestReserveClaudeCheckerHeldExpiresThroughDeadlineLiveDB(t *testing.T) {
	fx := newClaudeCheckerFix(t, BindModePinned)
	fx.env.exec(`UPDATE users SET weekly_reserve_pct = 70, weekly_reserve_pause_running = true WHERE id = $1`, fx.userID)
	fx.svc.p.UsagePollInterval = 5 * time.Minute
	fx.svc.ephemeralSettings = fakeEphemeralSettings{on: false} // no burst lane: the pinned worker is the only lane
	// The lead's own worker row is a registered lane too: pin it to the same blocked credential.
	fx.env.exec(`UPDATE workers SET anthropic_bind_mode = 'pinned', anthropic_secret_id = $2 WHERE user_id = $1`, fx.userID, fx.tok)
	fx.env.exec(`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, five_hour_resets_at,
	                 seven_day_pct, seven_day_resets_at, source, synced_at)
	             VALUES ($1, $2, 10, now() + interval '1 hour', 85, now() + interval '48 hours', 'usage_endpoint', now() - interval '1 minute')`,
		fx.tok, fx.userID)
	if p := fx.claim(t); p != nil {
		t.Fatal("a checker on a credential above the threshold was claimed")
	}
	if _, err := fx.svc.markReserveHolds(fx.env.ctx, time.Now()); err != nil {
		t.Fatalf("markReserveHolds: %v", err)
	}
	if child := mustRun(t, fx.env, fx.kid); child.Status != "queued" || child.ReserveHoldReason.String != usagereserve.HeldReason(85, 70) {
		t.Fatalf("held checker: status=%s marker=%q, want queued with the reserve reason", child.Status, child.ReserveHoldReason.String)
	}

	fx.env.exec(`UPDATE cross_checks SET deadline_at = now() - interval '1 second' WHERE lead_run_id = $1`, fx.lead)
	leadWorker := mustRun(t, fx.env, fx.lead).WorkerID.Bytes
	cc, _, err := fx.svc.PlanCrossCheckStatus(fx.env.ctx, store.Worker{ID: leadWorker, UserID: fx.userID}, fx.lead, 1, 1)
	if err != nil || cc.Verdict != "failed" || cc.ReasonClass.String != "timed_out" {
		t.Fatalf("expiry: verdict=%s reason=%s err=%v, want failed/timed_out", cc.Verdict, cc.ReasonClass.String, err)
	}
	if child := mustRun(t, fx.env, fx.kid); child.Status != "cancelled" {
		t.Fatalf("expired held checker status = %s, want cancelled", child.Status)
	}
}

// TestReserveQueueReasonBoardJobAndMaintenanceReadsLiveDB: a marked queued run reads queue_reason
// weekly_reserve on a board read (ListLatestRunsForRepo) and a job read (GetJobForCaller), and a
// marked requeued run is listed by ListMaintenanceHeldRuns as progressing (it is admitted during the
// intake pause, not an owner-needed hold).
func TestReserveQueueReasonBoardJobAndMaintenanceReadsLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	fx.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, fx.workerID, []string{capability.JobRunnerV1})
	issue := fx.queueRun(reserveRun{})
	job := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, status, budget_wall_seconds)
	             VALUES ($1, $2, 'job', 'research', 't', 'd', 'queued', 600)`, job, fx.userID)
	fx.env.exec(`INSERT INTO job_origins (run_id) VALUES ($1)`, job)
	requeued := fx.queueRun(reserveRun{})
	fx.env.exec(`UPDATE runs SET claim_generation = 1 WHERE id = $1`, requeued)
	fx.mark()

	board, err := fx.env.q.ListLatestRunsForRepo(fx.env.ctx, fx.repoID)
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, r := range board {
		if r.ID == issue {
			seen = true
			if r.QueueReason != "weekly_reserve" {
				t.Fatalf("board queue_reason = %q, want weekly_reserve", r.QueueReason)
			}
		}
	}
	if !seen {
		t.Fatal("the marked issue run is missing from the board read")
	}
	j, err := fx.env.q.GetJobForCaller(fx.env.ctx, store.GetJobForCallerParams{RunID: job, UserID: fx.userID})
	if err != nil || j.QueueReason != "weekly_reserve" {
		t.Fatalf("job queue_reason = %q (%v), want weekly_reserve", j.QueueReason, err)
	}

	tx, err := fx.env.pool.Begin(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.SetInstanceIntakePause(fx.env.ctx, store.New(tx), true, "m3 test", pgtype.UUID{}, "test"); err != nil {
		_ = tx.Rollback(fx.env.ctx)
		t.Fatal(err)
	}
	if err := tx.Commit(fx.env.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tx, err := fx.env.pool.Begin(context.Background())
		if err != nil {
			return
		}
		if _, _, err := store.SetInstanceIntakePause(context.Background(), store.New(tx), false, "m3 test", pgtype.UUID{}, "test"); err != nil {
			_ = tx.Rollback(context.Background())
			return
		}
		_ = tx.Commit(context.Background())
	})
	held, err := fx.env.q.ListMaintenanceHeldRuns(fx.env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range held {
		if h.RunID == requeued {
			if h.Tag != "progressing" {
				t.Fatalf("reserve-marked requeued run tag = %q, want progressing", h.Tag)
			}
			return
		}
	}
	t.Fatal("the marked requeued run is not listed by ListMaintenanceHeldRuns")
}

// claimRunParamsForChecker is ClaimRun's parameter set for a legacy checker child claimed on the run
// lane, with the reserve OFF (the test then moves the reading itself, between admission and assembly).
func claimRunParamsForChecker(w store.Worker) store.ClaimRunParams {
	now := time.Now()
	p := claimRunParamsFor(w)
	p.Lane = "run"
	p.CrossCheckEvaluatedAt = pgtype.Timestamptz{Time: now, Valid: true}
	p.CrossCheckAffinityCutoff = pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}
	p.ReserveNow = pgtype.Timestamptz{Time: now, Valid: true}
	return p
}

// TestReserveJobGatedLiveDB: a job (repo-less, never parks) resolves its credential through
// claimSecretID like a run, so the reserve gates it at admission and, when assembly's fresh snapshot
// read sees a moved reading (bare ctx: the requeue mechanics, not a production race), requeues it
// (queued, with the reason) rather than failing it or pool-waiting it.
func TestReserveJobGatedLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	tok := fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	fx.env.exec(`UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, fx.workerID, []string{capability.JobRunnerV1})
	id := uuid.New()
	fx.env.exec(`INSERT INTO runs (id, user_id, kind, job_type, issue_title, issue_description, status, budget_wall_seconds)
	             VALUES ($1, $2, 'job', 'research', 't', 'd', 'queued', 600)`, id, fx.userID)
	if p := fx.claim(); p != nil {
		t.Fatal("a job on a blocked default was claimed")
	}
	if r := mustRun(t, fx.env, id); r.Status != "queued" || r.ClaimGeneration != 0 {
		t.Fatalf("held job: status=%s gen=%d", r.Status, r.ClaimGeneration)
	}

	fx.reading(tok, 10, 10, "1 minute")
	w := wkrRow(t, fx.env, fx.workerID)
	run, err := fx.env.q.ClaimRun(fx.env.ctx, fx.claimParams(fx.workerID))
	if err != nil {
		t.Fatalf("ClaimRun: %v", err)
	}
	fx.reading(tok, 92, 10, "1 minute")
	if p, err := fx.svc.assembleAndFinishRunClaim(fx.env.ctx, w, run, false); err != nil || p != nil {
		t.Fatalf("assembleAndFinishRunClaim = (%v, %v)", p != nil, err)
	}
	r := mustRun(t, fx.env, id)
	if r.Status != "queued" || r.FailOrigin.Valid || r.ReserveHoldReason.String != usagereserve.HeldReason(92, 70) {
		t.Fatalf("status=%s origin=%v reason=%q, want a requeued job with the reserve reason", r.Status, r.FailOrigin, r.ReserveHoldReason.String)
	}
}

// TestReserveMarkerEphemeralLaneNeedsProvisionerLiveDB: the burst lane counts toward "every lane
// blocked" only when the provisioner could actually serve the run (instance kill-switch on AND the
// owner opted in). One worker pinned to a blocked token plus an unblocked owner default is held by
// ClaimRun when no burst worker can ever be provisioned, so the run must carry the marker.
func TestReserveMarkerEphemeralLaneNeedsProvisionerLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	hi := fx.token("pin-hi", false, false, 90)
	fx.token("default", true, false, 10) // unblocked: the ephemeral `default` lane could serve the run
	fx.bindWorker(BindModePinned, &hi)
	id := fx.queueRun(reserveRun{})

	fx.svc.ephemeralSettings = fakeEphemeralSettings{on: false}
	fx.mark()
	if got, ok := fx.reasonOf(id); !ok || got != usagereserve.HeldReason(90, 70) {
		t.Fatalf("kill-switch off: reason = (%q, %v), want %q (ClaimRun holds the run, nothing can serve it)", got, ok, usagereserve.HeldReason(90, 70))
	}
	if p := fx.claim(); p != nil {
		t.Fatal("control: ClaimRun must hold this run")
	}

	// Kill-switch on but the owner has not opted in: still no burst worker for them.
	fx.svc.ephemeralSettings = fakeEphemeralSettings{on: true}
	fx.mark()
	if _, ok := fx.reasonOf(id); !ok {
		t.Fatal("kill-switch on, owner not opted in: the marker must stay")
	}

	// Opted in: a fresh ephemeral worker (default lane, unblocked) can serve it, so no marker.
	fx.env.exec(`UPDATE users SET ephemeral_workers_enabled = true WHERE id = $1`, fx.userID)
	fx.mark()
	if _, ok := fx.reasonOf(id); ok {
		t.Fatal("kill-switch on and owner opted in: the ephemeral lane can serve the run, so it must not be marked")
	}
}

// TestReserveMarkerMaintenanceAffinityPinLiveDB: ClaimRun's affinity pin holds a run for a worker
// whose maintenance_phase is requested/ready/stopping/recycling even with a stale heartbeat; the
// marker must follow that pin (only the pinned, blocked worker may claim the run).
func TestReserveMarkerMaintenanceAffinityPinLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("default", true, false, 85) // blocked
	okTok := fx.token("ok", false, false, 5)
	fx.bindWorker(BindModeDefault, nil) // the pinned worker: blocked default
	fx.extraWorker(BindModePinned, &okTok)
	fx.svc.ephemeralSettings = fakeEphemeralSettings{on: false}
	fx.svc.p.WorkerHeartbeatStale = time.Minute
	fx.svc.p.WorkerAffinityCeiling = 2 * time.Hour
	fx.env.exec(`UPDATE workers SET last_heartbeat_at = now() - interval '1 hour' WHERE id = $1`, fx.workerID)
	id := fx.queueRun(reserveRun{workerPin: &fx.workerID})

	fx.mark()
	if _, ok := fx.reasonOf(id); ok {
		t.Fatal("control: a stale-heartbeat owner outside maintenance releases the pin, so the ok worker may claim")
	}
	fx.env.exec(`UPDATE workers SET maintenance_phase = 'requested' WHERE id = $1`, fx.workerID)
	fx.mark()
	if _, ok := fx.reasonOf(id); !ok {
		t.Fatal("a worker in a maintenance phase keeps the pin (ClaimRun's affinity arm): the run is claimable only on its blocked lane and must be marked")
	}
}

// TestReserveMarkerWriteIsGuardedOnTheReadValueLiveDB: SetRunReserveHoldReason writes only while the
// stored marker still equals the value the pass read, so a claim-requeue that wrote its own reason
// between the sweeper's read and write is not clobbered.
func TestReserveMarkerWriteIsGuardedOnTheReadValueLiveDB(t *testing.T) {
	fx := newReserveFix(t, 70, true)
	fx.token("only", true, false, 90)
	fx.bindWorker(BindModeDefault, nil)
	id := fx.queueRun(reserveRun{})
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
	set := func(reason, expected string) int64 {
		t.Helper()
		n, err := fx.env.q.SetRunReserveHoldReason(fx.env.ctx, store.SetRunReserveHoldReasonParams{ID: id, Reason: text(reason), Expected: text(expected)})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if set("first", "") != 1 {
		t.Fatal("a write against the value read (none) must land")
	}
	// The pass read NULL, but a requeue has since written "first": a clear computed from the old
	// read must not overwrite it.
	if set("", "") != 0 {
		t.Fatal("a write based on a stale read clobbered a newer marker")
	}
	if got, _ := fx.reasonOf(id); got != "first" {
		t.Fatalf("marker = %q, want the newer value preserved", got)
	}
	if set("", "first") != 1 {
		t.Fatal("a clear against the value read must land")
	}
}
