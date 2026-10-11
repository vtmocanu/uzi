package workersvc

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCompletedPublicationCompletionIdentityLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "self_improve"} {
		t.Run(kind, func(t *testing.T) { completedPublicationCompletionIdentity(t, kind) })
	}
}

func completedPublicationCompletionIdentity(t *testing.T, kind string) {
	for _, permit := range []bool{false, true} {
		name := "direct"
		if permit {
			name = "permit"
		}
		t.Run(name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			caps := []string{capability.RecoveryCompletedPublicationV1, "completion_interlock_v1"}
			wid := e.seedWorker(t, caps)
			run := e.seedLegacyRunningRun(t, wid)
			if permit {
				run = e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
			}
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			var iid int64
			if err := e.pool.QueryRow(e.ctx, "SELECT issue_iid FROM runs WHERE id=$1", run).Scan(&iid); err != nil {
				t.Fatal(err)
			}
			branch := agentIssueBranch(iid)
			if kind == "self_improve" {
				branch = selfImproveBranch(run)
				e.exec(t, "UPDATE runs SET kind='self_improve' WHERE id=$1", run)
			}
			head, changed := strings.Repeat("a", 40), strings.Repeat("c", 40)
			gen, mr := int64(1), int64(7)
			hold := uuid.New()
			e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
			svc := e.permitService(t)
			w := store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}
			req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head, Branch: &branch, MrIID: &mr}
			if permit {
				req.Head = &head
				p, err := svc.RequestCompletionPermit(e.ctx, w, run, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head})
				if err != nil || !p.Granted {
					t.Fatalf("grant permit: %+v %v", p, err)
				}
				bad := req
				bad.CompletionFinalHead = &changed
				if _, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, bad); !errors.Is(err, ErrInvalidState) {
					t.Fatalf("permit final head mismatch: %v", err)
				}
				var consumed bool
				if err := e.pool.QueryRow(e.ctx, "SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id=$1", run).Scan(&consumed); err != nil {
					t.Fatal(err)
				}
				if consumed || e.runStatus(t, run) != "running" {
					t.Fatal("mismatch consumed permit or completed run")
				}
			}
			// Initial proof failure commits completion and freezes identity for a retry.
			f := &publicationForge{t: t, projectID: 1, mrIID: 7, expectedBranch: branch, branch: branch, head: head, summaryHead: head, ancestry: forge.AncestryUnknown}
			svc.SetForges(settleUnitBuilder{f: f})
			first, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
			if err != nil || !first.Applied || first.CompletedPublicationReceipt != nil || first.CompletedPublicationReason != "ancestry_unknown" {
				t.Fatalf("initial completion: %+v %v", first, err)
			}
			if permit {
				var consumed bool
				if err := e.pool.QueryRow(e.ctx, "SELECT consumed_at IS NOT NULL FROM run_completion_permits WHERE run_id=$1 AND head=$2", run, head).Scan(&consumed); err != nil {
					t.Fatal(err)
				}
				if !consumed {
					t.Fatal("matching final head did not consume permit")
				}
			}
			read := func() store.RecoveryCustodyHold {
				t.Helper()
				h, err := e.q.GetCompletedPublicationHold(e.ctx, store.GetCompletedPublicationHoldParams{RunID: run, UserID: e.userID, WorkerID: wid, Generation: gen})
				if err != nil {
					t.Fatal(err)
				}
				return h
			}
			frozen := read()
			bad := req
			bad.CompletionFinalHead = &changed
			wrong, _ := svc.SetStateReportWithReconciliation(e.ctx, w, run, bad)
			after := read()
			if wrong.CompletedPublicationReceipt != nil || after.State != "open" || !bytes.Equal(frozen.CompletionIdentity, after.CompletionIdentity) || f.compareCalls != 1 {
				t.Fatal("changed duplicate altered frozen identity or invoked proof")
			}
			f.ancestry = forge.AncestryAncestor
			exact, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
			if err != nil || !exact.Applied || exact.CompletedPublicationReceipt == nil || f.compareCalls != 2 {
				t.Fatalf("exact duplicate proof: %+v %v", exact, err)
			}
			released := read()
			if !bytes.Equal(frozen.CompletionIdentity, released.CompletionIdentity) {
				t.Fatal("release changed frozen identity")
			}
			svc.SetForges(nil)
			replay, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
			if err != nil || replay.CompletedPublicationReceipt == nil {
				t.Fatalf("unavailable forge replay: %+v %v", replay, err)
			}
			wrong, _ = svc.SetStateReportWithReconciliation(e.ctx, w, run, bad)
			final := read()
			if wrong.CompletedPublicationReceipt != nil || final.State != "released" || !bytes.Equal(released.CompletedPublicationReceipt, final.CompletedPublicationReceipt) {
				t.Fatal("changed duplicate reused or changed receipt")
			}
		})
	}
}

func TestCompletedPublicationLegacyOmissionsLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		older      bool
		caps       []string
		omitHead   bool
	}{
		{name: "self_improve_without_capability", kind: "self_improve"},
		{name: "self_improve_without_head", kind: "self_improve", caps: []string{capability.RecoveryCompletedPublicationV1}, omitHead: true},
		{name: "older_completed_self_improve", kind: "self_improve", caps: []string{capability.RecoveryCompletedPublicationV1}, older: true},
		{name: "excluded_ci_fix", kind: "ci_fix", caps: []string{capability.RecoveryCompletedPublicationV1}},
		{name: "excluded_prompt", kind: "prompt", caps: []string{capability.RecoveryCompletedPublicationV1}},
		{name: "excluded_task", kind: "task", caps: []string{capability.RecoveryCompletedPublicationV1}},
		{name: "legacy_without_capability"},
		{name: "capable_without_final_head", caps: []string{capability.RecoveryCompletedPublicationV1}, omitHead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			wid := e.seedWorker(t, tc.caps)
			run := e.seedLegacyRunningRun(t, wid)
			gen := int64(1)
			head := strings.Repeat("a", 40)
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			hold := uuid.New()
			e.exec(t, `INSERT INTO recovery_custody_holds(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
 VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
			switch tc.kind {
			case "ci_fix":
				e.exec(t, "UPDATE runs SET kind='ci_fix',pipeline_id=123,pipeline_ref='agent/ci' WHERE id=$1", run)
			case "prompt", "task":
				e.exec(t, "UPDATE runs SET kind=$2,issue_iid=NULL,branch='agent/task' WHERE id=$1", run, tc.kind)
			case "self_improve":
				e.exec(t, "UPDATE runs SET kind='self_improve' WHERE id=$1", run)
			}
			if tc.older {
				e.exec(t, "UPDATE runs SET status='completed' WHERE id=$1", run)
			}
			req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head}
			if tc.kind == "task" {
				branch := "agent/task"
				req.Branch = &branch
			}
			if tc.omitHead {
				req.CompletionFinalHead = nil
			}
			svc := e.permitService(t)
			result, err := svc.SetStateReportWithReconciliation(e.ctx, store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: tc.caps}, run, req)
			if err != nil || result.Applied == tc.older || result.Run.Status != "completed" || result.CompletedPublicationReceipt != nil {
				t.Fatalf("legacy completion: %+v %v", result, err)
			}
			if tc.older {
				req.CompletionFinalHead = nil
				replay, err := svc.SetStateReportWithReconciliation(e.ctx, store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: tc.caps}, run, req)
				if err != nil || replay.CompletedPublicationReceipt != nil {
					t.Fatalf("older completed replay: %+v %v", replay, err)
				}
				var missingHead bool
				if err := e.pool.QueryRow(e.ctx, "SELECT completion_final_head IS NULL FROM runs WHERE id=$1", run).Scan(&missingHead); err != nil || !missingHead {
					t.Fatalf("older replay minted final head: missing=%t err=%v", missingHead, err)
				}
			}
			var state string
			var identity []byte
			if err := e.pool.QueryRow(e.ctx, "SELECT state,completion_identity FROM recovery_custody_holds WHERE id=$1", hold).Scan(&state, &identity); err != nil {
				t.Fatal(err)
			}
			if state != "open" || len(identity) != 0 {
				t.Fatalf("omission minted identity or released hold: %s %s", state, identity)
			}
		})
	}
}
