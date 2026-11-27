package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestWriteV1JobErrorMapsProviderLaneRefusal pins issue #4541: a job create is refused with
// ErrProviderLaneUnsupported (a site-list job under a selected Claude source, PRD #4322 M1b), and the
// v1 mapper must answer the shared typed 409 rather than a logged 500. The representative job
// mappings around it stay put.
func TestWriteV1JobErrorMapsProviderLaneRefusal(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		reason string
		msg    string
	}{
		{"lane (sentinel)", workersvc.ErrProviderLaneUnsupported, http.StatusConflict, "provider_lane_unsupported",
			"this run type does not support a model-provider source yet"},
		{"lane (wrapped)", fmt.Errorf("create job: %w", workersvc.ErrProviderLaneUnsupported), http.StatusConflict, "provider_lane_unsupported",
			"this run type does not support a model-provider source yet"},
		{"invalid", workersvc.ErrJobInvalid, http.StatusUnprocessableEntity, v1ReasonInvalidRequest, ""},
		{"unknown type", workersvc.ErrJobTypeUnknown, http.StatusUnprocessableEntity, v1ReasonUnknownJobType, ""},
		{"over cap", workersvc.ErrJobOverCap, http.StatusTooManyRequests, v1ReasonOverCap, ""},
		{"terminal", workersvc.ErrJobTerminal, http.StatusConflict, v1ReasonJobTerminal, ""},
		{"internal", errors.New("boom"), http.StatusInternalServerError, "", "internal error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeV1JobError(rec, "create", c.err)
			var body struct {
				Error  string `json:"error"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if rec.Code != c.status || body.Reason != c.reason || (c.msg != "" && body.Error != c.msg) {
				t.Fatalf("got %d %s, want %d reason=%q msg=%q", rec.Code, rec.Body.String(), c.status, c.reason, c.msg)
			}
		})
	}
}
