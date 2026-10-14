package workersvc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// ci_repair_livedb_test.go pins PRD #2865's episode transactions against a real Postgres:
// the lock order and the lock-time rechecks (configuration fingerprint, freshness, halt state,
// loop guard), admission, atomicity of the run and the episode write, and the concurrency
// cases. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres
// (./e2e/run-store-it.sh).

type crFix struct {
	env   codexTestEnv
	user  uuid.UUID
	repo  uuid.UUID
	svc   *Service
	sched store.RunSchedule
}

func newCRFix(t *testing.T) *crFix {
	t.Helper()
	env := setupCodexLiveDB(t)
	user, _, repo := env.seedCodexInfra(t)
	// Remove the fixture in dependency order: a claim opens a custody hold on its worker, which
	// would otherwise block the user delete and leave a ci_watch schedule behind for other
	// tests (the store migration round-trip test refuses leftovers).
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM recovery_custody_holds WHERE user_id = $1`,
			`DELETE FROM runs WHERE user_id = $1`,
			`DELETE FROM run_schedules WHERE user_id = $1`,
			`DELETE FROM workers WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := env.pool.Exec(context.Background(), q, user); err != nil {
				t.Errorf("fixture cleanup %q: %v", q, err)
			}
		}
	})
	anth, err := env.box.Seal([]byte(codexToken("anthropic")))
	if err != nil {
		t.Fatal(err)
	}
	env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with)
	          VALUES ($1, $2, 'anthropic_token', $3, true, $4, 'master')`, uuid.New(), user, "anthropic-"+uuid.NewString(), anth)
	// A claim assembles the bot credential, so the connection token must open.
	sealedPAT, err := env.box.Seal([]byte(codexToken("bot-pat")))
	if err != nil {
		t.Fatal(err)
	}
	env.exec(`UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, sealedPAT, user)
	svc := New(env.q, env.box, testParams())
	svc.SetTxBeginner(env.pool)
	sched, err := env.q.CreateRunSchedule(env.ctx, store.CreateRunScheduleParams{
		UserID: user, RepoID: repo, Target: "ci_watch", Prompt: pgtype.Text{String: "repair the build", Valid: true},
		Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC",
		NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true}, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create schedule: %v", err)
	}
	return &crFix{env: env, user: user, repo: repo, svc: svc, sched: sched}
}

var crBase = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func crObs(id int64, createdMin, updatedMin int) CIObservation {
	return CIObservation{RunID: id, CreatedAt: crBase.Add(time.Duration(createdMin) * time.Minute), UpdatedAt: crBase.Add(time.Duration(updatedMin) * time.Minute)}
}

