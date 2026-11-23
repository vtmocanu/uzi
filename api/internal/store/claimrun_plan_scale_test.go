package store_test

// Stage S of TestClaimRunPlanMeasurement (issue #4602): a deterministic plan-choice diagnostic
// over completed-run history scales. It has NO timed N-call loop and is not one of the
// pre-registered timing cells; the wall milliseconds it prints are diagnostic only.
//
// Per scale (UZI_CLAIMRUN_PLAN_SCALES) it
//  1. seeds a scale template: that many completed/failed/cancelled runs spread over the 8 fleet
//     users (the measured user holds 40 percent), closed custody holds and plan cross-checks
//     where the schema accepts them (each seed step reports rows or the database error), then
//     ANALYZE of the whole database;
//  2. on a clone with zero queued runs for the measured user: EXPLAIN (GENERIC_PLAN) total cost,
//     the custom-plan cost of EXECUTEs 1..8 of a prepared ClaimRun with the captured idle
//     bindings, then 20 idle segment calls on a fresh MaxConns=1 pool in auto mode;
//  3. the same 20 calls on a fresh clone of the same scale with force_generic_plan;
//  4. EXPLAIN ANALYZE of the claimable (c) scenario under a generic and a custom plan, in
//     rolled-back savepoints (runT3Cell).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var cpScaleDefault = []int{1000, 10000, 50000, 150000}

func cpParseScales(t *testing.T) []int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("UZI_CLAIMRUN_PLAN_SCALES"))
	if raw == "" {
		return slices.Clone(cpScaleDefault)
	}
	var out []int
	for _, s := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 100 {
			t.Fatalf("UZI_CLAIMRUN_PLAN_SCALES: %q is not an integer >= 100", s)
		}
		out = append(out, n)
	}
	return out
}

// cpTrySeed runs one optional seed statement and reports rows or the database error verbatim.
func cpTrySeed(ctx context.Context, c *pgx.Conn, sql string, args ...any) map[string]any {
	tag, err := c.Exec(ctx, sql, args...)
	if err != nil {
		return map[string]any{"seeded": false, "error": err.Error()}
	}
	return map[string]any{"seeded": true, "rows": tag.RowsAffected()}
}

func cpGroupCount(ctx context.Context, t *testing.T, c *pgx.Conn, sql string, args ...any) map[string]int64 {
	t.Helper()
	rows, err := c.Query(ctx, sql, args...)
	if err != nil {
		t.Fatalf("group count: %v", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatalf("group count scan: %v", err)
		}
		out[k] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("group count: %v", err)
	}
	return out
}

const cpScaleRunsSQL = `
INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind, harness,
                  pipeline_id, pipeline_ref, mr_iid, target_run_id, branch,
                  dispatched_at, created_at, updated_at, status_since)
SELECT gen_random_uuid(), s.u,
       CASE WHEN s.k = 'chat' THEN NULL WHEN s.u = $3::uuid THEN $4::uuid ELSE $2::uuid END,
       CASE WHEN s.k IN ('issue', 'self_improve') THEN 100000 + s.g END,
       'hist', 'd', s.st, s.k, s.hn,
       CASE WHEN s.k = 'ci_fix' THEN 1000000 + s.g END,
       CASE WHEN s.k IN ('ci_fix', 'mr_rework') THEN 'refs/heads/agent/issue-' || s.g END,
       CASE WHEN s.k = 'mr_rework' THEN s.g END,
       CASE WHEN s.k = 'mr_rework' THEN $6::uuid END,
       CASE WHEN s.k = 'task' THEN 'uzi/task/' || s.g END,
       CASE WHEN s.k = 'task' THEN s.ts END, s.ts, s.ts, s.ts
FROM (
    SELECT g, ($1::uuid[])[1 + x.bu::int] AS u,
           CASE WHEN x.bk < 62 THEN 'issue' WHEN x.bk < 72 THEN 'mr_rework' WHEN x.bk < 80 THEN 'ci_fix'
                WHEN x.bk < 86 THEN 'chat' WHEN x.bk < 91 THEN 'prompt' WHEN x.bk < 96 THEN 'task'
                ELSE 'self_improve' END AS k,
           CASE WHEN x.bs < 82 THEN 'completed' WHEN x.bs < 94 THEN 'failed' ELSE 'cancelled' END AS st,
           CASE WHEN x.bh < 88 THEN 'claude' ELSE 'codex' END AS hn,
           now() - make_interval(secs => (x.bt % 15552000)::double precision) - interval '10 minutes' AS ts
    FROM generate_series(1, $5::int) g
    CROSS JOIN LATERAL (SELECT (hashtextextended(g::text || 'u', 0) & 2147483647) % 100 AS bu,
                               (hashtextextended(g::text || 'k', 0) & 2147483647) % 100 AS bk,
                               (hashtextextended(g::text || 's', 0) & 2147483647) % 100 AS bs,
                               (hashtextextended(g::text || 'h', 0) & 2147483647) % 100 AS bh,
                               (hashtextextended(g::text || 't', 0) & 2147483647) AS bt) x
) s`

