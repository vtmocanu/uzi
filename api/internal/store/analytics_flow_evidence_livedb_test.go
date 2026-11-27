//go:build analyticsevidence

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/analytics"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4729 (m3): plan and latency evidence for the Flow statements (AnalyticsOwnerFlow and
// AnalyticsAllFlow). Same gate, build tag, throwaway-database marker and environment knobs as
// the Spend harness in analytics_evidence_livedb_test.go, whose seed it extends. Run:
//
//	cd api && UZI_ANALYTICS_PLAN_EVIDENCE=1 UZI_TEST_DATABASE_URL=... \
//	  go test -tags analyticsevidence -count=1 -v -timeout 50m \
//	  -run TestAnalyticsFlowPlanEvidenceLiveDB ./internal/store
//
// UZI_EVIDENCE_INDEXES here selects Flow candidates (flIndexes); it defaults to none, and
// "1,2,3" runs every candidate set.
//
// The run FAILS (t.Errorf, evidence still logged) when any Flow plan, custom or generic,
// contains a node on run_messages, a Seq Scan on run_user_inputs or run_gate_presentations,
// reaches run_user_inputs other than through idx_run_user_inputs_run_id, or reads a receipt CTE
// under a node that ran more than once (the per-run rescan), and when any workload's p95 or max
// reaches the 8 s statement_timeout.