// target builds the fire's view of the CURRENT configuration (the fingerprint a fire takes
// after reading the schedule and repo rows).
func (f *crFix) target(t *testing.T, key string, ob CIObservation) CIWatchTarget {
	t.Helper()
	sched, err := f.env.q.GetRunSchedule(f.env.ctx, f.sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	var branch pgtype.Text
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT default_branch FROM repos WHERE id = $1`, f.repo).Scan(&branch); err != nil {
		t.Fatal(err)
	}
	body := "repair the build"
	return CIWatchTarget{
		UserID: f.user, RepoID: f.repo, ScheduleID: f.sched.ID, WorkflowKey: key, ForgeType: "github", Observation: ob,
		Fingerprint: CIWatchFingerprint(sched, branch, body), ResolvedBody: body,
	}
}

func (f *crFix) params(t *testing.T, key string, ob CIObservation, sig string, maxAttempts int) CIRepairRunParams {
	t.Helper()
	return CIRepairRunParams{
		UserID: f.user, RepoID: f.repo, ScheduleID: f.sched.ID,
		Title: "CI repair: wf #1", Description: "repair\n```\nevidence\n```\n",
		CIConfigPaths: []string{"custom/ci/**"},
		Episode:       CIWatchStart{CIWatchTarget: f.target(t, key, ob), Signature: sig, MaxAttempts: maxAttempts},
	}
}

func (f *crFix) runCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM runs WHERE schedule_id = $1`, f.sched.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// finishRuns ends every run of the schedule so the one-active-run index admits the next.
func (f *crFix) finishRuns(t *testing.T) {
	t.Helper()
	f.env.exec(`UPDATE runs SET status = 'cancelled', finished_at = now() WHERE schedule_id = $1`, f.sched.ID)
}

func (f *crFix) episode(t *testing.T, key string) (store.CiWatchEpisode, bool) {
	t.Helper()
	ep, err := f.env.q.GetCIWatchEpisode(f.env.ctx, store.GetCIWatchEpisodeParams{ScheduleID: f.sched.ID, UserID: f.user, WorkflowKey: key})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.CiWatchEpisode{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return ep, true
}

func (f *crFix) episodeCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(*) FROM ci_watch_episodes WHERE schedule_id = $1`, f.sched.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (f *crFix) mustStart(t *testing.T, key string, ob CIObservation, sig string, maxAttempts int) store.Run {
	t.Helper()
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, f.params(t, key, ob, sig, maxAttempts))
	if err != nil {
		t.Fatalf("start %s@%d: %v", key, ob.RunID, err)
	}
	f.finishRuns(t)
	return run
}

// waitForLockWait polls pg_locks until a backend whose query matches pattern waits on an
// ungranted lock, proving the call under test is really blocked behind the holder.
func (f *crFix) waitForLockWait(t *testing.T, pattern string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := f.env.pool.QueryRow(f.env.ctx, `SELECT count(DISTINCT l.pid) FROM pg_locks l
		    JOIN pg_stat_activity a ON a.pid = l.pid
		    WHERE NOT l.granted AND a.datname = current_database() AND a.query ILIKE $1`, pattern).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no backend ever waited on a lock for a query like %q", pattern)
}

func awaitErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("call did not return after the lock holder committed")
		return nil
	}
}

func notDone(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("%s returned (%v) while the lock holder was still open", what, err)
	default:
	}
}

// TestCreateCIRepairRunStartsGuardedRunLiveDB: one atomic create writes the guarded prompt run
// (guard flag, floor-unioned paths) and the episode attempt.
func TestCreateCIRepairRunStartsGuardedRunLiveDB(t *testing.T) {
	f := newCRFix(t)
	ob := crObs(100, 0, 1)
	run, err := f.svc.CreateCIRepairRun(f.env.ctx, f.params(t, "wf-1", ob, "sig-1", 3))
	if err != nil {
		t.Fatal(err)
	}
	var kind string
	var guard bool
	var paths []string
	var sched pgtype.UUID
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT kind, ci_config_guard, ci_config_paths, schedule_id FROM runs WHERE id = $1`, run.ID).Scan(&kind, &guard, &paths, &sched); err != nil {
		t.Fatal(err)
	}
	wantPaths := append(append([]string{}, CIRepairConfigFloor...), "custom/ci/**")
	if kind != "prompt" || !guard || !reflect.DeepEqual(paths, wantPaths) || uuid.UUID(sched.Bytes) != f.sched.ID {
		t.Fatalf("run kind=%s guard=%v paths=%v schedule=%v, want a guarded prompt run with %v", kind, guard, paths, sched, wantPaths)
	}
	ep, ok := f.episode(t, "wf-1")
	if !ok || ep.Attempts != 1 || ep.LastSignature.String != "sig-1" || ep.LastHandledRunID.Int64 != 100 || ep.ObservedRunID.Int64 != 100 ||
		!ep.ObservedCreatedAt.Time.Equal(ob.CreatedAt) || !ep.ObservedUpdatedAt.Time.Equal(ob.UpdatedAt) || ep.HaltedReason.Valid {
		t.Fatalf("episode = %+v", ep)
	}

	// The unguarded CreatePromptRun is untouched: no guard, no paths.
	plain := New(f.env.q, f.env.box, testParams())
	plain.SetTxBeginner(f.env.pool)
	other, err := f.env.q.CreateRunSchedule(f.env.ctx, store.CreateRunScheduleParams{
		UserID: f.user, RepoID: f.repo, Target: "prompt", Prompt: pgtype.Text{String: "p", Valid: true}, Timing: "recurring",
		CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", Enabled: true,
		NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := plain.CreatePromptRun(f.env.ctx, f.user, f.repo, other.ID, "t", "p", false, false, nil, nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var pg bool
	var pp []string
	if err := f.env.pool.QueryRow(f.env.ctx, `SELECT ci_config_guard, ci_config_paths FROM runs WHERE id = $1`, pr.ID).Scan(&pg, &pp); err != nil {
		t.Fatal(err)
	}
	if pg || pp != nil {
		t.Fatalf("a plain prompt run carries guard=%v paths=%v", pg, pp)
	}
}

// TestCIRepairEpisodeRulesLiveDB walks the PRD's watermark, dedupe and loop-guard rules on the
// locked row.
func TestCIRepairEpisodeRulesLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.mustStart(t, "k", crObs(100, 0, 0), "S1", 3)

	// Already handled: the identical observation.
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(100, 0, 0), "S2", 3)); !errors.Is(err, ErrCIWatchAlreadyHandled) {
		t.Fatalf("identical observation: %v, want ErrCIWatchAlreadyHandled", err)
	}
	// Stale: an execution created BEFORE the watermark, whatever its id.
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(900, -10, -10), "S2", 3)); !errors.Is(err, ErrCIWatchStale) {
		t.Fatalf("older execution: %v, want ErrCIWatchStale", err)
	}
	if f.runCount(t) != 1 {
		t.Fatalf("runs = %d after refused creates, want 1", f.runCount(t))
	}

	// No progress: a fresh execution with the SAME signature halts instead of starting.
	var halted *CIWatchHaltedError
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(101, 5, 5), "S1", 3)); !errors.As(err, &halted) || halted.Reason != CIHaltNoProgress {
		t.Fatalf("same signature: %v, want a no_progress halt", err)
	}
	// Create never persists the halt; the transition does, from the LOCKED row.
	tr := CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(101, 5, 5)), HaltReason: CIHaltNoProgress, Signature: "S1", MaxAttempts: 3}
	if err := f.svc.TransitionCIWatchEpisode(ctx, tr); err != nil {
		t.Fatal(err)
	}
	ep, _ := f.episode(t, "k")
	if ep.HaltedReason.String != CIHaltNoProgress || ep.Attempts != 1 || ep.LastSignature.String != "S1" || ep.ObservedRunID.Int64 != 101 || ep.LastHandledRunID.Int64 != 101 {
		t.Fatalf("after halt episode = %+v (signature and attempts must be kept, watermark advanced)", ep)
	}
	// A halted episode refuses to start even a fresh, different execution.
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(102, 6, 6), "S9", 3)); !errors.As(err, &halted) || halted.Reason != CIHaltNoProgress {
		t.Fatalf("halted episode: %v, want the recorded halt", err)
	}

	// A halt whose guard no longer holds on the locked row is stale, not written.
	bad := CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(103, 7, 7)), HaltReason: CIHaltAttemptCap, Signature: "S1", MaxAttempts: 3}
	f.env.exec(`UPDATE ci_watch_episodes SET halted_reason = NULL WHERE schedule_id = $1`, f.sched.ID)
	if err := f.svc.TransitionCIWatchEpisode(ctx, bad); !errors.Is(err, ErrCIWatchStale) {
		t.Fatalf("halt on a row that no longer trips the guard: %v, want ErrCIWatchStale", err)
	}
	if ep, _ := f.episode(t, "k"); ep.HaltedReason.Valid || ep.ObservedRunID.Int64 != 101 {
		t.Fatalf("a refused halt wrote: %+v", ep)
	}

	// A stale green leaves the attempt count and a newer halt alone.
	rehalt := CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(104, 8, 8)), HaltReason: CIHaltNoProgress, Signature: "S1", MaxAttempts: 3}
	if err := f.svc.TransitionCIWatchEpisode(ctx, rehalt); err != nil {
		t.Fatal(err)
	}
	staleGreen := CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(50, -30, -30)), Reset: true}
	if err := f.svc.TransitionCIWatchEpisode(ctx, staleGreen); !errors.Is(err, ErrCIWatchStale) {
		t.Fatalf("stale green: %v, want ErrCIWatchStale", err)
	}
	if ep, _ := f.episode(t, "k"); ep.HaltedReason.String != CIHaltNoProgress || ep.Attempts != 1 {
		t.Fatalf("a stale green cleared the halt/attempts: %+v", ep)
	}

	// A fresh green resets the episode and advances the watermark.
	green := CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(110, 20, 20)), Reset: true}
	if err := f.svc.TransitionCIWatchEpisode(ctx, green); err != nil {
		t.Fatal(err)
	}
	ep, _ = f.episode(t, "k")
	if ep.Attempts != 0 || ep.HaltedReason.Valid || ep.LastSignature.Valid || ep.ObservedRunID.Int64 != 110 {
		t.Fatalf("after reset episode = %+v", ep)
	}
	// An identical green is already handled.
	if err := f.svc.TransitionCIWatchEpisode(ctx, green); !errors.Is(err, ErrCIWatchAlreadyHandled) {
		t.Fatalf("repeat green: %v, want ErrCIWatchAlreadyHandled", err)
	}
	// A delayed fire observing red R2 (older) after green R3 starts nothing.
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(108, 18, 18), "S1", 3)); !errors.Is(err, ErrCIWatchStale) {
		t.Fatalf("red older than the green watermark: %v, want ErrCIWatchStale", err)
	}
	// ...and does not disturb the episode.
	if ep2, _ := f.episode(t, "k"); !reflect.DeepEqual(ep, ep2) {
		t.Fatalf("episode disturbed:\n%+v\n%+v", ep, ep2)
	}
	if f.runCount(t) != 1 {
		t.Fatalf("runs = %d, want only the first", f.runCount(t))
	}
}

