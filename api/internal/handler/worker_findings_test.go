package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// workerFindingsStore answers the queries WorkerCreateFinding → Service.CreateFinding
// reaches; the embedded workersvc.Store panics on anything else. It captures the insert
// + disposition params so a test can assert the stored user/repo come from the RUN (not
// the client) and that a content_hash was computed.
type workerFindingsStore struct {
	workersvc.Store
	userID   uuid.UUID
	run      store.Run
	count    int64
	inserted *store.InsertFindingParams
	disp     *store.UpsertOpenDispositionParams

	// Disposition outcome knobs (default zero = a fresh `open` insert → notify). Set
	// upsertErr=pgx.ErrNoRows with reopenRows=0/updateRows=0 to model the SUPPRESSED
	// matching-hash re-report on a resolved coordinate (notify=false).
	upsertErr  error
	reopenRows int64
	updateRows int64
}

func (s *workerFindingsStore) GetRunByIDForUser(_ context.Context, arg store.GetRunByIDForUserParams) (store.Run, error) {
	if arg.ID == s.run.ID && arg.UserID == s.userID {
		return s.run, nil
	}
	return store.Run{}, pgx.ErrNoRows
}
func (s *workerFindingsStore) CountFindingsForRun(context.Context, uuid.UUID) (int64, error) {
	return s.count, nil
}
func (s *workerFindingsStore) InsertFinding(_ context.Context, arg store.InsertFindingParams) (store.IncidentalFinding, error) {
	s.inserted = &arg
	return store.IncidentalFinding{
		ID: uuid.New(), RunID: arg.RunID, UserID: arg.UserID, RepoID: arg.RepoID,
		Location: arg.Location, Title: arg.Title, DescriptionMd: arg.DescriptionMd,
		Labels: arg.Labels, Confidence: arg.Confidence,
	}, nil
}
func (s *workerFindingsStore) UpsertOpenDisposition(_ context.Context, arg store.UpsertOpenDispositionParams) (store.FindingDisposition, error) {
	s.disp = &arg
	if s.upsertErr != nil {
		return store.FindingDisposition{}, s.upsertErr
	}
	return store.FindingDisposition{}, nil // a fresh open row was inserted
}
func (s *workerFindingsStore) ReopenDispositionOnHashMismatch(context.Context, store.ReopenDispositionOnHashMismatchParams) (int64, error) {
	return s.reopenRows, nil
}
func (s *workerFindingsStore) UpdateDispositionLastTitle(context.Context, store.UpdateDispositionLastTitleParams) (int64, error) {
	return s.updateRows, nil
}

func findingRun(userID, repoID uuid.UUID) store.Run {
	return store.Run{
		ID:     uuid.New(),
		UserID: userID,
		RepoID: pgtype.UUID{Bytes: repoID, Valid: true},
		Status: "running",
		Kind:   "issue",
	}
}

func TestWorkerCreateFindingRequiresWorker(t *testing.T) {
	// No worker in context → 401, and the request never reaches the service.
	st := &workerFindingsStore{}
	h := newWorkerChatHandler(st)
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/x/findings", strings.NewReader(`{"title":"t","description":"d","location":"a/b.go#f"}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", uuid.New().String())
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.WorkerCreateFinding(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no worker = %d, want 401", rec.Code)
	}
	if st.inserted != nil {
		t.Error("no evidence must be inserted without a worker")
	}
}

