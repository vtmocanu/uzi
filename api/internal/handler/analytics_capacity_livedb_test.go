package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Issue #4731: Capacity through the real router and a real database. The all-users statements
// (runs and both gauges) have no owner predicate, so a test that reads them runs in a database of
// its own (capPrivateDBEnv); owner-scope tests share the LiveDB database like the Spend ones and
// claim a calendar year.

// capPrivateDBEnv creates and migrates a database only the calling test uses and points
// UZI_TEST_DATABASE_URL at it for the rest of the test (cliLiveDB reads it).
func capPrivateDBEnv(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	name := "caph_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:20]
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		admin.Close()
		t.Fatalf("create private database: %v", err)
	}
	t.Cleanup(func() {
		defer admin.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("drop private database: %v", err)
		}
	})
	owned := dsn + " dbname=" + name
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.Path = "/" + name
		q := u.Query()
		q.Del("dbname")
		u.RawQuery = q.Encode()
		owned = u.String()
	}
	t.Setenv("UZI_TEST_DATABASE_URL", owned)
}

// newCapFixture is newAnFixture with the poll intervals the gauges need; private selects a
// database of the test's own.
func newCapFixture(t *testing.T, year int, private bool) *anFixture {
	t.Helper()
	if private {
		capPrivateDBEnv(t)
	}
	f := newAnFixture(t, year)
	f.h.cfg.UsagePollInterval = 5 * time.Minute
	f.h.cfg.CodexUsagePollInterval = 5 * time.Minute
	return f
}

func (f *anFixture) capDecode(rec *httptest.ResponseRecorder) apitypes.AnalyticsCapacityDTO {
	f.t.Helper()
	if rec.Code != http.StatusOK {
		f.t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "private, no-store" {
		f.t.Fatalf("Cache-Control = %q, want private, no-store", got)
	}
	var dto apitypes.AnalyticsCapacityDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		f.t.Fatalf("decode capacity: %v: %s", err, rec.Body.String())
	}
	return dto
}

func (f *anFixture) capOwner(user uuid.UUID, query string) apitypes.AnalyticsCapacityDTO {
	f.t.Helper()
	return f.capDecode(f.cookie(user, "/api/analytics/capacity"+query))
}

func (f *anFixture) capAdmin(admin uuid.UUID, query string) apitypes.AnalyticsCapacityDTO {
	f.t.Helper()
	return f.capDecode(f.cookie(admin, "/api/admin/analytics/capacity"+query))
}

