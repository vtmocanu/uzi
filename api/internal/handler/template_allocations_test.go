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

// getTemplateAllocationsRec drives GetTemplateAllocations (GET) as actor.
func getTemplateAllocationsRec(t *testing.T, db store.DBTX, actor store.User) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodGet, "/api/template-allocations", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.GetTemplateAllocations(rec, req)
	return rec
}

// TestGetTemplateAllocationsPassesCallerIdentity pins the pass-through into
// ListTemplateAllocationsForViewer at loadTemplateAllocations
// (template_allocations.go:239). Mutating `IsAdmin: actor.IsAdmin` to `IsAdmin: true`
// makes every caller's allocation view read as admin — leaking private templates into
// the list — and it survived a full-scope `go test ./...` run.
//
// NOTE the positional order: ListTemplateAllocationsForViewer binds
// (viewer_id, is_admin), the REVERSE of the two other list queries, so viewerQueryArgs
// is called with isAdminIdx=1, viewerIdx=0. Both flag directions are asserted; the admin
// `== true` check is the backstop no zero value can satisfy.
func TestGetTemplateAllocationsPassesCallerIdentity(t *testing.T) {
	t.Run("non-admin caller", func(t *testing.T) {
		actor := nonAdminCaller()
		db := &fakeViewerListDB{}
		if rec := getTemplateAllocationsRec(t, db, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		isAdmin, viewer := viewerQueryArgs(t, db.gotArgs, db.called, 1, 0)
		if isAdmin {
			t.Error("ListTemplateAllocationsForViewer received is_admin=true for a NON-ADMIN caller: " +
				"every caller's allocation view reads as admin and private templates leak in")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})

	t.Run("admin caller", func(t *testing.T) {
		actor := adminCaller()
		db := &fakeViewerListDB{}
		if rec := getTemplateAllocationsRec(t, db, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		isAdmin, viewer := viewerQueryArgs(t, db.gotArgs, db.called, 1, 0)
		if !isAdmin {
			t.Error("ListTemplateAllocationsForViewer received is_admin=false for an ADMIN caller: " +
				"admins lose the cross-scope read the flag exists to grant")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})
}

// setTemplateAllocationsRec drives SetTemplateAllocations (POST) with body as actor.
func setTemplateAllocationsRec(t *testing.T, db store.DBTX, actor store.User, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodPost, "/api/template-allocations", strings.NewReader(body))
	req = req.WithContext(mw.ContextWithUser(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.SetTemplateAllocations(rec, req)
	return rec
}

// TestSetTemplateAllocationsOverridePassesCallerIdentity pins the caller-identity
// pass-through into GetAgentTemplateForViewer at templateForViewer
// (template_allocations.go:222), reached through SetTemplateAllocations →
// resolveOverrides for each my_overrides entry. Mutating `IsAdmin: actor.IsAdmin` to
// `IsAdmin: true` lets a caller reference any private template as an overlay — it
// survived a full-scope `go test ./...` run.
//
// my_overrides is any authenticated user's own overlay (not admin-only, unlike
// global_default_ids), so both a non-admin and an admin drive resolveOverrides. The
// referenced template is left not-visible (QueryRow returns ErrNoRows), so
// resolveOverrides returns an error and the handler answers 400 BEFORE it reaches
// h.pool — the arg capture happens at the QueryRow call, before that error. Both flag
// directions are asserted; the admin `== true` check is the backstop.
func TestSetTemplateAllocationsOverridePassesCallerIdentity(t *testing.T) {
	body := func(id uuid.UUID) string {
		return `{"my_overrides":[{"template_id":"` + id.String() + `","enabled":true}]}`
	}

	t.Run("non-admin caller", func(t *testing.T) {
		actor := nonAdminCaller()
		tid := uuid.New()
		db := &fakeTemplateViewerDB{} // tmpl nil -> ErrNoRows -> 400, no pool touched
		setTemplateAllocationsRec(t, db, actor, body(tid))
		gotID, isAdmin, viewer := templateViewerArgs(t, db)
		if isAdmin {
			t.Error("templateForViewer received is_admin=true for a NON-ADMIN caller: " +
				"any private template becomes referenceable as an overlay by anyone")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
		if gotID != tid {
			t.Errorf("id = %v, want the body template id %v", gotID, tid)
		}
	})

	t.Run("admin caller", func(t *testing.T) {
		actor := adminCaller()
		tid := uuid.New()
		db := &fakeTemplateViewerDB{}
		setTemplateAllocationsRec(t, db, actor, body(tid))
		_, isAdmin, viewer := templateViewerArgs(t, db)
		if !isAdmin {
			t.Error("templateForViewer received is_admin=false for an ADMIN caller: " +
				"admins lose the cross-scope visibility the flag exists to grant")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})
}
