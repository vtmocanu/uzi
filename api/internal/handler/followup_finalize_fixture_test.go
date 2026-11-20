package handler

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4479: the request half of the shared wire fixture must decode through the strict decoder
// WorkerRunState uses, so a key renamed in the fixture or in workersvc.StateRequest reddens.
func TestFollowupFinalizeFixtureDecodes(t *testing.T) {
	fx := readFollowupFinalizeFixture(t)
	decode := func(body []byte) (workersvc.StateRequest, error) {
		var req workersvc.StateRequest
		err := httpx.DecodeJSON(httptest.NewRequest(http.MethodPost, "/state", bytes.NewReader(body)), &req)
		return req, err
	}
	req, err := decode(fx.requestRaw)
	if err != nil {
		t.Fatalf("fixture request rejected by the strict decoder: %v", err)
	}
	if req.State != "followup_finalize" || req.ClaimGeneration == nil || *req.ClaimGeneration != 1 || req.FollowupParkSeq == nil || *req.FollowupParkSeq != 1 {
		t.Fatalf("decoded request = %+v", req)
	}
	bad := strings.Replace(string(fx.requestRaw), `"followup_park_seq"`, `"followup_park_seq_x"`, 1)
	if _, err := decode([]byte(bad)); err == nil {
		t.Fatalf("an unknown key was accepted")
	}
}