const cpScaleHoldsSQL = `
INSERT INTO recovery_custody_holds (id, user_id, repo_id, run_id, generation, state, original_worker_id,
                                    original_worker_identity, created_at, updated_at, released_at, release_evidence)
SELECT gen_random_uuid(), r.user_id, r.repo_id, r.id, 1, 'released', $1::uuid, $2::text,
       r.created_at, r.created_at, r.created_at + interval '1 hour', 'forge_no_output'
FROM runs r
WHERE r.kind IN ('issue', 'ci_fix', 'self_improve', 'prompt', 'task', 'mr_rework')
  AND r.status IN ('completed', 'failed')
  AND r.issue_title = 'hist'
  AND (hashtextextended(r.id::text, 1) & 2147483647) % 100 < 60`

const cpScaleChecksSQL = `
WITH leads AS (
    SELECT id, user_id, repo_id, created_at FROM runs
    WHERE kind = 'issue' AND status = 'completed' AND issue_title = 'hist'
      AND (hashtextextended(id::text, 2) & 2147483647) % 100 < 8
), chk AS (
    INSERT INTO runs (id, user_id, repo_id, kind, target_run_id, harness, status, report_only, budget_wall_seconds,
                      issue_title, issue_description, created_at, updated_at, status_since)
    SELECT gen_random_uuid(), l.user_id, l.repo_id, 'cross_check', l.id, 'claude', 'completed', true, 1800,
           'plan', 'candidate', l.created_at, l.created_at, l.created_at
    FROM leads l
    RETURNING id, target_run_id, created_at
)
INSERT INTO cross_checks (lead_run_id, checker_run_id, stage, round, lead_claim_generation, plan_md, milestones, size_class,
                          base_commit, candidate_digest, checker_harness, deadline_at, automatic_rounds_enabled,
                          automatic_revision_limit, verdict, reason_class)
SELECT c.target_run_id, c.id, 'plan', 1, 1, 'plan', '[]', 's', repeat('a', 40), '\x6469'::bytea, 'claude',
       c.created_at + interval '1 hour', true, 2, 'approve', 'approve'
FROM chk c`

