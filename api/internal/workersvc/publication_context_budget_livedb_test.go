package workersvc

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPublicationContextAssociationBudgetLiveDB(t *testing.T) {
	for _, scenario := range []string{"boundary", "overflow", "non_source_mr_conflict", "request_mr_conflict", "lookup_overflow"} {
		t.Run(scenario, func(t *testing.T) {
			f := seedSourcePublication(t, false)
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			if err != nil || !result.Applied {
				t.Fatalf("source: %+v %v", result, err)
			}
			branch := *f.req.Branch
			descendants := 4095
			if scenario == "overflow" {
				descendants++
			}
			if scenario == "non_source_mr_conflict" || scenario == "request_mr_conflict" {
				descendants--
			}
			// Each descendant occurs in BOTH discovery arms. Budget unique records,
			// not aliases or ancestry paths; the source plus 4095 descendants must fit.
			f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,branch,pipeline_ref,mr_iid,target_run_id,issue_title,issue_description)
 SELECT gen_random_uuid(),$1,$2,'mr_rework','completed',$3,$3,7,$4,'descendant','' FROM generate_series(1,$5) g`,
				f.w.UserID, f.e.repoID, branch, f.id, descendants)
			history := func(mr int64) {
				t.Helper()
				// A prior CI publication is association evidence, not source intent authority.
				f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,branch,pipeline_ref,mr_iid,pipeline_id,issue_title,issue_description)
 VALUES($1,$2,$3,'ci_fix','completed',$4,$4,$5,123,'published CI','')`, uuid.New(), f.w.UserID, f.e.repoID, branch, mr)
			}
			if scenario == "non_source_mr_conflict" {
				history(8)
			}
			id := uuid.New()
			f.e.exec(t, `INSERT INTO runs(id,user_id,repo_id,kind,status,worker_id,claim_generation,pipeline_ref,mr_iid,pipeline_id,issue_title,issue_description)
 VALUES($1,$2,$3,'ci_fix','running',$4,1,$5,7,123,'refresh','')`, id, f.w.UserID, f.e.repoID, f.w.ID, branch)
			if scenario == "request_mr_conflict" {
				f.e.exec(t, "UPDATE runs SET branch=$2,mr_iid=8 WHERE id=$1", id, branch)
			}
			snapshot, err := f.e.q.PublicationContextSnapshot(f.e.ctx, store.PublicationContextSnapshotParams{RunID: id, UserID: f.w.UserID})
			if err != nil {
				t.Fatal(err)
			}
			var raw publicationSnapshot
			if err := json.Unmarshal(snapshot, &raw); err != nil {
				t.Fatal(err)
			}
			if scenario == "overflow" {
				if !raw.AssociationUnknown || len(raw.Nodes) != 0 {
					t.Fatalf("truncated inventory produced authority: unknown=%t nodes=%d", raw.AssociationUnknown, len(raw.Nodes))
				}
			} else if raw.AssociationUnknown || len(raw.Nodes) != 1 || raw.Nodes[0].Run.ID != f.id {
				t.Fatalf("unique association boundary refused: unknown=%t nodes=%d", raw.AssociationUnknown, len(raw.Nodes))
			}
			if scenario == "request_mr_conflict" && !raw.CIMRConflict {
				t.Fatal("requesting branch association was omitted from conflict validation")
			}
			proof := &contextPublicationForge{summary: forge.MergeRequestSummary{IID: 7, State: forge.MRStateOpened, SourceBranch: branch, HeadSHA: strings.Repeat("b", 40)}}
			if scenario == "lookup_overflow" {
				proof.barrier = func() { history(7) }
			}
			f.svc.SetForges(settleUnitBuilder{f: proof})
			got := f.svc.ResolvePublicationContext(f.e.ctx, f.w, id, PublicationContextRequest{1, branch, strings.Repeat("c", 40)})
			if scenario == "boundary" {
				if got.Status != "resolved" || got.Binding == nil || got.Binding.Intent != "closing" || got.Binding.SourceRunID != f.id {
					t.Fatalf("same root at exact work limit: %+v", got)
				}
			} else if got.Status != "unknown" || got.Binding != nil {
				t.Fatalf("overflow or conflicting association authorized: %+v", got)
			}
			if scenario == "lookup_overflow" && got.Reason != "authority_changed" {
				t.Fatalf("final statement missed overflow drift: %+v", got)
			}
		})
	}
}
