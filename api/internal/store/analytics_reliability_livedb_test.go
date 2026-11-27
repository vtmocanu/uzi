package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4685: data tests for AnalyticsOwnerReliability, AnalyticsAllReliability and the two
// latest-failure statements. Like the Spend tests they seed through SQL, build parameters with
// the real calendar code and skip without UZI_TEST_DATABASE_URL. Unless a test says otherwise:
// now = spendNow (2026-11-25T12:00Z), zone UTC, window 30d (see analytics_spend_livedb_test.go).

// TestAnalyticsReliabilityParityWithSelfRunOutcomesLiveDB pins the Delivery-only population
// against SelfRunOutcomes (finished, classes, failure origins, needs_landing, latest failure,
// completed since) over a window that covers every run, then pins each intentional difference.
func TestAnalyticsReliabilityParityWithSelfRunOutcomesLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o, other := f.owner("rel-par"), f.owner("rel-par-other")
	repo, _ := f.repo(o, "rel-par")
	set := f.outcomeSet(o, repo, other)
	// The other owner's runs must stay invisible to o in every figure.
	otherRepo, _ := f.repo(other, "rel-par")
	f.run(fin(other.User, otherRepo, "failed", "agent_failure", spendDay(25, 9)))

	res := f.rel(relCall{Owner: o.User, Now: spendNow})
	d := res.Doc
	self, err := f.q.SelfRunOutcomes(f.ctx, store.SelfRunOutcomesParams{UserID: o.User, LandableOrigins: workersvc.AllHumanLandableFailOrigins()})
	if err != nil {
		t.Fatalf("SelfRunOutcomes: %v", err)
	}

	// Literal expectations first (independent of the other statement), then equality.
	wantBlock(t, "current", d.Totals.Current, 18, 4, 2, 1, 1, 10, 0)
	wantRate(t, "failure_rate", d.Totals.Current.FailureRate, 10.0/18.0, "")
	if d.NeedsLanding != 2 {
		t.Errorf("needs_landing = %d, want 2 (own available capture, preserved patch; every negative excluded)", d.NeedsLanding)
	}
	if self.LifetimeFinished != 18 || self.LifetimeCompleted != 4 || self.LifetimeCancelled != 2 ||
		self.LifetimePlanRejected != 1 || self.LifetimeBaseDiverged != 1 || self.LifetimeFailed != 10 || self.LifetimeNeedsLanding != 2 {
		t.Fatalf("SelfRunOutcomes disagrees with the seed's literal expectation: %+v", self)
	}
	b := d.Totals.Current
	if b.Finished != self.LifetimeFinished || b.Outcomes.Completed != self.LifetimeCompleted || b.Outcomes.Cancelled != self.LifetimeCancelled ||
		b.Outcomes.PlanRejected != self.LifetimePlanRejected || b.Outcomes.BaseDiverged != self.LifetimeBaseDiverged ||
		b.Outcomes.Failed != self.LifetimeFailed || d.NeedsLanding != self.LifetimeNeedsLanding {
		t.Errorf("Reliability %+v / needs_landing %d != SelfRunOutcomes lifetime %+v", b, d.NeedsLanding, self)
	}
	// Failure origins: causes (top five + Other) re-add to the existing per-origin map.
	origins := decodeOutcomeOrigins(t, "self lifetime", self.LifetimeFailOrigins)
	if d.Causes.Failed != self.LifetimeFailed {
		t.Errorf("causes.failed = %d, want %d", d.Causes.Failed, self.LifetimeFailed)
	}
	var causeSum, otherRuns int64
	for _, c := range d.Causes.Items {
		causeSum += c.Runs
		if c.IsOther {
			otherRuns = c.Runs
			continue
		}
		if c.Origin == nil || origins[*c.Origin] != c.Runs {
			t.Errorf("cause %+v does not match the existing origin map %v", c, origins)
		}
	}
	if causeSum != d.Causes.Failed {
		t.Errorf("cause runs sum %d != failed %d", causeSum, d.Causes.Failed)
	}
	// 7 origins: agent_failure 3, workflow_scope_missing 2, then five singletons ordered by origin
	// ("C" collation): finalize_base_align_conflict, history_rewritten, push_secret_blocked take
	// the remaining top-five slots; unknown and worker_lost fold into Other.
	if otherRuns != 2 || len(d.Causes.Items) != 6 {
		t.Errorf("Other runs = %d over %d items, want 2 over 6: %+v", otherRuns, len(d.Causes.Items), d.Causes.Items)
	}

	// Latest failure: same row, same count.
	lf := f.latest(relCall{Owner: o.User})
	if !lf.Found || lf.RunID != set.latestFailure || lf.RunID != self.LastFailedRunID || !lf.FailedAt.Equal(self.LastFailedAt.Time) ||
		lf.Origin != self.LastFailedOrigin || lf.Origin != "finalize_base_align_conflict" ||
		lf.Since != self.CompletedSinceLastFailure || lf.Since != 1 {
		t.Errorf("latest failure %+v vs self (run %s at %v origin %s since %d); want run %s since 1",
			lf, self.LastFailedRunID, self.LastFailedAt.Time, self.LastFailedOrigin, self.CompletedSinceLastFailure, set.latestFailure)
	}

	// Intentional differences: judge, cross_check and chat are excluded here and (chat apart)
	// counted by SelfRunOutcomes.
	f.run(fin(o.User, repo, "completed", "", spendDay(24, 9)))
	f.seedSupporting(o, repo, set.lastCompleted)
	self2, err := f.q.SelfRunOutcomes(f.ctx, store.SelfRunOutcomesParams{UserID: o.User, LandableOrigins: workersvc.AllHumanLandableFailOrigins()})
	if err != nil {
		t.Fatalf("SelfRunOutcomes: %v", err)
	}
	if self2.LifetimeFinished != 18+1+2 || self2.LifetimeFailed != 12 {
		t.Errorf("SelfRunOutcomes after supporting runs: finished %d failed %d, want 21 and 12", self2.LifetimeFinished, self2.LifetimeFailed)
	}
	d2 := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc
	wantBlock(t, "after supporting runs", d2.Totals.Current, 19, 5, 2, 1, 1, 10, 0)
	if d2.NeedsLanding != 2 {
		t.Errorf("needs_landing after supporting runs = %d", d2.NeedsLanding)
	}
	// The failed judge/cross_check runs are the newest failures, yet never the latest failure.
	lf2 := f.latest(relCall{Owner: o.User})
	if lf2.RunID != set.latestFailure {
		t.Errorf("latest failure moved to %s after supporting failures, want %s", lf2.RunID, set.latestFailure)
	}
	if self2.LastFailedRunID == set.latestFailure {
		t.Error("SelfRunOutcomes should now report a supporting run as its latest failure (the difference this test pins)")
	}
	// Asking for the supporting kinds explicitly is the caller's choice and works (the statement
	// is kind-parametric); the cross_check failure is the newest one.
	lf3 := f.latest(relCall{Owner: o.User, Kinds: []string{runkind.Judge, runkind.CrossCheck}})
	if !lf3.Found || lf3.RunID != self2.LastFailedRunID {
		t.Errorf("supporting-kinds latest failure = %+v, want run %s", lf3, self2.LastFailedRunID)
	}
}

