package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func enableRevisionProjection(s *Service, w *store.Worker) {
	s.p.PolicyHoldNegotiated, s.p.PolicyHoldSnapshotEnabled = true, true
	s.p.TerminalPendingLease = time.Minute
	w.ProtocolCapabilities = append(w.ProtocolCapabilities, capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1)
}

// The human-only case uses the creation producer's birth source. The checker
// fixture explicitly seeds coverage; it proves projection, not source production.
func TestPolicyRevisionProjectionStateLiveDB(t *testing.T) {
	for _, checker := range []bool{false, true} {
		t.Run(map[bool]string{false: "human_only", true: "checker"}[checker], func(t *testing.T) {
			var e codexTestEnv
			var svc *Service
			var w store.Worker
			var run uuid.UUID
			var req StateRequest
			if checker {
				f := newReconciliationFixture(t, "codex", "")
				e, svc, w, run, req = f.codexTestEnv, f.svc, f.worker, f.lead, f.req
				e.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", run,
					semanticJSON(t, apitypes.PolicyRevisionSourceV1{Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{}}))
			} else {
				var f *gateFix
				e, f, run = revisionGateFixture(t)
				svc, w, req = f.svc, f.wkr, gateReq(i64(1), uid())
			}
			enableRevisionProjection(svc, &w)
			result, err := svc.SetStateReportWithReconciliation(e.ctx, w, run, req)
			if err != nil || !result.Applied || len(result.PolicyRevisionGate) == 0 {
				t.Fatalf("applied=%v gate=%s err=%v", result.Applied, result.PolicyRevisionGate, err)
			}
			if (result.Reconciliation != nil) != checker {
				t.Fatalf("unexpected reconciliation=%v", result.Reconciliation != nil)
			}
			source := inventorySource(t, e, run)
			var gate apitypes.PolicyStopGateContext
			if source.Gate == nil || json.Unmarshal(result.PolicyRevisionGate, &gate) != nil || gate != *source.Gate {
				t.Fatalf("ACK did not capture published gate: %s", result.PolicyRevisionGate)
			}
		})
	}
}

func TestPolicyRevisionProjectionStateNoGateLiveDB(t *testing.T) {
	e, f, run := revisionGateFixture(t)
	enableRevisionProjection(f.svc, &f.wkr)
	result, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.wkr, run, StateRequest{State: "running", ClaimGeneration: i64(1)})
	if err != nil || !result.Applied || string(result.PolicyRevisionGate) != "null" || result.Reconciliation != nil {
		t.Fatalf("no-gate ACK applied=%v gate=%s error=%v", result.Applied, result.PolicyRevisionGate, err)
	}
}

func TestPolicyRevisionProjectionStateOmissionLiveDB(t *testing.T) {
	for _, mode := range []string{"commit", "noop", "early_replay", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			e, f, run := revisionGateFixture(t)
			enableRevisionProjection(f.svc, &f.wkr)
			req := gateReq(i64(1), uid())
			switch mode {
			case "commit":
				f.svc.SetTxBeginner(&revisionGateBeginner{TxBeginner: e.pool, point: "commit"})
			case "noop":
				f.svc.SetTxBeginner(&revisionGateBeginner{TxBeginner: e.pool, point: "update_zero"})
			case "unknown":
				e.exec("UPDATE runs SET policy_revision_source=NULL WHERE id=$1", run)
			case "early_replay":
				f.mustPublish(run, req, 1)
				active := seedRevisionTarget(t, f, run, 1)
				req = gateReq(i64(1), uid())
				req.ExpectedGateRevision, req.RevisionInputID = i64(1), &active
				f.mustPublish(run, req, 2)
			}
			result, err := f.svc.SetStateReportWithReconciliation(e.ctx, f.wkr, run, req)
			if (err != nil) != (mode == "commit") || len(result.PolicyRevisionGate) != 0 {
				t.Fatalf("mode=%s gate=%s applied=%v err=%v", mode, result.PolicyRevisionGate, result.Applied, err)
			}
			if mode == "commit" && (result.Applied || mustRun(t, e, run).GateRevision != 0) {
				t.Fatal("failed commit leaked state")
			}
			if mode == "early_replay" && !result.Applied {
				t.Fatal("fixture did not reach receipt replay")
			}
		})
	}
}

// Seeded coverage isolates final handoff from source creation. The assembly
// hook changes committed coverage after assembly; only the final lock may read it.
func TestPolicyRevisionProjectionClaimLiveDB(t *testing.T) {
	e := setupCodexLiveDB(t)
	for _, mode := range []string{"no_gate", "sql_null", "corrupt", "commit"} {
		t.Run(mode, func(t *testing.T) {
			f := newCodexClaimFix(t, e, false)
			run := f.runID
			w := wkrRow(t, e, f.workerA)
			w.ProtocolCapabilities = []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.RecoveryArchiveV1}
			enableRevisionProjection(f.svc, &w)
			e.exec("UPDATE workers SET protocol_capabilities=$2 WHERE id=$1", w.ID, w.ProtocolCapabilities)
			t.Cleanup(func() { dropCustodyHolds(t, e, f.userID) })
			hooked := false
			f.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, locked store.Run, payload *ClaimPayload, err error) {
				if err != nil || payload == nil || locked.ID != run {
					t.Fatalf("assembly: %v", err)
				}
				hooked = true
				payload.PolicyRevisionGate = json.RawMessage("null")
				var raw []byte
				switch mode {
				case "no_gate", "commit":
					raw = semanticJSON(t, apitypes.PolicyRevisionSourceV1{Version: 1, AssociatedCrossChecks: []apitypes.PolicyRevisionRef{}})
				case "corrupt":
					raw = []byte(`{"version":2}`)
				}
				e.exec("UPDATE runs SET policy_revision_source=$2 WHERE id=$1", run, raw)
				if mode == "commit" {
					f.svc.SetTxBeginner(&revisionGateBeginner{TxBeginner: e.pool, point: "commit"})
				}
			}}
			payload, err := f.svc.Claim(e.ctx, w, nil)
			if !hooked {
				t.Fatalf("assembly hook not reached: %v", err)
			}
			if mode == "commit" {
				if err == nil || payload != nil {
					t.Fatalf("failed commit delivered payload: %v", err)
				}
				return
			}
			if err != nil || payload == nil {
				t.Fatalf("claim: %v", err)
			}
			want := []byte(nil)
			if mode == "no_gate" {
				want = []byte("null")
			}
			if !bytes.Equal(payload.PolicyRevisionGate, want) {
				t.Fatalf("claim gate=%s want=%s", payload.PolicyRevisionGate, want)
			}
		})
	}
}

// A parent owns its commit; the inner call must never expose precommit evidence.
func TestPolicyRevisionProjectionParentTransactionLiveDB(t *testing.T) {
	e, f, run := revisionGateFixture(t)
	enableRevisionProjection(f.svc, &f.wkr)
	locked, _, tx, err := f.svc.beginClaimMutation(e.ctx, f.wkr, run, i64(1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	var projection json.RawMessage
	_, _, err = locked.setStateWithTx(e.ctx, f.wkr, run, StateRequest{State: "pause_failed", ClaimGeneration: i64(1)}, nil, nil, &projection, tx)
	if err != nil {
		t.Fatal(err)
	}
	if projection != nil {
		t.Fatalf("parent transaction leaked %s", projection)
	}
}
