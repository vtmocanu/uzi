package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/recovery"
)

func TestMapRecoveryErrorReason(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		msg    string
		reason string
	}{
		{"bad request", recovery.ErrBadRequest, http.StatusBadRequest, "invalid recovery request", "bad_request"},
		{"not authorized", recovery.ErrNotAuthorized, http.StatusForbidden, "not authorized for this capture", "not_authorized"},
		{"ambiguous", recovery.ErrAmbiguous, http.StatusConflict, "ambiguous open custody generation; name the generation", "ambiguous_generation"},
		{"capture not found", recovery.ErrCaptureNotFound, http.StatusNotFound, "capture not found", "capture_not_found"},
		{"not available", recovery.ErrNotAvailable, http.StatusConflict, "capture is not available", "not_available"},
		{"oversize", recovery.ErrOversize, http.StatusRequestEntityTooLarge, "bundle exceeds the maximum size", "oversize"},
		{"manifest conflict", recovery.ErrManifestConflict, http.StatusConflict, "a different manifest is already bound for this capture", "manifest_conflict"},
		{"quota", recovery.ErrQuota, http.StatusInsufficientStorage, "storage quota exceeded", "quota"},
		{"integrity", recovery.ErrIntegrity, http.StatusUnprocessableEntity, "archive integrity check failed", "integrity"},
		{"busy", recovery.ErrBusy, http.StatusServiceUnavailable, "too many concurrent transfers; retry shortly", "busy"},
		{"unknown", errors.New("boom"), http.StatusInternalServerError, "internal error", "internal"},
	}
	for _, tc := range cases {
		for _, wrapped := range []bool{false, true} {
			err := tc.err
			name := tc.name
			if wrapped {
				err = fmt.Errorf("x: %w", tc.err)
				name += " wrapped"
			}
			t.Run(name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				mapRecoveryError(rec, err)
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d", rec.Code, tc.status)
				}
				var body map[string]string
				if e := json.Unmarshal(rec.Body.Bytes(), &body); e != nil {
					t.Fatalf("body not JSON: %v", e)
				}
				if body["error"] != tc.msg {
					t.Errorf("error = %q, want %q", body["error"], tc.msg)
				}
				if body["reason"] != tc.reason {
					t.Errorf("reason = %q, want %q", body["reason"], tc.reason)
				}
			})
		}
	}
}
