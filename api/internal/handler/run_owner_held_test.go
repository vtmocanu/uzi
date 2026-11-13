package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			var msg struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}
			if msg.Error != tc.want {
				t.Fatalf("error = %q, want exactly %q", msg.Error, tc.want)
			}
		})
	}
}
