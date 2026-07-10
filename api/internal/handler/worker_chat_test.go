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

	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// workerChatStore is a minimal workersvc.Store for the M3 worker-endpoint tests: it
// owns one chat run + one repo for a fixed user and answers only the queries those
// paths reach (everything else panics via the embedded interface).
type workerChatStore struct {
	workersvc.Store
	userID       uuid.UUID
	chatRun      store.Run
	repoID       uuid.UUID
	pendingCount int64
	created      *store.CreateIssueProposalParams
	lastMsgLim   int32
}

func (s *workerChatStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.chatRun.ID && arg.UserID == s.userID {
		return s.chatRun, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *workerChatStore) GetRepoForUser(_ context.Context, arg store.GetRepoForUserParams) (store.GetRepoForUserRow, error) {
	if arg.ID == s.repoID && arg.UserID == s.userID {
		return store.GetRepoForUserRow{ID: s.repoID, UserID: s.userID}, nil
	}
	return store.GetRepoForUserRow{}, pgx.ErrNoRows
}
func (s *workerChatStore) CountPendingProposalsForRun(context.Context, uuid.UUID) (int64, error) {
	return s.pendingCount, nil
}
func (s *workerChatStore) CreateIssueProposal(_ context.Context, arg store.CreateIssueProposalParams) (store.IssueProposal, error) {
	s.created = &arg
	return store.IssueProposal{ID: uuid.New(), RunID: arg.RunID, RepoID: arg.RepoID, Title: arg.Title, Description: arg.Description, Labels: arg.Labels, Status: "pending"}, nil
}
func (s *workerChatStore) ListRunsForWorkerUser(_ context.Context, arg store.ListRunsForWorkerUserParams) ([]store.ListRunsForWorkerUserRow, error) {
	if arg.UserID != s.userID {
		return nil, nil
	}
	return []store.ListRunsForWorkerUserRow{{ID: s.chatRun.ID, Kind: "chat", Status: "running", IssueTitle: "t"}}, nil
}
func (s *workerChatStore) GetRunForWorkerUser(_ context.Context, arg store.GetRunForWorkerUserParams) (store.GetRunForWorkerUserRow, error) {
	if arg.ID == s.chatRun.ID && arg.UserID == s.userID {
		return store.GetRunForWorkerUserRow{ID: s.chatRun.ID, Kind: "chat", Status: "running", IssueTitle: "t"}, nil
	}
	return store.GetRunForWorkerUserRow{}, pgx.ErrNoRows
}
func (s *workerChatStore) ListRunMessagesForWorkerPage(_ context.Context, arg store.ListRunMessagesForWorkerPageParams) ([]store.RunMessage, error) {
	s.lastMsgLim = arg.Lim
	return []store.RunMessage{{Seq: 1, Kind: "text", Payload: []byte(`{}`)}}, nil
}

func newWorkerChatHandler(st workersvc.Store) *Handler {
	return &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}
}

// workerReq builds a worker-authenticated request with an optional chi {id} param
// and body/query.
func workerChatReq(method, target string, wkr store.Worker, id uuid.UUID, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rctx := chi.NewRouteContext()
	if id != uuid.Nil {
		rctx.URLParams.Add("id", id.String())
	}
	ctx := context.WithValue(mw.ContextWithWorker(r.Context(), wkr), chi.RouteCtxKey, rctx)
	return r.WithContext(ctx)
}

