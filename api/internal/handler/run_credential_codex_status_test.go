package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCodexCredentialSwitchRefusalStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		text   string
	}{
		{"park or queued", workersvc.ErrCodexSwitchIneligible, http.StatusUnprocessableEntity, workersvc.ErrCodexSwitchIneligible.Error()},
		{"claimed", workersvc.ErrCredentialSwitchClaimAssembling, http.StatusConflict, "claim is being assembled"},
		{"foreign run", workersvc.ErrRunNotFound, http.StatusNotFound, "run not found"},
		{"policy episode", &workersvc.RunOwnerHeldError{Label: "policy hold"}, http.StatusConflict, "policy hold"},
		{"unsupported worker", &workersvc.CredentialSwitchWorkerUnsupportedError{WorkerName: "older worker"}, http.StatusConflict, "must be upgraded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&Handler{}).writeSetRunCredentialError(recorder, tc.err)
			if recorder.Code != tc.status || !strings.Contains(recorder.Body.String(), tc.text) {
				t.Fatalf("got HTTP %d %s, want HTTP %d containing %q", recorder.Code, recorder.Body.String(), tc.status, tc.text)
			}
		})
	}
}
