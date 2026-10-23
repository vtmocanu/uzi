package workersvc

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Record both prepared-plan modes on substantial unrelated history and one heavily
// refreshed branch. This is an executable query-plan probe, not a timing threshold.
func TestPublicationContextQueryPlanLiveDB(t *testing.T) {
	f := seedSourcePublication(t, false)
	if r, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req); err != nil || !r.Applied {
		t.Fatalf("source: %+v %v", r, err)
	}
	branch := *f.req.Branch
	f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,branch,mr_iid,issue_title,issue_description)
 SELECT gen_random_uuid(),$1,$2,'prompt','completed','unrelated/'||g,7,'history','' FROM generate_series(1,20000) g`, f.w.UserID, f.e.repoID)
	f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 SELECT gen_random_uuid(),$1,$2,'mr_rework','completed',$3,7,$4,'descendant','' FROM generate_series(1,2000)`, f.w.UserID, f.e.repoID, branch, f.id)
	id := uuid.New()
	f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,pipeline_id,issue_title,issue_description)
 VALUES($1,$2,$3,'ci_fix','running',$4,1,$5,7,123,'refresh','')`, id, f.w.UserID, f.e.repoID, f.w.ID, branch)
	f.e.exec(t, "ANALYZE runs")
	raw, err := os.ReadFile("../store/queries/source_publication.sql")
	if err != nil {
		t.Fatal(err)
	}
	query := strings.Split(string(raw), "-- name: BindSourcePublicationIntent")[0]
	query = strings.ReplaceAll(strings.ReplaceAll(query, "@run_id", "$1"), "@user_id", "$2")
	for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
		t.Run(mode, func(t *testing.T) {
			tx, err := f.e.pool.Begin(f.e.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(f.e.ctx) }()
			q := store.New(tx)
			if _, err := q.GetWorkerForUpdate(f.e.ctx, f.w.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := q.GetRunOwnedByWorkerForUpdate(f.e.ctx, store.GetRunOwnedByWorkerForUpdateParams{ID: id, WorkerID: pgconv.UUID(f.w.ID)}); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(f.e.ctx, "SET LOCAL plan_cache_mode = "+mode); err != nil {
				t.Fatal(err)
			}
			name := "publication_context_" + mode
			if _, err := tx.Prepare(f.e.ctx, name, query); err != nil {
				t.Fatal(err)
			}
			var plan []byte
			// These literals are generated UUIDs, never caller-controlled SQL identifiers.
			explain := fmt.Sprintf("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) EXECUTE %s ('%s','%s')", name, id, f.w.UserID)
			if err := tx.QueryRow(f.e.ctx, explain).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			var docs []map[string]any
			if err := json.Unmarshal(plan, &docs); err != nil || len(docs) != 1 {
				t.Fatalf("plan decode: %v", err)
			}
			t.Logf("history=20000 descendants=2000 planning_ms=%v execution_ms=%v", docs[0]["Planning Time"], docs[0]["Execution Time"])
			var visit func(map[string]any)
			visit = func(n map[string]any) {
				if n["Relation Name"] == "runs" {
					t.Logf("runs scan=%v index=%v actual_rows=%v loops=%v removed=%v shared_hits=%v shared_reads=%v", n["Node Type"], n["Index Name"], n["Actual Rows"], n["Actual Loops"], n["Rows Removed by Filter"], n["Shared Hit Blocks"], n["Shared Read Blocks"])
				}
				if children, ok := n["Plans"].([]any); ok {
					for _, child := range children {
						if c, ok := child.(map[string]any); ok {
							visit(c)
						}
					}
				}
			}
			if p, ok := docs[0]["Plan"].(map[string]any); ok {
				visit(p)
			}
			snapshot, err := q.PublicationContextSnapshot(f.e.ctx, store.PublicationContextSnapshotParams{RunID: id, UserID: f.w.UserID})
			if err != nil {
				t.Fatal(err)
			}
			var s publicationSnapshot
			if err := json.Unmarshal(snapshot, &s); err != nil || len(s.Nodes) != 1 || s.Nodes[0].Run.ID != f.id || s.AssociationUnknown || s.AncestryUnknown {
				t.Fatalf("bounded shared-root snapshot: nodes=%d error=%v", len(s.Nodes), err)
			}
		})
	}
}