// capRun seeds a completed run with a retained interval [started, finished).
func (f *anFixture) capRun(user, repo uuid.UUID, kind, harness string, started, finished time.Time, waits int) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	cliMustExec(f.t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at,
		                   status_since, finished_at, first_started_at, harness, limit_wait_count)
		 VALUES ($1, $2, $3, $4, 't', 'd', 'completed', $5, $6, $7, $7, $6, $8, $9)`,
		id, user, repo, 5_000_000+anSeq.Add(1), kind, started, finished, harness, waits)
	return id
}

// anthropicToken seeds an enabled Claude token with a reading synced at syncedAt.
func (f *anFixture) anthropicToken(user uuid.UUID, label string, isDefault bool, pct int16, syncedAt time.Time) uuid.UUID {
	f.t.Helper()
	row, err := store.New(f.pool).InsertUserSecret(context.Background(), store.InsertUserSecretParams{
		UserID: user, Kind: store.KindAnthropicToken, Label: label, WantDefault: isDefault,
		Ciphertext: []byte("ct-" + label), SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		f.t.Fatalf("insert token %q: %v", label, err)
	}
	cliMustExec(f.t, f.pool,
		`INSERT INTO anthropic_rate_limits (user_secret_id, user_id, five_hour_pct, seven_day_pct, source, synced_at)
		 VALUES ($1, $2, $3, $3, 'usage_endpoint', $4)`, row.ID, user, pct, syncedAt)
	return row.ID
}

// codexAccount seeds one canonical Codex account with a linked alias and a reading at at.
func (f *anFixture) codexAccount(user uuid.UUID, label string, isDefault bool, at time.Time) uuid.UUID {
	f.t.Helper()
	ctx := context.Background()
	q := store.New(f.pool)
	sealed, err := f.h.box.Seal([]byte(`{"access_token":"a","refresh_token":"r"}`))
	if err != nil {
		f.t.Fatal(err)
	}
	account, err := q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: user, ProviderUserID: "prov-" + uuid.NewString(), WorkspaceAccountID: "ws-" + uuid.NewString(),
		SealedLogin: sealed, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		f.t.Fatalf("insert provider account: %v", err)
	}
	secret, err := q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: user, Kind: store.KindCodexAuth, Label: label, WantDefault: isDefault, Ciphertext: sealed, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		f.t.Fatalf("insert codex secret: %v", err)
	}
	if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: secret.ID, UserID: user, Status: "staging"}); err != nil {
		f.t.Fatal(err)
	}
	if n, err := q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secret.ID, UserID: user, MaterialRevision: 0,
	}); err != nil || n != 1 {
		f.t.Fatalf("link = (%d, %v)", n, err)
	}
	cliMustExec(f.t, f.pool, `INSERT INTO codex_account_rate_limits
		(user_id, provider_account_id, buckets, observed_generation, observed_credential_revision, last_success_at, last_attempt_at, attempt_status)
		VALUES ($1, $2, '[{"id":"primary"}]'::jsonb, 0, 0, $3, $3, 'ok')`, user, account.ID, at)
	return account.ID
}

func capCodexByAccount(g apitypes.AnalyticsCapacityCodexGaugeDTO, id uuid.UUID) (apitypes.AnalyticsCapacityCodexIdentityDTO, bool) {
	for _, i := range g.Identities {
		if i.AccountID == id.String() {
			return i, true
		}
	}
	return apitypes.AnalyticsCapacityCodexIdentityDTO{}, false
}

func capAnthropicBySecret(g apitypes.AnalyticsCapacityAnthropicGaugeDTO, id uuid.UUID) (apitypes.AnalyticsCapacityAnthropicIdentityDTO, bool) {
	for _, i := range g.Identities {
		if i.SecretID == id.String() {
			return i, true
		}
	}
	return apitypes.AnalyticsCapacityAnthropicIdentityDTO{}, false
}

func TestAnalyticsCapacityCredentialsLiveDB(t *testing.T) {
	f := newCapFixture(t, 2041, false)
	admin, member := f.user(true), f.user(false)
	adminRO := cliMintToken(t, f.pool, admin, clitoken.ScopeAdminRO)
	adminDefault := cliMintToken(t, f.pool, admin, clitoken.ScopeUser)
	memberToken := cliMintToken(t, f.pool, member, clitoken.ScopeUser)
	adminJWT, memberJWT := cliMintJWT(t, f.pool, admin), cliMintJWT(t, f.pool, member)

	const ownerPath, adminPath = "/api/analytics/capacity", "/api/admin/analytics/capacity"
	for _, path := range []string{ownerPath, adminPath} {
		if rec := bearerReq(f.router, http.MethodGet, path, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s no credential = %d, want 401", path, rec.Code)
		}
	}
	ownScope := func(name string, rec *httptest.ResponseRecorder, user uuid.UUID) {
		t.Helper()
		dto := f.capDecode(rec)
		if dto.Scope.Route != "owner" || dto.Scope.User != user.String() || dto.SelectedUser != user.String() {
			t.Fatalf("%s owner route scope = %+v selected %q, want own scope %s", name, dto.Scope, dto.SelectedUser, user)
		}
	}
	allUsers := func(name string, rec *httptest.ResponseRecorder) {
		t.Helper()
		dto := f.capDecode(rec)
		if dto.Scope.Route != "admin" || dto.Scope.User != "all" || dto.SelectedUser != "all" {
			t.Fatalf("%s admin route scope = %+v, want all users", name, dto.Scope)
		}
	}
	ownScope("admin cookie", cookieReq(t, f.router, http.MethodGet, ownerPath, adminJWT, ""), admin)
	allUsers("admin cookie", cookieReq(t, f.router, http.MethodGet, adminPath, adminJWT, ""))
	ownScope("admin_ro bearer", bearerReq(f.router, http.MethodGet, ownerPath, adminRO), admin)
	allUsers("admin_ro bearer", bearerReq(f.router, http.MethodGet, adminPath, adminRO))
	ownScope("admin default-scope bearer", bearerReq(f.router, http.MethodGet, ownerPath, adminDefault), admin)
	ownScope("member cookie", cookieReq(t, f.router, http.MethodGet, ownerPath, memberJWT, ""), member)
	ownScope("member bearer", bearerReq(f.router, http.MethodGet, ownerPath, memberToken), member)
	if rec := bearerReq(f.router, http.MethodGet, adminPath, adminDefault); rec.Code != http.StatusForbidden {
		t.Fatalf("admin route, ordinary bearer owned by an admin = %d, want 403", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, adminPath, memberJWT, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("admin route, member cookie = %d, want 403", rec.Code)
	}
	if rec := bearerReq(f.router, http.MethodGet, adminPath, memberToken); rec.Code != http.StatusForbidden {
		t.Fatalf("admin route, member bearer = %d, want 403", rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, "/api/analytics/capacity/extra", adminJWT, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown capacity subpath = %d, want 404", rec.Code)
	}
}

// Owner isolation and admin user=<uuid>: the historical measures and the gauge identities both
// follow the scope; an admin all-users response carries every user's identities with user fields.
func TestAnalyticsCapacityScopesAndIdentitiesLiveDB(t *testing.T) {
	f := newCapFixture(t, 2042, true)
	admin, a, b := f.user(true), f.user(false), f.user(false)
	repoA, _ := f.repoOwnedBy(a, "a")
	repoB, _ := f.repoOwnedBy(b, "b")
	h := time.Hour
	f.capRun(a, repoA, "issue", "claude", f.now.Add(-5*h), f.now.Add(-3*h), 0)
	f.capRun(b, repoB, "issue", "claude", f.now.Add(-4*h), f.now.Add(-1*h), 1)
	f.capRun(b, repoB, "issue", "codex", f.now.Add(-2*h), f.now.Add(-1*h), 0)

	tokA := f.anthropicToken(a, "a-token", true, 11, f.now.Add(-time.Minute))
	tokB := f.anthropicToken(b, "b-token", true, 22, f.now.Add(-time.Minute))
	codexA := f.codexAccount(a, "a-sub", true, f.now.Add(-time.Minute))
	codexB := f.codexAccount(b, "b-sub", true, f.now.Add(-time.Minute))

	check := func(name string, dto apitypes.AnalyticsCapacityDTO, wantCandidates int64, anth, codex []uuid.UUID, notAnth, notCodex []uuid.UUID) {
		t.Helper()
		if dto.Occupancy.Population.Candidates != wantCandidates || dto.Occupancy.Population.WithInterval != wantCandidates {
			t.Fatalf("%s population = %+v, want %d candidates with interval", name, dto.Occupancy.Population, wantCandidates)
		}
		for _, id := range anth {
			if _, ok := capAnthropicBySecret(dto.Gauges.Anthropic, id); !ok {
				t.Fatalf("%s: anthropic identity %s missing", name, id)
			}
		}
		for _, id := range notAnth {
			if _, ok := capAnthropicBySecret(dto.Gauges.Anthropic, id); ok {
				t.Fatalf("%s: anthropic identity %s leaked into this scope", name, id)
			}
		}
		for _, id := range codex {
			if _, ok := capCodexByAccount(dto.Gauges.Codex, id); !ok {
				t.Fatalf("%s: codex identity %s missing", name, id)
			}
		}
		for _, id := range notCodex {
			if _, ok := capCodexByAccount(dto.Gauges.Codex, id); ok {
				t.Fatalf("%s: codex identity %s leaked into this scope", name, id)
			}
		}
	}
	asA := f.capOwner(a, "")
	check("owner a", asA, 1, []uuid.UUID{tokA}, []uuid.UUID{codexA}, []uuid.UUID{tokB}, []uuid.UUID{codexB})
	if asA.Gauges.Anthropic.Identities[0].UserID != nil || asA.Gauges.Codex.Identities[0].Email != nil {
		t.Fatal("owner-scope identities carry user fields")
	}
	// An admin on the owner route stays in their own scope.
	check("admin owner route", f.capOwner(admin, ""), 0, nil, nil, []uuid.UUID{tokA, tokB}, []uuid.UUID{codexA, codexB})
	check("admin user=b", f.capAdmin(admin, "?user="+b.String()), 2, []uuid.UUID{tokB}, []uuid.UUID{codexB}, []uuid.UUID{tokA}, []uuid.UUID{codexA})
	if sel := f.capAdmin(admin, "?user="+b.String()); sel.Scope.User != b.String() || sel.Gauges.Anthropic.EffectiveScope.User != b.String() {
		t.Fatalf("admin user=b scope = %+v / %+v", sel.Scope, sel.Gauges.Anthropic.EffectiveScope)
	}
	all := f.capAdmin(admin, "")
	check("admin all users", all, 3, []uuid.UUID{tokA, tokB}, []uuid.UUID{codexA, codexB}, nil, nil)
	if id, _ := capAnthropicBySecret(all.Gauges.Anthropic, tokB); id.UserID == nil || *id.UserID != b.String() || id.Email == nil || !strings.HasPrefix(*id.Email, "cli-") {
		t.Fatalf("all-users anthropic identity lacks user fields: %+v", id)
	}
	if id, _ := capCodexByAccount(all.Gauges.Codex, codexA); id.UserID == nil || *id.UserID != a.String() || id.Email == nil {
		t.Fatalf("all-users codex identity lacks user fields: %+v", id)
	}
	if all.Gauges.Anthropic.EffectiveScope.User != "all" || all.Gauges.Anthropic.SetState == nil || *all.Gauges.Anthropic.SetState != "complete" {
		t.Fatalf("all-users anthropic gauge = %+v", all.Gauges.Anthropic.AnalyticsCapacityGaugeBaseDTO)
	}
	// Fresh readings of an unprotected owner.
	if id, _ := capCodexByAccount(asA.Gauges.Codex, codexA); id.Status != "fresh" || len(id.Aliases) != 1 || id.Aliases[0] != "a-sub" {
		t.Fatalf("codex identity = %+v", id)
	}
	if id, _ := capAnthropicBySecret(asA.Gauges.Anthropic, tokA); id.Limits.Status != "ok" || id.Limits.Stale == nil || *id.Limits.Stale || id.Label != "a-token" || !id.IsDefault {
		t.Fatalf("anthropic identity = %+v", id)
	}
	// Occupancy of user a: one 2h run.
	if asA.Occupancy.Window.RunSeconds != 7200 {
		t.Fatalf("a's window run_seconds = %v, want 7200", asA.Occupancy.Window.RunSeconds)
	}
}

func TestAnalyticsCapacityWindowAllLiveDB(t *testing.T) {
	f := newCapFixture(t, 2043, false)
	admin, owner := f.user(true), f.user(false)
	repo, _ := f.repoOwnedBy(owner, "alltime")
	f.capRun(owner, repo, "issue", "claude", time.Date(2043, 4, 1, 9, 0, 0, 0, time.UTC), time.Date(2043, 4, 1, 11, 0, 0, 0, time.UTC), 0)

	omitted := f.cookie(admin, "/api/admin/analytics/capacity?window=all")
	explicit := f.cookie(admin, "/api/admin/analytics/capacity?window=all&user=all")
	if omitted.Code != http.StatusBadRequest || explicit.Code != http.StatusBadRequest || omitted.Body.String() != explicit.Body.String() {
		t.Fatalf("admin all-users window=all = %d %q and %d %q, want identical 400s", omitted.Code, omitted.Body.String(), explicit.Code, explicit.Body.String())
	}
	if !strings.Contains(omitted.Body.String(), "window all not available for all users") {
		t.Fatalf("refusal text = %s", omitted.Body.String())
	}
	for name, dto := range map[string]apitypes.AnalyticsCapacityDTO{
		"owner":           f.capOwner(owner, "?window=all"),
		"admin user=uuid": f.capAdmin(admin, "?window=all&user="+owner.String()),
	} {
		if dto.Window != "all" || dto.Occupancy.Window.RunSeconds != 7200 || dto.Occupancy.Population.WithInterval != 1 {
			t.Fatalf("%s window=all = window %q run_seconds %v population %+v", name, dto.Window, dto.Occupancy.Window.RunSeconds, dto.Occupancy.Population)
		}
		if len(dto.Buckets) == 0 || len(dto.Occupancy.Buckets) != len(dto.Buckets) || len(dto.UsageWait.Buckets) != len(dto.Buckets) {
			t.Fatalf("%s buckets: envelope %d occupancy %d usage_wait %d", name, len(dto.Buckets), len(dto.Occupancy.Buckets), len(dto.UsageWait.Buckets))
		}
		if dto.BucketWidthDays != 7 {
			t.Fatalf("%s bucket_width_days = %d, want 7", name, dto.BucketWidthDays)
		}
	}
	if got := anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options")).Windows; slices.Contains(got, "all") {
		t.Fatalf("options windows for admin all-users = %v, must omit all", got)
	}
}

func TestAnalyticsCapacityRepoAuthorityLiveDB(t *testing.T) {
	f := newCapFixture(t, 2044, false)
	u, other := f.user(false), f.user(false)
	repoU, _ := f.repoOwnedBy(u, "u")
	repoOther, _ := f.repoOwnedBy(other, "other")

	foreign := f.cookie(u, "/api/analytics/capacity?repo="+repoOther.String())
	unknown := f.cookie(u, "/api/analytics/capacity?repo="+uuid.NewString())
	if foreign.Code != http.StatusBadRequest || unknown.Code != http.StatusBadRequest || foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("foreign repo = %d %s, unknown repo = %d %s, want identical 400s", foreign.Code, foreign.Body.String(), unknown.Code, unknown.Body.String())
	}
	if !strings.Contains(foreign.Body.String(), "invalid repo") {
		t.Fatalf("repo 400 = %s", foreign.Body.String())
	}
	dto := f.capOwner(u, "?repo="+repoU.String())
	if dto.Filters.Repo == nil || *dto.Filters.Repo != repoU.String() {
		t.Fatalf("filters.repo = %v, want the accessible repo", dto.Filters.Repo)
	}
	if strings.Contains(f.cookie(u, "/api/analytics/capacity").Body.String(), repoU.String()) {
		t.Fatal("an unfiltered response names a repo it was not asked about")
	}
	if none := f.capOwner(u, ""); none.Filters.Repo != nil {
		t.Fatalf("unfiltered filters.repo = %v, want null", none.Filters.Repo)
	}
}

// repo and kind narrow the historical counts but cannot partition a shared quota: the gauges are
// identical and name the selectors they could not honor.
func TestAnalyticsCapacitySelectorAttributionLiveDB(t *testing.T) {
	f := newCapFixture(t, 2045, false)
	u := f.user(false)
	repoA, _ := f.repoOwnedBy(u, "a")
	var conn uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT connection_id FROM repos WHERE id = $1`, repoA).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	repoB, _ := f.repoOn(conn, "b")
	h := time.Hour
	f.capRun(u, repoA, "issue", "claude", f.now.Add(-6*h), f.now.Add(-5*h), 0)
	f.capRun(u, repoB, "issue", "claude", f.now.Add(-4*h), f.now.Add(-3*h), 0)
	f.capRun(u, repoB, "issue", "claude", f.now.Add(-2*h), f.now.Add(-1*h), 0)
	f.anthropicToken(u, "tok", true, 30, f.now.Add(-time.Minute))
	f.codexAccount(u, "sub", true, f.now.Add(-time.Minute))

	base := f.capOwner(u, "")
	byRepo := f.capOwner(u, "?repo="+repoA.String())
	both := f.capOwner(u, "?repo="+repoA.String()+"&kind=supporting")
	if base.Occupancy.Population.Candidates != 3 || byRepo.Occupancy.Population.Candidates != 1 || both.Occupancy.Population.Candidates != 0 {
		t.Fatalf("candidates base/repo/repo+kind = %d/%d/%d, want 3/1/0",
			base.Occupancy.Population.Candidates, byRepo.Occupancy.Population.Candidates, both.Occupancy.Population.Candidates)
	}
	if base.Occupancy.Window.RunSeconds != 3*3600 || byRepo.Occupancy.Window.RunSeconds != 3600 {
		t.Fatalf("run_seconds base/repo = %v/%v, want 10800/3600", base.Occupancy.Window.RunSeconds, byRepo.Occupancy.Window.RunSeconds)
	}
	sameGauges := func(name string, got apitypes.AnalyticsCapacityDTO) {
		t.Helper()
		if !reflect.DeepEqual(got.Gauges.Anthropic.Identities, base.Gauges.Anthropic.Identities) || !reflect.DeepEqual(got.Gauges.Codex.Identities, base.Gauges.Codex.Identities) ||
			got.Gauges.Anthropic.Status != "available" || got.Gauges.Codex.Status != "available" {
			t.Fatalf("%s gauges differ from the unfiltered ones: %+v", name, got.Gauges)
		}
	}
	sameGauges("repo", byRepo)
	sameGauges("repo+kind", both)
	for name, want := range map[string][]string{"": {}, "repo": {"repo"}, "both": {"repo", "kind"}} {
		dto := map[string]apitypes.AnalyticsCapacityDTO{"": base, "repo": byRepo, "both": both}[name]
		for _, es := range []apitypes.AnalyticsCapacityEffectiveScopeDTO{dto.Gauges.Anthropic.EffectiveScope, dto.Gauges.Codex.EffectiveScope} {
			if !slices.Equal(es.UnattributedSelectors, want) || (len(want) > 0) != (es.Reason != nil) {
				t.Fatalf("%q effective scope = %+v, want selectors %v", name, es, want)
			}
		}
	}
}

