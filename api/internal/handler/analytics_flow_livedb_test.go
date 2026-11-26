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

// Issue #4729: the Flow routes through the real router and a real database. Like the Spend
// tests, each test that asserts all-scope figures claims a calendar year of its own (2041 and
// up). All-users waiting_now counts every user's waiting runs whatever the window, so those
// assertions are lower bounds.

// anFlowRun seeds one issue run. finished is the zero time for a run with no finished_at.
type anFlowRun struct {
	user, repo       uuid.UUID
	status           string
	at, statusAt     time.Time
	finished         time.Time
	failOrigin, kind string
}

func anSeedFlowRun(t *testing.T, pool *pgxpool.Pool, r anFlowRun) uuid.UUID {
	t.Helper()
	if r.statusAt.IsZero() {
		r.statusAt = r.at
	}
	if r.kind == "" {
		r.kind = "issue"
	}
	var finished, failOrigin any
	if !r.finished.IsZero() {
		finished = r.finished
	}
	if r.failOrigin != "" {
		failOrigin = r.failOrigin
	}
	id := uuid.New()
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		                   created_at, finished_at, status_since, harness, fail_origin)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, $7, $8, $9, 'claude', $10)`,
		id, r.user, r.repo, 5_000_000+anSeq.Add(1), r.status, r.kind, r.at, finished, r.statusAt, failOrigin)
	return id
}

func anDecodeFlow(t *testing.T, rec *httptest.ResponseRecorder) apitypes.AnalyticsFlowDTO {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var dto apitypes.AnalyticsFlowDTO
	dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&dto); err != nil {
		t.Fatalf("decode flow: %v: %s", err, rec.Body.String())
	}
	return dto
}

func TestAnalyticsFlowCredentialsLiveDB(t *testing.T) {
	f := newAnFixture(t, 2041)
	admin, member := f.user(true), f.user(false)
	adminRO := cliMintToken(t, f.pool, admin, clitoken.ScopeAdminRO)
	adminDefault := cliMintToken(t, f.pool, admin, clitoken.ScopeUser)
	memberToken := cliMintToken(t, f.pool, member, clitoken.ScopeUser)
	adminJWT, memberJWT := cliMintJWT(t, f.pool, admin), cliMintJWT(t, f.pool, member)

	adminPath := "/api/admin/analytics/flow"
	if rec := bearerReq(f.router, http.MethodGet, adminPath, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("%s no credential = %d, want 401", adminPath, rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, adminPath, adminJWT, ""); rec.Code != http.StatusOK {
		t.Fatalf("%s admin cookie = %d %s, want 200", adminPath, rec.Code, rec.Body.String())
	}
	if rec := bearerReq(f.router, http.MethodGet, adminPath, adminRO); rec.Code != http.StatusOK {
		t.Fatalf("%s admin_ro bearer = %d %s, want 200", adminPath, rec.Code, rec.Body.String())
	}
	if rec := bearerReq(f.router, http.MethodGet, adminPath, adminDefault); rec.Code != http.StatusForbidden {
		t.Fatalf("%s admin default-scope bearer = %d, want 403", adminPath, rec.Code)
	}
	if rec := cookieReq(t, f.router, http.MethodGet, adminPath, memberJWT, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("%s member cookie = %d, want 403", adminPath, rec.Code)
	}
	if rec := bearerReq(f.router, http.MethodGet, adminPath, memberToken); rec.Code != http.StatusForbidden {
		t.Fatalf("%s member bearer = %d, want 403", adminPath, rec.Code)
	}

	ownerPath := "/api/analytics/flow"
	if rec := bearerReq(f.router, http.MethodGet, ownerPath, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("%s no credential = %d, want 401", ownerPath, rec.Code)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"admin cookie":    cookieReq(t, f.router, http.MethodGet, ownerPath, adminJWT, ""),
		"member cookie":   cookieReq(t, f.router, http.MethodGet, ownerPath, memberJWT, ""),
		"member bearer":   bearerReq(f.router, http.MethodGet, ownerPath, memberToken),
		"admin default":   bearerReq(f.router, http.MethodGet, ownerPath, adminDefault),
		"admin_ro bearer": bearerReq(f.router, http.MethodGet, ownerPath, adminRO),
	} {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s = %d %s, want 200", ownerPath, name, rec.Code, rec.Body.String())
		}
	}
}

// A seeded scope: owner isolation, admin selected-user scope and the all-users scope.
func TestAnalyticsFlowSeededScopesLiveDB(t *testing.T) {
	f := newAnFixture(t, 2042)
	admin, a, b := f.user(true), f.user(false), f.user(false)
	repoA, _ := f.repoOwnedBy(a, "fa")
	repoB, _ := f.repoOwnedBy(b, "fb")
	h := func(d time.Duration) time.Time { return f.now.Add(-d) }

	// A: one queued, one awaiting approval, one completed (created 30h ago, finished 29h ago).
	anSeedFlowRun(t, f.pool, anFlowRun{user: a, repo: repoA, status: "queued", at: h(2 * time.Hour)})
	anSeedFlowRun(t, f.pool, anFlowRun{user: a, repo: repoA, status: "awaiting_approval", at: h(5 * time.Hour)})
	anSeedFlowRun(t, f.pool, anFlowRun{user: a, repo: repoA, status: "completed", at: h(30 * time.Hour), finished: h(29 * time.Hour)})
	// B: two queued and one completed, none of which may reach A's responses.
	anSeedFlowRun(t, f.pool, anFlowRun{user: b, repo: repoB, status: "queued", at: h(time.Hour)})
	anSeedFlowRun(t, f.pool, anFlowRun{user: b, repo: repoB, status: "queued", at: h(90 * time.Minute)})
	anSeedFlowRun(t, f.pool, anFlowRun{user: b, repo: repoB, status: "completed", at: h(3 * time.Hour), finished: h(2 * time.Hour)})

	queued := func(d apitypes.AnalyticsFlowDTO) apitypes.AnalyticsFlowWaitStateDTO { return d.WaitingNow.States[0] }

	// Owner route: only the caller's runs.
	ownA := anDecodeFlow(t, f.cookie(a, "/api/analytics/flow"))
	if ownA.Scope.Route != "owner" || ownA.SelectedUser != a.String() || ownA.Scope.User != a.String() {
		t.Fatalf("owner scope %+v selected %q", ownA.Scope, ownA.SelectedUser)
	}
	if ownA.Activity.Created.Total != 3 || ownA.WaitingNow.Total != 2 || ownA.Activity.Finished.Total.Completed != 1 || ownA.Activity.Finished.Total.Total != 1 {
		t.Fatalf("owner A: created %d waiting %d finished %+v, want 3 / 2 / one completed",
			ownA.Activity.Created.Total, ownA.WaitingNow.Total, ownA.Activity.Finished.Total)
	}
	if q := queued(ownA); q.State != "queued" || q.Count != 1 || q.OldestAgeSeconds == nil || *q.OldestAgeSeconds != 7200 {
		t.Fatalf("owner A queued = %+v, want one run aged 7200s", q)
	}
	if ownA.DeliveryFunnel.Stages.Created != 3 || ownA.IssueElapsed.Applicable != true {
		t.Fatalf("owner A funnel %+v elapsed applicable %v", ownA.DeliveryFunnel.Stages, ownA.IssueElapsed.Applicable)
	}
	if len(ownA.Activity.Created.Buckets) != len(ownA.Buckets) || len(ownA.IssueElapsed.Buckets) != len(ownA.Buckets) {
		t.Fatalf("bucket cells %d / %d vs calendar %d", len(ownA.Activity.Created.Buckets), len(ownA.IssueElapsed.Buckets), len(ownA.Buckets))
	}
	for i, c := range ownA.IssueElapsed.Buckets {
		if c.Index != i || c.Partial != ownA.Buckets[i].Partial {
			t.Fatalf("elapsed bucket %d = index %d partial %v, calendar partial %v", i, c.Index, c.Partial, ownA.Buckets[i].Partial)
		}
	}

	ownB := anDecodeFlow(t, f.cookie(b, "/api/analytics/flow"))
	if ownB.Activity.Created.Total != 3 || queued(ownB).Count != 2 || ownB.Activity.Finished.Total.Completed != 1 {
		t.Fatalf("owner B: created %d queued %d finished %+v", ownB.Activity.Created.Total, queued(ownB).Count, ownB.Activity.Finished.Total)
	}

	// An admin on the owner route stays in their own (empty) scope.
	if adminOwn := anDecodeFlow(t, f.cookie(admin, "/api/analytics/flow")); adminOwn.Activity.Created.Total != 0 || adminOwn.WaitingNow.Total != 0 {
		t.Fatalf("admin owner-route sees %d created / %d waiting, want their own empty scope", adminOwn.Activity.Created.Total, adminOwn.WaitingNow.Total)
	}

	// Admin selected-user scope reads the same figures as that user's owner route.
	selA := anDecodeFlow(t, f.cookie(admin, "/api/admin/analytics/flow?user="+a.String()))
	if selA.Scope.Route != "admin" || selA.Scope.User != a.String() || selA.SelectedUser != a.String() {
		t.Fatalf("admin user=A scope %+v selected %q", selA.Scope, selA.SelectedUser)
	}
	if selA.Activity.Created.Total != 3 || selA.WaitingNow.Total != 2 || !slices.Equal(selA.Activity.Created.Buckets, ownA.Activity.Created.Buckets) {
		t.Fatalf("admin user=A: created %d waiting %d, want owner A's figures", selA.Activity.Created.Total, selA.WaitingNow.Total)
	}

	// All users: both users' runs in the claimed year; waiting_now has no window, so a lower bound.
	all := anDecodeFlow(t, f.cookie(admin, "/api/admin/analytics/flow"))
	if all.Scope.User != "all" || all.SelectedUser != "all" || all.Activity.Created.Total != 6 || all.Activity.Finished.Total.Completed != 2 || queued(all).Count < 3 {
		t.Fatalf("admin all: scope %+v created %d completed %d queued %d, want all / 6 / 2 / >=3",
			all.Scope, all.Activity.Created.Total, all.Activity.Finished.Total.Completed, queued(all).Count)
	}
	explicit := anDecodeFlow(t, f.cookie(admin, "/api/admin/analytics/flow?user=all"))
	if explicit.Activity.Created.Total != all.Activity.Created.Total {
		t.Fatalf("user=all created %d vs omitted %d", explicit.Activity.Created.Total, all.Activity.Created.Total)
	}

	// Filters: the repo selector narrows and is echoed; another user's repo is the unknown-repo 400.
	byRepo := anDecodeFlow(t, f.cookie(a, "/api/analytics/flow?repo="+repoA.String()+"&harness=claude&kind=issue"))
	if byRepo.Filters.Repo == nil || *byRepo.Filters.Repo != repoA.String() || byRepo.Filters.Harness == nil || *byRepo.Filters.Harness != "claude" ||
		!slices.Equal(byRepo.Filters.Kinds, []string{"issue"}) || byRepo.Activity.Created.Total != 3 {
		t.Fatalf("filtered response: filters %+v created %d", byRepo.Filters, byRepo.Activity.Created.Total)
	}
	foreign := f.cookie(a, "/api/analytics/flow?repo="+repoB.String())
	unknown := f.cookie(a, "/api/analytics/flow?repo="+uuid.NewString())
	if foreign.Code != http.StatusBadRequest || foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("foreign repo = %d %s, unknown repo = %d %s, want identical 400s", foreign.Code, foreign.Body.String(), unknown.Code, unknown.Body.String())
	}
	if codex := anDecodeFlow(t, f.cookie(a, "/api/analytics/flow?harness=codex")); codex.Activity.Created.Total != 0 {
		t.Fatalf("harness=codex counted %d claude runs", codex.Activity.Created.Total)
	}
	chatOnly := anDecodeFlow(t, f.cookie(a, "/api/analytics/flow?kind=chat"))
	if chatOnly.DeliveryFunnel.Applicable || chatOnly.IssueElapsed.Applicable || chatOnly.Activity.Created.Total != 0 {
		t.Fatalf("kind=chat: funnel applicable %v elapsed applicable %v created %d", chatOnly.DeliveryFunnel.Applicable, chatOnly.IssueElapsed.Applicable, chatOnly.Activity.Created.Total)
	}

	// The body names no repo id or path beyond the caller's own selector echo.
	rec := f.cookie(a, "/api/analytics/flow?repo="+repoA.String())
	if n := strings.Count(rec.Body.String(), repoA.String()); n != 1 {
		t.Fatalf("repo id appears %d times in the body, want once (filters echo)", n)
	}
	if body := f.cookie(a, "/api/analytics/flow").Body.String(); strings.Contains(body, repoA.String()) || strings.Contains(body, repoB.String()) {
		t.Fatalf("an unfiltered body names a repo id: %s", body)
	}

	// The owner route refuses user, split and breakdown; the admin route refuses split.
	for path, want := range map[string]string{
		"/api/analytics/flow?user=" + b.String():                       "user not allowed",
		"/api/analytics/flow?user=all":                                 "user not allowed",
		"/api/analytics/flow?split=repo":                               "invalid split",
		"/api/admin/analytics/flow?breakdown=repo":                     "invalid breakdown",
		"/api/admin/analytics/flow?user=" + a.String() + "&split=user": "invalid split",
	} {
		rec := f.cookie(admin, path)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s = %d %s, want 400 %q", path, rec.Code, rec.Body.String(), want)
		}
	}
	if rec := f.cookie(admin, "/api/admin/analytics/flow?user="+uuid.NewString()); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown user = %d, want 400", rec.Code)
	}
}

// Owner decision: no all-time window for admin all-users; one user may use it.
func TestAnalyticsFlowWindowAllLiveDB(t *testing.T) {
	f := newAnFixture(t, 2043)
	admin, owner := f.user(true), f.user(false)
	repo, _ := f.repoOwnedBy(owner, "flowall")
	// More than 30 days back (2043-04-01 is a Wednesday), so only window=all reaches it.
	anSeedFlowRun(t, f.pool, anFlowRun{user: owner, repo: repo, status: "completed",
		at: time.Date(2043, 4, 1, 9, 0, 0, 0, time.UTC), finished: time.Date(2043, 4, 1, 11, 0, 0, 0, time.UTC)})

	omitted := f.cookie(admin, "/api/admin/analytics/flow?window=all")
	explicit := f.cookie(admin, "/api/admin/analytics/flow?window=all&user=all")
	if omitted.Code != http.StatusBadRequest || explicit.Code != http.StatusBadRequest || omitted.Body.String() != explicit.Body.String() {
		t.Fatalf("admin all-users window=all = %d %s and %d %s, want identical 400s", omitted.Code, omitted.Body.String(), explicit.Code, explicit.Body.String())
	}
	if !strings.Contains(omitted.Body.String(), msgAnalyticsWindowAllAllUsers) {
		t.Fatalf("400 body %s lacks %q", omitted.Body.String(), msgAnalyticsWindowAllAllUsers)
	}
	for _, w := range anDecodeOptions(t, f.cookie(admin, "/api/admin/analytics/options")).Windows {
		if w == "all" {
			t.Fatalf("admin all-users options offer window all")
		}
	}

	for name, rec := range map[string]*httptest.ResponseRecorder{
		"owner":           f.cookie(owner, "/api/analytics/flow?window=all"),
		"admin user=uuid": f.cookie(admin, "/api/admin/analytics/flow?window=all&user="+owner.String()),
	} {
		dto := anDecodeFlow(t, rec)
		if dto.Window != "all" || dto.PreviousFrom != nil || dto.HistoryStart == nil || dto.BucketWidthDays != 7 {
			t.Fatalf("%s: window %q previous %v history %v width %d", name, dto.Window, dto.PreviousFrom, dto.HistoryStart, dto.BucketWidthDays)
		}
		if dto.Activity.Created.Total != 1 || dto.Activity.Finished.Total.Completed != 1 {
			t.Fatalf("%s: created %d finished %+v, want the old run", name, dto.Activity.Created.Total, dto.Activity.Finished.Total)
		}
		if len(dto.IssueElapsed.Buckets) != len(dto.Buckets) || len(dto.Activity.Created.Buckets) != len(dto.Buckets) {
			t.Fatalf("%s: %d elapsed / %d created cells vs %d calendar buckets", name, len(dto.IssueElapsed.Buckets), len(dto.Activity.Created.Buckets), len(dto.Buckets))
		}
		if last := len(dto.Buckets) - 1; !dto.IssueElapsed.Buckets[last].Partial || dto.IssueElapsed.Buckets[0].Partial {
			t.Fatalf("%s: elapsed partial flags first=%v last=%v, want false/true", name, dto.IssueElapsed.Buckets[0].Partial, dto.IssueElapsed.Buckets[last].Partial)
		}
	}
	// The default 30d window does not reach the run.
	if def := anDecodeFlow(t, f.cookie(owner, "/api/analytics/flow")); def.Activity.Created.Total != 0 {
		t.Fatalf("30d created = %d, want 0", def.Activity.Created.Total)
	}
}

// An empty scope: every list is [] on the wire, the envelope is set and every elapsed bucket
// carries partial.
func TestAnalyticsFlowEmptyScopeLiveDB(t *testing.T) {
	f := newAnFixture(t, 2044)
	owner := f.user(false)

	lists := [][]string{
		{"buckets"}, {"rolling"}, {"filters", "kinds"}, {"waiting_now", "states"},
		{"activity", "created", "buckets"}, {"activity", "finished", "excluded_kinds"}, {"activity", "finished", "buckets"},
		{"first_start_delay", "limitations"}, {"delivery_funnel", "pr_units", "buckets"},
		{"issue_elapsed", "histogram"}, {"issue_elapsed", "buckets"},
	}
	rawLists := func(body []byte) map[string]int {
		var tree map[string]any
		if err := json.Unmarshal(body, &tree); err != nil {
			t.Fatal(err)
		}
		out := map[string]int{}
		for _, path := range lists {
			var v any = tree
			for _, k := range path {
				v = v.(map[string]any)[k]
			}
			arr, ok := v.([]any)
			if !ok || arr == nil {
				t.Fatalf("%s = %v in %s, want a JSON array", strings.Join(path, "."), v, body)
			}
			out[strings.Join(path, ".")] = len(arr)
		}
		return out
	}

	rec := f.cookie(owner, "/api/analytics/flow")
	dto := anDecodeFlow(t, rec)
	n := rawLists(rec.Body.Bytes())
	if dto.Window != "30d" || dto.Timezone != "UTC" || dto.Scope.Route != "owner" || dto.GeneratedAt.IsZero() || dto.HistoryStart != nil {
		t.Fatalf("envelope = %+v", dto.AnalyticsEnvelopeDTO)
	}
	if n["buckets"] != 30 || n["issue_elapsed.buckets"] != 30 || n["activity.created.buckets"] != 30 || n["activity.finished.buckets"] != 30 || n["waiting_now.states"] != 8 {
		t.Fatalf("list sizes = %v", n)
	}
	if n["issue_elapsed.histogram"] != len(store.FlowHistogramEdgesSeconds) || n["filters.kinds"] == 0 {
		t.Fatalf("histogram %d (want %d bins) kinds %d", n["issue_elapsed.histogram"], len(store.FlowHistogramEdgesSeconds), n["filters.kinds"])
	}
	for i, c := range dto.IssueElapsed.Buckets {
		if c.Index != i || c.Partial != (i == 29) {
			t.Fatalf("elapsed bucket %d = index %d partial %v, want partial only on the last", i, c.Index, c.Partial)
		}
		if c.P50Seconds.Value != nil || c.P50Seconds.Reason == nil || *c.P50Seconds.Reason != store.AnalyticsReasonNoObservations {
			t.Fatalf("empty bucket %d p50 = %+v", i, c.P50Seconds)
		}
	}
	if dto.WaitingNow.Total != 0 || dto.Activity.Created.Total != 0 || dto.DeliveryFunnel.Stages.Created != 0 {
		t.Fatalf("empty scope has counts: %+v", dto.WaitingNow)
	}
	if dto.Filters.Repo != nil || dto.Filters.Harness != nil {
		t.Fatalf("filters = %+v, want nil repo and harness", dto.Filters)
	}

	// window=all over an empty scope has no buckets and still no nulls.
	rec = f.cookie(owner, "/api/analytics/flow?window=all")
	all := anDecodeFlow(t, rec)
	n = rawLists(rec.Body.Bytes())
	if n["buckets"] != 0 || n["issue_elapsed.buckets"] != 0 || len(all.IssueElapsed.Buckets) != 0 {
		t.Fatalf("empty all-time list sizes = %v", n)
	}
}

// A cancelled flow request releases its admission slot and its connection; every other exit
// (a 400 found in the snapshot, a success) releases too; while a request holds the slot, a
// second one is a 503 with Retry-After and begins nothing.
func TestAnalyticsFlowAdmissionReleasedOnEveryExitLiveDB(t *testing.T) {
	f := newAnFixture(t, 2045)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "fadm")
	anSeedFlowRun(t, f.pool, anFlowRun{user: owner, repo: repo, status: "completed", at: f.now.Add(-time.Hour), finished: f.now.Add(-30 * time.Minute)})

	// Cancelled mid-request.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.h.analyticsDeps.afterFirstRead = func(context.Context) { cancel() }
	first := httptest.NewRecorder()
	f.h.AnalyticsFlow(first, httptest.NewRequest(http.MethodGet, "/api/analytics/flow", nil).WithContext(mw.ContextWithUser(ctx, store.User{ID: owner})))
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

	// A 400 found inside the snapshot (an unknown timezone), twice: the second is admitted.
	for i := 1; i <= 2; i++ {
		if rec := f.cookie(owner, "/api/analytics/flow?tz=Nowhere/Land"); rec.Code != http.StatusBadRequest {
			t.Fatalf("bad tz attempt %d = %d %s, want 400", i, rec.Code, rec.Body.String())
		}
	}
	// A success, twice.
	for i := 1; i <= 2; i++ {
		if got := anDecodeFlow(t, f.cookie(owner, "/api/analytics/flow")).Activity.Created.Total; got != 1 {
			t.Fatalf("success attempt %d counted %d runs, want 1", i, got)
		}
	}

	// Single flight: while the slot is held, the same caller's flow request is a 503.
	release, err := f.h.analyticsDeps.admission.Acquire(owner.String(), "flow")
	if err != nil {
		t.Fatal(err)
	}
	busy := f.cookie(owner, "/api/analytics/flow")
	release()
	if busy.Code != http.StatusServiceUnavailable || busy.Header().Get("Retry-After") != "1" {
		t.Fatalf("busy = %d Retry-After %q %s, want 503 and 1", busy.Code, busy.Header().Get("Retry-After"), busy.Body.String())
	}
	if rec := f.cookie(owner, "/api/analytics/flow"); rec.Code != http.StatusOK {
		t.Fatalf("after release = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

// A valid non-UTC tz reaches the calendar through the Flow handler: the buckets start at local
// midnights (including the 23h spring-forward day) and a run created at 23:30 local, which is
// already the next UTC day, lands in the local day's created cell.
func TestAnalyticsFlowLocalTimezoneLiveDB(t *testing.T) {
	f := newAnFixture(t, 2047)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "ftz")
	// 2047-03-10 is the second Sunday of March: New York springs forward that day.
	f.now = time.Date(2047, 3, 12, 12, 0, 0, 0, time.UTC)
	// 03:30Z on Mar 12 is 23:30 EDT on Mar 11: UTC says the 12th, New York the 11th.
	anSeedFlowRun(t, f.pool, anFlowRun{user: owner, repo: repo, status: "queued", at: time.Date(2047, 3, 12, 3, 30, 0, 0, time.UTC)})

	dto := anDecodeFlow(t, f.cookie(owner, "/api/analytics/flow?window=7d&tz=America/New_York"))
	if dto.Timezone != "America/New_York" {
		t.Fatalf("timezone = %q, want America/New_York", dto.Timezone)
	}
	wantStarts := []time.Time{
		time.Date(2047, 3, 6, 5, 0, 0, 0, time.UTC), time.Date(2047, 3, 7, 5, 0, 0, 0, time.UTC),
		time.Date(2047, 3, 8, 5, 0, 0, 0, time.UTC), time.Date(2047, 3, 9, 5, 0, 0, 0, time.UTC),
		time.Date(2047, 3, 10, 5, 0, 0, 0, time.UTC), // EST midnight; the day is 23h long
		time.Date(2047, 3, 11, 4, 0, 0, 0, time.UTC), // EDT midnights from here on
		time.Date(2047, 3, 12, 4, 0, 0, 0, time.UTC),
	}
	if len(dto.Buckets) != len(wantStarts) {
		t.Fatalf("%d buckets, want %d", len(dto.Buckets), len(wantStarts))
	}
	for i, w := range wantStarts {
		if !dto.Buckets[i].Start.Equal(w) {
			t.Fatalf("bucket %d starts %s, want %s", i, dto.Buckets[i].Start.UTC(), w)
		}
	}
	if d := dto.Buckets[4].End.Sub(dto.Buckets[4].Start); d != 23*time.Hour {
		t.Fatalf("transition bucket spans %s, want 23h", d)
	}
	if dto.Activity.Created.Total != 1 {
		t.Fatalf("created total = %d, want 1", dto.Activity.Created.Total)
	}
	got := map[int]int64{}
	for _, c := range dto.Activity.Created.Buckets {
		got[c.Index] = c.Count
	}
	// Index 5 is the local Mar 11 day; the UTC calendar would put the run in index 6.
	if got[5] != 1 || got[6] != 0 {
		t.Fatalf("created cells = %v, want the run in index 5 only", got)
	}
}

// Admin selected-user scope: a merged run of another user on the same (repo, mr_iid) is not a
// merge witness for the selected user, while the selected user's own out-of-window merged run
// on that PR is.
func TestAnalyticsFlowSelectedUserWitnessLiveDB(t *testing.T) {
	f := newAnFixture(t, 2048)
	admin, a, b := f.user(true), f.user(false), f.user(false)
	repo, _ := f.repoOwnedBy(a, "fwit")
	const mrIID = 4_729_001
	seedPR := func(user uuid.UUID, state string, at time.Time) {
		id := anSeedFlowRun(t, f.pool, anFlowRun{user: user, repo: repo, status: "completed", at: at, finished: at.Add(time.Hour)})
		cliMustExec(t, f.pool, `UPDATE runs SET mr_iid = $2, mr_state = $3 WHERE id = $1`, id, mrIID, state)
	}
	seedPR(a, "opened", f.now.Add(-2*time.Hour))
	seedPR(b, "merged", f.now.Add(-time.Hour))

	path := "/api/admin/analytics/flow?user=" + a.String()
	dto := anDecodeFlow(t, f.cookie(admin, path))
	if dto.DeliveryFunnel.PRUnits.Present != 1 || dto.DeliveryFunnel.PRUnits.Merged != 0 || dto.DeliveryFunnel.Stages.Merged != 0 {
		t.Fatalf("B's merged run witnessed A's PR: pr_units present %d merged %d, merged stage %d, want 1 / 0 / 0",
			dto.DeliveryFunnel.PRUnits.Present, dto.DeliveryFunnel.PRUnits.Merged, dto.DeliveryFunnel.Stages.Merged)
	}

	// Control: A's own merged run on the same PR, outside the 30d window, is a witness.
	seedPR(a, "merged", f.now.Add(-60*24*time.Hour))
	dto = anDecodeFlow(t, f.cookie(admin, path))
	if dto.DeliveryFunnel.PRUnits.Present != 1 || dto.DeliveryFunnel.PRUnits.Merged != 1 || dto.DeliveryFunnel.Stages.Merged != 1 {
		t.Fatalf("A's own out-of-window merged run: pr_units present %d merged %d, merged stage %d, want 1 / 1 / 1",
			dto.DeliveryFunnel.PRUnits.Present, dto.DeliveryFunnel.PRUnits.Merged, dto.DeliveryFunnel.Stages.Merged)
	}
}

// The snapshot barrier: a run committed on another connection after the first read is not seen
// by the in-flight response and is seen by the next request.
func TestAnalyticsFlowSnapshotBarrierLiveDB(t *testing.T) {
	f := newAnFixture(t, 2046)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "fbarrier")
	anSeedFlowRun(t, f.pool, anFlowRun{user: owner, repo: repo, status: "queued", at: f.now.Add(-time.Hour)})

	var fired atomic.Int32
	f.h.analyticsDeps.afterFirstRead = func(context.Context) {
		if fired.Add(1) != 1 {
			return
		}
		// pool.Exec takes a second connection: the request's snapshot connection is busy.
		anSeedFlowRun(t, f.pool, anFlowRun{user: owner, repo: repo, status: "queued", at: f.now.Add(-30 * time.Minute)})
	}
	first := anDecodeFlow(t, f.cookie(owner, "/api/analytics/flow"))
	if fired.Load() != 1 {
		t.Fatalf("the after-first-read hook fired %d times, want 1", fired.Load())
	}
	if first.Activity.Created.Total != 1 || first.WaitingNow.Total != 1 {
		t.Fatalf("in-flight response saw %d created / %d waiting, want 1 / 1 (the competing commit must be invisible)", first.Activity.Created.Total, first.WaitingNow.Total)
	}
	second := anDecodeFlow(t, f.cookie(owner, "/api/analytics/flow"))
	if second.Activity.Created.Total != 2 || second.WaitingNow.Total != 2 {
		t.Fatalf("fresh response saw %d created / %d waiting, want 2 / 2", second.Activity.Created.Total, second.WaitingNow.Total)
	}
}
