package workersvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This override exercises cancellation by the service's real proof deadline.
type blockingPublicationForge struct {
	*publicationForge
	cancelled bool
}

func (f *blockingPublicationForge) CompareAncestry(ctx context.Context, projectID int64, head, candidate string) (forge.Ancestry, error) {
	if _, err := f.publicationForge.CompareAncestry(ctx, projectID, head, candidate); err != nil {
		return forge.AncestryUnknown, err
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		f.t.Fatal("forge comparison needs a bounded proof deadline")
	}
	<-ctx.Done()
	f.cancelled = errors.Is(ctx.Err(), context.DeadlineExceeded)
	return forge.AncestryUnknown, ctx.Err()
}

func TestCompletedPublicationLifecycleLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name, kind, capture, reason string
		ancestry                    forge.Ancestry
		block                       bool
		change                      func(*publicationForge)
	}{
		{name: "rework_equal", kind: "mr_rework", ancestry: forge.AncestryAncestor},
		{name: "rework_ancestor", kind: "mr_rework", ancestry: forge.AncestryAncestor, change: func(f *publicationForge) { f.head = strings.Repeat("b", 40); f.summaryHead = f.head }},
		{name: "rework_mr_missing", kind: "mr_rework", reason: "mr_missing", change: func(f *publicationForge) { f.summaryErr = forge.ErrMergeRequestNotFound }},
		{name: "rework_branch_missing", kind: "mr_rework", reason: "branch_missing", change: func(f *publicationForge) { f.headErr = forge.ErrRefNotFound }},
		{name: "rework_branch_mismatch", kind: "mr_rework", reason: "branch_mismatch", change: func(f *publicationForge) { f.branch = "agent/wrong" }},
		{name: "rework_head_mismatch", kind: "mr_rework", reason: "head_mismatch", change: func(f *publicationForge) { f.summaryHead = strings.Repeat("c", 40) }},
		{name: "rework_forge_error", kind: "mr_rework", reason: "ancestry_unknown", change: func(f *publicationForge) { f.compareErr = errors.New("forge unavailable") }},
		{name: "rework_blocking_deadline", kind: "mr_rework", ancestry: forge.AncestryAncestor, reason: "forge_timeout", block: true},
		{name: "rework_unrelated", kind: "mr_rework", ancestry: forge.AncestryNotAncestor, reason: "not_ancestor"},
		{name: "rework_unknown", kind: "mr_rework", ancestry: forge.AncestryUnknown, reason: "ancestry_unknown"},
		{name: "available_capture", capture: "available", ancestry: forge.AncestryAncestor},
		{name: "thin_capture", capture: "thin", ancestry: forge.AncestryAncestor},
		{name: "needs_action_capture", capture: "needs_action", ancestry: forge.AncestryAncestor},
		{name: "blocking_deadline", ancestry: forge.AncestryAncestor, reason: "forge_timeout", block: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			caps := []string{capability.RecoveryCompletedPublicationV1}
			wid := e.seedWorker(t, caps)
			run := e.seedLegacyRunningRun(t, wid)
			gen, mr := int64(1), int64(17)
			head, branch := strings.Repeat("a", 40), "agent/issue-1"
			e.exec(t, "UPDATE repos SET forge_project_id=23 WHERE id=$1", e.repoID)
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			if tc.kind == "mr_rework" {
				branch = "agent/rework-lifecycle"
				e.exec(t, "UPDATE runs SET kind='mr_rework',pipeline_ref=$2,mr_iid=$3,target_run_id=$1 WHERE id=$1", run, branch, mr)
			}
			hold := uuid.New()
			e.exec(t, `INSERT INTO recovery_custody_holds
(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
			siblingRun := e.seedLegacyRunningRun(t, wid)
			siblingHold := mhOpenHold(t, e, siblingRun, 1, wid)
			capture := uuid.New()
			var before, after string
			if tc.capture != "" {
				// Same stored capture fixture shape used by custody_multihold_livedb_test.
				e.exec(t, `INSERT INTO recovery_captures
(id,hold_id,run_id,user_id,original_worker_id,original_worker_identity,source_sha,idempotency_key,state,expires_at,ready_retention_seconds)
VALUES($1,$2,$3,$4,$5,'ident',$6,$7,$8,now()+interval '7 days',604800)`,
					capture, hold, run, e.userID, wid, head, uuid.NewString(), tc.capture)
				if err := e.pool.QueryRow(e.ctx, "SELECT row_to_json(c)::text FROM recovery_captures c WHERE id=$1", capture).Scan(&before); err != nil {
					t.Fatal(err)
				}
			}
			f := &publicationForge{t: t, projectID: 23, mrIID: mr, expectedBranch: branch, branch: branch, head: head, summaryHead: head, ancestry: tc.ancestry}
			if tc.change != nil {
				tc.change(f)
			}
			svc := e.permitService(t)
			blocking := &blockingPublicationForge{publicationForge: f}
			if tc.block {
				svc.SetForges(settleUnitBuilder{f: blocking})
			} else {
				svc.SetForges(settleUnitBuilder{f: f})
			}
			w := store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}
			req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head, Branch: &branch, MrIID: &mr}
			start := time.Now()
			result, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
			if err != nil || !result.Applied || result.Run.Status != "completed" || e.runStatus(t, run) != "completed" {
				t.Fatalf("completion: %+v err=%v", result, err)
			}
			wantCalls := 1
			if tc.reason == "mr_missing" || tc.reason == "branch_missing" || tc.reason == "branch_mismatch" || tc.reason == "head_mismatch" {
				wantCalls = 0
			}
			if result.CompletedPublicationReason != tc.reason || f.compareCalls != wantCalls {
				t.Fatalf("proof reason=%q want=%q comparisons=%d", result.CompletedPublicationReason, tc.reason, f.compareCalls)
			}
			var state string
			var reason, disposition, evidence *string
			var receipt []byte
			if err := e.pool.QueryRow(e.ctx, `SELECT state,completed_publication_reason,final_disposition,release_evidence,completed_publication_receipt
FROM recovery_custody_holds WHERE id=$1`, hold).Scan(&state, &reason, &disposition, &evidence, &receipt); err != nil {
				t.Fatal(err)
			}
			if tc.reason != "" {
				if state != "open" || reason == nil || *reason != tc.reason || receipt != nil || result.CompletedPublicationReceipt != nil || disposition != nil || evidence != nil {
					t.Fatalf("refusal custody: state=%s reason=%v disposition=%v evidence=%v receipt=%s", state, reason, disposition, evidence, receipt)
				}
			} else if state != "released" || result.CompletedPublicationReceipt == nil || disposition == nil || *disposition != "completed_publication" || evidence == nil || *evidence != "completed_publication" || len(receipt) == 0 {
				t.Fatalf("publication release: state=%s result=%+v", state, result)
			}
			if tc.block && (!blocking.cancelled || time.Since(start) > 15*time.Second) {
				t.Fatalf("actual cancellation=%t elapsed=%s", blocking.cancelled, time.Since(start))
			}
			if tc.capture != "" {
				if err := e.pool.QueryRow(e.ctx, "SELECT row_to_json(c)::text FROM recovery_captures c WHERE id=$1", capture).Scan(&after); err != nil {
					t.Fatal(err)
				}
				if before != after {
					t.Fatalf("capture changed across publication release: before=%s after=%s", before, after)
				}
			}
			if mhHoldState(t, e, siblingHold) != "open" {
				t.Fatal("sibling run custody released")
			}
		})
	}
}

func TestCompletedPublicationAuthenticationLiveDB(t *testing.T) {
	for _, phase := range []string{"before_release", "receipt_replay"} {
		t.Run(phase, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			caps := []string{capability.RecoveryCompletedPublicationV1}
			wid := e.seedWorker(t, caps)
			otherWorker := e.seedWorker(t, caps)
			otherOwner := setupInterlockLiveDB(t)
			foreignWorker := otherOwner.seedWorker(t, caps)
			run := e.seedLegacyRunningRun(t, wid)
			gen, mr := int64(1), int64(7)
			head, branch := strings.Repeat("a", 40), "agent/issue-1"
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			hold := uuid.New()
			e.exec(t, `INSERT INTO recovery_custody_holds
(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
			siblingRun := e.seedLegacyRunningRun(t, otherWorker)
			sibling := mhOpenHold(t, e, siblingRun, 1, otherWorker)
			svc := e.permitService(t)
			f := &publicationForge{t: t, projectID: 1, mrIID: mr, expectedBranch: branch, branch: branch, head: head, summaryHead: head, ancestry: forge.AncestryAncestor}
			svc.SetForges(settleUnitBuilder{f: f})
			req := StateRequest{State: "completed", ClaimGeneration: &gen, CompletionFinalHead: &head, Branch: &branch, MrIID: &mr}
			wantState := "open"
			if phase == "receipt_replay" {
				result, err := svc.SetStateReportWithReconciliation(e.ctx, store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}, run, req)
				if err != nil || result.CompletedPublicationReceipt == nil {
					t.Fatalf("owner release: %+v %v", result, err)
				}
				wantState = "released"
				svc.SetForges(nil)
			}
			for _, w := range []store.Worker{
				{ID: otherWorker, UserID: e.userID, ProtocolCapabilities: caps},
				{ID: foreignWorker, UserID: otherOwner.userID, ProtocolCapabilities: caps},
			} {
				result, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
				if err == nil || result.Applied || result.CompletedPublicationReceipt != nil {
					t.Fatalf("unauthorized worker obtained completion/receipt: %+v err=%v", result, err)
				}
			}
			wantCalls := 0
			if phase == "receipt_replay" {
				wantCalls = 1
			}
			if f.compareCalls != wantCalls || mhHoldState(t, e, hold) != wantState || mhHoldState(t, e, sibling) != "open" {
				t.Fatal("unauthorized report changed custody or reached forge")
			}
			wantStatus := "running"
			if phase == "receipt_replay" {
				wantStatus = "completed"
			}
			if e.runStatus(t, run) != wantStatus {
				t.Fatal("unauthorized report changed run")
			}
		})
	}
}

