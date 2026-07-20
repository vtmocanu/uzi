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
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// userSkill builds a user-scope skill owned by owner.
func userSkill(owner uuid.UUID) store.Skill {
	return store.Skill{Scope: "user", UserID: pgUUID(owner)}
}

func TestValidateSkillFields(t *testing.T) {
	const maxBytes = 100
	base := skillWriteRequest{Description: "does a thing.", Body: "# Playbook\n\nsteps.\n"}

	f, err := validateSkillFields(base, maxBytes)
	if err != nil {
		t.Fatalf("valid skill rejected: %v", err)
	}
	if f.description != "does a thing." || f.body != base.Body {
		t.Errorf("fields not carried through: %+v", f)
	}

	rejects := map[string]skillWriteRequest{
		"empty description":      {Description: "  ", Body: "b\n"},
		"newline in description": {Description: "legit\ndescription: evil", Body: "b\n"},
		"cr in description":      {Description: "legit\rx", Body: "b\n"},
		"tab in description":     {Description: "a\tb", Body: "b\n"},
		"empty body":             {Description: "d.", Body: "   "},
		"oversized body":         {Description: "d.", Body: strings.Repeat("x", maxBytes+1)},
		"full token in body":     {Description: "d.", Body: "key " + "sk-ant-api03-" + strings.Repeat("A", 80) + "\n"},
		"full token in descr":    {Description: "sk-ant-api03-" + strings.Repeat("A", 80), Body: "b\n"},
	}
	for name, req := range rejects {
		if _, err := validateSkillFields(req, maxBytes); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}

	// A body exactly at the cap is allowed; a description mentioning the token
	// format (not a full token) stays legal.
	if _, err := validateSkillFields(skillWriteRequest{Description: "d.", Body: strings.Repeat("x", maxBytes)}, maxBytes); err != nil {
		t.Errorf("body at the cap should be allowed: %v", err)
	}
	if _, err := validateSkillFields(skillWriteRequest{Description: "tokens start with sk-ant-.", Body: "b\n"}, maxBytes); err != nil {
		t.Errorf("token format mention should be allowed: %v", err)
	}
}

// TestAuthorizeSkillWrite is the core write-authz matrix: builtin/global are
// admin-only, a user skill is owner-only, and a non-owner non-admin sees 404
// (existence hidden) while an admin who cannot edit a user skill sees 403.
func TestAuthorizeSkillWrite(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	other := store.User{ID: uuid.New()}
	admin := store.User{ID: uuid.New(), IsAdmin: true}

	builtin := store.Skill{Scope: "builtin"}
	global := store.Skill{Scope: "global"}
	mine := userSkill(owner.ID)

	cases := []struct {
		name       string
		actor      store.User
		skill      store.Skill
		wantOK     bool
		wantStatus int
	}{
		{"admin edits builtin", admin, builtin, true, 0},
		{"user edits builtin", owner, builtin, false, 403},
		{"admin edits global", admin, global, true, 0},
		{"user edits global", owner, global, false, 403},
		{"owner edits own user skill", owner, mine, true, 0},
		{"admin edits others user skill", admin, mine, false, 403},
		{"other user edits user skill", other, mine, false, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok := authorizeSkillWrite(c.actor, c.skill)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok && status != c.wantStatus {
				t.Errorf("status = %d, want %d", status, c.wantStatus)
			}
		})
	}
}

// TestResetSkillStatus pins the existence-oracle fix: reset applies write authz
// before the builtin-only 400, so another user's private skill returns 404 (same
// as an absent id), never a 400 that would confirm the id exists.
func TestResetSkillStatus(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	other := store.User{ID: uuid.New()}
	admin := store.User{ID: uuid.New(), IsAdmin: true}

	cases := []struct {
		name       string
		actor      store.User
		skill      store.Skill
		wantOK     bool
		wantStatus int
	}{
		{"admin resets builtin", admin, store.Skill{Scope: "builtin"}, true, 0},
		{"non-admin resets builtin", owner, store.Skill{Scope: "builtin"}, false, 403},
		{"admin resets global -> 400 not-builtin", admin, store.Skill{Scope: "global"}, false, 400},
		{"non-admin resets global -> 403", owner, store.Skill{Scope: "global"}, false, 403},
		{"owner resets own user skill -> 400 not-builtin", owner, userSkill(owner.ID), false, 400},
		// The oracle case: another user's private skill returns 404, NOT the 400
		// that would leak its existence.
		{"other resets private user skill -> 404", other, userSkill(owner.ID), false, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, ok := resetSkillStatus(c.actor, c.skill)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok && status != c.wantStatus {
				t.Errorf("status = %d, want %d", status, c.wantStatus)
			}
		})
	}
}

