package handler

import (
	"encoding/json"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestGetRunCapabilityProvenance(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	rid := uuid.New()
	h := newRunsHandler(t, &runsStore{ownerID: owner.ID, run: store.Run{ID: rid, UserID: owner.ID, Status: "completed",
		RequiredCapabilities: []string{"jvm"}, HintedCapabilities: []string{"jvm"}, IgnoredInferredCapabilities: []string{"jvm"},
		CapabilityInferenceEvidence: []byte(`{"jvm":{"triggers":[{"path":"app/android/build.gradle","reason":"jvm_manifest"}],"truncated":false}}`)}})
	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(owner, rid))
	var response struct {
		Run apitypes.RunDTO `json:"run"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
		t.Fatalf("GET run: %d %s", rec.Code, rec.Body.String())
	}
	p := response.Run.CapabilityProvenance
	if p == nil || !p.HintsKnown || !p.InferenceKnown || len(p.Capabilities) != 1 || len(p.Capabilities[0].Sources) != 2 || p.Capabilities[0].Triggers[0].Path != "app/android/build.gradle" {
		t.Fatalf("missing detail provenance: %+v", p)
	}
	other := httptest.NewRecorder()
	h.GetRun(other, runReq(store.User{ID: uuid.New()}, rid))
	if other.Code != http.StatusNotFound || strings.Contains(other.Body.String(), "build.gradle") {
		t.Fatal("foreign detail leaked provenance")
	}
}

func TestWorkerCapabilityEvidenceDecoding(t *testing.T) {
	for _, extra := range []string{"", `,"capability_inference_evidence":{}`, `,"capability_inference_evidence":{"jvm":{"triggers":[{"path":"app/android/build.gradle","reason":"jvm_manifest"}],"truncated":false}}`} {
		body := `{"status":"awaiting_approval","claim_generation":1,"plan_md":"plan"` + extra + `}`
		var current workersvc.StateRequest
		if err := httpx.DecodeJSON(httptest.NewRequest("POST", "/state", strings.NewReader(body)), &current); err != nil {
			t.Fatal(err)
		}
		if current.State != "awaiting_approval" || current.ClaimGeneration == nil || *current.ClaimGeneration != 1 || current.PlanMd == nil || *current.PlanMd != "plan" {
			t.Fatal("control fields changed")
		}
		if (len(current.CapabilityInferenceEvidence) > 0) != (extra != "") {
			t.Fatal("unknown and known-empty evidence collapsed")
		}
		var legacy struct {
			State           string `json:"status"`
			ClaimGeneration int64  `json:"claim_generation"`
			PlanMd          string `json:"plan_md"`
		}
		err := httpx.DecodeJSON(httptest.NewRequest("POST", "/state", strings.NewReader(body)), &legacy)
		if (err != nil) != (extra != "") {
			t.Fatalf("legacy negotiation control: extra=%q err=%v", extra, err)
		}
	}
}
