package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/settings"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeSelfimproveDB is a store.DBTX serving the queries PutSelfimproveConfig runs:
// GetRepoForUser (ownership gate), UpsertAppSetting (recorded), and the read-back's
// CountActiveSelfImproveRuns. repoOwned toggles the ownership gate.
type fakeSelfimproveDB struct {
	repoOwned bool
	repoID    uuid.UUID
	ownerID   uuid.UUID
	upserts   map[string]string
}

func (f *fakeSelfimproveDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (f *fakeSelfimproveDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}
func (f *fakeSelfimproveDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "INSERT INTO app_settings"):
		if f.upserts == nil {
			f.upserts = map[string]string{}
		}
		key, _ := args[0].(string)
		val, _ := args[1].(string)
		f.upserts[key] = val
		return selfimproveScanRow{upsert: &store.AppSetting{Key: key, Value: val}}
	case strings.Contains(sql, "FROM repos"):
		if !f.repoOwned {
			return selfimproveScanRow{err: pgx.ErrNoRows}
		}
		return selfimproveScanRow{repo: &store.GetRepoForUserRow{ID: f.repoID, UserID: f.ownerID, PathWithNamespace: "vtmocanu/uzi"}}
	case strings.Contains(sql, "count(*) FROM runs"):
		return selfimproveScanRow{count: new(int64)}
	}
	return selfimproveScanRow{err: pgx.ErrNoRows}
}

type selfimproveScanRow struct {
	upsert *store.AppSetting
	repo   *store.GetRepoForUserRow
	count  *int64
	err    error
}

func (r selfimproveScanRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	switch {
	case r.upsert != nil:
		*dest[0].(*string) = r.upsert.Key
		*dest[1].(*string) = r.upsert.Value
		// dest[2] updated_by (pgtype.UUID), dest[3] updated_at (pgtype.Timestamptz) — leave zero.
	case r.repo != nil:
		*dest[0].(*uuid.UUID) = r.repo.ID
		*dest[1].(*uuid.UUID) = r.repo.ConnectionID
		*dest[2].(*int64) = r.repo.ForgeProjectID
		*dest[3].(*string) = r.repo.PathWithNamespace
		*dest[4].(*string) = r.repo.WebUrl
		*dest[5].(*pgtype.Text) = r.repo.DefaultBranch
		*dest[6].(*bool) = r.repo.Enabled
		// #66 M8: the three guardrail-override columns sit between enabled and the
		// connection fields in GetRepoForUser's SELECT — left zero here.
		*dest[7].(*pgtype.Text) = r.repo.GuardrailOverrideReason
		*dest[8].(*pgtype.UUID) = r.repo.GuardrailOverrideBy
		*dest[9].(*pgtype.Timestamptz) = r.repo.GuardrailOverrideAt
		*dest[10].(*string) = r.repo.ForgeType
		*dest[11].(*string) = r.repo.BaseUrl
		*dest[12].(*[]byte) = r.repo.TokenCiphertext
		*dest[13].(*uuid.UUID) = r.repo.UserID
	case r.count != nil:
		*dest[0].(*int64) = *r.count
	}
	return nil
}

func selfimproveHandler(db *fakeSelfimproveDB) *Handler {
	return &Handler{q: store.New(db), settings: settings.New(&settingsStore{}, time.Minute)}
}

func siAuthed(req *http.Request, id uuid.UUID) *http.Request {
	return req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: id, IsAdmin: true, IsActive: true}))
}

func siPut(body string, id uuid.UUID) *http.Request {
	return siAuthed(httptest.NewRequest(http.MethodPut, "/api/admin/selfimprove", strings.NewReader(body)), id)
}

func TestPutSelfimproveRequiresAuth(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.PutSelfimproveConfig(rec, httptest.NewRequest(http.MethodPut, "/api/admin/selfimprove", strings.NewReader(`{"enabled":false}`)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestPutSelfimproveEnableRequiresRepo(t *testing.T) {
	h := selfimproveHandler(&fakeSelfimproveDB{})
	rec := httptest.NewRecorder()
	h.PutSelfimproveConfig(rec, siPut(`{"enabled":true}`, uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("enable without repo_id: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutSelfimproveRejectsBadInterval(t *testing.T) {
	h := selfimproveHandler(&fakeSelfimproveDB{})
	rec := httptest.NewRecorder()
	h.PutSelfimproveConfig(rec, siPut(`{"enabled":false,"interval":"soon"}`, uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad interval: status = %d, want 400", rec.Code)
	}
}

func TestPutSelfimproveRejectsUnownedRepo(t *testing.T) {
	// repoOwned=false ⇒ GetRepoForUser returns ErrNoRows ⇒ 400.
	h := selfimproveHandler(&fakeSelfimproveDB{repoOwned: false})
	rec := httptest.NewRecorder()
	h.PutSelfimproveConfig(rec, siPut(`{"enabled":true,"repo_id":"`+uuid.New().String()+`"}`, uuid.New()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unowned repo: status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestPutSelfimproveEnableSetsSessionUserAsOwner(t *testing.T) {
	// Audit H3: the run owner is the SESSION admin, never the body (the request has
	// no user-id field to begin with).
	admin := uuid.New()
	repoID := uuid.New()
	db := &fakeSelfimproveDB{repoOwned: true, repoID: repoID, ownerID: admin}
	h := selfimproveHandler(db)
	rec := httptest.NewRecorder()
	h.PutSelfimproveConfig(rec, siPut(`{"enabled":true,"repo_id":"`+repoID.String()+`","interval":"24h"}`, admin))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.upserts[settings.KeySelfimproveUserID] != admin.String() {
		t.Fatalf("selfimprove_user_id = %q, want the session admin %s", db.upserts[settings.KeySelfimproveUserID], admin)
	}
	if db.upserts[settings.KeySelfimproveRepo] != repoID.String() {
		t.Fatalf("selfimprove_repo = %q, want %s", db.upserts[settings.KeySelfimproveRepo], repoID)
	}
	if db.upserts[settings.KeySelfimproveEnabled] != "true" {
		t.Fatalf("selfimprove_enabled = %q, want true", db.upserts[settings.KeySelfimproveEnabled])
	}
	if db.upserts[settings.KeySelfimproveInterval] != "24h" {
		t.Fatalf("selfimprove_interval = %q, want 24h", db.upserts[settings.KeySelfimproveInterval])
	}
}

func TestPutSelfimproveDisableNeedsNoRepo(t *testing.T) {
	db := &fakeSelfimproveDB{}
	h := selfimproveHandler(db)
	rec := httptest.NewRecorder()
	h.PutSelfimproveConfig(rec, siPut(`{"enabled":false}`, uuid.New()))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if db.upserts[settings.KeySelfimproveEnabled] != "false" {
		t.Fatalf("selfimprove_enabled = %q, want false", db.upserts[settings.KeySelfimproveEnabled])
	}
	// Disabling must not write repo/user_id.
	if _, ok := db.upserts[settings.KeySelfimproveRepo]; ok {
		t.Fatalf("disable must not write selfimprove_repo")
	}
}