// flSeed runs after evSeed on the same connection. It adds what the Flow statements read:
// realistic statuses (waiting states only among recent runs), first_started_at, plan gate
// state, run_gate_presentations, run_user_inputs (approve_plan receipts in several shapes plus
// other kinds) and run_messages (read by no Flow statement; present so a plan that touched it
// would show up).
var flSeed = []string{
	`SELECT setseed(0.4729)`,
	// The base seed leaves ~3% of ALL issue runs queued forever; keep queued only for the last 2 days.
	`UPDATE runs SET status = 'completed', finished_at = created_at + interval '45 minutes',
	                 status_since = created_at + interval '45 minutes'
	 WHERE status = 'queued' AND created_at < TIMESTAMPTZ '` + evNowRFC + `' - interval '2 days'`,
	// ~25% of the runs of the last 2 days sit in one of the 8 waiting states.
	`UPDATE runs SET status = (ARRAY['queued','awaiting_approval','awaiting_input','awaiting_followup','paused','limit_wait','pool_wait','recovery_wait'])[1 + (hashtext(id::text)::bigint & 2147483647) % 8],
	                 finished_at = NULL, status_since = created_at + interval '1 minute'
	 WHERE kind IN ('issue','task','self_improve','ci_fix','prompt')
	   AND created_at >= TIMESTAMPTZ '` + evNowRFC + `' - interval '2 days'
	   AND (hashtext(id::text)::bigint & 2147483647) % 100 < 25`,
	// Per-run plan gate shape. Issue/self_improve: 70% human gate (1 revision 60%, 2 28%, 3 12%),
	// 10% auto-approve, 10% seeded plan, 10% none; plan_rejected failures carry one rejected gate.
	`CREATE TEMP TABLE ev_cls AS
	 SELECT f.*,
	        CASE WHEN f.kind NOT IN ('issue','self_improve') THEN 'other'
	             WHEN f.status = 'queued' THEN 'none'
	             WHEN f.fail_origin = 'plan_rejected' THEN 'rejected'
	             WHEN f.ra < 0.70 THEN 'human' WHEN f.ra < 0.80 THEN 'auto' WHEN f.ra < 0.90 THEN 'seeded' ELSE 'none' END AS cls,
	        CASE WHEN f.kind NOT IN ('issue','self_improve') OR f.status = 'queued' THEN 0
	             WHEN f.fail_origin = 'plan_rejected' THEN 1
	             WHEN f.ra < 0.70 THEN CASE WHEN f.rb < 0.60 THEN 1 WHEN f.rb < 0.88 THEN 2 ELSE 3 END
	             ELSE 0 END AS k
	 FROM (SELECT id, kind, status, created_at, fail_origin, (hashtext(id::text)::bigint & 2147483647) AS h,
	              random() AS ra, random() AS rb, random() AS rc, random() AS rd, random() AS re
	       FROM runs) f`,
	`UPDATE runs r SET gate_revision = c.k,
	        auto_approve = (c.cls = 'auto'),
	        plan_source = CASE WHEN c.cls = 'seeded' THEN 'seeded' ELSE 'agent' END,
	        plan_md = CASE WHEN c.cls IN ('human','auto','seeded','rejected') OR (c.cls = 'other' AND c.kind <> 'chat' AND c.ra < 0.5)
	                       THEN repeat('plan ', 800) END,
	        first_started_at = CASE WHEN r.status = 'queued' OR c.h % 100 < 4 THEN NULL
	                                WHEN c.h % 1000 > 994 THEN r.created_at - interval '10 seconds'
	                                ELSE r.created_at + (5 + c.h % 600) * interval '1 second' END
	 FROM ev_cls c WHERE c.id = r.id`,
	`INSERT INTO run_gate_presentations (run_id, presentation_id, revision, created_at)
	 SELECT c.id, gen_random_uuid(), v, c.created_at + (120 + 3600 * (v - 1) + c.h % 600) * interval '1 second'
	 FROM ev_cls c CROSS JOIN generate_series(1, 3) v WHERE v <= c.k`,
	`CREATE TEMP TABLE ev_rc AS
	 SELECT c.id, c.k, c.rc, c.rd, c.h, c.created_at,
	        c.created_at + (120 + 3600 * (c.k - 1) + c.h % 600 + 20 + c.re * 7200) * interval '1 second' AS in_at
	 FROM ev_cls c WHERE c.cls = 'human'`,
	// The approve_plan receipt of a human gate, by band of rd: <5% ACK-only, 5-10% superseded,
	// 10-14% legacy (NULL binding), 14-17% unbound, 17-19% chronology-invalid, else clean applied
	// (disposition 'applied' for 85%, NULL for the rest).
	`INSERT INTO run_user_inputs (run_id, kind, created_at, consumed_at, applied_at, disposition, gate_binding, gate_revision)
	 SELECT c.id, 'approve_plan', c.in_at, c.in_at + interval '5 seconds',
	        CASE WHEN c.rd < 0.05 THEN NULL WHEN c.rd >= 0.17 AND c.rd < 0.19 THEN c.in_at - interval '1 hour'
	             ELSE c.in_at + interval '5 seconds' END,
	        CASE WHEN c.rd < 0.05 THEN NULL WHEN c.rd < 0.10 THEN 'superseded' WHEN c.rd < 0.17 THEN NULL
	             WHEN c.rd < 0.19 THEN 'applied' WHEN c.rc < 0.85 THEN 'applied' END,
	        CASE WHEN c.rd >= 0.10 AND c.rd < 0.14 THEN NULL WHEN c.rd >= 0.14 AND c.rd < 0.17 THEN 'unbound' ELSE 'bound' END,
	        CASE WHEN c.rd >= 0.10 AND c.rd < 0.17 THEN NULL ELSE c.k END
	 FROM ev_rc c`,
	// 2% duplicate eligible receipt on the same revision; 40% of multi-revision runs also carry a
	// superseded approval of the previous revision.
	`INSERT INTO run_user_inputs (run_id, kind, created_at, consumed_at, applied_at, disposition, gate_binding, gate_revision)
	 SELECT c.id, 'approve_plan', c.in_at + interval '1 second', c.in_at + interval '6 seconds', c.in_at + interval '6 seconds', 'applied', 'bound', c.k
	 FROM ev_rc c WHERE c.rd >= 0.19 AND c.rd < 0.21`,
	`INSERT INTO run_user_inputs (run_id, kind, created_at, consumed_at, applied_at, disposition, gate_binding, gate_revision)
	 SELECT c.id, 'approve_plan', c.in_at - interval '30 minutes', c.in_at - interval '30 minutes' + interval '5 seconds',
	        c.in_at - interval '30 minutes' + interval '5 seconds', 'superseded', 'bound', c.k - 1
	 FROM ev_rc c WHERE c.k > 1 AND c.rc > 0.6`,
	`INSERT INTO run_user_inputs (run_id, kind, created_at, consumed_at, applied_at, disposition, gate_binding, gate_revision)
	 SELECT c.id, 'revise_plan', c.created_at + (720 + 3600 * (v - 1) + c.h % 600) * interval '1 second',
	        c.created_at + (725 + 3600 * (v - 1) + c.h % 600) * interval '1 second',
	        c.created_at + (725 + 3600 * (v - 1) + c.h % 600) * interval '1 second', 'applied', 'bound', v
	 FROM ev_rc c CROSS JOIN generate_series(1, 2) v WHERE v < c.k`,
	`INSERT INTO run_user_inputs (run_id, kind, created_at, consumed_at, applied_at, disposition, gate_binding, gate_revision)
	 SELECT c.id, 'reject_plan', c.created_at + (180 + c.h % 600) * interval '1 second',
	        c.created_at + (185 + c.h % 600) * interval '1 second', c.created_at + (185 + c.h % 600) * interval '1 second', 'applied', 'bound', 1
	 FROM ev_cls c WHERE c.cls = 'rejected'`,
	// Other input kinds: 0-5 per non-chat run (average 2.5).
	`INSERT INTO run_user_inputs (run_id, kind, body, created_at, consumed_at, applied_at)
	 SELECT x.id, CASE WHEN x.m < 45 THEN 'follow_up' WHEN x.m < 70 THEN 'scope' WHEN x.m < 80 THEN 'pause'
	                   WHEN x.m < 90 THEN 'resume' ELSE 'cancel' END,
	        'x', x.ts, x.ts + interval '10 seconds', x.ts + interval '10 seconds'
	 FROM (SELECT c.id, ((c.h / 7 + n * 13) % 100) AS m, c.created_at + n * interval '10 minutes' AS ts
	       FROM ev_cls c CROSS JOIN generate_series(1, 5) n WHERE c.kind <> 'chat' AND n <= c.h % 6) x`,
	// run_messages: 6-35 rows per run (average 20.5); human multi-revision runs carry plan and
	// plan_revising rows.
	`INSERT INTO run_messages (run_id, seq, kind, agent, payload, created_at)
	 SELECT m.id, m.s, m.kind, CASE WHEN m.kind IN ('tool_use', 'tool_result') THEN 'coder' ELSE 'lead' END,
	        CASE WHEN m.kind IN ('plan', 'plan_revising')
	             THEN jsonb_build_object('plan_md', (SELECT string_agg(md5(m.id::text || m.s || g), '') FROM generate_series(1, 30) g))
	             ELSE jsonb_build_object('text', md5(m.id::text || m.s) || md5(m.s || m.id::text) || md5(m.id::text || m.s || 'z')) END,
	        m.created_at + m.s * interval '20 seconds'
	 FROM (SELECT c.id, c.created_at, s,
	              CASE WHEN s = 1 THEN 'status'
	                   WHEN s = 2 THEN CASE WHEN c.cls IN ('human', 'auto', 'seeded', 'rejected') THEN 'plan' ELSE 'text' END
	                   WHEN c.cls = 'human' AND c.k > 1 AND s <= 2 * c.k THEN CASE WHEN s % 2 = 1 THEN 'plan_revising' ELSE 'plan' END
	                   ELSE (ARRAY['tool_use','tool_result','text','thinking','status','text','tool_result'])[1 + s % 7] END AS kind
	       FROM ev_cls c CROSS JOIN LATERAL generate_series(1, 6 + c.h % 30) s) m`,
	`DROP TABLE ev_rc`, `DROP TABLE ev_cls`,
	`VACUUM FULL runs`,
	`ANALYZE runs`, `ANALYZE run_user_inputs`, `ANALYZE run_gate_presentations`, `ANALYZE run_messages`,
}

