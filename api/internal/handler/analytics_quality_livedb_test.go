package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4730: the Quality routes through the real router and a real database. Fixtures and the
// per-test calendar-year isolation come from analytics_livedb_test.go (years 2040-2044 are
// claimed here).
//
// Query timeout: the unit test TestAnalyticsQualityStatementTimeoutIs503WithRetryAfterAndReleases
// pins the 503 mapping and the slot release through the handler, and
// TestAnalyticsQualityStatementTimeoutThroughRouterLiveDB below drives a real query timeout
// through the live router (about 8 s). Under its ACCESS EXCLUSIVE lock the snapshot's 8 s Go
// context deadline (started before BEGIN in RunAnalyticsSnapshot) normally fires before the
// server-side statement_timeout, so that test pins the timeout, 503 plus Retry-After and slot
// release path, not SQLSTATE 57014 specifically. Server-side 57014 classification is pinned at
// store level by TestAnalyticsSnapshotStatementTimeoutLiveDB (#4684).

func anDecodeQuality(t *testing.T, rec *httptest.ResponseRecorder) apitypes.AnalyticsQualityDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var dto apitypes.AnalyticsQualityDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		t.Fatalf("decode quality: %v: %s", err, rec.Body.String())
	}
	return dto
}

// anSeedPlanCheck inserts one decided plan cross-check attempt on lead, created ten minutes
// before it was decided. Round must be unique per lead.
func anSeedPlanCheck(t *testing.T, f *anFixture, lead uuid.UUID, round int, verdict string, decided time.Time) {
	t.Helper()
	cliMustExec(t, f.pool,
		`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class,
		     base_commit, candidate_digest, verdict, reason_class, decided_at, deadline_at, created_at, interrupted_at, checker_harness)
		 VALUES ($1, 'plan', $5, 0, 'p', '[]'::jsonb, 'S', 'base', '\x01'::bytea, $2, NULL, $3, $4::timestamptz + interval '1 hour', $4, NULL, 'codex')`,
		lead, verdict, decided, decided.Add(-10*time.Minute), round)
}

func anSeedReview(t *testing.T, f *anFixture, target, user uuid.UUID, verdict string, at time.Time) {
	t.Helper()
	cliMustExec(t, f.pool,
		`INSERT INTO run_reviews (target_run_id, user_id, verdict, status, created_at, updated_at) VALUES ($1, $2, $3, 'complete', $4, $4)`,
		target, user, verdict, at)
}

func TestAnalyticsQualityCredentialsLiveDB(t *testing.T) {
	f := newAnFixture(t, 2041)
	admin, member := f.user(true), f.user(false)
	adminRO := cliMintToken(t, f.pool, admin, clitoken.ScopeAdminRO)
	adminDefault := cliMintToken(t, f.pool, admin, clitoken.ScopeUser)
	memberToken := cliMintToken(t, f.pool, member, clitoken.ScopeUser)
	adminJWT, memberJWT := cliMintJWT(t, f.pool, admin), cliMintJWT(t, f.pool, member)

	path := "/api/admin/analytics/quality"
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

	path = "/api/analytics/quality"
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
		if got := anDecodeQuality(t, rec).Scope.Route; got != "owner" {
			t.Fatalf("%s %s scope route = %q, want owner", path, name, got)
		}
	}
}

