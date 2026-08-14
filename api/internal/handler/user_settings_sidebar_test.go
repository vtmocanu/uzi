package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSidebarDB extends fakeSettingsDB with a canned ListUserSecretsForKind
// answer, so the sidebar_token_ids PUT arm (validate -> ownership-filter ->
// store) runs end to end without a real database. Only the secrets listing
// goes through Query; everything else stays fakeSettingsDB's QueryRow.
type fakeSidebarDB struct {
	fakeSettingsDB
	secrets []store.ListUserSecretsForKindRow
}

func (f *fakeSidebarDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM user_secrets") {
		return &fakeSecretRows{rows: f.secrets}, nil
	}
	return nil, nil
}

// fakeSecretRows walks the canned listing. Only the methods the generated
// scanner touches do real work; the rest satisfy pgx.Rows.
type fakeSecretRows struct {
	rows []store.ListUserSecretsForKindRow
	i    int
}

func (r *fakeSecretRows) Next() bool { r.i++; return r.i <= len(r.rows) }
func (r *fakeSecretRows) Scan(dest ...any) error {
	row := r.rows[r.i-1]
	if p, ok := dest[0].(*uuid.UUID); ok {
		*p = row.ID
	}
	if p, ok := dest[1].(*string); ok {
		*p = row.Kind
	}
	if p, ok := dest[2].(*string); ok {
		*p = row.Label
	}
	if p, ok := dest[3].(*bool); ok {
		*p = row.IsDefault
	}
	if p, ok := dest[4].(*bool); ok {
		*p = row.AutoEligible
	}
	return nil
}
func (r *fakeSecretRows) Close()                                       {}
func (r *fakeSecretRows) Err() error                                   { return nil }
func (r *fakeSecretRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeSecretRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeSecretRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeSecretRows) RawValues() [][]byte                          { return nil }
func (r *fakeSecretRows) Conn() *pgx.Conn                              { return nil }

func secretRow(id uuid.UUID, label string, isDefault bool) store.ListUserSecretsForKindRow {
	return store.ListUserSecretsForKindRow{ID: id, Kind: "anthropic_token", Label: label, IsDefault: isDefault}
}

func decodeSidebarIds(t *testing.T, body []byte) []string {
	t.Helper()
	var resp struct {
		Settings struct {
			SidebarTokenIds []string `json:"sidebar_token_ids"`
		} `json:"settings"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode settings response %s: %v", body, err)
	}
	return resp.Settings.SidebarTokenIds
}

func putSidebar(t *testing.T, db *fakeSidebarDB, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &Handler{q: store.New(db)}
	rec := httptest.NewRecorder()
	req := authed(httptest.NewRequest(http.MethodPut, "/api/me/settings", strings.NewReader(body)))
	h.PutMySettings(rec, req)
	return rec
}

// The ownership filter is the security property of this arm: a foreign id must
// not be storable, and it is DROPPED rather than rejected (a token deleted from
// another tab must not fail the whole save). The default token's id drops too —
// the stored list holds only non-default extras — and duplicates collapse.
func TestPutMySettingsSidebarTokensFiltersToOwnedNonDefault(t *testing.T) {
	deflt, extra, foreign := uuid.New(), uuid.New(), uuid.New()
	db := &fakeSidebarDB{secrets: []store.ListUserSecretsForKindRow{
		secretRow(deflt, "default", true),
		secretRow(extra, "console-key", false),
	}}
	body := `{"sidebar_token_ids":["` + extra.String() + `","` + foreign.String() + `","` +
		deflt.String() + `","` + extra.String() + `"]}`
	rec := putSidebar(t, db, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(db.sidebarIDs) != 1 || db.sidebarIDs[0] != extra {
		t.Fatalf("stored ids = %v, want exactly [%s]", db.sidebarIDs, extra)
	}
	if got := decodeSidebarIds(t, rec.Body.Bytes()); len(got) != 1 || got[0] != extra.String() {
		t.Fatalf("response sidebar_token_ids = %v, want [%s]", got, extra)
	}
}

func TestPutMySettingsSidebarTokensRejectsNonUUID(t *testing.T) {
	db := &fakeSidebarDB{secrets: []store.ListUserSecretsForKindRow{}}
	rec := putSidebar(t, db, `{"sidebar_token_ids":["not-a-uuid"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutMySettingsSidebarTokensRejectsNonArray(t *testing.T) {
	db := &fakeSidebarDB{}
	rec := putSidebar(t, db, `{"sidebar_token_ids":"abc"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

// null and [] both clear back to default-only, without touching the secrets
// listing at all (the empty set needs no ownership check).
func TestPutMySettingsSidebarTokensClears(t *testing.T) {
	for _, body := range []string{`{"sidebar_token_ids":null}`, `{"sidebar_token_ids":[]}`} {
		db := &fakeSidebarDB{}
		db.sidebarIDs = []uuid.UUID{uuid.New()} // pre-existing choice
		rec := putSidebar(t, db, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body=%s", body, rec.Code, rec.Body.String())
		}
		if len(db.sidebarIDs) != 0 {
			t.Fatalf("%s: stored ids = %v, want cleared", body, db.sidebarIDs)
		}
	}
}

// PATCH-like semantics extend to the new field: a theme-only PUT leaves the
// stored sidebar set untouched and still echoes it in the response.
func TestPutMySettingsThemeOnlyLeavesSidebarTokensUntouched(t *testing.T) {
	kept := uuid.New()
	db := &fakeSidebarDB{}
	db.sidebarIDs = []uuid.UUID{kept}
	rec := putSidebar(t, db, `{"theme":"mission"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(db.sidebarIDs) != 1 || db.sidebarIDs[0] != kept {
		t.Fatalf("stored ids = %v, want the untouched [%s]", db.sidebarIDs, kept)
	}
	if got := decodeSidebarIds(t, rec.Body.Bytes()); len(got) != 1 || got[0] != kept.String() {
		t.Fatalf("response sidebar_token_ids = %v, want [%s]", got, kept)
	}
}
