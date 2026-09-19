package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A non-admin member hitting either guardrail-override decision route gets 403 (issue
// #1432): both are mounted under RequireAuth+RequireAdmin (routes_admin.go). The existing
// decision tests call the handlers directly and so bypass that gate; this pins the
// authorization the write group enforces. DB-free — RequireAdmin refuses before the handler
// touches the store.
func TestGuardrailOverrideDecisionRoutesRequireAdmin(t *testing.T) {
	h := &Handler{}
	member := store.User{ID: uuid.New(), Email: "member@e2e", IsAdmin: false}
	cases := []struct {
		name string
		hf   http.HandlerFunc
	}{
		{"approve", h.ApproveGuardrailOverrideRequest},
		{"reject", h.RejectGuardrailOverrideRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/admin/override-requests/x/"+tc.name, nil)
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", uuid.NewString())
			r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), member), chi.RouteCtxKey, rctx))
			w := httptest.NewRecorder()
			mw.RequireAdmin(tc.hf).ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s member status = %d, want 403 (body %s)", tc.name, w.Code, w.Body.String())
			}
		})
	}
}