// Owner isolation, the admin-selected scope and the all-users scope over the same three runs.
func TestAnalyticsQualityScopesLiveDB(t *testing.T) {
	f := newAnFixture(t, 2040)
	admin, a, b := f.user(true), f.user(false), f.user(false)
	repoA, _ := f.repoOwnedBy(a, "a")
	repoB, _ := f.repoOwnedBy(b, "b")
	at := f.now.Add(-6 * time.Hour)
	leadA := f.run(a, repoA, at, "1.000000")
	leadB1 := f.run(b, repoB, at, "1.000000")
	leadB2 := f.run(b, repoB, at, "1.000000")
	anSeedPlanCheck(t, f, leadA, 1, "approve", f.now.Add(-2*time.Hour))
	anSeedPlanCheck(t, f, leadB1, 1, "revise", f.now.Add(-2*time.Hour))
	anSeedPlanCheck(t, f, leadB2, 1, "revise", f.now.Add(-3*time.Hour))
	anSeedReview(t, f, leadA, a, "ideal", f.now.Add(-time.Hour))

	asA := anDecodeQuality(t, f.cookie(a, "/api/analytics/quality"))
	if asA.CrossCheck.Plan.DecidedInWindow != 1 || asA.CrossCheck.Plan.StoredResult.Approve != 1 || asA.CrossCheck.Plan.StoredResult.Revise != 0 {
		t.Fatalf("owner A plan = %+v, want exactly A's one approve (B's data must be absent)", asA.CrossCheck.Plan)
	}
	if asA.Judge.Eligible.Total != 1 || asA.Judge.Eligible.Complete.Ideal != 1 || asA.Judge.Coverage.Value == nil || *asA.Judge.Coverage.Value != 1 {
		t.Fatalf("owner A judge = %+v", asA.Judge)
	}
	if asA.SelectedUser != a.String() || asA.Scope.Route != "owner" {
		t.Fatalf("owner A scope = %+v selected %q", asA.Scope, asA.SelectedUser)
	}
	asB := anDecodeQuality(t, f.cookie(b, "/api/analytics/quality"))
	if asB.CrossCheck.Plan.DecidedInWindow != 2 || asB.CrossCheck.Plan.StoredResult.Revise != 2 || asB.Judge.Eligible.Unreviewed != 2 {
		t.Fatalf("owner B = plan %+v judge %+v, want two revises and two unreviewed targets", asB.CrossCheck.Plan, asB.Judge.Eligible)
	}

	// An admin on the owner route stays in their own (empty) scope.
	own := anDecodeQuality(t, f.cookie(admin, "/api/analytics/quality"))
	if own.CrossCheck.Plan.DecidedInWindow != 0 || own.Judge.Eligible.Total != 0 {
		t.Fatalf("admin owner-route scope leaked other users: %+v", own.CrossCheck.Plan)
	}

	selA := anDecodeQuality(t, f.cookie(admin, "/api/admin/analytics/quality?user="+a.String()))
	if selA.CrossCheck.Plan.DecidedInWindow != 1 || selA.SelectedUser != a.String() || selA.Scope.User != a.String() || selA.Scope.Route != "admin" {
		t.Fatalf("admin user=A = %+v selected %q scope %+v", selA.CrossCheck.Plan, selA.SelectedUser, selA.Scope)
	}
	selB := anDecodeQuality(t, f.cookie(admin, "/api/admin/analytics/quality?user="+b.String()))
	if selB.CrossCheck.Plan.DecidedInWindow != 2 {
		t.Fatalf("admin user=B plan = %+v, want 2", selB.CrossCheck.Plan)
	}
	omitted, explicit := f.cookie(admin, "/api/admin/analytics/quality"), f.cookie(admin, "/api/admin/analytics/quality?user=all")
	if omitted.Code != http.StatusOK || omitted.Body.String() != explicit.Body.String() {
		t.Fatalf("omitted user differs from user=all (%d):\n%s\n%s", omitted.Code, omitted.Body.String(), explicit.Body.String())
	}
	all := anDecodeQuality(t, omitted)
	if all.SelectedUser != "all" || all.Scope.User != "all" || all.CrossCheck.Plan.DecidedInWindow != 3 || all.Judge.Eligible.Total != 3 {
		t.Fatalf("admin all-users = plan %+v judge %+v selected %q, want 3 and 3", all.CrossCheck.Plan, all.Judge.Eligible, all.SelectedUser)
	}
	if all.PreviousFrom != nil || all.PreviousTo != nil || len(all.Rolling) != 0 || len(all.Buckets) != 30 {
		t.Fatalf("envelope: previous %v/%v rolling %d buckets %d, want none, 0 and 30", all.PreviousFrom, all.PreviousTo, len(all.Rolling), len(all.Buckets))
	}
	for _, rec := range []*httptest.ResponseRecorder{f.cookie(a, "/api/analytics/quality"), omitted} {
		for _, id := range []string{repoA.String(), repoB.String()} {
			if strings.Contains(rec.Body.String(), id) {
				t.Fatalf("a Quality body carries a repo id: %s", rec.Body.String())
			}
		}
	}
}