// TestCIRepairAttemptCapLiveDB: attempts accumulate to the cap, which then halts with
// attempt_cap even for a different signature.
func TestCIRepairAttemptCapLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.mustStart(t, "k", crObs(100, 0, 0), "A", 2)
	f.mustStart(t, "k", crObs(101, 5, 5), "B", 2)
	if ep, _ := f.episode(t, "k"); ep.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", ep.Attempts)
	}
	var halted *CIWatchHaltedError
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(102, 9, 9), "C", 2)); !errors.As(err, &halted) || halted.Reason != CIHaltAttemptCap {
		t.Fatalf("third attempt: %v, want attempt_cap", err)
	}
	// Cap wins over no-progress.
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(102, 9, 9), "B", 2)); !errors.As(err, &halted) || halted.Reason != CIHaltAttemptCap {
		t.Fatalf("same signature at the cap: %v, want attempt_cap", err)
	}
	tr := CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(102, 9, 9)), HaltReason: CIHaltAttemptCap, Signature: "C", MaxAttempts: 2}
	if err := f.svc.TransitionCIWatchEpisode(ctx, tr); err != nil {
		t.Fatal(err)
	}
	if ep, _ := f.episode(t, "k"); ep.HaltedReason.String != CIHaltAttemptCap || ep.Attempts != 2 || ep.LastSignature.String != "B" {
		t.Fatalf("episode = %+v, want the cap halt with B kept as the last signature", ep)
	}
}

