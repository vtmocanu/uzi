package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSetRunCredentialHoldSettlingIs409 (issue #3181): the set-token surface answers a refused
// switch on a still-settling credential-switch hold with 409 and the operator-facing message.
func TestSetRunCredentialHoldSettlingIs409(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.writeSetRunCredentialError(rec, workersvc.ErrRunHoldSettling)
	const want = "the worker is still stopping this run's processes; Resume becomes available when it finishes, or Cancel the run"
	if rec.Code != http.StatusConflict {
		t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if got := workersvc.ErrRunHoldSettling.Error(); got != want {
		t.Fatalf("ErrRunHoldSettling = %q, want %q", got, want)
	}
	if body := rec.Body.String(); !strings.Contains(body, want) {
		t.Fatalf("body %q does not carry %q", body, want)
	}
}