func TestAnalyticsQualityAuthorityErrorsLiveDB(t *testing.T) {
	f := newAnFixture(t, 2041)
	admin, u, other := f.user(true), f.user(false), f.user(false)
	repoU, _ := f.repoOwnedBy(u, "u")
	repoOther, _ := f.repoOwnedBy(other, "other")
	_ = repoU

	foreign := f.cookie(u, "/api/analytics/quality?repo="+repoOther.String())
	unknown := f.cookie(u, "/api/analytics/quality?repo="+uuid.NewString())
	if foreign.Code != http.StatusBadRequest || unknown.Code != http.StatusBadRequest || foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("foreign repo = %d %q, unknown repo = %d %q, want the identical 400", foreign.Code, foreign.Body.String(), unknown.Code, unknown.Body.String())
	}
	crossRepo := f.cookie(admin, "/api/admin/analytics/quality?user="+u.String()+"&repo="+repoOther.String())
	if crossRepo.Code != http.StatusBadRequest || crossRepo.Body.String() != unknown.Body.String() {
		t.Fatalf("admin user=U&repo=other's = %d %q, want the unknown-repo 400 %q", crossRepo.Code, crossRepo.Body.String(), unknown.Body.String())
	}
	if rec := f.cookie(admin, "/api/admin/analytics/quality?user="+uuid.NewString()); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid user") {
		t.Fatalf("unknown user = %d %s, want 400 invalid user", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/api/analytics/quality", "/api/admin/analytics/quality"} {
		if rec := f.cookie(admin, path+"?tz=Mars/Olympus"); rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid tz") {
			t.Fatalf("%s invalid tz = %d %s, want 400 invalid tz", path, rec.Code, rec.Body.String())
		}
	}
	if rec := f.cookie(u, "/api/analytics/quality?user="+u.String()); rec.Code != http.StatusBadRequest {
		t.Fatalf("owner route user param = %d, want 400", rec.Code)
	}
	for _, q := range []string{"split=repo", "breakdown=kind"} {
		for _, path := range []string{"/api/analytics/quality", "/api/admin/analytics/quality"} {
			if rec := f.cookie(admin, path+"?"+q); rec.Code != http.StatusBadRequest {
				t.Fatalf("%s?%s = %d, want 400", path, q, rec.Code)
			}
		}
	}
	// A repo filter the scope may see is accepted and echoed as an effective live filter.
	rec := f.cookie(u, "/api/analytics/quality?repo="+repoU.String()+"&harness=claude&kind=issue")
	dto := anDecodeQuality(t, rec)
	ef := dto.CrossCheck.Pending.EffectiveFilters
	if ef.Repo == nil || *ef.Repo != repoU.String() || ef.Harness == nil || *ef.Harness != "claude" || !slices.Equal(ef.Kinds, []string{"issue"}) {
		t.Fatalf("effective filters = %+v", ef)
	}
	if dto.CIFix.Applicable || dto.CIFix.Reason == nil || *dto.CIFix.Reason != "not_applicable" {
		t.Fatalf("kind=issue must make ci_fix not applicable: %+v", dto.CIFix.AnalyticsQualitySectionDTO)
	}
}

