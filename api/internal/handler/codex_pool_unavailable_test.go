package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestStartRunCodexPoolUnavailableReason pins issue #3719 on the start-run refusal: an explicit
// Codex harness whose auto-mode pool has no bindable account keeps the stable
// no_credential_for_harness code (existing clients keep working) and adds a typed, nonsecret
// reason plus a message that points at Settings. The generic credential refusal carries no reason.
//
// MUTATION: delete the ErrCodexPoolUnavailable arm in writeStartRunError; the error then falls to
// the generic no_credential_for_harness arm and the reason assertion fails.
func TestStartRunCodexPoolUnavailableReason(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Handler{}).writeStartRunError(rec, httptest.NewRequest(http.MethodPost, "/", nil), fmt.Errorf("create: %w", workersvc.ErrCodexPoolUnavailable))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	if body["code"] != "no_credential_for_harness" || body["reason"] != "codex_pool_unavailable" {
		t.Fatalf("body = %v, want code no_credential_for_harness and reason codex_pool_unavailable", body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "Settings") {
		t.Fatalf("message %q does not point at Settings", msg)
	}

	rec = httptest.NewRecorder()
	(&Handler{}).writeStartRunError(rec, httptest.NewRequest(http.MethodPost, "/", nil), workersvc.ErrNoCredentialForHarness)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("generic refusal status = %d, want 422", rec.Code)
	}
	body = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, has := body["reason"]; has || body["code"] != "no_credential_for_harness" {
		t.Fatalf("generic refusal body = %v, want the unchanged code and no reason", body)
	}
}
