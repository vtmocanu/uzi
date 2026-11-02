package workersvc

import (
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Exercise both the service and direct SQL entry point so session preservation
// cannot depend on request normalization before ApplyPolicyHold.
func TestPolicyHoldSessionIdentityLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc, _ := e.providerParkService(t, ppBound)
	wid := e.seedWorker(t, []string{capability.ProviderPolicyHoldV1})
	worker, err := e.q.GetWorkerByID(e.ctx, wid)
	if err != nil {
		t.Fatal(err)
	}
	ptr := func(s string) *string { return &s }
	for _, entry := range []string{"service", "sql"} {
		t.Run(entry, func(t *testing.T) {
			for _, tc := range []struct {
				name        string
				existing    *string
				incoming    *string
				constrained bool
				want        *string
			}{
				{"omitted", ptr("original-session"), nil, false, ptr("original-session")},
				{"empty", ptr("original-session"), ptr(""), false, ptr("original-session")},
				{"capture current session", ptr("previous-session"), ptr("current-session"), false, ptr("current-session")},
				{"missing identity", nil, nil, false, nil},
				{"empty missing identity", nil, ptr(""), false, nil},
				{"first identity", nil, ptr("current-session"), false, ptr("current-session")},
				{"constrained omitted", ptr("original-session"), nil, true, ptr("original-session")},
				{"constrained empty", ptr("original-session"), ptr(""), true, ptr("original-session")},
				{"constrained replacement", ptr("original-session"), ptr("different-session"), true, ptr("original-session")},
				{"constrained missing identity", nil, ptr("current-session"), true, ptr("current-session")},
			} {
				t.Run(tc.name, func(t *testing.T) {
					id := e.fpSeedRunning(t, wid, 2)
					e.exec(t, "UPDATE runs SET harness='codex',session_id=$2 WHERE id=$1", id, tc.existing)
					if tc.constrained {
						e.exec(t, "UPDATE runs SET policy_preserve_required=true,policy_outcome_id=$2,policy_continuation_mode='same_conversation',policy_applying_worker_id=$3,policy_applying_generation=1,policy_source_generation=1 WHERE id=$1",
							id, uuid.New(), wid)
					}
					req := policyHoldFixture()
					req.SourceGeneration, req.ClaimGeneration = 2, 2
					req.SessionID = tc.incoming
					if entry == "service" {
						got, err := svc.ReportPolicyHold(e.ctx, worker, id, req)
						if err != nil || got.Disposition != "applied" {
							t.Fatalf("hold: %+v %v", got, err)
						}
					} else {
						_, err := e.q.ApplyPolicyHold(e.ctx, store.ApplyPolicyHoldParams{
							ID: id, UserID: e.userID, WorkerID: pgconv.UUID(wid),
							OutcomeID:       pgconv.UUID(uuid.MustParse(req.OutcomeID)),
							ClaimGeneration: pgconv.Int8Ptr(&req.ClaimGeneration), SourceGeneration: pgconv.Int8Ptr(&req.SourceGeneration),
							ContinuationMode: pgconv.TextPtr(&req.ContinuationMode), ActualRefusal: true,
							PolicyTag: pgconv.TextPtr(req.PolicyTag), Phase: pgconv.TextPtr(req.Phase), CorrelationID: pgconv.TextPtr(req.CorrelationID),
							SessionID: pgconv.TextPtr(req.SessionID),
						})
						if err != nil {
							t.Fatal(err)
						}
					}
					row, err := e.q.GetRunByID(e.ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(row.SessionID, pgconv.TextPtr(tc.want)) {
						t.Fatalf("session=%+v, want %+v", row.SessionID, pgconv.TextPtr(tc.want))
					}
					if row.Status != "paused" || row.HoldReason.String != "codex_policy_hold" || !row.PolicyPreserveRequired {
						t.Fatal("session preservation bypassed hold transition")
					}
				})
			}
		})
	}
}
