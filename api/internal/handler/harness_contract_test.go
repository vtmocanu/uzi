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

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// harness_contract_test.go is the PRD #1332 (M5A / D7) negative acceptance for the dark
// harness boundary: a raw request body carrying a `harness` field to a public
// creation path MUST be rejected as an UNKNOWN field (HTTP 400), because the request
// structs have no `harness` member and httpx.DecodeJSON{,Limited} set
// DisallowUnknownFields. A raw API client sending {"harness":"codex"} therefore gets a
// 400, so adding the field later (M5B) is a real contract change, not a silently-ignored
// one. Each case is paired with a control body (same shape, no harness) that gets PAST
// the decoder to a DIFFERENT 400, proving the rejection is specifically the harness key
// and not a generic malformation.

// harnessContractDB answers only GetRepoForUser so repoForRequest (which runs BEFORE the
// body decode in both handlers) succeeds; the returned row is left zero because neither
// the decode-reject case nor the control case reads a repo field. Any other query is a
// test failure — it would mean the handler advanced past the point these tests probe.
type harnessContractDB struct{ t *testing.T }

func (harnessContractDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (harnessContractDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, pgx.ErrNoRows
}

func (d harnessContractDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	if strings.Contains(sql, "name: GetRepoForUser") {
		// A repo the caller owns: err==nil with a zero row is all repoForRequest needs.
		return fakeScanRow{func(...any) error { return nil }}
	}
	d.t.Errorf("unexpected query reached after the point under test: %s", sql)
	return fakeScanRow{func(...any) error { return pgx.ErrNoRows }}
}

// repoScopedReq builds a repo-scoped POST with the caller in context and the chi {id}
// URL param set to a fresh repo UUID, so repoForRequest resolves.
func repoScopedReq(body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	return req.WithContext(mw.ContextWithUser(ctx, store.User{ID: uuid.New(), IsActive: true}))
}

// TestCreateRunRejectsHarnessField: POST issue-run start with {"harness":"codex", ...}
// is a 400 unknown-field decode error, while the same body without harness gets past the
// decoder to the issue_iid validation 400 — so the harness key is what the decoder
// rejects.
func TestCreateRunRejectsHarnessField(t *testing.T) {
	h := &Handler{q: store.New(harnessContractDB{t: t})}

	rec := httptest.NewRecorder()
	h.CreateRun(rec, repoScopedReq(`{"harness":"codex","issue_iid":123}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown harness field; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid request body") {
		t.Fatalf("body = %s, want the decode-level 'invalid request body' (proving the harness field was rejected at decode)", rec.Body.String())
	}

	// Control: identical shape minus harness decodes fine and fails LATER, at the
	// issue_iid check — a different 400, so the harness key is the decode cause.
	ctrl := httptest.NewRecorder()
	h.CreateRun(ctrl, repoScopedReq(`{"issue_iid":0}`))
	if ctrl.Code != http.StatusBadRequest {
		t.Fatalf("control status = %d, want 400; body=%s", ctrl.Code, ctrl.Body.String())
	}
	if strings.Contains(ctrl.Body.String(), "invalid request body") {
		t.Fatalf("control body = %s, must NOT be the decode error — a harness-free body must decode", ctrl.Body.String())
	}
	if !strings.Contains(ctrl.Body.String(), "issue_iid must be a positive integer") {
		t.Fatalf("control body = %s, want the issue_iid validation 400", ctrl.Body.String())
	}
}

// TestCreateScheduleRejectsHarnessField: POST schedule-create with {"harness":"codex", ...}
// is a 400 unknown-field decode error, while a harness-free body gets past the decoder to
// schedule-config validation (a different 400).
func TestCreateScheduleRejectsHarnessField(t *testing.T) {
	h := &Handler{q: store.New(harnessContractDB{t: t})}

	rec := httptest.NewRecorder()
	h.CreateSchedule(rec, repoScopedReq(`{"harness":"codex","target":"issue","timing":"daily","issue_iid":1}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an unknown harness field; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid request body") {
		t.Fatalf("body = %s, want the decode-level 'invalid request body'", rec.Body.String())
	}

	// Control: a harness-free body decodes fine and fails LATER, in validateScheduleConfig.
	ctrl := httptest.NewRecorder()
	h.CreateSchedule(ctrl, repoScopedReq(`{}`))
	if ctrl.Code != http.StatusBadRequest {
		t.Fatalf("control status = %d, want 400; body=%s", ctrl.Code, ctrl.Body.String())
	}
	if strings.Contains(ctrl.Body.String(), "invalid request body") {
		t.Fatalf("control body = %s, must NOT be the decode error — a harness-free body must decode", ctrl.Body.String())
	}
	if !strings.Contains(ctrl.Body.String(), "target must be one of") {
		t.Fatalf("control body = %s, want the schedule-config validation 400", ctrl.Body.String())
	}
}
