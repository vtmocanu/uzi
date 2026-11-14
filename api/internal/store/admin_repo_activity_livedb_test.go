package store

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Issue #4334: AdminListEnabledRepoActivity. The tests live in package store (not
// store_test) so the plan harness can run EXPLAIN on the generated query text.

func openRepoActivityLiveDB(t *testing.T) (context.Context, *pgxpool.Pool, *Queries) {
	t.Helper()
	ctx := context.Background()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return ctx, pool, New(pool)
}

func raExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	tag, err := pool.Exec(ctx, sql, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return tag.RowsAffected()
}

var raProjectSeq int64 = 9_000_000

// raSeedUserConn inserts a user with one forge connection; deleting the user at
// cleanup cascades to its connections, repos and runs.
func raSeedUserConn(ctx context.Context, t *testing.T, pool *pgxpool.Pool, cleanup bool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	user, conn := uuid.New(), uuid.New()
	raExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, "ra-"+user.String()+"@e2e")
	raExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, user, []byte{0x1})
	if cleanup {
		t.Cleanup(func() {
			raExec(context.Background(), t, pool, `DELETE FROM users WHERE id = $1`, user)
		})
	}
	return user, conn
}

func raSeedRepo(ctx context.Context, t *testing.T, pool *pgxpool.Pool, conn uuid.UUID, path string, enabled bool) uuid.UUID {
	t.Helper()
	raProjectSeq++
	id := uuid.New()
	raExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.e2e/x', 'main', $5)`, id, conn, raProjectSeq, path, enabled)
	return id
}

func raSeedRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, user, repo uuid.UUID, iid int, status string, at time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	raExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'issue', $6)`, id, user, repo, iid, status, at)
	return id
}

func TestAdminListEnabledRepoActivityLiveDB(t *testing.T) {
	ctx, pool, q := openRepoActivityLiveDB(t)
	user1, conn1 := raSeedUserConn(ctx, t, pool, true)
	user2, conn2 := raSeedUserConn(ctx, t, pool, true)
	prefix := "ra4334-" + uuid.NewString()[:8] + "/"
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Inserted out of path order so ORDER BY is what sorts the result.
	enabledNever := raSeedRepo(ctx, t, pool, conn2, prefix+"b-enabled-never-ran", true)
	enabledRan := raSeedRepo(ctx, t, pool, conn1, prefix+"a-enabled-ran", true)
	disabledActive := raSeedRepo(ctx, t, pool, conn1, prefix+"c-disabled-active", false)
	disabledTerminal := raSeedRepo(ctx, t, pool, conn2, prefix+"d-disabled-terminal", false)
	disabledNever := raSeedRepo(ctx, t, pool, conn2, prefix+"e-disabled-never-ran", false)
	sameX := raSeedRepo(ctx, t, pool, conn1, prefix+"s-same-path", true)
	sameY := raSeedRepo(ctx, t, pool, conn2, prefix+"s-same-path", true)

	newest := now.Add(-time.Hour)
	raSeedRun(ctx, t, pool, user1, enabledRan, 1, "completed", now.Add(-3*time.Hour))
	raSeedRun(ctx, t, pool, user1, enabledRan, 2, "failed", newest)
	for i, st := range []string{"completed", "failed", "cancelled"} {
		raSeedRun(ctx, t, pool, user2, disabledTerminal, i+1, st, now.Add(-time.Duration(i+2)*time.Hour))
	}
	for i, st := range []string{"queued", "running", "awaiting_approval"} {
		// One active issue run per repo (uq_runs_one_active_per_issue), so each
		// non-terminal status gets its own disabled repo.
		repo := raSeedRepo(ctx, t, pool, conn1, fmt.Sprintf("%sf-disabled-%s", prefix, st), false)
		raSeedRun(ctx, t, pool, user1, repo, 1, st, now.Add(-time.Duration(i+1)*time.Minute))
	}
	activeAt := now.Add(-30 * time.Minute)
	raSeedRun(ctx, t, pool, user1, disabledActive, 1, "queued", activeAt)

	rows, err := q.AdminListEnabledRepoActivity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		id      uuid.UUID
		enabled bool
		last    *time.Time
	}
	var mine []got
	var paths []string
	for _, r := range rows {
		if !strings.HasPrefix(r.PathWithNamespace, prefix) {
			continue
		}
		g := got{id: r.ID, enabled: r.Enabled}
		if r.LastRunAt.Valid {
			tm := r.LastRunAt.Time
			g.last = &tm
		}
		mine = append(mine, g)
		paths = append(paths, r.PathWithNamespace)
	}

	// Path order, then id order within the duplicated path.
	same := []uuid.UUID{sameX, sameY}
	sort.Slice(same, func(i, j int) bool { return same[i].String() < same[j].String() })
	wantPaths := []string{
		prefix + "a-enabled-ran",
		prefix + "b-enabled-never-ran",
		prefix + "c-disabled-active",
		prefix + "f-disabled-awaiting_approval",
		prefix + "f-disabled-queued",
		prefix + "f-disabled-running",
		prefix + "s-same-path",
		prefix + "s-same-path",
	}
	if fmt.Sprint(paths) != fmt.Sprint(wantPaths) {
		t.Fatalf("paths = %v\nwant    %v (terminal-only %s and never-ran disabled %s must be excluded)",
			paths, wantPaths, disabledTerminal, disabledNever)
	}
	if mine[0].id != enabledRan || !mine[0].enabled || mine[0].last == nil || !mine[0].last.Equal(newest) {
		t.Fatalf("enabled repo that ran: %+v, want last %s", mine[0], newest)
	}
	if mine[1].id != enabledNever || mine[1].last != nil {
		t.Fatalf("enabled repo that never ran: %+v, want nil last_run_at", mine[1])
	}
	if mine[2].id != disabledActive || mine[2].enabled || mine[2].last == nil || !mine[2].last.Equal(activeAt) {
		t.Fatalf("disabled repo with an active run: %+v, want enabled=false last %s", mine[2], activeAt)
	}
	if mine[6].id != same[0] || mine[7].id != same[1] {
		t.Fatalf("same-path tie order = %s,%s want %s,%s", mine[6].id, mine[7].id, same[0], same[1])
	}
}

// TestAdminRepoActivityQueryPlanSkewed is a MEASUREMENT harness, not a gate: its name
// deliberately does not end in LiveDB so the CI sweep never selects it, and it skips
// unless UZI_REPO_ACTIVITY_PLAN=1. It logs the EXPLAIN (ANALYZE, BUFFERS) plan and the
// median of five timed executions of the exact generated query on a skewed table (one
// repo with 50,000 completed runs). It asserts no timing.
func TestAdminRepoActivityQueryPlanSkewed(t *testing.T) {
	if os.Getenv("UZI_REPO_ACTIVITY_PLAN") != "1" {
		t.Skip("UZI_REPO_ACTIVITY_PLAN=1 not set; measurement harness only")
	}
	ctx, pool, _ := openRepoActivityLiveDB(t)
	// No cleanup: cascading the 50,000-run delete from users took over ten minutes on a
	// scratch Postgres, so this harness leaves its rows and must run on a throwaway database.
	user, conn := raSeedUserConn(ctx, t, pool, false)
	prefix := "raplan-" + uuid.NewString()[:8] + "/"

	heavy := raSeedRepo(ctx, t, pool, conn, prefix+"heavy", true)
	const heavyRuns = 50000
	if n := raExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at)
		 SELECT gen_random_uuid(), $1, $2, g, 't', 'd', 'completed', 'issue', now() - g * interval '1 minute'
		 FROM generate_series(1, $3) g`, user, heavy, heavyRuns); n != heavyRuns {
		t.Fatalf("inserted %d heavy runs, want %d (a trigger may have dropped rows)", n, heavyRuns)
	}
	for i := 0; i < 20; i++ {
		repo := raSeedRepo(ctx, t, pool, conn, fmt.Sprintf("%ssmall-%02d", prefix, i), i%2 == 0)
		for j := 0; j < 3; j++ {
			raSeedRun(ctx, t, pool, user, repo, j+1, "completed", time.Now().Add(-time.Duration(j+1)*time.Hour))
		}
	}
	raExec(ctx, t, pool, `ANALYZE runs`)
	raExec(ctx, t, pool, `ANALYZE repos`)

	measure := func(label string) {
		rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+adminListEnabledRepoActivity)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, line)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s plan:\n%s", label, strings.Join(plan, "\n"))

		durs := make([]time.Duration, 0, 5)
		for i := 0; i < 5; i++ {
			start := time.Now()
			r, err := pool.Query(ctx, adminListEnabledRepoActivity)
			if err != nil {
				t.Fatal(err)
			}
			for r.Next() {
			}
			r.Close()
			if err := r.Err(); err != nil {
				t.Fatal(err)
			}
			durs = append(durs, time.Since(start))
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		t.Logf("%s: median of 5 = %s (all: %v)", label, durs[2], durs)
	}

	measure("heavy repo ENABLED")
	raExec(ctx, t, pool, `UPDATE repos SET enabled = false WHERE id = $1`, heavy)
	measure("heavy repo DISABLED")
}
