package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// toolGateDB is a store.DBTX that answers the three queries the two HTTP tool gates
// touch — ListToolAllowlist (Query), CreateToolAllowlistEntry and
// UpsertRepoToolProfileForOwner (both QueryRow) — from memory, capturing whether a
// write was reached so a test can assert the gate ran BEFORE any DB write. Every
// other query panics via the nil-return path (pgx.ErrNoRows), so an unexpected call
// surfaces rather than silently passing. The PRD #123 M3 coverage gates
// (CreateToolAllowlistEntry seed gate, SetRepoToolProfile profile gate) both fire
// before their respective write, so a 400 for the coverage reason leaves
// createCalled / upsertCalled false.
type toolGateDB struct {
	allowlist []store.ToolAllowlist // rows ListToolAllowlist returns

	createCalled bool
	createdName  string

	upsertCalled   bool
	upsertPackages []byte
}

func (d *toolGateDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (d *toolGateDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM tool_allowlist") {
		return &toolAllowlistRows{rows: d.allowlist}, nil
	}
	return nil, pgx.ErrNoRows
}

func (d *toolGateDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO tool_allowlist"):
		d.createCalled = true
		name, _ := args[0].(string)
		d.createdName = name
		return fakeScanRow{scanToolAllowlist(store.ToolAllowlist{ID: uuid.New(), Name: name})}
	case strings.Contains(sql, "INTO repo_tool_profiles"):
		d.upsertCalled = true
		// args are (UserID, Packages, RepoID) per UpsertRepoToolProfileForOwner.
		if pkgs, ok := args[1].([]byte); ok {
			d.upsertPackages = pkgs
		}
		return fakeScanRow{scanRepoToolProfile(store.RepoToolProfile{ID: uuid.New(), Packages: d.upsertPackages})}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

// scanToolAllowlist fills the 7 RETURNING columns of a tool_allowlist row in the
// order CreateToolAllowlistEntry scans them.
func scanToolAllowlist(e store.ToolAllowlist) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = e.ID
		*dest[1].(*string) = e.Name
		*dest[2].(*pgtype.Text) = e.PinnedVersion
		*dest[3].(*pgtype.Text) = e.Note
		*dest[4].(*pgtype.UUID) = e.UpdatedBy
		*dest[5].(*pgtype.Timestamptz) = e.CreatedAt
		*dest[6].(*pgtype.Timestamptz) = e.UpdatedAt
		return nil
	}
}

// scanRepoToolProfile fills the 6 RETURNING columns of a repo_tool_profiles row in
// the order UpsertRepoToolProfileForOwner scans them.
func scanRepoToolProfile(p store.RepoToolProfile) func(dest ...any) error {
	return func(dest ...any) error {
		*dest[0].(*uuid.UUID) = p.ID
		*dest[1].(*uuid.UUID) = p.UserID
		*dest[2].(*uuid.UUID) = p.RepoID
		*dest[3].(*[]byte) = p.Packages
		*dest[4].(*pgtype.Timestamptz) = p.CreatedAt
		*dest[5].(*pgtype.Timestamptz) = p.UpdatedAt
		return nil
	}
}

// toolAllowlistRows is the pgx.Rows ListToolAllowlist iterates, replaying a fixed
// slice of allowlist rows.
type toolAllowlistRows struct {
	rows []store.ToolAllowlist
	i    int
}

func (r *toolAllowlistRows) Close()                                       {}
func (r *toolAllowlistRows) Err() error                                   { return nil }
func (r *toolAllowlistRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *toolAllowlistRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *toolAllowlistRows) Next() bool                                   { r.i++; return r.i <= len(r.rows) }
func (r *toolAllowlistRows) Scan(dest ...any) error {
	return scanToolAllowlist(r.rows[r.i-1])(dest...)
}
func (r *toolAllowlistRows) Values() ([]any, error) { return nil, nil }
func (r *toolAllowlistRows) RawValues() [][]byte    { return nil }
func (r *toolAllowlistRows) Conn() *pgx.Conn        { return nil }

// allowlistRow is a terse constructor for a fake allowlist row: a name and an
// optional pinned version.
func allowlistRow(name, pinned string) store.ToolAllowlist {
	row := store.ToolAllowlist{ID: uuid.New(), Name: name}
	if pinned != "" {
		row.PinnedVersion = pgtype.Text{String: pinned, Valid: true}
	}
	return row
}

// adminReq builds an admin POST/PUT request to path with a JSON body, an admin user
// on the context, and the given chi URL params.
func adminReq(method, path, body string, params map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req = req.WithContext(mw.ContextWithUser(req.Context(),
		store.User{ID: uuid.New(), IsAdmin: true, IsActive: true}))
	if len(params) > 0 {
		rctx := chi.NewRouteContext()
		for k, v := range params {
			rctx.URLParams.Add(k, v)
		}
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}
	return req
}

