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
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// workerSummariesStore answers only the queries the summary endpoints reach; the embedded
// workersvc.Store panics on anything else. It records the write params + rows-affected so a
// test can assert idempotency (no write) and the stale-write guard value.
type workerSummariesStore struct {
	workersvc.Store
	userID uuid.UUID
	run    store.Run

	intentParams *store.SetRunIntentSummaryParams
	intentRows   int64
	planParams   *store.SetRunPlanSummaryParams
	planRows     int64
}

func (s *workerSummariesStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.run.ID && arg.UserID == s.userID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *workerSummariesStore) SetRunIntentSummary(_ context.Context, arg store.SetRunIntentSummaryParams) (int64, error) {
	s.intentParams = &arg
	return s.intentRows, nil
}
func (s *workerSummariesStore) SetRunPlanSummary(_ context.Context, arg store.SetRunPlanSummaryParams) (int64, error) {
	s.planParams = &arg
	return s.planRows, nil
}

func summaryRun(userID, repoID uuid.UUID) store.Run {
	return store.Run{
		ID:     uuid.New(),
		UserID: userID,
		RepoID: pgtype.UUID{Bytes: repoID, Valid: true},
		Status: "running",
		Kind:   "issue",
		PlanMd: pgtype.Text{String: "the plan", Valid: true},
	}
}

func TestWorkerSetIntentSummaryRequiresWorker(t *testing.T) {
	st := &workerSummariesStore{}
	h := newWorkerChatHandler(st)
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/summary/intent", strings.NewReader(`{"summary":"s"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.WorkerSetIntentSummary(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no worker = %d, want 401", rec.Code)
	}
	if st.intentParams != nil {
		t.Error("no summary must be written without a worker")
	}
}

func TestWorkerSetIntentSummaryHappyPath(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := summaryRun(uid, repoID)
	st := &workerSummariesStore{userID: uid, run: run, intentRows: 1}
	h := newWorkerChatHandler(st)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}

	rec := httptest.NewRecorder()
	h.WorkerSetIntentSummary(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/summary/intent", wkr, run.ID, `{"summary":"builds the thing"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("set intent = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.intentParams == nil || st.intentParams.SummaryIntent.String != "builds the thing" {
		t.Fatalf("intent params = %+v, want the summary text", st.intentParams)
	}
}

func TestWorkerSetIntentSummaryIdempotentSecondPostIsNoop(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := summaryRun(uid, repoID)
	run.SummaryIntent = pgtype.Text{String: "already set", Valid: true}
	st := &workerSummariesStore{userID: uid, run: run, intentRows: 1}
	h := newWorkerChatHandler(st)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}

	rec := httptest.NewRecorder()
	h.WorkerSetIntentSummary(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/summary/intent", wkr, run.ID, `{"summary":"a newer one"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("idempotent second post = %d, want 200 (no-op success); body=%s", rec.Code, rec.Body.String())
	}
	if st.intentParams != nil {
		t.Fatalf("an already-set intent must not be rewritten, got %+v", st.intentParams)
	}
}

func TestWorkerSetIntentSummaryGuards(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	run := summaryRun(uid, repoID)

	cases := []struct {
		name string
		st   *workerSummariesStore
		id   uuid.UUID
		body string
		want int
	}{
		{"empty summary 400", &workerSummariesStore{userID: uid, run: run, intentRows: 1}, run.ID, `{"summary":""}`, http.StatusBadRequest},
		{"oversize summary 400", &workerSummariesStore{userID: uid, run: run, intentRows: 1}, run.ID, `{"summary":"` + strings.Repeat("x", workersvc.MaxSummaryBytes+1) + `"}`, http.StatusBadRequest},
		{"foreign run 404", &workerSummariesStore{userID: uid, run: run, intentRows: 1}, uuid.New(), `{"summary":"s"}`, http.StatusNotFound},
		{"terminal run 409", &workerSummariesStore{userID: uid, run: terminalSummaryRun(uid, repoID), intentRows: 1}, terminalRunID, `{"summary":"s"}`, http.StatusConflict},
		{"repo-less run 409", &workerSummariesStore{userID: uid, run: repolessSummaryRun(uid), intentRows: 1}, repolessRunID, `{"summary":"s"}`, http.StatusConflict},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newWorkerChatHandler(c.st).WorkerSetIntentSummary(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/summary/intent", wkr, c.id, c.body))
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func TestWorkerSetPlanSummaryHappyPath(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := summaryRun(uid, repoID)
	st := &workerSummariesStore{userID: uid, run: run, planRows: 1}
	h := newWorkerChatHandler(st)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}

	body := `{"summary":"the plan does X","deltas":[{"kind":"added","text":"a test"}],"plan_md":"the plan"}`
	rec := httptest.NewRecorder()
	h.WorkerSetPlanSummary(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/summary/plan", wkr, run.ID, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("set plan = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if st.planParams == nil || st.planParams.ExpectedPlanMd.String != "the plan" {
		t.Fatalf("plan params = %+v, want expected_plan_md = the posted plan_md", st.planParams)
	}
}

func TestWorkerSetPlanSummaryGuardsAndValidation(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	run := summaryRun(uid, repoID)

	cases := []struct {
		name string
		st   *workerSummariesStore
		id   uuid.UUID
		body string
		want int
	}{
		{"missing plan_md 400", &workerSummariesStore{userID: uid, run: run, planRows: 1}, run.ID, `{"summary":"s","deltas":[],"plan_md":""}`, http.StatusBadRequest},
		{"bad delta kind 400", &workerSummariesStore{userID: uid, run: run, planRows: 1}, run.ID, `{"summary":"s","deltas":[{"kind":"removed","text":"x"}],"plan_md":"the plan"}`, http.StatusBadRequest},
		{"empty delta text 400", &workerSummariesStore{userID: uid, run: run, planRows: 1}, run.ID, `{"summary":"s","deltas":[{"kind":"added","text":""}],"plan_md":"the plan"}`, http.StatusBadRequest},
		{"stale write 409", &workerSummariesStore{userID: uid, run: run, planRows: 0}, run.ID, `{"summary":"s","deltas":[],"plan_md":"an older plan"}`, http.StatusConflict},
		{"foreign run 404", &workerSummariesStore{userID: uid, run: run, planRows: 1}, uuid.New(), `{"summary":"s","deltas":[],"plan_md":"the plan"}`, http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newWorkerChatHandler(c.st).WorkerSetPlanSummary(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/summary/plan", wkr, c.id, c.body))
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// Fixed ids so a table row's run and its requested id line up (a foreign run uses a fresh id).
var (
	terminalRunID = uuid.New()
	repolessRunID = uuid.New()
)

func terminalSummaryRun(userID, repoID uuid.UUID) store.Run {
	r := summaryRun(userID, repoID)
	r.ID = terminalRunID
	r.Status = "completed"
	return r
}
func repolessSummaryRun(userID uuid.UUID) store.Run {
	r := summaryRun(userID, uuid.New())
	r.ID = repolessRunID
	r.RepoID = pgtype.UUID{Valid: false}
	return r
}