// flIndexes are the Flow candidate indexes, built in the evidence database only.
var flIndexes = []struct{ name, ddl string }{
	{"evid_runs_terminal_end_idx", `CREATE INDEX evid_runs_terminal_end_idx ON runs ((COALESCE(finished_at, status_since))) WHERE status IN ('completed','failed','cancelled')`},
	{"evid_runs_created_idx", `CREATE INDEX evid_runs_created_idx ON runs (created_at)`},
}

// flCandidates maps the UZI_EVIDENCE_INDEXES numbers to the index names built together.
var flCandidates = map[int][]int{1: {0}, 2: {1}, 3: {0, 1}}

func (h *evHarness) flowSeed() {
	h.t.Helper()
	if os.Getenv("UZI_EVIDENCE_RESEED") != "0" {
		start := time.Now()
		for _, s := range flSeed {
			h.simple(s)
		}
		h.logf("flow seed extension took %s", time.Since(start).Round(time.Second))
	}
	h.logf("FLOW SEED SHAPE:")
	for _, q := range []string{
		`SELECT 'runs 7d share', round(100.0 * count(*) FILTER (WHERE created_at >= TIMESTAMPTZ '` + evNowRFC + `' - interval '7 days') / count(*), 2) FROM runs`,
		`SELECT 'runs rows', count(*) FROM runs`,
		`SELECT 'run_user_inputs rows', count(*) FROM run_user_inputs`,
		`SELECT 'run_user_inputs by kind', string_agg(kind || '=' || c, ' ' ORDER BY kind) FROM (SELECT kind, count(*) c FROM run_user_inputs GROUP BY kind) x`,
		`SELECT 'approve_plan by shape', string_agg(k || '=' || c, ' ' ORDER BY k) FROM (
		   SELECT CASE WHEN gate_binding IS NULL THEN 'legacy_null' WHEN gate_binding = 'unbound' THEN 'unbound'
		               WHEN applied_at IS NULL THEN 'ack_only' WHEN disposition = 'superseded' THEN 'superseded'
		               WHEN disposition IS NULL THEN 'applied_null_disp' ELSE 'applied' END AS k, count(*) c
		   FROM run_user_inputs WHERE kind = 'approve_plan' GROUP BY 1) x`,
		`SELECT 'issue/self_improve runs with an approve_plan row %', round(100.0 * count(*) FILTER (WHERE EXISTS (SELECT 1 FROM run_user_inputs i WHERE i.run_id = r.id AND i.kind = 'approve_plan')) / count(*), 1) FROM runs r WHERE r.kind IN ('issue','self_improve')`,
		`SELECT 'issue/self_improve gate_revision mix', string_agg(gate_revision || '=' || c, ' ' ORDER BY gate_revision) FROM (SELECT gate_revision, count(*) c FROM runs WHERE kind IN ('issue','self_improve') GROUP BY 1) x`,
		`SELECT 'run_gate_presentations rows', count(*) FROM run_gate_presentations`,
		`SELECT 'run_messages rows', count(*) FROM run_messages`,
		`SELECT 'run_messages per run avg', round(count(*)::numeric / (SELECT count(*) FROM runs), 1) FROM run_messages`,
		`SELECT 'run_messages plan/plan_revising rows', count(*) FROM run_messages WHERE kind IN ('plan','plan_revising')`,
		`SELECT 'first_started_at NULL % (non-queued)', round(100.0 * count(*) FILTER (WHERE first_started_at IS NULL) / count(*), 1) FROM runs WHERE status <> 'queued'`,
		`SELECT 'waiting-state runs', string_agg(status || '=' || c, ' ' ORDER BY status) FROM (SELECT status, count(*) c FROM runs WHERE status NOT IN ('completed','failed','cancelled') GROUP BY 1) x`,
		`SELECT 'status mix', string_agg(status || '=' || c, ' ' ORDER BY status) FROM (SELECT status, count(*) c FROM runs GROUP BY 1) x`,
		`SELECT 'median-rank owner runs (rank 100 of 200)', c FROM (SELECT count(*) c, row_number() OVER (ORDER BY count(*) DESC, user_id) rk FROM runs GROUP BY user_id) x WHERE rk = 100`,
		`SELECT 'largest owner runs', max(c) FROM (SELECT count(*) c FROM runs GROUP BY user_id) x`,
	} {
		h.logf("%s", h.shape(q))
	}
	for _, rel := range []string{"runs", "run_user_inputs", "run_gate_presentations", "run_messages"} {
		var rs, is, ts int64
		h.scalar(fmt.Sprintf("SELECT pg_relation_size('%[1]s'), pg_indexes_size('%[1]s'), pg_total_relation_size('%[1]s')", rel), &rs, &is, &ts)
		h.logf("  SIZE %-24s relation=%d indexes=%d total=%d bytes", rel, rs, is, ts)
	}
}

