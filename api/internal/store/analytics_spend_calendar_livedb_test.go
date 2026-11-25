package store_test

import (
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4684 M3: the Spend statements against the real calendar. The rolling fixtures are
// the m2 bound fixtures (a)-(e), whose ratio inputs this milestone pins.

func TestAnalyticsSpendRollingFixtureInputsLiveDB(t *testing.T) {
	type fixture struct {
		name   string
		zone   string
		window analytics.Window
		now    time.Time
		pin    time.Time // earliest run: fixes history_start
		label  analytics.Date
		inside time.Time // a merged-PR Delivery run on the bucket's last covered date L
	}
	fixtures := []fixture{
		{"a one-week bucket", "UTC", analytics.WindowAll, utcAt(2026, 11, 25, 12, 0, 0), utcAt(2026, 9, 9, 8, 0, 0),
			analytics.Date{Year: 2026, Month: 10, Day: 12}, utcAt(2026, 10, 18, 12, 0, 0)},
		// The outside run sits in the bucket's first week, so deriving R0 from the label date
		// would include it and fail the ratio.
		{"b multiweek bucket", "UTC", analytics.WindowAll, utcAt(2026, 11, 25, 12, 0, 0), utcAt(2023, 1, 2, 9, 0, 0),
			analytics.Date{Year: 2025, Month: 2, Day: 24}, utcAt(2025, 3, 9, 12, 0, 0)},
		{"c skipped date", "Pacific/Apia", analytics.Window7d, utcAt(2012, 1, 3, 0, 0, 0), utcAt(2011, 11, 1, 0, 0, 0),
			analytics.Date{Year: 2011, Month: 12, Day: 31}, utcAt(2011, 12, 31, 0, 0, 0)},
		{"d point ending at now", "UTC", analytics.Window7d, utcAt(2026, 11, 25, 12, 0, 0), utcAt(2026, 10, 1, 0, 0, 0),
			analytics.Date{Year: 2026, Month: 11, Day: 25}, utcAt(2026, 11, 25, 6, 0, 0)},
		{"e now at a local midnight", "Europe/Bucharest", analytics.Window7d, utcAt(2026, 11, 24, 22, 0, 0), utcAt(2026, 10, 1, 0, 0, 0),
			analytics.Date{Year: 2026, Month: 11, Day: 25}, utcAt(2026, 11, 24, 12, 0, 0)},
	}
	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			f := newSpendFixture(t)
			o := f.owner("roll")
			repo, _ := f.repo(o, "roll")
			loc, err := time.LoadLocation(fx.zone)
			if err != nil {
				t.Fatal(err)
			}
			b0, err := analytics.ComputeBounds(fx.now, loc, fx.window, &fx.pin)
			if err != nil {
				t.Fatal(err)
			}
			idx := -1
			for i, bk := range b0.Buckets {
				if bk.Label == fx.label {
					idx = i
				}
			}
			if idx < 0 {
				t.Fatalf("no bucket labelled %s", fx.label)
			}
			r0 := b0.Rolling[idx].UnclippedStart
			outside := r0.Add(-time.Second)
			if !fx.inside.After(r0) || !fx.inside.Before(b0.Rolling[idx].End) {
				t.Fatalf("inside run %s is not in R0 [%s, %s)", fx.inside, r0, b0.Rolling[idx].End)
			}

			// The pin is a cancelled Delivery run with no usage and no PR: it fixes the
			// history start and adds neither cost nor a denominator unit.
			f.run(spendRun{Owner: o.User, Repo: repo, At: fx.pin, Status: "cancelled"})
			in := f.run(spendRun{Owner: o.User, Repo: repo, At: fx.inside, MR: 101, MRState: "merged"})
			f.metered(in, "model-a", "2.500000")
			out := f.run(spendRun{Owner: o.User, Repo: repo, At: outside, MR: 102, MRState: "merged"})
			f.metered(out, "model-a", "7.000000")

			res := f.call(spendCall{Owner: o.User, Now: fx.now, Zone: fx.zone, Window: fx.window})
			if got := res.Bounds.Rolling[idx].UnclippedStart; !got.Equal(r0) {
				t.Fatalf("R0 moved: %s != %s", got, r0)
			}
			p := res.Doc.CostPerMergedPR.Rolling[idx]
			wantRatio(t, "pr point", p.AnalyticsRatioStat, "2.500000", 1, "2.500000", "")
			c := res.Doc.CostPerCompletedRun.Rolling[idx]
			wantRatio(t, "completed-run point", c.AnalyticsRatioStat, "2.500000", 1, "2.500000", "")
			wantDisclosure(t, "pr point", p.Disclosure, 1, 0, 0, 0, false)

			// One second earlier the outside run was inside: the previous start is the cut.
			if fx.name == "e now at a local midnight" {
				// Today's empty bucket is emitted; its panel cell is 0/0, while its point
				// carries the PR seeded on 2026-11-24 local.
				last := len(res.Doc.CostPerMergedPR.Buckets) - 1
				if last != idx {
					t.Fatalf("today is bucket %d, not the last (%d)", idx, last)
				}
				wantRatio(t, "today's panel cell", res.Doc.CostPerMergedPR.Buckets[idx].AnalyticsRatioStat, "0", 0, "", store.AnalyticsReasonNoMergedPRs)
				if res.Bounds.Buckets[idx].Start != res.Bounds.Buckets[idx].End {
					t.Errorf("today's bucket is not empty: %+v", res.Bounds.Buckets[idx])
				}
				// The bucket before today ends at now too, but its R0 starts a day earlier and
				// so also holds the outside run.
				prev := res.Doc.CostPerMergedPR.Rolling[idx-1]
				wantRatio(t, "previous bucket's point", prev.AnalyticsRatioStat, "9.500000", 2, "4.750000", "")
			}
		})
	}
}