// TestCIRepairRerunRevisionLiveDB: a GitHub re-run reuses the execution id. One that turns
// green resets the episode; one that stays red with a new signature is a new attempt.
func TestCIRepairRerunRevisionLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.mustStart(t, "k", crObs(100, 0, 0), "A", 5)
	// Same execution id, later update, new signature: a new attempt.
	f.mustStart(t, "k", crObs(100, 0, 30), "B", 5)
	if ep, _ := f.episode(t, "k"); ep.Attempts != 2 || ep.LastSignature.String != "B" {
		t.Fatalf("revision attempt: %+v", ep)
	}
	// Same execution id, later update again, turns green: reset.
	if err := f.svc.TransitionCIWatchEpisode(ctx, CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(100, 0, 60)), Reset: true}); err != nil {
		t.Fatal(err)
	}
	if ep, _ := f.episode(t, "k"); ep.Attempts != 0 || ep.ObservedUpdatedAt.Time.Before(crBase.Add(60*time.Minute)) {
		t.Fatalf("green re-run: %+v", ep)
	}
}

// TestCIRepairForgejoOrderingLiveDB: Forgejo compares execution ids only.
func TestCIRepairForgejoOrderingLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	mk := func(ob CIObservation, sig string) CIRepairRunParams {
		p := f.params(t, "k", ob, sig, 5)
		p.Episode.ForgeType = "forgejo"
		return p
	}
	if _, err := f.svc.CreateCIRepairRun(ctx, mk(crObs(100, 50, 50), "A")); err != nil {
		t.Fatal(err)
	}
	f.finishRuns(t)
	// Higher id with an OLDER timestamp is newer on Forgejo...
	if _, err := f.svc.CreateCIRepairRun(ctx, mk(crObs(101, -10, -10), "B")); err != nil {
		t.Fatalf("higher forgejo id: %v", err)
	}
	f.finishRuns(t)
	// ...and a lower id with a NEWER timestamp is stale.
	if _, err := f.svc.CreateCIRepairRun(ctx, mk(crObs(99, 500, 500), "C")); !errors.Is(err, ErrCIWatchStale) {
		t.Fatalf("lower forgejo id: %v, want ErrCIWatchStale", err)
	}
}