// TestAnalyticsReliabilityAllScopeParityWithAdminRunOutcomesLiveDB pins the all-users statement
// against AdminRunOutcomes (lifetime figures) over the Delivery-only parity population. The admin
// statement is factory-wide and takes no filter, so its figures are the delta between the calls
// before and after seeding; the Reliability call is isolated by the seeded repo filter.
func TestAnalyticsReliabilityAllScopeParityWithAdminRunOutcomesLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o, other := f.owner("rel-adm"), f.owner("rel-adm-other")
	repo, _ := f.repo(o, "rel-adm")
	landable := workersvc.AllHumanLandableFailOrigins()
	before, err := f.q.AdminRunOutcomes(f.ctx, landable)
	if err != nil {
		t.Fatalf("AdminRunOutcomes before: %v", err)
	}
	f.outcomeSet(o, repo, other)
	after, err := f.q.AdminRunOutcomes(f.ctx, landable)
	if err != nil {
		t.Fatalf("AdminRunOutcomes after: %v", err)
	}
	delta := func(a, b int64) int64 { return a - b }
	adm := struct{ finished, completed, cancelled, planRejected, baseDiverged, failed, needs int64 }{
		delta(after.LifetimeFinished, before.LifetimeFinished), delta(after.LifetimeCompleted, before.LifetimeCompleted),
		delta(after.LifetimeCancelled, before.LifetimeCancelled), delta(after.LifetimePlanRejected, before.LifetimePlanRejected),
		delta(after.LifetimeBaseDiverged, before.LifetimeBaseDiverged), delta(after.LifetimeFailed, before.LifetimeFailed),
		delta(after.LifetimeNeedsLanding, before.LifetimeNeedsLanding),
	}
	if adm.finished != 18 || adm.completed != 4 || adm.cancelled != 2 || adm.planRejected != 1 || adm.baseDiverged != 1 || adm.failed != 10 || adm.needs != 2 {
		t.Fatalf("AdminRunOutcomes delta disagrees with the seed's literal expectation: %+v", adm)
	}
	ob, oa := decodeOutcomeOrigins(t, "admin before", before.LifetimeFailOrigins), decodeOutcomeOrigins(t, "admin after", after.LifetimeFailOrigins)
	origins := map[string]int64{}
	for k, v := range oa {
		if d := v - ob[k]; d != 0 {
			origins[k] = d
		}
	}

	d := f.rel(relCall{Now: spendNow, Repo: repo}).Doc
	b := d.Totals.Current
	if b.Finished != adm.finished || b.Outcomes.Completed != adm.completed || b.Outcomes.Cancelled != adm.cancelled ||
		b.Outcomes.PlanRejected != adm.planRejected || b.Outcomes.BaseDiverged != adm.baseDiverged || b.Outcomes.Failed != adm.failed {
		t.Errorf("all-scope Reliability %+v != AdminRunOutcomes delta %+v", b, adm)
	}
	if d.NeedsLanding != adm.needs {
		t.Errorf("all-scope needs_landing = %d, AdminRunOutcomes delta %d", d.NeedsLanding, adm.needs)
	}
	if d.Causes.Failed != adm.failed {
		t.Errorf("causes.failed = %d, want %d", d.Causes.Failed, adm.failed)
	}
	var shown, otherRuns, originTotal int64
	for _, n := range origins {
		originTotal += n
	}
	for _, c := range d.Causes.Items {
		if c.IsOther {
			otherRuns = c.Runs
			continue
		}
		shown += c.Runs
		if c.Origin == nil || origins[*c.Origin] != c.Runs {
			t.Errorf("cause %+v does not match the admin origin delta %v", c, origins)
		}
	}
	if shown+otherRuns != originTotal || originTotal != adm.failed {
		t.Errorf("causes shown %d + Other %d != admin origin total %d (failed %d)", shown, otherRuns, originTotal, adm.failed)
	}
}

// TestAnalyticsReliabilityNullOriginAndInFlightLiveDB: a NULL fail_origin groups as unknown in
// causes; an in-flight run is in no finished figure; plan_rejected and base_diverged are
// finished but neither failed nor a cause nor the latest failure.
func TestAnalyticsReliabilityNullOriginAndInFlightLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-null")
	repo, _ := f.repo(o, "rel-null")
	f.run(fin(o.User, repo, "failed", "", spendDay(5, 9)))
	f.run(fin(o.User, repo, "failed", "plan_rejected", spendDay(6, 9)))
	f.run(fin(o.User, repo, "failed", "base_diverged", spendDay(7, 9)))
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", At: spendDay(8, 9)})
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "queued", At: spendDay(9, 9)})

	d := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc
	wantBlock(t, "totals", d.Totals.Current, 3, 0, 0, 1, 1, 1, 0)
	if d.Causes.Failed != 1 || len(d.Causes.Items) != 1 || d.Causes.Items[0].Origin == nil || *d.Causes.Items[0].Origin != "unknown" ||
		d.Causes.Items[0].Runs != 1 || d.Causes.Items[0].Share != 1 || d.Causes.Items[0].IsOther {
		t.Errorf("causes = %+v, want one unknown cause with share 1", d.Causes)
	}
	lf := f.latest(relCall{Owner: o.User})
	if !lf.Found || lf.Origin != "unknown" {
		t.Errorf("latest failure = %+v, want the NULL-origin failure as unknown, not the later plan_rejected/base_diverged runs", lf)
	}
}

