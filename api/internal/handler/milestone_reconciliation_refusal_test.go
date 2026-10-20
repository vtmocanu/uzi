package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCreateRunInputReconciliationNotReadyOwnerAction(t *testing.T) {
	for name, revisions := range map[string]int32{"revision_budget_remaining": 0, "revision_budget_exhausted": 3} {
		t.Run(name, func(t *testing.T) {
			owner := store.User{ID: uuid.New()}
			runID := uuid.New()
			st := &runsStore{ownerID: owner.ID, run: store.Run{
				ID: runID, UserID: owner.ID, Status: "awaiting_approval", ReviseCount: revisions,
			}}
			h := &Handler{wsvc: workersvc.New(st, newHandlerTestBox(t), workersvc.Params{CustodyHoldLimit: defaultCustodyLimit(), PlanMaxRevisions: 3})}
			// The service has no recovery transaction backend: a valid reset request
			// reaches ErrReconciliationNotReady without permitting any writes.
			for attempt := 0; attempt < 2; attempt++ {
				rec := httptest.NewRecorder()
				h.CreateRunInput(rec, inputReq(owner, runID,
					`{"kind":"approve_plan","expected_gate_revision":1,"expected_reset_state_digest":"digest","reconciliation":{"mode":"reset","reason":"replace milestones","expected_completion_revision":null}}`))
				if rec.Code != http.StatusConflict {
					t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
				}
				var body struct {
					Error  string `json:"error"`
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Reason != "milestone_reconciliation_not_ready" {
					t.Fatalf("reason = %q", body.Reason)
				}
				for _, hint := range []string{"published checkpoint witness", "plan revision", "limit is exhausted", "cancel", "start a new run", "Retrying approval cannot create a missing witness"} {
					if !strings.Contains(body.Error, hint) {
						t.Errorf("error %q lacks owner guidance %q", body.Error, hint)
					}
				}
				if st.createdApproval != nil || st.createInputRow.ID != 0 || st.run.Status != "awaiting_approval" || st.run.ReviseCount != revisions {
					t.Fatal("refused approval changed the run or enqueued input")
				}
			}
		})
	}
}
