package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/hub"
	mw "gitlab.example.com/vtmocanu/uzi/api/internal/middleware"
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// runsStore is a minimal workersvc.Store for the run REST + WS authz tests: it
// owns a single run and answers the viewer/list queries the tested paths reach.
type runsStore struct {
	workersvc.Store
	ownerID    uuid.UUID
	run        store.Run
	msgs       []store.RunMessage
	userRuns    []store.ListRunsForUserRow
	lastRunsArg *store.ListRunsForUserParams
	allWorkers  []store.ListAllWorkersRow
	activeRuns  []store.ListActiveRunsAllRow
}

func (s *runsStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.run.ID && arg.UserID == s.ownerID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *runsStore) GetRunByID(_ context.Context, id uuid.UUID) (store.Run, error) {
	if id == s.run.ID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *runsStore) ListRunMessagesAfter(context.Context, store.ListRunMessagesAfterParams) ([]store.RunMessage, error) {
	return s.msgs, nil
}
func (s *runsStore) ListRunsForUser(_ context.Context, arg store.ListRunsForUserParams) ([]store.ListRunsForUserRow, error) {
	s.lastRunsArg = &arg
	return s.userRuns, nil
}
func (s *runsStore) ListAllWorkers(context.Context) ([]store.ListAllWorkersRow, error) {
	return s.allWorkers, nil
}
func (s *runsStore) ListActiveRunsAll(context.Context) ([]store.ListActiveRunsAllRow, error) {
	return s.activeRuns, nil
}
func (s *runsStore) CreateRunInput(context.Context, store.CreateRunInputParams) (store.RunUserInput, error) {
	return store.RunUserInput{}, nil
}
func (s *runsStore) CreateStopVerdictInput(context.Context, store.CreateStopVerdictInputParams) (store.RunUserInput, error) {
	return store.RunUserInput{}, nil
}

func newRunsHandler(t *testing.T, st workersvc.Store) *Handler {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &Handler{wsvc: workersvc.New(st, box, workersvc.Params{}), hub: hub.New()}
}