// TestAllocatableRules pins which skills may be allocated as a shared row vs a
// user's own overlay. Shared: builtin/global only. Mine: builtin/global or the
// caller's own user skill — never another user's private skill.
func TestAllocatableRules(t *testing.T) {
	actor := store.User{ID: uuid.New()}
	otherID := uuid.New()

	builtin := store.Skill{Scope: "builtin"}
	global := store.Skill{Scope: "global"}
	mine := userSkill(actor.ID)
	theirs := userSkill(otherID)

	if !allocatableAsShared(builtin) || !allocatableAsShared(global) {
		t.Error("builtin/global must be shared-allocatable")
	}
	if allocatableAsShared(mine) || allocatableAsShared(theirs) {
		t.Error("user skills must not be shared-allocatable")
	}

	if !allocatableAsMine(builtin, actor) || !allocatableAsMine(global, actor) {
		t.Error("builtin/global must be mine-allocatable")
	}
	if !allocatableAsMine(mine, actor) {
		t.Error("own user skill must be mine-allocatable")
	}
	if allocatableAsMine(theirs, actor) {
		t.Error("another user's private skill must NOT be mine-allocatable")
	}
}

// fakeSkillDB is a store.DBTX standing in for GetSkillForViewer's two outcomes: it
// either scans back one skill row, or reports pgx.ErrNoRows.
type fakeSkillDB struct{ skill *store.Skill }

func (fakeSkillDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeSkillDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f fakeSkillDB) QueryRow(context.Context, string, ...any) pgx.Row {
	if f.skill == nil {
		return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
	}
	s := *f.skill
	// GetSkillForViewer scans id, name, description, body, scope, user_id,
	// updated_by, created_at, updated_at — the first five are all the DTO assertions
	// below need; the rest keep their zero values.
	return fakeScanRow{func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = s.ID
		}
		if p, ok := dest[1].(*string); ok {
			*p = s.Name
		}
		if p, ok := dest[4].(*string); ok {
			*p = s.Scope
		}
		return nil
	}}
}

// getSkillRec drives GetSkill for one id with an authenticated non-admin caller.
func getSkillRec(t *testing.T, db store.DBTX, id uuid.UUID) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodGet, "/api/skills/"+id.String(), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	req = req.WithContext(context.WithValue(
		mw.ContextWithUser(req.Context(), store.User{ID: uuid.New(), IsActive: true}),
		chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.GetSkill(rec, req)
	return rec
}

// TestGetSkillNotFoundVsFound joins the halves of the "another user's private skill is
// indistinguishable from an absent one" property (PRD #97 M9b, issue #16). The store
// half is proven against live PG by TestSkillsVisibilityLiveDB (its "single-get
// visibility" block): a non-owner non-admin gets pgx.ErrNoRows. The handler half — that ErrNoRows becomes a
// 404 and not a masked 500 — had no test anywhere until this one; the e2e leg that
// used to span both was dropped. A 500 here would still deny the read, but it would
// leak that the id resolves to something, which is the whole point of the 404.
func TestGetSkillNotFoundVsFound(t *testing.T) {
	t.Run("store says ErrNoRows -> 404", func(t *testing.T) {
		rec := getSkillRec(t, fakeSkillDB{}, uuid.New())
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("store returns a skill -> 200", func(t *testing.T) {
		want := store.Skill{ID: uuid.New(), Name: "visible-skill", Scope: "global"}
		rec := getSkillRec(t, fakeSkillDB{skill: &want}, want.ID)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Skill struct {
				ID    string `json:"id"`
				Name  string `json:"name"`
				Scope string `json:"scope"`
			} `json:"skill"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if out.Skill.ID != want.ID.String() || out.Skill.Name != want.Name || out.Skill.Scope != want.Scope {
			t.Errorf("skill = %+v, want id=%s name=%s scope=%s", out.Skill, want.ID, want.Name, want.Scope)
		}
	})
}