// TestCIRepairCapacityAndGreenAdmissionLiveDB: the 1024-key admission limit refuses unseen keys
// on start and halt, keeps serving existing keys, and a green never admits a key.
func TestCIRepairCapacityAndGreenAdmissionLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx

	// A green for an unseen key creates no row (so greens never use admission slots).
	if err := f.svc.TransitionCIWatchEpisode(ctx, CIWatchTransition{CIWatchTarget: f.target(t, "never-red", crObs(1, 0, 0)), Reset: true}); err != nil {
		t.Fatalf("green for an unseen key: %v", err)
	}
	if f.episodeCount(t) != 0 {
		t.Fatal("a green observation created an episode row")
	}

	f.env.exec(`INSERT INTO ci_watch_episodes (schedule_id, workflow_key) SELECT $1, 'k' || g FROM generate_series(1, 1024) g`, f.sched.ID)
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "unseen", crObs(100, 0, 0), "S", 3)); !errors.Is(err, ErrCIWatchCapacity) {
		t.Fatalf("start at capacity: %v, want ErrCIWatchCapacity", err)
	}
	halt := CIWatchTransition{CIWatchTarget: f.target(t, "unseen", crObs(100, 0, 0)), HaltReason: CIHaltAttemptCap, Signature: "S", MaxAttempts: 0}
	if err := f.svc.TransitionCIWatchEpisode(ctx, halt); !errors.Is(err, ErrCIWatchCapacity) {
		t.Fatalf("halt at capacity: %v, want ErrCIWatchCapacity", err)
	}
	if err := f.svc.TransitionCIWatchEpisode(ctx, CIWatchTransition{CIWatchTarget: f.target(t, "unseen", crObs(100, 0, 0)), Reset: true}); err != nil {
		t.Fatalf("green at capacity: %v", err)
	}
	if f.episodeCount(t) != 1024 || f.runCount(t) != 0 {
		t.Fatalf("episodes=%d runs=%d, want 1024 and 0 (nothing admitted, nothing started)", f.episodeCount(t), f.runCount(t))
	}
	// An existing key still works at capacity.
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k7", crObs(100, 0, 0), "S", 3)); err != nil {
		t.Fatalf("existing key at capacity: %v", err)
	}
}

