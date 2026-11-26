package store_test

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731: live-database proofs of the Capacity historical statements. Skipped without
// UZI_TEST_DATABASE_URL. Every case states its expected run-seconds as plain arithmetic.

func TestAnalyticsCapacityOverlapLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capov")
	repo, _ := f.repo(o, "capov")
	now := capDay(25, 12)
	run := func(created, started, finished time.Time) {
		f.capRun(capRun{Owner: o.User, Repo: repo, Created: created, Started: started, Finished: finished})
	}
	h := func(d time.Duration, base time.Time) time.Time { return base.Add(d) }
	// A began before the window: only its overlap [Nov 19 00:00, 06:00) counts.
	run(h(-time.Hour, capDay(18, 12)), capDay(18, 12), capDay(19, 6))
	// B ends exactly where C starts: no double count at the tie, no gap.
	run(capDay(19, 23), capDay(20, 0), capDay(20, 12))
	run(capDay(20, 11), capDay(20, 12), capDay(21, 0))
	// D and E start at the same instant.
	run(capDay(21, 5), capDay(21, 6), capDay(21, 12))
	run(capDay(21, 5), capDay(21, 6), capDay(21, 18))
	// Nov 22 is a gap. G spans the Nov 23/24 bucket boundary.
	run(capDay(23, 17), capDay(23, 18), capDay(24, 6))
	// F is still running: its interval ends at the captured now (partial today bucket).
	f.capRun(capRun{Owner: o.User, Repo: repo, Status: "running", Created: capDay(25, 5), Started: capDay(25, 6)})
	// Not candidates: ended before the window, and created after now.
	run(capDay(17, 0), capDay(17, 1), capDay(18, 12))
	f.capRun(capRun{Owner: o.User, Repo: repo, Status: "running", Created: capDay(26, 0), Started: capDay(26, 1)})

	res := f.capacity(capCall{Owner: o.User, Now: now})
	day := 86400.0
	wantRun := []float64{21600, 86400, 64800, 0, 21600, 21600, 21600}
	wantDur := []float64{day, day, day, day, day, day, 43200}
	wantAvg := []float64{0.25, 1, 0.75, 0, 0.25, 0.25, 0.5}
	for i := range wantRun {
		wantOcc(t, "bucket "+string(rune('0'+i)), res.Doc.Occupancy.Buckets[i], wantRun[i], wantDur[i], f6(wantAvg[i]), "")
	}
	// 237600 run-seconds over the 6.5-day window.
	wantOcc(t, "window", res.Doc.Occupancy.Window, 237600, 561600, f6(0.423077), "")
	wantPopulation(t, "overlap", res.Doc.Population, store.AnalyticsCapacityPopulation{
		Candidates: 7, WithInterval: 7, Ongoing: 1,
	})
	if res.Doc.GeneratedAt != "2026-11-25T12:00:00.000000Z" {
		t.Errorf("generated_at = %q", res.Doc.GeneratedAt)
	}
}

func TestAnalyticsCapacityIntervalQualityLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capq")
	repo, _ := f.repo(o, "capq")
	now := capDay(25, 12)
	// Missing start: counted, contributes nothing.
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(22, 8), Finished: capDay(22, 12)})
	// Invalid: ends before it starts; and a nonterminal run that starts at or after now.
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(22, 8), Started: capDay(22, 10), Finished: capDay(22, 9)})
	f.capRun(capRun{Owner: o.User, Repo: repo, Status: "running", Created: capDay(24, 0), Started: capDay(26, 0)})
	// Terminal without finished_at: status_since stands in for the end (Nov 22 10:00-12:00).
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(22, 9), Started: capDay(22, 10), StatusAt: capDay(22, 12)})
	// Ongoing: Nov 24 12:00 to now (24h).
	f.capRun(capRun{Owner: o.User, Repo: repo, Status: "running", Created: capDay(24, 11), Started: capDay(24, 12)})

	res := f.capacity(capCall{Owner: o.User, Now: now})
	wantPopulation(t, "quality", res.Doc.Population, store.AnalyticsCapacityPopulation{
		Candidates: 5, WithInterval: 2, WithoutStart: 1, InvalidInterval: 2, EndFromStatusSince: 1, Ongoing: 2,
	})
	wantOcc(t, "Nov 22", res.Doc.Occupancy.Buckets[3], 7200, 86400, f6(0.083333), "")
	wantOcc(t, "Nov 24", res.Doc.Occupancy.Buckets[5], 43200, 86400, f6(0.5), "")
	wantOcc(t, "Nov 25", res.Doc.Occupancy.Buckets[6], 43200, 43200, f6(1), "")
	wantOcc(t, "window", res.Doc.Occupancy.Window, 7200+86400, 561600, f6(0.166667), "")
}

func TestAnalyticsCapacityLanesLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capln")
	repo, _ := f.repo(o, "capln")
	now := capDay(25, 12)
	// Chat is never ordinary work, even when running for days.
	f.capRun(capRun{Owner: o.User, Kind: runkind.Chat, Status: "running", Created: capDay(19, 0), Started: capDay(19, 0)})
	// A cross_check run on its lane is excluded, a lane-false cross_check run is included and
	// disclosed.
	f.capRun(capRun{Owner: o.User, Repo: repo, Kind: runkind.CrossCheck, Status: "running", Lane: true,
		Created: capDay(20, 0), Started: capDay(20, 0)})
	f.capRun(capRun{Owner: o.User, Repo: repo, Kind: runkind.CrossCheck, Created: capDay(20, 0), Started: capDay(20, 0), Finished: capDay(20, 6)})

	res := f.capacity(capCall{Owner: o.User, Now: now})
	wantPopulation(t, "lanes", res.Doc.Population, store.AnalyticsCapacityPopulation{
		Candidates: 1, WithInterval: 1, CrossCheckKindIncluded: 1,
	})
	wantOcc(t, "Nov 20", res.Doc.Occupancy.Buckets[1], 21600, 86400, f6(0.25), "")
	wantOcc(t, "window", res.Doc.Occupancy.Window, 21600, 561600, f6(0.038462), "")

	// kind=chat alone selects nothing ordinary.
	chat := f.capacity(capCall{Owner: o.User, Now: now, Kinds: []string{runkind.Chat}})
	wantPopulation(t, "chat only", chat.Doc.Population, store.AnalyticsCapacityPopulation{})
	wantOcc(t, "chat only window", chat.Doc.Occupancy.Window, 0, 561600, f6(0), "")
	wantWait(t, "chat only wait", chat.Doc.UsageWait.Window, 0, 0, nil, store.AnalyticsReasonNoApplicableRuns)

	// kind=cross_check alone: the lane-false run is included and disclosed, the lane run is not.
	cc := f.capacity(capCall{Owner: o.User, Now: now, Kinds: []string{runkind.CrossCheck}})
	wantPopulation(t, "cross_check only", cc.Doc.Population, store.AnalyticsCapacityPopulation{
		Candidates: 1, WithInterval: 1, CrossCheckKindIncluded: 1,
	})
	wantOcc(t, "cross_check only window", cc.Doc.Occupancy.Window, 21600, 561600, f6(0.038462), "")
}

func TestAnalyticsCapacityMidnightZeroDurationBucketLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capmn")
	repo, _ := f.repo(o, "capmn")
	// 05:00Z on Nov 25 is local midnight in New York: today's bucket is [now, now).
	now := utcAt(2026, time.November, 25, 5, 0, 0)
	f.capRun(capRun{Owner: o.User, Repo: repo, Status: "running", Created: utcAt(2026, time.November, 24, 4, 0, 0), Started: utcAt(2026, time.November, 24, 5, 0, 0)})

	res := f.capacity(capCall{Owner: o.User, Now: now, Zone: "America/New_York"})
	last := len(res.Doc.Occupancy.Buckets) - 1
	wantOcc(t, "Nov 24 local", res.Doc.Occupancy.Buckets[last-1], 86400, 86400, f6(1), "")
	wantOcc(t, "final zero-duration bucket", res.Doc.Occupancy.Buckets[last], 0, 0, nil, store.AnalyticsReasonZeroDurationBucket)
	wantWait(t, "final bucket wait", res.Doc.UsageWait.Buckets[last], 0, 0, nil, store.AnalyticsReasonNoApplicableRuns)
	if got := res.Doc.Occupancy.Window.Reason; got != nil {
		t.Errorf("window reason = %q, want none: the range has duration", *got)
	}
}

func TestAnalyticsCapacityDSTDaysLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capdst")
	repo, _ := f.repo(o, "capdst")
	// Fall back: Sunday Nov 1 2026 is 25 hours long in New York (04:00Z to 05:00Z next day).
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: utcAt(2026, time.November, 1, 3, 0, 0),
		Started: utcAt(2026, time.November, 1, 4, 0, 0), Finished: utcAt(2026, time.November, 2, 5, 0, 0)})
	fall := f.capacity(capCall{Owner: o.User, Now: utcAt(2026, time.November, 3, 15, 0, 0), Zone: "America/New_York"})
	wantOcc(t, "25h day", fall.Doc.Occupancy.Buckets[4], 90000, 90000, f6(1), "")

	// Spring forward: Sunday Mar 8 2026 is 23 hours long (05:00Z to 04:00Z next day).
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: utcAt(2026, time.March, 8, 4, 0, 0),
		Started: utcAt(2026, time.March, 8, 5, 0, 0), Finished: utcAt(2026, time.March, 9, 4, 0, 0)})
	spring := f.capacity(capCall{Owner: o.User, Now: utcAt(2026, time.March, 9, 15, 0, 0), Zone: "America/New_York"})
	wantOcc(t, "23h day", spring.Doc.Occupancy.Buckets[5], 82800, 82800, f6(1), "")
}

func TestAnalyticsCapacityAllTimeLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	now := capDay(25, 12)

	// Empty scope: no buckets, an empty range, never a division.
	empty := f.owner("capall0")
	res := f.capacity(capCall{Owner: empty.User, Now: now, Window: analytics.WindowAll})
	if len(res.Doc.Occupancy.Buckets) != 0 || len(res.Doc.UsageWait.Buckets) != 0 {
		t.Fatalf("empty scope has buckets: %+v", res.Doc)
	}
	if !bytes.Contains(res.Raw, []byte(`"buckets": []`)) {
		t.Errorf("zero buckets are not an empty array in %s", res.Raw)
	}
	wantOcc(t, "empty window", res.Doc.Occupancy.Window, 0, 0, nil, store.AnalyticsReasonZeroDurationRange)
	wantWait(t, "empty wait", res.Doc.UsageWait.Window, 0, 0, nil, store.AnalyticsReasonNoApplicableRuns)
	wantPopulation(t, "empty", res.Doc.Population, store.AnalyticsCapacityPopulation{})

	// Ordinary weekly buckets: one run inside the first week, one straddling the first
	// week boundary by 6 hours each side.
	o := f.owner("capall1")
	repo, _ := f.repo(o, "capall1")
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: utcAt(2026, time.September, 9, 8, 0, 0),
		Started: utcAt(2026, time.September, 9, 9, 0, 0), Finished: utcAt(2026, time.September, 9, 12, 0, 0)}) // 3h
	probe := f.capacity(capCall{Owner: o.User, Now: now, Window: analytics.WindowAll})
	if probe.Bounds.WidthWeeks != 1 {
		t.Fatalf("fixture: width %d weeks, want 1", probe.Bounds.WidthWeeks)
	}
	edge := probe.Bounds.BucketStarts[1]
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: edge.Add(-7 * time.Hour),
		Started: edge.Add(-6 * time.Hour), Finished: edge.Add(6 * time.Hour)})
	res = f.capacity(capCall{Owner: o.User, Now: now, Window: analytics.WindowAll})
	wantOccSecs(t, "week 0", res.Doc.Occupancy.Buckets[0], 3*3600+6*3600, 7*86400)
	wantOccSecs(t, "week 1", res.Doc.Occupancy.Buckets[1], 6*3600, 7*86400)
	wantOccSecs(t, "all-time window", res.Doc.Occupancy.Window, 3*3600+12*3600, res.Doc.Occupancy.Window.DurationSeconds)

	// Coarsened buckets (more than 104 weeks): exact multi-week boundaries.
	c := f.owner("capall2")
	crepo, _ := f.repo(c, "capall2")
	f.capRun(capRun{Owner: c.User, Repo: crepo, Created: utcAt(2020, time.January, 1, 0, 0, 0),
		Started: utcAt(2020, time.January, 1, 1, 0, 0), Finished: utcAt(2020, time.January, 2, 1, 0, 0)}) // 24h
	cprobe := f.capacity(capCall{Owner: c.User, Now: now, Window: analytics.WindowAll})
	if cprobe.Bounds.WidthWeeks < 2 {
		t.Fatalf("fixture: width %d weeks, want >= 2", cprobe.Bounds.WidthWeeks)
	}
	cedge := cprobe.Bounds.BucketStarts[1]
	f.capRun(capRun{Owner: c.User, Repo: crepo, Created: cedge.Add(-7 * time.Hour),
		Started: cedge.Add(-6 * time.Hour), Finished: cedge.Add(6 * time.Hour)})
	cres := f.capacity(capCall{Owner: c.User, Now: now, Window: analytics.WindowAll})
	width := float64(cres.Bounds.WidthWeeks) * 7 * 86400
	wantOccSecs(t, "coarse bucket 0", cres.Doc.Occupancy.Buckets[0], 86400+6*3600, width)
	wantOccSecs(t, "coarse bucket 1", cres.Doc.Occupancy.Buckets[1], 6*3600, width)
	var total float64
	for _, b := range cres.Doc.Occupancy.Buckets {
		total += b.RunSeconds
	}
	if !nearly(total, cres.Doc.Occupancy.Window.RunSeconds) || !nearly(total, 86400+12*3600) {
		t.Errorf("coarse bucket run-seconds sum %v, window %v, want %v", total, cres.Doc.Occupancy.Window.RunSeconds, 86400+12*3600)
	}
}

func TestAnalyticsCapacityUsageWaitLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capuw")
	other := f.owner("capuwx")
	repo, _ := f.repo(o, "capuw")
	repo2, _ := f.repo(o, "capuw2")
	orepo, _ := f.repo(other, "capuwx")
	now := capDay(25, 12)
	// Applicable: claude waited twice (counts once), claude not waited, codex waited, and a
	// lane-false cross_check (codex, not waited).
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(20, 10), Waits: 2})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(21, 10)})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(21, 11), Harness: "codex", Waits: 1})
	f.capRun(capRun{Owner: o.User, Repo: repo, Kind: runkind.CrossCheck, Created: capDay(22, 10)})
	// Not applicable: a chat run that waited, a lane-true cross_check that waited, another
	// owner's waited run, runs created at or after now and before the window.
	f.capRun(capRun{Owner: o.User, Kind: runkind.Chat, Created: capDay(20, 9), Waits: 3})
	f.capRun(capRun{Owner: o.User, Repo: repo, Kind: runkind.CrossCheck, Status: "running", Lane: true, Created: capDay(20, 8), Waits: 1})
	f.capRun(capRun{Owner: other.User, Repo: orepo, Created: capDay(20, 10), Waits: 1})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(25, 12), Waits: 1})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(26, 0), Waits: 1})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(18, 23), Waits: 1})
	// A second repo of the same owner, to prove the repo selector.
	f.capRun(capRun{Owner: o.User, Repo: repo2, Created: capDay(23, 10), Waits: 1})
	// A started ordinary run that is parked in a limit wait right now (non-terminal limit_wait):
	// it has waited once, so it counts exactly once in usage_wait, and its retained interval runs
	// from first_started_at to now (Nov 24 07:00 to Nov 25 12:00 = 29h).
	f.capRun(capRun{Owner: o.User, Repo: repo, Status: "limit_wait", Created: capDay(24, 6), Started: capDay(24, 7), Waits: 1})

	all := f.capacity(capCall{Owner: o.User, Now: now})
	wantWait(t, "unfiltered", all.Doc.UsageWait.Window, 4, 6, f6(0.666667), "")
	// Unfiltered per bucket (Nov 19 = bucket 0): Nov 20 1/1, Nov 21 1/2, Nov 22 0/1, Nov 23 1/1
	// (the second repo's run), Nov 24 1/1 (the parked run), the rest empty.
	for i, c := range []struct {
		count, n int64
		share    *float64
	}{{0, 0, nil}, {1, 1, f6(1)}, {1, 2, f6(0.5)}, {0, 1, f6(0)}, {1, 1, f6(1)}, {1, 1, f6(1)}, {0, 0, nil}} {
		wantWait(t, "unfiltered bucket", all.Doc.UsageWait.Buckets[i], c.count, c.n, c.share, store.AnalyticsReasonNoApplicableRuns)
	}
	// The parked run's occupancy: 17h in Nov 24, 12h in Nov 25 (now), 29h in the window; it is the
	// only retained interval in this scope and the only ongoing run.
	wantOccSecs(t, "parked Nov 24", all.Doc.Occupancy.Buckets[5], 17*3600, 86400)
	wantOccSecs(t, "parked Nov 25", all.Doc.Occupancy.Buckets[6], 12*3600, 43200)
	wantOccSecs(t, "parked window", all.Doc.Occupancy.Window, 29*3600, 561600)
	if all.Doc.Population.Ongoing != 1 || all.Doc.Population.WithInterval != 1 {
		t.Errorf("population = %+v, want exactly the parked run as ongoing with an interval", all.Doc.Population)
	}

	scoped := f.capacity(capCall{Owner: o.User, Now: now, Repo: repo})
	wantWait(t, "repo", scoped.Doc.UsageWait.Window, 3, 5, f6(0.6), "")
	// Per bucket: Nov 20 1/1, Nov 21 1/2, Nov 22 0/1, Nov 24 1/1, the rest empty.
	for i, c := range []struct {
		count, n int64
		share    *float64
	}{{0, 0, nil}, {1, 1, f6(1)}, {1, 2, f6(0.5)}, {0, 1, f6(0)}, {0, 0, nil}, {1, 1, f6(1)}, {0, 0, nil}} {
		wantWait(t, "repo bucket", scoped.Doc.UsageWait.Buckets[i], c.count, c.n, c.share, store.AnalyticsReasonNoApplicableRuns)
	}
	claude := f.capacity(capCall{Owner: o.User, Now: now, Repo: repo, Harness: "claude"})
	wantWait(t, "claude", claude.Doc.UsageWait.Window, 2, 3, f6(0.666667), "")
	codex := f.capacity(capCall{Owner: o.User, Now: now, Repo: repo, Harness: "codex"})
	wantWait(t, "codex", codex.Doc.UsageWait.Window, 1, 2, f6(0.5), "")
	issue := f.capacity(capCall{Owner: o.User, Now: now, Repo: repo, Kinds: []string{runkind.Issue}})
	wantWait(t, "issue", issue.Doc.UsageWait.Window, 3, 4, f6(0.75), "")
	chat := f.capacity(capCall{Owner: o.User, Now: now, Kinds: []string{runkind.Chat}})
	wantWait(t, "chat", chat.Doc.UsageWait.Window, 0, 0, nil, store.AnalyticsReasonNoApplicableRuns)
}

