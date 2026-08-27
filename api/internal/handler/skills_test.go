package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
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
		// #166: a unicode.Cf format char (zero-width joiner / bidi override)
		// crosses a principal boundary in the SkillAllocationPanel tooltip for
		// builtin/global skills and must be rejected. \u escapes keep the test
		// source itself free of zero-width/bidi runes.
		"zwj (Cf) in description":      {Description: "legit\u200dname", Body: "b\n"},
		"bidi override in description": {Description: "legit\u202ename", Body: "b\n"},
		"empty body":                   {Description: "d.", Body: "   "},
		"oversized body":               {Description: "d.", Body: strings.Repeat("x", maxBytes+1)},
		"full token in body":           {Description: "d.", Body: "key " + "sk-ant-api03-" + strings.Repeat("A", 80) + "\n"},
		"full token in descr":          {Description: "sk-ant-api03-" + strings.Repeat("A", 80), Body: "b\n"},
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
//
// It also CAPTURES the query arguments. That is not incidental: a fake that decides its
// outcome purely from its own fixture is structurally incapable of observing WHICH
// params the handler built, so it cannot see an authorization bypass in the params
// (see TestGetSkillPassesCallerIdentity). Keep the capture.
type fakeSkillDB struct {
	skill *store.Skill

	// gotArgs is the raw positional argument list from the last QueryRow.
	// GetSkillForViewer binds them as (arg.ID, arg.IsAdmin, arg.ViewerID) —
	// store/skills.sql.go. Held untyped on purpose; see skillQueryArgs.
	gotArgs []any
	called  bool
}

