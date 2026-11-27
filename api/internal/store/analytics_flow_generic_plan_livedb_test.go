package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729 (m3): regression test for the generic-plan rescan of the receipt CTEs. The first
// Flow statements joined a cohort-wide per-run GROUP BY over rcpt_c, which a generic plan
// (plan_cache_mode = force_generic_plan, what a long-lived pooled connection reaches after five
// executions) planned as a nested loop re-reading the materialized CTE once per run: 12.8 s on a
// 120k-run seed against the 8 s statement_timeout. The statements now probe
// idx_run_user_inputs_run_id with a per-run LATERAL aggregate, so every receipt-evidence CTE is
// read a bounded number of times, never once per run.

// flowReceiptCTEs are the CTEs holding approve_plan receipts (and the presentations they pair
// with) in the Flow statements. A scan of one of them under a node that ran more than
// once is the per-run rescan.
var flowReceiptCTEs = map[string]bool{"rcpt": true, "rcpt_c": true, "pres": true}

// flowRescans returns one line per CTE Scan of a receipt CTE that sits under a node (itself
// included) that ran more than once, and the number of receipt-CTE scans seen (so the caller can
// prove the check is not vacuous). Checking the ancestors, not only the scan's own loop count,
// is what catches the per-run join: an inner-side aggregate over rcpt_c is rescanned from its
// hash table, so the CTE Scan below it reports loops=1 while the aggregate reports loops=<runs>
// and re-emits every receipt group on each loop.
func flowRescans(n *planNode, rescanned string) (rescans []string, scans int) {
	if rescanned == "" && n.ActualLoops > 1 {
		rescanned = fmt.Sprintf("%s loops=%.0f", n.NodeType, n.ActualLoops)
	}
	if n.NodeType == "CTE Scan" && flowReceiptCTEs[n.CTEName] {
		scans++
		if rescanned != "" {
			rescans = append(rescans, fmt.Sprintf("CTE Scan on %s under %s", n.CTEName, rescanned))
		}
	}
	for i := range n.Plans {
		r, s := flowRescans(&n.Plans[i], rescanned)
		rescans, scans = append(rescans, r...), scans+s
	}
	return rescans, scans
}

// flowGenericArg renders one Flow argument as a typed SQL literal: the shared helper plus the
// two Flow-only types (int32 min_percentile_n and the float8[] histogram edges).
func flowGenericArg(t *testing.T, a any) string {
	t.Helper()
	switch v := a.(type) {
	case int32:
		return strconv.FormatInt(int64(v), 10) + "::int"
	case []float64:
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = strconv.FormatFloat(x, 'g', -1, 64)
		}
		return "ARRAY[" + strings.Join(parts, ", ") + "]::float8[]"
	}
	return sqlLiteral(t, a)
}

