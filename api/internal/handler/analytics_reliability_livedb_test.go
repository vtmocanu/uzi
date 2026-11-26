package handler

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4685: the Reliability routes through the real router and a real database. Every test
// claims a calendar year of its own (see analytics_livedb_test.go) and runs with a fixed clock.

// relCaveat is the documented needs_landing caveat, asserted as a literal independent of the
// handler's constant.
const relCaveat = "work_may_already_have_landed"

type relModel struct {
	name string
	out  int64
}

// relSeed is one run to seed. Terminal statuses get finished_at = created + 1h unless finished is set.
type relSeed struct {
	user, repo uuid.UUID
	kind       string // default issue
	status     string // default completed
	origin     string // fail_origin
	created    time.Time
	finished   time.Time
	harness    string // default claude
	requeue    int
	patch      bool
	models     []relModel
}

// outcome is the run's class by the documented rules, computed from the seed alone.
func (s relSeed) outcome() string {
	switch {
	case s.status == "completed" || s.status == "cancelled":
		return s.status
	case s.status == "failed" && (s.origin == "plan_rejected" || s.origin == "base_diverged"):
		return s.origin
	case s.status == "failed":
		return "failed"
	}
	return ""
}

func (s relSeed) harnessOrDefault() string {
	if s.harness == "" {
		return "claude"
	}
	return s.harness
}

// primaryModel is the model with the most output tokens, or "unknown" without usage.
func (s relSeed) primaryModel() string {
	best, bestOut := "unknown", int64(-1)
	for _, m := range s.models {
		if m.out > bestOut || (m.out == bestOut && m.name < best) {
			best, bestOut = m.name, m.out
		}
	}
	return best
}

func (f *anFixture) relRun(s relSeed) uuid.UUID {
	f.t.Helper()
	return relInsert(f.t, f.pool, s)
}

