package handler

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// Issue #4780: the analytics options list only the repos with at least one run in the scope (any
// time, any kind), while the repo filter of every section still accepts any repo the scope can see.

// anOptionSections are the analytics routes; each takes the repo filter through authorize.
var anOptionSections = []string{"options", "spend", "reliability", "capacity", "quality", "flow"}

type anOptRepo struct {
	id   uuid.UUID
	path string
}

// anSortOptRepos orders by path bytes, then id: the statements' ORDER BY ... COLLATE "C", id.
func anSortOptRepos(in []anOptRepo) []anOptRepo {
	out := append([]anOptRepo(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].path != out[j].path {
			return out[i].path < out[j].path
		}
		return out[i].id.String() < out[j].id.String()
	})
	return out
}

func anOptIDs(in []anOptRepo) []string {
	out := make([]string, len(in))
	for i, r := range in {
		out[i] = r.id.String()
	}
	return out
}

func anOptItemIDs(dto apitypes.AnalyticsOptionsDTO) []string {
	out := make([]string, len(dto.Repos.Items))
	for i, it := range dto.Repos.Items {
		out[i] = it.ID
	}
	return out
}

func anWantOptItems(t *testing.T, what string, dto apitypes.AnalyticsOptionsDTO, want []anOptRepo, truncated bool) {
	t.Helper()
	got, wantIDs := anOptItemIDs(dto), anOptIDs(want)
	if len(got) != len(wantIDs) || dto.Repos.Truncated != truncated {
		t.Fatalf("%s: %d repos truncated=%v, want %d truncated=%v", what, len(got), dto.Repos.Truncated, len(wantIDs), truncated)
	}
	for i := range wantIDs {
		if got[i] != wantIDs[i] {
			t.Fatalf("%s: item %d = %s (%s), want %s (%s)", what, i, got[i], dto.Repos.Items[i].Path, wantIDs[i], want[i].path)
		}
	}
}

// conn seeds a forge connection for user.
func (f *anFixture) conn(user uuid.UUID) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	cliMustExec(f.t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, id, user, []byte{0x1})
	return id
}

// bulkRepos seeds n repos on conn with paths prefix0001..prefixNNNN, returned in path order.
func (f *anFixture) bulkRepos(conn uuid.UUID, prefix string, n int) []anOptRepo {
	f.t.Helper()
	base := 6_000_000 + anSeq.Add(int64(n))
	rows, err := f.pool.Query(context.Background(),
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 SELECT gen_random_uuid(), $1, $2 + g, $3 || lpad(g::text, 4, '0'), 'https://forge.e2e/x', 'main', true
		 FROM generate_series(1, $4::int) g
		 RETURNING id, path_with_namespace`, conn, base, prefix, n)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []anOptRepo
	for rows.Next() {
		var r anOptRepo
		if err := rows.Scan(&r.id, &r.path); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		f.t.Fatal(err)
	}
	return anSortOptRepos(out)
}

// anSeedRunsOn seeds one completed issue run by user on each repo, created at at (no usage rows).
func anSeedRunsOn(t *testing.T, pool *pgxpool.Pool, user uuid.UUID, repos []uuid.UUID, at time.Time) {
	t.Helper()
	base := 5_000_000 + anSeq.Add(int64(len(repos)))
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, status_since, harness)
		 SELECT gen_random_uuid(), $1, r.id, $2 + r.n, 't', 'd', 'completed', 'issue', $3, $3, 'claude'
		 FROM unnest($4::uuid[]) WITH ORDINALITY AS r(id, n)`, user, base, at, repos)
}

func anRepoIDs(in []anOptRepo) []uuid.UUID {
	out := make([]uuid.UUID, len(in))
	for i, r := range in {
		out[i] = r.id
	}
	return out
}

// anRepoFilterMatrix checks the repo filter on every section for one scope: each accepted repo
// is a 200 and each rejected one the 400 "invalid repo".
func (f *anFixture) anRepoFilterMatrix(viewer uuid.UUID, base string, accepted map[string]uuid.UUID, rejected uuid.UUID) {
	f.t.Helper()
	for _, section := range anOptionSections {
		path := strings.ReplaceAll(base, "{section}", section)
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		for name, repo := range accepted {
			if rec := f.cookie(viewer, path+sep+"repo="+repo.String()); rec.Code != http.StatusOK {
				f.t.Fatalf("%s %s repo %s = %d %s, want 200", path, name, repo, rec.Code, rec.Body.String())
			}
		}
		rec := f.cookie(viewer, path+sep+"repo="+rejected.String())
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid repo") {
			f.t.Fatalf("%s unauthorized repo %s = %d %s, want 400 invalid repo", path, rejected, rec.Code, rec.Body.String())
		}
	}
}

