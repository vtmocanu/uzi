package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestSetRunCredentialOwnerHeldIs409(t *testing.T) {
	const base = "this owner hold prevents switching the token; inspect the run's recovery options: Resume may re-hold it, Cancel ends the run"
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"labelled", &workersvc.RunOwnerHeldError{Label: "worker memory pressure"}, base + " (hold: worker memory pressure)"},
		{"unlabelled", &workersvc.RunOwnerHeldError{}, base},
		{"bare sentinel", workersvc.ErrRunOwnerHeld, base},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{}
			rec := httptest.NewRecorder()
			h.writeSetRunCredentialError(rec, tc.err)
			if rec.Code != http.StatusConflict {
				t.Fatalf("code = %d, want 409; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("body %q does not carry %q", rec.Body.String(), tc.want)
			}
			if tc.name != "labelled" && strings.Contains(rec.Body.String(), "(hold:") {
				t.Fatalf("unexpected label in %q", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "changed state") {
				t.Fatalf("mapped as race: %q", rec.Body.String())
			}
		})
	}
}
