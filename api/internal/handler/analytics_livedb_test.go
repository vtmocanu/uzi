package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4684: the analytics routes through the real router and a real database. The LiveDB
// database is shared across tests and the all-scope statements have no owner predicate, so each
// test that asserts all-scope figures claims a calendar year of its own (no other test seeds
// runs there), clears it, and runs with a fixed clock inside it. Years 2031 and up are used
// here; the store analytics tests use 2003-2005.

var anSeq atomic.Int64

// anFixture is one test's handler, router and fixed clock.
type anFixture struct {
	t      *testing.T
	h      *Handler
	router http.Handler
	pool   *pgxpool.Pool
	now    time.Time
	users  []uuid.UUID
}

func newAnFixture(t *testing.T, year int) *anFixture {
	t.Helper()
	h, router, pool := cliLiveDB(t)
	h.analyticsDeps = newAnalyticsDeps(pool)
	f := &anFixture{t: t, h: h, router: router, pool: pool, now: time.Date(year, 6, 15, 12, 0, 0, 0, time.UTC)}
	h.now = func() time.Time { return f.now }
	// Deletions are limited to rows of fixture-created users (cliSeedUser's cli-<id>@e2e
	// emails), so they can never remove an unrelated run that happens to sit in the year.
	cliMustExec(t, pool, `DELETE FROM runs WHERE created_at >= make_timestamptz($1, 1, 1, 0, 0, 0, 'UTC')
	                                          AND created_at <  make_timestamptz($1 + 1, 1, 1, 0, 0, 0, 'UTC')
	                                          AND user_id IN (SELECT id FROM users WHERE email LIKE 'cli-%@e2e')`, year)
	t.Cleanup(func() {
		cliMustExec(t, pool, `DELETE FROM runs WHERE user_id = ANY($1)`, f.users)
		cliMustExec(t, pool, `DELETE FROM users WHERE id = ANY($1)`, f.users)
	})
	return f
}

func (f *anFixture) user(isAdmin bool) uuid.UUID {
	f.t.Helper()
	id := cliSeedUser(f.t, f.pool, isAdmin)
	f.users = append(f.users, id)
	return id
}

// repoOwnedBy seeds a connection for user and one repo on it.
func (f *anFixture) repoOwnedBy(user uuid.UUID, tag string) (uuid.UUID, string) {
	f.t.Helper()
	conn := uuid.New()
	cliMustExec(f.t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, user, []byte{0x1})
	return f.repoOn(conn, tag)
}

func (f *anFixture) repoOn(conn uuid.UUID, tag string) (uuid.UUID, string) {
	f.t.Helper()
	id := uuid.New()
	path := fmt.Sprintf("an4684-%s-%s", tag, id)
	cliMustExec(f.t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, $3, $4, 'https://forge.e2e/x', 'main', true)`, id, conn, 6_000_000+anSeq.Add(1), path)
	return id, path
}

// run seeds a completed issue run created at at with one metered usage row costing cost.
func (f *anFixture) run(user, repo uuid.UUID, at time.Time, cost string) uuid.UUID {
	f.t.Helper()
	return anSeedRun(f.t, f.pool, user, repo, at, cost)
}

func anSeedRun(t *testing.T, pool *pgxpool.Pool, user, repo uuid.UUID, at time.Time, cost string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, status_since, harness)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', 'issue', $5, $5, 'claude')`, id, user, repo, 5_000_000+anSeq.Add(1), at)
	cliMustExec(t, pool,
		`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, usage_basis,
		     input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
		 VALUES ($1, $2, 'claude-sonnet', 0, 0, 'per_leg', 100, 50, 10, 20, $3::numeric, 'metered')`,
		id, fmt.Sprintf("an-%d", anSeq.Add(1)), cost)
	return id
}

func (f *anFixture) cookie(user uuid.UUID, path string) *httptest.ResponseRecorder {
	f.t.Helper()
	return cookieReq(f.t, f.router, http.MethodGet, path, cliMintJWT(f.t, f.pool, user), "")
}

func anDecodeSpend(t *testing.T, rec *httptest.ResponseRecorder) apitypes.AnalyticsSpendDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var dto apitypes.AnalyticsSpendDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		t.Fatalf("decode spend: %v: %s", err, rec.Body.String())
	}
	return dto
}

func anDecodeOptions(t *testing.T, rec *httptest.ResponseRecorder) apitypes.AnalyticsOptionsDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var dto apitypes.AnalyticsOptionsDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		t.Fatalf("decode options: %v: %s", err, rec.Body.String())
	}
	return dto
}

