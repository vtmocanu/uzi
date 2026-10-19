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
)

func TestSemanticWorkerProposalEnvelope(t *testing.T) {
	// The existing compiler admits a complete raw proposal up to 16 MiB.
	// Generation zero stops before storage, after the real route's strict decode.
	proposal := `{"criteria":[]}`
	for _, tc := range []struct {
		name  string
		bytes int
		want  string
	}{
		{"raw proposal ceiling plus transport", 16 << 20, "invalid semantic candidate"},
		{"above envelope", (32 << 20) + 1, "invalid semantic request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"claim_generation":0,"structural":{},"proposal":` +
				proposal + strings.Repeat(" ", tc.bytes-len(proposal)) + `,"sources":[]}`
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			route := chi.NewRouteContext()
			route.URLParams.Add("id", uuid.NewString())
			req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New()}))
			rec := httptest.NewRecorder()
			(&Handler{}).WorkerSemanticCandidate(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestSemanticWorkerAuthentication(t *testing.T) {
	h := &Handler{}
	for _, call := range []func(http.ResponseWriter, *http.Request){
		h.WorkerSemanticCandidate, h.WorkerSemanticCoverageStatus, h.WorkerSemanticCoverageReserve, h.WorkerSemanticCoverageSettle, h.WorkerSemanticCoveragePublish,
	} {
		rec := httptest.NewRecorder()
		call(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d", rec.Code)
		}
	}
}

func TestSemanticWorkerStrictEnvelope(t *testing.T) {
	h := &Handler{}
	for _, call := range []func(http.ResponseWriter, *http.Request){
		h.WorkerSemanticCandidate, h.WorkerSemanticCoverageReserve, h.WorkerSemanticCoverageSettle, h.WorkerSemanticCoveragePublish,
	} {
		for _, body := range []string{
			"{}", "null", `{"claim_generation":1,"claim_generation":2}`,
			`{"Claim_generation":1}`, `{"claim_generation":1,"nested":{"a":1,"a":2}}`,
			`{"claim_generation":1,"nested":{"a":"\ud800"}}`,
			`{"claim_generation":null}`, `{"claim_generation":1} {}`,
			string([]byte{'{', '"', 0xff, '"', '}'}), strings.Repeat(" ", (4<<20)+1),
		} {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			route := chi.NewRouteContext()
			route.URLParams.Add("id", uuid.NewString())
			req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New()}))
			rec := httptest.NewRecorder()
			call(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%.80s", rec.Code, body)
			}
		}
	}
}

func TestSemanticWorkerStatusGeneration(t *testing.T) {
	h := &Handler{}
	for _, query := range []string{"", "?claim_generation=0", "?claim_generation=-1", "?claim_generation=x", "?claim_generation=1&claim_generation=2", "?claim_generation=1&extra=x"} {
		req := httptest.NewRequest(http.MethodGet, "/"+query, nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", uuid.NewString())
		req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New()}))
		rec := httptest.NewRecorder()
		h.WorkerSemanticCoverageStatus(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%s status=%d", query, rec.Code)
		}
	}
}
