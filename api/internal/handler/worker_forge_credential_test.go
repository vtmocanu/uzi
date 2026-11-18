package handler

import (
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkerForgeCredentialPreServiceGuards(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		name     string
		worker   bool
		id, body string
		status   int
	}{
		{"unauthenticated", false, uuid.NewString(), `{"claim_generation":1}`, http.StatusUnauthorized},
		{"invalid_run", true, "bad", `{"claim_generation":1}`, http.StatusBadRequest},
		{"missing_generation", true, uuid.NewString(), `{}`, http.StatusBadRequest},
		{"zero_generation", true, uuid.NewString(), `{"claim_generation":0}`, http.StatusBadRequest},
		{"negative_generation", true, uuid.NewString(), `{"claim_generation":-1}`, http.StatusBadRequest},
		{"unknown_field", true, uuid.NewString(), `{"claim_generation":1,"private_key":"private-key-canary"}`, http.StatusBadRequest},
		{"trailing_json", true, uuid.NewString(), `{"claim_generation":1}{}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.WorkerForgeCredential(rr, codexWorkerReq(tc.worker, tc.id, tc.body))
			if rr.Code != tc.status {
				t.Fatalf("status %d, expected %d", rr.Code, tc.status)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("credential response can be cached")
			}
			if strings.Contains(rr.Body.String(), "private-key-canary") {
				t.Fatal("request credential leaked in response")
			}
		})
	}
}