func TestAnalyticsCapacityUsageWaitBoundariesLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capuwb")
	repo, _ := f.repo(o, "capuwb")
	now := capDay(25, 12)
	from := capDay(19, 0)
	// created_at == from counts (bucket 0); one microsecond earlier and exactly now do not.
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: from, Finished: from, Waits: 1})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: from.Add(-time.Microsecond), Waits: 1})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: now, Waits: 1})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(21, 3)})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(22, 3)})
	res := f.capacity(capCall{Owner: o.User, Now: now})
	wantWait(t, "1 of 3", res.Doc.UsageWait.Window, 1, 3, f6(0.333333), "")
	wantWait(t, "bucket 0", res.Doc.UsageWait.Buckets[0], 1, 1, f6(1), "")
}

func TestAnalyticsCapacitySelectorsAndIsolationLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capsel")
	other := f.owner("capselx")
	repoA, _ := f.repo(o, "capsela")
	repoB, _ := f.repo(o, "capselb")
	orepo, _ := f.repo(other, "capselx")
	now := capDay(25, 12)
	f.capRun(capRun{Owner: o.User, Repo: repoA, Harness: "claude", Created: capDay(20, 0), Started: capDay(20, 0), Finished: capDay(20, 6)}) // 6h
	f.capRun(capRun{Owner: o.User, Repo: repoB, Harness: "codex", Created: capDay(20, 0), Started: capDay(20, 0), Finished: capDay(20, 12)}) // 12h
	f.capRun(capRun{Owner: other.User, Repo: orepo, Created: capDay(20, 0), Started: capDay(20, 0), Finished: capDay(21, 0)})

	cases := []struct {
		name string
		call capCall
		secs float64
		cand int64
	}{
		{"owner", capCall{Owner: o.User, Now: now}, 18 * 3600, 2},
		{"repo A", capCall{Owner: o.User, Now: now, Repo: repoA}, 6 * 3600, 1},
		{"harness codex", capCall{Owner: o.User, Now: now, Harness: "codex"}, 12 * 3600, 1},
		{"repo A harness codex", capCall{Owner: o.User, Now: now, Repo: repoA, Harness: "codex"}, 0, 0},
		{"other owner", capCall{Owner: other.User, Now: now}, 24 * 3600, 1},
	}
	for _, c := range cases {
		res := f.capacity(c.call)
		if !nearly(res.Doc.Occupancy.Window.RunSeconds, c.secs) || res.Doc.Population.Candidates != c.cand {
			t.Errorf("%s: run_seconds %v candidates %d, want %v and %d", c.name, res.Doc.Occupancy.Window.RunSeconds, res.Doc.Population.Candidates, c.secs, c.cand)
		}
	}
}

