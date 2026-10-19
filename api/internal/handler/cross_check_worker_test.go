package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCrossCheckResponseRowIdentity(t *testing.T) {
	const rowID = "30680000-0000-0000-0000-000000000001"
	child := uuid.MustParse("30680000-0000-0000-0000-000000000002")
	for _, hasChild := range []bool{true, false} {
		t.Run(map[bool]string{true: "child", false: "absent child"}[hasChild], func(t *testing.T) {
			cc := store.CrossCheck{ID: uuid.MustParse(rowID)}
			if hasChild {
				cc.CheckerRunID = pgconv.UUID(child)
			}
			raw, err := json.Marshal(crossCheckResponse(cc, 7))
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["cross_check_id"]) != `"`+rowID+`"` {
				t.Fatalf("row identity missing or replaced by child: %s", raw)
			}
			wantChild := "null"
			if hasChild {
				wantChild = `"` + child.String() + `"`
			}
			if string(fields["checker_run_id"]) != wantChild {
				t.Fatalf("child provenance = %s, want %s", fields["checker_run_id"], wantChild)
			}
		})
	}
}

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
