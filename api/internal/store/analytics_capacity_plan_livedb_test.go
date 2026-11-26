package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4731 (U4): plan guard for the historical Capacity statements. It runs in a private
// database (shared data would distort the plans), seeds two owners with ordinary-lane terminal
// and ongoing runs, and EXPLAINs both statements verbatim with sequential scans disabled (the
// house pattern, see TestOwnerActivityAnchorUsesPerUserIndexesLiveDB): that proves the index
// path EXISTS for the owner statement, not that the planner picks it at any table size. The
// opt-in harness TestAnalyticsPlanEvidenceLiveDB measures the sizes.
//
// Guarded:
//   - the OWNER statement reaches runs through runs_user_terminal_end_idx (terminal arm) and
//     runs_user_nonterminal_idx (nonterminal arm);
//   - neither historical statement joins the interval/event set with the bucket set through a
//     Nested Loop (that join shape is quadratic in intervals x buckets);
//   - neither contains a self-join of the interval/event set (or of runs);
//   - the plan shows a bucket side (CTE bk or a Function Scan) at all, so the join
//     check cannot pass because the bucket set was inlined out of sight.
//
// Not guarded: a Seq Scan on runs. With enable_seqscan = off the planner avoids one wherever an
// index can serve the scan, so "no Seq Scan on runs" would be nearly vacuous here; the seq scans
// of the admin statements at 30d and beyond are measured by the evidence harness instead.
//
// The gauge statements are not checked here: they legitimately use nested loops.

// capPlanNode is one node of an EXPLAIN (FORMAT JSON) plan.
type capPlanNode struct {
	NodeType     string        `json:"Node Type"`
	ParentRel    string        `json:"Parent Relationship"`
	JoinType     string        `json:"Join Type"`
	RelationName string        `json:"Relation Name"`
	IndexName    string        `json:"Index Name"`
	CTEName      string        `json:"CTE Name"`
	Plans        []capPlanNode `json:"Plans"`
}

// capSources is the set of relation, index and CTE names scanned below a node.
func (n capPlanNode) sources(into map[string]bool) {
	switch {
	case n.NodeType == "CTE Scan" && n.CTEName != "":
		into["cte:"+n.CTEName] = true
	case n.NodeType == "Function Scan":
		// The bucket set is ROWS FROM (unnest(..), unnest(..)), a Function Scan; any Function
		// Scan counts as the bucket side.
		into["fn"] = true
	case n.RelationName != "":
		into["rel:"+n.RelationName] = true
	}
	for _, c := range n.Plans {
		c.sources(into)
	}
}

func (n capPlanNode) indexes(into map[string]bool) {
	if n.IndexName != "" {
		into[n.IndexName] = true
	}
	for _, c := range n.Plans {
		c.indexes(into)
	}
}

// capIntervalSources are the scans that carry the interval/event set or the runs it comes from;
// the bucket set is the CTE bk. When the planner inlines bk (AS NOT MATERIALIZED) the CTE Scan
// disappears and the bucket side shows as a Function Scan, so both count as the bucket
// side, and capPlanViolations also demands that the plan shows one of them at all (otherwise the
// no-join check would pass vacuously).
var capIntervalSources = []string{"rel:runs", "cte:pop", "cte:iv", "cte:ev", "cte:pts", "cte:run", "cte:seg", "cte:uw"}

var capBucketSources = []string{"cte:bk", "fn"}

func capHasAny(set map[string]bool, names []string) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}

// capPlanSources is every scan source of the whole plan.
func capPlanSources(n capPlanNode) map[string]bool {
	all := map[string]bool{}
	n.sources(all)
	return all
}

// capPlanViolations walks every join node and returns the guard violations under it.
func capPlanViolations(n capPlanNode, path string) []string {
	var out []string
	label := path + "/" + n.NodeType
	isJoin := strings.HasSuffix(n.NodeType, "Join") || n.NodeType == "Nested Loop"
	if isJoin {
		// A join's two inputs are its Outer and Inner children; InitPlan and SubPlan children
		// attached to the same node are not inputs, so a join carrying one is still checked.
		l, r := map[string]bool{}, map[string]bool{}
		sides := 0
		for _, c := range n.Plans {
			switch c.ParentRel {
			case "Outer":
				c.sources(l)
				sides++
			case "Inner":
				c.sources(r)
				sides++
			}
		}
		if sides != 2 {
			// Never pass a join whose inputs cannot be identified.
			out = append(out, label+": join without exactly one Outer and one Inner child")
		}
		lInt, rInt := capHasAny(l, capIntervalSources), capHasAny(r, capIntervalSources)
		if n.NodeType == "Nested Loop" && ((capHasAny(l, capBucketSources) && rInt) || (capHasAny(r, capBucketSources) && lInt)) {
			out = append(out, label+": Nested Loop joins the interval/event set with the bucket set")
		}
		for _, name := range capIntervalSources {
			if l[name] && r[name] {
				out = append(out, label+": self-join, both sides scan "+name)
			}
		}
	}
	for _, c := range n.Plans {
		out = append(out, capPlanViolations(c, label)...)
	}
	return out
}