func relInsert(t *testing.T, pool *pgxpool.Pool, s relSeed) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if s.kind == "" {
		s.kind = runkind.Issue
	}
	if s.status == "" {
		s.status = "completed"
	}
	var repo, iid, finished, patch, origin any
	if s.repo != uuid.Nil {
		repo = s.repo
	}
	if s.kind == runkind.Issue {
		iid = 5_000_000 + anSeq.Add(1)
	}
	switch {
	case !s.finished.IsZero():
		finished = s.finished
	case s.status == "completed" || s.status == "cancelled" || s.status == "failed":
		finished = s.created.Add(time.Hour)
	}
	if s.patch {
		patch = "diff --git a/x b/x"
	}
	if s.origin != "" {
		origin = s.origin
	}
	cliMustExec(t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, created_at, status_since,
		                   finished_at, harness, fail_origin, requeue_count, preserved_patch)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, $6, $7, $7, $8, $9, $10, $11, $12)`,
		id, s.user, repo, iid, s.status, s.kind, s.created, finished, s.harnessOrDefault(), origin, s.requeue, patch)
	for i, m := range s.models {
		cliMustExec(t, pool,
			`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, usage_basis,
			     input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
			 VALUES ($1, $2, $3, 0, $4, 'per_leg', 10, 5, 1, $5, 0.5::numeric, 'metered')`,
			id, fmt.Sprintf("rel-%d", anSeq.Add(1)), m.name, i, m.out)
	}
	return id
}

func (f *anFixture) rel(user uuid.UUID, path string) (apitypes.AnalyticsReliabilityDTO, *httptest.ResponseRecorder) {
	f.t.Helper()
	rec := f.cookie(user, path)
	dto, _ := decodeReliability(f.t, rec)
	return dto, rec
}

func relNear(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-6 {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func relWantNull(t *testing.T, label string, v *float64, reason *string, want string) {
	t.Helper()
	if v != nil || reason == nil || *reason != want {
		t.Fatalf("%s = value %v reason %v, want null with %q", label, v, reason, want)
	}
}

func relOutcomes(seeds []relSeed) (fin int64, o apitypes.AnalyticsOutcomesDTO) {
	for _, s := range seeds {
		switch s.outcome() {
		case "completed":
			o.Completed++
		case "cancelled":
			o.Cancelled++
		case "plan_rejected":
			o.PlanRejected++
		case "base_diverged":
			o.BaseDiverged++
		case "failed":
			o.Failed++
		default:
			continue
		}
		fin++
	}
	return fin, o
}

// Acceptance example 1: an owner's 30-day figures reconcile across totals, buckets, causes,
// rolling points and the breakdown, computed from the seeds alone; another owner is invisible.
func TestAnalyticsReliabilityOwnerReconcilesLiveDB(t *testing.T) {
	f := newAnFixture(t, 2041)
	owner, other := f.user(false), f.user(false)
	repo, _ := f.repoOwnedBy(owner, "rel")
	otherRepo, otherPath := f.repoOwnedBy(other, "rel-other")
	landable := workersvc.AllHumanLandableFailOrigins()[0]
	day := func(d int) time.Time { return f.now.Add(-time.Duration(d) * 24 * time.Hour) }

	seeds := []relSeed{
		{status: "completed", created: day(20), models: []relModel{{"m-a", 300}}},
		{status: "completed", created: day(10), models: []relModel{{"m-a", 300}, {"m-b", 50}}},
		{status: "completed", created: day(5), harness: "codex", models: []relModel{{"m-c", 10}}},
		{status: "completed", created: day(2), harness: "codex"},
		{status: "failed", origin: "agent_failure", created: day(8), requeue: 2, models: []relModel{{"m-a", 20}}},
		{status: "failed", origin: landable, created: day(3), harness: "codex", patch: true, models: []relModel{{"m-c", 20}}},
		{status: "cancelled", created: day(6)},
		{status: "failed", origin: "plan_rejected", created: day(7)},
		{status: "failed", origin: "base_diverged", created: day(9)},
		{status: "queued", created: day(1)},
	}
	for i := range seeds {
		seeds[i].user, seeds[i].repo = owner, repo
		f.relRun(seeds[i])
	}
	// Another owner's runs must stay out of every figure.
	for _, s := range []relSeed{
		{status: "failed", origin: "agent_failure", created: day(2)},
		{status: "failed", origin: "agent_failure", created: day(3)},
		{status: "completed", created: day(1)},
	} {
		s.user, s.repo = other, otherRepo
		f.relRun(s)
	}

	dto, rec := f.rel(owner, "/api/analytics/reliability")
	body := rec.Body.String()
	if strings.Contains(body, other.String()) || strings.Contains(body, otherPath) || strings.Contains(body, otherRepo.String()) {
		t.Fatalf("owner response mentions another owner: %s", body)
	}
	if !dto.Applicable || dto.Reason != nil || dto.Scope.Route != "owner" || dto.SelectedUser != owner.String() || dto.Window != "30d" {
		t.Fatalf("header = %+v", dto)
	}

	wantFin, wantOut := relOutcomes(seeds)
	cur := dto.Totals.Current
	if wantFin != 9 || wantOut != (apitypes.AnalyticsOutcomesDTO{Completed: 4, Cancelled: 1, PlanRejected: 1, BaseDiverged: 1, Failed: 2}) {
		t.Fatalf("seed arithmetic drifted: %d %+v", wantFin, wantOut)
	}
	if cur.Finished != wantFin || cur.Outcomes != wantOut {
		t.Fatalf("totals.current = %+v, want %d finished %+v (the in-flight run is in no finished figure)", cur, wantFin, wantOut)
	}
	relNear(t, "failure_rate", cur.FailureRate.Value, 2.0/9.0)
	if cur.RequeuedRuns != 1 {
		t.Fatalf("requeued_runs = %d, want 1", cur.RequeuedRuns)
	}
	if dto.Totals.Previous == nil || dto.Totals.Previous.Finished != 0 {
		t.Fatalf("totals.previous = %+v, want a bounded window with no runs", dto.Totals.Previous)
	}
	relWantNull(t, "previous failure_rate", dto.Totals.Previous.FailureRate.Value, dto.Totals.Previous.FailureRate.Reason, "no_finished_runs")

	// Buckets: every bucket present, each cell recomputed from the seeds by creation time.
	if len(dto.Series) != len(dto.Buckets) || len(dto.Buckets) != 30 {
		t.Fatalf("series %d buckets %d, want 30 each", len(dto.Series), len(dto.Buckets))
	}
	var sumFin int64
	var sumOut apitypes.AnalyticsOutcomesDTO
	for i, c := range dto.Series {
		var in []relSeed
		for _, s := range seeds {
			if !s.created.Before(dto.Buckets[i].Start) && s.created.Before(dto.Buckets[i].End) {
				in = append(in, s)
			}
		}
		fin, out := relOutcomes(in)
		if c.Index != i || c.Finished != fin || c.Outcomes != out {
			t.Fatalf("series[%d] = %+v, want index %d finished %d %+v", i, c, i, fin, out)
		}
		sumFin += c.Finished
		sumOut.Completed += c.Outcomes.Completed
		sumOut.Failed += c.Outcomes.Failed
	}
	if sumFin != cur.Finished || sumOut.Completed != cur.Outcomes.Completed || sumOut.Failed != cur.Outcomes.Failed {
		t.Fatalf("buckets do not add up to the totals: %d %+v vs %+v", sumFin, sumOut, cur)
	}

	// Causes: the two failures, by origin, shares adding up to one.
	if dto.Causes.Failed != 2 || len(dto.Causes.Items) != 2 {
		t.Fatalf("causes = %+v, want 2 failed in 2 items", dto.Causes)
	}
	var share float64
	byOrigin := map[string]int64{}
	for _, c := range dto.Causes.Items {
		if c.Origin == nil || c.IsOther {
			t.Fatalf("cause = %+v, want a named origin", c)
		}
		byOrigin[*c.Origin] += c.Runs
		share += c.Share
	}
	if byOrigin["agent_failure"] != 1 || byOrigin[landable] != 1 || math.Abs(share-1) > 1e-5 {
		t.Fatalf("causes by origin %v share %v", byOrigin, share)
	}

	// Rolling: every point recomputed from the seeds over the response's own ranges.
	if len(dto.RollingFailureRate.Overall) != len(dto.Rolling) || len(dto.RollingFailureRate.Harnesses) != 2 ||
		dto.RollingFailureRate.Harnesses[0].Harness != "claude" || dto.RollingFailureRate.Harnesses[1].Harness != "codex" {
		t.Fatalf("rolling = %d overall, harness series %+v", len(dto.RollingFailureRate.Overall), dto.RollingFailureRate.Harnesses)
	}
	checkPoints := func(label, harness string, pts []apitypes.AnalyticsReliabilityPointDTO) {
		for i, p := range pts {
			var fin, failed int64
			for _, s := range seeds {
				if (harness != "" && s.harnessOrDefault() != harness) || s.created.Before(dto.Rolling[i].Start) || !s.created.Before(dto.Rolling[i].End) {
					continue
				}
				switch s.outcome() {
				case "":
				case "failed":
					fin++
					failed++
				default:
					fin++
				}
			}
			if p.Index != i || p.Finished != fin || p.Failed != failed {
				t.Fatalf("%s[%d] = %+v, want finished %d failed %d", label, i, p, fin, failed)
			}
			if fin < 5 {
				relWantNull(t, fmt.Sprintf("%s[%d]", label, i), p.Value, p.Reason, "insufficient_runs")
			} else {
				relNear(t, fmt.Sprintf("%s[%d]", label, i), p.Value, float64(failed)/float64(fin))
			}
		}
	}
	checkPoints("overall", "", dto.RollingFailureRate.Overall)
	for _, h := range dto.RollingFailureRate.Harnesses {
		checkPoints(h.Harness, h.Harness, h.Points)
	}

	// Envelope rolling flags: the first range starts before the scope's history (clipped, partial),
	// a middle range is fully covered, and the last range ends at generated_at (today's bucket).
	if n := len(dto.Rolling); n != 30 || !dto.Rolling[0].Partial || dto.Rolling[n/2].Partial || dto.Rolling[n-2].Partial || !dto.Rolling[n-1].Partial {
		t.Fatalf("rolling partial flags: first %v middle %v second-to-last %v last %v (of %d), want true false false true",
			dto.Rolling[0].Partial, dto.Rolling[len(dto.Rolling)/2].Partial, dto.Rolling[len(dto.Rolling)-2].Partial, dto.Rolling[len(dto.Rolling)-1].Partial, len(dto.Rolling))
	}

	// Breakdown (default primary_model): one row per Primary model, each run in exactly one.
	if dto.Breakdown.Dimension != "primary_model" {
		t.Fatalf("breakdown dimension = %q, want primary_model", dto.Breakdown.Dimension)
	}
	wantRows := map[string][]relSeed{}
	for _, s := range seeds {
		wantRows[s.primaryModel()] = append(wantRows[s.primaryModel()], s)
	}
	gotRows := map[string]apitypes.AnalyticsReliabilityRowDTO{}
	var rowFin int64
	for _, r := range dto.Breakdown.Rows {
		if r.Key == nil {
			t.Fatalf("unexpected keyless row %+v", r)
		}
		gotRows[*r.Key] = r
		rowFin += r.N
	}
	if len(gotRows) != len(wantRows) || rowFin != cur.Finished {
		t.Fatalf("breakdown rows %v (n sum %d), want models %v summing to %d", dto.Breakdown.Rows, rowFin, slices.Sorted(mapsKeys(wantRows)), cur.Finished)
	}
	for model, in := range wantRows {
		fin, out := relOutcomes(in)
		if r := gotRows[model]; r.N != fin || r.Outcomes != out {
			t.Fatalf("row %s = %+v, want n %d %+v", model, r, fin, out)
		}
	}

	// Latest failure: the newest non-plan_rejected, non-base_diverged failure, with the one
	// completed run after it.
	lf := dto.LatestFailure
	if lf == nil || lf.Origin != landable || lf.UserID != owner.String() || lf.CompletedSinceLastFailure != 1 {
		t.Fatalf("latest_failure = %+v, want origin %s user %s completed_since 1", lf, landable, owner)
	}
	if want := day(3).Add(time.Hour); !lf.FailedAt.Equal(want) {
		t.Fatalf("latest_failure.failed_at = %v, want %v", lf.FailedAt, want)
	}

	// needs_landing: the landable failure with a preserved patch, with its caveat.
	if dto.NeedsLanding.Runs != 1 || dto.NeedsLanding.Caveat != relCaveat {
		t.Fatalf("needs_landing = %+v, want 1 run and the documented caveat", dto.NeedsLanding)
	}

	// The other owner sees only their own three runs.
	theirs, _ := f.rel(other, "/api/analytics/reliability")
	if theirs.Totals.Current.Finished != 3 || theirs.Totals.Current.Outcomes.Failed != 2 {
		t.Fatalf("other owner totals = %+v, want 3 finished with 2 failed", theirs.Totals.Current)
	}
}

// Acceptance example 2: an admin sees instance totals and per-user groups; the owner route stays
// owner-only for the same admin. Both routes carry the same needs_landing caveat.
func TestAnalyticsReliabilityAdminAllUsersLiveDB(t *testing.T) {
	f := newAnFixture(t, 2042)
	admin, a, b := f.user(true), f.user(false), f.user(false)
	repoAdmin, _ := f.repoOwnedBy(admin, "adm")
	repoA, _ := f.repoOwnedBy(a, "a")
	repoB, _ := f.repoOwnedBy(b, "b")
	at := f.now.Add(-48 * time.Hour)
	seed := func(u, r uuid.UUID, status, origin string, hoursAgo int) {
		f.relRun(relSeed{user: u, repo: r, status: status, origin: origin, created: at.Add(-time.Duration(hoursAgo) * time.Hour)})
	}
	seed(admin, repoAdmin, "completed", "", 1)
	for i := 0; i < 3; i++ {
		seed(a, repoA, "completed", "", 2+i)
	}
	seed(a, repoA, "failed", "agent_failure", 6)
	seed(b, repoB, "completed", "", 7)
	seed(b, repoB, "completed", "", 8)
	seed(b, repoB, "failed", "agent_failure", 9)
	seed(b, repoB, "failed", "agent_failure", 10)

	all, _ := f.rel(admin, "/api/admin/analytics/reliability?user=all&breakdown=user")
	if all.Scope.Route != "admin" || all.Scope.User != "all" || all.SelectedUser != "all" {
		t.Fatalf("scope = %+v selected %q", all.Scope, all.SelectedUser)
	}
	if all.Totals.Current.Finished != 9 || all.Totals.Current.Outcomes.Failed != 3 || all.Totals.Current.Outcomes.Completed != 6 {
		t.Fatalf("instance totals = %+v, want 9 finished, 6 completed, 3 failed", all.Totals.Current)
	}
	if all.NeedsLanding.Caveat != relCaveat {
		t.Fatalf("admin needs_landing caveat = %q", all.NeedsLanding.Caveat)
	}
	rows := map[string]apitypes.AnalyticsReliabilityRowDTO{}
	for _, r := range all.Breakdown.Rows {
		if r.Key != nil {
			rows[*r.Key] = r
		}
	}
	if all.Breakdown.Dimension != "user" {
		t.Fatalf("breakdown dimension = %q", all.Breakdown.Dimension)
	}
	for u, want := range map[uuid.UUID]int64{admin: 1, a: 4, b: 4} {
		r, ok := rows[u.String()]
		if !ok || r.N != want || r.Label == nil || !strings.HasPrefix(*r.Label, "cli-") || r.ID == nil || *r.ID != u.String() {
			t.Fatalf("user row %s = %+v (present %v), want n %d labelled with the email", u, r, ok, want)
		}
	}
	if rows[b.String()].Outcomes.Failed != 2 || rows[a.String()].Outcomes.Failed != 1 {
		t.Fatalf("per-user failures: a %+v b %+v", rows[a.String()].Outcomes, rows[b.String()].Outcomes)
	}
	if lf := all.LatestFailure; lf == nil || lf.UserID != a.String() {
		t.Fatalf("latest failure = %+v, want user a's (the newest failure of the instance)", all.LatestFailure)
	}

	// An omitted user is byte-identical to user=all.
	omitted := f.cookie(admin, "/api/admin/analytics/reliability?breakdown=user")
	explicit := f.cookie(admin, "/api/admin/analytics/reliability?user=all&breakdown=user")
	if omitted.Code != http.StatusOK || omitted.Body.String() != explicit.Body.String() {
		t.Fatalf("omitted user differs from user=all (%d)", omitted.Code)
	}

	// One selected user, and the owner route for the same admin.
	if sel, _ := f.rel(admin, "/api/admin/analytics/reliability?user="+b.String()); sel.Totals.Current.Finished != 4 || sel.SelectedUser != b.String() || sel.NeedsLanding.Caveat != relCaveat {
		t.Fatalf("admin user=b = %+v selected %q", sel.Totals.Current, sel.SelectedUser)
	}
	own, _ := f.rel(admin, "/api/analytics/reliability")
	if own.Totals.Current.Finished != 1 || own.Scope.Route != "owner" || own.SelectedUser != admin.String() {
		t.Fatalf("admin on the owner route = %+v, want only the admin's own run", own.Totals.Current)
	}
}

// The repo and harness cohort filters reach both the cohort statement and the latest-failure
// statement: each filtered request has totals that differ from the unfiltered ones and equal
// counts worked out by hand from the seeds below, and its latest failure is the older matching
// failure, not the newest failure of the run set.
func TestAnalyticsReliabilityRepoAndHarnessFiltersLiveDB(t *testing.T) {
	f := newAnFixture(t, 2040)
	admin, u := f.user(true), f.user(false)
	r1, _ := f.repoOwnedBy(u, "flt1")
	var conn uuid.UUID
	if err := f.pool.QueryRow(context.Background(), `SELECT connection_id FROM repos WHERE id = $1`, r1).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	r2, _ := f.repoOn(conn, "flt2")
	seed := func(repo uuid.UUID, harness, status, origin string, hoursAgo int) uuid.UUID {
		return f.relRun(relSeed{user: u, repo: repo, harness: harness, status: status, origin: origin, created: f.now.Add(-time.Duration(hoursAgo) * time.Hour)})
	}
	seed(r1, "claude", "completed", "", 100)
	seed(r1, "claude", "completed", "", 90)
	seed(r1, "claude", "failed", "agent_failure", 80)
	seed(r2, "codex", "completed", "", 70)
	seed(r2, "codex", "failed", "run_timeout", 60)
	f50 := seed(r1, "codex", "failed", "agent_failure", 50) // newest failure on codex and in r1
	seed(r1, "claude", "completed", "", 40)                 // completed after f50, in r1 on claude
	g20 := seed(r2, "claude", "failed", "worker_lost", 20)  // newest failure overall, in r2 on claude

	type want struct {
		finished, completed, failed int64
		latest                      uuid.UUID
		since                       int64
	}
	check := func(label string, user uuid.UUID, path string, w want) {
		t.Helper()
		dto, _ := f.rel(user, path)
		c := dto.Totals.Current
		if c.Finished != w.finished || c.Outcomes.Completed != w.completed || c.Outcomes.Failed != w.failed {
			t.Errorf("%s: totals = %d finished %d completed %d failed, want %d %d %d", label, c.Finished, c.Outcomes.Completed, c.Outcomes.Failed, w.finished, w.completed, w.failed)
		}
		lf := dto.LatestFailure
		if lf == nil || lf.RunID != w.latest.String() || lf.CompletedSinceLastFailure != w.since {
			t.Errorf("%s: latest_failure = %+v, want run %s with %d completed since", label, lf, w.latest, w.since)
		}
	}
	for _, rt := range []struct {
		name, base string
		user       uuid.UUID
		sep        string
	}{
		{"owner", "/api/analytics/reliability", u, "?"},
		{"admin all", "/api/admin/analytics/reliability?user=all", admin, "&"},
	} {
		check(rt.name+" unfiltered", rt.user, rt.base, want{8, 4, 4, g20, 0})
		check(rt.name+" harness=codex", rt.user, rt.base+rt.sep+"harness=codex", want{3, 1, 2, f50, 0})
		check(rt.name+" repo=r1", rt.user, rt.base+rt.sep+"repo="+r1.String(), want{5, 3, 2, f50, 1})
		check(rt.name+" repo=r1 harness=codex", rt.user, rt.base+rt.sep+"repo="+r1.String()+"&harness=codex", want{1, 0, 1, f50, 0})
	}
}

// A failure created strictly after the request clock is outside every cohort count but is still
// the latest failure (the statement is unwindowed and takes no clock), whichever breakdown is asked.
func TestAnalyticsReliabilityFutureFailureIsLatestButNotCountedLiveDB(t *testing.T) {
	f := newAnFixture(t, 2050)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "future")
	f.relRun(relSeed{user: owner, repo: repo, status: "completed", created: f.now.Add(-48 * time.Hour)})
	past := f.relRun(relSeed{user: owner, repo: repo, status: "failed", origin: "agent_failure", created: f.now.Add(-24 * time.Hour)})
	future := f.relRun(relSeed{user: owner, repo: repo, status: "failed", origin: "worker_lost", created: f.now.Add(2 * time.Hour)})
	if past == future {
		t.Fatal("seed ids collide")
	}
	for _, path := range []string{
		"/api/analytics/reliability",
		"/api/analytics/reliability?breakdown=harness",
		"/api/analytics/reliability?breakdown=primary_model",
	} {
		dto, _ := f.rel(owner, path)
		c := dto.Totals.Current
		if c.Finished != 2 || c.Outcomes.Failed != 1 || c.Outcomes.Completed != 1 || dto.Causes.Failed != 1 {
			t.Errorf("%s: totals %+v causes %+v, want the future failure outside every count", path, c, dto.Causes)
		}
		lf := dto.LatestFailure
		if lf == nil || lf.RunID != future.String() || lf.Origin != "worker_lost" || lf.CompletedSinceLastFailure != 0 {
			t.Errorf("%s: latest_failure = %+v, want the future run %s", path, lf, future)
		}
	}
}

func mapsKeys[V any](m map[string][]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// Acceptance examples 3 and 4: a run with two usage models counts once, under its Primary
// model; a supporting-only filter says "not applicable" instead of reporting zero rates.
func TestAnalyticsReliabilityPrimaryModelAndSupportingOnlyLiveDB(t *testing.T) {
	f := newAnFixture(t, 2043)
	owner := f.user(false)
	repo, _ := f.repoOwnedBy(owner, "pm")
	at := f.now.Add(-5 * time.Hour)
	f.relRun(relSeed{user: owner, repo: repo, status: "failed", origin: "agent_failure", created: at,
		models: []relModel{{"m-small", 100}, {"m-big", 500}}})
	// A failed chat run: a supporting kind, in no figure and never counted as a failure.
	f.relRun(relSeed{user: owner, kind: runkind.Chat, status: "failed", origin: "agent_failure", created: at})

	dto, _ := f.rel(owner, "/api/analytics/reliability")
	if dto.Totals.Current.Finished != 1 || dto.Totals.Current.Outcomes.Failed != 1 {
		t.Fatalf("totals = %+v, want the one delivery run (the chat run is excluded)", dto.Totals.Current)
	}
	if len(dto.Breakdown.Rows) != 1 || dto.Breakdown.Rows[0].Key == nil || *dto.Breakdown.Rows[0].Key != "m-big" || dto.Breakdown.Rows[0].N != 1 {
		t.Fatalf("breakdown rows = %+v, want exactly one row, m-big, n=1", dto.Breakdown.Rows)
	}

	// Per-kind filters keep their meaning: chat alone is supporting-only.
	for _, kind := range []string{"supporting", "chat"} {
		rec := f.cookie(owner, "/api/analytics/reliability?kind="+kind)
		got, raw := decodeReliability(t, rec)
		if got.Applicable || got.Reason == nil || *got.Reason != "supporting_kinds_only" || string(raw["latest_failure"]) != "null" {
			t.Fatalf("kind=%s: applicable %v reason %v latest %s", kind, got.Applicable, got.Reason, raw["latest_failure"])
		}
		c := got.Totals.Current
		if c.Finished != 0 || c.Outcomes != (apitypes.AnalyticsOutcomesDTO{}) {
			t.Fatalf("kind=%s counts = %+v, want zero despite the failed chat run", kind, c)
		}
		relWantNull(t, "kind="+kind+" failure_rate", c.FailureRate.Value, c.FailureRate.Reason, "not_applicable")
		if got.NeedsLanding.Caveat != relCaveat {
			t.Fatalf("kind=%s caveat = %q", kind, got.NeedsLanding.Caveat)
		}
	}
	// kind=delivery is the same as no filter, and kind=issue narrows to the issue run.
	if d, _ := f.rel(owner, "/api/analytics/reliability?kind=delivery"); d.Totals.Current.Finished != 1 || !d.Applicable {
		t.Fatalf("kind=delivery totals = %+v", d.Totals.Current)
	}
	if d, _ := f.rel(owner, "/api/analytics/reliability?kind=job"); d.Totals.Current.Finished != 0 || !d.Applicable {
		t.Fatalf("kind=job = applicable %v totals %+v, want an applicable empty population", d.Applicable, d.Totals.Current)
	}
}

// The scope with no failure: latest_failure is null, and an empty applicable population is a
// null rate with no_finished_runs rather than zero. The latest failure is lifetime, not windowed.
func TestAnalyticsReliabilityEmptyAndLifetimeLatestFailureLiveDB(t *testing.T) {
	f := newAnFixture(t, 2047)
	empty, owner := f.user(false), f.user(false)
	repo, _ := f.repoOwnedBy(owner, "life")

	rec := f.cookie(empty, "/api/analytics/reliability")
	dto, raw := decodeReliability(t, rec)
	if !dto.Applicable || string(raw["latest_failure"]) != "null" || string(raw["reason"]) != "null" {
		t.Fatalf("empty scope: applicable %v latest %s reason %s", dto.Applicable, raw["latest_failure"], raw["reason"])
	}
	relWantNull(t, "empty failure_rate", dto.Totals.Current.FailureRate.Value, dto.Totals.Current.FailureRate.Reason, "no_finished_runs")
	if len(dto.Series) != 30 || len(dto.RollingFailureRate.Harnesses) != 2 || dto.NeedsLanding.Runs != 0 || dto.NeedsLanding.Caveat != relCaveat {
		t.Fatalf("empty scope shape: %d series, %d harness series, needs_landing %+v", len(dto.Series), len(dto.RollingFailureRate.Harnesses), dto.NeedsLanding)
	}
	for _, p := range dto.RollingFailureRate.Overall {
		relWantNull(t, "empty rolling point", p.Value, p.Reason, "insufficient_runs")
	}
	if dto.Series == nil || dto.Causes.Items == nil || dto.Breakdown.Rows == nil || dto.RollingFailureRate.Overall == nil || dto.RollingFailureRate.Harnesses == nil || string(raw["series"]) == "null" {
		t.Fatal("an empty scope serialized a panel slice as null, want []")
	}

	old := f.now.Add(-120 * 24 * time.Hour)
	failed := f.relRun(relSeed{user: owner, repo: repo, status: "failed", origin: "agent_failure", created: old})
	f.relRun(relSeed{user: owner, repo: repo, status: "completed", created: f.now.Add(-24 * time.Hour)})
	got, _ := f.rel(owner, "/api/analytics/reliability")
	if got.Totals.Current.Finished != 1 || got.Causes.Failed != 0 {
		t.Fatalf("30d window = %+v causes %+v, want only the recent completed run", got.Totals.Current, got.Causes)
	}
	lf := got.LatestFailure
	if lf == nil || lf.RunID != failed.String() || lf.Origin != "agent_failure" || lf.CompletedSinceLastFailure != 1 || !lf.FailedAt.Equal(old.Add(time.Hour)) {
		t.Fatalf("latest_failure = %+v, want the 120-day-old failure (lifetime, not windowed) with 1 completed since", lf)
	}
}

// A caller-owned run on a repo the caller has no connection to is masked on the owner route
// (as the user and as an admin) and revealed on the admin route; the repo filter answers the
// same 400 for an inaccessible repo as for an unknown one.
func TestAnalyticsReliabilityRepoMaskingLiveDB(t *testing.T) {
	f := newAnFixture(t, 2044)
	admin, u, foreignOwner := f.user(true), f.user(false), f.user(false)
	repoAdmin, _ := f.repoOwnedBy(admin, "admin")
	repoU, pathU := f.repoOwnedBy(u, "u")
	repoF, pathF := f.repoOwnedBy(foreignOwner, "foreign")
	at := f.now.Add(-2 * time.Hour)
	f.relRun(relSeed{user: admin, repo: repoAdmin, created: at})
	f.relRun(relSeed{user: admin, repo: repoF, status: "failed", origin: "agent_failure", created: at}) // no connection to repoF
	f.relRun(relSeed{user: u, repo: repoU, created: at})
	f.relRun(relSeed{user: u, repo: repoF, status: "failed", origin: "agent_failure", created: at})

	for name, user := range map[string]uuid.UUID{"user": u, "admin": admin} {
		t.Run("owner route as "+name, func(t *testing.T) {
			dto, rec := f.rel(user, "/api/analytics/reliability?breakdown=repo")
			if dto.Totals.Current.Finished != 2 || dto.Totals.Current.Outcomes.Failed != 1 {
				t.Fatalf("totals = %+v", dto.Totals.Current)
			}
			var masked *apitypes.AnalyticsReliabilityRowDTO
			for i, r := range dto.Breakdown.Rows {
				if r.Key != nil && *r.Key == "inaccessible_repo" {
					masked = &dto.Breakdown.Rows[i]
				}
			}
			if masked == nil || masked.ID != nil || masked.Label != nil || masked.N != 1 || masked.Outcomes.Failed != 1 {
				t.Fatalf("rows = %+v, want an inaccessible_repo row with null identifiers", dto.Breakdown.Rows)
			}
			if body := rec.Body.String(); strings.Contains(body, repoF.String()) || strings.Contains(body, pathF) {
				t.Fatalf("owner-route body leaks the repo: %s", body)
			}
		})
	}

	revealed, _ := f.rel(admin, "/api/admin/analytics/reliability?user="+u.String()+"&breakdown=repo")
	got := map[string]apitypes.AnalyticsReliabilityRowDTO{}
	for _, r := range revealed.Breakdown.Rows {
		if r.Key != nil {
			got[*r.Key] = r
		}
	}
	if r, ok := got[repoF.String()]; !ok || r.Label == nil || *r.Label != pathF || r.ID == nil || *r.ID != repoF.String() {
		t.Fatalf("admin route must reveal the repo: rows %+v", revealed.Breakdown.Rows)
	}
	if r, ok := got[repoU.String()]; !ok || r.Label == nil || *r.Label != pathU {
		t.Fatalf("admin route must reveal the owned repo: rows %+v", revealed.Breakdown.Rows)
	}

	// Inaccessible and unknown repo filters are the same 400, byte for byte.
	foreign := f.cookie(u, "/api/analytics/reliability?repo="+repoF.String())
	unknown := f.cookie(u, "/api/analytics/reliability?repo="+uuid.NewString())
	if foreign.Code != http.StatusBadRequest || unknown.Code != http.StatusBadRequest || foreign.Body.String() != unknown.Body.String() {
		t.Fatalf("inaccessible repo = %d %q, unknown repo = %d %q, want the identical 400", foreign.Code, foreign.Body.String(), unknown.Code, unknown.Body.String())
	}
	asym := f.cookie(admin, "/api/admin/analytics/reliability?user="+u.String()+"&repo="+repoF.String())
	if asym.Code != http.StatusBadRequest || asym.Body.String() != unknown.Body.String() {
		t.Fatalf("admin user=u&repo=<not u's> = %d %s, want the unknown-repo 400", asym.Code, asym.Body.String())
	}
	// A supporting-only filter still runs the authority check, so it cannot probe a repo either.
	probe := f.cookie(u, "/api/analytics/reliability?kind=supporting&repo="+repoF.String())
	if probe.Code != http.StatusBadRequest || probe.Body.String() != unknown.Body.String() {
		t.Fatalf("supporting-only with an inaccessible repo = %d %s, want the identical 400", probe.Code, probe.Body.String())
	}
}

// relCommit runs a competing commit once, on another pool connection (the request's snapshot
// holds its own, so the pool.Exec of cliMustExec cannot be the snapshot's connection).
type relCommit struct {
	once sync.Once
	do   func()
}

func (c *relCommit) hook() func(context.Context) {
	return func(context.Context) { c.once.Do(c.do) }
}

// Snapshot isolation: a write committed after the first read, or after the cohort read, is
// invisible to the in-flight response and visible to the next request.
func TestAnalyticsReliabilitySnapshotIsolationLiveDB(t *testing.T) {
	seedBase := func(f *anFixture) (owner, repo uuid.UUID, failedRun uuid.UUID) {
		owner = f.user(false)
		repo, _ = f.repoOwnedBy(owner, "snap")
		for i := 0; i < 3; i++ {
			f.relRun(relSeed{user: owner, repo: repo, created: f.now.Add(-time.Duration(10+i) * time.Hour)})
		}
		failedRun = f.relRun(relSeed{user: owner, repo: repo, status: "failed", origin: "agent_failure", created: f.now.Add(-20 * time.Hour)})
		return owner, repo, failedRun
	}
	newFailure := func(f *anFixture, owner, repo uuid.UUID) uuid.UUID {
		// Created before now and inside the 30-day window, and newer than every other failure.
		return relInsert(f.t, f.pool, relSeed{user: owner, repo: repo, status: "failed", origin: "history_rewritten", created: f.now.Add(-2 * time.Hour)})
	}

	t.Run("after the first read", func(t *testing.T) {
		f := newAnFixture(t, 2045)
		owner, repo, _ := seedBase(f)
		before, _ := f.rel(owner, "/api/analytics/reliability")
		if before.Totals.Current.Finished != 4 || before.Totals.Current.Outcomes.Failed != 1 {
			t.Fatalf("baseline totals = %+v", before.Totals.Current)
		}
		c := &relCommit{do: func() { newFailure(f, owner, repo) }}
		f.h.analyticsDeps.afterFirstRead = c.hook()
		inflight, _ := f.rel(owner, "/api/analytics/reliability")
		f.h.analyticsDeps.afterFirstRead = nil

		if !reflect.DeepEqual(inflight.Totals.Current, before.Totals.Current) {
			t.Fatalf("in-flight totals = %+v, want the old %+v", inflight.Totals.Current, before.Totals.Current)
		}
		last := len(inflight.Series) - 1
		if inflight.Series[last] != before.Series[last] || inflight.Series[last].Outcomes.Failed != 0 {
			t.Fatalf("in-flight last bucket = %+v, want the old one (3 completed, no failure)", inflight.Series[last])
		}
		if inflight.Causes.Failed != 1 || len(inflight.Causes.Items) != 1 || *inflight.Causes.Items[0].Origin != "agent_failure" {
			t.Fatalf("in-flight causes = %+v, want only the old origin", inflight.Causes)
		}
		after, _ := f.rel(owner, "/api/analytics/reliability")
		if after.Totals.Current.Finished != 5 || after.Totals.Current.Outcomes.Failed != 2 || after.Causes.Failed != 2 || after.Series[last].Outcomes.Failed != 1 {
			t.Fatalf("follow-up = totals %+v last bucket %+v causes %+v, want the new failure", after.Totals.Current, after.Series[last], after.Causes)
		}
	})

	t.Run("after the cohort read", func(t *testing.T) {
		f := newAnFixture(t, 2046)
		owner, repo, oldFailure := seedBase(f)
		var newID uuid.UUID
		c := &relCommit{do: func() { newID = newFailure(f, owner, repo) }}
		f.h.analyticsDeps.afterCohortRead = c.hook()
		inflight, _ := f.rel(owner, "/api/analytics/reliability")
		f.h.analyticsDeps.afterCohortRead = nil

		if newID == uuid.Nil {
			t.Fatal("the after-cohort hook did not fire")
		}
		if lf := inflight.LatestFailure; lf == nil || lf.RunID != oldFailure.String() {
			t.Fatalf("in-flight latest_failure = %+v, want the old failure %s (the commit came after the snapshot)", inflight.LatestFailure, oldFailure)
		}
		if inflight.Totals.Current.Finished != 4 {
			t.Fatalf("in-flight totals = %+v, want the old population", inflight.Totals.Current)
		}
		after, _ := f.rel(owner, "/api/analytics/reliability")
		if lf := after.LatestFailure; lf == nil || lf.RunID != newID.String() || lf.Origin != "history_rewritten" {
			t.Fatalf("follow-up latest_failure = %+v, want the new failure %s", after.LatestFailure, newID)
		}
	})
}

// countingPool wraps the real pool's BeginTx and counts the statements of every transaction.
type countingPool struct {
	pool   *pgxpool.Pool
	begins atomic.Int32
	mu     sync.Mutex
	names  map[string]int
	kinds  map[string][][]string
}

func (p *countingPool) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	p.begins.Add(1)
	tx, err := p.pool.BeginTx(ctx, o)
	if err != nil {
		return nil, err
	}
	return &countingTx{Tx: tx, p: p}, nil
}

type countingTx struct {
	pgx.Tx
	p *countingPool
}

// note records the sqlc statement name (the "-- name: X :one" header every generated constant
// starts with) and any []string argument, which for these statements is the kinds filter.
func (t *countingTx) note(sql string, args []any) {
	name := "?"
	if f := strings.Fields(strings.TrimPrefix(sql, "-- name:")); strings.HasPrefix(sql, "-- name:") && len(f) > 0 {
		name = f[0]
	}
	t.p.mu.Lock()
	defer t.p.mu.Unlock()
	t.p.names[name]++
	for _, a := range args {
		if ks, ok := a.([]string); ok {
			t.p.kinds[name] = append(t.p.kinds[name], slices.Clone(ks))
		}
	}
}

func (t *countingTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.note(sql, args)
	return t.Tx.Query(ctx, sql, args...)
}

func (t *countingTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.note(sql, args)
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t *countingTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.Tx.Exec(ctx, sql, args...)
}

// One request is one transaction, one cohort statement and one latest-failure statement, and
// every statement that takes a kinds filter is handed the Delivery kinds only.
func TestAnalyticsReliabilityStatementCountsLiveDB(t *testing.T) {
	f := newAnFixture(t, 2048)
	admin, owner := f.user(true), f.user(false)
	repo, _ := f.repoOwnedBy(owner, "count")
	f.relRun(relSeed{user: owner, repo: repo, status: "failed", origin: "agent_failure", created: f.now.Add(-3 * time.Hour)})
	f.relRun(relSeed{user: owner, kind: runkind.Chat, status: "failed", origin: "agent_failure", created: f.now.Add(-3 * time.Hour)})
	delivery := runkind.Delivery()
	slices.Sort(delivery)

	for _, c := range []struct {
		name, path string
		user       uuid.UUID
		cohort     string
		latest     string
		history    string
	}{
		{"owner", "/api/analytics/reliability", owner, "AnalyticsOwnerReliability", "AnalyticsOwnerLatestFailure", "AnalyticsOwnerHistoryStart"},
		{"admin user=<uuid>", "/api/admin/analytics/reliability?user=" + owner.String(), admin, "AnalyticsOwnerReliability", "AnalyticsOwnerLatestFailure", "AnalyticsOwnerHistoryStart"},
		{"admin all users", "/api/admin/analytics/reliability", admin, "AnalyticsAllReliability", "AnalyticsAllLatestFailure", "AnalyticsAllHistoryStart"},
	} {
		t.Run(c.name, func(t *testing.T) {
			cp := &countingPool{pool: f.pool, names: map[string]int{}, kinds: map[string][][]string{}}
			f.h.analyticsDeps.begin = cp
			defer func() { f.h.analyticsDeps.begin = f.pool }()

			dto, _ := f.rel(c.user, c.path)
			if dto.Totals.Current.Finished == 0 {
				t.Fatalf("totals = %+v, want the seeded failure", dto.Totals.Current)
			}
			if got := cp.begins.Load(); got != 1 {
				t.Fatalf("BeginTx = %d, want 1 (one snapshot per request)", got)
			}
			cp.mu.Lock()
			defer cp.mu.Unlock()
			if cp.names[c.cohort] != 1 || cp.names[c.latest] != 1 || cp.names[c.history] != 1 {
				t.Fatalf("statement counts = %v, want exactly one each of %s, %s, %s", cp.names, c.cohort, c.latest, c.history)
			}
			for name, n := range cp.names {
				if (strings.Contains(name, "Reliability") || strings.Contains(name, "LatestFailure") || strings.Contains(name, "HistoryStart")) &&
					name != c.cohort && name != c.latest && name != c.history {
					t.Fatalf("unexpected statement %s x%d (counts %v)", name, n, cp.names)
				}
			}
			for _, name := range []string{c.cohort, c.latest, c.history} {
				// The statements also take string lists that are not kinds (harnesses, origins);
				// exactly one list must be the Delivery set and none may hold a supporting kind.
				var hits int
				for _, list := range cp.kinds[name] {
					if slices.Equal(slices.Sorted(slices.Values(list)), delivery) {
						hits++
					}
					for _, k := range runkind.Supporting() {
						if slices.Contains(list, k) {
							t.Fatalf("%s was handed the supporting kind %q: %v", name, k, list)
						}
					}
				}
				if hits != 1 {
					t.Fatalf("%s string-list arguments = %v, want exactly one equal to the Delivery kinds", name, cp.kinds[name])
				}
			}
		})
	}

	// A supporting-only request begins one transaction and runs none of the three statements.
	cp := &countingPool{pool: f.pool, names: map[string]int{}, kinds: map[string][][]string{}}
	f.h.analyticsDeps.begin = cp
	defer func() { f.h.analyticsDeps.begin = f.pool }()
	if dto, _ := f.rel(owner, "/api/analytics/reliability?kind=chat"); dto.Applicable {
		t.Fatal("kind=chat must not be applicable")
	}
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for name := range cp.names {
		if strings.Contains(name, "Reliability") || strings.Contains(name, "LatestFailure") || strings.Contains(name, "HistoryStart") {
			t.Fatalf("supporting-only request ran %s", name)
		}
	}
}

// Credentials through the real router: the admin route takes an admin cookie and an admin_ro
// bearer, refuses an ordinary bearer owned by an admin and any member; the owner route takes all.
func TestAnalyticsReliabilityCredentialsLiveDB(t *testing.T) {
	f := newAnFixture(t, 2049)
	admin, member := f.user(true), f.user(false)
	adminRO := cliMintToken(t, f.pool, admin, clitoken.ScopeAdminRO)
	adminDefault := cliMintToken(t, f.pool, admin, clitoken.ScopeUser)
	memberToken := cliMintToken(t, f.pool, member, clitoken.ScopeUser)
	adminJWT, memberJWT := cliMintJWT(t, f.pool, admin), cliMintJWT(t, f.pool, member)

	const adminPath, ownerPath = "/api/admin/analytics/reliability", "/api/analytics/reliability"
	if rec := bearerReq(f.router, http.MethodGet, adminPath, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("admin route no credential = %d, want 401", rec.Code)
	}
	for name, want := range map[string]int{"admin cookie": http.StatusOK, "admin_ro bearer": http.StatusOK, "admin default-scope bearer": http.StatusForbidden, "member cookie": http.StatusForbidden, "member bearer": http.StatusForbidden} {
		var rec *httptest.ResponseRecorder
		switch name {
		case "admin cookie":
			rec = cookieReq(t, f.router, http.MethodGet, adminPath, adminJWT, "")
		case "admin_ro bearer":
			rec = bearerReq(f.router, http.MethodGet, adminPath, adminRO)
		case "admin default-scope bearer":
			rec = bearerReq(f.router, http.MethodGet, adminPath, adminDefault)
		case "member cookie":
			rec = cookieReq(t, f.router, http.MethodGet, adminPath, memberJWT, "")
		default:
			rec = bearerReq(f.router, http.MethodGet, adminPath, memberToken)
		}
		if rec.Code != want {
			t.Fatalf("%s on the admin route = %d %s, want %d", name, rec.Code, rec.Body.String(), want)
		}
	}
	if rec := bearerReq(f.router, http.MethodGet, ownerPath, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("owner route no credential = %d, want 401", rec.Code)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"admin cookie":    cookieReq(t, f.router, http.MethodGet, ownerPath, adminJWT, ""),
		"member cookie":   cookieReq(t, f.router, http.MethodGet, ownerPath, memberJWT, ""),
		"member bearer":   bearerReq(f.router, http.MethodGet, ownerPath, memberToken),
		"admin default":   bearerReq(f.router, http.MethodGet, ownerPath, adminDefault),
		"admin_ro bearer": bearerReq(f.router, http.MethodGet, ownerPath, adminRO),
	} {
		if rec.Code != http.StatusOK {
			t.Fatalf("%s on the owner route = %d %s, want 200", name, rec.Code, rec.Body.String())
		}
	}
}