// Owner and admin user=<uuid>: exactly the user's connection repos with that user's runs, ordered;
// a run-less repo, a repo only another user ran on and a repo off the user's connections are
// absent. The window is ignored: a repo whose only run is 120 days old is listed, byte-identically,
// with no window, 7d and 30d.
func TestAnalyticsOptionsRunBearingScopesLiveDB(t *testing.T) {
	f := newAnFixture(t, 2057)
	admin, u, other := f.user(true), f.user(false), f.user(false)
	conn := f.conn(u)
	// Witness tags sort before the run-bearing ones ("a…" < "m…"), so a leak cannot hide past a cut.
	f.repoOn(conn, "a4780-runless")
	otherOnly, _ := f.repoOn(conn, "a4780-otheronly")
	recent, recentPath := f.repoOn(conn, "m4780-recent")
	old, oldPath := f.repoOn(conn, "m4780-old")
	foreign, _ := f.repoOwnedBy(other, "a4780-foreign")
	f.run(u, recent, f.now.Add(-3*time.Hour), "1.000000")
	f.run(u, old, f.now.Add(-120*24*time.Hour), "1.000000")
	f.run(other, otherOnly, f.now.Add(-3*time.Hour), "1.000000")
	f.run(u, foreign, f.now.Add(-3*time.Hour), "1.000000")
	want := anSortOptRepos([]anOptRepo{{recent, recentPath}, {old, oldPath}})

	for _, c := range []struct {
		name   string
		viewer uuid.UUID
		base   string
	}{
		{"owner", u, "/api/analytics/options"},
		{"admin user=uuid", admin, "/api/admin/analytics/options?user=" + u.String()},
	} {
		t.Run(c.name, func(t *testing.T) {
			first := f.cookie(c.viewer, c.base)
			anWantOptItems(t, c.name, anDecodeOptions(t, first), want, false)
			sep := "?"
			if strings.Contains(c.base, "?") {
				sep = "&"
			}
			for _, w := range []string{"7d", "30d"} {
				rec := f.cookie(c.viewer, c.base+sep+"window="+w)
				if rec.Code != http.StatusOK || rec.Body.String() != first.Body.String() {
					t.Fatalf("%s window=%s differs from no window (%d):\n%s\n%s", c.name, w, rec.Code, rec.Body.String(), first.Body.String())
				}
			}
		})
	}
}

