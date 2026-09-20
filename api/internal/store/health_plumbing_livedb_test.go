package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Admin-health M2 plumbing live-DB coverage (PRD #1484 M2): the controller-report singleton,
// the Danger-episode lifecycle (including the two-concurrent-open race the partial unique index
// resolves to exactly one winner), the per-admin banner snooze, ListAdmins, and the
// eligible-CI-watch-refs-per-repo count. These queries have no NON-test production caller until
// M2-B/M6 wires the evaluator, the snooze endpoint and the forge.ciwatch check; referencing
// every one here also keeps `deadcode -test ./...` green (it treats a test caller as reachable,
// the same property the custody-episode M6 queries rely on).
//
// It lives in the store package DELIBERATELY: e2e/run-store-it.sh + CI's test-api-store-it job
// run `-run 'LiveDB$'` over store/handler/forgesvc/schedsvc/workersvc only. The new healthsvc
// and slacksvc packages are NOT enumerated, so query behaviour is proven here.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres. `ts`, `mustExec` and
// `isUniqueViolation` are shared store_test helpers (worker_roll_health / ci_fix suites).

func openHealthLiveDB(t *testing.T) (context.Context, *pgxpool.Pool, *store.Queries) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, store.New(pool)
}

// TestControllerReportSingletonLiveDB proves GetControllerReport reports "never" (pgx.ErrNoRows)
// before the first write, and that UpsertControllerReport ADVANCES the single row's observed_at
// on a repeat call — the property ControllerStatus relies on to leave a fleet-independent trace
// even for a zero-worker report.
func TestControllerReportSingletonLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	// The shared LiveDB run persists across test functions; this is the only writer of the
	// singleton, so a reset makes the "never reported" assertion order-independent.
	mustExec(ctx, t, pool, `DELETE FROM controller_report_status`)

	if _, err := q.GetControllerReport(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetControllerReport before any write = %v, want pgx.ErrNoRows (never reported)", err)
	}

	t1 := time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	if err := q.UpsertControllerReport(ctx, ts(t1)); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	got, err := q.GetControllerReport(ctx)
	if err != nil {
		t.Fatalf("get after first upsert: %v", err)
	}
	if !got.Time.Equal(t1) {
		t.Fatalf("observed_at after first write = %v, want %v", got.Time, t1)
	}

	// A repeat is an UPSERT of the one legal row (id=1), advancing observed_at.
	t2 := time.Now().Truncate(time.Microsecond)
	if err := q.UpsertControllerReport(ctx, ts(t2)); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, err = q.GetControllerReport(ctx)
	if err != nil {
		t.Fatalf("get after second upsert: %v", err)
	}
	if !got.Time.Equal(t2) {
		t.Fatalf("observed_at after repeat = %v, want it advanced to %v", got.Time, t2)
	}

	// Still exactly one row (the CHECK (id = 1) makes any second row impossible anyway).
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM controller_report_status`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("controller_report_status has %d rows, want exactly 1 (singleton)", n)
	}
}

// TestHealthEpisodeLifecycleLiveDB walks one episode: none open → open → a second open trips
// the partial-unique violation → GetOpenHealthEpisode returns the open row → close (the re-arm)
// → none open → a fresh open succeeds because the closed row dropped out of the partial index.
func TestHealthEpisodeLifecycleLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)

	if _, err := q.GetOpenHealthEpisode(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetOpenHealthEpisode with none open = %v, want pgx.ErrNoRows", err)
	}

	opened := time.Now().Truncate(time.Microsecond)
	id, err := q.OpenHealthEpisode(ctx, ts(opened))
	if err != nil {
		t.Fatalf("open episode: %v", err)
	}

	// A second open WHILE one is open is a 23505 unique violation (the partial index), not a
	// silent no-op — that is why the query must not use ON CONFLICT DO NOTHING.
	if _, err := q.OpenHealthEpisode(ctx, ts(time.Now())); !isUniqueViolation(err) {
		t.Fatalf("second open while one is open = %v, want a 23505 unique violation", err)
	}

	got, err := q.GetOpenHealthEpisode(ctx)
	if err != nil {
		t.Fatalf("get open episode: %v", err)
	}
	if got.ID != id {
		t.Fatalf("open episode id = %v, want the one just opened %v", got.ID, id)
	}
	if !got.OpenedAt.Time.Equal(opened) {
		t.Fatalf("open episode opened_at = %v, want %v", got.OpenedAt.Time, opened)
	}

	// Close it (the re-arm). CloseHealthEpisode is scoped to still-open rows.
	if err := q.CloseHealthEpisode(ctx, store.CloseHealthEpisodeParams{ID: id, ClosedAt: ts(time.Now())}); err != nil {
		t.Fatalf("close episode: %v", err)
	}
	if _, err := q.GetOpenHealthEpisode(ctx); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetOpenHealthEpisode after close = %v, want pgx.ErrNoRows", err)
	}

	// A fresh open now succeeds — the closed row left the partial index.
	id2, err := q.OpenHealthEpisode(ctx, ts(time.Now()))
	if err != nil {
		t.Fatalf("re-open after close: %v", err)
	}
	if id2 == id {
		t.Fatalf("re-open returned the SAME id %v; a fresh episode must be a new row", id2)
	}
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)
}

// TestHealthEpisodeConcurrentOpenLiveDB is the M2 two-replica proof: N goroutines race
// OpenHealthEpisode with no episode open, and EXACTLY ONE wins while every loser trips the
// partial-unique (23505) violation. This is what makes "open a Danger episode" an atomic claim
// two api replicas cannot both win. Run under -race.
func TestHealthEpisodeConcurrentOpenLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	type res struct {
		id  uuid.UUID
		err error
	}
	results := make([]res, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			id, err := q.OpenHealthEpisode(ctx, ts(time.Now()))
			results[i] = res{id: id, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var winners, violations, others int
	for _, r := range results {
		switch {
		case r.err == nil && r.id != uuid.Nil:
			winners++
		case isUniqueViolation(r.err):
			violations++
		default:
			others++
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (the partial unique index admits one open episode)", winners)
	}
	if violations != n-1 {
		t.Fatalf("unique violations = %d, want %d (every loser trips 23505)", violations, n-1)
	}
	if others != 0 {
		t.Fatalf("got %d results that were neither the winner nor a 23505 violation", others)
	}
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)
}

// TestClaimHealthEpisodeNoticeAtomicLiveDB is the M6 exactly-once proof: N goroutines race
// ClaimHealthEpisodeNotice for the SAME (episode, admin) slot, and EXACTLY ONE insert takes
// (rows-affected 1) while every loser is an ON CONFLICT DO NOTHING no-op (rows-affected 0) —
// none an error. That is what makes the danger notice fire exactly once per admin per episode
// across api replicas and repeated still-danger ticks. It then proves the claim is PER-EPISODE:
// the same admin claims freshly against a SECOND episode (a distinct PK) — the re-arm — while a
// repeat within an episode stays a no-op. Modeled on TestHealthEpisodeConcurrentOpenLiveDB; run
// under -race.
func TestClaimHealthEpisodeNoticeAtomicLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("claim-%s@e2e", userID))
	ep1, err := q.OpenHealthEpisode(ctx, ts(time.Now()))
	if err != nil {
		t.Fatalf("open episode 1: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	type res struct {
		rows int64
		err  error
	}
	results := make([]res, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rows, err := q.ClaimHealthEpisodeNotice(ctx, store.ClaimHealthEpisodeNoticeParams{EpisodeID: ep1, UserID: userID})
			results[i] = res{rows: rows, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var took, noop, errs int
	for _, r := range results {
		switch {
		case r.err != nil:
			errs++
		case r.rows == 1:
			took++
		case r.rows == 0:
			noop++
		}
	}
	if errs != 0 {
		t.Fatalf("got %d errored claims; ON CONFLICT DO NOTHING must never error a loser", errs)
	}
	if took != 1 {
		t.Fatalf("claims that inserted = %d, want exactly 1 (the atomic per-admin-per-episode claim)", took)
	}
	if noop != n-1 {
		t.Fatalf("no-op claims = %d, want %d (every loser is a DO NOTHING no-op)", noop, n-1)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM health_episode_notices WHERE episode_id = $1 AND user_id = $2`, ep1, userID).Scan(&count); err != nil {
		t.Fatalf("count notices: %v", err)
	}
	if count != 1 {
		t.Fatalf("health_episode_notices rows for (ep1,user) = %d, want 1 (exactly one claim landed)", count)
	}

	// PER-EPISODE: the SAME admin claims FRESH against a second episode (the re-arm). Close ep1
	// first so the partial unique index admits a second open.
	if err := q.CloseHealthEpisode(ctx, store.CloseHealthEpisodeParams{ID: ep1, ClosedAt: ts(time.Now())}); err != nil {
		t.Fatalf("close episode 1: %v", err)
	}
	ep2, err := q.OpenHealthEpisode(ctx, ts(time.Now()))
	if err != nil {
		t.Fatalf("open episode 2: %v", err)
	}
	rows, err := q.ClaimHealthEpisodeNotice(ctx, store.ClaimHealthEpisodeNoticeParams{EpisodeID: ep2, UserID: userID})
	if err != nil {
		t.Fatalf("claim for episode 2: %v", err)
	}
	if rows != 1 {
		t.Fatalf("claim for a DIFFERENT episode returned rows = %d, want 1 (claims are per-episode — the re-arm)", rows)
	}
	// A repeat of the ep2 claim is a no-op (idempotent within the episode).
	rows, err = q.ClaimHealthEpisodeNotice(ctx, store.ClaimHealthEpisodeNoticeParams{EpisodeID: ep2, UserID: userID})
	if err != nil {
		t.Fatalf("repeat claim for episode 2: %v", err)
	}
	if rows != 0 {
		t.Fatalf("repeat claim within an episode returned rows = %d, want 0 (idempotent)", rows)
	}
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)
}