// bucketIndexOf is the independent (Go-side) half-open membership the SQL result is
// compared with.
func bucketIndexOf(b analytics.Bounds, t time.Time) int {
	for i, bk := range b.Buckets {
		if !t.Before(bk.Start) && t.Before(bk.End) {
			return i
		}
	}
	return -1
}

func TestAnalyticsSpendBucketsAroundZoneTransitionsLiveDB(t *testing.T) {
	cases := []struct {
		zone string
		date analytics.Date // the transition date
		now  time.Time
	}{
		{"Europe/Bucharest", analytics.Date{Year: 2026, Month: 10, Day: 25}, utcAt(2026, 10, 27, 12, 0, 0)},
		{"America/Santiago", analytics.Date{Year: 2022, Month: 9, Day: 11}, utcAt(2022, 9, 13, 12, 0, 0)},
		{"America/Havana", analytics.Date{Year: 2022, Month: 11, Day: 6}, utcAt(2022, 11, 8, 12, 0, 0)},
		{"America/Sao_Paulo", analytics.Date{Year: 2018, Month: 2, Day: 18}, utcAt(2018, 2, 20, 12, 0, 0)},
		{"Pacific/Apia", analytics.Date{Year: 2011, Month: 12, Day: 30}, utcAt(2012, 1, 1, 12, 0, 0)},
	}
	for _, c := range cases {
		t.Run(c.zone, func(t *testing.T) {
			f := newSpendFixture(t)
			o := f.owner("zone")
			repo, _ := f.repo(o, "zone")
			loc, err := time.LoadLocation(c.zone)
			if err != nil {
				t.Fatal(err)
			}
			b0, err := analytics.ComputeBounds(c.now, loc, analytics.Window7d, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Runs at start-1s, start and start+1s of every bucket within a day of the
			// transition date, each with a distinct cost.
			type seeded struct {
				at   time.Time
				cost string
			}
			var runs []seeded
			k := 0
			for _, bk := range b0.Buckets {
				if bk.Label.DaysUntil(c.date) > 1 || c.date.DaysUntil(bk.Label) > 1 {
					continue
				}
				for _, at := range []time.Time{bk.Start.Add(-time.Second), bk.Start, bk.Start.Add(time.Second)} {
					k++
					cost := big.NewRat(int64(k), 1000).FloatString(6)
					id := f.run(spendRun{Owner: o.User, Repo: repo, At: at})
					f.metered(id, "model-a", cost)
					runs = append(runs, seeded{at, cost})
				}
			}
			if len(runs) < 6 {
				t.Fatalf("only %d boundary runs seeded", len(runs))
			}
			want := make([][]string, len(b0.Buckets))
			for _, r := range runs {
				if i := bucketIndexOf(b0, r.at); i >= 0 {
					want[i] = append(want[i], r.cost)
				}
			}

			base := f.call(spendCall{Owner: o.User, Now: c.now, Zone: c.zone, Window: analytics.Window7d})
			if len(base.Doc.Buckets) != len(b0.Buckets) {
				t.Fatalf("%d cells for %d buckets", len(base.Doc.Buckets), len(b0.Buckets))
			}
			for i, cell := range base.Doc.Buckets {
				wantRatSum(t, fmt.Sprintf("bucket %d (%s)", i, b0.Buckets[i].Label), ratOf(t, cell.Cost), sumDollars(t, append([]string{"0"}, want[i]...)...).FloatString(6))
			}

			// The document does not depend on the session TimeZone.
			for _, tz := range []string{"UTC", "Asia/Tokyo", "America/Los_Angeles", "Pacific/Apia", c.zone} {
				got := f.call(spendCall{Owner: o.User, Now: c.now, Zone: c.zone, Window: analytics.Window7d, SessionTZ: tz})
				if string(got.Raw) != string(base.Raw) {
					t.Errorf("session TimeZone %s changed the document", tz)
				}
			}
		})
	}
}

// TestAnalyticsSpendBucharestTransitionPinsLiveDB pins the 25-hour 2026-10-25 day in
// Europe/Bucharest to literal instants, independent of the calendar code under test.
func TestAnalyticsSpendBucharestTransitionPinsLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("buch")
	repo, _ := f.repo(o, "buch")
	now := utcAt(2026, 10, 27, 12, 0, 0)
	for _, at := range []time.Time{
		utcAt(2026, 10, 24, 20, 59, 59), // 23:59:59 on the 24th
		utcAt(2026, 10, 24, 21, 0, 0),   // 00:00 on the 25th
		utcAt(2026, 10, 25, 21, 59, 59), // 23:59:59 on the 25th (a 25-hour day)
		utcAt(2026, 10, 25, 22, 0, 0),   // 00:00 on the 26th
	} {
		id := f.run(spendRun{Owner: o.User, Repo: repo, At: at})
		f.metered(id, "model-a", "1.000000")
	}
	res := f.call(spendCall{Owner: o.User, Now: now, Zone: "Europe/Bucharest", Window: analytics.Window7d})
	cell := func(day int) store.AnalyticsSpendCell {
		for i, bk := range res.Bounds.Buckets {
			if bk.Label == (analytics.Date{Year: 2026, Month: 10, Day: day}) {
				return res.Doc.Buckets[i]
			}
		}
		t.Fatalf("no bucket for 2026-10-%d", day)
		return store.AnalyticsSpendCell{}
	}
	for _, day := range []int{24, 25, 26} {
		want := int64(1)
		if day == 25 {
			want = 2
		}
		if cell(day).Runs != want {
			t.Errorf("2026-10-%d has %d runs, want %d", day, cell(day).Runs, want)
		}
	}
	for i, bk := range res.Bounds.Buckets {
		if bk.Label == (analytics.Date{Year: 2026, Month: 10, Day: 25}) {
			if !bk.Start.Equal(utcAt(2026, 10, 24, 21, 0, 0)) || !bk.End.Equal(utcAt(2026, 10, 25, 22, 0, 0)) {
				t.Errorf("bucket %d = [%s, %s)", i, bk.Start, bk.End)
			}
		}
	}
}

