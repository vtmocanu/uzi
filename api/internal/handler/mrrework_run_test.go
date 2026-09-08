package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// erroringSettingsStore is a settings.Store whose ListAppSettings always fails, so the
// cache's cold read propagates the error — the fail-closed input for the kill-switch
// read-error case.
type erroringSettingsStore struct{}

func (erroringSettingsStore) ListAppSettings(context.Context) ([]store.AppSetting, error) {
	return nil, errors.New("app_settings unavailable")
}

// reworkReq builds a POST /api/runs/{id}/rework request authed as user with the chi {id}
// param set (the handler is invoked directly, not through the router).
func reworkReq(user store.User, id uuid.UUID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+id.String()+"/rework", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	ctx := context.WithValue(mw.ContextWithUser(req.Context(), user), chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

func aTestUser() store.User {
	return store.User{ID: uuid.New(), Email: "u@uzi.local", IsActive: true}
}

// TestStartRunReworkGuidanceTooLong: the length gate runs before any settings/DB/forge
// touch, so an oversized guidance is a clean 400 on a bare handler.
func TestStartRunReworkGuidanceTooLong(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	body := `{"guidance":"` + strings.Repeat("g", MaxGuidanceBytes+1) + `"}`
	h.StartRunRework(rec, reworkReq(aTestUser(), uuid.New(), body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized guidance = %d, want 400\nbody: %s", rec.Code, rec.Body.String())
	}
}

// TestStartRunReworkKillSwitchOff: an admin kill-switch set OFF refuses with 409 before any
// run/forge read (D2). The handler needs only the settings cache here.
func TestStartRunReworkKillSwitchOff(t *testing.T) {
	h := newSettingsHandler(store.AppSetting{Key: settings.KeyMrReworkEnabled, Value: "false"})
	rec := httptest.NewRecorder()
	h.StartRunRework(rec, reworkReq(aTestUser(), uuid.New(), `{"guidance":"x"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("kill-switch off = %d, want 409\nbody: %s", rec.Code, rec.Body.String())
	}
	if b := rec.Body.String(); !strings.Contains(b, "disabled on this instance") {
		t.Fatalf("kill-switch off body = %s, want the disabled reason", b)
	}
}

// TestStartRunReworkKillSwitchReadError: a settings READ ERROR is fail-closed to 409, exactly
// as the detector maps a settings blip to OFF — a read blip must never fail OPEN into
// starting a run.
func TestStartRunReworkKillSwitchReadError(t *testing.T) {
	h := &Handler{settings: settings.New(erroringSettingsStore{}, time.Minute)}
	rec := httptest.NewRecorder()
	h.StartRunRework(rec, reworkReq(aTestUser(), uuid.New(), `{"guidance":"x"}`))
	if rec.Code != http.StatusConflict {
		t.Fatalf("kill-switch read error = %d, want 409 (fail closed)\nbody: %s", rec.Code, rec.Body.String())
	}
}