// TestAnalyticsReliabilityBoundedWindowLiveDB uses independently derived expectations: a run
// created before the window and finished inside it is not in the current window, the half-open
// edges hold, in-flight runs are not finished, and the previous window is its own cohort.
func TestAnalyticsReliabilityBoundedWindowLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-win")
	repo, _ := f.repo(o, "rel-win")
	from := utcAt(2026, 10, 27, 0, 0, 0)
	prevFrom := utcAt(2026, 9, 27, 0, 0, 0)

	// Created in the previous window, finished inside the current one: previous cohort only.
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "completed", At: utcAt(2026, 10, 20, 9, 0, 0), Finished: utcAt(2026, 10, 28, 9, 0, 0)})
	// Created before every fold bound, finished inside the window: in no figure at all.
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "failed", FailOrigin: "agent_failure", At: utcAt(2026, 6, 1, 9, 0, 0), Finished: utcAt(2026, 11, 2, 9, 0, 0)})
	// Created exactly at from: current. Created exactly at now: outside (half-open).
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "completed", At: from, Finished: from.Add(time.Hour)})
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "failed", FailOrigin: "agent_failure", At: spendNow, Finished: spendNow})
	// Current window: day 3 failed, day 9 two completed + one cancelled, an in-flight run.
	f.run(fin(o.User, repo, "failed", "run_timeout", spendDay(3, 9)))
	f.run(fin(o.User, repo, "completed", "", spendDay(9, 9)))
	f.run(fin(o.User, repo, "completed", "", spendDay(9, 10)))
	f.run(fin(o.User, repo, "cancelled", "", spendDay(9, 11)))
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", At: spendDay(9, 12)})
	// Previous window: one failed, one completed.
	f.run(fin(o.User, repo, "failed", "agent_failure", utcAt(2026, 10, 1, 9, 0, 0)))
	f.run(fin(o.User, repo, "completed", "", utcAt(2026, 10, 2, 9, 0, 0)))

	res := f.rel(relCall{Owner: o.User, Now: spendNow})
	d := res.Doc
	if !res.Bounds.From.Equal(from) || res.Bounds.PreviousFrom == nil || !res.Bounds.PreviousFrom.Equal(prevFrom) {
		t.Fatalf("bounds from %s previous %v, want %s and %s", res.Bounds.From, res.Bounds.PreviousFrom, from, prevFrom)
	}
	wantBlock(t, "current", d.Totals.Current, 5, 3, 1, 0, 0, 1, 0)
	wantRate(t, "current rate", d.Totals.Current.FailureRate, 0.2, "")
	if d.Totals.Previous == nil {
		t.Fatal("bounded window has no previous totals")
	}
	wantBlock(t, "previous", *d.Totals.Previous, 3, 2, 0, 0, 0, 1, 0)
	wantRate(t, "previous rate", d.Totals.Previous.FailureRate, 1.0/3.0, "")
	if d.GeneratedAt != "2026-11-25T12:00:00.000000Z" {
		t.Errorf("generated_at = %q", d.GeneratedAt)
	}

	// Buckets: one cell per bucket, bucket sums re-add to the current totals.
	if len(d.Buckets) != len(res.Bounds.Buckets) || len(d.Buckets) != 30 {
		t.Fatalf("buckets = %d, want 30 (one per bucket)", len(d.Buckets))
	}
	var sum int64
	for i, c := range d.Buckets {
		if c.Index != i {
			t.Fatalf("bucket %d has index %d", i, c.Index)
		}
		sum += c.Finished
	}
	if sum != d.Totals.Current.Finished {
		t.Errorf("bucket finished sum %d != totals %d", sum, d.Totals.Current.Finished)
	}
	day := func(k int) store.AnalyticsReliabilityBucket { return d.Buckets[k-1] }
	if c := day(1); c.Finished != 1 || c.Outcomes.Completed != 1 {
		t.Errorf("day 1 (created at from) = %+v", c)
	}
	if c := day(3); c.Finished != 1 || c.Outcomes.Failed != 1 {
		t.Errorf("day 3 = %+v", c)
	}
	if c := day(9); c.Finished != 3 || c.Outcomes.Completed != 2 || c.Outcomes.Cancelled != 1 || c.Outcomes.Failed != 0 {
		t.Errorf("day 9 = %+v (the in-flight run is not finished)", c)
	}
	if c := day(30); c.Finished != 0 {
		t.Errorf("last bucket = %+v: the run created exactly at now must be outside", c)
	}
}

// TestAnalyticsReliabilityCausesTopFivePlusOtherLiveDB: five ranked causes then Other, ties
// broken by origin in the "C" collation, runs summing to failed, shares re-adding to one.
func TestAnalyticsReliabilityCausesTopFivePlusOtherLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-cause")
	repo, _ := f.repo(o, "rel-cause")
	n := 0
	add := func(origin string, count int) {
		for i := 0; i < count; i++ {
			n++
			f.run(fin(o.User, repo, "failed", origin, spendDay(2+n%25, 1+n%20)))
		}
	}
	add("agent_failure", 4)
	add("worker_lost", 3)
	add("run_timeout", 3) // ties worker_lost; "run_timeout" < "worker_lost" under "C"
	add("rate_limited", 2)
	add("provisioning_failed", 1)
	add("credential_unavailable", 1)
	add("guardrail_blocked", 1)
	add("", 1) // NULL origin: unknown
	// Not causes: plan_rejected and base_diverged are their own classes.
	add("plan_rejected", 2)
	add("base_diverged", 1)

	d := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc
	if d.Causes.Failed != 16 {
		t.Fatalf("causes.failed = %d, want 16 (4+3+3+2+1+1+1+1)", d.Causes.Failed)
	}
	wantBlock(t, "totals", d.Totals.Current, 19, 0, 0, 2, 1, 16, 0)
	want := []struct {
		origin string
		runs   int64
		other  bool
		share  float64
	}{
		{"agent_failure", 4, false, 0.25},
		{"run_timeout", 3, false, 0.1875},
		{"worker_lost", 3, false, 0.1875},
		{"rate_limited", 2, false, 0.125},
		{"credential_unavailable", 1, false, 0.0625},
		{"", 3, true, 0.1875}, // guardrail_blocked, provisioning_failed, unknown
	}
	if len(d.Causes.Items) != len(want) {
		t.Fatalf("items = %+v, want %d", d.Causes.Items, len(want))
	}
	var runs int64
	var share float64
	for i, w := range want {
		g := d.Causes.Items[i]
		runs += g.Runs
		share += g.Share
		if g.Rank != i+1 || g.IsOther != w.other || g.Runs != w.runs || g.Share != w.share {
			t.Errorf("item %d = %+v, want %+v", i, g, w)
		}
		if w.other != (g.Origin == nil) || (!w.other && *g.Origin != w.origin) {
			t.Errorf("item %d origin = %v, want %q (nil for Other)", i, g.Origin, w.origin)
		}
	}
	if runs != d.Causes.Failed || share < 0.999999 || share > 1.000001 {
		t.Errorf("causes sum to %d runs, share %v; want %d and 1", runs, share, d.Causes.Failed)
	}
}

