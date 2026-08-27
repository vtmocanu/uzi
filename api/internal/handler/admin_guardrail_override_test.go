package handler

import (
	"bytes"
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

// postOverrideReason drives SetRepoGuardrailOverride with the given raw reason and an
// admin actor, WITHOUT a DB: the reason validation runs before any h.q call, so a nil
// store is never dereferenced on the rejection paths. It returns the recorder.
func postOverrideReason(t *testing.T, reason string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{} // nil q: the validation paths below never reach it
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	body, _ := json.Marshal(setGuardrailOverrideRequest{Reason: reason})
	r := httptest.NewRequest(http.MethodPost, "/admin/repos/x/guardrail-override", bytes.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	r = r.WithContext(context.WithValue(mw.ContextWithUser(r.Context(), admin), chi.RouteCtxKey, rctx))
	w := httptest.NewRecorder()
	h.SetRepoGuardrailOverride(w, r)
	return w
}

// TestGuardrailOverrideReasonValidation pins the write-side backstop (PRD #66 M8
// audit hardening): the admin's reason is rejected BEFORE it is stored when it is
// empty, over-long, or carries a control character — so a forged audit line cannot be
// smuggled into M9's badge / admin list / a plain-text CLI or log sink. All three
// rejections happen before the DB write (a nil store would panic otherwise).
func TestGuardrailOverrideReasonValidation(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   int
	}{
		{"empty", "", http.StatusBadRequest},
		{"whitespace only", "   \t  ", http.StatusBadRequest}, // TrimSpace → empty
		{"newline (forged audit line)", "looks fine\nActor: someone-else approved", http.StatusBadRequest},
		{"ansi escape", "reason\x1b[31mred", http.StatusBadRequest},
		{"carriage return", "a\rb", http.StatusBadRequest},
		{"too long", strings.Repeat("x", maxGuardrailOverrideReasonBytes+1), http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := postOverrideReason(t, tc.reason)
			if w.Code != tc.want {
				t.Fatalf("reason %q → status %d, want %d", tc.reason, w.Code, tc.want)
			}
		})
	}
}