// Staleness is judged at the request's captured clock (h.now), not the wall clock. The fixture year
// is in the past, so a reading synced a minute before the pinned now would read stale (and a Codex
// account stale) if any gauge call site (owner and admin all-users Anthropic, Codex) used
// time.Now instead.
func TestAnalyticsCapacityGaugeStalenessUsesRequestClockLiveDB(t *testing.T) {
	f := newCapFixture(t, 2020, true)
	u := f.user(false)
	tok := f.anthropicToken(u, "tok", true, 10, f.now.Add(-time.Minute))
	acct := f.codexAccount(u, "sub", true, f.now.Add(-time.Minute))

	dto := f.capOwner(u, "")
	a, ok := capAnthropicBySecret(dto.Gauges.Anthropic, tok)
	if !ok || a.Limits.Status != "ok" || a.Limits.Stale == nil || *a.Limits.Stale {
		t.Fatalf("anthropic identity = %+v (found %v), want a fresh reading at the request clock", a, ok)
	}
	c, ok := capCodexByAccount(dto.Gauges.Codex, acct)
	if !ok || c.Status != "fresh" {
		t.Fatalf("codex identity = %+v (found %v), want status fresh at the request clock", c, ok)
	}

	// The admin all-users route maps its own store rows through a separate call site.
	all := f.capAdmin(f.user(true), "")
	a, ok = capAnthropicBySecret(all.Gauges.Anthropic, tok)
	if !ok || a.Limits.Status != "ok" || a.Limits.Stale == nil || *a.Limits.Stale {
		t.Fatalf("admin all-users anthropic identity = %+v (found %v), want a fresh reading at the request clock", a, ok)
	}
	c, ok = capCodexByAccount(all.Gauges.Codex, acct)
	if !ok || c.Status != "fresh" {
		t.Fatalf("admin all-users codex identity = %+v (found %v), want status fresh at the request clock", c, ok)
	}
}

