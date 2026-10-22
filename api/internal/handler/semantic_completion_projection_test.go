package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestSemanticCompletionWorkerStateAckProjection(t *testing.T) {
	run, _ := semanticCompletionTransportFixture(t, 1)
	want, err := workersvc.CompletionPending(run)
	if err != nil || want == nil || len(want.Criteria) != 1 {
		t.Fatalf("pending fixture: %v", err)
	}
	h := newRunsHandler(t, &runsStore{ownerID: run.UserID, run: run})
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	ack, err := h.workerStateAck(rec, request, run)
	if err != nil {
		t.Fatal(err)
	}
	dto, ok := ack["run"].(apitypes.RunDTO)
	if !ok || !reflect.DeepEqual(dto.CompletionPending, want) {
		t.Fatal("worker ACK differs from frozen run pending projection")
	}
	// The header grants the worker's bounded pending allowance; a structural run gets none.
	if got := rec.Header().Get(apitypes.CompletionPendingHeader); got != apitypes.CompletionPendingVersion {
		t.Fatalf("pending ACK header = %q", got)
	}
	structural := httptest.NewRecorder()
	if _, err := h.workerStateAck(structural, request, store.Run{ID: run.ID, UserID: run.UserID}); err != nil {
		t.Fatal(err)
	}
	if got := structural.Header().Get(apitypes.CompletionPendingHeader); got != "" {
		t.Fatalf("structural ACK carries pending header %q", got)
	}
	criterion := dto.CompletionPending.Criteria[0]
	if uuid.Validate(criterion.CriterionID) != nil || criterion.CriterionID == criterion.MilestoneID || criterion.CriterionID == "c1" || criterion.Status != "unverified" {
		t.Fatalf("worker ACK criterion identity/status: %+v", criterion)
	}
	for _, mode := range []string{"corrupt authorization", "missing authorization"} {
		t.Run(mode, func(t *testing.T) {
			var contract map[string]json.RawMessage
			if err := json.Unmarshal(run.CompletionContract, &contract); err != nil {
				t.Fatal(err)
			}
			if mode == "missing authorization" {
				delete(contract, "maintainer_authorizations")
			} else {
				contract["maintainer_authorizations"] = json.RawMessage(`[{"id":"foreign","revision":1}]`)
			}
			corruptRun := run
			corruptRun.CompletionContract, err = json.Marshal(contract)
			if err != nil {
				t.Fatal(err)
			}
			ack, err := h.workerStateAck(httptest.NewRecorder(), request, corruptRun)
			if err == nil || ack != nil {
				t.Fatalf("invalid authorization returned ACK: ack=%v err=%v", ack, err)
			}
		})
	}
}
