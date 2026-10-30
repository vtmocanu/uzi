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
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestForgeOwnershipNullHold(t *testing.T) {
	for _, status := range []string{"running", "recovery_wait"} {
		t.Run(status, func(t *testing.T) {
			id := uuid.New()
			h := newProtocolHandler(t, &protocolStore{ownedRun: store.Run{ID: id, Status: status, ClaimGeneration: 1}})
			rec := httptest.NewRecorder()
			h.WorkerRunOwnership(rec, workerReq(http.MethodGet, "", id))
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("ownership=%d %s, %v", rec.Code, rec.Body.String(), err)
			}
			retry, present := body["recovery_retry_not_before"]
			if present != (status == "recovery_wait") || (present && string(retry) != "null") {
				t.Fatalf("NULL hold vs absent stamp: %s", rec.Body.String())
			}
		})
	}
}

func TestForgeCloneSuccessStrictRequest(t *testing.T) {
	// Rejected before any service/database work; arbitrary URL/time authority is
	// never accepted even on an authenticated endpoint.
	for _, body := range []string{
		`{"url":"https://arbitrary.example"}`,
		`{"cooled_until":"2099-01-01T00:00:00Z"}`,
		`{"claim_generation":1} {}`,
		strings.Repeat(" ", 1025) + `{}`,
		`{"incarnation":"invalid"}`,
	} {
		t.Run(body[:min(len(body), 50)], func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/runs/test/forge-clone-success", strings.NewReader(body))
			route := chi.NewRouteContext()
			route.URLParams.Add("id", uuid.NewString())
			req = req.WithContext(context.WithValue(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New()}), chi.RouteCtxKey, route))
			res := httptest.NewRecorder()
			(&Handler{}).WorkerForgeCloneSuccess(res, req)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
			}
		})
	}
	res := httptest.NewRecorder()
	(&Handler{}).WorkerForgeCloneSuccess(res, httptest.NewRequest(http.MethodPost, "/runs/test/forge-clone-success", strings.NewReader("{}")))
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status=%d", res.Code)
	}
}