// The snapshot is fixed by the first read: a run and a fresh reading committed on another
// connection afterwards are invisible in flight and visible to the next request. A second request
// by the same caller during the hold is refused as busy.
func TestAnalyticsCapacitySnapshotBarrierLiveDB(t *testing.T) {
	f := newCapFixture(t, 2046, false)
	u := f.user(false)
	repo, _ := f.repoOwnedBy(u, "barrier")
	f.capRun(u, repo, "issue", "claude", f.now.Add(-3*time.Hour), f.now.Add(-2*time.Hour), 0)
	tok := f.anthropicToken(u, "tok", true, 10, f.now.Add(-time.Hour))
	oldCodex, newCodex := f.now.Add(-time.Hour), f.now.Add(-time.Minute)
	acct := f.codexAccount(u, "sub", true, oldCodex)

	var fired atomic.Int32
	var busy *httptest.ResponseRecorder
	f.h.analyticsDeps.afterFirstRead = func(context.Context) {
		if fired.Add(1) != 1 {
			return
		}
		f.capRun(u, repo, "issue", "claude", f.now.Add(-90*time.Minute), f.now.Add(-30*time.Minute), 0)
		cliMustExec(t, f.pool, `UPDATE anthropic_rate_limits SET five_hour_pct = 99, seven_day_pct = 99, synced_at = $2 WHERE user_secret_id = $1`, tok, f.now.Add(-time.Minute))
		cliMustExec(t, f.pool, `UPDATE codex_account_rate_limits SET last_success_at = $2, last_attempt_at = $2 WHERE provider_account_id = $1`, acct, newCodex)
		busy = f.cookie(u, "/api/analytics/capacity")
	}
	first := f.capOwner(u, "")
	f.h.analyticsDeps.afterFirstRead = nil
	if fired.Load() != 1 {
		t.Fatalf("hook fired %d times, want 1", fired.Load())
	}
	if busy == nil || busy.Code != http.StatusServiceUnavailable || busy.Header().Get("Retry-After") != "1" {
		t.Fatalf("same-caller request during the hold = %v, want 503 with Retry-After", busy)
	}
	if first.Occupancy.Population.Candidates != 1 {
		t.Fatalf("in-flight candidates = %d, want 1 (the competing commit must be invisible)", first.Occupancy.Population.Candidates)
	}
	id, _ := capAnthropicBySecret(first.Gauges.Anthropic, tok)
	if id.Limits.FiveHour == nil || id.Limits.FiveHour.Pct != 10 {
		t.Fatalf("in-flight reading = %+v, want the pre-commit 10%%", id.Limits)
	}
	cx, ok := capCodexByAccount(first.Gauges.Codex, acct)
	if !ok || cx.LastSuccessAt != oldCodex.UTC().Format(time.RFC3339) || cx.Status != "stale" {
		t.Fatalf("in-flight codex reading = %+v (found %v), want last_success_at %s and stale (the pre-commit reading)", cx, ok, oldCodex.UTC().Format(time.RFC3339))
	}
	second := f.capOwner(u, "")
	if second.Occupancy.Population.Candidates != 2 {
		t.Fatalf("fresh candidates = %d, want 2", second.Occupancy.Population.Candidates)
	}
	id, _ = capAnthropicBySecret(second.Gauges.Anthropic, tok)
	if id.Limits.FiveHour == nil || id.Limits.FiveHour.Pct != 99 {
		t.Fatalf("fresh reading = %+v, want 99%%", id.Limits)
	}
	cx, ok = capCodexByAccount(second.Gauges.Codex, acct)
	if !ok || cx.LastSuccessAt != newCodex.UTC().Format(time.RFC3339) || cx.Status != "fresh" {
		t.Fatalf("fresh codex reading = %+v (found %v), want last_success_at %s and fresh", cx, ok, newCodex.UTC().Format(time.RFC3339))
	}
}

