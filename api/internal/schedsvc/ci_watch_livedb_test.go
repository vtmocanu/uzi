package schedsvc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// ci_watch_livedb_test.go drives the whole ci_watch fire (PRD #2865) against a real Postgres
// with a fake forge: the real workersvc seam, the real locks and rechecks. Skipped unless
// UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh).

type ciLive struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	q     *store.Queries
	user  uuid.UUID
	repo  uuid.UUID
	sched store.RunSchedule
	cf    *ciForge
	sc    *Scheduler
}

func newCILive(t *testing.T, runs ...forge.WorkflowRun) *ciLive {
	t.Helper()
	ctx, pool, q, box := openScheduleFireLiveDB(t)
	user, repo := scheduleFireUser(ctx, t, pool)
	t.Cleanup(func() {
		for _, q := range []string{
			`DELETE FROM runs WHERE user_id = $1`,
			`DELETE FROM run_schedules WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := pool.Exec(context.Background(), q, user); err != nil {
				t.Errorf("fixture cleanup %q: %v", q, err)
			}
		}
	})
	seedAnthropicDefault(ctx, t, pool, box, user)
	sealed, err := box.Seal([]byte("bot-pat-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forge_connections SET token_ciphertext = $1 WHERE user_id = $2`, sealed, user); err != nil {
		t.Fatal(err)
	}
	svc := workersvc.New(q, box, workersvc.Params{})
	svc.SetTxBeginner(pool)
	cf := &ciForge{runs: runs, jobs: map[int64][]forge.Job{}, tails: map[int64]string{}}
	rebuildJobs := func() {
		for _, r := range cf.runs {
			for id, js := range ciFailedJobs(r.ID) {
				cf.jobs[id] = js
				cf.tails[js[0].ID] = "--- FAIL: TestNightly\nboom " + r.WebURL
			}
		}
	}
	rebuildJobs()
	sched, err := q.CreateRunSchedule(ctx, store.CreateRunScheduleParams{
		UserID: user, RepoID: repo, Target: "ci_watch", Prompt: pgtype.Text{String: "repair the build", Valid: true},
		Timing: "recurring", CronExpr: pgtype.Text{String: "0 * * * *", Valid: true}, Timezone: "UTC", Enabled: true,
		NextFireAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := New(q, svc, &ciBuilder{f: cf}, nil, nil, nil, time.Minute, nil)
	return &ciLive{ctx: ctx, pool: pool, q: q, user: user, repo: repo, sched: sched, cf: cf, sc: sc}
}

func (l *ciLive) setRuns(runs ...forge.WorkflowRun) {
	l.cf.runs = runs
	for _, r := range runs {
		for id, js := range ciFailedJobs(r.ID) {
			l.cf.jobs[id] = js
			l.cf.tails[js[0].ID] = "--- FAIL: TestNightly\nboom " + r.WebURL
		}
	}
}

// fire reads the schedule row fresh (as the tick's claim does) and fires it.
func (l *ciLive) fire(t *testing.T) FireOutcome {
	t.Helper()
	sched, err := l.q.GetRunSchedule(l.ctx, l.sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := l.sc.fireOne(l.ctx, sched)
	if err != nil {
		t.Fatalf("fireOne: %v", err)
	}
	return out
}

func (l *ciLive) runCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := l.pool.QueryRow(l.ctx, `SELECT count(*) FROM runs WHERE schedule_id = $1`, l.sched.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (l *ciLive) episodes(t *testing.T) int {
	t.Helper()
	var n int
	if err := l.pool.QueryRow(l.ctx, `SELECT count(*) FROM ci_watch_episodes WHERE schedule_id = $1`, l.sched.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (l *ciLive) episode(t *testing.T, key string) store.CiWatchEpisode {
	t.Helper()
	ep, err := l.q.GetCIWatchEpisode(l.ctx, store.GetCIWatchEpisodeParams{ScheduleID: l.sched.ID, UserID: l.user, WorkflowKey: key})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func (l *ciLive) endRuns(t *testing.T) {
	t.Helper()
	if _, err := l.pool.Exec(l.ctx, `UPDATE runs SET status = 'cancelled', finished_at = now() WHERE schedule_id = $1`, l.sched.ID); err != nil {
		t.Fatal(err)
	}
}

// Acceptance examples 2 and 3 end to end: one red starts one guarded run; while it is active
// the next fire is already_running; afterwards the same execution is already handled; the same
// failure again halts (no progress), a different one is a second attempt, then the cap halts,
// and a green resets.
func TestCIWatchEpisodeLifecycleLiveDB(t *testing.T) {
	l := newCILive(t, ciRed("wf", 100, 0))
	l.sc.SetCIRepair(CIRepairConfig{MaxJobs: 5, LogTailBytes: 1000, MaxAttempts: 2, ConfigPaths: []string{"custom/ci/**"}})

	out := l.fire(t)
	if len(out.Started) != 1 || l.runCount(t) != 1 {
		t.Fatalf("first fire: started=%d runs=%d skips=%v", len(out.Started), l.runCount(t), skipReasons(out))
	}
	var guard bool
	var paths []string
	var autoApprove bool
	if err := l.pool.QueryRow(l.ctx, `SELECT ci_config_guard, ci_config_paths, auto_approve FROM runs WHERE id = $1`, out.Started[0].RunID).Scan(&guard, &paths, &autoApprove); err != nil {
		t.Fatal(err)
	}
	if !guard || autoApprove || len(paths) != len(workersvc.CIRepairConfigFloor)+1 || paths[len(paths)-1] != "custom/ci/**" {
		t.Fatalf("run guard=%v auto=%v paths=%v", guard, autoApprove, paths)
	}
	if ep := l.episode(t, "wf"); ep.Attempts != 1 || ep.ObservedRunID.Int64 != 100 {
		t.Fatalf("episode = %+v", ep)
	}

	wantOnlySkip(t, l.fire(t), SkipAlreadyRunning) // the run is still active
	l.endRuns(t)
	wantOnlySkip(t, l.fire(t), SkipCIAlreadyHandled) // R is still the newest, already handled

	// R2 fails with the same failure signature: halted, no progress, recorded durably.
	// (volatile digits in the log are normalised, so only the failure text matters)
	l.setRuns(ciRed("wf", 101, 5))
	wantOnlySkip(t, l.fire(t), SkipCIHaltedNoProgress)
	if ep := l.episode(t, "wf"); ep.HaltedReason.String != "no_progress" || ep.Attempts != 1 || ep.ObservedRunID.Int64 != 101 {
		t.Fatalf("after halt: %+v", ep)
	}
	if l.runCount(t) != 1 {
		t.Fatalf("runs = %d, want still 1", l.runCount(t))
	}
	// The halted episode skips with its reason on the next fire.
	wantOnlySkip(t, l.fire(t), SkipCIHaltedNoProgress)

	// A green resets the episode.
	l.setRuns(ciGreenRun("wf", 102, 10))
	wantOnlySkip(t, l.fire(t), SkipCIGreen)
	if ep := l.episode(t, "wf"); ep.Attempts != 0 || ep.HaltedReason.Valid || ep.LastSignature.Valid {
		t.Fatalf("after green: %+v", ep)
	}

	// Two different failures use the attempt budget, then the cap halts.
	for i, tail := range []string{"first different failure", "second different failure"} {
		id := int64(110 + i)
		l.setRuns(ciRed("wf", id, 20+i))
		l.cf.tails[l.cf.jobs[id][0].ID] = tail
		out := l.fire(t)
		if len(out.Started) != 1 {
			t.Fatalf("attempt %d: skips %v", i+1, skipReasons(out))
		}
		l.endRuns(t)
	}
	l.setRuns(ciRed("wf", 120, 40))
	l.cf.tails[l.cf.jobs[120][0].ID] = "third different failure"
	wantOnlySkip(t, l.fire(t), SkipCIHaltedAttemptCap)
	if ep := l.episode(t, "wf"); ep.HaltedReason.String != "attempt_cap" || ep.Attempts != 2 {
		t.Fatalf("after cap: %+v", ep)
	}
}

// Two fires racing on the same red execution start exactly one run.
func TestCIWatchConcurrentFiresLiveDB(t *testing.T) {
	l := newCILive(t, ciRed("wf", 100, 0))
	// A second scheduler instance, as another API pod would run.
	other := New(l.q, l.sc.runs, &ciBuilder{f: l.cf}, nil, nil, nil, time.Minute, nil)

	sched, err := l.q.GetRunSchedule(l.ctx, l.sched.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	outs := make([]FireOutcome, 2)
	errs := make([]error, 2)
	gate := make(chan struct{})
	for i, sc := range []*Scheduler{l.sc, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			outs[i], errs[i] = sc.fireOne(l.ctx, sched)
		}()
	}
	close(gate)
	wg.Wait()
	started := 0
	for i := range outs {
		if errs[i] != nil {
			t.Fatalf("fire %d: %v", i, errs[i])
		}
		started += len(outs[i].Started)
		if len(outs[i].Started) == 0 {
			if r := skipReasons(outs[i]); len(r) != 1 || (r[0] != SkipCIAlreadyHandled && r[0] != SkipAlreadyRunning) {
				t.Fatalf("losing fire skips = %v", r)
			}
		}
	}
	if started != 1 || l.runCount(t) != 1 {
		t.Fatalf("started=%d runs=%d, want exactly one", started, l.runCount(t))
	}
}

// A configuration edit landing between discovery and the locked start is a benign
// ci_config_changed skip: no run, no episode write. Selectors, auto-approve, guidance and the
// repo's default branch are all covered.
// MUTATION: dropping the fingerprint comparison in recheckCIWatchConfig starts a run here.
func TestCIWatchConfigChangeBetweenDiscoveryAndStartLiveDB(t *testing.T) {
	for name, change := range map[string]string{
		"selectors":      `UPDATE run_schedules SET ci_workflows = '["other"]' WHERE id = $1`,
		"auto_approve":   `UPDATE run_schedules SET auto_approve = true WHERE id = $1`,
		"guidance":       `UPDATE run_schedules SET guidance = 'prefer small diffs' WHERE id = $1`,
		"default branch": `UPDATE repos SET default_branch = 'trunk' WHERE id = (SELECT repo_id FROM run_schedules WHERE id = $1)`,
		"disabled":       `UPDATE run_schedules SET enabled = false WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			l := newCILive(t, ciRed("wf", 100, 0))
			l.cf.onList = func() {
				if _, err := l.pool.Exec(l.ctx, change, l.sched.ID); err != nil {
					t.Error(err)
				}
			}
			wantOnlySkip(t, l.fire(t), SkipCIConfigChanged)
			if l.runCount(t) != 0 || l.episodes(t) != 0 {
				t.Fatalf("runs=%d episodes=%d, want nothing written", l.runCount(t), l.episodes(t))
			}
		})
	}
}

// A schedule deleted during the fire starts nothing and leaves no orphan episode.
func TestCIWatchScheduleDeletedDuringFireLiveDB(t *testing.T) {
	l := newCILive(t, ciRed("wf", 100, 0))
	l.cf.onList = func() {
		if _, err := l.pool.Exec(l.ctx, `DELETE FROM run_schedules WHERE id = $1`, l.sched.ID); err != nil {
			t.Error(err)
		}
	}
	sched := l.sched
	out, err := l.sc.fireOne(l.ctx, sched)
	if err != nil {
		t.Fatalf("fireOne: %v", err)
	}
	wantOnlySkip(t, out, SkipCIConfigChanged)
	if l.runCount(t) != 0 || l.episodes(t) != 0 {
		t.Fatalf("runs=%d episodes=%d, want nothing written", l.runCount(t), l.episodes(t))
	}
}

// A stale GitHub observation (an older execution the unlocked read missed) maps to
// ci_already_handled and writes nothing.
func TestCIWatchStaleObservationLiveDB(t *testing.T) {
	l := newCILive(t, ciRed("wf", 100, 10))
	l.fire(t) // handles execution 100
	l.endRuns(t)
	// A delayed forge view still shows an OLDER red execution as the newest.
	l.setRuns(ciRed("wf", 90, 0))
	wantOnlySkip(t, l.fire(t), SkipCIAlreadyHandled)
	if l.runCount(t) != 1 {
		t.Fatalf("runs = %d, want 1", l.runCount(t))
	}
	if ep := l.episode(t, "wf"); ep.ObservedRunID.Int64 != 100 || ep.Attempts != 1 {
		t.Fatalf("episode moved: %+v", ep)
	}
}

// Green workflows never use admission slots; a capacity-full schedule still starts existing keys
// and reports ci_capacity for unseen ones.
func TestCIWatchCapacityLiveDB(t *testing.T) {
	l := newCILive(t, ciGreenRun("g1", 1, 0), ciGreenRun("g2", 2, 0))
	wantOnlySkip(t, l.fire(t), SkipCIGreen)
	if l.episodes(t) != 0 {
		t.Fatalf("greens admitted %d episodes", l.episodes(t))
	}
	if _, err := l.pool.Exec(l.ctx, `INSERT INTO ci_watch_episodes (schedule_id, workflow_key) SELECT $1, 'k' || g FROM generate_series(1, 1024) g`, l.sched.ID); err != nil {
		t.Fatal(err)
	}
	l.setRuns(ciRed("unseen", 100, 0))
	wantOnlySkip(t, l.fire(t), SkipCICapacity)
	l.setRuns(ciRed("k5", 101, 0))
	if out := l.fire(t); len(out.Started) != 1 {
		t.Fatalf("an existing key at capacity must start, got %v", skipReasons(out))
	}
}

// Deleting the owner's schedule row cascades the episodes.
func TestCIWatchEpisodesCascadeLiveDB(t *testing.T) {
	l := newCILive(t, ciRed("wf", 100, 0))
	l.fire(t)
	if l.episodes(t) != 1 {
		t.Fatal("no episode")
	}
	l.endRuns(t)
	if _, err := l.pool.Exec(l.ctx, `DELETE FROM runs WHERE schedule_id = $1`, l.sched.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := l.pool.Exec(l.ctx, `DELETE FROM run_schedules WHERE id = $1`, l.sched.ID); err != nil {
		t.Fatal(err)
	}
	if l.episodes(t) != 0 {
		t.Fatal("episodes survived the schedule delete")
	}
	_, err := l.q.GetCIWatchEpisode(l.ctx, store.GetCIWatchEpisodeParams{ScheduleID: l.sched.ID, UserID: l.user, WorkflowKey: "wf"})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("err = %v", err)
	}
}