// TestCIRepairCascadeLiveDB: deleting the schedule removes its episodes; a start for a deleted
// schedule writes nothing.
func TestCIRepairCascadeLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.mustStart(t, "k", crObs(100, 0, 0), "S", 3)
	p := f.params(t, "k2", crObs(100, 0, 0), "S", 3)
	f.env.exec(`DELETE FROM runs WHERE schedule_id = $1`, f.sched.ID)
	f.env.exec(`DELETE FROM run_schedules WHERE id = $1`, f.sched.ID)
	if f.episodeCount(t) != 0 {
		t.Fatal("episodes survived the schedule delete")
	}
	if _, err := f.svc.CreateCIRepairRun(ctx, p); !errors.Is(err, ErrCIWatchConfigChanged) {
		t.Fatalf("start for a deleted schedule: %v, want ErrCIWatchConfigChanged", err)
	}
	if err := f.svc.TransitionCIWatchEpisode(ctx, CIWatchTransition{CIWatchTarget: p.Episode.CIWatchTarget, Reset: true}); !errors.Is(err, ErrCIWatchConfigChanged) {
		t.Fatalf("reset for a deleted schedule: %v, want ErrCIWatchConfigChanged", err)
	}
	if err := f.svc.TransitionCIWatchEpisode(ctx, CIWatchTransition{CIWatchTarget: p.Episode.CIWatchTarget, HaltReason: CIHaltNoProgress, Signature: "S", MaxAttempts: 3}); !errors.Is(err, ErrCIWatchConfigChanged) {
		t.Fatalf("halt for a deleted schedule: %v, want ErrCIWatchConfigChanged", err)
	}
	var n int
	if err := f.env.pool.QueryRow(ctx, `SELECT count(*) FROM ci_watch_episodes WHERE schedule_id = $1`, f.sched.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan episodes = %d (%v)", n, err)
	}
}

// TestCIRepairHookFailureRollsBackBothLiveDB: a failure after the run INSERT, inside the
// create transaction, leaves no run and an unchanged (here: absent, and then pre-existing)
// episode.
func TestCIRepairHookFailureRollsBackBothLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	boom := errors.New("injected after insert")
	f.svc.ciRepairAfterInsert = func(uuid.UUID) error { return boom }

	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "fresh", crObs(100, 0, 0), "S", 3)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
		t.Fatalf("runs=%d episodes=%d after a rolled-back create, want 0 and 0 (the admission row rolls back too)", f.runCount(t), f.episodeCount(t))
	}

	f.svc.ciRepairAfterInsert = nil
	f.mustStart(t, "k", crObs(100, 0, 0), "S1", 5)
	before, _ := f.episode(t, "k")
	f.svc.ciRepairAfterInsert = func(uuid.UUID) error { return boom }
	if _, err := f.svc.CreateCIRepairRun(ctx, f.params(t, "k", crObs(101, 5, 5), "S2", 5)); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the injected failure", err)
	}
	after, _ := f.episode(t, "k")
	if !reflect.DeepEqual(before, after) || f.runCount(t) != 1 {
		t.Fatalf("a rolled-back create changed state: before=%+v after=%+v runs=%d", before, after, f.runCount(t))
	}
}

// TestCIRepairConcurrentFiresStartOneRunLiveDB: concurrent fires on the same red execution
// start exactly one run; the loser is "already handled" on the locked row.
func TestCIRepairConcurrentFiresStartOneRunLiveDB(t *testing.T) {
	f := newCRFix(t)
	for round := 0; round < 4; round++ {
		key := fmt.Sprintf("k%d", round)
		ob := crObs(int64(100+round), round, round)
		const n = 6
		start := make(chan struct{})
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			p := f.params(t, key, ob, "S", 3)
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				_, errs[i] = f.svc.CreateCIRepairRun(f.env.ctx, p)
			}(i)
		}
		close(start)
		wg.Wait()
		ok, handled := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrCIWatchAlreadyHandled), errors.Is(err, ErrActivePromptExists):
				handled++
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if ok != 1 || handled != n-1 {
			t.Fatalf("round %d: ok=%d handled=%d, want exactly one start", round, ok, handled)
		}
		if got := f.runCount(t); got != round+1 {
			t.Fatalf("round %d: runs=%d, want %d", round, got, round+1)
		}
		ep, _ := f.episode(t, key)
		if ep.Attempts != 1 {
			t.Fatalf("round %d: attempts=%d, want 1", round, ep.Attempts)
		}
		f.finishRuns(t)
	}
}

