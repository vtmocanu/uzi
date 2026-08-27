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

// getTemplateSkillsRec drives GetTemplateSkills (GET /{id}) for one id as actor.
func getTemplateSkillsRec(t *testing.T, db store.DBTX, id uuid.UUID, actor store.User) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodGet, "/api/agent-templates/"+id.String()+"/skills", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	req = req.WithContext(context.WithValue(
		mw.ContextWithUser(req.Context(), actor), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.GetTemplateSkills(rec, req)
	return rec
}

// TestGetTemplateSkillsPassesCallerIdentity pins the caller-identity pass-through into
// GetAgentTemplateForViewer at templateIDForSkills (skill_allocations.go:186), the
// visibility gate GetTemplateSkills applies before returning a template's allocations.
// Mutating `IsAdmin: actor.IsAdmin` to `IsAdmin: true` lets any caller read the
// allocations of a template they may not see — it survived a full-scope
// `go test ./...` run.
//
// The fake scans back a visible template from QueryRow so templateIDForSkills succeeds,
// then returns empty allocation rows from Query so the handler reaches 200. Both flag
// directions are asserted; the admin `== true` check is the backstop no zero value can
// satisfy (see skillQueryArgs / templateViewerArgs).
func TestGetTemplateSkillsPassesCallerIdentity(t *testing.T) {
	t.Run("non-admin caller", func(t *testing.T) {
		actor := nonAdminCaller()
		tmpl := store.AgentTemplate{ID: uuid.New(), Name: "t", Scope: "global"}
		db := &fakeTemplateViewerDB{tmpl: &tmpl}
		if rec := getTemplateSkillsRec(t, db, tmpl.ID, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		gotID, isAdmin, viewer := templateViewerArgs(t, db)
		if isAdmin {
			t.Error("templateIDForSkills received is_admin=true for a NON-ADMIN caller: " +
				"any private template's allocations become readable by anyone")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
		if gotID != tmpl.ID {
			t.Errorf("id = %v, want the path id %v", gotID, tmpl.ID)
		}
	})

	t.Run("admin caller", func(t *testing.T) {
		actor := adminCaller()
		tmpl := store.AgentTemplate{ID: uuid.New(), Name: "t", Scope: "global"}
		db := &fakeTemplateViewerDB{tmpl: &tmpl}
		if rec := getTemplateSkillsRec(t, db, tmpl.ID, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		_, isAdmin, viewer := templateViewerArgs(t, db)
		if !isAdmin {
			t.Error("templateIDForSkills received is_admin=false for an ADMIN caller: " +
				"admins lose the cross-scope visibility the flag exists to grant")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})
}