func TestWorkerCreateProposalAuthzAndCaps(t *testing.T) {
	uid, runID, repoID := uuid.New(), uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	base := func() *workerChatStore {
		return &workerChatStore{userID: uid, repoID: repoID, chatRun: store.Run{ID: runID, UserID: uid, Kind: workersvc.RunKindChat, Status: "running"}}
	}
	body := `{"repo_id":"` + repoID.String() + `","title":"Add a job","description":"d","labels":["enhancement"]}`

	// Happy path: owner's chat run + owned repo → 201 and a proposal row is created.
	st := base()
	h := newWorkerChatHandler(st)
	rec := httptest.NewRecorder()
	h.WorkerCreateProposal(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/proposals", wkr, runID, body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create proposal = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if st.created == nil || st.created.RunID != runID {
		t.Fatalf("proposal must be created against the run")
	}

	// A run the worker's user does not own → 404.
	rec = httptest.NewRecorder()
	h.WorkerCreateProposal(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/proposals", wkr, uuid.New(), body))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign run proposal = %d, want 404", rec.Code)
	}

	// A non-chat run target → 409.
	st = base()
	st.chatRun.Kind = workersvc.RunKindIssue
	rec = httptest.NewRecorder()
	newWorkerChatHandler(st).WorkerCreateProposal(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/proposals", wkr, runID, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("non-chat proposal target = %d, want 409", rec.Code)
	}

	// A terminal chat → 409.
	st = base()
	st.chatRun.Status = "completed"
	rec = httptest.NewRecorder()
	newWorkerChatHandler(st).WorkerCreateProposal(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/proposals", wkr, runID, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("terminal chat proposal = %d, want 409", rec.Code)
	}

	// A repo the user does not own → 404.
	rec = httptest.NewRecorder()
	foreignRepoBody := `{"repo_id":"` + uuid.New().String() + `","title":"t","description":"d","labels":[]}`
	newWorkerChatHandler(base()).WorkerCreateProposal(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/proposals", wkr, runID, foreignRepoBody))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unowned repo proposal = %d, want 404", rec.Code)
	}

	// Per-run pending cap reached → 409.
	st = base()
	st.pendingCount = workersvc.MaxPendingProposalsPerRun
	rec = httptest.NewRecorder()
	newWorkerChatHandler(st).WorkerCreateProposal(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/proposals", wkr, runID, body))
	if rec.Code != http.StatusConflict {
		t.Fatalf("capped proposal = %d, want 409", rec.Code)
	}
}

func TestWorkerChatReadsAreUserScoped(t *testing.T) {
	uid, runID := uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	st := &workerChatStore{userID: uid, chatRun: store.Run{ID: runID, UserID: uid, Kind: workersvc.RunKindChat, Status: "running"}}
	h := newWorkerChatHandler(st)

	// List: the worker's user's runs.
	rec := httptest.NewRecorder()
	h.WorkerChatListRuns(rec, workerChatReq(http.MethodGet, "/api/worker/chat/runs", wkr, uuid.Nil, ""))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"runs"`) {
		t.Fatalf("list runs = %d %q, want 200 with runs", rec.Code, rec.Body.String())
	}

	// Detail: own run → 200; a foreign run id → 404.
	rec = httptest.NewRecorder()
	h.WorkerChatGetRun(rec, workerChatReq(http.MethodGet, "/api/worker/chat/runs/x", wkr, runID, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("get own run = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.WorkerChatGetRun(rec, workerChatReq(http.MethodGet, "/api/worker/chat/runs/x", wkr, uuid.New(), ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("get foreign run = %d, want 404", rec.Code)
	}

	// Messages: own run → 200; a foreign run id → 404.
	rec = httptest.NewRecorder()
	h.WorkerChatRunMessages(rec, workerChatReq(http.MethodGet, "/api/worker/chat/runs/x/messages", wkr, runID, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("own run messages = %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.WorkerChatRunMessages(rec, workerChatReq(http.MethodGet, "/api/worker/chat/runs/x/messages", wkr, uuid.New(), ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign run messages = %d, want 404", rec.Code)
	}
}

func TestWorkerChatMessagesPagingBounds(t *testing.T) {
	uid, runID := uuid.New(), uuid.New()
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	st := &workerChatStore{userID: uid, chatRun: store.Run{ID: runID, UserID: uid, Kind: workersvc.RunKindChat, Status: "running"}}
	h := newWorkerChatHandler(st)

	// An over-cap limit is clamped to the max; an absent limit uses the default max.
	for _, target := range []string{"/api/worker/chat/runs/x/messages?limit=9999", "/api/worker/chat/runs/x/messages"} {
		rec := httptest.NewRecorder()
		h.WorkerChatRunMessages(rec, workerChatReq(http.MethodGet, target, wkr, runID, ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", target, rec.Code)
		}
		if st.lastMsgLim != 200 {
			t.Fatalf("%s: page limit = %d, want clamped to 200", target, st.lastMsgLim)
		}
	}

	// A small explicit limit passes through unchanged.
	rec := httptest.NewRecorder()
	h.WorkerChatRunMessages(rec, workerChatReq(http.MethodGet, "/api/worker/chat/runs/x/messages?limit=5", wkr, runID, ""))
	if st.lastMsgLim != 5 {
		t.Fatalf("explicit small limit = %d, want 5", st.lastMsgLim)
	}
}
