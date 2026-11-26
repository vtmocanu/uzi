package store_test

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4684 M3: data tests for AnalyticsOwnerSpend and AnalyticsAllSpend. Every test
// seeds its own users and repos through SQL and builds the statement parameters with the
// real analytics calendar code. All tests skip without UZI_TEST_DATABASE_URL.
//
// Unless a test says otherwise: now = 2026-11-25T12:00Z, zone UTC, window 30d, so
// from = 2026-10-27T00:00Z ("day 1") and previous_from = 2026-09-27T00:00Z; day k is
// from + (k-1) days, so day 3 is 2026-10-29 and day 9 is 2026-11-04.

var spendNow = utcAt(2026, 11, 25, 12, 0, 0)

// spendDay is the instant h hours into day k of the 30d window ending at spendNow.
func spendDay(k, h int) time.Time {
	return utcAt(2026, 10, 27, 0, 0, 0).AddDate(0, 0, k-1).Add(time.Duration(h) * time.Hour)
}

func sumDollars(t *testing.T, vals ...string) *big.Rat {
	t.Helper()
	sum := new(big.Rat)
	for _, v := range vals {
		sum.Add(sum, ratOf(t, v))
	}
	return sum
}

func wantRatSum(t *testing.T, what string, got *big.Rat, want string) {
	t.Helper()
	if got.Cmp(ratOf(t, want)) != 0 {
		t.Errorf("%s = %s, want %s", what, got.FloatString(6), want)
	}
}

func groupByKey(t *testing.T, groups []store.AnalyticsSplitGroup, key string) store.AnalyticsSplitGroup {
	t.Helper()
	for _, g := range groups {
		if g.Key != nil && *g.Key == key {
			return g
		}
	}
	t.Fatalf("no split group with key %q", key)
	return store.AnalyticsSplitGroup{}
}

func rowByKey(t *testing.T, rows []store.AnalyticsBreakdownRow, key string) store.AnalyticsBreakdownRow {
	t.Helper()
	for _, r := range rows {
		if r.Key != nil && *r.Key == key {
			return r
		}
	}
	t.Fatalf("no breakdown row with key %q", key)
	return store.AnalyticsBreakdownRow{}
}

func TestAnalyticsSpendReconciliationLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("recon")
	repo, _ := f.repo(o, "recon")

	a := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 10, 31, 10, 0, 0), Finished: utcAt(2026, 10, 31, 11, 0, 0)})
	f.metered(a, "model-a", "1.100000")
	b := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 11, 7, 8, 0, 0), Status: "failed", FailOrigin: "agent_failure", Harness: "codex"})
	f.metered(b, "model-b", "2.250000")
	c := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 11, 25, 9, 0, 0), Status: "running"})
	f.metered(c, "model-a", "0.333333")
	// d is in the previous window; e was CREATED before the window (the previous one) and
	// FINISHES inside the current window: spend is attributed by creation, so e is previous.
	d := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 10, 10, 12, 0, 0), Finished: utcAt(2026, 10, 10, 13, 0, 0)})
	f.metered(d, "model-a", "4.000000")
	e := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 10, 26, 22, 0, 0), Finished: utcAt(2026, 10, 27, 3, 0, 0)})
	f.metered(e, "model-a", "0.750000")
	// A chat run with a stray historical usage row is untracked, never a cost.
	chat := f.run(spendRun{Owner: o.User, Kind: runkind.Chat, At: utcAt(2026, 11, 1, 9, 0, 0)})
	f.metered(chat, "model-a", "9.990000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow, Split: "harness", Breakdown: "kind"})
	d1 := res.Doc

	// Independent expectations straight from run_usage.
	var wantCur, wantPrev string
	const sumSQL = `SELECT COALESCE(sum(ru.cost_usd), 0)::text FROM run_usage ru JOIN runs r ON r.id = ru.run_id
	                WHERE r.user_id = $1 AND r.kind <> 'chat' AND r.created_at >= $2 AND r.created_at < $3`
	if err := f.pool.QueryRow(f.ctx, sumSQL, o.User, res.Bounds.From, res.Bounds.Now).Scan(&wantCur); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(f.ctx, sumSQL, o.User, *res.Bounds.PreviousFrom, res.Bounds.From).Scan(&wantPrev); err != nil {
		t.Fatal(err)
	}
	wantDollars(t, "independent current", wantCur, "3.683333")
	wantDollars(t, "independent previous", wantPrev, "4.750000")
	wantDollars(t, "totals.current.cost", d1.Totals.Current.Cost, wantCur)
	if d1.Totals.Previous == nil {
		t.Fatal("30d window has no previous totals")
	}
	wantDollars(t, "totals.previous.cost", d1.Totals.Previous.Cost, wantPrev)
	if d1.Totals.Current.Runs != 3 || d1.Totals.Previous.Runs != 2 {
		t.Errorf("runs current %d previous %d, want 3 and 2 (e is previous: created before from)", d1.Totals.Current.Runs, d1.Totals.Previous.Runs)
	}
	if d1.UntrackedChatRuns != 1 {
		t.Errorf("untracked_chat_runs = %d, want 1", d1.UntrackedChatRuns)
	}
	if bytes.Contains(res.Raw, []byte("9.99")) {
		t.Error("the chat run's stray usage leaked into the document")
	}

	// Additive measures reconcile exactly: buckets, groups and breakdown rows all sum to
	// the totals.
	var cells, groups, rows []string
	var cellRuns int64
	for _, c := range d1.Buckets {
		cells = append(cells, c.Cost)
		cellRuns += c.Runs
	}
	for _, g := range d1.Split.Groups {
		groups = append(groups, g.Totals.Cost)
	}
	for _, r := range d1.Breakdown.Rows {
		rows = append(rows, r.Cost)
	}
	wantRatSum(t, "sum of bucket costs", sumDollars(t, cells...), wantCur)
	wantRatSum(t, "sum of group costs", sumDollars(t, groups...), wantCur)
	wantRatSum(t, "sum of breakdown costs", sumDollars(t, rows...), wantCur)
	if cellRuns != 3 {
		t.Errorf("sum of bucket runs = %d, want 3", cellRuns)
	}
	if len(d1.Buckets) != 30 {
		t.Errorf("30d window emitted %d buckets, want 30", len(d1.Buckets))
	}
	cl, cx := groupByKey(t, d1.Split.Groups, "claude"), groupByKey(t, d1.Split.Groups, "codex")
	wantDollars(t, "claude group", cl.Totals.Cost, "1.433333")
	wantDollars(t, "codex group", cx.Totals.Cost, "2.250000")
	if cl.Rank != 2 || cx.Rank != 1 {
		t.Errorf("ranks claude %d codex %d, want 2 and 1 (by current cost)", cl.Rank, cx.Rank)
	}

	// Top runs: most expensive first, current window only.
	if len(d1.TopRuns) != 3 {
		t.Fatalf("top runs = %d, want 3", len(d1.TopRuns))
	}
	for i, want := range []struct {
		id   uuid.UUID
		cost string
	}{{b, "2.250000"}, {a, "1.100000"}, {c, "0.333333"}} {
		if d1.TopRuns[i].RunID != want.id {
			t.Errorf("top run %d = %s, want %s", i, d1.TopRuns[i].RunID, want.id)
		}
		wantDollars(t, "top run cost", d1.TopRuns[i].Cost, want.cost)
	}

	// Breakdown by kind: finished runs are a (completed, 3600 s) and b (failed, 0 s); c is in
	// flight and in no finished figure; e finished in the window but belongs to previous.
	if len(d1.Breakdown.Rows) != 1 {
		t.Fatalf("breakdown rows = %d, want 1", len(d1.Breakdown.Rows))
	}
	row := d1.Breakdown.Rows[0]
	if row.Runs != 3 || row.Finished != 2 || row.Outcomes.Completed != 1 || row.Outcomes.Failed != 1 {
		t.Errorf("breakdown row runs %d finished %d outcomes %+v", row.Runs, row.Finished, row.Outcomes)
	}
	if row.ElapsedP50Seconds.Value == nil || *row.ElapsedP50Seconds.Value != 1800 || row.ElapsedP90Seconds.Value == nil || *row.ElapsedP90Seconds.Value != 3240 {
		t.Errorf("elapsed p50/p90 = %v/%v, want 1800/3240", row.ElapsedP50Seconds.Value, row.ElapsedP90Seconds.Value)
	}
	if row.CostShare.Value == nil || *row.CostShare.Value != 1 {
		t.Errorf("cost share = %v, want 1", row.CostShare.Value)
	}

	// The harness filter narrows the cohort.
	codex := f.call(spendCall{Owner: o.User, Now: spendNow, Harness: "codex"})
	wantDollars(t, "codex-only cost", codex.Doc.Totals.Current.Cost, "2.250000")
	if codex.Doc.Totals.Current.Runs != 1 {
		t.Errorf("codex-only runs = %d, want 1", codex.Doc.Totals.Current.Runs)
	}
}

func TestAnalyticsSpendDisclosureLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("disc")
	repo, _ := f.repo(o, "disc")

	// M: mixed-status multi-model run (A metered, B subscription): run status unreported with
	// the metered dollars retained. X: no run_usage rows at all. P: plain metered, model A.
	m := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 11, 1, 10, 0, 0), Finished: utcAt(2026, 11, 1, 11, 0, 0)})
	f.usage(m, "model-a", "metered", "1.234567", 100, 40, 10, 20)
	f.usage(m, "model-b", "subscription", "0", 50, 10, 5, 10)
	p := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 11, 2, 10, 0, 0), Finished: utcAt(2026, 11, 2, 11, 0, 0)})
	f.usage(p, "model-a", "metered", "2.000000", 100, 40, 10, 20)
	x := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 11, 3, 10, 0, 0), Finished: utcAt(2026, 11, 3, 11, 0, 0)})

	res := f.call(spendCall{Owner: o.User, Now: spendNow, Split: "usage_model", Breakdown: "repo"})
	d := res.Doc
	tot := d.Totals.Current
	wantDollars(t, "totals cost", tot.Cost, "3.234567")
	wantDisclosure(t, "totals", tot.Disclosure, 1, 0, 1, 1, true)
	if tot.Coverage.Metered != 1 || tot.Coverage.Applicable != 3 || tot.Coverage.Value == nil || *tot.Coverage.Value != 0.333333 {
		t.Errorf("coverage = %+v, want 1 of 3 (X stays in the denominator)", tot.Coverage)
	}
	if tot.Runs != 3 {
		t.Errorf("runs = %d, want 3", tot.Runs)
	}

	if !d.Split.NonAdditive || d.Split.Dimension != "usage_model" {
		t.Errorf("split = %q non_additive %v", d.Split.Dimension, d.Split.NonAdditive)
	}
	ga, gb, gu := groupByKey(t, d.Split.Groups, "model-a"), groupByKey(t, d.Split.Groups, "model-b"), groupByKey(t, d.Split.Groups, "unknown")
	if ga.Totals.Runs != 2 || gb.Totals.Runs != 1 || gu.Totals.Runs != 1 {
		t.Errorf("group runs A %d B %d unknown %d, want 2/1/1", ga.Totals.Runs, gb.Totals.Runs, gu.Totals.Runs)
	}
	wantDollars(t, "model-a cost", ga.Totals.Cost, "3.234567")
	wantDollars(t, "model-b cost", gb.Totals.Cost, "0")
	wantDollars(t, "unknown cost", gu.Totals.Cost, "0")
	wantDisclosure(t, "model-a", ga.Totals.Disclosure, 1, 0, 1, 0, true)
	wantDisclosure(t, "model-b", gb.Totals.Disclosure, 0, 0, 1, 0, true)
	wantDisclosure(t, "unknown", gu.Totals.Disclosure, 0, 0, 0, 1, true)
	if ga.Rank != 1 || gb.Rank != 2 || gu.Rank != 3 {
		t.Errorf("ranks A %d B %d unknown %d, want 1/2/3 (cost, then key)", ga.Rank, gb.Rank, gu.Rank)
	}

	// Bucket cells carry the status of the runs in each cell; together they carry the totals'.
	var metered, unreported, missing, subscription int64
	for i, c := range d.Buckets {
		metered += c.Disclosure.Metered
		subscription += c.Disclosure.Subscription
		unreported += c.Disclosure.Unreported
		missing += c.Disclosure.Missing
		switch i {
		case 5: // M, 2026-11-01
			wantDisclosure(t, "cell 5", c.Disclosure, 0, 0, 1, 0, true)
			wantDollars(t, "cell 5 cost", c.Cost, "1.234567")
		case 6: // P
			wantDisclosure(t, "cell 6", c.Disclosure, 1, 0, 0, 0, false)
		case 7: // X
			wantDisclosure(t, "cell 7", c.Disclosure, 0, 0, 0, 1, true)
		default:
			wantDisclosure(t, "empty cell", c.Disclosure, 0, 0, 0, 0, false)
		}
	}
	if metered != 1 || subscription != 0 || unreported != 1 || missing != 1 {
		t.Errorf("cells sum to %d/%d/%d/%d, want 1/0/1/1", metered, subscription, unreported, missing)
	}

	// The single repo row and the ratio numerators carry the totals' counts.
	if len(d.Breakdown.Rows) != 1 {
		t.Fatalf("breakdown rows = %d, want 1", len(d.Breakdown.Rows))
	}
	wantDisclosure(t, "breakdown row", d.Breakdown.Rows[0].Disclosure, 1, 0, 1, 1, true)
	if d.Breakdown.Rows[0].Coverage.Applicable != 3 || d.Breakdown.Rows[0].Coverage.Metered != 1 {
		t.Errorf("breakdown coverage %+v", d.Breakdown.Rows[0].Coverage)
	}
	wantDisclosure(t, "cost per completed run", d.CostPerCompletedRun.Current.Disclosure, 1, 0, 1, 1, true)
	wantRatio(t, "cost per completed run", d.CostPerCompletedRun.Current, "3.234567", 3, "1.078189", "")
	wantDisclosure(t, "cache read", d.CacheRead.Disclosure, 1, 0, 1, 1, true)

	// Primary model: M's model A has the most output tokens; X has none, so unknown.
	prim := map[uuid.UUID]string{}
	for _, tr := range d.TopRuns {
		prim[tr.RunID] = tr.PrimaryModel
	}
	if prim[m] != "model-a" || prim[p] != "model-a" || prim[x] != "unknown" {
		t.Errorf("primary models %v", prim)
	}
	for _, tr := range d.TopRuns {
		switch tr.RunID {
		case m:
			if tr.UsageStatus != "unreported" {
				t.Errorf("M usage status %q", tr.UsageStatus)
			}
		case x:
			if tr.UsageStatus != "missing" {
				t.Errorf("X usage status %q", tr.UsageStatus)
			}
		}
	}
}

func TestAnalyticsSpendOtherGroupLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("other")
	repo, _ := f.repo(o, "other")

	// Eight models: five named groups (m1..m5, one single-model run each) and three that all
	// fall into Other (m6..m8), all used by ONE run q, plus m6 again on a second run t2.
	costs := []string{"10.000000", "9.000000", "8.000000", "7.000000", "6.000000"}
	for i, c := range costs {
		r := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(5+i, 8)})
		f.metered(r, "m"+string(rune('1'+i)), c)
	}
	q := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(12, 8)})
	f.metered(q, "m6", "3.000000")
	f.metered(q, "m7", "2.000000")
	f.metered(q, "m8", "1.000000")
	t2 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(13, 8)})
	f.metered(t2, "m6", "0.500000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow, Split: "usage_model"})
	g := res.Doc.Split.Groups
	if len(g) != 6 {
		t.Fatalf("groups = %d, want five named plus Other", len(g))
	}
	for i, want := range []string{"m1", "m2", "m3", "m4", "m5"} {
		if g[i].Key == nil || *g[i].Key != want || g[i].Rank != i+1 || g[i].IsOther {
			t.Errorf("group %d = %+v, want %s", i, g[i], want)
		}
	}
	other := g[5]
	if !other.IsOther || other.Key != nil || other.Rank != 6 {
		t.Fatalf("last group = %+v, want Other", other)
	}
	// Other holds m6, m7, m8 over two runs; q (three models in Other) counts once.
	if other.Totals.Runs != 2 {
		t.Errorf("Other runs = %d, want 2 (q counted once)", other.Totals.Runs)
	}
	wantDisclosure(t, "Other", other.Totals.Disclosure, 2, 0, 0, 0, false)
	wantDollars(t, "Other cost", other.Totals.Cost, "6.500000")
	var all []string
	for _, x := range g {
		all = append(all, x.Totals.Cost)
	}
	wantRatSum(t, "sum of group costs", sumDollars(t, all...), "46.500000")
	wantDollars(t, "totals cost", res.Doc.Totals.Current.Cost, "46.500000")
	if res.Doc.Totals.Current.Runs != 7 {
		t.Errorf("runs = %d, want 7", res.Doc.Totals.Current.Runs)
	}
	// Other's buckets are exact too: q is on day 12, t2 on day 13.
	if len(other.Buckets) != 30 {
		t.Fatalf("Other buckets = %d", len(other.Buckets))
	}
	wantDollars(t, "Other day 12", other.Buckets[11].Cost, "6.000000")
	wantDollars(t, "Other day 13", other.Buckets[12].Cost, "0.500000")
	if other.Buckets[11].Runs != 1 {
		t.Errorf("Other day 12 runs = %d, want 1", other.Buckets[11].Runs)
	}
}

func TestAnalyticsSpendPopulationLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("pop")
	repo, _ := f.repo(o, "pop")

	// p1: completed PR-carrying issue run, p2: completed plain run, chat: stray metered usage,
	// inflight: a running Delivery run with recorded cost.
	p1 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(3, 9), Finished: spendDay(3, 10), MR: 811, MRState: "merged"})
	f.metered(p1, "model-a", "2.000000")
	p2 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(4, 9), Finished: spendDay(4, 10)})
	f.metered(p2, "model-a", "1.000000")
	chat := f.run(spendRun{Owner: o.User, Kind: runkind.Chat, At: spendDay(4, 11)})
	f.metered(chat, "model-a", "9.990000")
	infl := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(5, 9), Status: "running"})
	f.metered(infl, "model-a", "0.500000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow, Split: "harness", Breakdown: "kind"})
	d := res.Doc

	// Chat: only in untracked_chat_runs.
	if d.UntrackedChatRuns != 1 {
		t.Errorf("untracked_chat_runs = %d, want 1", d.UntrackedChatRuns)
	}
	if bytes.Contains(res.Raw, []byte("9.99")) || bytes.Contains(res.Raw, []byte(chat.String())) {
		t.Error("the chat run appears in a cost figure or the top runs")
	}
	wantDollars(t, "totals cost (no chat)", d.Totals.Current.Cost, "3.500000")
	if d.Totals.Current.Runs != 3 {
		t.Errorf("runs = %d, want 3", d.Totals.Current.Runs)
	}
	// The in-flight run is an applicable non-chat run: it counts in coverage, classified by
	// its usage status (metered), and in the totals' cost.
	if c := d.Totals.Current.Coverage; c.Applicable != 3 || c.Metered != 3 {
		t.Errorf("coverage %+v, want 3 of 3 (in-flight run is applicable)", c)
	}
	// ... but in no finished, completed or failure denominator.
	if len(d.Breakdown.Rows) != 1 {
		t.Fatalf("breakdown rows = %d", len(d.Breakdown.Rows))
	}
	row := d.Breakdown.Rows[0]
	if row.Runs != 3 || row.Finished != 2 || row.Outcomes.Completed != 2 || row.Outcomes.Failed != 0 {
		t.Errorf("breakdown row runs %d finished %d outcomes %+v, want 3/2/{completed 2}", row.Runs, row.Finished, row.Outcomes)
	}
	if row.FailureRate.Value == nil || *row.FailureRate.Value != 0 {
		t.Errorf("failure rate = %+v, want 0 (finished 2)", row.FailureRate)
	}
	if row.CompletionRatio.Value == nil || *row.CompletionRatio.Value != 1 {
		t.Errorf("completion ratio = %+v, want 1", row.CompletionRatio)
	}
	// Cost per completed run: p1 (a PR-carrying completed Delivery run) is in the
	// denominator and its cost in the numerator; the in-flight run's cost stays in the
	// numerator only.
	wantRatio(t, "cost per completed run", d.CostPerCompletedRun.Current, "3.500000", 2, "1.750000", "")
	// p1's PR is merged: its numerator is p1's own cost.
	wantRatio(t, "cost per merged PR", d.CostPerMergedPR.Current, "2.000000", 1, "2.000000", "")
	for _, tr := range d.TopRuns {
		if tr.RunID == chat {
			t.Error("chat run in the top runs")
		}
	}
}

