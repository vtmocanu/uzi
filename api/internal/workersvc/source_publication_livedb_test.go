package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type sourcePublicationFixture struct {
	e   interlockLiveDB
	svc *Service
	w   store.Worker
	id  uuid.UUID
	req StateRequest
}

func seedSourcePublication(t *testing.T, permit bool) sourcePublicationFixture {
	t.Helper()
	e := setupInterlockLiveDB(t)
	wid := e.seedWorker(t, nil) // Deliberately no inventory/recovery capability.
	id := e.seedLegacyRunningRun(t, wid)
	if permit {
		id = e.seedFrozenRun(t, wid, []string{"m1"}, []string{"m1"}, false)
	}
	e.exec(t, "UPDATE runs SET claim_generation=1 WHERE id=$1", id)
	gen, mr := int64(1), int64(7)
	var iid int64
	if err := e.pool.QueryRow(e.ctx, "SELECT issue_iid FROM runs WHERE id=$1", id).Scan(&iid); err != nil {
		t.Fatal(err)
	}
	branch, head := agentIssueBranch(iid), strings.Repeat("a", 40)
	req := StateRequest{State: "completed", ClaimGeneration: &gen, Branch: &branch, MrIID: &mr,
		SourcePublication: &SourcePublicationReport{Version: 1, PublishedHead: head}}
	svc := e.permitService(t)
	w := store.Worker{ID: wid, UserID: e.userID}
	if permit {
		req.Head = &head
		p, err := svc.RequestCompletionPermit(e.ctx, w, id, CompletionPermitRequest{ContractRevision: 1, Branch: branch, Head: head})
		if err != nil || !p.Granted {
			t.Fatalf("permit: %+v %v", p, err)
		}
	}
	return sourcePublicationFixture{e, svc, w, id, req}
}

func (f sourcePublicationFixture) stored(t *testing.T) (string, []byte, bool) {
	t.Helper()
	var status string
	var stamp []byte
	var consumed bool
	err := f.e.pool.QueryRow(f.e.ctx, `SELECT status,source_publication_intent,
 EXISTS(SELECT 1 FROM run_completion_permits p WHERE p.run_id=r.id AND p.consumed_at IS NOT NULL)
 FROM runs r WHERE id=$1`, f.id).Scan(&status, &stamp, &consumed)
	if err != nil {
		t.Fatal(err)
	}
	return status, stamp, consumed
}

func TestSourcePublicationCompletionLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprint("permit=", permit), func(t *testing.T) {
			f := seedSourcePublication(t, permit)
			result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			if err != nil || !result.Applied {
				t.Fatalf("completion: %+v %v", result, err)
			}
			status, b, consumed := f.stored(t)
			var stamp SourcePublicationIntent
			if json.Unmarshal(b, &stamp) != nil || status != "completed" || consumed != permit ||
				stamp.Intent != "closing" || stamp.ReportedNonClosing || stamp.ClaimGeneration != 1 ||
				stamp.Branch != *f.req.Branch || stamp.MRIID != 7 || stamp.PublishedHead != strings.Repeat("a", 40) {
				t.Fatalf("atomic pair: %s %s consumed=%t", status, b, consumed)
			}
			replay, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
			if err != nil || !replay.Applied {
				t.Fatalf("lost ACK replay: %+v %v", replay, err)
			}
			for _, change := range []string{"head", "bool", "branch", "mr", "generation"} {
				bad := f.req
				report := *bad.SourcePublication
				bad.SourcePublication = &report
				switch change {
				case "head":
					report.PublishedHead = strings.Repeat("b", 40)
				case "bool":
					report.NonClosing = true
				case "branch":
					bad.Branch = strPtr("other")
				case "mr":
					v := int64(8)
					bad.MrIID = &v
				case "generation":
					v := int64(2)
					bad.ClaimGeneration = &v
				}
				rejected, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, bad)
				if err == nil || rejected.Applied {
					t.Fatalf("%s conflict accepted: %+v %v", change, rejected, err)
				}
			}
			// Later reconciliation/reclaim must retain historical publication evidence.
			f.e.exec(t, "UPDATE runs SET claim_generation=2,claim_released_at=now() WHERE id=$1", f.id)
			_, after, _ := f.stored(t)
			if !bytes.Equal(b, after) {
				t.Fatal("reclaim changed historical stamp")
			}
			if _, err := f.e.pool.Exec(f.e.ctx, "UPDATE runs SET source_publication_intent=NULL WHERE id=$1", f.id); err == nil {
				t.Fatal("stamp was mutable")
			}
		})
	}
}

func TestSourcePublicationRefusalsLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		for _, state := range []string{"zero", "missing_report_generation", "zero_report_generation", "negative_report_generation", "stale", "released", "terminal_absent", "failed", "malformed_contract", "missing_tx"} {
			t.Run(fmt.Sprintf("permit=%t/%s", permit, state), func(t *testing.T) {
				f := seedSourcePublication(t, permit)
				switch state {
				case "missing_report_generation":
					f.req.ClaimGeneration = nil
				case "zero_report_generation", "negative_report_generation":
					generation := int64(0)
					if state == "negative_report_generation" {
						generation = -1
					}
					f.req.ClaimGeneration = &generation
				case "zero":
					f.e.exec(t, "UPDATE runs SET claim_generation=0 WHERE id=$1", f.id)
				case "stale":
					f.e.exec(t, "UPDATE runs SET claim_generation=2 WHERE id=$1", f.id)
				case "released":
					f.e.exec(t, "UPDATE runs SET claim_released_at=now() WHERE id=$1", f.id)
				case "terminal_absent":
					f.e.exec(t, "UPDATE runs SET status='completed' WHERE id=$1", f.id)
				case "failed":
					f.e.exec(t, "UPDATE runs SET status='failed' WHERE id=$1", f.id)
				case "malformed_contract":
					f.e.exec(t, "UPDATE runs SET completion_contract_version=1,contract_revision=1,completion_contract='{}' WHERE id=$1", f.id)
				case "missing_tx":
					f.svc.SetTxBeginner(nil)
				}
				before, _, spent := f.stored(t)
				result, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
				after, b, consumed := f.stored(t)
				if (err == nil && result.Applied) || len(b) != 0 || before != after || spent != consumed {
					t.Fatalf("refusal mutated pair: %+v err=%v status=%s->%s stamp=%s consumed=%t", result, err, before, after, b, consumed)
				}
			})
		}
	}
}

type sourceCommitBeginner struct {
	TxBeginner
	lost bool
}
type sourceCommitTx struct {
	pgx.Tx
	lost bool
}

func (b sourceCommitBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.TxBeginner.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return sourceCommitTx{tx, b.lost}, nil
}
func (tx sourceCommitTx) Commit(ctx context.Context) error {
	if tx.lost {
		if err := tx.Tx.Commit(ctx); err != nil {
			return err
		}
	} else if err := tx.Rollback(ctx); err != nil {
		return err
	}
	return errors.New("injected commit acknowledgement failure")
}

func TestSourcePublicationCommitBoundaryLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		for _, lost := range []bool{false, true} {
			t.Run(fmt.Sprintf("permit=%t/lost=%t", permit, lost), func(t *testing.T) {
				f := seedSourcePublication(t, permit)
				f.svc.SetTxBeginner(sourceCommitBeginner{f.e.pool, lost})
				if _, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req); err == nil {
					t.Fatal("commit error hidden")
				}
				status, b, spent := f.stored(t)
				if lost {
					if status != "completed" || len(b) == 0 || spent != permit {
						t.Fatalf("lost ACK pair: %s %s %t", status, b, spent)
					}
				} else if status != "running" || len(b) != 0 || spent {
					t.Fatalf("aborted pair: %s %s %t", status, b, spent)
				}
				f.svc.SetTxBeginner(f.e.pool)
				replay, err := f.svc.SetStateReportWithReconciliation(f.e.ctx, f.w, f.id, f.req)
				if err != nil || !replay.Applied {
					t.Fatalf("recovery: %+v %v", replay, err)
				}
			})
		}
	}
}

func TestSourcePublicationRecoveryReceiptLiveDB(t *testing.T) {
	for _, permit := range []bool{false, true} {
		t.Run(fmt.Sprint("permit=", permit), func(t *testing.T) {
			e := setupInterlockLiveDB(t)
			f := seedSerializedPublication(t, e, permit)
			f.req.SourcePublication = &SourcePublicationReport{Version: 1, PublishedHead: *f.req.CompletionFinalHead}
			f.proof.ancestry = forge.AncestryUnknown
			first, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.worker, f.run, f.req)
			if err != nil || !first.Applied || first.CompletedPublicationReason != "ancestry_unknown" {
				t.Fatalf("first: %+v %v", first, err)
			}
			f.proof.ancestry = forge.AncestryAncestor
			retry, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.worker, f.run, f.req)
			if err != nil || retry.CompletedPublicationReceipt == nil {
				t.Fatalf("verification replay bypassed: %+v %v", retry, err)
			}
			f.svc.SetForges(nil)
			replay, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.worker, f.run, f.req)
			if err != nil || replay.CompletedPublicationReceipt == nil {
				t.Fatalf("receipt replay bypassed: %+v %v", replay, err)
			}
			bad := f.req
			report := *bad.SourcePublication
			report.NonClosing = true
			bad.SourcePublication = &report
			conflict, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.worker, f.run, bad)
			if err == nil || conflict.CompletedPublicationReceipt != nil {
				t.Fatalf("conflict got receipt: %+v %v", conflict, err)
			}
		})
	}
}