func TestAnalyticsCapacityIgnoresWorkersAndSessionTimeZoneLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("capenv")
	repo, _ := f.repo(o, "capenv")
	now := capDay(25, 12)
	running := f.capRun(capRun{Owner: o.User, Repo: repo, Status: "running", Created: capDay(24, 0), Started: capDay(24, 1)})
	f.capRun(capRun{Owner: o.User, Repo: repo, Created: capDay(23, 0), Started: capDay(23, 1), Finished: capDay(23, 5), Waits: 1})

	base := f.capacity(capCall{Owner: o.User, Now: now, Zone: "America/New_York"})
	for _, tz := range []string{"Asia/Kolkata", "America/Los_Angeles", "UTC"} {
		got := f.capacity(capCall{Owner: o.User, Now: now, Zone: "America/New_York", SessionTZ: tz})
		if !bytes.Equal(got.Raw, base.Raw) {
			t.Errorf("session TimeZone %s changed the document:\n%s\nvs\n%s", tz, got.Raw, base.Raw)
		}
	}

	// A worker, and a run bound to it, change nothing: history never consults worker slots.
	worker := uuid.New()
	mustExec(f.ctx, t, f.pool, `INSERT INTO workers(id, user_id, name, token_hash) VALUES ($1, $2, 'capenv', $3)`,
		worker, o.User, worker[:])
	mustExec(f.ctx, t, f.pool, `UPDATE runs SET worker_id = $2 WHERE id = $1`, running, worker)
	if got := f.capacity(capCall{Owner: o.User, Now: now, Zone: "America/New_York"}); !bytes.Equal(got.Raw, base.Raw) {
		t.Errorf("a worker row changed the document:\n%s\nvs\n%s", got.Raw, base.Raw)
	}
}

func TestAnalyticsCapacityAllScopeSumsOwnersLiveDB(t *testing.T) {
	ctx, pool := capPrivateDB(t)
	f := capFixtureOn(t, ctx, pool)
	a, b := f.owner("capsuma"), f.owner("capsumb")
	ra, _ := f.repo(a, "capsuma")
	rb, _ := f.repo(b, "capsumb")
	now := capDay(25, 12)
	f.capRun(capRun{Owner: a.User, Repo: ra, Created: capDay(20, 0), Started: capDay(20, 0), Finished: capDay(20, 6), Waits: 1})
	f.capRun(capRun{Owner: b.User, Repo: rb, Created: capDay(20, 3), Started: capDay(20, 3), Finished: capDay(21, 3)})
	f.capRun(capRun{Owner: b.User, Repo: rb, Status: "running", Created: capDay(24, 11), Started: capDay(24, 12)})
	f.capRun(capRun{Owner: b.User, Kind: runkind.Chat, Status: "running", Created: capDay(19, 0), Started: capDay(19, 0)})

	ownerA := f.capacity(capCall{Owner: a.User, Now: now})
	ownerB := f.capacity(capCall{Owner: b.User, Now: now})
	all := f.capacity(capCall{Now: now})
	for i := range all.Doc.Occupancy.Buckets {
		want := ownerA.Doc.Occupancy.Buckets[i].RunSeconds + ownerB.Doc.Occupancy.Buckets[i].RunSeconds
		if !nearly(all.Doc.Occupancy.Buckets[i].RunSeconds, want) {
			t.Errorf("bucket %d: all %v, owners sum %v", i, all.Doc.Occupancy.Buckets[i].RunSeconds, want)
		}
	}
	if got, want := all.Doc.Occupancy.Window.RunSeconds, ownerA.Doc.Occupancy.Window.RunSeconds+ownerB.Doc.Occupancy.Window.RunSeconds; !nearly(got, want) {
		t.Errorf("window: all %v, owners sum %v", got, want)
	}
	// 6h + 24h + 24h (Nov 24 12:00 to Nov 25 12:00).
	wantOcc(t, "all window", all.Doc.Occupancy.Window, (6+24+24)*3600, 561600, f6(0.346154), "")
	wantPopulation(t, "all", all.Doc.Population, store.AnalyticsCapacityPopulation{Candidates: 3, WithInterval: 3, Ongoing: 1})
	wantWait(t, "all wait", all.Doc.UsageWait.Window, 1, 3, f6(0.333333), "")
	// The all-scope statement also takes the repo selector across owners.
	one := f.capacity(capCall{Now: now, Repo: rb})
	wantPopulation(t, "all repo", one.Doc.Population, store.AnalyticsCapacityPopulation{Candidates: 2, WithInterval: 2, Ongoing: 1})
}
