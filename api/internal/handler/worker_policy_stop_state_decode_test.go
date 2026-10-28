package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Decoder contract only: these requests do not exercise HTTP authentication.
func TestWorkerPolicyStopStateDecode(t *testing.T) {
	const resolution = "12345678-1234-4abc-8def-123456789abc"
	for _, tc := range []struct {
		name       string
		extra      string
		wantScalar bool
		wantError  string
	}{
		{name: "absent"},
		{name: "null", extra: `,"policy_stop_resolution_id":null`},
		{name: "valid", extra: `,"policy_stop_resolution_id":"` + resolution + `"`, wantScalar: true},
		{name: "execution", extra: `,"policy_stop_execution":{"resolution_id":"` + resolution + `","claim_generation":"4"}`, wantError: `unknown field "policy_stop_execution"`},
		{name: "scope", extra: `,"scope":{"ceiling":1}`, wantError: `unknown field "scope"`},
		{name: "execution_scope", extra: `,"execution_scope":{"milestones":[]}`, wantError: `unknown field "execution_scope"`},
		{name: "object", extra: `,"policy_stop_resolution_id":{"resolution_id":"` + resolution + `"}`, wantError: "cannot unmarshal object"},
		{name: "number", extra: `,"policy_stop_resolution_id":42`, wantError: "cannot unmarshal number"},
		{name: "boolean", extra: `,"policy_stop_resolution_id":true`, wantError: "cannot unmarshal bool"},
		{name: "array", extra: `,"policy_stop_resolution_id":[]`, wantError: "cannot unmarshal array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/worker/runs/"+resolution+"/state",
				strings.NewReader(`{"status":"running","claim_generation":4`+tc.extra+`}`))
			var req workersvc.StateRequest
			err := httpx.DecodeJSONStrict(r, &req)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("decode err=%v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if req.State != "running" || req.ClaimGeneration == nil || *req.ClaimGeneration != 4 ||
				(req.PolicyStopResolutionID != nil) != tc.wantScalar {
				t.Fatalf("decoded request=%+v", req)
			}
			if tc.wantScalar && *req.PolicyStopResolutionID != resolution {
				t.Fatalf("scalar=%q", *req.PolicyStopResolutionID)
			}
		})
	}
}

// Middleware context is injected here; this is route rejection, not Bearer auth proof.
func TestWorkerPolicyStopStateRouteRefusals(t *testing.T) {
	const resolution = "12345678-1234-4abc-8def-123456789abc"
	for _, tc := range []struct {
		name, body string
		status     int
		reason     string
	}{
		{"malformed", `{"status":"running","policy_stop_resolution_id":"bad"}`, http.StatusBadRequest, ""},
		{"execution", `{"status":"running","policy_stop_execution":{"resolution_id":"` + resolution + `","claim_generation":"4"}}`, http.StatusBadRequest, ""},
		{"scope", `{"status":"running","execution_scope":{"ceiling":1}}`, http.StatusBadRequest, ""},
		{"trailing_object", `{"status":"running"} {"policy_stop_execution":{}}`, http.StatusBadRequest, ""},
		{"trailing_scalar", `{"status":"running"} true`, http.StatusBadRequest, ""},
		{"unsolicited", `{"status":"running","policy_stop_resolution_id":"` + resolution + `"}`, http.StatusConflict, "policy_stop_execution_unverifiable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Unexpected store methods panic; malformed requests use a nil service,
			// so neither decode nor scalar rejection may reach the store.
			h := &Handler{}
			if tc.name == "malformed" {
				h = newProtocolHandler(t, nil)
			}
			if tc.name == "unsolicited" {
				h = newProtocolHandler(t, &protocolStore{ownedRun: store.Run{ID: uuid.New()}})
			}
			rec := httptest.NewRecorder()
			h.WorkerRunState(rec, workerReq(http.MethodPost, tc.body, uuid.New()))
			if rec.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &fields); err != nil {
				t.Fatal(err)
			}
			if _, ok := fields["policy_stop_execution"]; ok {
				t.Fatal("refusal issued execution authority")
			}
			if tc.reason != "" && string(fields["reason"]) != `"`+tc.reason+`"` {
				t.Fatalf("reason=%s", fields["reason"])
			}
		})
	}
}
