package handler

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Shared in-memory pgx row fakes for handler tests that drive a store.DBTX without a
// database. They were born in the retired notifications inbox tests (PRD #1650 D1) and
// moved here unchanged because other suites reuse them.

// fakeScanRow is a pgx.Row whose Scan runs the given function.
type fakeScanRow struct{ scan func(dest ...any) error }

func (r fakeScanRow) Scan(dest ...any) error { return r.scan(dest...) }

// scanInt64 writes v into a single *int64 destination (a count(*) row).
func scanInt64(v int64) func(dest ...any) error {
	return func(dest ...any) error {
		if p, ok := dest[0].(*int64); ok {
			*p = v
		}
		return nil
	}
}

// fakeNotifRows is a pgx.Rows over a fixed list of per-row scan functions.
type fakeNotifRows struct {
	scans []func(dest ...any) error
	i     int
}

func (r *fakeNotifRows) Close()                                       {}
func (r *fakeNotifRows) Err() error                                   { return nil }
func (r *fakeNotifRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeNotifRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeNotifRows) Next() bool                                   { r.i++; return r.i <= len(r.scans) }
func (r *fakeNotifRows) Scan(dest ...any) error                       { return r.scans[r.i-1](dest...) }
func (r *fakeNotifRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeNotifRows) RawValues() [][]byte                          { return nil }
func (r *fakeNotifRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeNotifRows) TypeMap() *pgtype.Map                         { return pgtype.NewMap() }

// notifUser attaches a session user (own-user / admin scope) to a request.
func notifUser(req *http.Request, id uuid.UUID, admin bool) *http.Request {
	return req.WithContext(mw.ContextWithUser(req.Context(),
		store.User{ID: id, IsAdmin: admin, IsActive: true}))
}