// TestCIRepairTransitionVsCreateLockWaitLiveDB: a halt in flight holds the schedule lock; a
// create for a fresh execution really blocks on it (observed in pg_locks) and, once the halt
// commits, sees the halt on the locked row and starts nothing.
func TestCIRepairTransitionVsCreateLockWaitLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	f.mustStart(t, "k", crObs(100, 0, 0), "S1", 5)

	tx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tr := CIWatchTransition{CIWatchTarget: f.target(t, "k", crObs(101, 5, 5)), HaltReason: CIHaltNoProgress, Signature: "S1", MaxAttempts: 5}
	if err := transitionCIWatchEpisodeLocked(ctx, store.New(tx), tr); err != nil { // holds repo(share), schedule and episode locks
		t.Fatal(err)
	}

	res := make(chan error, 1)
	p := f.params(t, "k", crObs(102, 9, 9), "S9", 5)
	go func() { _, err := f.svc.CreateCIRepairRun(ctx, p); res <- err }()
	f.waitForLockWait(t, "%FROM run_schedules WHERE id%FOR UPDATE%")
	notDone(t, res, "CreateCIRepairRun")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var halted *CIWatchHaltedError
	if err := awaitErr(t, res); !errors.As(err, &halted) || halted.Reason != CIHaltNoProgress {
		t.Fatalf("create after the halt committed: %v, want the halt", err)
	}
	if f.runCount(t) != 1 {
		t.Fatalf("runs = %d, want 1", f.runCount(t))
	}
}

// TestCIRepairConfigPatchRacingFireLiveDB: a schedule edit holds the row lock; the fire's start
// blocks behind it, then skips with ErrCIWatchConfigChanged. No run, no episode write.
func TestCIRepairConfigPatchRacingFireLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	p := f.params(t, "k", crObs(100, 0, 0), "S", 3)

	tx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE run_schedules SET auto_approve = true WHERE id = $1`, f.sched.ID); err != nil { // the PATCH
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { _, err := f.svc.CreateCIRepairRun(ctx, p); res <- err }()
	f.waitForLockWait(t, "%FROM run_schedules WHERE id%FOR UPDATE%")
	notDone(t, res, "CreateCIRepairRun")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := awaitErr(t, res); !errors.Is(err, ErrCIWatchConfigChanged) {
		t.Fatalf("create behind a PATCH: %v, want ErrCIWatchConfigChanged", err)
	}
	if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
		t.Fatalf("runs=%d episodes=%d, want nothing written", f.runCount(t), f.episodeCount(t))
	}
}

// TestCIRepairRepoLockBeforeScheduleLiveDB: the repo share lock is taken FIRST. A holder of the
// repo row's update lock blocks the create at the repo lock (before the schedule lock), and
// the default branch changing underneath is a config change.
func TestCIRepairRepoLockBeforeScheduleLiveDB(t *testing.T) {
	f := newCRFix(t)
	ctx := f.env.ctx
	p := f.params(t, "k", crObs(100, 0, 0), "S", 3)

	tx, err := f.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE repos SET default_branch = 'trunk' WHERE id = $1`, f.repo); err != nil {
		t.Fatal(err)
	}
	res := make(chan error, 1)
	go func() { _, err := f.svc.CreateCIRepairRun(ctx, p); res <- err }()
	f.waitForLockWait(t, "%default_branch FROM repos WHERE id%FOR SHARE%")
	notDone(t, res, "CreateCIRepairRun")
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := awaitErr(t, res); !errors.Is(err, ErrCIWatchConfigChanged) {
		t.Fatalf("create after a default-branch change: %v, want ErrCIWatchConfigChanged", err)
	}
	if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
		t.Fatalf("runs=%d episodes=%d, want nothing written", f.runCount(t), f.episodeCount(t))
	}
}