// A kind filter whose kinds cannot run a plan cross-check (task has no plan gate) reports the plan
// section as not applicable AND counts no plan attempts, while the code section still counts.
func TestAnalyticsQualityPlanApplicabilityMatchesPopulationLiveDB(t *testing.T) {
	f := newAnFixture(t, 2044)
	u := f.user(false)
	repo, _ := f.repoOwnedBy(u, "task")
	lead := f.run(u, repo, f.now.Add(-6*time.Hour), "1.000000")
	cliMustExec(t, f.pool, `UPDATE runs SET kind = 'task', issue_iid = NULL, branch = 'uzi/task/' || id::text WHERE id = $1`, lead)
	anSeedPlanCheck(t, f, lead, 1, "approve", f.now.Add(-2*time.Hour))
	cliMustExec(t, f.pool,
		`INSERT INTO cross_checks (lead_run_id, stage, round, lead_claim_generation, base_commit, head_commit, candidate_digest,
		     verdict, outcome, reason_class, findings, decided_at, deadline_at, created_at, checker_harness,
		     code_context, guidance_snapshot, guidance_text, guidance_digest, repo_instructions_enabled,
		     repo_instructions_text, repo_instructions_digest)
		 VALUES ($1, 'code', 1, 0, repeat('a', 40), repeat('b', 40), '\x01'::bytea,
		     'failed', 'completed', NULL, '[]'::jsonb, $2, $3::timestamptz + interval '1 hour', $3, 'codex',
		     '{}'::jsonb, '', '', sha256(convert_to('', 'UTF8')), false, '', sha256(convert_to('', 'UTF8')))`,
		lead, f.now.Add(-time.Hour), f.now.Add(-2*time.Hour))

	dto := anDecodeQuality(t, f.cookie(u, "/api/analytics/quality?kind=task"))
	if dto.CrossCheck.Plan.Applicable || dto.CrossCheck.Plan.DecidedInWindow != 0 {
		t.Fatalf("kind=task plan = applicable %v decided %d, want not applicable with no counts", dto.CrossCheck.Plan.Applicable, dto.CrossCheck.Plan.DecidedInWindow)
	}
	if !dto.CrossCheck.Code.Applicable || dto.CrossCheck.Code.DecidedInWindow != 1 {
		t.Fatalf("kind=task code = applicable %v decided %d, want applicable with 1", dto.CrossCheck.Code.Applicable, dto.CrossCheck.Code.DecidedInWindow)
	}
}