// TestAnalyticsReliabilityPrimaryModelCountsRunOnceLiveDB: a run that used two models is one
// finished run under its Primary model; with another breakdown the model is not consulted.
func TestAnalyticsReliabilityPrimaryModelCountsRunOnceLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-pm")
	repo, _ := f.repo(o, "rel-pm")
	two := f.run(fin(o.User, repo, "completed", "", spendDay(5, 9)))
	f.usage(two, "model-small", "metered", "0.100000", 10, 0, 0, 100)
	f.usage(two, "model-big", "metered", "0.200000", 10, 0, 0, 300)
	tie := f.run(fin(o.User, repo, "failed", "agent_failure", spendDay(6, 9)))
	f.usage(tie, "model-z", "metered", "0.100000", 1, 0, 0, 50)
	f.usage(tie, "model-a", "metered", "0.100000", 1, 0, 0, 50) // equal output: "C"-first model wins
	f.run(fin(o.User, repo, "completed", "", spendDay(7, 9)))   // no usage: unknown

	d := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc
	wantBlock(t, "totals", d.Totals.Current, 3, 2, 0, 0, 0, 1, 0)
	if d.Breakdown.Dimension != "primary_model" || len(d.Breakdown.Rows) != 3 {
		t.Fatalf("breakdown = %+v", d.Breakdown)
	}
	var n int64
	for _, r := range d.Breakdown.Rows {
		n += r.N
		if r.N != 1 || r.Rank < 1 || r.Rank > 3 {
			t.Errorf("row %+v: every model holds exactly one finished run", r)
		}
	}
	if n != 3 {
		t.Errorf("rows count %d runs, want 3 (each run once)", n)
	}
	if r := relRowByKey(t, d.Breakdown.Rows, "model-big"); r.Outcomes.Completed != 1 {
		t.Errorf("model-big = %+v", r)
	}
	if r := relRowByKey(t, d.Breakdown.Rows, "model-a"); r.Outcomes.Failed != 1 {
		t.Errorf("model-a = %+v: the tie goes to the first model in \"C\" order", r)
	}
	relRowByKey(t, d.Breakdown.Rows, "unknown")
	// Rank order: equal finished counts tie-break on key in "C" collation.
	wantKeys := []string{"model-a", "model-big", "unknown"}
	for i, k := range wantKeys {
		if d.Breakdown.Rows[i].Key == nil || *d.Breakdown.Rows[i].Key != k {
			t.Errorf("row %d key = %v, want %s", i, d.Breakdown.Rows[i].Key, k)
		}
	}

	// breakdown=harness: the usage fold is not read, runs are grouped by harness, once each.
	dh := f.rel(relCall{Owner: o.User, Now: spendNow, Breakdown: "harness"}).Doc
	if len(dh.Breakdown.Rows) != 1 || dh.Breakdown.Rows[0].N != 3 || *dh.Breakdown.Rows[0].Key != "claude" {
		t.Errorf("harness breakdown = %+v", dh.Breakdown.Rows)
	}
	wantBlock(t, "totals under harness breakdown", dh.Totals.Current, 3, 2, 0, 0, 0, 1, 0)
}