// TestHealthBannerSnoozeLiveDB proves the snooze read reports "not snoozed" (pgx.ErrNoRows)
// before any snooze, and that a re-snooze OVERWRITES the expiry (ON CONFLICT DO UPDATE) — the
// per-admin, per-episode key the Danger banner reads.
func TestHealthBannerSnoozeLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("snooze-%s@e2e", userID))
	epID, err := q.OpenHealthEpisode(ctx, ts(time.Now()))
	if err != nil {
		t.Fatalf("open episode: %v", err)
	}

	key := store.GetHealthBannerSnoozeParams{EpisodeID: epID, UserID: userID}
	if _, err := q.GetHealthBannerSnooze(ctx, key); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetHealthBannerSnooze before any snooze = %v, want pgx.ErrNoRows", err)
	}

	until1 := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	if err := q.UpsertHealthBannerSnooze(ctx, store.UpsertHealthBannerSnoozeParams{
		EpisodeID: epID, UserID: userID, SnoozedUntil: ts(until1),
	}); err != nil {
		t.Fatalf("first snooze: %v", err)
	}
	got, err := q.GetHealthBannerSnooze(ctx, key)
	if err != nil {
		t.Fatalf("get after first snooze: %v", err)
	}
	if !got.Time.Equal(until1) {
		t.Fatalf("snoozed_until after first snooze = %v, want %v", got.Time, until1)
	}

	// Re-snooze OVERWRITES rather than inserting a second row.
	until2 := until1.Add(30 * time.Minute)
	if err := q.UpsertHealthBannerSnooze(ctx, store.UpsertHealthBannerSnoozeParams{
		EpisodeID: epID, UserID: userID, SnoozedUntil: ts(until2),
	}); err != nil {
		t.Fatalf("re-snooze: %v", err)
	}
	got, err = q.GetHealthBannerSnooze(ctx, key)
	if err != nil {
		t.Fatalf("get after re-snooze: %v", err)
	}
	if !got.Time.Equal(until2) {
		t.Fatalf("snoozed_until after re-snooze = %v, want the overwritten %v", got.Time, until2)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM health_banner_snoozes WHERE episode_id = $1 AND user_id = $2`, epID, userID).Scan(&n); err != nil {
		t.Fatalf("count snoozes: %v", err)
	}
	if n != 1 {
		t.Fatalf("health_banner_snoozes has %d rows for (episode,user), want 1 (upsert overwrites)", n)
	}
	mustExec(ctx, t, pool, `UPDATE health_episodes SET closed_at = now() WHERE closed_at IS NULL`)
}

// TestListAdminsLiveDB proves ListAdmins returns the admins and only the admins: a seeded admin
// appears and a seeded non-admin does not. Asserted as membership, not a full-set match, because
// the shared LiveDB run may already carry other admins.
func TestListAdminsLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)

	adminID := uuid.New()
	nonAdminID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', true)`,
		adminID, fmt.Sprintf("ladmin-%s@e2e", adminID))
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, is_admin) VALUES ($1, $2, 'x', false)`,
		nonAdminID, fmt.Sprintf("lnon-%s@e2e", nonAdminID))

	admins, err := q.ListAdmins(ctx)
	if err != nil {
		t.Fatalf("ListAdmins: %v", err)
	}
	set := make(map[uuid.UUID]bool, len(admins))
	for _, id := range admins {
		set[id] = true
	}
	if !set[adminID] {
		t.Errorf("ListAdmins omitted the seeded admin %s", adminID)
	}
	if set[nonAdminID] {
		t.Errorf("ListAdmins returned the seeded non-admin %s", nonAdminID)
	}
}

// TestCountEligibleCIWatchRefsPerRepoLiveDB proves the per-repo eligible-branch count reuses the
// watcher's eligibility predicate exactly: distinct branches per repo (a branch's several runs
// collapse to one), a terminal-with-MR run counts only inside the finished-after window, a
// terminal run with no MR and a blank branch never count, and a repo with zero eligible branches
// produces NO row. Results are filtered to the seeded repos so the shared DB does not interfere.
func TestCountEligibleCIWatchRefsPerRepoLiveDB(t *testing.T) {
	ctx, pool, q := openHealthLiveDB(t)

	userID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("ciw-%s@e2e", userID))
	connID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})

	repoA, repoB, repoC := uuid.New(), uuid.New(), uuid.New()
	seedRepo := func(id uuid.UUID, path string, pid int64) {
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
			 VALUES ($1, $2, $3, $4, $5, 'main', true)`, id, connID, pid, path, "https://forge.e2e/"+path)
	}
	seedRepo(repoA, "g/repoA-"+repoA.String()[:8], int64(uuid.New().ID()))
	seedRepo(repoB, "g/repoB-"+repoB.String()[:8], int64(uuid.New().ID()))
	seedRepo(repoC, "g/repoC-"+repoC.String()[:8], int64(uuid.New().ID()))

	var iid int64 = 900000
	seedRun := func(repo uuid.UUID, branch, status string, mrIID *int64, finishedAt *time.Time) {
		iid++
		var mr, fin any
		if mrIID != nil {
			mr = *mrIID
		}
		if finishedAt != nil {
			fin = *finishedAt
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status, mr_iid, finished_at)
			 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, $7, $8)`,
			uuid.New(), userID, repo, iid, branch, status, mr, fin)
	}

	now := time.Now()
	recent := now.Add(-5 * time.Minute) // inside the finished-after window
	old := now.Add(-2 * time.Hour)      // outside it
	mrB3 := int64(6001)
	mrB5 := int64(6005)

	// Repo A: two DISTINCT eligible branches. b1 appears twice (different runs) and must collapse
	// to one; b2 once. Both non-terminal (always eligible).
	seedRun(repoA, "b1", "running", nil, nil)
	seedRun(repoA, "b1", "queued", nil, nil)
	seedRun(repoA, "b2", "running", nil, nil)
	// Repo B: one eligible (b3: terminal + MR + finished in-window); three ineligible (b4 terminal
	// no MR; b5 terminal + MR but finished OUT of window; blank branch).
	seedRun(repoB, "b3", "completed", &mrB3, &recent)
	seedRun(repoB, "b4", "completed", nil, &recent)
	seedRun(repoB, "b5", "completed", &mrB5, &old)
	seedRun(repoB, "", "running", nil, nil)
	// Repo C: only an ineligible run (terminal, no MR) → must NOT appear at all.
	seedRun(repoC, "b6", "completed", nil, &recent)

	cutoff := now.Add(-time.Hour)
	rows, err := q.CountEligibleCIWatchRefsPerRepo(ctx, ts(cutoff))
	if err != nil {
		t.Fatalf("CountEligibleCIWatchRefsPerRepo: %v", err)
	}

	counts := make(map[uuid.UUID]int64)
	paths := make(map[uuid.UUID]string)
	for _, r := range rows {
		if r.RepoID.Valid {
			id := uuid.UUID(r.RepoID.Bytes)
			counts[id] = r.EligibleRefs
			paths[id] = r.RepoPath
		}
	}
	if counts[repoA] != 2 {
		t.Errorf("repoA eligible refs = %d, want 2 (b1 collapsed + b2)", counts[repoA])
	}
	if paths[repoA] == "" {
		t.Errorf("repoA repo_path is empty; the JOIN to repos must carry the identifier")
	}
	if counts[repoB] != 1 {
		t.Errorf("repoB eligible refs = %d, want 1 (only b3: terminal + MR + in-window)", counts[repoB])
	}
	if c, ok := counts[repoC]; ok {
		t.Errorf("repoC present with %d eligible refs; a repo with zero eligible branches must produce NO row", c)
	}
}