// TestAnalyticsDayStartMatchesPostgresLiveDB cross-checks the pinned dayStart instants
// against PostgreSQL's own zone rules (the first instant of the local date).
func TestAnalyticsDayStartMatchesPostgresLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	pins := []struct {
		zone string
		date analytics.Date
		want time.Time
	}{
		{"Europe/Bucharest", analytics.Date{Year: 2026, Month: 10, Day: 25}, utcAt(2026, 10, 24, 21, 0, 0)},
		{"Europe/Bucharest", analytics.Date{Year: 2026, Month: 10, Day: 26}, utcAt(2026, 10, 25, 22, 0, 0)},
		{"America/Santiago", analytics.Date{Year: 2022, Month: 9, Day: 11}, utcAt(2022, 9, 11, 4, 0, 0)},
		{"America/Havana", analytics.Date{Year: 2022, Month: 11, Day: 6}, utcAt(2022, 11, 6, 4, 0, 0)},
		{"America/Sao_Paulo", analytics.Date{Year: 2018, Month: 2, Day: 18}, utcAt(2018, 2, 18, 3, 0, 0)},
		{"Pacific/Apia", analytics.Date{Year: 2011, Month: 12, Day: 31}, utcAt(2011, 12, 30, 10, 0, 0)},
	}
	for _, p := range pins {
		loc, err := time.LoadLocation(p.zone)
		if err != nil {
			t.Fatal(err)
		}
		got, err := analytics.DayStart(p.date, loc)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(p.want) {
			t.Errorf("DayStart(%s, %s) = %s, want %s", p.date, p.zone, got, p.want)
		}
		// PostgreSQL's own zone rules, independently of Go: the first minute whose local date
		// is >= the pinned date. (A bare timestamp AT TIME ZONE would resolve an ambiguous
		// midnight such as Havana's to the later occurrence.)
		var pg pgtype.Timestamptz
		if err := f.pool.QueryRow(f.ctx,
			`SELECT min(t) FROM generate_series($1::timestamptz, $2::timestamptz, interval '1 minute') t
			 WHERE (t AT TIME ZONE $3::text)::date >= $4::date`,
			p.want.Add(-30*time.Hour), p.want.Add(30*time.Hour), p.zone, p.date.String()).Scan(&pg); err != nil {
			t.Fatal(err)
		}
		if !pg.Time.Equal(p.want) {
			t.Errorf("PostgreSQL first instant of %s in %s = %s, want %s", p.date, p.zone, pg.Time.UTC(), p.want)
		}
	}
}

