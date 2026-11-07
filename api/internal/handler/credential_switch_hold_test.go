package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestSetRunCredentialCredentialsResetHoldIs409 (PRD #3989 M3): set-token on a credentials_reset
// hold answers 409 with the same named text the owner Resume refusal uses.
func TestSetRunCredentialCredentialsResetHoldIs409(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.writeSetRunCredentialError(rec, workersvc.ErrCredentialsResetHeld)
	const want = "credentials were reset; add a credential in Settings and the run resumes automatically"
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("code=%d body=%s, want 409 carrying %q", rec.Code, rec.Body.String(), want)
	}
	if credentialsResetResumeRefusal != want {
		t.Fatalf("resume refusal = %q, want %q", credentialsResetResumeRefusal, want)
	}
}

// TestSetRunCredentialHoldSettlingIs409 (issue #3181): the set-token surface answers a refused
// switch on a still-settling credential-switch hold with 409 and the operator-facing message.
func TestSetRunCredentialHoldSettlingIs409(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.writeSetRunCredentialError(rec, workersvc.ErrRunHoldSettling)
	const want = "the worker is still finishing its own operations for this run; try Resume again shortly, or Cancel the run"
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