// TestCIRepairConfigChangeBetweenDiscoveryAndStartLiveDB: every fingerprinted field changing
// after the fire read it is refused with no run and no episode write, for start, halt and
// reset alike.
func TestCIRepairConfigChangeBetweenDiscoveryAndStartLiveDB(t *testing.T) {
	for name, change := range map[string]string{
		"selectors":     `UPDATE run_schedules SET ci_workflows = '["other"]' WHERE id = $1`,
		"auto_approve":  `UPDATE run_schedules SET auto_approve = true WHERE id = $1`,
		"guidance":      `UPDATE run_schedules SET guidance = 'new guidance' WHERE id = $1`,
		"prompt":        `UPDATE run_schedules SET prompt = 'rewritten' WHERE id = $1`,
		"wait_on_limit": `UPDATE run_schedules SET wait_on_limit = true WHERE id = $1`,
		"disabled":      `UPDATE run_schedules SET enabled = false WHERE id = $1`,
		"model":         `UPDATE run_schedules SET model = 'claude-x' WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newCRFix(t)
			ctx := f.env.ctx
			p := f.params(t, "k", crObs(100, 0, 0), "S", 3)
			f.env.exec(change, f.sched.ID)
			if _, err := f.svc.CreateCIRepairRun(ctx, p); !errors.Is(err, ErrCIWatchConfigChanged) {
				t.Fatalf("start: %v, want ErrCIWatchConfigChanged", err)
			}
			halt := CIWatchTransition{CIWatchTarget: p.Episode.CIWatchTarget, HaltReason: CIHaltAttemptCap, Signature: "S", MaxAttempts: 0}
			if err := f.svc.TransitionCIWatchEpisode(ctx, halt); !errors.Is(err, ErrCIWatchConfigChanged) {
				t.Fatalf("halt: %v, want ErrCIWatchConfigChanged", err)
			}
			if err := f.svc.TransitionCIWatchEpisode(ctx, CIWatchTransition{CIWatchTarget: p.Episode.CIWatchTarget, Reset: true}); !errors.Is(err, ErrCIWatchConfigChanged) {
				t.Fatalf("reset: %v, want ErrCIWatchConfigChanged", err)
			}
			if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
				t.Fatalf("runs=%d episodes=%d, want nothing written", f.runCount(t), f.episodeCount(t))
			}
		})
	}
}

// TestCIRepairResolvedBodyIsPartOfTheFingerprintLiveDB: a catalog body that differs from the
// one the fire resolved is a config change too.
func TestCIRepairResolvedBodyIsPartOfTheFingerprintLiveDB(t *testing.T) {
	f := newCRFix(t)
	p := f.params(t, "k", crObs(100, 0, 0), "S", 3)
	p.Episode.ResolvedBody = "a different body"
	if _, err := f.svc.CreateCIRepairRun(f.env.ctx, p); !errors.Is(err, ErrCIWatchConfigChanged) {
		t.Fatalf("err = %v, want ErrCIWatchConfigChanged", err)
	}
}

// TestCIRepairHonoursPinnedSecretLiveDB: the pinned-secret share lock still runs before the
// episode locks: a stored pin disabled since the schedule was saved starts no run and leaves no
// episode.
func TestCIRepairDisabledPinStartsNothingLiveDB(t *testing.T) {
	f := newCRFix(t)
	secret := uuid.New()
	anth, err := f.env.box.Seal([]byte(codexToken("pinned")))
	if err != nil {
		t.Fatal(err)
	}
	f.env.exec(`INSERT INTO user_secrets (id, user_id, kind, label, is_default, ciphertext, sealed_with, disabled_at)
	            VALUES ($1, $2, 'anthropic_token', $3, false, $4, 'master', now())`, secret, f.user, "pin-"+uuid.NewString(), anth)
	p := f.params(t, "k", crObs(100, 0, 0), "S", 3)
	p.CredOverride = &CredentialOverride{Mode: CredentialOverrideModePinned, SecretID: &secret}
	if _, err := f.svc.CreateCIRepairRun(f.env.ctx, p); !errors.Is(err, ErrCredentialDisabled) {
		t.Fatalf("err = %v, want ErrCredentialDisabled", err)
	}
	if f.runCount(t) != 0 || f.episodeCount(t) != 0 {
		t.Fatalf("runs=%d episodes=%d, want nothing written", f.runCount(t), f.episodeCount(t))
	}
}