func TestWorkerCreateFindingHappyPathDerivesFromRun(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := findingRun(uid, repoID)
	st := &workerFindingsStore{userID: uid, run: run}
	h := newWorkerChatHandler(st)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}

	// Note the client-side location drift the server canonicalises.
	body := `{"title":"Leaked ticker","description":"sweepLoop never Stops the ticker","location":"./api/internal/Sweep.go#sweepLoop","labels":["bug"],"confidence":"high"}`
	rec := httptest.NewRecorder()
	h.WorkerCreateFinding(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/findings", wkr, run.ID, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("create finding = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// Response is {"id": "<uuid>"}.
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, err := uuid.Parse(resp.ID); err != nil {
		t.Errorf("response id is not a uuid: %q", resp.ID)
	}

	if st.inserted == nil {
		t.Fatal("no evidence inserted")
	}
	// The stored row's user/repo come from the RUN, never a client value.
	if st.inserted.UserID != uid {
		t.Errorf("stored user_id = %v, want the run's %v", st.inserted.UserID, uid)
	}
	if st.inserted.RepoID != repoID {
		t.Errorf("stored repo_id = %v, want the run's %v", st.inserted.RepoID, repoID)
	}
	// location canonicalised server-side.
	if st.inserted.Location != "api/internal/sweep.go#sweeploop" {
		t.Errorf("stored location = %q, want the canonical form", st.inserted.Location)
	}
	// content_hash computed and passed to the disposition upsert.
	if st.disp == nil || st.disp.ContentHash == "" || len(st.disp.ContentHash) != 64 {
		t.Errorf("a 64-char content_hash must be computed and upserted, got %+v", st.disp)
	}
	if st.disp.LastTitle != "Leaked ticker" {
		t.Errorf("disposition last_title = %q, want the sanitised title", st.disp.LastTitle)
	}
}

