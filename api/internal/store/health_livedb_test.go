package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestHealthChecksLiveDB exercises the admin-health read queries (PRD #1484 M1) against a
// REAL Postgres: the ListAllWorkers roll-health join, the three new run/queue/schedule
// aggregates, and the SchemaVersionStatus accessor. The severity logic over these rows is
// unit-tested in the healthsvc package with a fake; this suite proves the SQL itself —
// tenancy of the join, the heartbeat-freshness window, the min() nullability, and the
// paused-with-enabled-schedules HAVING filter — which a fake store cannot show.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix).
func TestHealthChecksLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()
	q := store.New(pool)

	now := time.Now().UTC().Truncate(time.Second)

	// A user + its forge connection + one enabled repo, so runs (repo_id NOT NULL) and
	// schedules can hang off it.
	seedUserRepo := func(tag string) (userID, repoID uuid.UUID) {
		userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
		mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
			userID, fmt.Sprintf("hc-%s-%s@e2e", tag, userID))
		mustExec(ctx, t, pool,
			`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
			 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/'||$4, 'main', true)`,
			repoID, connID, int64(uuid.New().ID()), tag)
		return userID, repoID
	}

	seedWorker := func(userID uuid.UUID, kind string, hbAgo *time.Duration, draining bool) uuid.UUID {
		id := uuid.New()
		var size, tmpl any
		if kind == "hosted" {
			size, tmpl = "m", "base"
		}
		var hb any
		if hbAgo != nil {
			hb = now.Add(-*hbAgo)
		}
		var drain any
		if draining {
			drain = now.Add(-time.Minute)
		}
		mustExec(ctx, t, pool,
			`INSERT INTO workers (id, user_id, name, token_hash, status, kind, hosted_size, template_declared, last_heartbeat_at, draining_since)
			 VALUES ($1, $2, $3, $4, 'online', $5, $6, $7, $8, $9)`,
			id, userID, "w-"+id.String()[:8], []byte(id.String()), kind, size, tmpl, hb, drain)
		return id
	}

	// -------------------------------------------------------------------------
	// ListAllWorkers roll join: a hosted worker with a stuck report carries the roll
	// columns; a worker with no report LEFT-JOINs to NULLs and still lists.
	// -------------------------------------------------------------------------
	rollUser, _ := seedUserRepo("roll")
	stuckWorker := seedWorker(rollUser, "hosted", durPtr(5*time.Second), false)
	noReportWorker := seedWorker(rollUser, "external", durPtr(5*time.Second), false)

	if _, err := q.UpsertWorkerRollHealth(ctx, store.UpsertWorkerRollHealthParams{
		WorkerID:             stuckWorker,
		Phase:                "stuck",
		ObservedAt:           ts(now),
		ControllerReportedAt: ts(now),
		BlockingContainer:    pgtype.Text{String: "agent", Valid: true},
		BlockingReason:       pgtype.Text{String: "ImagePullBackOff", Valid: true},
		WorkerImageTag:       pgtype.Text{String: "0.84.0", Valid: true},
		TagValid:             true,
	}); err != nil {
		t.Fatalf("upsert roll health: %v", err)
	}

	rows, err := q.ListAllWorkers(ctx)
	if err != nil {
		t.Fatalf("ListAllWorkers: %v", err)
	}
	var sawStuck, sawNoReport bool
	for _, r := range rows {
		switch r.Worker.ID {
		case stuckWorker:
			sawStuck = true
			if !r.RollPhase.Valid || r.RollPhase.String != "stuck" {
				t.Errorf("stuck worker roll_phase = %+v, want stuck", r.RollPhase)
			}
			if r.RollBlockingReason.String != "ImagePullBackOff" {
				t.Errorf("stuck worker roll_blocking_reason = %q, want ImagePullBackOff", r.RollBlockingReason.String)
			}
			if !r.RollObservedAt.Valid {
				t.Errorf("stuck worker roll_observed_at is null; the join must carry it for freshness")
			}
			if r.RollWorkerImageTag.String != "0.84.0" {
				t.Errorf("stuck worker roll_worker_image_tag = %q, want 0.84.0", r.RollWorkerImageTag.String)
			}
		case noReportWorker:
			sawNoReport = true
			if r.RollPhase.Valid {
				t.Errorf("no-report worker roll_phase is non-null (%v); a LEFT JOIN must leave it null", r.RollPhase)
			}
		}
	}
	if !sawStuck || !sawNoReport {
		t.Fatalf("ListAllWorkers missing seeded workers (stuck=%v noReport=%v)", sawStuck, sawNoReport)
	}

	// -------------------------------------------------------------------------
	// ListOwnersWaitingNoCapacity: an owner waiting with no usable worker is returned
	// with the oldest health_since; a fresh non-draining worker excludes its owner; a
	// draining or stale worker does not count as capacity.
	// -------------------------------------------------------------------------
	// noCap: two waiting runs, no workers → returned, oldest health_since.
	noCapUser, noCapRepo := seedUserRepo("nocap")
	oldWait := now.Add(-9 * time.Minute)
	seedWaitingRun(ctx, t, pool, noCapUser, noCapRepo, oldWait)
	seedWaitingRun(ctx, t, pool, noCapUser, noCapRepo, now.Add(-2*time.Minute))

	// hasCap: a waiting run BUT a fresh non-draining worker → excluded.
	hasCapUser, hasCapRepo := seedUserRepo("hascap")
	seedWaitingRun(ctx, t, pool, hasCapUser, hasCapRepo, now.Add(-time.Minute))
	seedWorker(hasCapUser, "external", durPtr(5*time.Second), false)

	// drainOnly: a waiting run but only a draining worker → returned (draining is not capacity).
	drainUser, drainRepo := seedUserRepo("drain")
	seedWaitingRun(ctx, t, pool, drainUser, drainRepo, now.Add(-time.Minute))
	seedWorker(drainUser, "external", durPtr(5*time.Second), true)

	// staleOnly: a waiting run but only a stale-heartbeat worker → returned.
	staleUser, staleRepo := seedUserRepo("stale")
	seedWaitingRun(ctx, t, pool, staleUser, staleRepo, now.Add(-time.Minute))
	seedWorker(staleUser, "external", durPtr(5*time.Minute), false)

	cutoff := ts(now.Add(-45 * time.Second))
	capRows, err := q.ListOwnersWaitingNoCapacity(ctx, cutoff)
	if err != nil {
		t.Fatalf("ListOwnersWaitingNoCapacity: %v", err)
	}
	got := map[uuid.UUID]pgtype.Timestamptz{}
	for _, r := range capRows {
		got[r.UserID] = r.OldestHealthSince
	}
	if _, ok := got[hasCapUser]; ok {
		t.Errorf("owner with a fresh non-draining worker must NOT be waiting-no-capacity")
	}
	for _, u := range []uuid.UUID{noCapUser, drainUser, staleUser} {
		if _, ok := got[u]; !ok {
			t.Errorf("owner %s with no usable worker must be returned", u)
		}
	}
	if ts := got[noCapUser]; !ts.Valid || !ts.Time.Equal(oldWait) {
		t.Errorf("noCap oldest_health_since = %v, want %v (the min across its two waits)", ts.Time, oldWait)
	}

	// -------------------------------------------------------------------------
	// OldestWaitingWorkerRun: the global min health_since across every waiting run.
	// (The noCap owner's 9-minute wait above is the oldest seeded so far.)
	// -------------------------------------------------------------------------
	oldest, err := q.OldestWaitingWorkerRun(ctx)
	if err != nil {
		t.Fatalf("OldestWaitingWorkerRun: %v", err)
	}
	if !oldest.Valid || oldest.Time.After(oldWait) {
		t.Errorf("OldestWaitingWorkerRun = %v, want <= %v", oldest.Time, oldWait)
	}

	// -------------------------------------------------------------------------
	// OldestUndispatchedTaskRun: min created_at over kind='task', status='queued',
	// dispatched_at IS NULL. A dispatched task and a queued issue run must not count.
	// -------------------------------------------------------------------------
	taskUser, taskRepo := seedUserRepo("task")
	taskCreated := now.Add(-20 * time.Minute)
	seedTaskRun(ctx, t, pool, taskUser, taskRepo, "queued", taskCreated, false)        // counts
	seedTaskRun(ctx, t, pool, taskUser, taskRepo, "queued", now.Add(-time.Hour), true) // dispatched, excluded
	// A queued ISSUE run must not count (kind scope is load-bearing).
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
		 VALUES ($1, $2, $3, 7, 'i', 'd', 'queued', 'issue', $4)`,
		uuid.New(), taskUser, taskRepo, now.Add(-2*time.Hour))

	undis, err := q.OldestUndispatchedTaskRun(ctx)
	if err != nil {
		t.Fatalf("OldestUndispatchedTaskRun: %v", err)
	}
	if !undis.Valid || !undis.Time.Equal(taskCreated) {
		t.Errorf("OldestUndispatchedTaskRun = %v, want %v (dispatched task and queued issue excluded)", undis.Time, taskCreated)
	}

	// -------------------------------------------------------------------------
	// CountUsersPausedWithEnabledSchedules: a paused user owning >= 1 enabled schedule
	// counts; a paused user with only a disabled schedule does not; an unpaused user does
	// not; a schedules_paused_until in the future counts.
	// -------------------------------------------------------------------------
	pausedEnabled, peRepo := seedUserRepo("pe")
	mustExec(ctx, t, pool, `UPDATE users SET schedules_paused = true WHERE id = $1`, pausedEnabled)
	seedSchedule(ctx, t, pool, pausedEnabled, peRepo, true)

	pausedDisabledOnly, pdRepo := seedUserRepo("pd")
	mustExec(ctx, t, pool, `UPDATE users SET schedules_paused = true WHERE id = $1`, pausedDisabledOnly)
	seedSchedule(ctx, t, pool, pausedDisabledOnly, pdRepo, false)

	unpaused, upRepo := seedUserRepo("up")
	seedSchedule(ctx, t, pool, unpaused, upRepo, true)

	pausedUntil, puRepo := seedUserRepo("pu")
	mustExec(ctx, t, pool, `UPDATE users SET schedules_paused_until = $2 WHERE id = $1`, pausedUntil, now.Add(time.Hour))
	seedSchedule(ctx, t, pool, pausedUntil, puRepo, true)

	// A single count over the whole table is hard to pin exactly in a shared DB, so probe
	// each user's membership by comparing the count immediately before/after we would
	// expect it — instead, assert the count is at least 2 (pausedEnabled + pausedUntil) and
	// verify the negatives are absent by re-counting after flipping them, which is fragile;
	// simpler: count with @now and assert it reflects exactly the two positive users we can
	// isolate by running the query against a scratch expectation below.
	cnt, err := q.CountUsersPausedWithEnabledSchedules(ctx, ts(now))
	if err != nil {
		t.Fatalf("CountUsersPausedWithEnabledSchedules: %v", err)
	}
	// pausedEnabled and pausedUntil qualify; pausedDisabledOnly and unpaused do not. Other
	// suites in the shared DB may add their own, so assert our two positives are counted and
	// our two negatives are provably excluded by a targeted re-count.
	if cnt < 2 {
		t.Errorf("CountUsersPausedWithEnabledSchedules = %d, want >= 2 (pausedEnabled + pausedUntil)", cnt)
	}
	// Targeted proof the negatives are excluded: pause the "unpaused" user's schedule owner
	// but leave it enabled=false-only cannot be — instead flip pausedDisabledOnly's schedule
	// to enabled and confirm the count rises by exactly one.
	before := cnt
	mustExec(ctx, t, pool, `UPDATE run_schedules SET enabled = true WHERE user_id = $1`, pausedDisabledOnly)
	after, err := q.CountUsersPausedWithEnabledSchedules(ctx, ts(now))
	if err != nil {
		t.Fatalf("re-count: %v", err)
	}
	if after != before+1 {
		t.Errorf("enabling the paused user's only schedule changed the count by %d, want +1 "+
			"(proves the HAVING enabled filter)", after-before)
	}
	// Targeted proof of the PAUSE predicate itself: pausing a user who ALREADY owns an enabled
	// schedule (the seeded `unpaused` negative) must raise the count by exactly one. Without
	// this the WHERE pause clause could be dropped entirely and every assertion above would
	// still pass — `unpaused` already contributes to the baseline `>= 2`, and the enable-flip
	// delta exercises only the HAVING filter.
	mustExec(ctx, t, pool, `UPDATE users SET schedules_paused = true WHERE id = $1`, unpaused)
	withUnpaused, err := q.CountUsersPausedWithEnabledSchedules(ctx, ts(now))
	if err != nil {
		t.Fatalf("re-count after pausing the unpaused user: %v", err)
	}
	if withUnpaused != after+1 {
		t.Errorf("pausing a user with an enabled schedule changed the count by %d, want +1 "+
			"(proves the pause predicate)", withUnpaused-after)
	}

	// -------------------------------------------------------------------------
	// SchemaVersionStatus: a freshly-migrated database is at head.
	// -------------------------------------------------------------------------
	applied, head, atHead, err := store.SchemaVersionStatus(ctx, pool)
	if err != nil {
		t.Fatalf("SchemaVersionStatus: %v", err)
	}
	if applied != head || !atHead {
		t.Errorf("SchemaVersionStatus applied=%d head=%d atHead=%v, want applied==head and atHead", applied, head, atHead)
	}
	if head <= 0 {
		t.Errorf("SchemaVersionStatus head = %d, want a positive embedded head", head)
	}
}

func durPtr(d time.Duration) *time.Duration { return &d }

// healthIID hands out distinct issue iids so two seeded issue runs on one repo never
// collide on the (repo_id, issue_iid) unique index.
var healthIID int64 = 500000

func nextHealthIID() int64 { healthIID++; return healthIID }

func seedWaitingRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, healthSince time.Time) {
	t.Helper()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, health, health_since)
		 VALUES ($1, $2, $3, $4, 'w', 'd', 'queued', 'issue', 'waiting_worker', $5)`,
		uuid.New(), userID, repoID, nextHealthIID(), healthSince)
}