func (*fakeSkillDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (*fakeSkillDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeSkillDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	f.called = true
	f.gotArgs = args
	if f.skill == nil {
		return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
	}
	s := *f.skill
	// GetSkillForViewer scans all nine columns in this order: id, name,
	// description, body, scope, user_id, updated_by, created_at, updated_at
	// (store/skills.sql.go). Scatter EVERY column from the fixture so the full DTO
	// round-trips — a fake that filled only id/name/scope would let a dropped field
	// (e.g. body) sail past TestGetSkillDTOAllFields. The identity/404 callers set
	// only id/name/scope on their fixtures, so the extra fields carry zero values
	// there and change nothing for them.
	return fakeScanRow{func(dest ...any) error {
		if p, ok := dest[0].(*uuid.UUID); ok {
			*p = s.ID
		}
		if p, ok := dest[1].(*string); ok {
			*p = s.Name
		}
		if p, ok := dest[2].(*string); ok {
			*p = s.Description
		}
		if p, ok := dest[3].(*string); ok {
			*p = s.Body
		}
		if p, ok := dest[4].(*string); ok {
			*p = s.Scope
		}
		if p, ok := dest[5].(*pgtype.UUID); ok {
			*p = s.UserID
		}
		if p, ok := dest[6].(*pgtype.UUID); ok {
			*p = s.UpdatedBy
		}
		if p, ok := dest[7].(*pgtype.Timestamptz); ok {
			*p = s.CreatedAt
		}
		if p, ok := dest[8].(*pgtype.Timestamptz); ok {
			*p = s.UpdatedAt
		}
		return nil
	}}
}

// nonAdminCaller / adminCaller build callers whose IsAdmin is set DELIBERATELY rather
// than left to the zero value. An authz assertion resting on a zero value is one
// struct-literal edit away from passing for the wrong reason.
func nonAdminCaller() store.User {
	return store.User{ID: uuid.New(), IsActive: true, IsAdmin: false}
}
func adminCaller() store.User {
	return store.User{ID: uuid.New(), IsActive: true, IsAdmin: true}
}

// getSkillRec drives GetSkill for one id as actor.
func getSkillRec(t *testing.T, db store.DBTX, id uuid.UUID, actor store.User) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodGet, "/api/skills/"+id.String(), nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id.String())
	req = req.WithContext(context.WithValue(
		mw.ContextWithUser(req.Context(), actor), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.GetSkill(rec, req)
	return rec
}

// skillQueryArgs unpacks the positional args the handler bound into
// GetSkillForViewerParams, failing loudly if the shape is not what the generated query
// sends. Deliberately not written as `id, _ := args[0].(uuid.UUID)`: on a type
// mismatch that form yields a zero value, and a zero value would happily satisfy an
// "IsAdmin must be false" assertion against a param that was never actually captured.
//
// That is not a hypothetical, and the two hardenings here are COMPLEMENTARY — neither
// alone covers the non-admin side. Measured by restoring the comma-ok form with a type
// mismatch in place:
//
//	--- PASS: TestGetSkillPassesCallerIdentity/non-admin_caller   <-- vacuous
//	--- FAIL: TestGetSkillPassesCallerIdentity/admin_caller
//
// The t.Fatalf guards below are what keep `non-admin_caller` honest; without them it is
// a zero-value tautology that observes nothing and still goes green. The `admin_caller`
// subtest is the BACKSTOP: it demands is_admin == true, which no zero value can ever
// satisfy, so it is the only assertion that still fails if this helper is ever
// "simplified" back to comma-ok. Weaken either one and the other is load-bearing;
// weaken both and the authz assertion silently stops asserting.
func skillQueryArgs(t *testing.T, db *fakeSkillDB) (uuid.UUID, bool, pgtype.UUID) {
	t.Helper()
	if !db.called {
		t.Fatal("GetSkillForViewer was never queried")
	}
	if len(db.gotArgs) != 3 {
		t.Fatalf("query got %d args, want 3 (id, is_admin, viewer_id): %#v", len(db.gotArgs), db.gotArgs)
	}
	id, ok := db.gotArgs[0].(uuid.UUID)
	if !ok {
		t.Fatalf("arg 0 (id) = %#v, want uuid.UUID", db.gotArgs[0])
	}
	isAdmin, ok := db.gotArgs[1].(bool)
	if !ok {
		t.Fatalf("arg 1 (is_admin) = %#v, want bool", db.gotArgs[1])
	}
	viewer, ok := db.gotArgs[2].(pgtype.UUID)
	if !ok {
		t.Fatalf("arg 2 (viewer_id) = %#v, want pgtype.UUID", db.gotArgs[2])
	}
	return id, isAdmin, viewer
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
		rec := getSkillRec(t, &fakeSkillDB{}, uuid.New(), nonAdminCaller())
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("store returns a skill -> 200", func(t *testing.T) {
		want := store.Skill{ID: uuid.New(), Name: "visible-skill", Scope: "global"}
		rec := getSkillRec(t, &fakeSkillDB{skill: &want}, want.ID, nonAdminCaller())
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

// TestGetSkillDTOAllFields pins the found -> 200 body on ALL NINE skillDTO fields
// (skills.go:24), not just the id/name/scope the existing found subtest checks.
// body is the actual payload of a skills API, and user_id/updated_by/timestamps are
// the audit trail; dropping any of them from skillToDTO would go unnoticed with a
// three-field assertion. Every expected value is deliberately NON-ZERO so no
// assertion is vacuously satisfiable (PRD #97 rule 4): user_id/updated_by are valid
// pgtype.UUIDs (so the DTO's *string pointers are non-nil) and the two timestamps are
// valid and distinct. Proven to bite by commenting out `Body: s.Body` (and, spot-
// checked, `UserID`/`CreatedAt`) in skillToDTO.
func TestGetSkillDTOAllFields(t *testing.T) {
	userID := uuid.New()
	updatedBy := uuid.New()
	created := time.Date(2023, 1, 2, 3, 4, 5, 0, time.UTC)
	updated := time.Date(2024, 6, 7, 8, 9, 10, 0, time.UTC)

	want := store.Skill{
		ID:          uuid.New(),
		Name:        "deploy-runbook",
		Description: "how to deploy the thing",
		Body:        "# Playbook\n\nstep 1: do it\n",
		Scope:       "user",
		UserID:      pgUUID(userID),
		UpdatedBy:   pgUUID(updatedBy),
		CreatedAt:   pgtype.Timestamptz{Time: created, Valid: true},
		UpdatedAt:   pgtype.Timestamptz{Time: updated, Valid: true},
	}

	rec := getSkillRec(t, &fakeSkillDB{skill: &want}, want.ID, adminCaller())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var out struct {
		Skill struct {
			ID          string    `json:"id"`
			Name        string    `json:"name"`
			Description string    `json:"description"`
			Body        string    `json:"body"`
			Scope       string    `json:"scope"`
			UserID      *string   `json:"user_id"`
			UpdatedBy   *string   `json:"updated_by"`
			CreatedAt   time.Time `json:"created_at"`
			UpdatedAt   time.Time `json:"updated_at"`
		} `json:"skill"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := out.Skill

	if got.ID != want.ID.String() {
		t.Errorf("id = %q, want %q", got.ID, want.ID)
	}
	if got.Name != want.Name {
		t.Errorf("name = %q, want %q", got.Name, want.Name)
	}
	if got.Description != want.Description {
		t.Errorf("description = %q, want %q", got.Description, want.Description)
	}
	if got.Body != want.Body {
		t.Errorf("body = %q, want %q", got.Body, want.Body)
	}
	if got.Scope != want.Scope {
		t.Errorf("scope = %q, want %q", got.Scope, want.Scope)
	}
	if got.UserID == nil || *got.UserID != userID.String() {
		t.Errorf("user_id = %v, want %q", got.UserID, userID)
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != updatedBy.String() {
		t.Errorf("updated_by = %v, want %q", got.UpdatedBy, updatedBy)
	}
	if !got.CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got.CreatedAt, created)
	}
	if !got.UpdatedAt.Equal(updated) {
		t.Errorf("updated_at = %v, want %v", got.UpdatedAt, updated)
	}
}

// fakeErrSkillDB is a store.DBTX whose QueryRow scan reports a NON-ErrNoRows error,
// standing in for a DB fault (dropped connection, query error) that GetSkill must map
// to 500. The 404 branch is reserved for pgx.ErrNoRows alone (skills.go:198-205); a
// generic error collapsing to 404 would be an existence-oracle lie in the opposite
// direction — "does not exist" when the truth is "the DB fell over".
type fakeErrSkillDB struct{}

func (*fakeErrSkillDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (*fakeErrSkillDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (*fakeErrSkillDB) QueryRow(context.Context, string, ...any) pgx.Row {
	// A wrapped error that is explicitly NOT pgx.ErrNoRows.
	return fakeScanRow{func(...any) error { return errors.New("boom: connection reset") }}
}

// TestGetSkillGenericErrorIs500 pins that a non-ErrNoRows DB error stays 500 and never
// collapses to the 404 reserved for a genuinely absent row. Proven to bite by
// transiently mapping all errors to 404 in GetSkill's error branch.
func TestGetSkillGenericErrorIs500(t *testing.T) {
	rec := getSkillRec(t, &fakeErrSkillDB{}, uuid.New(), nonAdminCaller())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (a DB fault must not read as 404 not-found); body=%s",
			rec.Code, rec.Body.String())
	}
}

// TestGetSkillAuthAndParamGuards pins the two defence-in-depth edges behind RequireAuth
// and chi routing: no user in context -> 401, an unparseable {id} -> 400. Terse on
// purpose; the middleware/router normally never let these reach the handler.
func TestGetSkillAuthAndParamGuards(t *testing.T) {
	t.Run("no user in context -> 401", func(t *testing.T) {
		id := uuid.New()
		h := &Handler{q: store.New(&fakeSkillDB{})}
		req := httptest.NewRequest(http.MethodGet, "/api/skills/"+id.String(), nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id.String())
		// Deliberately NO mw.ContextWithUser.
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.GetSkill(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("unparseable id -> 400", func(t *testing.T) {
		h := &Handler{q: store.New(&fakeSkillDB{})}
		req := httptest.NewRequest(http.MethodGet, "/api/skills/not-a-uuid", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", "not-a-uuid")
		req = req.WithContext(context.WithValue(
			mw.ContextWithUser(req.Context(), nonAdminCaller()), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.GetSkill(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	})
}

// emptyViewerRows is an empty-but-valid pgx.Rows: Next() is immediately false, so a
// list handler walks zero rows and reaches a clean 200. The arg capture that the
// *ForViewer pass-through tests rely on happens at the Query call, before any row is
// walked, so an empty result set is all these tests need. Shared by every list-site
// guard (ListSkills, ListAgentTemplates, ListTemplateAllocations) and by the
// GetTemplateSkills allocation listing.
type emptyViewerRows struct{}

func (*emptyViewerRows) Next() bool                    { return false }
func (*emptyViewerRows) Scan(...any) error             { return nil }
func (*emptyViewerRows) Close()                        {}
func (*emptyViewerRows) Err() error                    { return nil }
func (*emptyViewerRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (*emptyViewerRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (*emptyViewerRows) Values() ([]any, error) { return nil, nil }
func (*emptyViewerRows) RawValues() [][]byte    { return nil }
func (*emptyViewerRows) Conn() *pgx.Conn        { return nil }

// fakeViewerListDB is a store.DBTX for the list-endpoint *ForViewer queries. Like
// fakeSkillDB it CAPTURES the raw positional args of the Query call — a fake that
// answered purely from its own fixture could not observe WHICH params the handler
// built, so it could not see the IsAdmin/ViewerID pass-through bug the guards pin.
// It returns an empty-but-valid result so the handler reaches 200.
type fakeViewerListDB struct {
	gotArgs []any
	called  bool
}

func (*fakeViewerListDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeViewerListDB) Query(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
	f.called = true
	f.gotArgs = args
	return &emptyViewerRows{}, nil
}
func (*fakeViewerListDB) QueryRow(context.Context, string, ...any) pgx.Row {
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

// viewerQueryArgs unpacks the (is_admin, viewer_id) pair from a captured list-query
// arg slice. The two *ForViewer list queries do not agree on positional order —
// ListSkillsForViewer/ListAgentTemplatesForViewer bind (is_admin, viewer_id) while
// ListTemplateAllocationsForViewer binds (viewer_id, is_admin) — so the caller passes
// the indices. The t.Fatalf type guards (not comma-ok into a zero value) are what keep
// the non-admin assertion honest: a mismatched capture must fail loudly, never yield a
// false that vacuously satisfies "is_admin must be false". See skillQueryArgs.
func viewerQueryArgs(t *testing.T, args []any, called bool, isAdminIdx, viewerIdx int) (bool, pgtype.UUID) {
	t.Helper()
	if !called {
		t.Fatal("the *ForViewer list query was never executed")
	}
	if len(args) != 2 {
		t.Fatalf("query got %d args, want 2 (is_admin, viewer_id in some order): %#v", len(args), args)
	}
	isAdmin, ok := args[isAdminIdx].(bool)
	if !ok {
		t.Fatalf("arg %d (is_admin) = %#v, want bool", isAdminIdx, args[isAdminIdx])
	}
	viewer, ok := args[viewerIdx].(pgtype.UUID)
	if !ok {
		t.Fatalf("arg %d (viewer_id) = %#v, want pgtype.UUID", viewerIdx, args[viewerIdx])
	}
	return isAdmin, viewer
}

// listSkillsRec drives ListSkills (GET, no path param) as actor.
func listSkillsRec(t *testing.T, db store.DBTX, actor store.User) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	req := httptest.NewRequest(http.MethodGet, "/api/skills", nil)
	req = req.WithContext(mw.ContextWithUser(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.ListSkills(rec, req)
	return rec
}

// TestListSkillsPassesCallerIdentity is the list-side twin of
// TestGetSkillPassesCallerIdentity. ListSkills builds ListSkillsForViewerParams from
// the caller (skills.go:164); nothing pinned that the caller's real identity is passed
// through. Mutating `IsAdmin: actor.IsAdmin` to `IsAdmin: true` is a total bypass —
// every caller lists as admin, so any authenticated user sees every private skill — and
// it left the whole api suite green (confirmed by a full-scope `go test ./...` run).
// Both directions of the flag are asserted: the admin `== true` check is the backstop no
// zero value can satisfy, and the non-admin check guards the mirror mutation
// (hardcoded `false`) that would strip admins of their cross-scope read.
func TestListSkillsPassesCallerIdentity(t *testing.T) {
	t.Run("non-admin caller", func(t *testing.T) {
		actor := nonAdminCaller()
		db := &fakeViewerListDB{}
		if rec := listSkillsRec(t, db, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		isAdmin, viewer := viewerQueryArgs(t, db.gotArgs, db.called, 0, 1)
		if isAdmin {
			t.Error("ListSkillsForViewer received is_admin=true for a NON-ADMIN caller: " +
				"every caller lists as admin and any private skill is listable by anyone")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})

	t.Run("admin caller", func(t *testing.T) {
		actor := adminCaller()
		db := &fakeViewerListDB{}
		if rec := listSkillsRec(t, db, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		isAdmin, viewer := viewerQueryArgs(t, db.gotArgs, db.called, 0, 1)
		if !isAdmin {
			t.Error("ListSkillsForViewer received is_admin=false for an ADMIN caller: " +
				"admins lose the cross-scope read the flag exists to grant")
		}
		if !viewer.Valid || uuid.UUID(viewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(viewer.Bytes), viewer.Valid, actor.ID)
		}
	})
}

// TestGetSkillPassesCallerIdentity closes the OTHER half of the issue-#16 seam. The
// 404 mapping is only a denial if the visibility query was asked the right question in
// the first place, and nothing in the tree pinned that: TestSkillsVisibilityLiveDB
// calls GetSkillForViewer with explicit literals (IsAdmin: false, ViewerID: userB), so
// it proves the SQL honours the params it is handed and says nothing about which params
// GetSkill hands it. Mutating `IsAdmin: actor.IsAdmin` to `IsAdmin: true` at
// skills.go:195 is a total authorization bypass — every caller reads as admin, so any
// authenticated user can fetch any other user's private skill — and it left the whole
// api suite green. So did `ViewerID: pgUUID(uuid.Nil)` at :196.
//
// Both directions of the admin flag are asserted on purpose. Checking only "a
// non-admin's query carries false" would stay green under the mirror mutation
// (hardcoding `IsAdmin: false`), which silently strips admins of their cross-scope
// read. What is being pinned is PASS-THROUGH of the caller's real identity, not either
// constant.
func TestGetSkillPassesCallerIdentity(t *testing.T) {
	t.Run("non-admin caller", func(t *testing.T) {
		actor := nonAdminCaller()
		skill := store.Skill{ID: uuid.New(), Name: "s", Scope: "global"}
		db := &fakeSkillDB{skill: &skill}

		if rec := getSkillRec(t, db, skill.ID, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		gotID, gotIsAdmin, gotViewer := skillQueryArgs(t, db)

		if gotIsAdmin {
			t.Error("the visibility query received is_admin=true for a NON-ADMIN caller: " +
				"every caller reads as admin and any private skill is readable by anyone")
		}
		if !gotViewer.Valid || uuid.UUID(gotViewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v — a wrong or nil "+
				"viewer scopes the visibility predicate to the wrong user",
				uuid.UUID(gotViewer.Bytes), gotViewer.Valid, actor.ID)
		}
		if gotID != skill.ID {
			t.Errorf("id = %v, want the path id %v", gotID, skill.ID)
		}
	})

	t.Run("admin caller", func(t *testing.T) {
		actor := adminCaller()
		skill := store.Skill{ID: uuid.New(), Name: "s", Scope: "global"}
		db := &fakeSkillDB{skill: &skill}

		if rec := getSkillRec(t, db, skill.ID, actor); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		_, gotIsAdmin, gotViewer := skillQueryArgs(t, db)

		if !gotIsAdmin {
			t.Error("the visibility query received is_admin=false for an ADMIN caller: " +
				"admins lose the cross-scope read the flag exists to grant")
		}
		if !gotViewer.Valid || uuid.UUID(gotViewer.Bytes) != actor.ID {
			t.Errorf("viewer_id = %v (valid=%v), want the caller's own id %v",
				uuid.UUID(gotViewer.Bytes), gotViewer.Valid, actor.ID)
		}
	})
}
