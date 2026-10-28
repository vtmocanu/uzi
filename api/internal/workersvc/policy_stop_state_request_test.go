package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestPolicyStopStateRequestMalformedBeforeStore(t *testing.T) {
	const canonical = "12345678-1234-4abc-8def-123456789abc"
	for _, value := range []string{"", " ", uuid.Nil.String(), "not-a-uuid", "{" + canonical + "}",
		"urn:uuid:" + canonical, strings.ReplaceAll(canonical, "-", ""), strings.ToUpper(canonical),
		" " + canonical, canonical + "\n"} {
		t.Run(value, func(t *testing.T) {
			// A nil receiver has no store or transaction beginner: reaching either
			// would panic rather than satisfy the required ErrInvalidState result.
			var s *Service
			req := StateRequest{State: "running", PolicyStopResolutionID: &value}
			_, applied, err := s.SetState(context.Background(), store.Worker{}, uuid.Nil, req)
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("SetState applied=%v err=%v", applied, err)
			}
			_, applied, _, err = s.SetStateReport(context.Background(), store.Worker{}, uuid.Nil, req)
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("SetStateReport applied=%v err=%v", applied, err)
			}
			result, err := s.SetStateReportWithReconciliation(context.Background(), store.Worker{}, uuid.Nil, req)
			if result.Applied || result.PolicyStopExecution != nil || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("reconciled report=%+v err=%v", result, err)
			}
			_, applied, err = s.setStateWithTx(context.Background(), store.Worker{}, uuid.Nil, req, nil, nil, nil, nil)
			if applied || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("shared entry applied=%v err=%v", applied, err)
			}
		})
	}
}

// Both replay families can return before setStateWithTx; malformed scalar
// rejection must precede even their first receiver/store access.
func TestPolicyStopStateRequestMalformedBeforeReplay(t *testing.T) {
	for _, mode := range []string{"source", "completed"} {
		t.Run(mode, func(t *testing.T) {
			var s *Service
			generation, mr := int64(4), int64(1)
			head := strings.Repeat("a", 40)
			req := StateRequest{State: "completed", ClaimGeneration: &generation,
				PolicyStopResolutionID: strPtr("bad"), CompletionFinalHead: &head}
			worker := store.Worker{ProtocolCapabilities: []string{capability.RecoveryCompletedPublicationV1}}
			if mode == "source" {
				req.Branch, req.MrIID = strPtr("agent/test"), &mr
				req.SourcePublication = &SourcePublicationReport{Version: 1, PublishedHead: head}
			}
			result, err := s.SetStateReportWithReconciliation(context.Background(), worker, uuid.New(), req)
			if result.Applied || result.PolicyStopExecution != nil || !errors.Is(err, ErrInvalidState) {
				t.Fatalf("replay report=%+v err=%v", result, err)
			}
		})
	}
}

type policyStopStateOwnedStore struct {
	Store
	run   store.Run
	err   error
	reads int
}

func (q *policyStopStateOwnedStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	q.reads++
	return q.run, q.err
}

func TestPolicyStopStateRequestAbsentReceiptRefused(t *testing.T) {
	for _, state := range []string{"running", "awaiting_approval", "awaiting_input"} {
		t.Run(state, func(t *testing.T) {
			f := stopExecutionFixtureFor(t, "legacy", false)
			f.run.PolicyStopResolution = nil
			q := &policyStopStateOwnedStore{run: f.run}
			s := &Service{q: q}
			before := mustStopJSON(t, q.run)
			req := StateRequest{State: state, ClaimGeneration: &f.run.ClaimGeneration,
				PolicyStopResolutionID: strPtr(f.record.ResolutionID.String())}
			result, err := s.SetStateReportWithReconciliation(context.Background(), f.worker, f.run.ID, req)
			requirePolicyOwnerStopConflict(t, err, "policy_stop_execution_unverifiable")
			if result.Applied || result.PolicyStopExecution != nil || q.reads != 1 ||
				!bytes.Equal(before, mustStopJSON(t, q.run)) {
				t.Fatal("absent receipt gained authority or changed owned row")
			}
			// The embedded nil Store also makes every unexpected mutation panic.
		})
	}
}

func TestPolicyStopStateRequestScalarWire(t *testing.T) {
	const canonical = "12345678-1234-4abc-8def-123456789abc"
	for _, value := range []*string{nil, strPtr(canonical)} {
		req := StateRequest{PolicyStopResolutionID: value}
		if err := validatePolicyStopStateRequest(req); err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		got, present := fields["policy_stop_resolution_id"]
		if present != (value != nil) || (present && string(got) != `"`+canonical+`"`) {
			t.Fatalf("scalar field=%s present=%v", got, present)
		}
	}
}

func TestPolicyStopStateRequestLockedEvidence(t *testing.T) {
	for _, status := range []string{"claimed", "running", "awaiting_approval", "awaiting_input"} {
		t.Run(status, func(t *testing.T) {
			for _, mode := range []string{"exact", "missing", "mismatch", "absent", "absent_scalar",
				"wrong_worker", "wrong_user", "wrong_generation", "missing_generation", "unnegotiated", "old_worker", "invalid_evidence"} {
				t.Run(mode, func(t *testing.T) {
					f := stopExecutionFixtureFor(t, "legacy", false)
					f.run.Status = status
					f.worker.ProtocolCapabilities = []string{capability.ProviderPolicyHoldV1, capability.CredentialSwitchV1}
					s := stopProjectionService()
					generation := f.run.ClaimGeneration
					req := StateRequest{State: "running", ClaimGeneration: &generation, PolicyStopResolutionID: strPtr(f.record.ResolutionID.String())}
					switch mode {
					case "missing":
						req.PolicyStopResolutionID = nil
					case "mismatch":
						req.PolicyStopResolutionID = strPtr(uuid.NewString())
					case "absent", "absent_scalar":
						f.run.PolicyStopResolution = nil
						if mode == "absent" {
							req.PolicyStopResolutionID = nil
						}
					case "wrong_worker":
						f.worker.ID = uuid.New()
					case "wrong_user":
						f.worker.UserID = uuid.New()
					case "wrong_generation":
						generation--
					case "missing_generation":
						req.ClaimGeneration = nil
					case "unnegotiated":
						s.p.PolicyStopNegotiated = false
					case "old_worker":
						f.worker.ProtocolCapabilities = nil
					case "invalid_evidence":
						f.run.PolicyStopResolution = []byte("{}")
					}
					snapshot := func() []byte {
						return mustStopJSON(t, []any{f.run, f.reader.scope, f.reader.inputs, f.reader.checks, f.reader.pending})
					}
					before := snapshot()
					err := s.policyStopStateRequestLocked(context.Background(), f.reader, f.worker, f.run, req)
					wantOK := mode == "exact" || mode == "absent"
					if (err == nil) != wantOK {
						t.Fatalf("request validation err=%v", err)
					}
					if !bytes.Equal(before, snapshot()) {
						t.Fatal("request validation changed evidence")
					}
				})
			}
		})
	}
}