func TestAnalyticsSpendPrivacyAndSharedRepoLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	u := f.owner("priv-u")
	v := f.owner("priv-v")
	ru, ruPath := f.repo(u, "priv-u")
	rv, rvPath := f.repo(v, "priv-v") // owned by V; U's run references it

	own := f.run(spendRun{Owner: u.User, Repo: ru, At: spendDay(3, 9)})
	f.metered(own, "model-a", "1.000000")
	foreign := f.run(spendRun{Owner: u.User, Repo: rv, At: spendDay(4, 9)})
	f.metered(foreign, "model-a", "7.654321")
	norepo := f.run(spendRun{Owner: u.User, Kind: runkind.Job, At: spendDay(5, 9)})
	f.metered(norepo, "model-a", "0.250000")
	// V's own run on rv: must never appear in U's documents, revealed or not.
	vrun := f.run(spendRun{Owner: v.User, Repo: rv, At: spendDay(4, 10)})
	f.metered(vrun, "model-a", "99.990000")

	for _, split := range []string{"repo", "harness"} {
		masked := f.call(spendCall{Owner: u.User, Reveal: false, Now: spendNow, Split: split, Breakdown: "repo"})
		for _, secret := range []string{rv.String(), rvPath, "99.99", vrun.String()} {
			if bytes.Contains(masked.Raw, []byte(secret)) {
				t.Errorf("split=%s reveal=false: document contains %q", split, secret)
			}
		}
		wantDollars(t, "masked totals", masked.Doc.Totals.Current.Cost, "8.904321")
		// The foreign repo keeps its cost under inaccessible_repo with no identifiers.
		row := rowByKey(t, masked.Doc.Breakdown.Rows, "inaccessible_repo")
		wantDollars(t, "inaccessible_repo cost", row.Cost, "7.654321")
		if row.ID != nil || row.Label != nil {
			t.Errorf("inaccessible_repo row exposes id %v label %v", row.ID, row.Label)
		}
		own := rowByKey(t, masked.Doc.Breakdown.Rows, ru.String())
		if own.Label == nil || *own.Label != ruPath || own.ID == nil || *own.ID != ru {
			t.Errorf("own repo row = %+v", own)
		}
		nr := rowByKey(t, masked.Doc.Breakdown.Rows, "no_repo")
		wantDollars(t, "no_repo cost", nr.Cost, "0.250000")
		for _, tr := range masked.Doc.TopRuns {
			if tr.RunID == foreign && (tr.RepoKey != "inaccessible_repo" || tr.RepoID != nil || tr.RepoPath != nil) {
				t.Errorf("foreign top run repo %q %v %v", tr.RepoKey, tr.RepoID, tr.RepoPath)
			}
		}
	}

	// Shared repo: selected user U with a run on V's repo. reveal=true (the admin
	// selected-user path) shows R's id and path in groups and top runs; V's own run is still
	// out of scope.
	rev := f.call(spendCall{Owner: u.User, Reveal: true, Now: spendNow, Split: "repo", Breakdown: "repo"})
	if bytes.Contains(rev.Raw, []byte(vrun.String())) || bytes.Contains(rev.Raw, []byte("99.99")) {
		t.Error("reveal=true leaked V's own run")
	}
	wantDollars(t, "revealed totals", rev.Doc.Totals.Current.Cost, "8.904321")
	g := groupByKey(t, rev.Doc.Split.Groups, rv.String())
	if g.ID == nil || *g.ID != rv || g.Label == nil || *g.Label != rvPath {
		t.Errorf("revealed group = %+v", g)
	}
	wantDollars(t, "revealed group cost", g.Totals.Cost, "7.654321")
	for _, tr := range rev.Doc.TopRuns {
		if tr.RunID == foreign && (tr.RepoID == nil || *tr.RepoID != rv || tr.RepoPath == nil || *tr.RepoPath != rvPath) {
			t.Errorf("revealed top run repo %v %v", tr.RepoID, tr.RepoPath)
		}
	}
	for _, k := range []string{"inaccessible_repo"} {
		for _, gr := range rev.Doc.Split.Groups {
			if gr.Key != nil && *gr.Key == k {
				t.Errorf("reveal=true still has group %s", k)
			}
		}
	}
}

func TestAnalyticsSpendMergeWitnessInvarianceLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	a := f.owner("wit-a")
	b := f.owner("wit-b")
	ra, _ := f.repo(a, "wit")

	// A's run on PR (ra, 5), state not merged. B (another owner) also has a run on the same
	// (repo_id, mr_iid) -- created long before A's fold range -- whose state is toggled.
	ar := f.run(spendRun{Owner: a.User, Repo: ra, At: spendDay(3, 9), MR: 5, MRState: "opened"})
	f.metered(ar, "model-a", "2.000000")
	br := f.run(spendRun{Owner: b.User, Repo: ra, At: utcAt(2026, 1, 5, 9, 0, 0), MR: 5, MRState: "opened"})

	call := func() spendResult { return f.call(spendCall{Owner: a.User, Now: spendNow}) }
	base := call()
	wantRatio(t, "baseline PR ratio", base.Doc.CostPerMergedPR.Current, "2.000000", 0, "", store.AnalyticsReasonNoMergedPRs)

	// Admin-all scope (restricted to this repo so unrelated tests cannot interfere).
	allScope := func() spendResult { return f.call(spendCall{Now: spendNow, Repo: ra}) }
	wantRatio(t, "all-scope baseline", allScope().Doc.CostPerMergedPR.Current, "2.000000", 0, "", store.AnalyticsReasonNoMergedPRs)

	mustExec(f.ctx, t, f.pool, `UPDATE runs SET mr_state = 'merged' WHERE id = $1`, br)
	after := call()
	if !bytes.Equal(base.Raw, after.Raw) {
		t.Errorf("toggling a foreign owner's witness changed A's document:\n before %s\n after  %s", base.Raw, after.Raw)
	}
	// Admin-all has no owner predicate: B's witness counts.
	wantRatio(t, "all-scope with B's witness", allScope().Doc.CostPerMergedPR.Current, "2.000000", 1, "2.000000", "")

	// A's own merged run from before fold_from is a witness (not window-limited).
	f.run(spendRun{Owner: a.User, Repo: ra, At: utcAt(2026, 6, 1, 9, 0, 0), MR: 5, MRState: "merged"})
	withOwn := call()
	wantRatio(t, "A's own pre-fold witness", withOwn.Doc.CostPerMergedPR.Current, "2.000000", 1, "2.000000", "")
}

