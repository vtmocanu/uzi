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

	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeRunsCountDB is a store.DBTX that answers the in-progress count(*) query from a
// fixed number, so RunsInProgressCount runs end to end (scope check → store →
// respond) without a database.
type fakeRunsCountDB struct{ count int64 }

func (f *fakeRunsCountDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (f *fakeRunsCountDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

func (f *fakeRunsCountDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "count(*) FROM runs") {
		return fakeScanRow{scanInt64(f.count)}
	}
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

func TestRunsInProgressCountRequireAuth(t *testing.T) {
	h := &Handler{} // no user in context ⇒ 401 before any store access
	rec := httptest.NewRecorder()
	h.RunsInProgressCount(rec, httptest.NewRequest(http.MethodGet, "/api/me/runs/in-progress-count", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status = %d, want 401", rec.Code)
	}
}

func TestRunsInProgressCountShape(t *testing.T) {
	user := uuid.New()
	h := &Handler{q: store.New(&fakeRunsCountDB{count: 7})}
	rec := httptest.NewRecorder()
	h.RunsInProgressCount(rec, notifUser(httptest.NewRequest(http.MethodGet, "/api/me/runs/in-progress-count", nil), user, false))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Count int64 `json:"count"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Count != 7 {
		t.Fatalf("count = %d, want 7", out.Count)
	}
}
