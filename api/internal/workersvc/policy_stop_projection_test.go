package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func stopProjectionService() *Service {
	return &Service{p: Params{PolicyStopNegotiated: true, PolicyHoldNegotiated: true,
		PolicyHoldSnapshotEnabled: true, TerminalPendingLease: time.Minute}}
}

func TestPolicyStopProjectionEvidenceAndNegotiation(t *testing.T) {
	for _, mode := range []string{"current", "absent", "absent_no_reader", "missing_reader", "typed_nil_reader",
		"invalid", "invalid_off", "off", "hold_off", "snapshot_off", "lease_off", "old_worker", "missing_fence",
		"wrong_user", "wrong_worker", "stale_generation", "zero_generation", "released", "terminal", "scope_delivered", "storage_error", "storage_error_off"} {
		t.Run(mode, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "legacy", false)
			s := stopProjectionService()
			f.worker.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1}
			var reader any = f.reader
			generation := int64(4)
			sentinel := errors.New("scope unavailable")
			switch mode {
			case "absent", "absent_no_reader":
				f.run.PolicyStopResolution = nil
				s.p.PolicyStopNegotiated = false
				if mode == "absent_no_reader" {
					reader = struct{}{}
				}
			case "missing_reader":
				reader = struct{}{}
			case "typed_nil_reader":
				reader = (*stopExecutionReader)(nil)
			case "invalid", "invalid_off":
				f.run.PolicyStopResolution = []byte("{}")
				s.p.PolicyStopNegotiated = mode != "invalid_off"
			case "off":
				s.p.PolicyStopNegotiated = false
			case "hold_off":
				s.p.PolicyHoldNegotiated = false
			case "snapshot_off":
				s.p.PolicyHoldSnapshotEnabled = false
			case "lease_off":
				s.p.TerminalPendingLease = 0
			case "old_worker":
				f.worker.ProtocolCapabilities = []string{capability.CredentialSwitchV1}
			case "missing_fence":
				f.worker.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1}
			case "wrong_user":
				f.worker.UserID = uuid.New()
			case "wrong_worker":
				f.worker.ID = uuid.New()
			case "stale_generation":
				generation = 3
			case "zero_generation":
				generation = 0
			case "released":
				f.run.ClaimReleasedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
			case "terminal":
				f.run.Status = "completed"
			case "scope_delivered":
				f.reader.scope.ConsumedWorkerID = pgconv.UUID(f.worker.ID)
			case "storage_error", "storage_error_off":
				f.reader.fail, f.reader.err = "scope", sentinel
				if mode == "storage_error_off" {
					s.p.PolicyStopNegotiated = false
				}
			}
			before := mustStopJSON(t, f.run)
			got, err := s.policyStopProjectionLocked(context.Background(), reader, f.worker, f.run, generation, s.policyRevisionProjectionNegotiated(f.worker))
			absent := mode == "absent" || mode == "absent_no_reader"
			if mode == "current" {
				if err != nil || got == nil || got.ResolutionID != f.record.ResolutionID.String() || got.ClaimGeneration != "4" {
					t.Fatalf("current claim projection=%+v err=%v", got, err)
				}
				want := `{"resolution_id":"` + f.record.ResolutionID.String() + `","claim_generation":"4"}`
				if raw := mustStopJSON(t, got); string(raw) != want {
					t.Fatalf("wire=%s", raw)
				}
				if policyRevisionPlanApproved(context.Background(), f.reader, f.run, true) {
					t.Fatal("Stop approved the unapproved candidate")
				}
			} else if got != nil || (err == nil) != absent {
				t.Fatalf("projection=%+v err=%v", got, err)
			}
			if (mode == "storage_error" || mode == "storage_error_off") && !errors.Is(err, sentinel) {
				t.Fatalf("lost storage error: %v", err)
			}
			if mode == "off" && len(f.reader.reads) != 2 {
				t.Fatalf("switch-off skipped evidence: %v", f.reader.reads)
			}
			if !bytes.Equal(before, mustStopJSON(t, f.run)) {
				t.Fatal("projection changed persisted scope")
			}
		})
	}
}

// The fake transaction supplies accepted validator evidence and valid API-key
// credential authority. It models lock custody; SQL locking needs LiveDB coverage.
type stopProjectionClaimTx struct {
	*fakeClaimFinishTx
	*stopExecutionReader
}

func (q *stopProjectionClaimTx) LockCodexAliasForShareNowait(context.Context, store.LockCodexAliasForShareNowaitParams) (store.LockCodexAliasForShareNowaitRow, error) {
	return store.LockCodexAliasForShareNowaitRow{Status: "linked", MaterialRevision: 1}, nil
}

func (q *stopProjectionClaimTx) GetRunCodexAuthContext(context.Context, uuid.UUID) (store.GetRunCodexAuthContextRow, error) {
	return store.GetRunCodexAuthContextRow{CodexAuthMode: pgtype.Text{String: codexAuthModeAPIKey, Valid: true},
		BoundKind: store.KindOpenAIAPIKey, Status: "claimed",
		CodexMaterialRevision: pgtype.Int8{Int64: 1, Valid: true}, CurrentMaterialRevision: 1}, nil
}

