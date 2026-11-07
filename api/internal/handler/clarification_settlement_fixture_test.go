package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #3985: the shared worker-protocol fixture (the agent's test sends the same bytes) must
// decode through the strict decoder WorkerRunState uses, nested object included.
func TestClarificationSettlementFixtureDecodes(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/worker-protocol/clarification-settlement.json")
	if err != nil {
		t.Fatalf("read the wire fixture: %v", err)
	}
	decode := func(body []byte) (workersvc.StateRequest, error) {
		var req workersvc.StateRequest
		err := httpx.DecodeJSON(httptest.NewRequest(http.MethodPost, "/state", bytes.NewReader(body)), &req)
		return req, err
	}
	req, err := decode(raw)
	if err != nil {
		t.Fatalf("fixture rejected by the strict decoder: %v", err)
	}
	if req.State != "clarification_settled" || req.ClaimGeneration == nil || *req.ClaimGeneration != 1 {
		t.Fatalf("decoded request = %+v", req)
	}
	st := req.ClarificationSettlement
	if st == nil || st.QuestionID != "question-1" || st.RevisionInputID != 1 || st.GateRevision != 1 {
		t.Fatalf("decoded settlement = %+v, want question-1 / 1 / 1", st)
	}
	// An unknown nested key must be refused, not ignored.
	bad := strings.Replace(string(raw), `"gate_revision": 1`, `"gate_revision": 1, "extra": 1`, 1)
	if _, err := decode([]byte(bad)); err == nil {
		t.Fatalf("an unknown nested key was accepted")
	}
}