// TestCreateToolAllowlistEntrySeedGate is the SC2 create gate: an admin adding an
// UNCOVERED, non-denylisted, well-formed package is refused with a 400 that names the
// package and the image-roll requirement, before any DB write. A COVERED name (a baked
// package or a grandfathered exception) passes the coverage gate and reaches the
// normal create path (201).
func TestCreateToolAllowlistEntrySeedGate(t *testing.T) {
	t.Run("uncovered package is 400 with the image-roll requirement", func(t *testing.T) {
		db := &toolGateDB{}
		h := &Handler{q: store.New(db)}
		rec := httptest.NewRecorder()

		h.CreateToolAllowlistEntry(rec, adminReq(http.MethodPost, "/api/tool-allowlist/", `{"name":"ruby"}`, nil))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "ruby") {
			t.Errorf("400 body must name the package %q; got %s", "ruby", body)
		}
		if !strings.Contains(body, "baked worker toolchain") {
			t.Errorf("400 body must cite the baked worker toolchain; got %s", body)
		}
		if !strings.Contains(body, "rolled") {
			t.Errorf("400 body must state the image-roll requirement; got %s", body)
		}
		if db.createCalled {
			t.Fatal("an uncovered package reached the INSERT — the coverage gate ran too late")
		}
	})

	t.Run("covered baked package passes the gate and is created", func(t *testing.T) {
		db := &toolGateDB{}
		h := &Handler{q: store.New(db)}
		rec := httptest.NewRecorder()

		h.CreateToolAllowlistEntry(rec, adminReq(http.MethodPost, "/api/tool-allowlist/", `{"name":"ripgrep"}`, nil))

		if rec.Code != http.StatusCreated {
			t.Fatalf("code = %d, want 201; body=%s", rec.Code, rec.Body.String())
		}
		if !db.createCalled || db.createdName != "ripgrep" {
			t.Fatalf("covered create did not reach the INSERT (createCalled=%v name=%q)", db.createCalled, db.createdName)
		}
	})

	t.Run("grandfathered exception passes the gate and is created", func(t *testing.T) {
		db := &toolGateDB{}
		h := &Handler{q: store.New(db)}
		rec := httptest.NewRecorder()

		// kubectl is a seedException (allowlisted-but-unbaked): it must not be rejected
		// for the coverage reason.
		h.CreateToolAllowlistEntry(rec, adminReq(http.MethodPost, "/api/tool-allowlist/", `{"name":"kubectl"}`, nil))

		if rec.Code != http.StatusCreated {
			t.Fatalf("code = %d, want 201; body=%s", rec.Code, rec.Body.String())
		}
		if !db.createCalled {
			t.Fatal("a covered exception did not reach the INSERT")
		}
	})
}

// TestSetRepoToolProfileBakedGate is the profile gate (Decision 4c): a profile that
// requests a grandfathered-but-unbaked allowlist row is refused with a 400 citing the
// baked worker toolchain, before any DB write; a profile of only covered packages
// passes and is saved.
func TestSetRepoToolProfileBakedGate(t *testing.T) {
	repoID := uuid.New()

	t.Run("unbaked allowlisted package is 400 citing the toolchain", func(t *testing.T) {
		// terraform is on the allowlist (grandfathered) but not in the baked seed.
		db := &toolGateDB{allowlist: []store.ToolAllowlist{
			allowlistRow("terraform", ""),
			allowlistRow("jq", ""),
		}}
		h := &Handler{q: store.New(db)}
		rec := httptest.NewRecorder()

		req := adminReq(http.MethodPut, "/api/repos/"+repoID.String()+"/tool-profile",
			`{"packages":["terraform"]}`, map[string]string{"id": repoID.String()})
		h.SetRepoToolProfile(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("code = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if !strings.Contains(body, "baked worker toolchain") {
			t.Errorf("400 body must cite the baked worker toolchain; got %s", body)
		}
		if !strings.Contains(body, "terraform") {
			t.Errorf("400 body must name the offending package; got %s", body)
		}
		if db.upsertCalled {
			t.Fatal("an unbaked package reached the UPSERT — the baked gate ran too late")
		}
	})

	t.Run("covered packages pass the gate and are saved", func(t *testing.T) {
		db := &toolGateDB{allowlist: []store.ToolAllowlist{
			allowlistRow("kubectl", "1.31"),
			allowlistRow("jq", ""),
		}}
		h := &Handler{q: store.New(db)}
		rec := httptest.NewRecorder()

		req := adminReq(http.MethodPut, "/api/repos/"+repoID.String()+"/tool-profile",
			`{"packages":["kubectl@1.31","jq"]}`, map[string]string{"id": repoID.String()})
		h.SetRepoToolProfile(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if !db.upsertCalled {
			t.Fatal("a fully covered profile did not reach the UPSERT")
		}
	})
}
