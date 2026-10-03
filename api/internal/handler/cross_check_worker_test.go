package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCrossCheckMilestonesScrubEscapedContent(t *testing.T) {
	token := "glpat-" + "abcdefghijklmnopqrst"
	raw := []byte(`[{"title":"` + "glpat-" + `\u0061bcdefghijklmnopqrst \u202e"}]`)
	candidate, err := workersvc.NormalizePlanCrossCheckCandidate(workersvc.PlanCrossCheckCandidate{Milestones: raw})
	clean := candidate.Milestones
	if err != nil {
		t.Fatal("valid escaped milestones rejected")
	}
	if strings.Contains(string(clean), token) || strings.Contains(string(clean), "\\u202e") {
		t.Fatalf("escaped credential or bidi control survived scrub: %s", clean)
	}
	for _, key := range []string{
		`[{"` + "glpat-" + `\u0061bcdefghijklmnopqrst":"ok"}]`,
		`[{"\u202e":"ok"}]`,
	} {
		if _, err := workersvc.NormalizePlanCrossCheckCandidate(workersvc.PlanCrossCheckCandidate{Milestones: []byte(key)}); err == nil {
			t.Fatalf("unsafe milestone key accepted: %s", key)
		}
	}
}

func TestCrossCheckWorkerRoutesRejectUnfencedAndUnknownBodies(t *testing.T) {
	h := &Handler{}
	worker := store.Worker{ID: uuid.New(), UserID: uuid.New()}
	for _, tc := range []struct {
		body string
		call func(http.ResponseWriter, *http.Request)
	}{
		{`{"stage":"plan","plan_md":"x"}`, h.WorkerSubmitPlanCrossCheck},
		{`{"stage":"plan","claim_generation":1,"secret_id":"foreign"}`, h.WorkerSubmitPlanCrossCheck},
		{`{"verdict":"approve","summary":"ok"}`, h.WorkerCrossCheckVerdict},
		{`{"verdict":"approve","claim_generation":1,"token":"foreign"}`, h.WorkerCrossCheckVerdict},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+uuid.New().String(), strings.NewReader(tc.body))
		route := chi.NewRouteContext()
		route.URLParams.Add("id", uuid.New().String())
		req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), worker))
		rec := httptest.NewRecorder()
		tc.call(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", tc.body, rec.Code)
		}
	}
}