// TestAnalyticsFlowGenericPlanShapeLiveDB runs EXPLAIN (ANALYZE) of the real owner and all-scope
// Flow statements under force_generic_plan, over a cohort of a few hundred issue runs that each
// carry a presentation and an approve_plan receipt, and fails when a receipt CTE is re-read per
// run. The generic plan is confirmed through pg_prepared_statements.generic_plans after the
// EXPLAIN; the statements are PREPAREd with the literal arguments the generated methods pass.
func TestAnalyticsFlowGenericPlanShapeLiveDB(t *testing.T) {
	const runs = 400
	f := newSpendFixture(t)
	o := f.owner("flowgp")
	repo, _ := f.repo(o, "flowgp")
	base := spendSeq.Add(runs)
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM runs WHERE user_id = $1`, o.User)
	})
	// 400 completed human-gated issue runs across the window, one revision, one presentation and
	// one clean applied receipt each.
	mustExec(f.ctx, t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		                   created_at, finished_at, status_since, harness, gate_revision, plan_md)
		 SELECT gen_random_uuid(), $1, $2, $3::bigint + n, 't', 'd', 'completed', 'issue',
		        $4::timestamptz + n * interval '20 minutes', $4::timestamptz + n * interval '20 minutes' + interval '1 hour',
		        $4::timestamptz + n * interval '20 minutes' + interval '1 hour', 'claude', 1, 'plan'
		 FROM generate_series(1, $5::int) n`,
		o.User, repo, base-runs, flowAt(10, 0, 0), runs)
	mustExec(f.ctx, t, f.pool,
		`INSERT INTO run_gate_presentations (run_id, presentation_id, revision, created_at)
		 SELECT id, gen_random_uuid(), 1, created_at + interval '2 minutes' FROM runs WHERE user_id = $1`, o.User)
	mustExec(f.ctx, t, f.pool,
		`INSERT INTO run_user_inputs (run_id, kind, created_at, consumed_at, applied_at, disposition, gate_binding, gate_revision)
		 SELECT id, 'approve_plan', created_at + interval '10 minutes', created_at + interval '10 minutes' + interval '5 seconds',
		        created_at + interval '10 minutes' + interval '5 seconds', 'applied', 'bound', 1
		 FROM runs WHERE user_id = $1`, o.User)
	for _, tbl := range []string{"runs", "run_user_inputs", "run_gate_presentations"} {
		mustExec(f.ctx, t, f.pool, "ANALYZE "+tbl)
	}

	conn, err := f.pool.Acquire(f.ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	simple := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(f.ctx, sql, pgx.QueryExecModeSimpleProtocol); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	simple(`SET plan_cache_mode = force_generic_plan`)
	defer func() { _, _ = conn.Exec(context.Background(), `RESET ALL`, pgx.QueryExecModeSimpleProtocol) }()

	b := f.bounds(spendCall{Now: flowNow, Window: analytics.Window7d})
	kinds := runkind.All()
	ownerP := store.AnalyticsOwnerFlowParams{
		UserID: o.User, Now: spendTS(b.Now), From: spendTS(b.From),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN, Kinds: kinds,
	}
	allP := store.AnalyticsAllFlowParams{
		Now: spendTS(b.Now), From: spendTS(b.From),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN, Kinds: kinds,
	}
	capture := &capturingDBTX{}
	_, _ = store.New(capture).AnalyticsOwnerFlow(f.ctx, ownerP)
	ownerArgs := capture.args
	_, _ = store.New(capture).AnalyticsAllFlow(f.ctx, allP)
	allArgs := capture.args

	for _, c := range []struct {
		name, sql string
		args      []any
	}{
		{"AnalyticsOwnerFlow", store.AnalyticsOwnerFlowSQL, ownerArgs},
		{"AnalyticsAllFlow", store.AnalyticsAllFlowSQL, allArgs},
	} {
		lits := make([]string, len(c.args))
		for i, a := range c.args {
			lits[i] = flowGenericArg(t, a)
		}
		stmt := "flow_gp_" + strings.ToLower(c.name)
		simple("DEALLOCATE ALL")
		simple("PREPARE " + stmt + " AS " + c.sql)
		var raw []byte
		if err := conn.QueryRow(f.ctx, fmt.Sprintf("EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE %s(%s)", stmt, strings.Join(lits, ", ")),
			pgx.QueryExecModeSimpleProtocol).Scan(&raw); err != nil {
			t.Fatalf("%s: EXPLAIN EXECUTE: %v", c.name, err)
		}
		var generic int64
		if err := conn.QueryRow(f.ctx, `SELECT generic_plans FROM pg_prepared_statements WHERE name = $1`, pgx.QueryExecModeSimpleProtocol, stmt).Scan(&generic); err != nil {
			t.Fatalf("%s: read pg_prepared_statements: %v", c.name, err)
		}
		if generic < 1 {
			t.Fatalf("%s: generic_plans = %d; the EXPLAIN did not run the generic plan this test exists to check", c.name, generic)
		}
		var doc []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
			t.Fatalf("%s: decode EXPLAIN JSON (%d docs): %v", c.name, len(doc), err)
		}
		root := &doc[0].Plan
		rescans, scans := flowRescans(root, "")
		if scans == 0 {
			t.Errorf("%s: no CTE Scan of a receipt CTE (%v) in the plan; the check is vacuous\n%s", c.name, flowReceiptCTEs, renderPlan(root, 0))
		}
		for _, r := range rescans {
			t.Errorf("%s: receipt CTE re-read per outer row under a generic plan: %s\n%s", c.name, r, renderPlan(root, 0))
		}
	}
}
