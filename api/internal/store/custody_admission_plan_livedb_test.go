package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// custodyPlanRecorder captures the statement and bindings from the generated
// method without executing it. Exec and Query retain the embedded DBTX behavior.
type custodyPlanRecorder struct {
	store.DBTX
	sql   string
	args  []any
	calls int
}

var errCustodyPlanCaptured = errors.New("custody plan statement captured")

func (r *custodyPlanRecorder) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	r.sql, r.args = sql, append([]any(nil), args...)
	r.calls++
	return custodyPlanCapturedRow{}
}

type custodyPlanCapturedRow struct{}

func (custodyPlanCapturedRow) Scan(...any) error { return errCustodyPlanCaptured }

// These names are deliberately local to the custody plan contract.
type custodyAdmissionPlanNode struct {
	NodeType           string                     `json:"Node Type"`
	ParentRelationship string                     `json:"Parent Relationship"`
	SubplanName        string                     `json:"Subplan Name"`
	RelationName       string                     `json:"Relation Name"`
	CTEName            string                     `json:"CTE Name"`
	Output             []string                   `json:"Output"`
	Filter             string                     `json:"Filter"`
	IndexCond          string                     `json:"Index Cond"`
	ActualLoops        float64                    `json:"Actual Loops"`
	ActualRows         float64                    `json:"Actual Rows"`
	RemovedByFilter    float64                    `json:"Rows Removed by Filter"`
	Plans              []custodyAdmissionPlanNode `json:"Plans"`
}

func custodyAdmissionPlanNodes(root *custodyAdmissionPlanNode) []*custodyAdmissionPlanNode {
	nodes := []*custodyAdmissionPlanNode{root}
	for i := range root.Plans {
		nodes = append(nodes, custodyAdmissionPlanNodes(&root.Plans[i])...)
	}
	return nodes
}

func explainCustodyAdmission(t *testing.T, f *admissionFixture, recorder *custodyPlanRecorder, err error) *custodyAdmissionPlanNode {
	t.Helper()
	if !errors.Is(err, errCustodyPlanCaptured) || recorder.calls != 1 || recorder.sql == "" {
		t.Fatalf("generated statement capture: calls=%d err=%v", recorder.calls, err)
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(f.ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback EXPLAIN transaction: %v", err)
		}
	}()
	var raw []byte
	if err := tx.QueryRow(f.ctx, "EXPLAIN (ANALYZE, VERBOSE, FORMAT JSON) "+recorder.sql, recorder.args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN generated statement: %v", err)
	}
	var result []struct {
		Plan custodyAdmissionPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode EXPLAIN: %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("EXPLAIN returned %d plans, want one", len(result))
	}
	return &result[0].Plan
}

func assertCustodyAdmissionOnce(t *testing.T, node *custodyAdmissionPlanNode) {
	t.Helper()
	if node.ActualLoops != 1 || node.ActualRows != 1 {
		t.Fatalf("%s %q: loops=%g rows=%g, want one evaluation returning one row",
			node.NodeType, node.SubplanName, node.ActualLoops, node.ActualRows)
	}
}

func custodyAdmissionFunctionProducer(t *testing.T, nodes []*custodyAdmissionPlanNode) *custodyAdmissionPlanNode {
	t.Helper()
	const function = "fn_custody_admission_count"
	var producer *custodyAdmissionPlanNode
	calls := 0
	for _, node := range nodes {
		output := strings.Join(node.Output, " ")
		calls += strings.Count(output, function) + strings.Count(node.Filter, function) + strings.Count(node.IndexCond, function)
		if strings.Contains(output, function) {
			if producer != nil {
				t.Fatal("multiple admission function output nodes")
			}
			producer = node
		}
	}
	if producer == nil || calls != 1 || producer.ParentRelationship != "InitPlan" {
		t.Fatalf("admission producer: node=%+v function references=%d, want one InitPlan output invocation", producer, calls)
	}
	assertCustodyAdmissionOnce(t, producer)
	return producer
}

// custodyAdmissionInitReference handles both PG17's "(InitPlan N).col1"
// and the older "InitPlan N (returns $M)" deparsing without fixing node numbers.
func custodyAdmissionInitReference(t *testing.T, producer *custodyAdmissionPlanNode) string {
	t.Helper()
	returned := regexp.MustCompile(`\(returns (\$[0-9]+)\)`).FindStringSubmatch(producer.SubplanName)
	if len(returned) == 2 {
		return returned[1]
	}
	if regexp.MustCompile(`^InitPlan [0-9]+$`).MatchString(producer.SubplanName) {
		return "(" + producer.SubplanName + ").col1"
	}
	t.Fatalf("unrecognized admission InitPlan reference %q", producer.SubplanName)
	return ""
}