func TestAnalyticsCapacityCancellationReleasesAdmissionLiveDB(t *testing.T) {
	f := newCapFixture(t, 2047, false)
	owner := f.user(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.h.analyticsDeps.afterFirstRead = func(context.Context) { cancel() }
	req := httptest.NewRequest(http.MethodGet, "/api/analytics/capacity", nil).WithContext(mw.ContextWithUser(ctx, store.User{ID: owner}))
	first := httptest.NewRecorder()
	f.h.AnalyticsCapacity(first, req)
	f.h.analyticsDeps.afterFirstRead = nil
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("request context err = %v, want Canceled", ctx.Err())
	}
	if first.Body.Len() != 0 || first.Code != http.StatusOK {
		t.Fatalf("cancelled request wrote %d %q, want nothing", first.Code, first.Body.String())
	}
	if n := f.pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still acquired", n)
	}
	f.capOwner(owner, "")
}

type countingTransport struct{ n atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, errors.New("outbound HTTP is forbidden in this test: " + r.URL.String())
}

func TestAnalyticsCapacityMakesNoOutboundHTTPLiveDB(t *testing.T) {
	f := newCapFixture(t, 2048, false)
	admin, u := f.user(true), f.user(false)
	f.anthropicToken(u, "tok", true, 5, f.now.Add(-time.Minute))
	f.codexAccount(u, "sub", true, f.now.Add(-time.Minute))
	ct := &countingTransport{}
	prev := http.DefaultTransport
	http.DefaultTransport = ct
	t.Cleanup(func() { http.DefaultTransport = prev })
	f.capOwner(u, "")
	f.capAdmin(admin, "?user="+u.String())
	f.capAdmin(admin, "")
	if n := ct.n.Load(); n != 0 {
		t.Fatalf("capacity made %d outbound HTTP requests, want 0", n)
	}
}

// An ordinary gauge error leaves occupancy, usage-wait and the other gauge intact.
func TestAnalyticsCapacityGaugeErrorKeepsOccupancyLiveDB(t *testing.T) {
	f := newCapFixture(t, 2049, false)
	u := f.user(false)
	repo, _ := f.repoOwnedBy(u, "gauge-error")
	f.capRun(u, repo, "issue", "claude", f.now.Add(-5*time.Hour), f.now.Add(-3*time.Hour), 2)
	f.anthropicToken(u, "tok", true, 5, f.now.Add(-time.Minute))
	codex := f.codexAccount(u, "sub", true, f.now.Add(-time.Minute))

	control := f.capOwner(u, "")
	if control.Gauges.Anthropic.Status != "available" || control.Occupancy.Window.RunSeconds != 7200 || control.UsageWait.Current.Count != 1 {
		t.Fatalf("control = %+v", control)
	}
	var calls []string
	f.h.analyticsDeps.beforeGauge = func(ctx context.Context, db store.DBTX, provider string) error {
		calls = append(calls, provider)
		if provider == "anthropic" {
			_, err := db.Exec(ctx, "SELECT 1/0")
			return err
		}
		return nil
	}
	// Keep the recorder of THIS request (the one that hits SELECT 1/0) for the sanitization check.
	failing := f.cookie(u, "/api/analytics/capacity")
	f.h.analyticsDeps.beforeGauge = nil
	got := f.capDecode(failing)
	if !slices.Equal(calls, []string{"anthropic", "codex"}) {
		t.Fatalf("gauge order = %v, want anthropic then codex", calls)
	}
	an := got.Gauges.Anthropic
	if an.Status != "unavailable" || an.Reason == nil || *an.Reason != "gauge_unavailable" || an.SetState != nil || len(an.Identities) != 0 {
		t.Fatalf("anthropic gauge = %+v, want unavailable/gauge_unavailable", an)
	}
	if got.Gauges.Codex.Status != "available" {
		t.Fatalf("codex gauge = %+v, want available", got.Gauges.Codex.AnalyticsCapacityGaugeBaseDTO)
	}
	if id, ok := capCodexByAccount(got.Gauges.Codex, codex); !ok || id.Status != "fresh" {
		t.Fatalf("codex rows missing after the anthropic failure: %+v", got.Gauges.Codex.Identities)
	}
	if !reflect.DeepEqual(got.Occupancy, control.Occupancy) || !reflect.DeepEqual(got.UsageWait, control.UsageWait) {
		t.Fatal("occupancy or usage_wait changed because a gauge failed")
	}
	if body := failing.Body.String(); strings.Contains(body, "division") || strings.Contains(body, "22012") || !strings.Contains(body, "gauge_unavailable") {
		t.Fatalf("failing request's response leaks a database error or lacks the sanitized reason: %s", body)
	}
}