func TestCompletedPublicationNonCompletedCustodyLiveDB(t *testing.T) {
	for _, kind := range []string{"issue", "self_improve"} {
		t.Run(kind, func(t *testing.T) { completedPublicationNonCompletedCustody(t, kind) })
	}
}
func completedPublicationNonCompletedCustody(t *testing.T, kind string) {
	for _, status := range []string{"failed", "cancelled", "paused"} {
		t.Run(status, func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			caps := []string{capability.RecoveryCompletedPublicationV1}
			wid := e.seedWorker(t, caps)
			run := e.seedLegacyRunningRun(t, wid)
			e.exec(t, "UPDATE runs SET kind=$2 WHERE id=$1", run, kind)
			gen := int64(1)
			e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", run)
			if status == "paused" {
				e.exec(t, "UPDATE runs SET pause_requested_at=now(),pause_mode='now' WHERE id=$1", run)
			}
			if status == "cancelled" {
				e.exec(t, "UPDATE runs SET stop_kind='cancelled' WHERE id=$1", run)
			}
			hold := uuid.New()
			e.exec(t, `INSERT INTO recovery_custody_holds
(id,user_id,repo_id,run_id,generation,state,original_worker_id,original_worker_identity,live_worker_id,live_run_id,inventory_guarded)
VALUES($1,$2,$3,$4,1,'open',$5,'ident',$5,$4,true)`, hold, e.userID, e.repoID, run, wid)
			svc := e.permitService(t)
			// Loud default forge methods catch any accidental completed-publication proof.
			svc.SetForges(settleUnitBuilder{f: &publicationForge{t: t}})
			report := status
			if status == "cancelled" {
				report = "failed"
			}
			result, err := svc.SetStateReportWithReconciliation(e.ctx, store.Worker{ID: wid, UserID: e.userID, ProtocolCapabilities: caps}, run, StateRequest{State: report, ClaimGeneration: &gen})
			if err != nil || !result.Applied || result.Run.Status != status || result.CompletedPublicationReceipt != nil || result.CompletedPublicationReason != "" {
				t.Fatalf("non-completed report: %+v %v", result, err)
			}
			if _, err := svc.ReconcileCustodyReleases(e.ctx); err != nil {
				t.Fatal(err)
			}
			var untouched bool
			if err := e.pool.QueryRow(e.ctx, `SELECT state='open' AND completion_identity IS NULL AND completed_publication_receipt IS NULL
AND completed_publication_reason IS NULL AND final_disposition IS NULL AND release_evidence IS NULL
FROM recovery_custody_holds WHERE id=$1`, hold).Scan(&untouched); err != nil || !untouched {
				t.Fatalf("non-completed custody changed: untouched=%t err=%v", untouched, err)
			}
		})
	}
}