// flLiteral renders a Flow argument as a typed SQL literal: the Spend helper plus the two
// Flow-only types (int32 min_percentile_n and the float8[] histogram edges).
func flLiteral(t *testing.T, a any) string {
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

// flowWorkload builds one Flow workload over the owner statement (owner != uuid.Nil) or the
// all-scope statement, with bounds from analytics.ComputeBounds at the harness's fixed now.
func (h *evHarness) flowWorkload(name string, owner uuid.UUID, win analytics.Window) flWorkload {
	h.t.Helper()
	kinds := runkind.All()
	var hist pgtype.Timestamptz
	var err error
	if owner != uuid.Nil {
		hist, err = h.q.AnalyticsOwnerHistoryStart(h.ctx, store.AnalyticsOwnerHistoryStartParams{UserID: owner, Kinds: kinds})
	} else {
		hist, err = h.q.AnalyticsAllHistoryStart(h.ctx, store.AnalyticsAllHistoryStartParams{Kinds: kinds})
	}
	if err != nil {
		h.t.Fatalf("history start: %v", err)
	}
	var earliest *time.Time
	if hist.Valid {
		e := hist.Time
		earliest = &e
	}
	b, err := analytics.ComputeBounds(h.now, time.UTC, win, earliest)
	if err != nil {
		h.t.Fatalf("ComputeBounds %s: %v", name, err)
	}
	ownerP := store.AnalyticsOwnerFlowParams{
		UserID: owner, Now: spendTS(b.Now), From: spendTS(b.From),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN, Kinds: kinds,
	}
	allP := store.AnalyticsAllFlowParams{
		Now: spendTS(b.Now), From: spendTS(b.From),
		BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
		HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN, Kinds: kinds,
	}
	capture := &capturingDBTX{}
	cq := store.New(capture)
	w := evWorkload{name: name}
	if owner != uuid.Nil {
		_, _ = cq.AnalyticsOwnerFlow(h.ctx, ownerP)
		w.stmts = []evStmt{{name: "AnalyticsOwnerFlow", sql: store.AnalyticsOwnerFlowSQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsOwnerFlow(ctx, ownerP)
			return err
		}
	} else {
		_, _ = cq.AnalyticsAllFlow(h.ctx, allP)
		w.stmts = []evStmt{{name: "AnalyticsAllFlow", sql: store.AnalyticsAllFlowSQL}}
		w.run = func(ctx context.Context, q *store.Queries) error {
			_, err := q.AnalyticsAllFlow(ctx, allP)
			return err
		}
	}
	lits := make([]string, len(capture.args))
	for i, a := range capture.args {
		lits[i] = flLiteral(h.t, a)
	}
	w.stmts[0].args = strings.Join(lits, ", ")
	if dir := os.Getenv("UZI_EVIDENCE_DUMP_DIR"); dir != "" {
		// A reproducible psql script (PREPARE + EXECUTE with literal arguments) per workload.
		file := filepath.Join(filepath.Clean(dir), strings.NewReplacer(" ", "_", "(", "", ")", "", ";", "").Replace(name)+".sql")
		script := "PREPARE evstmt AS " + w.stmts[0].sql + ";\nEXECUTE evstmt(" + w.stmts[0].args + ");\n"
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil { //nolint:gosec // G306/G703: operator-chosen dump directory of a measurement harness that never runs in CI
			h.t.Fatalf("dump %s: %v", file, err)
		}
	}
	scope := ""
	if owner != uuid.Nil {
		scope = fmt.Sprintf("user_id = '%s' AND ", owner)
	}
	var cohort, total int64
	h.scalar(fmt.Sprintf(`SELECT count(*) FILTER (WHERE %[1]screated_at >= '%[2]s' AND created_at < '%[3]s'), count(*) FILTER (WHERE %[1]strue) FROM runs`,
		scope, b.From.UTC().Format(time.RFC3339Nano), b.Now.UTC().Format(time.RFC3339Nano)), &cohort, &total)
	h.logf("WORKLOAD %s: window=%s buckets=%d from=%s cohort_runs=%d scope_runs=%d", name, win, len(b.BucketStarts), b.From.Format(time.RFC3339), cohort, total)
	return flWorkload{evWorkload: w, request: h.flowRequest(owner, win)}
}

// flWorkload is a Flow workload plus its request-path run.
type flWorkload struct {
	evWorkload
	request func(ctx context.Context) error
}

// flowRequest is what one Flow request does on the database side:
// store.RunAnalyticsSnapshot (REPEATABLE READ READ ONLY, its real 8 s deadline, statement_timeout
// = 8s and jit = off) around the history-start query, ComputeBounds (in UTC, not the caller's
// zone), the Flow statement and store.DecodeAnalyticsFlowDoc. It runs on the pool, so pgx's
// default statement cache applies. It excludes HTTP, auth middleware, admission
// (deps.admission.Acquire), in-snapshot authorization (tz-name check, AnalyticsUserExists for an
// admin user=<uuid>, repo checks), the DTO mapping (mapAnalyticsFlow), JSON encoding of the DTO
// and the network.
func (h *evHarness) flowRequest(owner uuid.UUID, win analytics.Window) func(ctx context.Context) error {
	kinds := runkind.All()
	return func(ctx context.Context) error {
		return store.RunAnalyticsSnapshot(ctx, h.pool, func(ctx context.Context, s *store.AnalyticsSnapshot) error {
			var hist pgtype.Timestamptz
			var err error
			if owner != uuid.Nil {
				hist, err = s.Q.AnalyticsOwnerHistoryStart(ctx, store.AnalyticsOwnerHistoryStartParams{UserID: owner, Kinds: kinds})
			} else {
				hist, err = s.Q.AnalyticsAllHistoryStart(ctx, store.AnalyticsAllHistoryStartParams{Kinds: kinds})
			}
			if err != nil {
				return err
			}
			var earliest *time.Time
			if hist.Valid {
				e := hist.Time
				earliest = &e
			}
			b, err := analytics.ComputeBounds(h.now, time.UTC, win, earliest)
			if err != nil {
				return err
			}
			var raw []byte
			if owner != uuid.Nil {
				raw, err = s.Q.AnalyticsOwnerFlow(ctx, store.AnalyticsOwnerFlowParams{
					UserID: owner, Now: spendTS(b.Now), From: spendTS(b.From),
					BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
					HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN, Kinds: kinds,
				})
			} else {
				raw, err = s.Q.AnalyticsAllFlow(ctx, store.AnalyticsAllFlowParams{
					Now: spendTS(b.Now), From: spendTS(b.From),
					BucketStarts: spendTSs(b.BucketStarts), BucketEnds: spendTSs(b.BucketEnds),
					HistogramEdges: store.FlowHistogramEdgesSeconds, MinPercentileN: store.FlowMinPercentileN, Kinds: kinds,
				})
			}
			if err != nil {
				return err
			}
			_, err = store.DecodeAnalyticsFlowDoc(raw)
			return err
		})
	}
}

func (h *evHarness) flowWorkloads() []flWorkload {
	var median uuid.UUID
	h.scalar(`SELECT user_id FROM (SELECT user_id, row_number() OVER (ORDER BY count(*) DESC, user_id) rk FROM runs GROUP BY user_id) x WHERE rk = 100`, &median)
	var all []flWorkload
	for _, o := range []struct {
		label string
		id    uuid.UUID
	}{{"top", h.topOwner}, {"median", median}} {
		for _, win := range []analytics.Window{analytics.Window7d, analytics.Window30d, analytics.Window90d, analytics.WindowAll} {
			all = append(all, h.flowWorkload("owner-"+o.label+"-"+string(win), o.id, win))
		}
	}
	// The admin route refuses all-users window=all; admin selected-user runs the owner statement.
	all = append(all,
		h.flowWorkload("admin-all-30d", uuid.Nil, analytics.Window30d),
		h.flowWorkload("admin-all-90d", uuid.Nil, analytics.Window90d),
		h.flowWorkload("admin-user-all (top owner; owner statement)", h.topOwner, analytics.WindowAll),
	)
	only := os.Getenv("UZI_EVIDENCE_ONLY")
	if only == "" {
		return all
	}
	var out []flWorkload
	for _, w := range all {
		for _, f := range strings.Split(only, ",") {
			if f != "" && strings.Contains(w.name, f) {
				out = append(out, w)
				break
			}
		}
	}
	if len(out) == 0 {
		names := make([]string, len(all))
		for i, w := range all {
			names[i] = w.name
		}
		h.t.Fatalf("UZI_EVIDENCE_ONLY=%q matches no workload (available: %s)", only, strings.Join(names, ", "))
	}
	names := make([]string, len(out))
	for i, w := range out {
		names[i] = w.name
	}
	h.logf("UZI_EVIDENCE_ONLY=%q selected %d of %d workloads: %s", only, len(out), len(all), strings.Join(names, ", "))
	return out
}

// flRecorder checks every Flow plan against the contract and collects the timings.
type flRecorder struct {
	h      *evHarness
	rows   []flRow
	phases []string
	// requestRows are the formatted request-path summary rows.
	requestRows []string
	// partial is non-empty when UZI_EVIDENCE_ONLY selected a subset of the workloads.
	partial string
}

type flRow struct {
	phase, workload, mode string
	tm                    evTiming
}

func flLabels(p *evPlan, sub string) []string {
	var out []string
	for _, l := range p.scanLines() {
		if strings.Contains(l, sub) {
			out = append(out, strings.Join(strings.Fields(l), " "))
		}
	}
	return out
}

// flInputIndex is the only index a Flow plan may use to reach run_user_inputs.
const flInputIndex = "idx_run_user_inputs_run_id"

// flInputScanProblems checks EVERY plan node that touches run_user_inputs: it must be an Index
// Scan or Index Only Scan on flInputIndex whose Index Cond references run_id, or a Bitmap Heap
// Scan fed by a Bitmap Index Scan on flInputIndex with such a condition. An index that merely
// appears elsewhere in the plan does not satisfy another node.
func flInputScanProblems(n map[string]any) []string {
	var out []string
	condOK := func(m map[string]any) bool {
		return evStr(m, "Index Name") == flInputIndex && strings.Contains(evStr(m, "Index Cond"), "run_id")
	}
	var bitmapOK func(m map[string]any) bool
	bitmapOK = func(m map[string]any) bool {
		if evStr(m, "Node Type") == "Bitmap Index Scan" && condOK(m) {
			return true
		}
		for _, k := range evKids(m) {
			if bitmapOK(k) {
				return true
			}
		}
		return false
	}
	var walk func(m map[string]any)
	walk = func(m map[string]any) {
		nt := evStr(m, "Node Type")
		if evStr(m, "Relation Name") == "run_user_inputs" {
			ok := false
			switch nt {
			case "Index Scan", "Index Only Scan":
				ok = condOK(m)
			case "Bitmap Heap Scan":
				ok = bitmapOK(m)
			}
			if !ok {
				out = append(out, fmt.Sprintf("run_user_inputs node %q (index %q, cond %q) is not an index scan on %s with run_id in its Index Cond",
					nt, evStr(m, "Index Name"), evStr(m, "Index Cond"), flInputIndex))
			}
		}
		for _, k := range evKids(m) {
			walk(k)
		}
	}
	walk(n)
	return out
}

func (r *flRecorder) plan(w evWorkload, mode string, st evStmt, p *evPlan) {
	h := r.h
	ctx := fmt.Sprintf("%s %s plan_cache_mode=%s", st.name, w.name, mode)
	for label := range p.scans {
		switch {
		case strings.Contains(label, "run_messages"):
			h.t.Errorf("%s: plan touches run_messages: %s", ctx, label)
		case strings.Contains(label, "Seq Scan on run_user_inputs"), strings.Contains(label, "Seq Scan on run_gate_presentations"):
			h.t.Errorf("%s: sequential scan of a gate-evidence table: %s", ctx, label)
		}
	}
	for _, problem := range flInputScanProblems(p.root) {
		h.t.Errorf("%s: %s", ctx, problem)
	}
	if blob, err := json.Marshal(p.root); err != nil {
		h.t.Errorf("%s: re-encode plan: %v", ctx, err)
	} else {
		var root planNode
		if err := json.Unmarshal(blob, &root); err != nil {
			h.t.Errorf("%s: decode plan: %v", ctx, err)
		} else {
			rescans, scans := flowRescans(&root, "")
			if scans == 0 {
				h.t.Errorf("%s: no CTE Scan of a receipt CTE in the plan; the rescan check is vacuous", ctx)
			}
			for _, r := range rescans {
				h.t.Errorf("%s: receipt CTE re-read per outer row: %s", ctx, r)
			}
		}
	}
	h.logf("ACCESS PATH [%s]", ctx)
	h.logf("   finished-time range / terminal idx: runs_user_terminal_end_idx=%v evid_runs_terminal_end_idx=%v",
		p.indexNames["runs_user_terminal_end_idx"], p.indexNames["evid_runs_terminal_end_idx"])
	h.logf("   creation range: runs_user_created_idx=%v idx_runs_claimable=%v evid_runs_created_idx=%v idx_runs_user=%v runs_user_nonterminal_idx=%v",
		p.indexNames["runs_user_created_idx"], p.indexNames["idx_runs_claimable"], p.indexNames["evid_runs_created_idx"], p.indexNames["idx_runs_user"], p.indexNames["runs_user_nonterminal_idx"])
	for _, sub := range []string{"on runs", "run_gate_presentations", "run_user_inputs", "run_messages"} {
		ls := flLabels(p, sub)
		if sub == "on runs" {
			ls = append(ls, flLabels(p, "using idx_runs")...)
			ls = append(ls, flLabels(p, "using runs_")...)
		}
		h.logf("   %-24s %s", sub+":", strings.Join(ls, " | "))
	}
	h.logf("   run_messages node present=%v; gate-evidence Seq Scan present=%v; run_user_inputs via idx_run_user_inputs_run_id=%v",
		len(flLabels(p, "run_messages")) > 0,
		len(flLabels(p, "Seq Scan on run_user_inputs"))+len(flLabels(p, "Seq Scan on run_gate_presentations")) > 0,
		p.indexNames["idx_run_user_inputs_run_id"])
}

func (r *flRecorder) timing(phase string, w evWorkload, mode string, tm evTiming) {
	r.rows = append(r.rows, flRow{phase, w.name, mode, tm})
	seen := false
	for _, p := range r.phases {
		seen = seen || p == phase
	}
	if !seen {
		r.phases = append(r.phases, phase)
	}
	switch {
	case tm.p95 >= 8*time.Second || tm.max >= 8*time.Second:
		r.h.t.Errorf("OVER the 8s statement_timeout: [%s] %s mode=%s p95=%.1fms max=%.1fms", phase, w.name, mode, ms(tm.p95), ms(tm.max))
		r.h.logf("!!!! OVER the 8s statement_timeout: [%s] %s mode=%s p95=%.1fms max=%.1fms", phase, w.name, mode, ms(tm.p95), ms(tm.max))
	case tm.p95 >= 4*time.Second:
		r.h.logf("!!!! NEAR the 8s statement_timeout: [%s] %s mode=%s p95=%.1fms max=%.1fms", phase, w.name, mode, ms(tm.p95), ms(tm.max))
	}
}

func (r *flRecorder) table() {
	if r.partial != "" {
		r.h.logf("\n#### PARTIAL SELECTION: this summary covers only %s; it is not the full workload matrix", r.partial)
	}
	if len(r.requestRows) > 0 {
		r.h.logf("\n#### SUMMARY [%s]: ms (requests | timeouts | first | median / p95 | max); excludes HTTP, auth middleware, admission, in-snapshot authorization, DTO mapping, JSON encoding of the DTO and network", flRequestPhase)
		for _, row := range r.requestRows {
			r.h.logf("%s", row)
		}
	}
	for _, phase := range r.phases {
		if phase == flRequestPhase {
			continue
		}
		r.h.logf("\n#### SUMMARY [%s]: median / p95 ms (custom | generic)", phase)
		type cell struct{ med, p95 float64 }
		by := map[string]map[string]cell{}
		var names []string
		for _, row := range r.rows {
			if row.phase != phase {
				continue
			}
			if by[row.workload] == nil {
				by[row.workload] = map[string]cell{}
				names = append(names, row.workload)
			}
			by[row.workload][row.mode] = cell{ms(row.tm.median), ms(row.tm.p95)}
		}
		for _, n := range names {
			c, g := by[n]["custom"], by[n]["generic"]
			r.h.logf("| %-46s | %9.1f / %9.1f | %9.1f / %9.1f |", n, c.med, c.p95, g.med, g.p95)
		}
	}
}

// flRequestPhase is the phase name of the request-path timings.
const flRequestPhase = "request-path"

// requestPhase times flowRequest per workload on the pool with pgx's default statement cache.
// The requests run on a warm pooled connection: by default the baseline phase has already run
// through the same pool and statement cache, so the Flow statement is already prepared and its
// plan choice settled (workloads that share a statement share one prepared statement). The
// history-start statements run once per built workload while flowWorkloads builds the full
// list, before the UZI_EVIDENCE_ONLY filter and the baseline (9 owner and 2 all-users
// executions over the pool), so the all-users plan may still switch inside the sample and
// the owner plan's state on the request connection is not checked. No
// plan_cache_mode is forced, so any generic plan in use is whatever Postgres auto mode chose.
// Each workload runs max(UZI_EVIDENCE_RUNS, 12) requests. A 57014 or any other timeout is a
// test failure; the sample is kept either way.
func (r *flRecorder) requestPhase(workloads []flWorkload) {
	h := r.h
	n := max(h.runs, 12)
	h.logf("\n######## FLOW REQUEST PATH: RunAnalyticsSnapshot (8s statement_timeout, jit=off) + history start + Flow statement + DecodeAnalyticsFlowDoc; pool, default statement cache, %d requests per workload ########", n)
	h.logf("EXCLUDES: HTTP, auth middleware, admission, in-snapshot authorization, DTO mapping, JSON encoding of the DTO, network. Bounds are computed in UTC.")
	for _, w := range workloads {
		durs := make([]time.Duration, 0, n)
		timeouts := 0
		for i := 0; i < n; i++ {
			start := time.Now()
			err := w.request(h.ctx)
			d := time.Since(start)
			durs = append(durs, d)
			if err != nil {
				if errors.Is(store.ClassifyAnalyticsErr(err), store.ErrAnalyticsTimeout) {
					timeouts++
					h.t.Errorf("request path %s request %d: 57014 / analytics timeout after %.1fms: %v", w.name, i+1, ms(d), err)
				} else {
					h.t.Errorf("request path %s request %d: %v", w.name, i+1, err)
				}
			}
		}
		first := durs[0]
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		tm := evTiming{n: len(durs), max: durs[len(durs)-1], p95: durs[int(math.Ceil(0.95*float64(len(durs))))-1]}
		if len(durs)%2 == 1 {
			tm.median = durs[len(durs)/2]
		} else {
			tm.median = (durs[len(durs)/2-1] + durs[len(durs)/2]) / 2
		}
		h.logf("TIMING %s %s n=%d timeouts=%d first=%.1fms median=%.1fms p95=%.1fms max=%.1fms", flRequestPhase, w.name, tm.n, timeouts, ms(first), ms(tm.median), ms(tm.p95), ms(tm.max))
		r.timing(flRequestPhase, w.evWorkload, "request", tm)
		r.requestRows = append(r.requestRows, fmt.Sprintf("| %-46s | %3d | %2d | %9.1f | %9.1f / %9.1f | %9.1f |", w.name, tm.n, timeouts, ms(first), ms(tm.median), ms(tm.p95), ms(tm.max)))
	}
}

func TestAnalyticsFlowPlanEvidenceLiveDB(t *testing.T) {
	h := newEvidenceHarness(t)
	pool := h.pool
	h.simple(`SET statement_timeout = 0`)
	dropIndexes := func(exec func(string)) {
		for _, ix := range flIndexes {
			exec("DROP INDEX IF EXISTS " + ix.name)
		}
	}
	dropIndexes(h.simple)
	if os.Getenv("UZI_EVIDENCE_KEEP") != "1" {
		t.Cleanup(func() {
			dropIndexes(func(s string) { _, _ = pool.Exec(context.Background(), s) })
			_, _ = pool.Exec(context.Background(), `TRUNCATE users CASCADE`)
		})
	}

	if os.Getenv("UZI_EVIDENCE_RESEED") != "0" {
		h.seed()
	} else {
		h.scalar(`SELECT id FROM users WHERE email = 'evid-0001@e2e'`, &h.topOwner)
	}
	h.flowSeed()
	workloads := h.flowWorkloads()
	rec := &flRecorder{h: h}
	if only := os.Getenv("UZI_EVIDENCE_ONLY"); only != "" {
		names := make([]string, len(workloads))
		for i, w := range workloads {
			names[i] = w.name
		}
		rec.partial = fmt.Sprintf("UZI_EVIDENCE_ONLY=%q (%s)", only, strings.Join(names, ", "))
	}
	h.onPlan, h.onTiming = rec.plan, rec.timing

	if os.Getenv("UZI_EVIDENCE_BASELINE") != "0" {
		h.logf("\n######## FLOW BASELINE (no candidate index) ########")
		for _, w := range workloads {
			h.measure("baseline", w.evWorkload, h.modes, true)
		}
	}
	if os.Getenv("UZI_EVIDENCE_REQUEST") != "0" {
		rec.requestPhase(workloads)
	}
	// Candidate indexes are optional extras judged on the shipped statements; UZI_EVIDENCE_INDEXES
	// selects them (default none, "1,2,3" runs every candidate set).
	list := ""
	if v, ok := os.LookupEnv("UZI_EVIDENCE_INDEXES"); ok {
		list = v
	}
	for _, s := range strings.Split(list, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || flCandidates[n] == nil {
			continue
		}
		var names, ddls []string
		for _, i := range flCandidates[n] {
			names, ddls = append(names, flIndexes[i].name), append(ddls, flIndexes[i].ddl)
		}
		h.logf("\n######## FLOW CANDIDATE %d: %s ########", n, strings.Join(ddls, "; "))
		start := time.Now()
		for _, d := range ddls {
			h.simple(d)
		}
		h.simple(`ANALYZE runs`)
		for _, nm := range names {
			var size int64
			h.scalar(fmt.Sprintf("SELECT pg_relation_size('%s')", nm), &size)
			h.logf("CREATE INDEX + ANALYZE took %s; %s size %d bytes", time.Since(start).Round(time.Millisecond), nm, size)
		}
		for _, w := range workloads {
			h.measure("idx"+strconv.Itoa(n), w.evWorkload, h.modes, true, names...)
		}
		dropIndexes(h.simple)
		h.simple(`ANALYZE runs`)
	}
	rec.table()
}