func anNear(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s = %v, want %v (tolerance 1e-6)", label, got, want)
	}
}

func anKeyList(g []apitypes.AnalyticsSplitGroupDTO) []string {
	var out []string
	for _, x := range g {
		if x.Key != nil {
			out = append(out, *x.Key)
		}
	}
	return out
}

func anGroupKeys(g []apitypes.AnalyticsSplitGroupDTO) map[string]apitypes.AnalyticsSplitGroupDTO {
	out := map[string]apitypes.AnalyticsSplitGroupDTO{}
	for _, x := range g {
		if x.Key != nil {
			out[*x.Key] = x
		}
	}
	return out
}

func TestAnalyticsCredentialsLiveDB(t *testing.T) {
	f := newAnFixture(t, 2031)
	admin, member := f.user(true), f.user(false)
	adminRO := cliMintToken(t, f.pool, admin, clitoken.ScopeAdminRO)
	adminDefault := cliMintToken(t, f.pool, admin, clitoken.ScopeUser)
	memberToken := cliMintToken(t, f.pool, member, clitoken.ScopeUser)
	adminJWT, memberJWT := cliMintJWT(t, f.pool, admin), cliMintJWT(t, f.pool, member)

	for _, path := range []string{"/api/admin/analytics/options", "/api/admin/analytics/spend"} {
		if rec := bearerReq(f.router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s no credential = %d, want 401", path, rec.Code)
		}
		if rec := cookieReq(t, f.router, http.MethodGet, path, adminJWT, ""); rec.Code != http.StatusOK {
			t.Fatalf("%s admin cookie = %d %s, want 200", path, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(f.router, http.MethodGet, path, adminRO); rec.Code != http.StatusOK {
			t.Fatalf("%s admin_ro bearer = %d %s, want 200", path, rec.Code, rec.Body.String())
		}
		if rec := bearerReq(f.router, http.MethodGet, path, adminDefault); rec.Code != http.StatusForbidden {
			t.Fatalf("%s admin default-scope bearer = %d, want 403", path, rec.Code)
		}
		if rec := cookieReq(t, f.router, http.MethodGet, path, memberJWT, ""); rec.Code != http.StatusForbidden {
			t.Fatalf("%s member cookie = %d, want 403", path, rec.Code)
		}
		if rec := bearerReq(f.router, http.MethodGet, path, memberToken); rec.Code != http.StatusForbidden {
			t.Fatalf("%s member bearer = %d, want 403", path, rec.Code)
		}
	}
	for _, path := range []string{"/api/analytics/options", "/api/analytics/spend"} {
		if rec := bearerReq(f.router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s no credential = %d, want 401", path, rec.Code)
		}
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"admin cookie":    cookieReq(t, f.router, http.MethodGet, path, adminJWT, ""),
			"member cookie":   cookieReq(t, f.router, http.MethodGet, path, memberJWT, ""),
			"member bearer":   bearerReq(f.router, http.MethodGet, path, memberToken),
			"admin default":   bearerReq(f.router, http.MethodGet, path, adminDefault),
			"admin_ro bearer": bearerReq(f.router, http.MethodGet, path, adminRO),
		} {
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s = %d %s, want 200", path, name, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestAnalyticsUnknownSectionIs404LiveDB(t *testing.T) {
	f := newAnFixture(t, 2031)
	admin := f.user(true)
	jwt := cliMintJWT(t, f.pool, admin)
	for _, path := range []string{"/api/analytics/trends", "/api/admin/analytics/trends", "/api/analytics/", "/api/admin/analytics"} {
		if rec := cookieReq(t, f.router, http.MethodGet, path, jwt, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestAnalyticsOwnerScopeAndRepoMaskingLiveDB(t *testing.T) {
	f := newAnFixture(t, 2032)
	admin, u, foreignOwner := f.user(true), f.user(false), f.user(false)
	repoAdmin, _ := f.repoOwnedBy(admin, "admin")
	repoU, _ := f.repoOwnedBy(u, "u")
	repoF, pathF := f.repoOwnedBy(foreignOwner, "foreign")

	at := f.now.Add(-2 * time.Hour)
	f.run(admin, repoAdmin, at, "1.000000")
	f.run(admin, repoF, at, "2.000000") // the admin's own run on a repo they have no connection to
	f.run(u, repoU, at, "3.000000")
	f.run(u, repoF, at, "4.000000") // a shared repo: owned by foreignOwner, used by u

	// An admin on the owner route stays in their own scope.
	rec := f.cookie(admin, "/api/analytics/spend?split=repo")
	adminOwn := anDecodeSpend(t, rec)
	anNear(t, "admin owner-route total", adminOwn.Totals.Current.CostUSD, 3)
	groups := anGroupKeys(adminOwn.Split.Groups)
	if g, ok := groups[repoAdmin.String()]; !ok || g.ID == nil || g.Label == nil {
		t.Fatalf("own repo group missing or hidden: group keys %v", anKeyList(adminOwn.Split.Groups))
	}
	masked, ok := groups["inaccessible_repo"]
	if !ok || masked.ID != nil || masked.Label != nil {
		t.Fatalf("foreign repo must be masked as inaccessible_repo with null identifiers: group keys %v", anKeyList(adminOwn.Split.Groups))
	}
	anNear(t, "masked group cost", masked.Totals.CostUSD, 2)
	if strings.Contains(rec.Body.String(), repoF.String()) || strings.Contains(rec.Body.String(), pathF) {
		t.Fatalf("owner-route body leaks the foreign repo: %s", rec.Body.String())
	}

	// The same 400, byte for byte, for a repo the admin cannot see and one that does not exist.
	foreign := f.cookie(admin, "/api/analytics/spend?repo="+repoU.String())
	unknown := f.cookie(admin, "/api/analytics/spend?repo="+uuid.NewString())
	if foreign.Code != http.StatusBadRequest || unknown.Code != http.StatusBadRequest {
		t.Fatalf("foreign repo = %d, unknown repo = %d, want 400 and 400", foreign.Code, unknown.Code)
	}
	if foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("repo 400s differ: %q vs %q", foreign.Body.String(), unknown.Body.String())
	}
	if rec := f.cookie(admin, "/api/analytics/spend?user="+u.String()); rec.Code != http.StatusBadRequest {
		t.Fatalf("owner route user param = %d, want 400", rec.Code)
	}
	if rec := f.cookie(admin, "/api/analytics/options?repo="+repoU.String()); rec.Code != http.StatusBadRequest {
		t.Fatalf("owner options with a foreign repo = %d, want 400", rec.Code)
	}

	// Shared repo: the admin selecting u reveals repoF; u on the owner route sees it masked.
	revealed := anDecodeSpend(t, f.cookie(admin, "/api/admin/analytics/spend?split=repo&user="+u.String()))
	anNear(t, "admin user=u total", revealed.Totals.Current.CostUSD, 7)
	rg, ok := anGroupKeys(revealed.Split.Groups)[repoF.String()]
	if !ok || rg.Label == nil || *rg.Label != pathF || rg.ID == nil || *rg.ID != repoF.String() {
		t.Fatalf("admin user=u must reveal the shared repo: group keys %v", anKeyList(revealed.Split.Groups))
	}
	ownerRec := f.cookie(u, "/api/analytics/spend?split=repo")
	asU := anDecodeSpend(t, ownerRec)
	anNear(t, "u owner-route total", asU.Totals.Current.CostUSD, 7)
	if g, ok := anGroupKeys(asU.Split.Groups)["inaccessible_repo"]; !ok || g.ID != nil || g.Label != nil {
		t.Fatalf("owner route as u must mask the shared repo: group keys %v", anKeyList(asU.Split.Groups))
	}
	if strings.Contains(ownerRec.Body.String(), repoF.String()) || strings.Contains(ownerRec.Body.String(), pathF) {
		t.Fatalf("owner-route body leaks the shared repo: %s", ownerRec.Body.String())
	}
	for _, tr := range asU.TopRuns {
		if tr.RepoKey != repoU.String() && tr.RepoKey != "inaccessible_repo" {
			t.Fatalf("top run repo_key %q on the owner route, want the owned repo or inaccessible_repo", tr.RepoKey)
		}
		if tr.RepoKey == "inaccessible_repo" && (tr.RepoID != nil || tr.RepoPath != nil) {
			t.Fatalf("masked top run carries repo identifiers: %+v", tr)
		}
	}

	// The shared-repo filter asymmetry: u's options list only u's own repos, so an admin
	// filtering u's scope by the shared repo gets the unknown-repo 400.
	asym := f.cookie(admin, "/api/admin/analytics/spend?user="+u.String()+"&repo="+repoF.String())
	if asym.Code != http.StatusBadRequest || asym.Body.String() != unknown.Body.String() {
		t.Fatalf("admin user=u&repo=shared = %d %s, want the unknown-repo 400 %s", asym.Code, asym.Body.String(), unknown.Body.String())
	}
}

func TestAnalyticsAdminUserScopesLiveDB(t *testing.T) {
	f := newAnFixture(t, 2033)
	admin, a, b := f.user(true), f.user(false), f.user(false)
	repoA, _ := f.repoOwnedBy(a, "a")
	repoB, _ := f.repoOwnedBy(b, "b")
	at := f.now.Add(-3 * time.Hour)
	f.run(a, repoA, at, "2.500000")
	f.run(b, repoB, at, "9.750000")

	spend := func(q string) apitypes.AnalyticsSpendDTO {
		return anDecodeSpend(t, f.cookie(admin, "/api/admin/analytics/spend?"+q))
	}
	sa, sb, sall := spend("user="+a.String()), spend("user="+b.String()), spend("user=all")
	anNear(t, "user=A", sa.Totals.Current.CostUSD, 2.5)
	anNear(t, "user=B", sb.Totals.Current.CostUSD, 9.75)
	anNear(t, "user=all", sall.Totals.Current.CostUSD, 12.25)
	if sa.Scope.User != a.String() || sa.Scope.Route != "admin" || sall.Scope.User != "all" {
		t.Fatalf("scopes = %+v %+v", sa.Scope, sall.Scope)
	}

	// Grouping by user exists only for the all-users scope.
	byUser := anDecodeSpend(t, f.cookie(admin, "/api/admin/analytics/spend?user=all&split=user&breakdown=user"))
	keys := anGroupKeys(byUser.Split.Groups)
	for _, u := range []uuid.UUID{a, b} {
		g, ok := keys[u.String()]
		if !ok || g.Label == nil || !strings.HasPrefix(*g.Label, "cli-") {
			t.Fatalf("split=user missing user %s: group keys %v", u, anKeyList(byUser.Split.Groups))
		}
	}
	anNear(t, "split user A", keys[a.String()].Totals.CostUSD, 2.5)
	if byUser.Breakdown.Dimension != "user" {
		t.Fatalf("breakdown dimension = %q, want user", byUser.Breakdown.Dimension)
	}
	if rec := f.cookie(admin, "/api/admin/analytics/spend?user="+a.String()+"&split=user"); rec.Code != http.StatusBadRequest {
		t.Fatalf("split=user with a selected user = %d, want 400", rec.Code)
	}

	// An omitted user is byte-identical to user=all, for Spend and for Options.
	for _, section := range []string{"spend", "options"} {
		omitted := f.cookie(admin, "/api/admin/analytics/"+section)
		explicit := f.cookie(admin, "/api/admin/analytics/"+section+"?user=all")
		if omitted.Code != http.StatusOK || omitted.Body.String() != explicit.Body.String() {
			t.Fatalf("%s: omitted user differs from user=all (%d):\n%s\n%s", section, omitted.Code, omitted.Body.String(), explicit.Body.String())
		}
	}
	if opt := anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options")); opt.SelectedUser != "all" {
		t.Fatalf("omitted user selected_user = %q, want all", opt.SelectedUser)
	}

	// Spend carries selected_user with Options semantics: all, or the selected user's id.
	for name, got := range map[string]string{
		"omitted user": anDecodeSpend(t, f.cookie(admin, "/api/admin/analytics/spend")).SelectedUser,
		"user=all":     sall.SelectedUser,
	} {
		if got != "all" {
			t.Fatalf("spend %s selected_user = %q, want all", name, got)
		}
	}
	if sa.SelectedUser != a.String() || sb.SelectedUser != b.String() {
		t.Fatalf("spend selected_user for a/b = %q/%q, want %s/%s", sa.SelectedUser, sb.SelectedUser, a, b)
	}
	if got := anDecodeSpend(t, f.cookie(admin, "/api/analytics/spend")).SelectedUser; got != admin.String() {
		t.Fatalf("owner-route spend selected_user = %q, want the caller %s", got, admin)
	}

	// Options with a selected user narrow the repos and keep the user list.
	opt := anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options?user="+a.String()))
	if opt.SelectedUser != a.String() || opt.Scope.User != a.String() {
		t.Fatalf("selected user = %q scope %+v, want %s", opt.SelectedUser, opt.Scope, a)
	}
	if len(opt.Repos.Items) != 1 || opt.Repos.Items[0].ID != repoA.String() {
		t.Fatalf("repos for user A = %+v, want only %s", opt.Repos.Items, repoA)
	}
	if opt.Users == nil || opt.UsersReason != nil {
		t.Fatalf("admin options must list users: present=%v reason %v", opt.Users != nil, opt.UsersReason)
	}
	var sawB bool
	for _, u := range opt.Users.Items {
		sawB = sawB || u.ID == b.String()
	}
	if !sawB && !opt.Users.Truncated {
		t.Fatalf("user list lacks user B and is not truncated (%d items)", len(opt.Users.Items))
	}

	// Authority: an unknown user and another user's repo are 400s; the repo one matches the
	// unknown-repo response byte for byte.
	if rec := f.cookie(admin, "/api/admin/analytics/spend?user="+uuid.NewString()); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown user = %d, want 400", rec.Code)
	}
	if rec := f.cookie(admin, "/api/admin/analytics/options?user="+uuid.NewString()); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown user options = %d, want 400", rec.Code)
	}
	crossRepo := f.cookie(admin, "/api/admin/analytics/spend?user="+a.String()+"&repo="+repoB.String())
	unknownRepo := f.cookie(admin, "/api/admin/analytics/spend?user="+a.String()+"&repo="+uuid.NewString())
	if crossRepo.Code != http.StatusBadRequest || crossRepo.Body.String() != unknownRepo.Body.String() {
		t.Fatalf("user=A&repo=B's = %d %s, want the unknown-repo 400 %s", crossRepo.Code, crossRepo.Body.String(), unknownRepo.Body.String())
	}
	// Admin-all accepts any existing repo.
	if rec := f.cookie(admin, "/api/admin/analytics/spend?repo="+repoB.String()); rec.Code != http.StatusOK {
		t.Fatalf("admin-all with an existing repo = %d, want 200", rec.Code)
	}
}

// Owner decision (#4684): the all-time window is not offered for admin all-users Spend.
func TestAnalyticsAllUsersWindowChoicesLiveDB(t *testing.T) {
	f := newAnFixture(t, 2039)
	admin, owner := f.user(true), f.user(false)

	for _, c := range []struct {
		path string
		want []string
	}{
		{"/api/admin/analytics/options", []string{"7d", "30d", "90d"}},
		{"/api/admin/analytics/options?user=all", []string{"7d", "30d", "90d"}},
		{"/api/admin/analytics/options?user=" + owner.String(), []string{"7d", "30d", "90d", "all"}},
	} {
		if got := anDecodeOptions(t, f.cookie(admin, c.path)).Windows; !slices.Equal(got, c.want) {
			t.Fatalf("%s windows = %v, want %v", c.path, got, c.want)
		}
	}
	if got, want := anDecodeOptions(t, f.cookie(owner, "/api/analytics/options")).Windows, []string{"7d", "30d", "90d", "all"}; !slices.Equal(got, want) {
		t.Fatalf("owner windows = %v, want %v", got, want)
	}

	omitted := f.cookie(admin, "/api/admin/analytics/spend?window=all")
	explicit := f.cookie(admin, "/api/admin/analytics/spend?window=all&user=all")
	if omitted.Code != http.StatusBadRequest || explicit.Code != http.StatusBadRequest {
		t.Fatalf("admin all-users window=all = %d and %d, want 400 and 400", omitted.Code, explicit.Code)
	}
	if omitted.Body.String() != explicit.Body.String() {
		t.Fatalf("400 bodies differ: %q vs %q", omitted.Body.String(), explicit.Body.String())
	}
}

// window=all for one user: the run sits more than 30 days back, so the default window misses
// it while the all-time window reaches it, from the Monday of its ISO week (2039-04-01 is a Friday).
func TestAnalyticsAllTimeWindowSingleUserLiveDB(t *testing.T) {
	f := newAnFixture(t, 2039)
	admin, owner := f.user(true), f.user(false)
	repo, _ := f.repoOwnedBy(owner, "alltime")
	f.run(owner, repo, time.Date(2039, 4, 1, 9, 0, 0, 0, time.UTC), "4.250000")
	wantStart := time.Date(2039, 3, 28, 0, 0, 0, 0, time.UTC)

	for _, c := range []struct {
		name, base, selected string
		viewer               uuid.UUID
	}{
		{"owner", "/api/analytics/spend", owner.String(), owner},
		{"admin user=uuid", "/api/admin/analytics/spend?user=" + owner.String() + "&", owner.String(), admin},
	} {
		t.Run(c.name, func(t *testing.T) {
			sep := "?"
			if strings.Contains(c.base, "?") {
				sep = ""
			}
			rec := f.cookie(c.viewer, c.base+sep+"window=all")
			dto := anDecodeSpend(t, rec)
			if dto.Window != "all" {
				t.Fatalf("window = %q, want all", dto.Window)
			}
			if dto.HistoryStart == nil || !dto.HistoryStart.Equal(wantStart) {
				t.Fatalf("history_start = %v, want %v", dto.HistoryStart, wantStart)
			}
			if !dto.From.Equal(*dto.HistoryStart) {
				t.Fatalf("from = %v, want history_start %v", dto.From, dto.HistoryStart)
			}
			if dto.BucketWidthDays != 7 {
				t.Fatalf("bucket_width_days = %d, want 7", dto.BucketWidthDays)
			}
			if dto.Totals.Current.Runs != 1 {
				t.Fatalf("runs = %d, want 1", dto.Totals.Current.Runs)
			}
			anNear(t, "cost", dto.Totals.Current.CostUSD, 4.25)
			if dto.SelectedUser != c.selected {
				t.Fatalf("selected_user = %q, want %s", dto.SelectedUser, c.selected)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if _, present := raw["previous_from"]; present {
				t.Fatalf("previous_from present for window=all: %s", raw["previous_from"])
			}

			// The default 30d window does not reach the run.
			def := anDecodeSpend(t, f.cookie(c.viewer, strings.TrimSuffix(c.base, "&")))
			if def.Totals.Current.Runs != 0 {
				t.Fatalf("30d runs = %d, want 0", def.Totals.Current.Runs)
			}
		})
	}
}

func TestAnalyticsOptionsTruncationLiveDB(t *testing.T) {
	f := newAnFixture(t, 2034)
	owner := f.user(false)
	conn := uuid.New()
	cliMustExec(t, f.pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, owner, []byte{0x1})
	prefix := "an4684-trunc-" + uuid.NewString()[:8] + "-"
	cliMustExec(t, f.pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 SELECT gen_random_uuid(), $1, 7000000 + g, $2 || lpad(g::text, 4, '0'), 'https://forge.e2e/x', 'main', true
		 FROM generate_series(1, 205) g`, conn, prefix)
	var lastID uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM repos WHERE connection_id = $1 AND path_with_namespace = $2`,
		conn, prefix+"0205").Scan(&lastID); err != nil {
		t.Fatal(err)
	}

	opt := anDecodeOptions(t, f.cookie(owner, "/api/analytics/options"))
	if len(opt.Repos.Items) != 200 || !opt.Repos.Truncated {
		t.Fatalf("repos = %d truncated=%v, want 200 and true", len(opt.Repos.Items), opt.Repos.Truncated)
	}
	for _, it := range opt.Repos.Items {
		if it.ID == lastID.String() {
			t.Fatal("the 205th repo must be cut from the list")
		}
	}
	if opt.Users != nil || opt.UsersReason == nil || *opt.UsersReason != "not_applicable" {
		t.Fatalf("owner options users present=%v reason %v, want null with not_applicable", opt.Users != nil, opt.UsersReason)
	}
	if opt.Scope.Route != "owner" || opt.SelectedUser != owner.String() {
		t.Fatalf("owner options scope = %+v selected %q", opt.Scope, opt.SelectedUser)
	}
	for _, list := range [][]string{opt.Windows, opt.Kinds, opt.Harnesses, opt.Splits, opt.Breakdowns} {
		if len(list) == 0 {
			t.Fatalf("an enum list is empty: windows %v kinds %v harnesses %v splits %v breakdowns %v", opt.Windows, opt.Kinds, opt.Harnesses, opt.Splits, opt.Breakdowns)
		}
	}
	for _, s := range opt.Splits {
		if s == "user" {
			t.Fatalf("owner options offer user grouping: %v", opt.Splits)
		}
	}

	// A valid repo that was cut off the list is still accepted as a filter, on both routes.
	if rec := f.cookie(owner, "/api/analytics/spend?repo="+lastID.String()); rec.Code != http.StatusOK {
		t.Fatalf("truncated-off repo filter = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := f.cookie(owner, "/api/analytics/options?repo="+lastID.String()); rec.Code != http.StatusOK {
		t.Fatalf("truncated-off repo on options = %d, want 200", rec.Code)
	}
}

func TestAnalyticsUsersListTruncationLiveDB(t *testing.T) {
	f := newAnFixture(t, 2035)
	admin := f.user(true)
	prefix := "0an4684-trunc-" + uuid.NewString()[:8] + "-"
	t.Cleanup(func() {
		cliMustExec(t, f.pool, `DELETE FROM users WHERE email LIKE $1`, prefix+"%")
	})
	cliMustExec(t, f.pool,
		`INSERT INTO users (id, email, password_hash, is_admin, vault_mode)
		 SELECT gen_random_uuid(), $1 || lpad(g::text, 4, '0') || '@e2e', 'x', false, 'unprotected'
		 FROM generate_series(1, 205) g`, prefix)
	var lastID uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM users WHERE email = $1`, prefix+"0205@e2e").Scan(&lastID); err != nil {
		t.Fatal(err)
	}

	opt := anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options"))
	if opt.Users == nil || len(opt.Users.Items) != 200 || !opt.Users.Truncated {
		t.Fatalf("users = %+v, want 200 items and truncated", opt.Users)
	}
	for _, it := range opt.Users.Items {
		if it.ID == lastID.String() {
			t.Fatal("the 205th user must be cut from the list")
		}
	}
	// A valid user that was cut off the list is still accepted as a selection.
	if rec := f.cookie(admin, "/api/admin/analytics/spend?user="+lastID.String()); rec.Code != http.StatusOK {
		t.Fatalf("truncated-off user = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if rec := f.cookie(admin, "/api/admin/analytics/options?user="+lastID.String()); rec.Code != http.StatusOK {
		t.Fatalf("truncated-off user on options = %d, want 200", rec.Code)
	}
}

func TestAnalyticsEmptyPopulationNullReasonsLiveDB(t *testing.T) {
	f := newAnFixture(t, 2035)
	owner := f.user(false)

	dto := anDecodeSpend(t, f.cookie(owner, "/api/analytics/spend"))
	reasonIs := func(label string, v *float64, r *string, want string) {
		t.Helper()
		if v != nil || r == nil || *r != want {
			t.Fatalf("%s = value %v reason %v, want null with %q", label, v, r, want)
		}
	}
	reasonIs("cost_per_completed_run", dto.CostPerCompletedRun.Current.Value, dto.CostPerCompletedRun.Current.Reason, "no_completed_delivery_runs")
	reasonIs("cost_per_merged_pr", dto.CostPerMergedPR.Current.Value, dto.CostPerMergedPR.Current.Reason, "no_merged_prs")
	reasonIs("cache_read", dto.CacheRead.Value, dto.CacheRead.Reason, "no_input_tokens")
	reasonIs("coverage", dto.Totals.Current.Coverage.Value, dto.Totals.Current.Coverage.Reason, "no_applicable_runs")
	if dto.Totals.Previous == nil || dto.PreviousFrom == nil {
		t.Fatal("a bounded window has a previous period")
	}
	if len(dto.Buckets) != 30 || len(dto.Rolling) != 30 || len(dto.Series) != 30 {
		t.Fatalf("empty 30d scope: %d buckets, %d rolling, %d series, want 30 each", len(dto.Buckets), len(dto.Rolling), len(dto.Series))
	}
	if dto.HistoryStart != nil {
		t.Fatalf("history_start = %v for an empty scope, want null", dto.HistoryStart)
	}

	// window=all over an empty scope has no buckets, which travel as [] and not null, and no previous window.
	rec := f.cookie(owner, "/api/analytics/spend?window=all")
	all := anDecodeSpend(t, rec)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"buckets", "rolling", "series", "top_runs"} {
		if string(raw[k]) != "[]" {
			t.Fatalf("%s = %s, want []", k, raw[k])
		}
	}
	if _, ok := raw["previous_from"]; ok || all.Totals.Previous != nil {
		t.Fatalf("window=all must carry no previous period")
	}
	if string(raw["history_start"]) != "null" {
		t.Fatalf("history_start = %s, want null", raw["history_start"])
	}
	if all.BucketWidthDays != 7 {
		t.Fatalf("bucket_width_days = %d for an empty all-time scope, want 7", all.BucketWidthDays)
	}
}

func TestAnalyticsWireReconciliationLiveDB(t *testing.T) {
	f := newAnFixture(t, 2036)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "recon")
	costs := []string{"0.123456", "1.100000", "2.200000", "0.000001", "3.333333", "7.777777"}
	var want float64
	for i, c := range costs {
		f.run(owner, repo, f.now.Add(-time.Duration(i*20+2)*time.Hour), c)
		var v float64
		if _, err := fmt.Sscanf(c, "%f", &v); err != nil {
			t.Fatal(err)
		}
		want += v
	}

	dto := anDecodeSpend(t, f.cookie(owner, "/api/analytics/spend?split=harness&breakdown=kind"))
	total := dto.Totals.Current.CostUSD
	anNear(t, "total", total, want)
	var bucketSum, groupSum, rowSum float64
	for _, c := range dto.Series {
		bucketSum += c.CostUSD
	}
	for _, g := range dto.Split.Groups {
		groupSum += g.Totals.CostUSD
	}
	for _, r := range dto.Breakdown.Rows {
		rowSum += r.CostUSD
	}
	anNear(t, "sum of buckets", bucketSum, total)
	anNear(t, "sum of split groups", groupSum, total)
	anNear(t, "sum of breakdown rows", rowSum, total)
	if dto.Totals.Current.Runs != int64(len(costs)) {
		t.Fatalf("runs = %d, want %d", dto.Totals.Current.Runs, len(costs))
	}
	if len(dto.Series) != len(dto.Buckets) || len(dto.Rolling) != len(dto.Buckets) {
		t.Fatalf("series %d, buckets %d, rolling %d must align", len(dto.Series), len(dto.Buckets), len(dto.Rolling))
	}
}

// A cancelled request releases its admission slot and its connection: an immediate second
// request from the same caller is admitted.
func TestAnalyticsAdmissionReleasedAfterCancelLiveDB(t *testing.T) {
	f := newAnFixture(t, 2037)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "cancel")
	f.run(owner, repo, f.now.Add(-time.Hour), "1.000000")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.h.analyticsDeps.afterFirstRead = func(context.Context) { cancel() }
	req := httptest.NewRequest(http.MethodGet, "/api/analytics/spend", nil).WithContext(mw.ContextWithUser(ctx, store.User{ID: owner}))
	first := httptest.NewRecorder()
	f.h.AnalyticsSpend(first, req)
	f.h.analyticsDeps.afterFirstRead = nil

	// The cancellation really happened mid-request, and nobody was answered.
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("request context err = %v, want context.Canceled (the hook did not cancel)", ctx.Err())
	}
	if first.Body.Len() != 0 {
		t.Fatalf("cancelled request wrote a body: %q", first.Body.String())
	}
	if n := f.pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still acquired after a cancelled request", n)
	}
	rec := f.cookie(owner, "/api/analytics/spend")
	if rec.Code != http.StatusOK {
		t.Fatalf("request right after a cancelled one = %d %s, want 200", rec.Code, rec.Body.String())
	}
	anNear(t, "total", anDecodeSpend(t, rec).Totals.Current.CostUSD, 1)
}

// The snapshot barrier: a run committed on another connection after the first read is not seen
// by the in-flight response, and is seen by the next request.
func TestAnalyticsSnapshotBarrierLiveDB(t *testing.T) {
	f := newAnFixture(t, 2038)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "barrier")
	f.run(owner, repo, f.now.Add(-time.Hour), "1.000000")

	var fired atomic.Int32
	f.h.analyticsDeps.afterFirstRead = func(context.Context) {
		if fired.Add(1) != 1 {
			return
		}
		// pool.Exec takes a second connection: the request's snapshot connection is busy.
		anSeedRun(t, f.pool, owner, repo, f.now.Add(-30*time.Minute), "3.210000")
	}
	first := anDecodeSpend(t, f.cookie(owner, "/api/analytics/spend"))
	if fired.Load() != 1 {
		t.Fatalf("the after-first-read hook fired %d times, want 1", fired.Load())
	}
	anNear(t, "in-flight total", first.Totals.Current.CostUSD, 1)
	if first.Totals.Current.Runs != 1 {
		t.Fatalf("in-flight response saw %d runs, want 1 (the competing commit must be invisible)", first.Totals.Current.Runs)
	}

	second := anDecodeSpend(t, f.cookie(owner, "/api/analytics/spend"))
	anNear(t, "fresh total", second.Totals.Current.CostUSD, 4.21)
	if second.Totals.Current.Runs != 2 {
		t.Fatalf("fresh response saw %d runs, want 2", second.Totals.Current.Runs)
	}
}