// witnessScopes runs fn once per statement: the owner statement and the all-users
// statement (no owner, isolated by the repo filter).
func witnessScopes(t *testing.T, o spendOwner, repo uuid.UUID, fn func(t *testing.T, c spendCall)) {
	t.Helper()
	t.Run("owner", func(t *testing.T) { fn(t, spendCall{Owner: o.User, Now: spendNow, Repo: repo}) })
	t.Run("all", func(t *testing.T) { fn(t, spendCall{Now: spendNow, Repo: repo}) })
}

// The merge witness decides only whether a selected PR merged; it ignores the harness filter.
func TestAnalyticsSpendWitnessIgnoresHarnessFilterLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("witharn")
	repo, _ := f.repo(o, "witharn")

	issue := f.run(spendRun{Owner: o.User, Repo: repo, Harness: "claude", At: spendDay(3, 9), MR: 61, MRState: "opened"})
	f.metered(issue, "model-a", "2.000000")
	other := f.run(spendRun{Owner: o.User, Repo: repo, Harness: "codex", At: spendDay(4, 9), MR: 61, MRState: "merged"})
	f.metered(other, "model-a", "7.000000")

	witnessScopes(t, o, repo, func(t *testing.T, c spendCall) {
		c.Harness = "claude"
		res := f.call(c)
		wantRatio(t, "cost_per_merged_pr", res.Doc.CostPerMergedPR.Current, "2.000000", 1, "2.000000", "")
		wantDollars(t, "totals.current.cost", res.Doc.Totals.Current.Cost, "2.000000")
		if res.Doc.Totals.Current.Runs != 1 {
			t.Errorf("totals.current.runs = %d, want 1", res.Doc.Totals.Current.Runs)
		}
	})
}

// The merge witness also ignores the kind filter: a rework's merged state counts for the
// issue run's PR while the rework's own cost stays out of the selection.
func TestAnalyticsSpendWitnessIgnoresKindFilterLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("witkind")
	repo, _ := f.repo(o, "witkind")

	issue := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(3, 9), MR: 62, MRState: "opened"})
	f.metered(issue, "model-a", "2.000000")
	rw := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.MRRework, Target: issue, At: spendDay(4, 9), MR: 62, MRState: "merged"})
	f.metered(rw, "model-a", "5.000000")

	witnessScopes(t, o, repo, func(t *testing.T, c spendCall) {
		c.Kinds = []string{runkind.Issue}
		res := f.call(c)
		wantRatio(t, "cost_per_merged_pr", res.Doc.CostPerMergedPR.Current, "2.000000", 1, "2.000000", "")
		wantDollars(t, "totals.current.cost", res.Doc.Totals.Current.Cost, "2.000000")
		if res.Doc.Totals.Current.Runs != 1 {
			t.Errorf("totals.current.runs = %d, want 1", res.Doc.Totals.Current.Runs)
		}
	})
}

func TestAnalyticsSpendPRAttributionWithinWindowLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("prattr")
	repo, _ := f.repo(o, "prattr")

	// One merged PR with Delivery runs on day 3 ($1) and day 9 ($4); 30d, from = day 1.
	r3 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(3, 9), MR: 21, MRState: "opened"})
	f.metered(r3, "model-a", "1.000000")
	r9 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(9, 9), MR: 21, MRState: "merged"})
	f.metered(r9, "model-a", "4.000000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow})
	pr := res.Doc.CostPerMergedPR
	d := func(k int) analytics.Date { return analytics.DateOf(spendDay(k, 0), time.UTC) }

	// Panel: the PR's whole current-window cost lands in the earliest run's bucket.
	c3 := bucketCell(t, res, pr, d(3))
	wantRatio(t, "day 3 bucket", c3.AnalyticsRatioStat, "5.000000", 1, "5.000000", "")
	wantDisclosure(t, "day 3 bucket", c3.Disclosure, 2, 0, 0, 0, false) // the $4 run's disclosure sits here
	c9 := bucketCell(t, res, pr, d(9))
	wantRatio(t, "day 9 bucket", c9.AnalyticsRatioStat, "0", 0, "", store.AnalyticsReasonNoMergedPRs)
	wantRatio(t, "headline", pr.Current, "5.000000", 1, "5.000000", "")
	var cellSum []string
	var cellDen int64
	for _, c := range pr.Buckets {
		cellSum = append(cellSum, c.Numerator)
		cellDen += c.Denominator
	}
	wantRatSum(t, "sum of bucket numerators", sumDollars(t, cellSum...), "5.000000")
	if cellDen != 1 {
		t.Errorf("sum of bucket denominators = %d, want 1", cellDen)
	}

	// Rolling: the day 3 point (R = days -3..3) deliberately carries day 9's $4; the day 9
	// point (R = days 3..9) holds the PR; the day 10 point (R = days 4..10) no longer does.
	r := rollCell(t, res, pr, d(3))
	wantRatio(t, "day 3 point", r.AnalyticsRatioStat, "5.000000", 1, "5.000000", "")
	r = rollCell(t, res, pr, d(9))
	wantRatio(t, "day 9 point", r.AnalyticsRatioStat, "5.000000", 1, "5.000000", "")
	r = rollCell(t, res, pr, d(10))
	wantRatio(t, "day 10 point", r.AnalyticsRatioStat, "0", 0, "", store.AnalyticsReasonNoMergedPRs)
}

func TestAnalyticsSpendPRAttributionAcrossFromLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("prfrom")
	repo, _ := f.repo(o, "prfrom")

	// One merged PR: a Delivery run on from-2 ($2) and one on day 1 ($3).
	lookback := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(1, 0).AddDate(0, 0, -2).Add(9 * time.Hour), MR: 31, MRState: "opened"})
	f.metered(lookback, "model-a", "2.000000")
	cur := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(1, 9), MR: 31, MRState: "merged"})
	f.metered(cur, "model-a", "3.000000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow})
	pr := res.Doc.CostPerMergedPR
	d := func(k int) analytics.Date { return analytics.DateOf(spendDay(k, 0), time.UTC) }

	// Panel and headline: the current window only; the previous headline over [previous_from, from).
	wantRatio(t, "day 1 bucket", bucketCell(t, res, pr, d(1)).AnalyticsRatioStat, "3.000000", 1, "3.000000", "")
	wantRatio(t, "headline current", pr.Current, "3.000000", 1, "3.000000", "")
	if pr.Previous == nil {
		t.Fatal("no previous headline")
	}
	wantRatio(t, "headline previous", *pr.Previous, "2.000000", 1, "2.000000", "")

	// Rolling: attributed to from-2 over the whole fold range with numerator 5 / denominator 1.
	wantRatio(t, "day 1 point", rollCell(t, res, pr, d(1)).AnalyticsRatioStat, "5.000000", 1, "5.000000", "")
	// Day 6's range (days 0..6) excludes from-2.
	wantRatio(t, "day 6 point", rollCell(t, res, pr, d(6)).AnalyticsRatioStat, "0", 0, "", store.AnalyticsReasonNoMergedPRs)
}