// A real query timeout through the live router: while another connection holds an ACCESS
// EXCLUSIVE lock on runs the snapshot's first read of runs blocks until the snapshot's 8 s Go context
// deadline (normally before the server-side statement_timeout), the response is a 503 with
// Retry-After, and the admission slot is released for the next request. A watchdog releases the
// lock after 30 s so a regression fails instead of hanging.
func TestAnalyticsQualityStatementTimeoutThroughRouterLiveDB(t *testing.T) {
	f := newAnFixture(t, 2042)
	u := f.user(false)
	jwt := cliMintJWT(t, f.pool, u)

	lockCtx := context.Background()
	tx, err := f.pool.Begin(lockCtx)
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() { once.Do(func() { _ = tx.Rollback(lockCtx) }) }
	t.Cleanup(release)
	if _, err := tx.Exec(lockCtx, `SET LOCAL lock_timeout = '5s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(lockCtx, `LOCK TABLE runs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	watchdog := time.AfterFunc(30*time.Second, release)
	defer watchdog.Stop()

	start := time.Now()
	rec := cookieReq(t, f.router, http.MethodGet, "/api/analytics/quality", jwt, "")
	elapsed := time.Since(start)
	release()
	t.Logf("timeout request took %s", elapsed)
	if elapsed < 7*time.Second || elapsed > 20*time.Second {
		t.Errorf("request took %s, want between 7s and 20s", elapsed)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("locked status = %d %s, want 503", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("503 carries no Retry-After header")
	}
	if !strings.Contains(rec.Body.String(), "analytics query timed out") {
		t.Errorf("503 body = %q, want the query timed out text", rec.Body.String())
	}

	if next := cookieReq(t, f.router, http.MethodGet, "/api/analytics/quality", jwt, ""); next.Code != http.StatusOK {
		t.Fatalf("request after the lock released = %d %s, want 200 (slot released)", next.Code, next.Body.String())
	}
}

// Owner decision (#4684): the all-time window is not offered for admin all-users responses; the
// owner route and an admin-selected user still accept it.
func TestAnalyticsQualityWindowAllLiveDB(t *testing.T) {
	f := newAnFixture(t, 2043)
	admin, owner := f.user(true), f.user(false)

	for _, c := range []struct {
		path string
		want []string
	}{
		{"/api/admin/analytics/options", []string{"7d", "30d", "90d"}},
		{"/api/admin/analytics/options?user=all", []string{"7d", "30d", "90d"}},
	} {
		if got := anDecodeOptions(t, f.cookie(admin, c.path)).Windows; !slices.Equal(got, c.want) {
			t.Fatalf("%s windows = %v, want %v", c.path, got, c.want)
		}
	}
	omitted := f.cookie(admin, "/api/admin/analytics/quality?window=all")
	explicit := f.cookie(admin, "/api/admin/analytics/quality?window=all&user=all")
	if omitted.Code != http.StatusBadRequest || explicit.Code != http.StatusBadRequest || omitted.Body.String() != explicit.Body.String() {
		t.Fatalf("admin all-users window=all = %d %q and %d %q, want the identical 400", omitted.Code, omitted.Body.String(), explicit.Code, explicit.Body.String())
	}
	if !strings.Contains(omitted.Body.String(), "window all not available for all users") {
		t.Fatalf("400 body = %s", omitted.Body.String())
	}

	repo, _ := f.repoOwnedBy(owner, "alltime")
	f.run(owner, repo, time.Date(2043, 4, 1, 9, 0, 0, 0, time.UTC), "1.000000")
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"owner":           f.cookie(owner, "/api/analytics/quality?window=all"),
		"admin user=uuid": f.cookie(admin, "/api/admin/analytics/quality?window=all&user="+owner.String()),
	} {
		dto := anDecodeQuality(t, rec)
		if dto.Window != "all" || dto.BucketWidthDays != 7 || dto.HistoryStart == nil {
			t.Fatalf("%s window=all = window %q width %d history %v", name, dto.Window, dto.BucketWidthDays, dto.HistoryStart)
		}
		if dto.Judge.Eligible.Total != 1 {
			t.Fatalf("%s all-time judge eligible = %d, want the one run months back", name, dto.Judge.Eligible.Total)
		}
		var sum int64
		for _, bk := range dto.Judge.Buckets {
			sum += bk.Eligible
		}
		if len(dto.Judge.Buckets) != len(dto.Buckets) || sum != 1 {
			t.Fatalf("%s judge buckets %d (calendar %d) sum %d, want aligned and 1", name, len(dto.Judge.Buckets), len(dto.Buckets), sum)
		}
	}
}

// The snapshot barrier. A commit made after the first read is invisible to the in-flight
// response and visible to the next; a commit made before the snapshot's first query is counted
// even when its decision time is backdated to before the request's clock.
func TestAnalyticsQualitySnapshotBarrierLiveDB(t *testing.T) {
	f := newAnFixture(t, 2042)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "barrier")
	lead := f.run(owner, repo, f.now.Add(-5*time.Hour), "1.000000")
	anSeedPlanCheck(t, f, lead, 1, "approve", f.now.Add(-4*time.Hour))

	var fired atomic.Int32
	f.h.analyticsDeps.afterFirstRead = func(context.Context) {
		if fired.Add(1) == 1 {
			anSeedPlanCheck(t, f, lead, 2, "revise", f.now.Add(-30*time.Minute))
		}
	}
	first := anDecodeQuality(t, f.cookie(owner, "/api/analytics/quality"))
	f.h.analyticsDeps.afterFirstRead = nil
	if fired.Load() != 1 {
		t.Fatalf("the after-first-read hook fired %d times, want 1", fired.Load())
	}
	if first.CrossCheck.Plan.DecidedInWindow != 1 || first.CrossCheck.Plan.StoredResult.Revise != 0 {
		t.Fatalf("in-flight response saw %+v, want 1 decided (the competing commit must be invisible)", first.CrossCheck.Plan)
	}
	second := anDecodeQuality(t, f.cookie(owner, "/api/analytics/quality"))
	if second.CrossCheck.Plan.DecidedInWindow != 2 || second.CrossCheck.Plan.StoredResult.Revise != 1 {
		t.Fatalf("fresh response saw %+v, want 2 decided with the revise", second.CrossCheck.Plan)
	}

	// Committed before the snapshot's first query, decided before the request clock: counted now.
	var before atomic.Int32
	f.h.analyticsDeps.qualityBeforeSnapshotRead = func(context.Context) {
		if before.Add(1) == 1 {
			anSeedPlanCheck(t, f, lead, 3, "block", f.now.Add(-90*time.Minute))
		}
	}
	third := anDecodeQuality(t, f.cookie(owner, "/api/analytics/quality"))
	f.h.analyticsDeps.qualityBeforeSnapshotRead = nil
	if before.Load() != 1 {
		t.Fatalf("the before-snapshot hook fired %d times, want 1", before.Load())
	}
	if third.CrossCheck.Plan.DecidedInWindow != 3 || third.CrossCheck.Plan.StoredResult.Block != 1 {
		t.Fatalf("response saw %+v, want the backdated block counted in the same response", third.CrossCheck.Plan)
	}
}