// TestAnalyticsReliabilityGrainGuardLiveDB: usage rows fan out (several models and legs per
// run) but the statement is one row per run, so totals equal a plain count of finished runs.
func TestAnalyticsReliabilityGrainGuardLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-grain")
	repo, _ := f.repo(o, "rel-grain")
	for i := 0; i < 6; i++ {
		id := f.run(fin(o.User, repo, "completed", "", spendDay(3+i, 9)))
		for m := 0; m < 3; m++ {
			f.metered(id, fmt.Sprintf("model-%d", m), "0.010000")
			f.metered(id, fmt.Sprintf("model-%d", m), "0.010000")
		}
	}
	f.run(fin(o.User, repo, "failed", "agent_failure", spendDay(12, 9)))
	var plain int64
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM runs WHERE user_id = $1 AND status IN ('completed','failed','cancelled') AND kind = ANY($2)`,
		o.User, runkind.Delivery()).Scan(&plain); err != nil {
		t.Fatal(err)
	}
	for _, bd := range []string{"primary_model", "harness", "kind"} {
		d := f.rel(relCall{Owner: o.User, Now: spendNow, Breakdown: bd}).Doc
		if d.Totals.Current.Finished != plain || plain != 7 {
			t.Errorf("breakdown %s: totals finished %d, plain count %d, want 7", bd, d.Totals.Current.Finished, plain)
		}
		var n int64
		for _, r := range d.Breakdown.Rows {
			n += r.N
		}
		if n != plain {
			t.Errorf("breakdown %s: rows hold %d finished runs, want %d", bd, n, plain)
		}
	}
}

// TestAnalyticsReliabilityRequeuedRunsLiveDB: requeued_runs counts cohort runs whose retained
// requeue_count is above zero, finished or not, per window.
func TestAnalyticsReliabilityRequeuedRunsLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-req")
	repo, _ := f.repo(o, "rel-req")
	a := f.run(fin(o.User, repo, "completed", "", spendDay(4, 9)))
	f.requeue(a, 3)
	b := f.run(fin(o.User, repo, "failed", "worker_lost", spendDay(5, 9)))
	f.requeue(b, 1)
	f.run(fin(o.User, repo, "completed", "", spendDay(6, 9))) // never requeued
	c := f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", At: spendDay(7, 9)})
	f.requeue(c, 2) // in flight, still a requeued run
	p := f.run(fin(o.User, repo, "completed", "", utcAt(2026, 10, 3, 9, 0, 0)))
	f.requeue(p, 1) // previous window
	// Supporting kinds are outside the statement's cohort even when requeued.
	j := f.run(spendRun{Owner: o.User, Kind: runkind.Judge, Target: a, Status: "completed", At: spendDay(8, 9), Finished: spendDay(8, 10)})
	f.requeue(j, 5)

	d := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc
	wantBlock(t, "current", d.Totals.Current, 3, 2, 0, 0, 0, 1, 3)
	wantBlock(t, "previous", *d.Totals.Previous, 1, 1, 0, 0, 0, 0, 1)
}

// TestAnalyticsReliabilityRollingLiveDB pins the seven-day rolling series: one point per bucket
// per series, real counts under suppression, per-harness isolation and the rate at five runs.
func TestAnalyticsReliabilityRollingLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-roll")
	repo, _ := f.repo(o, "rel-roll")
	cl := func(status, origin string, at time.Time) {
		r := fin(o.User, repo, status, origin, at)
		r.Harness = "claude"
		f.run(r)
	}
	cx := func(status, origin string, at time.Time) {
		r := fin(o.User, repo, status, origin, at)
		r.Harness = "codex"
		f.run(r)
	}
	// claude on Nov 20: two finished, one failed: a non-empty but suppressed point.
	cl("completed", "", utcAt(2026, 11, 20, 9, 0, 0))
	cl("failed", "agent_failure", utcAt(2026, 11, 20, 10, 0, 0))
	// codex on Nov 10: five finished, one failed: a computed point.
	for i := 0; i < 4; i++ {
		cx("completed", "", utcAt(2026, 11, 10, 8+i, 0, 0))
	}
	cx("failed", "run_timeout", utcAt(2026, 11, 10, 14, 0, 0))
	// An in-flight run is in no point.
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "running", Harness: "codex", At: utcAt(2026, 11, 10, 15, 0, 0)})

	res := f.rel(relCall{Owner: o.User, Now: spendNow})
	d := res.Doc
	n := len(res.Bounds.Buckets)
	if len(d.Rolling.Overall) != n || len(d.Rolling.Harnesses) != 2 || d.Rolling.Harnesses[0].Harness != "claude" || d.Rolling.Harnesses[1].Harness != "codex" {
		t.Fatalf("rolling shape: overall %d, harnesses %+v, buckets %d", len(d.Rolling.Overall), d.Rolling.Harnesses, n)
	}
	for _, series := range [][]store.AnalyticsReliabilityPoint{d.Rolling.Overall, relSeries(t, d, "claude"), relSeries(t, d, "codex")} {
		if len(series) != n {
			t.Fatalf("series has %d points, want one per bucket (%d)", len(series), n)
		}
		for i, p := range series {
			if p.Index != i {
				t.Fatalf("point %d has index %d", i, p.Index)
			}
		}
	}
	at := func(day time.Time) int { return relBucketIdx(t, res.Bounds, day) }
	nov := func(day int) int { return at(utcAt(2026, 11, day, 0, 0, 0)) }

	// Nov 21's range [Nov 15, Nov 22) holds the claude pair: suppressed, counts kept.
	p := relSeries(t, d, "claude")[nov(21)]
	if p.Finished != 2 || p.Failed != 1 || p.Value != nil || p.Reason == nil || *p.Reason != store.AnalyticsReasonInsufficientRuns {
		t.Errorf("claude Nov 21 = %+v, want 2 finished 1 failed, null with insufficient_runs", p)
	}
	// Same range, other harness and the overall series: codex has nothing there.
	if p := relSeries(t, d, "codex")[nov(21)]; p.Finished != 0 || p.Failed != 0 || p.Value != nil || p.Reason == nil || *p.Reason != store.AnalyticsReasonInsufficientRuns {
		t.Errorf("codex Nov 21 = %+v, want an empty suppressed point", p)
	}
	if p := d.Rolling.Overall[nov(21)]; p.Finished != 2 || p.Failed != 1 || p.Value != nil {
		t.Errorf("overall Nov 21 = %+v", p)
	}
	// Nov 12's range [Nov 6, Nov 13) holds the five codex runs: a real rate, claude is empty.
	pc := relSeries(t, d, "codex")[nov(12)]
	if pc.Finished != 5 || pc.Failed != 1 || pc.Value == nil || *pc.Value != 0.2 || pc.Reason != nil {
		t.Errorf("codex Nov 12 = %+v, want 5 finished 1 failed rate 0.2", pc)
	}
	if p := relSeries(t, d, "claude")[nov(12)]; p.Finished != 0 || p.Value != nil {
		t.Errorf("claude Nov 12 = %+v: harness isolation", p)
	}
	if p := d.Rolling.Overall[nov(12)]; p.Finished != 5 || p.Value == nil || *p.Value != 0.2 {
		t.Errorf("overall Nov 12 = %+v", p)
	}
	// A bucket whose range has aged out of the codex runs (Nov 17's range [Nov 11, Nov 18)).
	if p := relSeries(t, d, "codex")[nov(17)]; p.Finished != 0 {
		t.Errorf("codex Nov 17 = %+v, want the Nov 10 runs aged out", p)
	}
	// The first bucket's range is empty: 0/0 with a reason, never a zero rate.
	if p := d.Rolling.Overall[0]; p.Finished != 0 || p.Failed != 0 || p.Value != nil || p.Reason == nil {
		t.Errorf("first overall point = %+v, want empty and suppressed", p)
	}
}

// TestAnalyticsReliabilityWideBucketRollingLiveDB: in a multi-week bucket (window=all) the
// bucket cell counts the whole bucket but the rolling point only its final seven days.
func TestAnalyticsReliabilityWideBucketRollingLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-wide")
	repo, _ := f.repo(o, "rel-wide")
	now := utcAt(2026, 11, 25, 12, 0, 0)
	// The earliest run fixes history_start at 2023-01-02, which makes multi-week buckets.
	f.run(fin(o.User, repo, "completed", "", utcAt(2023, 1, 2, 9, 0, 0)))
	// Bucket labelled 2025-02-24 ends after 2025-03-09 (see the Spend rolling fixtures); the
	// early run is in the bucket but outside its final seven days, the late run is inside both.
	f.run(fin(o.User, repo, "failed", "agent_failure", utcAt(2025, 2, 25, 9, 0, 0)))
	f.run(fin(o.User, repo, "completed", "", utcAt(2025, 3, 9, 12, 0, 0)))

	res := f.rel(relCall{Owner: o.User, Now: now, Window: analytics.WindowAll})
	if res.Bounds.WidthWeeks < 2 {
		t.Fatalf("width weeks = %d, the fixture needs a multi-week bucket", res.Bounds.WidthWeeks)
	}
	if res.Doc.Totals.Previous != nil {
		t.Error("window=all has previous totals")
	}
	idx := -1
	for i, bk := range res.Bounds.Buckets {
		if bk.Label == (analytics.Date{Year: 2025, Month: 2, Day: 24}) {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatal("no bucket labelled 2025-02-24")
	}
	cell := res.Doc.Buckets[idx]
	if cell.Finished != 2 || cell.Outcomes.Failed != 1 || cell.Outcomes.Completed != 1 {
		t.Errorf("bucket cell = %+v, want both runs of the bucket", cell)
	}
	pt := res.Doc.Rolling.Overall[idx]
	if pt.Finished != 1 || pt.Failed != 0 || pt.Value != nil || pt.Index != idx {
		t.Errorf("rolling point = %+v, want only the run in the final seven days", pt)
	}
	if len(res.Doc.Rolling.Overall) != len(res.Bounds.Buckets) {
		t.Errorf("rolling points %d, buckets %d", len(res.Doc.Rolling.Overall), len(res.Bounds.Buckets))
	}
}

// TestAnalyticsReliabilityBreakdownLiveDB: suppression keeps counts, ranking is by finished
// runs then key, and the 51st slot is Other.
func TestAnalyticsReliabilityBreakdownLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-bd")
	repo, _ := f.repo(o, "rel-bd")
	// Six finished runs in model-big (one failed), two in model-small (suppressed), then 50
	// single-run models: 52 groups, so ranks 1..50 plus Other holding the last two.
	for i := 0; i < 6; i++ {
		status, origin := "completed", ""
		if i == 0 {
			status, origin = "failed", "agent_failure"
		}
		id := f.run(fin(o.User, repo, status, origin, spendDay(3+i, 9)))
		f.metered(id, "model-big", "0.010000")
	}
	for i := 0; i < 2; i++ {
		id := f.run(fin(o.User, repo, "failed", "agent_failure", spendDay(10+i, 9)))
		f.metered(id, "model-small", "0.010000")
	}
	for i := 0; i < 50; i++ {
		id := f.run(fin(o.User, repo, "completed", "", spendDay(1+i%28, 2+i%5)))
		f.metered(id, fmt.Sprintf("solo-%02d", i), "0.010000")
	}
	d := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc
	rows := d.Breakdown.Rows
	if len(rows) != 51 {
		t.Fatalf("rows = %d, want 50 + Other", len(rows))
	}
	big, small := rows[0], rows[1]
	if big.Key == nil || *big.Key != "model-big" || big.N != 6 || big.Outcomes.Failed != 1 || big.Outcomes.Completed != 5 {
		t.Errorf("rank 1 = %+v", big)
	}
	wantRate(t, "model-big rate", big.FailureRate, 1.0/6.0, "")
	if small.Key == nil || *small.Key != "model-small" || small.N != 2 || small.Outcomes.Failed != 2 {
		t.Errorf("rank 2 = %+v", small)
	}
	wantRate(t, "model-small rate", small.FailureRate, 0, store.AnalyticsReasonInsufficientRuns)
	if *rows[2].Key != "solo-00" || *rows[49].Key != "solo-47" {
		t.Errorf("single-run rows ordered by key: rank 3 %s rank 50 %s", *rows[2].Key, *rows[49].Key)
	}
	other := rows[50]
	if other.Rank != 51 || !other.IsOther || other.Key != nil || other.Label != nil || other.ID != nil || other.N != 2 {
		t.Errorf("Other = %+v, want rank 51, no key and the last two single-run models", other)
	}
	var n int64
	for _, r := range rows {
		n += r.N
	}
	if n != d.Totals.Current.Finished || n != 58 {
		t.Errorf("rows hold %d runs, totals %d, want 58", n, d.Totals.Current.Finished)
	}
}

// TestAnalyticsReliabilityAllScopeUserBreakdownLiveDB: the all-scope statement groups by user
// with the email label and id, totals span owners, and the owner statement sees only its own.
func TestAnalyticsReliabilityAllScopeUserBreakdownLiveDB(t *testing.T) {
	f := newRelFixture(t)
	f.claimYear(2006)
	now := utcAt(2006, 6, 15, 12, 0, 0)
	a, b := f.owner("rel-us-a"), f.owner("rel-us-b")
	ra, _ := f.repo(a, "rel-us")
	rb, _ := f.repo(b, "rel-us")
	at := func(d int) time.Time { return utcAt(2006, 6, d, 9, 0, 0) }
	f.run(fin(a.User, ra, "completed", "", at(2)))
	f.run(fin(a.User, ra, "failed", "agent_failure", at(3)))
	for i := 0; i < 3; i++ {
		f.run(fin(b.User, rb, "completed", "", at(4+i)))
	}
	f.run(fin(b.User, rb, "failed", "worker_lost", at(8)))
	f.run(fin(b.User, rb, "failed", "plan_rejected", at(9)))
	// A judge run never reaches the Delivery cohort.
	judgeTarget := f.run(fin(b.User, rb, "completed", "", at(1)))
	f.run(spendRun{Owner: b.User, Kind: runkind.Judge, Target: judgeTarget, Status: "failed", FailOrigin: "agent_failure", At: at(10), Finished: at(10)})

	res := f.rel(relCall{Now: now, Breakdown: "user"})
	d := res.Doc
	wantBlock(t, "instance totals", d.Totals.Current, 8, 5, 0, 1, 0, 2, 0)
	rows := d.Breakdown.Rows
	if len(rows) != 2 || rows[0].Key == nil || *rows[0].Key != b.User.String() {
		t.Fatalf("rows = %+v, want b (6 finished) first", rows)
	}
	if rows[0].Label == nil || *rows[0].Label != "spend-rel-us-b-"+b.User.String()+"@e2e" || rows[0].ID == nil || *rows[0].ID != b.User || rows[0].N != 6 {
		t.Errorf("user b row = %+v", rows[0])
	}
	if rows[1].N != 2 || rows[1].Outcomes.Failed != 1 || rows[1].FailureRate.Reason == nil {
		t.Errorf("user a row = %+v", rows[1])
	}
	// The owner statement over the same year sees only its own runs.
	owner := f.rel(relCall{Owner: a.User, Now: now, Breakdown: "user"}).Doc
	wantBlock(t, "owner a totals", owner.Totals.Current, 2, 1, 0, 0, 0, 1, 0)
	if len(owner.Breakdown.Rows) != 1 || *owner.Breakdown.Rows[0].Key != a.User.String() {
		t.Errorf("owner breakdown = %+v", owner.Breakdown.Rows)
	}
}

// TestAnalyticsReliabilityRepoMaskingLiveDB: an owner's run on a repo outside the owner's
// connections is masked unless the caller reveals repos; the all-scope statement always reveals.
func TestAnalyticsReliabilityRepoMaskingLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o, p := f.owner("rel-mask"), f.owner("rel-mask-p")
	own, ownPath := f.repo(o, "rel-mask")
	foreign, foreignPath := f.repo(p, "rel-mask")
	f.run(fin(o.User, own, "completed", "", spendDay(3, 9)))
	f.run(fin(o.User, foreign, "failed", "agent_failure", spendDay(4, 9)))
	f.run(spendRun{Owner: o.User, Kind: runkind.Job, Status: "completed", At: spendDay(5, 9), Finished: spendDay(5, 10)})

	d := f.rel(relCall{Owner: o.User, Now: spendNow, Breakdown: "repo"}).Doc
	if len(d.Breakdown.Rows) != 3 {
		t.Fatalf("rows = %+v", d.Breakdown.Rows)
	}
	r := relRowByKey(t, d.Breakdown.Rows, own.String())
	if r.Label == nil || *r.Label != ownPath || r.ID == nil || *r.ID != own {
		t.Errorf("own repo row = %+v", r)
	}
	r = relRowByKey(t, d.Breakdown.Rows, "inaccessible_repo")
	if r.Label != nil || r.ID != nil || r.Outcomes.Failed != 1 {
		t.Errorf("masked row = %+v: no label, no id", r)
	}
	relRowByKey(t, d.Breakdown.Rows, "no_repo")
	revealed := f.rel(relCall{Owner: o.User, Reveal: true, Now: spendNow, Breakdown: "repo"}).Doc
	r = relRowByKey(t, revealed.Breakdown.Rows, foreign.String())
	if r.Label == nil || *r.Label != foreignPath || r.ID == nil || *r.ID != foreign {
		t.Errorf("revealed row = %+v", r)
	}
}

// TestAnalyticsReliabilityAllScopeRepoRevealLiveDB: the all-scope statement shows every repo with
// its real key, id and path, including one the owner statement masks.
func TestAnalyticsReliabilityAllScopeRepoRevealLiveDB(t *testing.T) {
	f := newRelFixture(t)
	f.claimYear(2007)
	now := utcAt(2007, 6, 15, 12, 0, 0)
	o, p := f.owner("rel-allrepo"), f.owner("rel-allrepo-p")
	own, ownPath := f.repo(o, "rel-allrepo")
	foreign, foreignPath := f.repo(p, "rel-allrepo")
	f.run(fin(o.User, own, "completed", "", utcAt(2007, 6, 3, 9, 0, 0)))
	f.run(fin(o.User, foreign, "failed", "agent_failure", utcAt(2007, 6, 4, 9, 0, 0)))

	rows := f.rel(relCall{Now: now, Breakdown: "repo"}).Doc.Breakdown.Rows
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two real repos", rows)
	}
	for key, path := range map[uuid.UUID]string{own: ownPath, foreign: foreignPath} {
		r := relRowByKey(t, rows, key.String())
		if r.Label == nil || *r.Label != path || r.ID == nil || *r.ID != key {
			t.Errorf("all-scope row for %s = %+v, want path %q and its id", key, r, path)
		}
	}
	for _, r := range rows {
		if r.Key != nil && *r.Key == "inaccessible_repo" {
			t.Errorf("all-scope statement masked a repo: %+v", r)
		}
	}
}

// TestAnalyticsReliabilityFiltersLiveDB: repo, harness and kinds restrict the cohort, and the
// totals, buckets and needs_landing all follow it.
func TestAnalyticsReliabilityFiltersLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("rel-flt")
	r1, _ := f.repo(o, "rel-flt")
	r2, _ := f.repo(o, "rel-flt")
	mk := func(repo uuid.UUID, kind, harness, status, origin string, k int) uuid.UUID {
		r := fin(o.User, repo, status, origin, spendDay(k, 9))
		r.Kind, r.Harness = kind, harness
		return f.run(r)
	}
	mk(r1, runkind.Issue, "claude", "completed", "", 3)
	mk(r1, runkind.Issue, "codex", "failed", "agent_failure", 4)
	n := mk(r2, runkind.Task, "claude", "failed", "workflow_scope_missing", 5)
	f.preservePatch(n)
	mk(r2, runkind.Issue, "claude", "completed", "", 6)

	if d := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc; d.Totals.Current.Finished != 4 || d.NeedsLanding != 1 {
		t.Errorf("unfiltered finished %d needs_landing %d, want 4 and 1", d.Totals.Current.Finished, d.NeedsLanding)
	}
	if d := f.rel(relCall{Owner: o.User, Now: spendNow, Repo: r1}).Doc; d.Totals.Current.Finished != 2 || d.NeedsLanding != 0 {
		t.Errorf("repo filter finished %d needs_landing %d, want 2 and 0", d.Totals.Current.Finished, d.NeedsLanding)
	}
	if d := f.rel(relCall{Owner: o.User, Now: spendNow, Harness: "codex"}).Doc; d.Totals.Current.Finished != 1 || d.Totals.Current.Outcomes.Failed != 1 {
		t.Errorf("harness filter totals = %+v", d.Totals.Current)
	}
	if d := f.rel(relCall{Owner: o.User, Now: spendNow, Kinds: []string{runkind.Task}}).Doc; d.Totals.Current.Finished != 1 || d.NeedsLanding != 1 {
		t.Errorf("kinds filter finished %d needs_landing %d, want 1 and 1", d.Totals.Current.Finished, d.NeedsLanding)
	}
	// Supporting kinds only: the statement is kind-parametric; an empty kind list is an empty cohort.
	if d := f.rel(relCall{Owner: o.User, Now: spendNow, Kinds: []string{}}).Doc; d.Totals.Current.Finished != 0 || d.Totals.Current.FailureRate.Reason == nil {
		t.Errorf("empty kinds totals = %+v", d.Totals.Current)
	}
}

// TestAnalyticsLatestFailureStrictAfterLiveDB: completed runs ending exactly at the failure's
// terminal timestamp do not count; one microsecond later does; COALESCE(finished_at,
// status_since) orders runs with no finished_at.
func TestAnalyticsLatestFailureStrictAfterLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o := f.owner("lf-strict")
	repo, _ := f.repo(o, "lf-strict")
	T := utcAt(2026, 11, 10, 10, 0, 0)
	failed := f.run(spendRun{Owner: o.User, Repo: repo, Status: "failed", FailOrigin: "worker_lost", At: T.Add(-time.Hour), Finished: T})
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "completed", At: T.Add(-2 * time.Hour), Finished: T})                       // exactly T: no
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "completed", At: T.Add(-2 * time.Hour), Finished: T.Add(-time.Hour)})       // before: no
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "completed", At: T.Add(-2 * time.Hour), Finished: T.Add(time.Microsecond)}) // T+1us: yes
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "cancelled", At: T.Add(-2 * time.Hour), Finished: T.Add(time.Hour)})        // not completed: no
	lf := f.latest(relCall{Owner: o.User})
	if !lf.Found || lf.RunID != failed || !lf.FailedAt.Equal(T) || lf.Origin != "worker_lost" || lf.UserID != o.User || lf.Since != 1 {
		t.Errorf("latest failure = %+v, want run %s at %s origin worker_lost since 1", lf, failed, T)
	}
	// A completed run with no finished_at is ordered by status_since.
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "completed", At: T.Add(-time.Hour), StatusAt: T.Add(2 * time.Hour)})
	if lf := f.latest(relCall{Owner: o.User}); lf.Since != 2 {
		t.Errorf("since = %d after a completed run with only status_since after the failure, want 2", lf.Since)
	}
	// A newer failure moves the latest failure and restarts the count.
	f.run(spendRun{Owner: o.User, Repo: repo, Status: "failed", FailOrigin: "", At: T, Finished: T.Add(3 * time.Hour)})
	if lf := f.latest(relCall{Owner: o.User}); lf.Origin != "unknown" || lf.Since != 0 || !lf.FailedAt.Equal(T.Add(3*time.Hour)) {
		t.Errorf("after a newer NULL-origin failure: %+v, want unknown and since 0", lf)
	}
	// A run with the same terminal instant ties on id: the larger id is the latest.
	same := T.Add(3 * time.Hour)
	other := f.run(spendRun{Owner: o.User, Repo: repo, Status: "failed", FailOrigin: "run_timeout", At: T, Finished: same})
	var wantID uuid.UUID
	if err := f.pool.QueryRow(f.ctx, `SELECT id FROM runs WHERE user_id = $1 AND status = 'failed' AND finished_at = $2 ORDER BY id DESC LIMIT 1`, o.User, same).Scan(&wantID); err != nil {
		t.Fatal(err)
	}
	if lf := f.latest(relCall{Owner: o.User}); lf.RunID != wantID {
		t.Errorf("tie on terminal instant: got %s, want the larger id %s (other seeded %s)", lf.RunID, wantID, other)
	}
}

// TestAnalyticsLatestFailureUnwindowedAndFilteredLiveDB: no created_at cutoff, plan_rejected,
// base_diverged, chat and (by default) supporting kinds never qualify, repo/harness/kinds
// filters apply, another owner's failure is invisible, and no failure is not found.
func TestAnalyticsLatestFailureUnwindowedAndFilteredLiveDB(t *testing.T) {
	f := newRelFixture(t)
	o, other, none := f.owner("lf-flt"), f.owner("lf-flt-o"), f.owner("lf-flt-none")
	r1, _ := f.repo(o, "lf-flt")
	r2, _ := f.repo(o, "lf-flt")
	or, _ := f.repo(other, "lf-flt")
	mk := func(repo uuid.UUID, kind, harness, origin string, at time.Time) uuid.UUID {
		r := spendRun{Owner: o.User, Repo: repo, Kind: kind, Harness: harness, Status: "failed", FailOrigin: origin, At: at, Finished: at.Add(time.Hour)}
		return f.run(r)
	}
	// Created two years before the cohort's now and long before any window: still returned.
	old := mk(r1, runkind.Issue, "claude", "agent_failure", utcAt(2024, 3, 1, 9, 0, 0))
	// Newer failures that must not qualify.
	mk(r1, runkind.Issue, "claude", "plan_rejected", utcAt(2024, 4, 1, 9, 0, 0))
	mk(r1, runkind.Issue, "claude", "base_diverged", utcAt(2024, 4, 2, 9, 0, 0))
	f.run(spendRun{Owner: o.User, Kind: runkind.Chat, Status: "failed", FailOrigin: "agent_failure", At: utcAt(2024, 4, 3, 9, 0, 0), Finished: utcAt(2024, 4, 3, 10, 0, 0)})
	f.run(spendRun{Owner: o.User, Kind: runkind.Judge, Target: old, Status: "failed", FailOrigin: "agent_failure", At: utcAt(2024, 4, 4, 9, 0, 0), Finished: utcAt(2024, 4, 4, 10, 0, 0)})
	f.run(spendRun{Owner: other.User, Repo: or, Status: "failed", FailOrigin: "agent_failure", At: utcAt(2024, 5, 1, 9, 0, 0), Finished: utcAt(2024, 5, 1, 10, 0, 0)})
	// An earlier completed run does not count; two later ones do.
	f.run(spendRun{Owner: o.User, Repo: r1, Status: "completed", At: utcAt(2024, 2, 1, 9, 0, 0), Finished: utcAt(2024, 2, 1, 10, 0, 0)})
	f.run(spendRun{Owner: o.User, Repo: r1, Status: "completed", At: utcAt(2024, 6, 1, 9, 0, 0), Finished: utcAt(2024, 6, 1, 10, 0, 0)})
	f.run(spendRun{Owner: o.User, Repo: r2, Status: "completed", Harness: "codex", At: utcAt(2024, 7, 1, 9, 0, 0), Finished: utcAt(2024, 7, 1, 10, 0, 0)})
	// Another owner's completions after the failure never count toward this owner's total.
	f.run(spendRun{Owner: other.User, Repo: or, Status: "completed", At: utcAt(2024, 8, 1, 9, 0, 0), Finished: utcAt(2024, 8, 1, 10, 0, 0)})
	f.run(spendRun{Owner: other.User, Repo: or, Status: "completed", At: utcAt(2024, 9, 1, 9, 0, 0), Finished: utcAt(2024, 9, 1, 10, 0, 0)})
	// The cohort at spendNow does not contain the run, the latest failure still does.
	if d := f.rel(relCall{Owner: o.User, Now: spendNow}).Doc; d.Totals.Current.Finished != 0 {
		t.Errorf("window cohort finished = %d, want 0", d.Totals.Current.Finished)
	}
	lf := f.latest(relCall{Owner: o.User})
	if !lf.Found || lf.RunID != old || lf.Origin != "agent_failure" || lf.Since != 2 {
		t.Errorf("latest failure = %+v, want %s with 2 completed since", lf, old)
	}
	// Filters: the completion count follows the same repo/harness/kinds restriction.
	if lf := f.latest(relCall{Owner: o.User, Repo: r1}); lf.RunID != old || lf.Since != 1 {
		t.Errorf("repo filter: %+v, want since 1 (the r2 completion is out)", lf)
	}
	if lf := f.latest(relCall{Owner: o.User, Harness: "codex"}); lf.Found {
		t.Errorf("harness codex: %+v, want none (the only failure ran on claude)", lf)
	}
	if lf := f.latest(relCall{Owner: o.User, Repo: r2}); lf.Found {
		t.Errorf("repo r2: %+v, want none", lf)
	}
	if lf := f.latest(relCall{Owner: o.User, Kinds: []string{runkind.Task}}); lf.Found {
		t.Errorf("kinds task: %+v, want none", lf)
	}
	// An owner with no failure, and the null object's zero values.
	if lf := f.latest(relCall{Owner: none.User}); lf.Found || lf.Since != 0 || lf.RunID != uuid.Nil || lf.Origin != "" || !lf.FailedAt.IsZero() {
		t.Errorf("no-failure latest = %+v", lf)
	}
}

// TestAnalyticsLatestFailureAllScopeLiveDB: the all-scope statement returns the newest failure
// of any owner and its owner id; completions of any owner count.
func TestAnalyticsLatestFailureAllScopeLiveDB(t *testing.T) {
	f := newRelFixture(t)
	f.claimYear(2099)
	a, b := f.owner("lf-all-a"), f.owner("lf-all-b")
	ra, _ := f.repo(a, "lf-all")
	rb, _ := f.repo(b, "lf-all")
	T := utcAt(2099, 1, 2, 10, 0, 0)
	f.run(spendRun{Owner: a.User, Repo: ra, Status: "failed", FailOrigin: "agent_failure", At: T.Add(-time.Hour), Finished: T.Add(-time.Hour)})
	newest := f.run(spendRun{Owner: b.User, Repo: rb, Status: "failed", FailOrigin: "run_timeout", At: T.Add(-time.Hour), Finished: T})
	f.run(spendRun{Owner: a.User, Repo: ra, Status: "completed", At: T, Finished: T.Add(time.Hour)})
	f.run(spendRun{Owner: b.User, Repo: rb, Status: "completed", At: T, Finished: T.Add(2 * time.Hour)})
	f.run(spendRun{Owner: b.User, Repo: rb, Status: "completed", At: T, Finished: T}) // not strictly after
	lf := f.latest(relCall{})
	if !lf.Found || lf.RunID != newest || lf.UserID != b.User || lf.Origin != "run_timeout" || lf.Since != 2 {
		t.Errorf("all-scope latest = %+v, want run %s of b with 2 completed since", lf, newest)
	}
	if lf := f.latest(relCall{Repo: ra}); lf.RunID == newest || !lf.Found || lf.UserID != a.User || lf.Since != 1 {
		// repo ra: a's failure at T-1h; completions in repo ra after it: only a's completed run.
		t.Errorf("all-scope repo filter = %+v", lf)
	}
}