func TestAnalyticsSpendPreviousCurrentBoundaryLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("bound")
	repo, _ := f.repo(o, "bound")

	// A $3 issue run in the previous window and a $2 rework in the current one.
	issue := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 10, 20, 12, 0, 0), Finished: utcAt(2026, 10, 20, 13, 0, 0)})
	f.metered(issue, "model-a", "3.000000")
	rework := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.MRRework, Target: issue, At: spendDay(2, 9), Finished: spendDay(2, 10)})
	f.metered(rework, "model-a", "2.000000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow})
	cpr := res.Doc.CostPerCompletedRun
	if cpr.Previous == nil {
		t.Fatal("no previous headline")
	}
	wantRatio(t, "previous", *cpr.Previous, "3.000000", 1, "3.000000", "")
	wantRatio(t, "current", cpr.Current, "2.000000", 1, "2.000000", "")
}

func TestAnalyticsSpendBoundaryInstantsLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("inst")
	repo, _ := f.repo(o, "inst")
	// Half-open ranges: a run exactly at from is current, one a second earlier is previous,
	// and one exactly at now is in no total.
	atFrom := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 10, 27, 0, 0, 0)})
	f.metered(atFrom, "model-a", "1.000000")
	beforeFrom := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 10, 26, 23, 59, 59)})
	f.metered(beforeFrom, "model-a", "4.000000")
	atNow := f.run(spendRun{Owner: o.User, Repo: repo, At: spendNow})
	f.metered(atNow, "model-a", "8.000000")
	justBeforeNow := f.run(spendRun{Owner: o.User, Repo: repo, At: spendNow.Add(-time.Second)})
	f.metered(justBeforeNow, "model-a", "0.500000")
	atPrevFrom := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 9, 27, 0, 0, 0)})
	f.metered(atPrevFrom, "model-a", "0.250000")
	beforePrevFrom := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 9, 26, 23, 59, 59)})
	f.metered(beforePrevFrom, "model-a", "16.000000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow})
	wantDollars(t, "current", res.Doc.Totals.Current.Cost, "1.500000")
	wantDollars(t, "previous", res.Doc.Totals.Previous.Cost, "4.250000")
	for _, tr := range res.Doc.TopRuns {
		if tr.RunID == atNow || tr.RunID == beforePrevFrom {
			t.Errorf("run %s is outside every range but appears in the top runs", tr.RunID)
		}
	}
	// The last bucket is [today, now): the run one second before now is in it.
	last := res.Doc.Buckets[len(res.Doc.Buckets)-1]
	wantDollars(t, "last bucket", last.Cost, "0.500000")
}

func TestAnalyticsSpendPRIdentityLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("pridn")
	repo, _ := f.repo(o, "pridn")

	// PR 51: an issue run whose own state is stale ('opened'), a failed attempt, a prompt run,
	// and a rework carrying the merged state. The helper's state decides, not the issue run's;
	// the PR counts once (denominator 1) and its numerator includes every Delivery run.
	i51 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(3, 9), MR: 51, MRState: "opened"})
	f.metered(i51, "model-a", "2.000000")
	failed51 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(3, 7), Status: "failed", FailOrigin: "agent_failure", MR: 51})
	f.metered(failed51, "model-a", "1.000000")
	prompt51 := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.Prompt, At: spendDay(4, 9), MR: 51, MRState: "opened"})
	f.metered(prompt51, "model-a", "0.500000")
	rw51 := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.MRRework, Target: i51, At: spendDay(5, 9), MR: 51, MRState: "merged"})
	f.metered(rw51, "model-a", "0.250000")
	// PR 52: merged on the issue run, a ci_fix helper left with a stale NULL state.
	i52 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(6, 9), MR: 52, MRState: "merged"})
	f.metered(i52, "model-a", "5.000000")
	ci52 := f.run(spendRun{Owner: o.User, Repo: repo, Kind: runkind.CIFix, At: spendDay(7, 9), MR: 52})
	f.metered(ci52, "model-a", "0.125000")
	// PR 53: referenced but never merged. Its cost is in the numerator (every Delivery run
	// carrying a selected PR); it adds no denominator unit.
	i53 := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(8, 9), MR: 53, MRState: "opened"})
	f.metered(i53, "model-a", "10.000000")
	// A run without a PR is outside this ratio.
	plain := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(8, 10)})
	f.metered(plain, "model-a", "100.000000")

	res := f.call(spendCall{Owner: o.User, Now: spendNow, Breakdown: "repo"})
	pr := res.Doc.CostPerMergedPR
	wantRatio(t, "headline", pr.Current, "18.875000", 2, "9.437500", "")
	d := func(k int) analytics.Date { return analytics.DateOf(spendDay(k, 0), time.UTC) }
	// PR 51 is attributed to day 3 (its earliest run, the failed attempt at 07:00).
	wantRatio(t, "day 3 bucket", bucketCell(t, res, pr, d(3)).AnalyticsRatioStat, "3.750000", 1, "3.750000", "")
	wantRatio(t, "day 6 bucket", bucketCell(t, res, pr, d(6)).AnalyticsRatioStat, "5.125000", 1, "5.125000", "")
	// PR 53's cost lands in its bucket with no denominator.
	wantRatio(t, "day 8 bucket", bucketCell(t, res, pr, d(8)).AnalyticsRatioStat, "10.000000", 0, "", store.AnalyticsReasonNoMergedPRs)
	// The breakdown counts distinct witnessed merged PRs.
	if len(res.Doc.Breakdown.Rows) != 1 || res.Doc.Breakdown.Rows[0].MergedPRs != 2 {
		t.Errorf("breakdown merged_prs = %+v, want 2", res.Doc.Breakdown.Rows)
	}
}