func TestWorkerCreateFindingCapsAndErrors(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := findingRun(uid, repoID)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}
	base := func() *workerFindingsStore { return &workerFindingsStore{userID: uid, run: findingRun(uid, repoID)} }
	valid := `{"title":"t","description":"d","location":"a/b.go#f"}`

	cases := []struct {
		name string
		st   *workerFindingsStore
		body string
		id   uuid.UUID
		want int
	}{
		{"empty title 400", func() *workerFindingsStore { s := base(); s.run = run; return s }(), `{"title":"","description":"d","location":"a/b.go#f"}`, run.ID, http.StatusBadRequest},
		{"oversized title 400", func() *workerFindingsStore { s := base(); s.run = run; return s }(), `{"title":"` + strings.Repeat("x", 256) + `","description":"d","location":"a/b.go#f"}`, run.ID, http.StatusBadRequest},
		{"empty description 400", func() *workerFindingsStore { s := base(); s.run = run; return s }(), `{"title":"t","description":"","location":"a/b.go#f"}`, run.ID, http.StatusBadRequest},
		{"empty location 400", func() *workerFindingsStore { s := base(); s.run = run; return s }(), `{"title":"t","description":"d","location":""}`, run.ID, http.StatusBadRequest},
		{"too many labels 400", func() *workerFindingsStore { s := base(); s.run = run; return s }(), `{"title":"t","description":"d","location":"a/b.go#f","labels":` + manyLabels(21) + `}`, run.ID, http.StatusBadRequest},
		{"foreign run 404", func() *workerFindingsStore { s := base(); s.run = run; return s }(), valid, uuid.New(), http.StatusNotFound},
		{"cap reached 429", func() *workerFindingsStore { s := base(); s.run = run; s.count = workersvc.MaxFindingsPerRun; return s }(), valid, run.ID, http.StatusTooManyRequests},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			newWorkerChatHandler(c.st).WorkerCreateFinding(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/findings", wkr, c.id, c.body))
			if rec.Code != c.want {
				t.Fatalf("%s = %d, want %d; body=%s", c.name, rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

func manyLabels(n int) string {
	ls := make([]string, n)
	for i := range ls {
		ls[i] = "l"
	}
	b, _ := json.Marshal(ls)
	return string(b)
}

// notifyingSpyStore is a notifysvc.Store that records the InsertNotification calls the M3
// coalescing path makes, so the handler test can assert WorkerCreateFinding fires the
// notification when the finding opened/re-opened a coordinate and stays silent when it was
// suppressed. FindUnreadNotificationForRunKind always reports "no coalescible row" so the
// first (and only) finding takes the insert-and-DM branch. insertErr lets a test prove the
// notification failing does not fail the 200.
type notifyingSpyStore struct {
	inserts   int
	insertErr error
}

func (s *notifyingSpyStore) InsertNotification(_ context.Context, arg store.InsertNotificationParams) (store.Notification, error) {
	s.inserts++
	if s.insertErr != nil {
		return store.Notification{}, s.insertErr
	}
	return store.Notification{ID: uuid.New(), UserID: arg.UserID, Kind: arg.Kind, Payload: arg.Payload}, nil
}
func (s *notifyingSpyStore) PruneNotificationsForUser(context.Context, store.PruneNotificationsForUserParams) (int64, error) {
	return 0, nil
}
func (s *notifyingSpyStore) GetRunByID(context.Context, uuid.UUID) (store.Run, error) {
	return store.Run{}, nil
}
func (s *notifyingSpyStore) FindUnreadNotificationForRunKind(context.Context, store.FindUnreadNotificationForRunKindParams) (store.Notification, error) {
	return store.Notification{}, pgx.ErrNoRows
}
func (s *notifyingSpyStore) UpdateNotificationPayload(_ context.Context, arg store.UpdateNotificationPayloadParams) (store.Notification, error) {
	return store.Notification{ID: arg.ID, UserID: arg.UserID, Payload: arg.Payload}, nil
}

func newNotifyingWorkerHandler(st workersvc.Store, ns *notifyingSpyStore) *Handler {
	h := &Handler{wsvc: workersvc.New(st, nil, workersvc.Params{})}
	h.SetNotifier(notifysvc.New(ns, nil, 0, nil))
	return h
}

func TestWorkerCreateFindingFiresNotificationWhenOpened(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := findingRun(uid, repoID)
	st := &workerFindingsStore{userID: uid, run: run} // fresh open insert → notify
	ns := &notifyingSpyStore{}
	h := newNotifyingWorkerHandler(st, ns)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}

	body := `{"title":"Leaked ticker","description":"sweepLoop never Stops it","location":"a/b.go#f"}`
	rec := httptest.NewRecorder()
	h.WorkerCreateFinding(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/findings", wkr, run.ID, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("create finding = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if ns.inserts != 1 {
		t.Errorf("a newly-opened coordinate must fire exactly one notification, got %d", ns.inserts)
	}
}

func TestWorkerCreateFindingSuppressedDoesNotNotify(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := findingRun(uid, repoID)
	// Suppressed: the coordinate already exists (upsert conflict), the re-open matches 0
	// rows (identical hash) and the open-only refresh matches 0 (resolved) → notify=false.
	st := &workerFindingsStore{userID: uid, run: run, upsertErr: pgx.ErrNoRows, reopenRows: 0, updateRows: 0}
	ns := &notifyingSpyStore{}
	h := newNotifyingWorkerHandler(st, ns)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}

	body := `{"title":"Leaked ticker","description":"sweepLoop never Stops it","location":"a/b.go#f"}`
	rec := httptest.NewRecorder()
	h.WorkerCreateFinding(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/findings", wkr, run.ID, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("suppressed capture must still return 200 (the evidence is stored); got %d, body=%s", rec.Code, rec.Body.String())
	}
	if ns.inserts != 0 {
		t.Errorf("a suppressed matching-hash re-report must NOT notify (anti-nag, R2), got %d inserts", ns.inserts)
	}
}

func TestWorkerCreateFindingNotificationFailureDoesNotFail200(t *testing.T) {
	uid, repoID := uuid.New(), uuid.New()
	run := findingRun(uid, repoID)
	st := &workerFindingsStore{userID: uid, run: run}
	ns := &notifyingSpyStore{insertErr: errors.New("inbox down")}
	h := newNotifyingWorkerHandler(st, ns)
	wkr := store.Worker{ID: uuid.New(), UserID: uid}

	body := `{"title":"Leaked ticker","description":"sweepLoop never Stops it","location":"a/b.go#f"}`
	rec := httptest.NewRecorder()
	h.WorkerCreateFinding(rec, workerChatReq(http.MethodPost, "/api/worker/runs/x/findings", wkr, run.ID, body))
	if rec.Code != http.StatusOK {
		t.Fatalf("a notification failure must not fail the capture; got %d, body=%s", rec.Code, rec.Body.String())
	}
	if st.inserted == nil {
		t.Error("the finding must still be durably stored even when the notification fails")
	}
}