type stopProjectionClaimStore struct {
	*fakeStore
	reader *stopExecutionReader
}

func (q *stopProjectionClaimStore) BeginClaimFinish(context.Context) (claimFinishTx, error) {
	return &stopProjectionClaimTx{fakeClaimFinishTx: &fakeClaimFinishTx{f: q.fakeStore}, stopExecutionReader: q.reader}, nil
}

func TestPolicyStopFinalClaimProjection(t *testing.T) {
	for _, mode := range []string{"accepted", "wrong_authenticated_user", "missing_authenticated_user", "off", "invalid", "commit_failure"} {
		t.Run(mode, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "legacy", false)
			f.run.CodexSecretID = pgconv.UUID(uuid.New())
			fs := &fakeStore{claimRun: f.run, claimFinishLocked: &f.run, claimFinishCtx: &store.GetRunClaimContextRow{}}
			s := New(&stopProjectionClaimStore{fakeStore: fs, reader: f.reader}, nil, testParams())
			s.p.PolicyStopNegotiated = true
			id := claimRecoveryIdentity{workerID: f.worker.ID, userID: f.worker.UserID, policyRevisionProjection: true}
			switch mode {
			case "wrong_authenticated_user":
				id.userID = uuid.New()
			case "missing_authenticated_user":
				id.userID = uuid.Nil
			case "off":
				s.p.PolicyStopNegotiated = false
			case "invalid":
				f.run.PolicyStopResolution = []byte("{}")
			case "commit_failure":
				fs.claimFinishCommitErr = errors.New("commit refused")
			}
			payload := &ClaimPayload{PolicyStopExecution: &PolicyStopExecution{ResolutionID: uuid.NewString(), ClaimGeneration: "1"},
				PlanApproved: true, PlanMd: strPtr("assembly candidate")}
			got, err := s.finishRunClaim(context.Background(), f.run, payload, nil, id)
			if mode != "accepted" {
				if err == nil || got != nil {
					t.Fatalf("handoff=%+v err=%v", got, err)
				}
				return
			}
			if err != nil || got == nil || got.PolicyStopExecution == nil {
				t.Fatalf("handoff=%+v err=%v", got, err)
			}
			if got.PolicyStopExecution.ResolutionID != f.record.ResolutionID.String() || got.PolicyStopExecution.ClaimGeneration != "4" {
				t.Fatalf("assembly authority survived: %+v", got.PolicyStopExecution)
			}
			if got.PlanApproved || got.ResumePhase != "awaiting_approval" || got.PlanMd == nil || *got.PlanMd != "current unapproved B" ||
				len(got.Milestones) != 1 || got.Milestones[0].ID != "b1" || got.ExecutionMilestones == nil {
				t.Fatalf("incoherent candidate projection: %+v", got)
			}
			frozen, err := DecodeMilestones(f.run.MilestonesFrozen)
			if err != nil || !bytes.Equal(mustStopJSON(t, *got.ExecutionMilestones), mustStopJSON(t, frozen)) {
				t.Fatalf("frozen execution replaced: %v", err)
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(mustStopJSON(t, got), &wire); err != nil {
				t.Fatal(err)
			}
			wantExecution := `{"resolution_id":"` + f.record.ResolutionID.String() + `","claim_generation":"4"}`
			if string(wire["policy_stop_execution"]) != wantExecution || string(wire["plan_approved"]) != "false" {
				t.Fatalf("claim wire authority/approval: %s %s", wire["policy_stop_execution"], wire["plan_approved"])
			}
			var gate map[string]json.RawMessage
			if json.Unmarshal(got.PolicyRevisionGate, &gate) != nil || string(gate["revision"]) != `"2"` {
				t.Fatalf("gate=%s", got.PolicyRevisionGate)
			}
		})
	}
}

func TestPolicyStopFinalClaimMissingReaderAndAbsentFallback(t *testing.T) {
	for _, present := range []bool{false, true} {
		fs, s, run, id := finishFixture(t)
		if present {
			run.PolicyStopResolution = []byte("{}")
		}
		run.AutoApprove = true
		fs.claimRun = run
		payload := &ClaimPayload{PolicyStopExecution: &PolicyStopExecution{ResolutionID: uuid.NewString(), ClaimGeneration: "3"}}
		got, err := s.finishRunClaim(context.Background(), run, payload, nil, id)
		if present {
			if err == nil || got != nil {
				t.Fatalf("missing reader handed off: %v", err)
			}
		} else if err != nil || got == nil || got.PolicyStopExecution != nil || !got.PlanApproved {
			t.Fatalf("absent fallback=%+v err=%v", got, err)
		}
		if !present {
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(mustStopJSON(t, got), &wire); err != nil {
				t.Fatal(err)
			}
			if _, exists := wire["policy_stop_execution"]; exists {
				t.Fatal("absent receipt emitted wire authority")
			}
		}
		if payload.PolicyStopExecution != nil {
			t.Fatal("assembly authority retained")
		}
	}
}
