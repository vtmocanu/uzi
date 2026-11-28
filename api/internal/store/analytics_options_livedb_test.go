package store_test

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4780: the analytics repo options list only repos with at least one run in scope, and a
// private DSN must really name the private database.

func TestCapPrivateDSNIgnoresDatabaseAliases(t *testing.T) {
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGSERVICEFILE", "")
	const name = "cap_private_x"
	for _, dsn := range []string{
		"postgres://u@h:5432/shared?dbname=shared",
		"postgres://u@h:5432/shared?database=shared",
		"postgres://u@h:5432/shared?%64atabase=shared",
		"postgres://u@h:5432/shared?dbname=shared&database=shared&sslmode=disable",
		"host=h dbname=shared",
		"host=h database=shared",
	} {
		out, err := capPrivateDSN(dsn, name)
		if err != nil {
			t.Errorf("capPrivateDSN(%q): %v", dsn, err)
			continue
		}
		cfg, err := pgx.ParseConfig(out)
		if err != nil || cfg.Database != name {
			t.Errorf("capPrivateDSN(%q) = %q resolves to %q (err %v), want %q", dsn, out, cfg.Database, err, name)
		}
	}
	for _, dsn := range []string{
		`host=h password=x\`,
		"postgres://u@h:5432/shared#frag",
	} {
		if out, err := capPrivateDSN(dsn, name); err == nil {
			t.Errorf("capPrivateDSN(%q) = %q, want an error", dsn, out)
		}
	}
}

func TestCapPrivateDBRequiresCurrentDBLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := requireCurrentDB(ctx, pool, "cap_"+strings.ReplaceAll(uuid.NewString(), "-", "")[:20]); err == nil {
		t.Fatal("requireCurrentDB accepted a database name the pool is not connected to")
	}
	var actual string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&actual); err != nil {
		t.Fatal(err)
	}
	if err := requireCurrentDB(ctx, pool, actual); err != nil {
		t.Fatalf("requireCurrentDB(%q) = %v, want nil", actual, err)
	}
}

type optRepo struct {
	id   uuid.UUID
	path string
}

// sortedOptRepos orders by path bytes, then id: the statements' ORDER BY ... COLLATE "C", id.
func sortedOptRepos(in []optRepo) []optRepo {
	out := append([]optRepo(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].id.String() < out[j].id.String()
	})
	return out
}

func wantOptRepos(t *testing.T, what string, got []optRepo, want []optRepo) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d repos, want %d", what, len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: row %d = %s (%s), want %s (%s)", what, i, got[i].path, got[i].id, want[i].path, want[i].id)
		}
	}
}

func TestAnalyticsRepoOptionsRunBearingPrivateDBLiveDB(t *testing.T) {
	ctx, pool := capPrivateDB(t)
	f := capFixtureOn(t, ctx, pool)
	q := store.New(pool)
	a, b := f.owner("opt"), f.owner("opt")
	at := time.Date(2026, time.November, 10, 12, 0, 0, 0, time.UTC)

	var aRepos, bRepos []optRepo
	for i := 0; i < 150; i++ {
		id, path := f.repo(a, "opt-a")
		aRepos = append(aRepos, optRepo{id, path})
	}
	for i := 0; i < 100; i++ {
		id, path := f.repo(b, "opt-b")
		bRepos = append(bRepos, optRepo{id, path})
	}
	hasRun := map[uuid.UUID]bool{}
	run := func(owner spendOwner, repo optRepo) uuid.UUID {
		hasRun[repo.id] = true
		return f.run(spendRun{Owner: owner.User, Repo: repo.id, At: at})
	}
	// A: 16 plain run repos, a cross_check-only repo whose checked run lives on another repo of A,
	// and a repo whose only run is B's.
	aMine := map[uuid.UUID]bool{}
	for _, r := range aRepos[:16] {
		run(a, r)
		aMine[r.id] = true
	}
	ccRepo, targetRepo, foreignRunRepo := aRepos[16], aRepos[17], aRepos[18]
	target := run(a, targetRepo)
	aMine[targetRepo.id] = true
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, created_at,
		                   finished_at, status_since, harness, target_run_id, report_only, budget_wall_seconds, cross_check_lane)
		 VALUES ($1, $2, $3, 't', 'd', 'completed', $4, $5, $5, $5, 'codex', $6, true, 600, false)`,
		uuid.New(), a.User, ccRepo.id, runkind.CrossCheck, at, target)
	hasRun[ccRepo.id] = true
	aMine[ccRepo.id] = true
	run(b, foreignRunRepo) // B's run on A's connection: all-users sees it, A and B do not
	// B: 10 repos with B runs, one of them B-only; A has a run on a further repo of B's connection.
	bMine := map[uuid.UUID]bool{}
	for _, r := range bRepos[:10] {
		run(b, r)
		bMine[r.id] = true
	}
	run(a, bRepos[10])
	// A chat run has no repo.
	f.run(spendRun{Owner: a.User, Kind: runkind.Chat, At: at})

	pick := func(in []optRepo, keep func(optRepo) bool) []optRepo {
		var out []optRepo
		for _, r := range in {
			if keep(r) {
				out = append(out, r)
			}
		}
		return sortedOptRepos(out)
	}
	toRepos := func(rows []store.AnalyticsAllRepoOptionsRow) []optRepo {
		out := make([]optRepo, len(rows))
		for i, r := range rows {
			out[i] = optRepo{r.ID, r.PathWithNamespace}
		}
		return out
	}
	toOwnerRepos := func(rows []store.AnalyticsOwnerRepoOptionsRow) []optRepo {
		out := make([]optRepo, len(rows))
		for i, r := range rows {
			out[i] = optRepo{r.ID, r.PathWithNamespace}
		}
		return out
	}
	all := append(append([]optRepo(nil), aRepos...), bRepos...)
	check := func(phase string, wantAll, wantA, wantB []optRepo) {
		t.Helper()
		gotAll, err := q.AnalyticsAllRepoOptions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wantOptRepos(t, phase+" all-users", toRepos(gotAll), wantAll)
		gotA, err := q.AnalyticsOwnerRepoOptions(ctx, a.User)
		if err != nil {
			t.Fatal(err)
		}
		wantOptRepos(t, phase+" owner A", toOwnerRepos(gotA), wantA)
		gotB, err := q.AnalyticsOwnerRepoOptions(ctx, b.User)
		if err != nil {
			t.Fatal(err)
		}
		wantOptRepos(t, phase+" owner B", toOwnerRepos(gotB), wantB)
	}

	wantAll := pick(all, func(r optRepo) bool { return hasRun[r.id] })
	if len(wantAll) != 30 { // 16 plain, cross_check repo, its target repo, B's run on A, 11 on B's connection
		t.Fatalf("seeded %d run-bearing repos, test premise wants 30 (issue #4780 example: 250 repos, 30 with runs)", len(wantAll))
	}
	check("sparse",
		wantAll,
		pick(aRepos, func(r optRepo) bool { return aMine[r.id] }),
		pick(bRepos, func(r optRepo) bool { return bMine[r.id] }))

	// More than 200 run-bearing repos: A now also ran on the repo that only had B's run, and every
	// remaining repo gets a run by its connection's owner.
	run(a, foreignRunRepo)
	for _, r := range aRepos {
		if !hasRun[r.id] {
			run(a, r)
		}
	}
	for _, r := range bRepos {
		if !hasRun[r.id] {
			run(b, r)
		}
	}
	full := sortedOptRepos(all)
	if len(full) != 250 {
		t.Fatalf("seeded %d repos, want 250", len(full))
	}
	gotAll, err := q.AnalyticsAllRepoOptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantOptRepos(t, "capped all-users", toRepos(gotAll), full[:201])
	gotA, err := q.AnalyticsOwnerRepoOptions(ctx, a.User)
	if err != nil {
		t.Fatal(err)
	}
	wantOptRepos(t, "full owner A", toOwnerRepos(gotA), sortedOptRepos(aRepos))
	if len(gotAll) != 201 {
		t.Fatalf("capped all-users = %d rows, want 201", len(gotAll))
	}
}