func TestAnalyticsSpendCrossOwnerDuplicatePRLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	// Two owners each connect the same external project (same forge_project_id on two
	// connections): two repos, so the same external PR is two identities. A year no other
	// test uses keeps the all-scope cohort to this test's runs.
	f.claimYear(2003)
	now := utcAt(2003, 6, 15, 12, 0, 0)
	a, b := f.owner("dup-a"), f.owner("dup-b")
	// The same external project and the same mr_iid, each on the owner's own connection.
	project := spendSeq.Add(1) + 9_500_000
	ra, _ := f.repoWithProject(a, "dup-a", project)
	rb, _ := f.repoWithProject(b, "dup-b", project)
	if a.Conn == b.Conn {
		t.Fatal("test setup: both owners share one connection")
	}
	run := func(o spendOwner, r uuid.UUID, cost string) {
		id := f.run(spendRun{Owner: o.User, Repo: r, At: utcAt(2003, 6, 10, 9, 0, 0), MR: 77, MRState: "merged"})
		f.metered(id, "model-a", cost)
	}
	run(a, ra, "2.000000")
	run(b, rb, "3.000000")

	wantRatio(t, "owner A", f.call(spendCall{Owner: a.User, Now: now}).Doc.CostPerMergedPR.Current, "2.000000", 1, "2.000000", "")
	wantRatio(t, "owner B", f.call(spendCall{Owner: b.User, Now: now}).Doc.CostPerMergedPR.Current, "3.000000", 1, "3.000000", "")
	// Admin-all counts the duplicate twice.
	all := f.call(spendCall{Now: now})
	wantRatio(t, "all scope", all.Doc.CostPerMergedPR.Current, "5.000000", 2, "2.500000", "")
}

func TestAnalyticsSpendEmptyBucketsLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("empty")
	repo, _ := f.repo(o, "empty")
	now := spendNow
	d2 := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 11, 20, 9, 0, 0)})
	f.metered(d2, "model-a", "1.000000")
	d6 := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2026, 11, 24, 9, 0, 0)})
	f.metered(d6, "model-a", "2.000000")

	res := f.call(spendCall{Owner: o.User, Now: now, Window: analytics.Window7d})
	d := res.Doc
	if len(d.Buckets) != 7 || len(res.Bounds.Buckets) != 7 {
		t.Fatalf("7d window emitted %d buckets", len(d.Buckets))
	}
	for i, c := range d.Buckets {
		if c.Index != i {
			t.Errorf("bucket %d has index %d", i, c.Index)
		}
		switch i {
		case 1:
			wantDollars(t, "bucket 1", c.Cost, "1.000000")
		case 5:
			wantDollars(t, "bucket 5", c.Cost, "2.000000")
		default:
			wantDollars(t, "empty bucket", c.Cost, "0")
			if c.Runs != 0 || c.Coverage.Value != nil || c.Coverage.Reason == nil || *c.Coverage.Reason != store.AnalyticsReasonNoApplicableRuns {
				t.Errorf("empty bucket %d: %+v", i, c)
			}
		}
	}
	for name, p := range map[string]store.AnalyticsRatioPanel{"cpr": d.CostPerCompletedRun, "pr": d.CostPerMergedPR} {
		if len(p.Buckets) != 7 || len(p.Rolling) != 7 {
			t.Errorf("%s: %d cells, %d points", name, len(p.Buckets), len(p.Rolling))
		}
	}
	if len(d.Split.Groups) != 1 || len(d.Split.Groups[0].Buckets) != 7 {
		t.Errorf("split groups %+v", d.Split.Groups)
	}
	wantRatio(t, "pr headline", d.CostPerMergedPR.Current, "0", 0, "", store.AnalyticsReasonNoMergedPRs)
	if d.CacheRead.Value == nil {
		t.Error("cache-read value is null with input tokens present")
	}
}

func TestAnalyticsSpendPrimaryModelTieBreakLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("prim")
	repo, _ := f.repo(o, "prim")

	// Equal output tokens: the lexically smallest model (byte order) wins, whatever the
	// insertion order. "Zulu" < "alpha" in byte order.
	tie := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(3, 9)})
	f.usage(tie, "alpha", "metered", "1.000000", 10, 0, 0, 100)
	f.usage(tie, "Zulu", "metered", "1.000000", 10, 0, 0, 100)
	f.usage(tie, "mid", "metered", "1.000000", 10, 0, 0, 50)
	most := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(4, 9)})
	f.usage(most, "beta", "metered", "1.000000", 10, 0, 0, 90)
	f.usage(most, "gamma", "metered", "1.000000", 10, 0, 0, 100)
	none := f.run(spendRun{Owner: o.User, Repo: repo, At: spendDay(5, 9)})

	res := f.call(spendCall{Owner: o.User, Now: spendNow, Breakdown: "primary_model"})
	got := map[uuid.UUID]string{}
	for _, tr := range res.Doc.TopRuns {
		got[tr.RunID] = tr.PrimaryModel
	}
	if got[tie] != "Zulu" || got[most] != "gamma" || got[none] != "unknown" {
		t.Errorf("primary models %v, want tie=Zulu most=gamma none=unknown", got)
	}
	// Each run is counted once in the primary_model breakdown.
	var runs int64
	for _, r := range res.Doc.Breakdown.Rows {
		runs += r.Runs
	}
	if runs != 3 {
		t.Errorf("breakdown runs = %d, want 3 (each run once)", runs)
	}
	for _, k := range []string{"Zulu", "gamma", "unknown"} {
		if r := rowByKey(t, res.Doc.Breakdown.Rows, k); r.Runs != 1 {
			t.Errorf("row %s runs = %d", k, r.Runs)
		}
	}
}

func TestAnalyticsSpendBreakdownCapLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("cap")
	repo, _ := f.repo(o, "cap")
	at := spendDay(3, 9)
	add := func(i int, model string, cost int, elapsed time.Duration) {
		start := at.Add(time.Duration(i) * time.Minute)
		r := f.run(spendRun{Owner: o.User, Repo: repo, At: start, Finished: start.Add(elapsed)})
		f.metered(r, model, decimalCost(cost))
	}
	// 48 groups of distinct cost (200 - i), then three groups TIED at cost 100 (t1, t2, t3):
	// the key breaks the tie ascending, so t1 and t2 take ranks 49 and 50 and t3 falls into
	// Other. Three cheaper groups follow, so Other holds four runs. Their elapsed times are
	// skewed (1, 2, 10 and 100 minutes) so the direct population percentiles (6 minutes at
	// p50, 73 minutes at p90) differ from the mean of the per-group values (28.25 minutes).
	var want []string
	for i := 0; i < 48; i++ {
		key := fmt.Sprintf("pm-%02d", i)
		want = append(want, key)
		add(i, key, 200-i, time.Hour)
	}
	add(48, "pm-t3", 100, 60*time.Second)
	add(49, "pm-t1", 100, time.Hour)
	add(50, "pm-t2", 100, time.Hour)
	want = append(want, "pm-t1", "pm-t2")
	add(51, "pm-u1", 10, 2*time.Minute)
	add(52, "pm-u2", 9, 10*time.Minute)
	add(53, "pm-u3", 8, 100*time.Minute)

	res := f.call(spendCall{Owner: o.User, Now: spendNow, Breakdown: "primary_model"})
	rows := res.Doc.Breakdown.Rows
	if len(rows) != 51 {
		t.Fatalf("breakdown rows = %d, want 50 plus Other", len(rows))
	}
	var keys []string
	for _, r := range rows[:50] {
		if r.IsOther || r.Key == nil {
			t.Fatalf("a kept row is Other or keyless: %+v", r)
		}
		keys = append(keys, *r.Key)
	}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("kept rows are not the 50 highest-cost groups in cost order with the key tie-break:\n got %v\nwant %v", keys, want)
	}
	other := rows[50]
	if !other.IsOther || other.Key != nil || other.Rank != 51 || other.Runs != 4 || other.Finished != 4 {
		t.Errorf("Other row = %+v", other)
	}
	for _, c := range []struct {
		what string
		got  *float64
		want float64
	}{
		{"Other p50 seconds", other.ElapsedP50Seconds.Value, 360},
		{"Other p90 seconds", other.ElapsedP90Seconds.Value, 4380},
	} {
		if c.got == nil {
			t.Errorf("%s is NULL, want %v", c.what, c.want)
		} else if math.Abs(*c.got-c.want) > 1e-6 {
			t.Errorf("%s = %v, want %v (a direct percentile over the four Other runs)", c.what, *c.got, c.want)
		}
	}
	var costs []string
	for _, r := range rows {
		costs = append(costs, r.Cost)
	}
	wantDollars(t, "sum of row costs", sumDollars(t, costs...).FloatString(6), res.Doc.Totals.Current.Cost)
}