func TestAnalyticsCapacityGaugeTimeoutIs503LiveDB(t *testing.T) {
	f := newCapFixture(t, 2050, false)
	u := f.user(false)
	f.anthropicToken(u, "tok", true, 5, f.now.Add(-time.Minute))
	f.h.analyticsDeps.beforeGauge = func(ctx context.Context, db store.DBTX, provider string) error {
		if provider != "anthropic" {
			return nil
		}
		if _, err := db.Exec(ctx, "SET LOCAL statement_timeout = '50ms'"); err != nil {
			return err
		}
		_, err := db.Exec(ctx, "SELECT pg_sleep(1)")
		return err
	}
	rec := f.cookie(u, "/api/analytics/capacity")
	f.h.analyticsDeps.beforeGauge = nil
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("gauge statement timeout = %d Retry-After %q %s, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gauge_unavailable") || strings.Contains(rec.Body.String(), "57014") {
		t.Fatalf("timeout body = %s", rec.Body.String())
	}
	f.capOwner(u, "") // admission was released and the snapshot connection was returned
}

// A Codex account whose only alias was disabled and re-enabled hides its pre-disable reading: the
// response status is exactly "pending" (polling on, vault unlocked, no reauth) in both shapes.
func TestAnalyticsCapacityCodexReenableIsPendingLiveDB(t *testing.T) {
	f := newCapFixture(t, 2054, true)
	admin, u := f.user(true), f.user(false)
	acct := f.codexAccount(u, "sub", true, f.now.Add(-time.Minute))
	owner := f.capOwner(u, "")
	if id, ok := capCodexByAccount(owner.Gauges.Codex, acct); !ok || id.Status != "fresh" {
		t.Fatalf("control: owner codex identity = %+v (found %v), want fresh", id, ok)
	}
	var secret uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT id FROM user_secrets WHERE user_id = $1 AND kind = $2 AND label = 'sub'`, u, store.KindCodexAuth).Scan(&secret); err != nil {
		t.Fatalf("find the alias secret: %v", err)
	}
	q := store.New(f.pool)
	for _, enabled := range []bool{false, true} {
		if _, err := q.SetSecretEnablement(context.Background(), store.SetSecretEnablementParams{ID: secret, UserID: u, Enabled: enabled}); err != nil {
			t.Fatalf("set enabled=%v: %v", enabled, err)
		}
	}
	for _, c := range []struct {
		name string
		dto  apitypes.AnalyticsCapacityDTO
	}{
		{"owner", f.capOwner(u, "")},
		{"admin all-users", f.capAdmin(admin, "")},
	} {
		id, ok := capCodexByAccount(c.dto.Gauges.Codex, acct)
		if c.dto.Gauges.Codex.Status != "available" || !ok || id.Status != "pending" || id.LastSuccessAt != "" || len(id.Buckets) != 0 {
			t.Fatalf("%s: re-enabled codex identity = %+v (found %v, gauge %q), want exactly pending with no reading", c.name, id, ok, c.dto.Gauges.Codex.Status)
		}
	}
}

// More than 200 identities per provider on the admin all-users route: exactly the first 200 in the
// documented order (owner email, owner id, then default-first label for Claude tokens or account
// id for Codex) come back, flagged truncated with set_state "truncated" for both providers.
func TestAnalyticsCapacityGaugeTruncationLiveDB(t *testing.T) {
	f := newCapFixture(t, 2055, true)
	admin := f.user(true)
	type owner struct {
		id    uuid.UUID
		email string
	}
	owners := make([]owner, 3)
	for i := range owners {
		owners[i].id = f.user(false)
		if err := f.pool.QueryRow(context.Background(), `SELECT email FROM users WHERE id = $1`, owners[i].id).Scan(&owners[i].email); err != nil {
			t.Fatal(err)
		}
	}
	slices.SortFunc(owners, func(a, b owner) int {
		if c := strings.Compare(a.email, b.email); c != 0 {
			return c
		}
		return strings.Compare(a.id.String(), b.id.String())
	})
	// Per owner: 80 Claude tokens (the first is the default, labels ascend) and 80 Codex accounts.
	var wantTokens, wantAccounts []string
	for oi, o := range owners {
		for i := 0; i < 80; i++ {
			wantTokens = append(wantTokens, f.anthropicToken(o.id, "t"+strconv.Itoa(1000+i), i == 0, 10, f.now.Add(-time.Minute)).String())
		}
		var accts []string
		for i := 0; i < 80; i++ {
			accts = append(accts, f.codexAccount(o.id, "c"+strconv.Itoa(oi*1000+i), i == 0, f.now.Add(-time.Minute)).String())
		}
		slices.Sort(accts)
		wantAccounts = append(wantAccounts, accts...)
	}
	got := f.capAdmin(admin, "")
	an, cx := got.Gauges.Anthropic, got.Gauges.Codex
	if an.Status != "available" || cx.Status != "available" {
		t.Fatalf("gauges = %q / %q, want both available", an.Status, cx.Status)
	}
	if !an.Truncated || an.SetState == nil || *an.SetState != "truncated" || len(an.Identities) != 200 {
		t.Fatalf("anthropic: truncated=%v set_state=%v n=%d, want true / truncated / 200", an.Truncated, an.SetState, len(an.Identities))
	}
	if !cx.Truncated || cx.SetState == nil || *cx.SetState != "truncated" || len(cx.Identities) != 200 {
		t.Fatalf("codex: truncated=%v set_state=%v n=%d, want true / truncated / 200", cx.Truncated, cx.SetState, len(cx.Identities))
	}
	for i, id := range an.Identities {
		if id.SecretID != wantTokens[i] {
			t.Fatalf("anthropic identity %d = %s, want %s (the ordered 200-prefix)", i, id.SecretID, wantTokens[i])
		}
	}
	for i, id := range cx.Identities {
		if id.AccountID != wantAccounts[i] {
			t.Fatalf("codex identity %d = %s, want %s (the ordered 200-prefix)", i, id.AccountID, wantAccounts[i])
		}
	}
}

// timeoutCapacityTx runs everything on the real transaction except the Capacity historical
// statements, which fail the way a statement_timeout does (SQLSTATE 57014).
type timeoutCapacityTx struct {
	pgx.Tx
	hits *atomic.Int32
}

type erroringRow struct{ err error }

func (r erroringRow) Scan(...any) error { return r.err }

func (t *timeoutCapacityTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	// Only the two historical statements build the occupancy document; the gauges, the options
	// and the timezone reads never mention it.
	if strings.Contains(sql, "'average_concurrent_runs'") {
		t.hits.Add(1)
		return erroringRow{&pgconn.PgError{Code: "57014", Message: "canceling statement due to statement timeout"}}
	}
	return t.Tx.QueryRow(ctx, sql, args...)
}

type timeoutCapacityBeginner struct {
	inner store.AnalyticsBeginner
	hits  *atomic.Int32
}

func (b *timeoutCapacityBeginner) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.inner.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &timeoutCapacityTx{Tx: tx, hits: b.hits}, nil
}

// The historical statement itself timing out (not the gauges, not BeginTx) is a 503 with
// Retry-After, leaks nothing, and releases admission and the snapshot connection.
func TestAnalyticsCapacityHistoricalStatementTimeoutIs503LiveDB(t *testing.T) {
	f := newCapFixture(t, 2053, false)
	admin, u := f.user(true), f.user(false)
	real := f.h.analyticsDeps.begin
	for _, c := range []struct {
		name, path string
		viewer     uuid.UUID
	}{
		{"owner", "/api/analytics/capacity", u},
		{"admin all-users", "/api/admin/analytics/capacity", admin},
	} {
		t.Run(c.name, func(t *testing.T) {
			var hits atomic.Int32
			f.h.analyticsDeps.begin = &timeoutCapacityBeginner{inner: real, hits: &hits}
			rec := f.cookie(c.viewer, c.path)
			f.h.analyticsDeps.begin = real
			if hits.Load() != 1 {
				t.Fatalf("the historical statement ran %d times through the failing transaction, want 1", hits.Load())
			}
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" {
				t.Fatalf("historical statement timeout = %d Retry-After %q %s, want 503 and 1", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "57014") || strings.Contains(rec.Body.String(), "statement timeout") {
				t.Fatalf("timeout body leaks the database error: %s", rec.Body.String())
			}
			// Admission was released and the snapshot connection returned: the next request is admitted and served.
			if c.viewer == admin {
				f.capAdmin(admin, "")
			} else {
				f.capOwner(u, "")
			}
			if n := f.pool.Stat().AcquiredConns(); n != 0 {
				t.Fatalf("%d connections still acquired", n)
			}
		})
	}
}

// A deadline error that reaches the handler as the gauge's own error with a live snapshot context
// is a timeout (503), not an ordinary gauge failure.
func TestAnalyticsCapacityGaugeRacePassThroughLiveDB(t *testing.T) {
	f := newCapFixture(t, 2051, false)
	u := f.user(false)
	f.h.analyticsDeps.beforeGauge = func(_ context.Context, _ store.DBTX, provider string) error {
		if provider == "anthropic" {
			return context.DeadlineExceeded
		}
		return nil
	}
	rec := f.cookie(u, "/api/analytics/capacity")
	f.h.analyticsDeps.beforeGauge = nil
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") != "1" || strings.Contains(rec.Body.String(), "gauge_unavailable") {
		t.Fatalf("race pass-through = %d Retry-After %q %s, want 503", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	f.capOwner(u, "")
}

// vaultCountingStore counts the vault reads a request performs.
type vaultCountingStore struct {
	*store.Queries
	reads *atomic.Int32
}

func (s *vaultCountingStore) GetUserVaultState(ctx context.Context, id uuid.UUID) (store.GetUserVaultStateRow, error) {
	s.reads.Add(1)
	return s.Queries.GetUserVaultState(ctx, id)
}

func (s *vaultCountingStore) GetVaultPolicy(ctx context.Context) (string, error) {
	s.reads.Add(1)
	return s.Queries.GetVaultPolicy(ctx)
}

// A provider the harness selector excludes runs no gauge SQL and no vault lookup.
func TestAnalyticsCapacityUnselectedProviderRunsNothingLiveDB(t *testing.T) {
	f := newCapFixture(t, 2052, true)
	admin, u := f.user(true), f.user(false)
	f.anthropicToken(u, "tok", true, 5, f.now.Add(-time.Minute))
	codex := f.codexAccount(u, "sub", true, f.now.Add(-time.Minute))
	var reads atomic.Int32
	f.h.vault = newWrappedPostgresVault(f.h.box, f.pool, &vaultCountingStore{Queries: f.h.q, reads: &reads},
		func(q *store.Queries) vault.Store { return &vaultCountingStore{Queries: q, reads: &reads} })
	var providers []string
	f.h.analyticsDeps.beforeGauge = func(_ context.Context, _ store.DBTX, provider string) error {
		providers = append(providers, provider)
		return nil
	}

	for _, route := range []struct {
		name string
		get  func(string) apitypes.AnalyticsCapacityDTO
	}{
		{"owner", func(q string) apitypes.AnalyticsCapacityDTO { return f.capOwner(u, q) }},
		{"admin all users", func(q string) apitypes.AnalyticsCapacityDTO { return f.capAdmin(admin, q) }},
	} {
		providers = nil
		claude := route.get("?harness=claude")
		if !slices.Equal(providers, []string{"anthropic"}) || reads.Load() != 0 {
			t.Fatalf("%s harness=claude ran gauges %v with %d vault reads, want [anthropic] and 0", route.name, providers, reads.Load())
		}
		cg := claude.Gauges.Codex
		if cg.Status != "not_selected" || cg.Reason == nil || *cg.Reason != "not_selected" || cg.SetState != nil || len(cg.Identities) != 0 || claude.Gauges.Anthropic.Status != "available" {
			t.Fatalf("%s harness=claude gauges = %+v / %+v", route.name, cg.AnalyticsCapacityGaugeBaseDTO, claude.Gauges.Anthropic.AnalyticsCapacityGaugeBaseDTO)
		}
		if claude.Filters.Harness == nil || *claude.Filters.Harness != "claude" {
			t.Fatalf("filters.harness = %v", claude.Filters.Harness)
		}

		providers = nil
		cdx := route.get("?harness=codex")
		if !slices.Equal(providers, []string{"codex"}) {
			t.Fatalf("%s harness=codex ran gauges %v, want [codex]", route.name, providers)
		}
		ag := cdx.Gauges.Anthropic
		if ag.Status != "not_selected" || ag.Reason == nil || *ag.Reason != "not_selected" || ag.SetState != nil || len(ag.Identities) != 0 {
			t.Fatalf("%s harness=codex anthropic gauge = %+v", route.name, ag)
		}
		if _, ok := capCodexByAccount(cdx.Gauges.Codex, codex); !ok || reads.Load() == 0 {
			t.Fatalf("%s harness=codex: codex identity present=%v vault reads=%d", route.name, ok, reads.Load())
		}
		reads.Store(0)
	}
}

// vaultPolicyBarrier blocks the first GetVaultPolicy call once armed, which is the read
// vault.Status performs after it holds the user's lifecycle mutex. Single-shot: later calls
// pass straight through.
type vaultPolicyBarrier struct {
	*store.Queries
	armed            *atomic.Bool
	calls            *atomic.Int32
	entered, release chan struct{}
}

func (b *vaultPolicyBarrier) GetVaultPolicy(ctx context.Context) (string, error) {
	if b.armed.Load() && b.calls.Add(1) == 1 {
		close(b.entered)
		select {
		case <-b.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return b.Queries.GetVaultPolicy(ctx)
}

// While another lifecycle operation holds a Codex owner's vault boundary, an admin all-users
// request does not wait for it: the Codex gauge is unavailable, everything else is intact, and
// the lifecycle wait observer never fires for the request.
func TestAnalyticsCapacityAdminVaultContentionLiveDB(t *testing.T) {
	f := newCapFixture(t, 2053, true)
	admin, codexOwner := f.user(true), f.user(false)
	repo, _ := f.repoOwnedBy(codexOwner, "vault")
	f.capRun(codexOwner, repo, "issue", "claude", f.now.Add(-4*time.Hour), f.now.Add(-2*time.Hour), 0)
	f.anthropicToken(codexOwner, "tok", true, 5, f.now.Add(-time.Minute))
	codex := f.codexAccount(codexOwner, "sub", true, f.now.Add(-time.Minute))

	var waits atomic.Int32
	var armed atomic.Bool
	var calls atomic.Int32
	barrier := &vaultPolicyBarrier{Queries: f.h.q, armed: &armed, calls: &calls, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	t.Cleanup(release)
	f.h.vault = newWrappedPostgresVault(f.h.box, f.pool, barrier, func(q *store.Queries) vault.Store { return q },
		vault.WithLifecycleWaitObserverForTest(func(uuid.UUID) { waits.Add(1) }))

	// Fixture setup is done; arm the barrier and start the concurrent Status.
	armed.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	statusDone := make(chan error, 1)
	go func() {
		_, err := f.h.vault.Status(ctx, codexOwner)
		statusDone <- err
	}()
	select {
	case <-barrier.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Status never reached GetVaultPolicy")
	}

	adminJWT := cliMintJWT(t, f.pool, admin)
	serve := cookieReqPrepared(t, f.router, http.MethodGet, "/api/admin/analytics/capacity", adminJWT, "")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- serve() }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the admin all-users request did not return within 2s while the vault boundary was held")
	}
	dto := f.capDecode(rec)
	cg := dto.Gauges.Codex
	if cg.Status != "unavailable" || cg.Reason == nil || *cg.Reason != "vault_status_unavailable" || len(cg.Identities) != 0 || cg.SetState != nil {
		t.Fatalf("codex gauge = %+v, want unavailable/vault_status_unavailable", cg)
	}
	if dto.Gauges.Anthropic.Status != "available" || len(dto.Gauges.Anthropic.Identities) != 1 {
		t.Fatalf("anthropic gauge = %+v", dto.Gauges.Anthropic)
	}
	if dto.Occupancy.Window.RunSeconds != 7200 || dto.Occupancy.Population.WithInterval != 1 || dto.UsageWait.Current.N != 1 {
		t.Fatalf("occupancy/usage_wait not intact: %+v / %+v", dto.Occupancy.Window, dto.UsageWait.Current)
	}
	if n := waits.Load(); n != 0 {
		t.Fatalf("lifecycle wait observer fired %d times: the admin request waited on the held boundary", n)
	}

	// The same caller is admitted again (admission released), and the owner route, which never
	// takes the lifecycle mutex, answers promptly with the owner's own state.
	again := make(chan *httptest.ResponseRecorder, 1)
	go func() { again <- serve() }()
	select {
	case r := <-again:
		if r.Code != http.StatusOK {
			t.Fatalf("following same-caller request = %d %s", r.Code, r.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("following same-caller request did not return")
	}
	ownerServe := cookieReqPrepared(t, f.router, http.MethodGet, "/api/analytics/capacity", cliMintJWT(t, f.pool, codexOwner), "")
	ownerDone := make(chan *httptest.ResponseRecorder, 1)
	go func() { ownerDone <- ownerServe() }()
	select {
	case r := <-ownerDone:
		od := f.capDecode(r)
		if id, ok := capCodexByAccount(od.Gauges.Codex, codex); od.Gauges.Codex.Status != "available" || !ok || id.Status != "fresh" {
			t.Fatalf("owner route codex gauge = %+v", od.Gauges.Codex)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner route blocked behind the held vault boundary")
	}

	release()
	select {
	case err := <-statusDone:
		if err != nil {
			t.Fatalf("concurrent Status: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Status did not finish after release")
	}
	after := f.capAdmin(admin, "")
	if after.Gauges.Codex.Status != "available" {
		t.Fatalf("after release codex gauge = %+v", after.Gauges.Codex.AnalyticsCapacityGaugeBaseDTO)
	}
	if id, ok := capCodexByAccount(after.Gauges.Codex, codex); !ok || id.Status != "fresh" || id.UserID == nil || *id.UserID != codexOwner.String() {
		t.Fatalf("after release codex identity = %+v present=%v", id, ok)
	}
	if n := waits.Load(); n != 0 {
		t.Fatalf("lifecycle wait observer count = %d after release, want 0", n)
	}
}