// cpSeedScale seeds the scale template and returns what was seeded.
func (e *cpEnv) cpSeedScale(ctx context.Context, dsn string, scale int) map[string]any {
	t := e.t
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("scale seed connect: %v", err)
	}
	defer func() { _ = c.Close(ctx) }()
	started := time.Now()
	// 100 slots: the measured user 40, the other pmSeed user 20, five mid users 7 each, the fresh user 5.
	var slots []uuid.UUID
	add := func(id uuid.UUID, n int) {
		for i := 0; i < n; i++ {
			slots = append(slots, id)
		}
	}
	add(e.fx.User, 40)
	add(e.fx.OtherUser, 20)
	for _, m := range e.cx.Mid {
		add(m, 7)
	}
	add(e.cx.Fresh, 5)
	if len(slots) != 100 {
		t.Fatalf("scale seed: %d user slots, want 100", len(slots))
	}
	tag, err := c.Exec(ctx, cpScaleRunsSQL, slots, e.fx.Repo, e.cx.Fresh, e.cx.FreshRepo, scale, e.fx.Run)
	if err != nil {
		t.Fatalf("scale %d: seed runs: %v", scale, err)
	}
	rep := map[string]any{"scale": scale, "runs_seeded": tag.RowsAffected()}
	rep["custody_holds"] = cpTrySeed(ctx, c, cpScaleHoldsSQL, e.fx.Worker, e.fx.Worker.String())
	rep["cross_checks"] = cpTrySeed(ctx, c, cpScaleChecksSQL)
	rep["worker_active_runs"] = map[string]any{"seeded": false, "reason": "rows exist only for live attempts; history is terminal and has none"}
	if _, err := c.Exec(ctx, `ANALYZE`); err != nil {
		t.Fatalf("scale %d: ANALYZE: %v", scale, err)
	}
	rep["analyze_whole_database"] = true
	var total, mine int64
	if err := c.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE user_id = $1) FROM runs`, e.fx.User).Scan(&total, &mine); err != nil {
		t.Fatalf("scale %d: count: %v", scale, err)
	}
	rep["runs_total"], rep["runs_measured_user"] = total, mine
	rep["measured_user_share"] = float64(mine) / float64(total)
	rep["measured_user_by_kind"] = cpGroupCount(ctx, t, c, `SELECT kind, count(*) FROM runs WHERE user_id = $1 GROUP BY 1`, e.fx.User)
	rep["measured_user_by_status"] = cpGroupCount(ctx, t, c, `SELECT status, count(*) FROM runs WHERE user_id = $1 GROUP BY 1`, e.fx.User)
	rep["measured_user_by_harness"] = cpGroupCount(ctx, t, c, `SELECT harness, count(*) FROM runs WHERE user_id = $1 GROUP BY 1`, e.fx.User)
	rep["runs_by_user"] = cpGroupCount(ctx, t, c, `SELECT user_id::text, count(*) FROM runs GROUP BY 1 ORDER BY 2 DESC LIMIT 10`)
	rep["queued_runs_all_users"] = cpGroupCount(ctx, t, c, `SELECT user_id::text, count(*) FROM runs WHERE status = 'queued' GROUP BY 1`)
	var holds, checks int64
	if err := c.QueryRow(ctx, `SELECT (SELECT count(*) FROM recovery_custody_holds), (SELECT count(*) FROM cross_checks)`).Scan(&holds, &checks); err != nil {
		t.Fatalf("scale %d: count holds: %v", scale, err)
	}
	rep["recovery_custody_holds_total"], rep["cross_checks_total"] = holds, checks
	rep["seed_seconds"] = time.Since(started).Seconds()
	return rep
}

// cpPlanSummary lists the scan nodes of an EXPLAIN (FORMAT JSON) document with their counts.
func cpPlanSummary(raw []byte) (cost float64, scans map[string]int, err error) {
	var docs []map[string]any
	if err = json.Unmarshal(raw, &docs); err != nil || len(docs) != 1 {
		return 0, nil, fmt.Errorf("unparseable EXPLAIN JSON (%d docs, err %v): %.200s", len(docs), err, raw)
	}
	root, _ := docs[0]["Plan"].(map[string]any)
	cost, _ = root["Total Cost"].(float64)
	scans = map[string]int{}
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if nt, _ := n["Node Type"].(string); strings.Contains(nt, "Scan") {
			key := nt
			if rel, _ := n["Relation Name"].(string); rel != "" {
				key += ":" + rel
			}
			if idx, _ := n["Index Name"].(string); idx != "" {
				key += "(" + idx + ")"
			}
			scans[key]++
		}
		if ps, ok := n["Plans"].([]any); ok {
			for _, p := range ps {
				if pm, ok := p.(map[string]any); ok {
					walk(pm)
				}
			}
		}
	}
	walk(root)
	return cost, scans, nil
}

func cpMedian(xs []float64) float64 { return cpQ(xs, 0.5) }

// planCosts records the generic cost and the custom costs of executions 1..8.
func (e *cpEnv) planCosts(ctx context.Context, r *cpRunner, dsn string, scale int) map[string]any {
	t := e.t
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("scale costs: parse: %v", err)
	}
	xc, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("scale costs: connect: %v", err)
	}
	defer func() { _ = xc.Close(context.WithoutCancel(ctx)) }()
	var mode string
	if err := xc.QueryRow(ctx, `SHOW plan_cache_mode`, pgx.QueryExecModeSimpleProtocol).Scan(&mode); err != nil || mode != "auto" {
		t.Fatalf("scale costs: plan_cache_mode = %q (err %v), want auto", mode, err)
	}
	res := map[string]any{"scale": scale}
	// PgConn().Exec sends the text as-is: pgx's own simple protocol would reject the unbound $n.
	gres, err := xc.PgConn().Exec(ctx, `EXPLAIN (GENERIC_PLAN, FORMAT JSON) `+e.claimSQL).ReadAll()
	if err != nil || len(gres) != 1 || len(gres[0].Rows) != 1 || len(gres[0].Rows[0]) != 1 {
		t.Fatalf("scale %d: EXPLAIN (GENERIC_PLAN): %d results, err %v", scale, len(gres), err)
	}
	raw := gres[0].Rows[0][0]
	gcost, gscans, err := cpPlanSummary(raw)
	if err != nil {
		t.Fatalf("scale %d: generic plan: %v", scale, err)
	}
	res["generic_plan_total_cost"], res["generic_plan_scans"] = gcost, gscans
	if _, err := xc.Exec(ctx, `PREPARE cpclaim AS `+e.claimSQL, pgx.QueryExecModeSimpleProtocol); err != nil {
		t.Fatalf("scale costs: PREPARE: %v", err)
	}
	args := cpSimpleArgs(cpArgsOf(r.params))
	ph := make([]string, len(args))
	for i := range ph {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	explain := `EXPLAIN (FORMAT JSON) EXECUTE cpclaim(` + strings.Join(ph, ",") + `)`
	qargs := append([]any{pgx.QueryExecModeSimpleProtocol}, args...)
	var execs []map[string]any
	for n := 1; n <= 8; n++ {
		var raw []byte
		if err := xc.QueryRow(ctx, explain, qargs...).Scan(&raw); err != nil {
			t.Fatalf("scale %d: EXPLAIN EXECUTE %d: %v", scale, n, err)
		}
		cost, scans, err := cpPlanSummary(raw)
		if err != nil {
			t.Fatalf("scale %d: execution %d: %v", scale, n, err)
		}
		var g, cu int64
		if err := xc.QueryRow(ctx, `SELECT generic_plans, custom_plans FROM pg_prepared_statements WHERE name = 'cpclaim'`,
			pgx.QueryExecModeSimpleProtocol).Scan(&g, &cu); err != nil {
			t.Fatalf("scale costs: pg_prepared_statements: %v", err)
		}
		execs = append(execs, map[string]any{"execution": n, "plan_total_cost": cost, "scans": scans, "generic_plans_after": g, "custom_plans_after": cu})
	}
	res["explain_execute"] = execs
	return res
}

// cpCallSeries flattens a phase into the 20 per-call wall times (call 1 is the cold call).
func cpCallSeries(p cpPhaseRes) []float64 {
	return append([]float64{p.ColdMs}, p.WarmupMs...)
}

func cpSwitchCall(p cpPhaseRes) string {
	for _, e := range p.EarlyPlanCounts {
		if e.Generic > 0 {
			return strconv.Itoa(e.Call)
		}
	}
	if p.GenericDeltaTotal > 0 {
		return "after 12"
	}
	return "never"
}

func (e *cpEnv) scaleCalls(ctx context.Context, r *cpRunner, scale int, mode string) {
	ph := r.phase(ctx, "segment", 19, 0, r.mimic)
	calls := cpCallSeries(ph)
	if len(calls) != 20 {
		e.t.Fatalf("scale %d %s: %d calls, want 20", scale, mode, len(calls))
	}
	rec := map[string]any{"scale": scale, "plan_cache_mode": mode, "calls": len(calls), "client_wall_ms_per_call": calls,
		"call_1_ms": calls[0], "calls_7_to_20_median_ms": cpMedian(calls[6:]), "calls_2_to_20_median_ms": cpMedian(calls[1:]),
		"first_generic_call": cpSwitchCall(ph), "generic_plans_after_20": ph.GenericDeltaTotal, "custom_plans_after_20": ph.CustomDeltaTotal,
		"early_plan_counts_per_call": ph.EarlyPlanCounts, "pid": ph.PID, "timing_is_diagnostic_only": true}
	e.out.emit("scale_calls", rec)
	fmt.Printf("SCALECALLS|scale=%d|mode=%s|first_generic_call=%s|generic=%d|custom=%d|call1_ms=%.2f|med7_20_ms=%.3f\n",
		scale, mode, cpSwitchCall(ph), ph.GenericDeltaTotal, ph.CustomDeltaTotal, calls[0], cpMedian(calls[6:]))
}

// runScale is stage S for one history scale.
func (e *cpEnv) runScale(ctx context.Context, scale int) {
	t := e.t
	prof := e.profiles["p1"]
	bind := e.binding(ctx, prof)
	tplName, err := pmDBName(e.lab.prefix, fmt.Sprintf("_s%dtpl", scale))
	if err != nil {
		t.Fatalf("scale template name: %v", err)
	}
	if err := e.lab.create(ctx, tplName, "CREATE DATABASE "+pgx.Identifier{tplName}.Sanitize()+" TEMPLATE "+pgx.Identifier{e.lab.templateName}.Sanitize()); err != nil {
		t.Fatalf("scale %d: %v", scale, err)
	}
	defer func() {
		if err := e.lab.drop(tplName); err != nil {
			t.Errorf("drop scale template %s: %v", tplName, err)
		}
	}()
	seed := e.cpSeedScale(ctx, pmDSNForDB(t, e.lab.adminDSN, tplName), scale)
	e.out.emit("scale_seed", seed)
	fmt.Printf("SCALESEED|scale=%d|runs=%v|measured=%v|holds=%v|checks=%v\n", scale, seed["runs_total"], seed["runs_measured_user"], seed["custody_holds"], seed["cross_checks"])
	time.Sleep(500 * time.Millisecond) // the seed connection's backend must be gone before the first clone

	sl := *e.lab
	sl.templateName = tplName
	se := *e
	se.lab = &sl
	cell := func(mode, scen, tag string) cpCell {
		return cpCell{Stage: "S", Cand: cpBase, Mode: mode, Scen: scen, Prof: "p1", Rep: 1, N: 0, Warm: 19,
			Label: fmt.Sprintf("s%d_%s", scale, tag)}
	}

	// Scenario (a), auto: plan costs, then 20 idle calls on a fresh pool.
	sl.withClone(ctx, cell("auto", "a", "auto").Label, func(_, dsn string, obs *pmObserver) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		r := &cpRunner{env: &se, cell: cell("auto", "a", "auto"), prof: prof, bind: bind, dsn: dsn, obs: obs}
		defer r.cleanup(context.WithoutCancel(ctx))
		r.prepare(cctx)
		costs := se.planCosts(cctx, r, dsn, scale)
		e.out.emit("scale_plan_costs", costs)
		fmt.Printf("SCALECOST|scale=%d|generic_cost=%.1f\n", scale, costs["generic_plan_total_cost"])
		se.scaleCalls(cctx, r, scale, "auto")
		r.verifyClean(cctx)
	})
	// Scenario (a), force_generic_plan, on a fresh clone of the same scale.
	sl.withClone(ctx, cell("force_generic_plan", "a", "gen").Label, func(_, dsn string, obs *pmObserver) {
		cctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		defer cancel()
		r := &cpRunner{env: &se, cell: cell("force_generic_plan", "a", "gen"), prof: prof, bind: bind, dsn: dsn, obs: obs}
		defer r.cleanup(context.WithoutCancel(ctx))
		r.prepare(cctx)
		se.scaleCalls(cctx, r, scale, "force_generic_plan")
		r.verifyClean(cctx)
	})
	// Scenario (c): EXPLAIN ANALYZE under a generic and a custom plan (12 executions each).
	for _, m := range []string{"force_generic_plan", "force_custom_plan"} {
		c := cell(m, "c", "c"+cpModeShort[m])
		c.T3 = true
		se.runT3Cell(ctx, c)
	}
	e.out.emit("scale_done", map[string]any{"scale": scale})
}
