package handler

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestPolicyHoldNotReady(t *testing.T) {
	if slices.Contains(protocolFeatures(true, true), capability.ProviderPolicyHoldV1) {
		t.Fatal("incomplete policy continuation advertised ready")
	}
}

func TestPolicyHoldRequiresWorker(t *testing.T) {
	w := httptest.NewRecorder()
	(&Handler{}).WorkerRunPolicyHold(w, httptest.NewRequest(http.MethodPost, "/api/worker/runs/id/policy-hold", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d", w.Code)
	}
}

func TestPolicyHoldStrictBody(t *testing.T) {
	for name, body := range map[string]string{
		"unknown":   `{"execution":{}}`,
		"trailing":  `{} {}`,
		"overflow":  `{}` + strings.Repeat(" ", 17*1024),
		"malformed": `{`,
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			var req workersvc.PolicyHoldRequest
			if httpx.DecodeJSONStrictBounded(r, &req, 17*1024) == nil {
				t.Fatal("accepted invalid body")
			}
		})
	}
}