// TestCustodyAdmissionProductionPlansLiveDB protects ADR-2445's once-per-statement
// accounting at the generated production query seam. Eight decision holds block
// four fresh candidates, forcing both statements to evaluate their admission value.
func TestCustodyAdmissionProductionPlansLiveDB(t *testing.T) {
	for _, name := range []string{"ClaimRun", "GetCustodyAggregateForOwner"} {
		t.Run(name, func(t *testing.T) {
			f := newAdmissionFixture(t, 8)
			for i, hold := range f.holds {
				f.capture(hold, f.runs[i], f.owner, "needs_action")
			}
			// newAdmissionFixture already creates the first fresh queued candidate.
			for i := 0; i < 3; i++ {
				f.run("queued", uuid.Nil, 0)
			}
			f.exec("ANALYZE runs")
			f.exec("ANALYZE recovery_custody_holds")
			f.exec("ANALYZE recovery_captures")
			params := store.GetCustodyAggregateForOwnerParams{
				UserID: f.owner, HeartbeatCutoff: f.cutoff, CustodyHoldLimit: 8,
			}
			facts, err := f.q.GetCustodyAggregateForOwner(f.ctx, params)
			if err != nil {
				t.Fatal(err)
			}
			if facts.OpenHolds != 8 || facts.AdmissionCountedHolds != 8 || facts.BlockedRuns != 4 {
				t.Fatalf("fixture aggregate=%+v, want open=8 counted=8 blocked=4", facts)
			}
			recorder := &custodyPlanRecorder{DBTX: f.pool}
			q := store.New(recorder)
			var captureErr error
			if name == "ClaimRun" {
				now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
				_, captureErr = q.ClaimRun(f.ctx, store.ClaimRunParams{
					UserID: f.owner, WorkerID: pgtype.UUID{Bytes: f.claimant, Valid: true},
					HeartbeatCutoff: f.cutoff, AffinityCutoff: now, SpreadCutoff: now,
					BackgroundGraceCutoff: now, CapabilityAware: true, WorkerCaps: []string{},
					WorkerProtocolCaps: []string{"recovery_archive_v1"}, CustodyHoldLimit: 8,
					RecoveryCapable: true, WorkerIdentity: f.claimant.String(),
				})
			} else {
				_, captureErr = q.GetCustodyAggregateForOwner(f.ctx, params)
			}
			root := explainCustodyAdmission(t, f, recorder, captureErr)
			nodes := custodyAdmissionPlanNodes(root)
			producer := custodyAdmissionFunctionProducer(t, nodes)
			if name == "ClaimRun" {
				reference := custodyAdmissionInitReference(t, producer)
				var candidate *custodyAdmissionPlanNode
				for _, node := range nodes {
					if node.RelationName == "runs" && strings.Contains(node.Filter, reference) {
						if candidate != nil {
							t.Fatal("multiple run filters consume the admission InitPlan")
						}
						candidate = node
					}
				}
				if candidate == nil {
					t.Fatalf("no runs candidate filter consumes admission reference %q", reference)
				}
				if strings.Contains(candidate.Filter, "fn_custody_admission_count") ||
					candidate.ActualLoops != 1 || candidate.ActualRows != 0 || candidate.RemovedByFilter != 4 {
					t.Fatalf("candidate filter: loops=%g rows=%g rejected=%g filter=%s, want all four rejected",
						candidate.ActualLoops, candidate.ActualRows, candidate.RemovedByFilter, candidate.Filter)
				}
				t.Logf("admission %s loops=1 rows=1; candidate %s loops=1 rows=0 rejected=4",
					producer.SubplanName, candidate.NodeType)
				return
			}
			if producer.SubplanName != "CTE admission" {
				t.Fatalf("admission producer=%q, want materialized CTE admission", producer.SubplanName)
			}
			var scan *custodyAdmissionPlanNode
			for _, node := range nodes {
				if node.NodeType == "CTE Scan" && node.CTEName == "admission" {
					if scan != nil {
						t.Fatal("multiple admission CTE scans")
					}
					scan = node
				}
			}
			if scan == nil {
				t.Fatal("missing admission CTE scan")
			}
			assertCustodyAdmissionOnce(t, scan)
			output := strings.Join(scan.Output, " ")
			if strings.Count(output, "admission.counted") < 2 || !strings.Contains(output, "CASE") {
				t.Fatalf("count output and CASE must reuse admission.counted: %s", output)
			}
			// Locate the executed blocked-run count branch by its aggregate and
			// runs descendant, without requiring a particular scan/index choice.
			branches := 0
			for _, node := range nodes {
				if node.ParentRelationship != "InitPlan" || !strings.Contains(strings.Join(node.Output, " "), "count(*)") {
					continue
				}
				for _, child := range custodyAdmissionPlanNodes(node) {
					if child.RelationName == "runs" && child.ActualLoops == 1 && child.ActualRows == 4 {
						assertCustodyAdmissionOnce(t, node)
						branches++
					}
				}
			}
			if branches != 1 {
				t.Fatalf("executed blocked-run count branches=%d, want one consuming four rows", branches)
			}
			t.Log("admission CTE producer and scan loops=1 rows=1; counted reused by output and CASE; blocked branch consumed four rows")
		})
	}
}
