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
)

func TestWorkerCodexAdmissionStrictBody(t *testing.T) {
	valid := fmt.Sprintf(`{"claim_generation":1,"attempt_id":%q,"class":"rate_limit"}`, uuid.NewString())
	for _, body := range []string{
		"{}", valid + "{}", strings.TrimSuffix(valid, "}") + `,"user_id":"x"}`,
		strings.TrimSuffix(valid, "}") + `,"account_id":"x"}`,
		strings.TrimSuffix(valid, "}") + `,"retry_after":30}`,
		strings.TrimSuffix(valid, "}") + `,"raw_text":"x"}`,
		strings.TrimSuffix(valid, "}") + `,"provider_turn_id":"\u001b[31m"}`,
		strings.TrimSuffix(valid, "}") + `,"provider_thread_id":"` + strings.Repeat("a", 2048) + `"}`,
		strings.Replace(valid, `"claim_generation":1`, `"claim_generation":9007199254740992`, 1),
		strings.Replace(valid, `"claim_generation":1`, `"claim_generation":1.5`, 1),
	} {
		rec := httptest.NewRecorder()
		(&Handler{}).WorkerCodexAdmissionSignal(rec, codexWorkerReq(true, uuid.NewString(), body))
		if rec.Code != http.StatusBadRequest || errorField(t, rec.Body.Bytes()) != codexErrInvalid {
			t.Fatalf("invalid body returned %d %q", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
	}
	rec := httptest.NewRecorder()
	(&Handler{}).WorkerCodexAdmissionSignal(rec, codexWorkerReq(false, uuid.NewString(), valid))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d", rec.Code)
	}
}

func TestWorkerCodexAdmissionProtocolAndRoute(t *testing.T) {
	if !slices.Contains(protocolFeatures(false, false), "codex_account_admission_signal_v1") {
		t.Fatal("missing admission feature")
	}
	router, ok := (&Handler{}).WorkerRoutes(nil).(chi.Routes)
	if !ok {
		t.Fatal("not walkable")
	}
	mounted := false
	err := chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == "POST" && pattern == "/api/worker/runs/{id}/codex/admission-signal" {
			mounted = true
		}
		return nil
	})
	if err != nil || !mounted {
		t.Fatalf("route mounted = %v: %v", mounted, err)
	}
}
