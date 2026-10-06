package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestSetCrossCheckRejectsMissingSessionAndInvalidBodies(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		name    string
		body    string
		session bool
		want    int
	}{
		{"missing session", `{"plan":false}`, false, http.StatusUnauthorized},
		{"unknown field", `{"plan":false,"user_id":"other"}`, true, http.StatusBadRequest},
		{"wrong type", `{"plan":"yes"}`, true, http.StatusBadRequest},
		{"trailing value", `{"plan":false} {"plan":true}`, true, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPut, "/api/me/cross-check", strings.NewReader(tc.body))
			if tc.session {
				r = r.WithContext(mw.ContextWithUser(r.Context(), store.User{ID: uuid.New()}))
			}
			w := httptest.NewRecorder()
			h.SetCrossCheck(w, r)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