// Admission: single-flight busy answers 503 with Retry-After through the router, and the slot is
// released on success, on an in-snapshot error and on cancellation.
func TestAnalyticsQualityAdmissionLiveDB(t *testing.T) {
	f := newAnFixture(t, 2044)
	// The pool-derived capacity is MaxConns/4, which is 1 on a 4-CPU runner (pgxpool's default
	// MaxConns is max(4, NumCPU)), and then the held Quality key alone fills it. Pin capacity 2 so
	// the cross-section check below tests the per-key rule, not the machine's CPU count.
	f.h.analyticsDeps.admission = analytics.NewAdmission(8)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "admission")
	f.run(owner, repo, f.now.Add(-time.Hour), "1.000000")

	release, err := f.h.analyticsDeps.admission.Acquire(owner.String(), analytics.SectionQuality)
	if err != nil {
		t.Fatal(err)
	}
	busy := f.cookie(owner, "/api/analytics/quality")
	if busy.Code != http.StatusServiceUnavailable || busy.Header().Get("Retry-After") != "1" {
		t.Fatalf("busy = %d Retry-After %q %s, want 503 and 1", busy.Code, busy.Header().Get("Retry-After"), busy.Body.String())
	}
	// Spend is a different key and is not blocked.
	if rec := f.cookie(owner, "/api/analytics/spend"); rec.Code != http.StatusOK {
		t.Fatalf("spend while quality held = %d, want 200", rec.Code)
	}
	release()

	// Success releases.
	if rec := f.cookie(owner, "/api/analytics/quality"); rec.Code != http.StatusOK {
		t.Fatalf("after release = %d %s, want 200", rec.Code, rec.Body.String())
	}
	// An error found inside the snapshot releases.
	if rec := f.cookie(owner, "/api/analytics/quality?tz=Mars/Olympus"); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid tz = %d, want 400", rec.Code)
	}
	if rec := f.cookie(owner, "/api/analytics/quality"); rec.Code != http.StatusOK {
		t.Fatalf("after an in-snapshot error = %d %s, want 200", rec.Code, rec.Body.String())
	}

	// A cancelled request releases its slot and its connection.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.h.analyticsDeps.afterFirstRead = func(context.Context) { cancel() }
	req := httptest.NewRequest(http.MethodGet, "/api/analytics/quality", nil).WithContext(mw.ContextWithUser(ctx, store.User{ID: owner}))
	first := httptest.NewRecorder()
	f.h.AnalyticsQuality(first, req)
	f.h.analyticsDeps.afterFirstRead = nil
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("request context err = %v, want context.Canceled (the hook did not cancel)", ctx.Err())
	}
	if first.Body.Len() != 0 {
		t.Fatalf("cancelled request wrote a body: %q", first.Body.String())
	}
	if n := f.pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("%d connections still acquired after a cancelled request", n)
	}
	if rec := f.cookie(owner, "/api/analytics/quality"); rec.Code != http.StatusOK {
		t.Fatalf("request right after a cancelled one = %d %s, want 200", rec.Code, rec.Body.String())
	}
}
