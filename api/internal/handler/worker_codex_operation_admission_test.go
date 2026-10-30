package handler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerCodexOperationAdmissionStrictBody(t *testing.T) {
	valid := fmt.Sprintf(`{"claim_generation":1,"operation_id":%q,"kind":"root"}`, uuid.NewString())
	for _, body := range []string{
		"null", "{}", valid + "{}", valid + strings.Repeat(" ", 4096),
		strings.TrimSuffix(valid, "}") + `,"account_id":"x"}`,
		strings.Replace(valid, `"root"`, `"unknown"`, 1),
		strings.Replace(valid, `"claim_generation":1`, `"claim_generation":0`, 1),
		strings.Replace(valid, `"claim_generation":1`, `"claim_generation":1.5`, 1),
		strings.Replace(valid, `"claim_generation":1`, `"claim_generation":9007199254740992`, 1),
		strings.TrimSuffix(valid, "}") + `,"source":{"gate_revision":1,"presentation_id":"bad","revision_input_id":1}}`,
		strings.TrimSuffix(valid, "}") + `,"source":{"gate_revision":1,"presentation_id":"bad","revision_input_id":1,"extra":true}}`,
	} {
		rec := httptest.NewRecorder()
		(&Handler{}).WorkerCodexOperationAdmission(rec, codexWorkerReq(true, uuid.NewString(), body))
		if rec.Code != http.StatusBadRequest || errorField(t, rec.Body.Bytes()) != codexErrInvalid {
			t.Fatalf("invalid body returned %d %q", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Body.Len() > 8192 {
			t.Fatal("unbounded/cacheable error")
		}
	}
	h := &Handler{wsvc: workersvc.New(nil, nil, workersvc.Params{})}
	for _, n := range []int{4096, 4097} {
		body := valid + strings.Repeat(" ", n-len(valid))
		req := codexWorkerReq(true, uuid.NewString(), body)
		req.ContentLength = 1 // The actual-byte limit must ignore a lying header.
		rec := httptest.NewRecorder()
		h.WorkerCodexOperationAdmission(rec, req)
		expected := http.StatusServiceUnavailable
		if n > 4096 {
			expected = http.StatusBadRequest
		}
		if rec.Code != expected {
			t.Fatalf("%d actual bytes: %d %s", n, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	h.WorkerCodexOperationAdmission(rec, codexWorkerReq(false, uuid.NewString(), valid))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized=%d", rec.Code)
	}
}

func TestWorkerCodexOperationFeatureAndRoute(t *testing.T) {
	if !slices.Contains(protocolFeatures(false, false), "codex_account_operation_admission_v1") {
		t.Fatal("missing independent feature")
	}
	routes, ok := (&Handler{}).WorkerRoutes(nil).(chi.Routes)
	if !ok {
		t.Fatal("not walkable")
	}
	found := false
	err := chi.Walk(routes, func(method, path string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == "POST" && path == "/api/worker/runs/{id}/codex/operation-admission" {
			found = true
		}
		return nil
	})
	if err != nil || !found {
		t.Fatalf("route missing: %v", err)
	}
}