func decimalCost(n int) string {
	return big.NewRat(int64(n), 1).FloatString(6)
}

func TestAnalyticsSpendAllScopeUserSplitAndKindsLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	// A year of its own keeps the all-scope cohort to this test's runs.
	f.claimYear(2005)
	now := utcAt(2005, 6, 15, 12, 0, 0)
	a, b := f.owner("usplit-a"), f.owner("usplit-b")
	ra, _ := f.repo(a, "usplit")
	rb, rbPath := f.repo(b, "usplit")
	ar := f.run(spendRun{Owner: a.User, Repo: ra, At: utcAt(2005, 6, 10, 9, 0, 0)})
	f.metered(ar, "model-a", "2.500000")
	br := f.run(spendRun{Owner: b.User, Repo: rb, At: utcAt(2005, 6, 11, 9, 0, 0)})
	f.metered(br, "model-a", "9.750000")
	// A judge run is a Supporting kind: in totals, out of every Delivery ratio.
	jd := f.run(spendRun{Owner: b.User, Kind: runkind.Judge, Target: br, At: utcAt(2005, 6, 11, 10, 0, 0)})
	f.metered(jd, "model-a", "0.500000")
	ch := f.run(spendRun{Owner: b.User, Kind: runkind.Chat, At: utcAt(2005, 6, 11, 11, 0, 0)})
	f.metered(ch, "model-a", "5.000000")

	res := f.call(spendCall{Now: now, Split: "user", Breakdown: "user"})
	d := res.Doc
	wantDollars(t, "all-scope total", d.Totals.Current.Cost, "12.750000")
	ga, gb := groupByKey(t, d.Split.Groups, a.User.String()), groupByKey(t, d.Split.Groups, b.User.String())
	if ga.Rank != 2 || gb.Rank != 1 || ga.ID == nil || *ga.ID != a.User || ga.Label == nil || *ga.Label != "spend-usplit-a-"+a.User.String()+"@e2e" {
		t.Errorf("user groups: a %+v b %+v", ga, gb)
	}
	wantDollars(t, "b group", gb.Totals.Cost, "10.250000")
	if d.Split.NonAdditive {
		t.Error("a user split is additive")
	}
	if d.UntrackedChatRuns != 1 {
		t.Errorf("untracked chat runs = %d", d.UntrackedChatRuns)
	}
	if r := rowByKey(t, d.Breakdown.Rows, b.User.String()); r.Runs != 2 {
		t.Errorf("breakdown row for b has %d runs, want 2 (chat excluded)", r.Runs)
	}
	// The all-scope statement always reveals repos: b's repo is visible in the top runs.
	var seen bool
	for _, tr := range d.TopRuns {
		if tr.RunID == br && tr.RepoID != nil && *tr.RepoID == rb && tr.RepoPath != nil && *tr.RepoPath == rbPath {
			seen = true
		}
	}
	if !seen {
		t.Error("all-scope top runs do not reveal the repo")
	}
	// Delivery ratios ignore the judge: cost per completed run divides all recorded cost
	// (12.75) by the two completed Delivery runs.
	wantRatio(t, "cost per completed run", d.CostPerCompletedRun.Current, "12.750000", 2, "6.375000", "")

	// kind=supporting: only the judge is applicable (chat is untracked); no Delivery runs.
	sup := f.call(spendCall{Now: now, Kinds: runkind.Supporting()})
	wantDollars(t, "supporting total", sup.Doc.Totals.Current.Cost, "0.500000")
	if sup.Doc.Totals.Current.Runs != 1 || sup.Doc.UntrackedChatRuns != 1 {
		t.Errorf("supporting: runs %d untracked %d", sup.Doc.Totals.Current.Runs, sup.Doc.UntrackedChatRuns)
	}
	wantRatio(t, "supporting cost per completed run", sup.Doc.CostPerCompletedRun.Current, "0.500000", 0, "", store.AnalyticsReasonNoCompletedDeliveryRuns)
}

// TestAnalyticsSpendNullRevealFailsClosedLiveDB: the visibility gate is fail-closed. The
// typed method cannot pass NULL for @reveal_repos, so the statement text is executed with the
// generated argument list and the flag replaced by NULL: the foreign repo must stay masked,
// exactly as with false.
func TestAnalyticsSpendNullRevealFailsClosedLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	u, v := f.owner("null-u"), f.owner("null-v")
	ru, _ := f.repo(u, "null")
	rv, rvPath := f.repo(v, "null")
	own := f.run(spendRun{Owner: u.User, Repo: ru, At: spendDay(3, 9)})
	f.metered(own, "model-a", "1.000000")
	foreign := f.run(spendRun{Owner: u.User, Repo: rv, At: spendDay(4, 9)})
	f.metered(foreign, "model-a", "7.654321")

	args := f.ownerArgs(spendCall{Owner: u.User, Now: spendNow, Split: "repo", Breakdown: "repo"})
	var replaced int
	for i, a := range args {
		if _, ok := a.(bool); ok {
			args[i] = nil // SQL NULL
			replaced++
		}
	}
	if replaced != 1 {
		t.Fatalf("expected exactly one boolean argument (@reveal_repos), found %d", replaced)
	}
	var raw []byte
	if err := f.pool.QueryRow(f.ctx, store.AnalyticsOwnerSpendSQL, args...).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(rv.String())) || bytes.Contains(raw, []byte(rvPath)) {
		t.Errorf("a NULL reveal flag exposed the foreign repo: %s", raw)
	}
	doc, err := store.DecodeAnalyticsSpendDoc(raw)
	if err != nil {
		t.Fatal(err)
	}
	wantDollars(t, "inaccessible_repo cost", rowByKey(t, doc.Breakdown.Rows, "inaccessible_repo").Cost, "7.654321")
}