// capJoinNodes lists the join node types of a plan, for the test log.
func capJoinNodes(n capPlanNode, into []string) []string {
	if strings.HasSuffix(n.NodeType, "Join") || n.NodeType == "Nested Loop" {
		into = append(into, n.NodeType)
	}
	for _, c := range n.Plans {
		into = capJoinNodes(c, into)
	}
	return into
}

// capExplain returns the JSON plan of sql under real parameters with sequential scans off.
func capExplain(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args []any) (capPlanNode, string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("SET enable_seqscan: %v", err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	var doc []struct {
		Plan capPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("decode EXPLAIN JSON: %v", err)
	}
	pretty, _ := json.MarshalIndent(doc[0].Plan, "", " ")
	return doc[0].Plan, string(pretty)
}

func TestAnalyticsCapacityPlanGuardLiveDB(t *testing.T) {
	ctx, pool := capPrivateDB(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	owner, other := seedCapUser(ctx, t, pool, "cap-plan-a@e2e"), seedCapUser(ctx, t, pool, "cap-plan-b@e2e")
	// The two owners' rows are interleaved (ORDER BY g, user): a heap laid out owner by owner has a
	// user_id correlation of +1 or -1 depending on which random uuid sorts first, and that tips the
	// planner between idx_runs_user and runs_user_terminal_end_idx from one run to the next.
	// 6000 ordinary-lane runs per owner over 30 days: every twentieth is still running, the rest
	// are terminal with a retained end 1-97 minutes after the start.
	mustExec(ctx, t, pool, `INSERT INTO runs (id, user_id, issue_title, issue_description, status, kind, created_at,
	                                          first_started_at, finished_at, status_since, harness, job_type)
	    SELECT gen_random_uuid(), u.id, 't', 'd',
	           CASE WHEN g % 20 = 0 THEN 'running' ELSE 'completed' END, 'job',
	           $2::timestamptz - (g::float8 / 6000) * interval '30 days',
	           $2::timestamptz - (g::float8 / 6000) * interval '30 days' + interval '30 seconds',
	           CASE WHEN g % 20 = 0 THEN NULL
	                ELSE $2::timestamptz - (g::float8 / 6000) * interval '30 days' + (90 + (g % 97) * 60) * interval '1 second' END,
	           CASE WHEN g % 20 = 0 THEN $2::timestamptz - (g::float8 / 6000) * interval '30 days' + interval '30 seconds'
	                ELSE $2::timestamptz - (g::float8 / 6000) * interval '30 days' + (90 + (g % 97) * 60) * interval '1 second' END,
	           'claude', 'research'
	    FROM generate_series(1, 6000) g CROSS JOIN unnest($1::uuid[]) AS u(id)
	    ORDER BY g, u.id`, []uuid.UUID{owner, other}, now)
	mustExec(ctx, t, pool, `ANALYZE runs`)

	earliest := now.AddDate(0, 0, -30)
	for _, win := range []analytics.Window{analytics.Window30d, analytics.WindowAll} {
		b, err := analytics.ComputeBounds(now, time.UTC, win, &earliest)
		if err != nil {
			t.Fatalf("%s: ComputeBounds: %v", win, err)
		}
		if err := analytics.ValidateBuckets(b); err != nil {
			t.Fatalf("%s: ValidateBuckets: %v", win, err)
		}
		kinds := runkind.All()
		capture := &capturingDBTX{}
		cq := store.New(capture)
		_, _ = cq.AnalyticsOwnerCapacity(ctx, store.AnalyticsOwnerCapacityParams{
			Now: spendTS(b.Now), From: spendTS(b.From), UserID: owner, Kinds: kinds,
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		})
		ownerArgs := capture.args
		_, _ = cq.AnalyticsAllCapacity(ctx, store.AnalyticsAllCapacityParams{
			Now: spendTS(b.Now), From: spendTS(b.From), Kinds: kinds,
			BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		})
		allArgs := capture.args
		if len(ownerArgs) != 8 || len(allArgs) != 7 {
			t.Fatalf("%s: captured %d owner and %d all arguments, want 8 and 7", win, len(ownerArgs), len(allArgs))
		}

		for _, c := range []struct {
			name string
			sql  string
			args []any
			want []string // indexes the plan must use
		}{
			{"AnalyticsOwnerCapacity", store.AnalyticsOwnerCapacitySQL, ownerArgs, []string{"runs_user_terminal_end_idx", "runs_user_nonterminal_idx"}},
			{"AnalyticsAllCapacity", store.AnalyticsAllCapacitySQL, allArgs, nil},
		} {
			plan, pretty := capExplain(ctx, t, pool, c.sql, c.args)
			label := fmt.Sprintf("%s window=%s buckets=%d", c.name, win, len(b.BucketStarts))
			used := map[string]bool{}
			plan.indexes(used)
			names := make([]string, 0, len(used))
			for n := range used {
				names = append(names, n)
			}
			sort.Strings(names)
			t.Logf("%s: indexes used %v; joins %v", label, names, capJoinNodes(plan, nil))
			for _, idx := range c.want {
				if !used[idx] {
					t.Errorf("%s: plan does not use %s (uses %v):\n%s", label, idx, names, pretty)
				}
			}
			if !capHasAny(capPlanSources(plan), capBucketSources) {
				// Calibration: without a visible bucket side the Nested Loop check cannot fire.
				t.Errorf("%s: the plan shows no bucket side (%v), so the join check is blind:\n%s", label, capBucketSources, pretty)
			}
			if v := capPlanViolations(plan, ""); len(v) > 0 {
				t.Errorf("%s: %s\n%s", label, strings.Join(v, "; "), pretty)
			}
		}
	}
}

// TestAnalyticsCapacityPlanGuardNegativeControlLiveDB proves the join check can fail: a
// hand-written interval x bucket join forced into a Nested Loop is reported, and so is an
// interval self-join.
func TestAnalyticsCapacityPlanGuardNegativeControlLiveDB(t *testing.T) {
	ctx, pool := capPrivateDB(t)
	mustExec(ctx, t, pool, `ANALYZE runs`)
	explain := func(sql string) capPlanNode {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, s := range []string{`SET LOCAL enable_hashjoin = off`, `SET LOCAL enable_mergejoin = off`, `SET LOCAL enable_seqscan = off`} {
			if _, err := tx.Exec(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var doc []struct {
			Plan capPlanNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
			t.Fatalf("decode: %v", err)
		}
		return doc[0].Plan
	}
	// The bucket set (CTE bk, used twice so it is materialized) against the interval set (CTE
	// iv, also used twice), joined on overlap.
	cross := explain(`WITH bk AS MATERIALIZED (SELECT g AS idx, now() + g * interval '1 day' AS s FROM generate_series(1, 30) g),
	    iv AS MATERIALIZED (SELECT now() + g * interval '1 hour' AS cs FROM generate_series(1, 500) g)
	SELECT count(*) FROM bk JOIN iv ON iv.cs >= bk.s
	WHERE (SELECT count(*) FROM iv) > 0 AND (SELECT count(*) FROM bk) > 0`)
	if v := capPlanViolations(cross, ""); len(v) == 0 {
		t.Error("interval x bucket Nested Loop was not reported")
	}
	self := explain(`WITH iv AS MATERIALIZED (SELECT g AS id, now() + g * interval '1 hour' AS cs FROM generate_series(1, 500) g)
	SELECT count(*) FROM iv a JOIN iv b ON a.cs < b.cs
	WHERE (SELECT count(*) FROM iv) > 0`)
	if v := capPlanViolations(self, ""); len(v) == 0 {
		t.Error("interval self-join was not reported")
	}
	// A join that also carries an InitPlan child (the join is the top node, so the uncorrelated
	// subquery in its condition attaches to it) has three children; it must still be checked by
	// its Outer and Inner inputs, not skipped because it does not have exactly two.
	withInit := explain(`WITH bk AS MATERIALIZED (SELECT g AS idx, now() + g * interval '1 day' AS s FROM generate_series(1, 30) g),
	    iv AS MATERIALIZED (SELECT now() + g * interval '1 hour' AS cs FROM generate_series(1, 500) g)
	SELECT bk.idx FROM bk JOIN iv ON iv.cs >= bk.s + (SELECT interval '0' FROM generate_series(1, 1) LIMIT 1)`)
	if !capJoinCarriesSubplan(withInit) {
		t.Fatalf("calibration: the control plan has no join with an InitPlan or SubPlan child; the shape this test needs did not materialize")
	}
	if v := capPlanViolations(withInit, ""); !slices.ContainsFunc(v, func(m string) bool {
		return strings.Contains(m, "Nested Loop joins the interval/event set with the bucket set")
	}) {
		t.Errorf("interval x bucket Nested Loop carrying an InitPlan child was not reported as that join (got %v)", v)
	}
}

// capJoinCarriesSubplan reports whether some join node has an InitPlan or SubPlan child.
func capJoinCarriesSubplan(n capPlanNode) bool {
	if strings.HasSuffix(n.NodeType, "Join") || n.NodeType == "Nested Loop" {
		for _, c := range n.Plans {
			if c.ParentRel == "InitPlan" || c.ParentRel == "SubPlan" {
				return true
			}
		}
	}
	for _, c := range n.Plans {
		if capJoinCarriesSubplan(c) {
			return true
		}
	}
	return false
}