// AC4 on the owner and admin user=<uuid> scopes: the list is capped at 200 of 205 run-bearing repos,
// and a repo cut off the list, a run-less repo and a listed one are all accepted as a filter on every
// section; another user's repo is the 400.
func TestAnalyticsOptionsUnlistedRepoFilterLiveDB(t *testing.T) {
	f := newCapFixture(t, 2058, false)
	admin, u, other := f.user(true), f.user(false), f.user(false)
	conn := f.conn(u)
	repos := f.bulkRepos(conn, "an4780-cap-"+uuid.NewString()[:8]+"-", 205)
	anSeedRunsOn(t, f.pool, u, anRepoIDs(repos), f.now.Add(-3*time.Hour))
	runless, _ := f.repoOn(conn, "a4780-runless")
	foreign, _ := f.repoOwnedBy(other, "a4780-foreign")
	f.run(other, foreign, f.now.Add(-3*time.Hour), "1.000000")
	cut := repos[len(repos)-1]

	accepted := map[string]uuid.UUID{"listed": repos[0].id, "cut off": cut.id, "run-less": runless}
	f.anRepoFilterMatrix(u, "/api/analytics/{section}", accepted, foreign)
	f.anRepoFilterMatrix(admin, "/api/admin/analytics/{section}?user="+u.String(), accepted, foreign)

	anWantOptItems(t, "owner", anDecodeOptions(t, f.cookie(u, "/api/analytics/options")), repos[:200], true)
	anWantOptItems(t, "admin user=uuid", anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options?user="+u.String())), repos[:200], true)
}

// Admin all-users, in a database of the test's own: 250 repos, 30 run-bearing (a cross_check-only
// repo and a repo whose only run is 120 days old among them, plus a repo-less chat run) list exactly
// those 30, untruncated, for every window including all; with 231 run-bearing the list is the first
// 200, truncated, and the cut-off, run-less and unknown repos behave as AC4 says.
func TestAnalyticsOptionsAllUsersPrivateDBLiveDB(t *testing.T) {
	f := newCapFixture(t, 2059, true)
	admin, a, b := f.user(true), f.user(false), f.user(false)
	aRepos := f.bulkRepos(f.conn(a), "an4780-a-", 150)
	bRepos := f.bulkRepos(f.conn(b), "an4780-b-", 100)
	at := f.now.Add(-3 * time.Hour)
	hasRun := map[uuid.UUID]bool{}
	runOn := func(user uuid.UUID, r anOptRepo, when time.Time) uuid.UUID {
		hasRun[r.id] = true
		return anSeedRun(t, f.pool, user, r.id, when, "1.000000")
	}
	for _, r := range aRepos[1:16] {
		runOn(a, r, at)
	}
	runOn(a, aRepos[0], f.now.Add(-120*24*time.Hour)) // the window witness
	ccRepo, targetRepo := aRepos[16], aRepos[17]
	target := runOn(a, targetRepo, at)
	cliMustExec(t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind, created_at,
		                   finished_at, status_since, harness, target_run_id, report_only, budget_wall_seconds, cross_check_lane)
		 VALUES ($1, $2, $3, 't', 'd', 'completed', $4, $5, $5, $5, 'codex', $6, true, 600, false)`,
		uuid.New(), a, ccRepo.id, runkind.CrossCheck, at, target)
	hasRun[ccRepo.id] = true
	runOn(b, aRepos[18], at) // B's run on A's connection
	for _, r := range bRepos[:10] {
		runOn(b, r, at)
	}
	runOn(a, bRepos[10], at)
	cliMustExec(t, f.pool,
		`INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind, created_at, status_since, harness)
		 VALUES ($1, $2, 't', 'd', 'completed', $3, $4, $4, 'claude')`, uuid.New(), a, runkind.Chat, at)

	all := append(append([]anOptRepo(nil), aRepos...), bRepos...)
	bearing := func() []anOptRepo {
		var out []anOptRepo
		for _, r := range all {
			if hasRun[r.id] {
				out = append(out, r)
			}
		}
		return anSortOptRepos(out)
	}
	sparse := bearing()
	if len(sparse) != 30 {
		t.Fatalf("seeded %d run-bearing repos, test premise wants 30", len(sparse))
	}
	f.anRepoFilterMatrix(admin, "/api/admin/analytics/{section}",
		map[string]uuid.UUID{"listed": sparse[0].id, "run-less": aRepos[149].id}, uuid.New())
	first := f.cookie(admin, "/api/admin/analytics/options")
	anWantOptItems(t, "sparse all-users", anDecodeOptions(t, first), sparse, false)
	for _, w := range []string{"7d", "30d", "all"} {
		rec := f.cookie(admin, "/api/admin/analytics/options?window="+w)
		if rec.Code != http.StatusOK || rec.Body.String() != first.Body.String() {
			t.Fatalf("all-users window=%s differs from no window (%d):\n%s\n%s", w, rec.Code, rec.Body.String(), first.Body.String())
		}
	}

	// 201 more run-bearing repos: 231 in all, so the list is the first 200 and truncated.
	var more []uuid.UUID
	for _, r := range anSortOptRepos(all) {
		if len(more) == 201 {
			break
		}
		if !hasRun[r.id] {
			more = append(more, r.id)
			hasRun[r.id] = true
		}
	}
	anSeedRunsOn(t, f.pool, a, more, at)
	full := bearing()
	if len(full) != 231 {
		t.Fatalf("seeded %d run-bearing repos, want 231", len(full))
	}
	anWantOptItems(t, "capped all-users", anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options")), full[:200], true)

	var runless uuid.UUID
	for _, r := range all {
		if !hasRun[r.id] {
			runless = r.id
			break
		}
	}
	if runless == uuid.Nil {
		t.Fatal("test premise: a run-less repo remains")
	}
	accepted := map[string]uuid.UUID{"listed": full[0].id, "cut off": full[230].id, "run-less": runless}
	f.anRepoFilterMatrix(admin, "/api/admin/analytics/{section}", accepted, uuid.New())
}