func numericRat(t *testing.T, n pgtype.Numeric) *big.Rat {
	t.Helper()
	if !n.Valid || n.NaN || n.InfinityModifier != pgtype.Finite {
		t.Fatalf("numeric %+v is not finite", n)
	}
	r := new(big.Rat).SetInt(n.Int)
	e := big.NewInt(10)
	e.Exp(e, big.NewInt(int64(absInt(int(n.Exp)))), nil)
	if n.Exp >= 0 {
		return r.Mul(r, new(big.Rat).SetInt(e))
	}
	return r.Quo(r, new(big.Rat).SetInt(e))
}

func absInt(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

func TestAnalyticsSpendParityWithSelfAggregatesLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	o := f.owner("parity")
	repo, _ := f.repo(o, "parity")
	base := utcAt(2026, 11, 1, 0, 0, 0)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Hour) }
	n := 0
	next := func(r spendRun) uuid.UUID {
		n++
		r.Owner = o.User
		r.At = at(n)
		if r.Finished.IsZero() && r.Status != "running" {
			r.Finished = at(n).Add(time.Minute)
		}
		return f.run(r)
	}
	// Issue, prompt, task, job, judge (supporting) and chat with stray usage, plus outcome-only runs.
	i1 := next(spendRun{Repo: repo})
	f.metered(i1, "model-a", "1.500000")
	p1 := next(spendRun{Repo: repo, Kind: runkind.Prompt})
	f.usage(p1, "model-a", "subscription", "0", 200, 100, 20, 40)
	t1 := next(spendRun{Repo: repo, Kind: runkind.Task})
	f.usage(t1, "model-b", "unreported", "0", 300, 0, 0, 60)
	j1 := next(spendRun{Kind: runkind.Job})
	f.metered(j1, "model-a", "0.750000")
	jd := next(spendRun{Kind: runkind.Judge, Target: i1})
	f.metered(jd, "model-a", "0.125000")
	ch := next(spendRun{Kind: runkind.Chat})
	f.metered(ch, "model-a", "9.990000")
	next(spendRun{Repo: repo, Status: "running"})                             // in flight: no outcome
	next(spendRun{Repo: repo, Status: "cancelled"})                           // cancelled
	next(spendRun{Repo: repo, Status: "failed", FailOrigin: "plan_rejected"}) // plan rejected
	next(spendRun{Repo: repo, Status: "failed", FailOrigin: "base_diverged"}) // base diverged
	next(spendRun{Repo: repo, Status: "failed", FailOrigin: "agent_failure"}) // failed
	next(spendRun{Repo: repo, Status: "failed", FailOrigin: "agent_failure"}) // failed
	nonChat := n - 1

	now := base.Add(24 * time.Hour * 3)
	res := f.call(spendCall{Owner: o.User, Now: now, Window: analytics.WindowAll, Breakdown: "kind"})
	d := res.Doc

	// SelfUsage lifetime totals: tokens, dollars and the subscription/unreported run counts
	// equal the Spend totals over the same population (window all covers every run).
	su, err := f.q.SelfUsage(f.ctx, o.User)
	if err != nil {
		t.Fatal(err)
	}
	cur := d.Totals.Current
	if cur.InputTokens != su.LifetimeInputTokens || cur.CacheReadTokens != su.LifetimeCacheReadTokens ||
		cur.CacheCreationTokens != su.LifetimeCacheCreationTokens || cur.OutputTokens != su.LifetimeOutputTokens {
		t.Errorf("tokens %+v vs SelfUsage %+v", cur, su)
	}
	if ratOf(t, cur.Cost).Cmp(numericRat(t, su.LifetimeCostUsd)) != 0 {
		t.Errorf("cost %s vs SelfUsage %s", cur.Cost, numericRat(t, su.LifetimeCostUsd).FloatString(6))
	}
	if cur.Disclosure.Subscription != su.LifetimeSubscriptionRunCount || cur.Disclosure.Unreported != su.LifetimeUnreportedRunCount {
		t.Errorf("disclosure %+v vs SelfUsage subscription %d unreported %d", cur.Disclosure, su.LifetimeSubscriptionRunCount, su.LifetimeUnreportedRunCount)
	}
	wantDollars(t, "cost", cur.Cost, "2.375000")
	// Intentional difference: SelfUsage.run_count counts PRIMARY work runs that carry usage
	// (issue, prompt, task, job: 4; not the judge), while Spend counts every non-chat run it
	// applies to, usage or not.
	if su.RunCount != 4 {
		t.Errorf("SelfUsage run_count = %d, want 4 (primary kinds with usage)", su.RunCount)
	}
	if cur.Runs != int64(nonChat) {
		t.Errorf("Spend runs = %d, want %d (every non-chat run)", cur.Runs, nonChat)
	}
	wantDisclosure(t, "totals", cur.Disclosure, 3, 1, 1, int64(nonChat)-5, true)

	// SelfRunOutcomes lifetime counts equal the breakdown's outcome counts summed over rows.
	so, err := f.q.SelfRunOutcomes(f.ctx, store.SelfRunOutcomesParams{UserID: o.User, LandableOrigins: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var got store.AnalyticsOutcomes
	var finished int64
	for _, r := range d.Breakdown.Rows {
		got.Completed += r.Outcomes.Completed
		got.Cancelled += r.Outcomes.Cancelled
		got.PlanRejected += r.Outcomes.PlanRejected
		got.BaseDiverged += r.Outcomes.BaseDiverged
		got.Failed += r.Outcomes.Failed
		finished += r.Finished
	}
	want := store.AnalyticsOutcomes{Completed: so.LifetimeCompleted, Cancelled: so.LifetimeCancelled,
		PlanRejected: so.LifetimePlanRejected, BaseDiverged: so.LifetimeBaseDiverged, Failed: so.LifetimeFailed}
	if got != want || finished != so.LifetimeFinished {
		t.Errorf("breakdown outcomes %+v finished %d, SelfRunOutcomes %+v finished %d", got, finished, want, so.LifetimeFinished)
	}
	if want.PlanRejected != 1 || want.BaseDiverged != 1 || want.Failed != 2 || want.Cancelled != 1 || want.Completed != 5 {
		t.Errorf("fixture outcomes %+v", want)
	}
}

func TestAnalyticsSpendParityWithAdminUsageTotalsLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	f.claimYear(2004)
	o := f.owner("adminparity")
	repo, _ := f.repo(o, "adminparity")
	// No HTTP route reaches this all-users window=all execution: the admin all-users scope
	// rejects window=all with a 400 (owner decision at landing). The store statement is kept
	// and tested here for lifetime parity and the deferred follow-up.
	// A far-past year of its own, plus the whole shared database below: window all, with
	// now strictly after every run in the database, must equal the lifetime admin totals.
	for i := 0; i < 3; i++ {
		id := f.run(spendRun{Owner: o.User, Repo: repo, At: utcAt(2004, 3, 1+i, 9, 0, 0)})
		status, cost := [3]string{"metered", "subscription", "unreported"}[i], "0"
		if status == "metered" {
			cost = "1.250000"
		}
		f.usage(id, "model-a", status, cost, 100, 10, 5, 20)
	}
	ch := f.run(spendRun{Owner: o.User, Kind: runkind.Chat, At: utcAt(2004, 3, 5, 9, 0, 0)})
	f.metered(ch, "model-a", "9.990000")

	var latest time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT max(created_at) FROM runs`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	now := latest.Add(time.Second)
	adm, err := f.q.AdminUsageTotals(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	res := f.call(spendCall{Now: now, Window: analytics.WindowAll})
	cur := res.Doc.Totals.Current
	if cur.InputTokens != adm.LifetimeInputTokens || cur.CacheReadTokens != adm.LifetimeCacheReadTokens ||
		cur.CacheCreationTokens != adm.LifetimeCacheCreationTokens || cur.OutputTokens != adm.LifetimeOutputTokens {
		t.Errorf("tokens %+v vs AdminUsageTotals %+v", cur, adm)
	}
	if ratOf(t, cur.Cost).Cmp(numericRat(t, adm.LifetimeCostUsd)) != 0 {
		t.Errorf("cost %s vs AdminUsageTotals %s", cur.Cost, numericRat(t, adm.LifetimeCostUsd).FloatString(6))
	}
	if cur.Disclosure.Subscription != adm.LifetimeSubscriptionRunCount || cur.Disclosure.Unreported != adm.LifetimeUnreportedRunCount {
		t.Errorf("disclosure %+v vs AdminUsageTotals subscription %d unreported %d", cur.Disclosure, adm.LifetimeSubscriptionRunCount, adm.LifetimeUnreportedRunCount)
	}
	if res.Doc.Totals.Previous != nil {
		t.Error("window all has previous totals")
	}
}
