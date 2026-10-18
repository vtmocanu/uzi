package workersvc

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// These tests marshal the actual service result: referencing a future DTO field
// would turn the missing projection into a compile failure instead of assertion RED.
func TestRequestCompletionPermitScopeJSON(t *testing.T) {
	for _, tc := range []struct {
		name      string
		decision  *CompletionDecisionInput
		completed []string
		wantScope string
	}{
		{"no_decision", nil, []string{"m1", "m2", "m3"}, `{}`},
		{"deferred", &CompletionDecisionInput{Decision: "partial", Keep: []string{"m1", "m3"}, Reason: "ship the other milestones"}, []string{"m1", "m3"},
			`{"deferred":[{"milestone_id":"m2","title":"Second milestone","reason":"ship the other milestones"}]}`},
		{"accepted", &CompletionDecisionInput{Decision: "accept", Criteria: []string{"m2.c1"}, Reason: "owner accepts the limitation"}, []string{"m1", "m3"},
			`{"accepted":[{"id":"m2.c1","milestone_id":"m2","text":"Second milestone","reason":"owner accepts the limitation"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := worker()
			run := interlockedRun(w)
			run.CompletionContract = titledContract(t)
			run.MilestonesFrozen = milestonesJSON(t, titledMilestones()...)
			run.MilestonesCompleted = idsJSON(t, tc.completed...)
			revision := 1
			if tc.decision != nil {
				var err error
				run.CompletionContract, err = buildRevisedContract(run.CompletionContract, *tc.decision, titledMilestones(), 2)
				if err != nil {
					t.Fatalf("build decision fixture: %v", err)
				}
				revision = 2
				run.ContractRevision = pgtype.Int4{Int32: 2, Valid: true}
			}
			fs := &fakeStore{runOwned: run}
			svc := New(fs, newBox(t), testParams())
			res, err := svc.RequestCompletionPermit(context.Background(), w, run.ID, CompletionPermitRequest{
				ContractRevision: revision, Branch: "agent/issue-1", Head: "abc123",
			})
			if err != nil || !res.Granted || res.Permit == nil {
				t.Fatalf("RequestCompletionPermit must grant: result=%+v err=%v", res, err)
			}
			if fs.upsertPermitParams == nil || fs.upsertPermitParams.RunID != run.ID ||
				int(fs.upsertPermitParams.ContractRevision) != revision {
				t.Fatalf("permit must bind the source run/revision: %+v", fs.upsertPermitParams)
			}
			assertCompletionPermitScopeJSON(t, res, revision, tc.wantScope)
		})
	}
}

// The expected JSON is a literal contract, independent of CompletionScopeClaim.
func assertCompletionPermitScopeJSON(t *testing.T, res CompletionPermitResult, revision int, wantScope string) {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal permit service result: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decode permit service result: %v", err)
	}
	permit, ok := result["permit"].(map[string]any)
	if !ok {
		t.Fatalf("grant must contain permit object: %s", raw)
	}
	if got := permit["contract_revision"]; got != float64(revision) {
		t.Errorf("permit contract_revision = %v, want exactly %d", got, revision)
	}
	scope, ok := permit["completion_scope"].(map[string]any)
	if !ok {
		t.Fatalf("permit completion_scope must be a JSON object ({} with no decision): %s", raw)
	}
	var want map[string]any
	if err := json.Unmarshal([]byte(wantScope), &want); err != nil {
		t.Fatalf("decode expected scope: %v", err)
	}
	if !reflect.DeepEqual(scope, want) {
		t.Errorf("permit completion_scope = %v, want %s", scope, wantScope)
	}
}