// seedTaskRun inserts a kind='task' run. The runs_kind_shape CHECK requires branch IS NOT
// NULL and issue_iid IS NULL, so a task run is issue-less with a branch.
func seedTaskRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, status string, createdAt time.Time, dispatched bool) {
	t.Helper()
	id := uuid.New()
	var dispatchedAt any
	if dispatched {
		dispatchedAt = createdAt.Add(time.Second)
	}
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, kind, branch, issue_title, issue_description, status, created_at, dispatched_at)
		 VALUES ($1, $2, $3, 'task', $4, 't', 'd', $5, $6, $7)`,
		id, userID, repoID, "uzi/task/"+id.String(), status, createdAt, dispatchedAt)
}

// seedSchedule inserts a recurring prompt schedule owned by userID, enabled or not.
func seedSchedule(ctx context.Context, t *testing.T, pool *pgxpool.Pool, userID, repoID uuid.UUID, enabled bool) {
	t.Helper()
	mustExec(ctx, t, pool,
		`INSERT INTO run_schedules (id, user_id, repo_id, target, prompt, timing, cron_expr, enabled)
		 VALUES ($1, $2, $3, 'prompt', 'do the thing', 'recurring', '0 0 * * *', $4)`,
		uuid.New(), userID, repoID, enabled)
}
