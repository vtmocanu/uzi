package handler

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4488: every gated run-create site maps the terminal consumer gate refusals the same
// way: busy is a retryable 503 with Retry-After (never a 500), identity is a 409.

type gateSite struct {
	name  string
	write func(w http.ResponseWriter, err error)
}

func terminalGateSites() []gateSite {
	return []gateSite{
		{"writeStartRunError (issue run, task run, chat start)", func(w http.ResponseWriter, err error) {
			(&Handler{}).writeStartRunError(w, httptest.NewRequest(http.MethodPost, "/x", nil), err)
		}},
		{"mr-rework start", func(w http.ResponseWriter, err error) { writeStartReworkError(w, uuid.New(), err) }},
		{"ci-fix create", writeCIFixCreateError},
		{"judge rerun", writeRerunJudgeError},
		{"schedule run-now", func(w http.ResponseWriter, err error) { writeRunScheduleNowError(w, uuid.New(), err) }},
	}
}

func TestTerminalConsumerGateBusyIs503RetryAfterAtEverySite(t *testing.T) {
	// A wrapped, joined error exactly as the service returns it (busy joined with a driver error).
	busy := fmt.Errorf("create: %w", errors.Join(workersvc.ErrTerminalConsumerGateBusy, errors.New("lock timeout")))
	for _, s := range terminalGateSites() {
		t.Run(s.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.write(rec, busy)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
			if b := rec.Body.String(); !strings.Contains(b, "retry") || !strings.Contains(b, `"error"`) {
				t.Fatalf("body = %s, want a JSON error telling the caller to retry", b)
			}
		})
	}
}

func TestTerminalConsumerGateIdentityIs409AtEverySite(t *testing.T) {
	ident := fmt.Errorf("create: %w", workersvc.ErrTerminalConsumerGateIdentity)
	for _, s := range terminalGateSites() {
		t.Run(s.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.write(rec, ident)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Retry-After"); got != "" {
				t.Fatalf("Retry-After = %q on a non-retryable refusal, want none", got)
			}
		})
	}
}

// The helper must not swallow unrelated errors, so each site's own mapping still applies.
func TestTerminalConsumerGateRefusalIgnoresOtherErrors(t *testing.T) {
	for _, err := range []error{errors.New("boom"), workersvc.ErrRunNotFound, store.ErrTerminalConsumerGateIsolation} {
		rec := httptest.NewRecorder()
		if writeTerminalConsumerGateRefusal(rec, err) {
			t.Fatalf("%v was claimed by the gate helper", err)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("helper wrote a body for %v", err)
		}
	}
}