// runReq builds a GET request authenticated as user with a chi {id} route param.
func runReq(user store.User, runID uuid.UUID) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/runs/x", nil)
	ctx := mw.ContextWithUser(req.Context(), user)
	if runID != uuid.Nil {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

// inputReq builds a POST /runs/{id}/inputs authenticated as user, carrying a JSON
// steering body.
func inputReq(user store.User, runID uuid.UUID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/runs/x/inputs", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID.String())
	ctx := context.WithValue(mw.ContextWithUser(req.Context(), user), chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

func TestGetRunOwnerNonOwnerAdmin(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)

	// Owner sees.
	rec := httptest.NewRecorder()
	h.GetRun(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner GetRun = %d, want 200", rec.Code)
	}

	// Non-owner, non-admin: 404 (indistinguishable from unknown id).
	other := store.User{ID: uuid.New()}
	rec = httptest.NewRecorder()
	h.GetRun(rec, runReq(other, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner GetRun = %d, want 404", rec.Code)
	}

	// Admin sees any run.
	admin := store.User{ID: uuid.New(), IsAdmin: true}
	rec = httptest.NewRecorder()
	h.GetRun(rec, runReq(admin, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GetRun = %d, want 200 (admin sees all)", rec.Code)
	}
}

func TestListRunMessagesViewerAuthz(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{
		ownerID: owner.ID,
		run:     store.Run{ID: runID, UserID: owner.ID, Status: "running"},
		msgs:    []store.RunMessage{{Seq: 1, Kind: "text", Payload: []byte(`{}`)}},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.ListRunMessages(rec, runReq(owner, runID))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"seq":1`) {
		t.Fatalf("owner messages = %d %q, want 200 with seq 1", rec.Code, rec.Body.String())
	}

	other := store.User{ID: uuid.New()}
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReq(other, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner messages = %d, want 404", rec.Code)
	}

	admin := store.User{ID: uuid.New(), IsAdmin: true}
	rec = httptest.NewRecorder()
	h.ListRunMessages(rec, runReq(admin, runID))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin messages = %d, want 200", rec.Code)
	}
}

func TestCreateRunInputIsOwnerOnly(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)

	// Owner may steer their own run.
	rec := httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(owner, runID, `{"kind":"follow_up","body":"keep going"}`))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("owner steering = %d, want 202", rec.Code)
	}

	// A non-owner is denied (404, indistinguishable from an unknown run).
	rec = httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(store.User{ID: uuid.New()}, runID, `{"kind":"follow_up","body":"x"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner steering = %d, want 404", rec.Code)
	}

	// A non-owner ADMIN is ALSO denied: steering is owner-only, with no admin
	// bypass (admin visibility is read-only — reads + WS are owner-or-admin, but
	// approving/cancelling another user's run is not).
	rec = httptest.NewRecorder()
	h.CreateRunInput(rec, inputReq(store.User{ID: uuid.New(), IsAdmin: true}, runID, `{"kind":"cancel"}`))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner admin steering = %d, want 404 (steering is owner-only)", rec.Code)
	}
}

func TestListRunsReturnsUsersRuns(t *testing.T) {
	user := store.User{ID: uuid.New()}
	st := &runsStore{userRuns: []store.ListRunsForUserRow{
		{Run: store.Run{ID: uuid.New(), Status: "queued"}, RepoPath: "grp/repo", WorkerName: pgtype.Text{String: "laptop", Valid: true}},
	}}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "grp/repo") || !strings.Contains(rec.Body.String(), "laptop") {
		t.Fatalf("ListRuns body missing display fields: %q", rec.Body.String())
	}
	// No query params → both narrowings NULL (the unchanged full list).
	if st.lastRunsArg == nil || st.lastRunsArg.RepoID.Valid || st.lastRunsArg.IssueIid.Valid {
		t.Fatalf("unfiltered ListRuns must pass NULL narrowings, got %+v", st.lastRunsArg)
	}
	if st.lastRunsArg.UserID != user.ID {
		t.Fatalf("ListRuns must scope to the requesting user")
	}
}

func TestListRunsThreadsRepoIssueFilters(t *testing.T) {
	user := store.User{ID: uuid.New()}
	repoID := uuid.New()
	st := &runsStore{}
	h := newRunsHandler(t, st)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/runs?repo_id="+repoID.String()+"&issue_iid=42", nil)
	h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
	if rec.Code != http.StatusOK {
		t.Fatalf("ListRuns = %d, want 200", rec.Code)
	}
	if st.lastRunsArg == nil {
		t.Fatal("ListRunsForUser not called")
	}
	if !st.lastRunsArg.RepoID.Valid || uuid.UUID(st.lastRunsArg.RepoID.Bytes) != repoID {
		t.Fatalf("repo_id filter not threaded: %+v", st.lastRunsArg.RepoID)
	}
	if !st.lastRunsArg.IssueIid.Valid || st.lastRunsArg.IssueIid.Int64 != 42 {
		t.Fatalf("issue_iid filter not threaded: %+v", st.lastRunsArg.IssueIid)
	}
}

func TestListRunsRejectsMalformedFilters(t *testing.T) {
	user := store.User{ID: uuid.New()}
	for _, q := range []string{"?repo_id=not-a-uuid", "?issue_iid=abc"} {
		st := &runsStore{}
		h := newRunsHandler(t, st)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/runs"+q, nil)
		h.ListRuns(rec, req.WithContext(mw.ContextWithUser(req.Context(), user)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("ListRuns%s = %d, want 400", q, rec.Code)
		}
		if st.lastRunsArg != nil {
			t.Fatalf("a malformed filter must reject before hitting the store (%s)", q)
		}
	}
}

func TestAdminListWorkersAndRuns(t *testing.T) {
	st := &runsStore{
		allWorkers: []store.ListAllWorkersRow{
			{Worker: store.Worker{ID: uuid.New(), Name: "w1", Status: "online"}, Busy: true, OwnerEmail: "u@example.com"},
		},
		activeRuns: []store.ListActiveRunsAllRow{
			{Run: store.Run{ID: uuid.New(), Status: "running"}, RepoPath: "grp/repo", OwnerEmail: "u@example.com"},
		},
	}
	h := newRunsHandler(t, st)

	rec := httptest.NewRecorder()
	h.AdminListWorkers(rec, httptest.NewRequest(http.MethodGet, "/api/admin/workers", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "u@example.com") {
		t.Fatalf("AdminListWorkers = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.AdminListRuns(rec, httptest.NewRequest(http.MethodGet, "/api/admin/runs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "grp/repo") {
		t.Fatalf("AdminListRuns = %d %q", rec.Code, rec.Body.String())
	}
}

// wsTestServer wires ServeWS behind a middleware that injects user into context,
// standing in for RequireAuth so the WS authz + upgrade can be exercised end to
// end over a real (hijackable) connection.
func wsTestServer(h *Handler, user store.User) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeWS(w, r.WithContext(mw.ContextWithUser(r.Context(), user)))
	}))
}

func wsURL(t *testing.T, base string, runID uuid.UUID) string {
	t.Helper()
	return "ws" + strings.TrimPrefix(base, "http") + "/?run=" + runID.String()
}

func TestServeWSDeniesNonOwnerBeforeUpgrade(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)

	// A non-owner is refused at authz, before any upgrade — recorder never hijacks.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ws?run="+runID.String(), nil)
	h.ServeWS(rec, req.WithContext(mw.ContextWithUser(req.Context(), store.User{ID: uuid.New()})))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner ServeWS = %d, want 404 (denied before upgrade)", rec.Code)
	}
}

func TestServeWSDeliversLiveEventsToOwner(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)
	srv := wsTestServer(h, owner)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(t, srv.URL, runID), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.CloseNow()

	// Publish repeatedly until the read lands: the subscription is registered a
	// beat after the handshake, so a single early publish could race it.
	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				h.hub.PublishMessage(runID, 1, "text", "coder", json.RawMessage(`{"x":1}`), time.Now())
			}
		}
	}()
	defer close(stop)

	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	var ev hub.Event
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if ev.Type != "message" || ev.Seq != 1 {
		t.Fatalf("unexpected frame: %+v", ev)
	}
	c.Close(websocket.StatusNormalClosure, "")
}

func TestServeWSRejectsCrossOrigin(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	runID := uuid.New()
	st := &runsStore{ownerID: owner.ID, run: store.Run{ID: runID, UserID: owner.ID, Status: "running"}}
	h := newRunsHandler(t, st)
	srv := wsTestServer(h, owner)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// A mismatched Origin (the CSWSH case) must fail the handshake: coder/websocket
	// enforces Origin == Host by default, which ServeWS relies on.
	_, _, err := websocket.Dial(ctx, wsURL(t, srv.URL, runID), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": []string{"http://evil.example"}},
	})
	if err == nil {
		t.Fatal("cross-origin WS handshake must be rejected (CSWSH defense)")
	}
}
