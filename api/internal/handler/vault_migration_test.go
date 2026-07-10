package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeCountDB is a store.DBTX whose QueryRow scans a fixed int64 — enough to drive
// CountMasterSealedSecrets (a single-column count) without a database.
type fakeCountDB struct{ count int64 }

func (fakeCountDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (fakeCountDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fakeCountDB: Query not used")
}
func (f fakeCountDB) QueryRow(context.Context, string, ...any) pgx.Row { return fakeCountRow{f.count} }

type fakeCountRow struct{ count int64 }

func (r fakeCountRow) Scan(dest ...any) error {
	if len(dest) == 1 {
		if p, ok := dest[0].(*int64); ok {
			*p = r.count
		}
	}
	return nil
}

// TestVaultMigrationReturnsCount: the handler surfaces the store count as
// {master_sealed: N} and nothing else (no secret value, no identity).
func TestVaultMigrationReturnsCount(t *testing.T) {
	h := &Handler{q: store.New(fakeCountDB{count: 3})}
	rec := httptest.NewRecorder()
	h.VaultMigration(rec, httptest.NewRequest(http.MethodGet, "/api/admin/vault-migration", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := out["master_sealed"]; got != float64(3) {
		t.Fatalf("master_sealed = %v, want 3", got)
	}
	if len(out) != 1 {
		t.Fatalf("response carries extra fields: %v", out)
	}
}

// TestVaultMigrationAdminOnly: mounted under RequireAdmin, a non-admin gets 403
// and never reaches the count; an admin passes through.
func TestVaultMigrationAdminOnly(t *testing.T) {
	h := &Handler{q: store.New(fakeCountDB{count: 0})}
	guarded := mw.RequireAdmin(http.HandlerFunc(h.VaultMigration))

	for _, tc := range []struct {
		name    string
		isAdmin bool
		want    int
	}{
		{"non-admin", false, http.StatusForbidden},
		{"admin", true, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/admin/vault-migration", nil)
			req = req.WithContext(mw.ContextWithUser(req.Context(),
				store.User{ID: uuid.New(), IsAdmin: tc.isAdmin, IsActive: true}))
			rec := httptest.NewRecorder()
			guarded.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("%s: status = %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}
