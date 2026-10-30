package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func publicationIntentBody(runID uuid.UUID, extra string) string {
	return fmt.Sprintf(`{"claim_generation":1,"intent":{"run_id":%q,"repo_id":%q,"branch":"agent/issue-1","claim_generation":1,`+
		`"original_head":%q,"floor":{"kind":"absent"},"candidate":null%s}}`,
		runID, uuid.New(), strings.Repeat("a", 40), extra)
}

// The endpoint maps every service error to its documented status and machine-readable reason.
func TestWorkerPublicationIntentErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		reason string
	}{
		{"malformed", workersvc.ErrInvalidState, http.StatusBadRequest, ""},
		{"capability missing", workersvc.ErrPublicationResumeUnsupported, http.StatusBadRequest, "publication_resume_unsupported"},
		{"kind unsupported", workersvc.ErrPublicationKindUnsupported, http.StatusBadRequest, "publication_kind_unsupported"},
		{"not owned", workersvc.ErrRunNotOwned, http.StatusNotFound, ""},
		{"stale claim", workersvc.ErrStaleClaim, http.StatusConflict, "stale_claim"},
		{"identity mismatch", workersvc.ErrPublicationIntentMismatch, http.StatusConflict, "publication_intent_mismatch"},
		{"retired", workersvc.ErrPublicationEnrollmentRetired, http.StatusConflict, "publication_enrollment_retired"},
		{"feature off", workersvc.ErrPublicationResumeDisabled, http.StatusServiceUnavailable, ""},
		{"store failure", errors.New("boom"), http.StatusServiceUnavailable, ""},
		{"wrapped", fmt.Errorf("wrap: %w", workersvc.ErrStaleClaim), http.StatusConflict, "stale_claim"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writePublicationIntentError(rec, tc.err)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s, want %d", rec.Code, rec.Body.String(), tc.status)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body["error"] == "" {
				t.Fatalf("body=%s err=%v", rec.Body.String(), err)
			}
			if body["reason"] != tc.reason {
				t.Fatalf("reason=%q, want %q (body %s)", body["reason"], tc.reason, rec.Body.String())
			}
			if tc.err != nil && strings.Contains(rec.Body.String(), "boom") {
				t.Fatalf("internal error text leaked: %s", rec.Body.String())
			}
		})
	}
}

// Through the real handler: body validation and the size bound answer 400 before any store work;
// a well-formed request reaches the service, which here has no transaction wiring (503).
func TestWorkerPublicationIntentHandler(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{})
	run := uuid.New()
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.WorkerPublicationIntent(rec, workerReq(http.MethodPost, body, run))
		return rec
	}
	t.Run("well-formed reaches the service", func(t *testing.T) {
		if rec := post(publicationIntentBody(run, "")); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d body=%s, want 503 without a transaction beginner", rec.Code, rec.Body.String())
		}
	})
	t.Run("run id disagrees with the path", func(t *testing.T) {
		if rec := post(publicationIntentBody(uuid.New(), "")); rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unknown intent member", func(t *testing.T) {
		if rec := post(publicationIntentBody(run, `,"extra":1`)); rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("unknown envelope member", func(t *testing.T) {
		body := strings.Replace(publicationIntentBody(run, ""), `{"claim_generation":1,`, `{"claim_generation":1,"x":1,`, 1)
		if rec := post(body); rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("oversize body", func(t *testing.T) {
		big := publicationIntentBody(run, `,"mr":{"pad":"`+strings.Repeat("x", workersvc.MaxPublicationIntentBytes+2048)+`"}`)
		if rec := post(big); rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d, want 400 for a body past the bound", rec.Code)
		}
	})
	t.Run("oversize intent inside the envelope bound", func(t *testing.T) {
		pad := strings.Repeat("x", workersvc.MaxPublicationIntentBytes-600)
		if rec := post(publicationIntentBody(run, `,"mr":{"pad":"`+pad+`"}`)); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status=%d body=%s, want the in-bound intent to reach the service", rec.Code, rec.Body.String())
		}
		pad = strings.Repeat("x", workersvc.MaxPublicationIntentBytes+200)
		if rec := post(publicationIntentBody(run, `,"mr":{"pad":"`+pad+`"}`)); rec.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s, want 400 for an intent past 64 KiB", rec.Code, rec.Body.String())
		}
	})
	t.Run("no worker", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.WorkerPublicationIntent(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d", rec.Code)
		}
	})
}
