package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// statelessPublishStore is publishStore with a fixed attempt id and no mutation, so many requests
// may run through one Service at once without a data race.
type statelessPublishStore struct {
	*publishStore
	attempt uuid.UUID
}

func (s *statelessPublishStore) RecordLiveCheckpointPublishAttempt(context.Context, store.RecordLiveCheckpointPublishAttemptParams) (uuid.UUID, error) {
	return s.attempt, nil
}

func (s *statelessPublishStore) MarkCheckpointPublishAttemptReady(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}

func (s *statelessPublishStore) DeleteCheckpointPublishAttempt(context.Context, uuid.UUID) (int64, error) {
	return 1, nil
}

// recordingBody counts the reads made of a request body.
type recordingBody struct {
	r     io.Reader
	reads atomic.Int64
}

func (b *recordingBody) Read(p []byte) (int, error) {
	b.reads.Add(1)
	return b.r.Read(p)
}

// publishAdmissionRig is the real WorkerRunPublish behind a router whose stand-in worker middleware
// takes the worker id from the X-Test-Worker header. The stubbed broker blocks until released.
type publishAdmissionRig struct {
	router  http.Handler
	stub    *publishStub
	entered chan struct{} // one send per publisher call that started
	gate    chan struct{} // closed to let blocked publishers return
	wg      sync.WaitGroup
}

func newPublishAdmissionRig(t *testing.T) *publishAdmissionRig {
	t.Helper()
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	st := &statelessPublishStore{
		publishStore: &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)},
		attempt:      uuid.New(),
	}
	rig := &publishAdmissionRig{entered: make(chan struct{}, 16), gate: make(chan struct{})}
	rig.stub = &publishStub{fn: func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
		rig.entered <- struct{}{}
		select {
		case <-rig.gate:
		case <-ctx.Done():
			return pushbroker.Result{}, ctx.Err()
		}
		return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
	}}
	h := newPublishHandler(t, st, box, func(u string) bool { return u == "https://gitlab.example.com" }, rig.stub.publish)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			id := uuid.MustParse(req.Header.Get("X-Test-Worker"))
			next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: id, UserID: uuid.New()})))
		})
	})
	r.Post("/api/worker/runs/{id}/publish", h.WorkerRunPublish)
	rig.router = r
	t.Cleanup(func() {
		rig.release()
		rig.wg.Wait()
	})
	return rig
}

func (rig *publishAdmissionRig) release() {
	select {
	case <-rig.gate:
	default:
		close(rig.gate)
	}
}

func publishAdmissionReq(worker uuid.UUID, body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/worker/runs/"+uuid.NewString()+"/publish", body)
	req.Header.Set("X-Test-Worker", worker.String())
	req.Header.Set("X-Uzi-Checkpoint-Tip", validTip)
	return req
}

// start sends a request in the background and delivers its recorder when it finishes.
func (rig *publishAdmissionRig) start(worker uuid.UUID) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	rig.wg.Add(1)
	go func() {
		defer rig.wg.Done()
		rec := httptest.NewRecorder()
		rig.router.ServeHTTP(rec, publishAdmissionReq(worker, strings.NewReader("pack")))
		done <- rec
	}()
	return done
}

func (rig *publishAdmissionRig) waitEntered(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-rig.entered:
		case <-time.After(10 * time.Second):
			t.Fatalf("only %d of %d publishers started", i, n)
		}
	}
}

func waitRecorder(t *testing.T, ch <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rec := <-ch:
		return rec
	case <-time.After(10 * time.Second):
		t.Fatal("request did not finish")
		return nil
	}
}

// A worker at its share is refused with 429 busy before its body is read or the publisher runs; a
// second worker is still served; after a release the first worker is served again.
func TestWorkerRunPublishAdmissionPerWorkerShare(t *testing.T) {
	rig := newPublishAdmissionRig(t)
	workerA, workerB := uuid.New(), uuid.New()

	held := []<-chan *httptest.ResponseRecorder{rig.start(workerA), rig.start(workerA)}
	rig.waitEntered(t, 2)

	// Worker A's third request: refused, body untouched, publisher not called.
	body := &recordingBody{r: strings.NewReader("pack")}
	rec := httptest.NewRecorder()
	rig.router.ServeHTTP(rec, publishAdmissionReq(workerA, body))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}
	if !strings.Contains(rec.Body.String(), `"busy"`) {
		t.Fatalf("body %q does not carry reason busy", rec.Body.String())
	}
	if n := body.reads.Load(); n != 0 {
		t.Fatalf("the refused request's body was read %d times", n)
	}
	if n := rig.stub.calls.Load(); n != 2 {
		t.Fatalf("publisher calls = %d, want 2 (the refused request must not reach it)", n)
	}

	// Worker B is unaffected: the global cap still has room.
	bDone := rig.start(workerB)
	rig.waitEntered(t, 1)

	// Let everything finish; worker A is admitted again.
	rig.release()
	for _, ch := range append(held, bDone) {
		if rec := waitRecorder(t, ch); rec.Code != http.StatusOK {
			t.Fatalf("held request status = %d, body %q", rec.Code, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	rig.router.ServeHTTP(rec, publishAdmissionReq(workerA, strings.NewReader("pack")))
	if rec.Code != http.StatusOK {
		t.Fatalf("after release: status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
}

// A worker naming a run it does not hold gets PublishAttested's not-owned answer (404) from the
// pre-body ownership gate: no body read, no slot taken (the worker's whole share and every
// process-wide slot stay acquirable afterwards), and the publisher never runs.
func TestWorkerRunPublishUnownedRunTakesNoSlotAndReadsNoBody(t *testing.T) {
	stub := &publishStub{}
	h := newPublishHandler(t, &publishStore{ownedErr: pgx.ErrNoRows}, newBox(t), func(string) bool { return true }, stub.publish)
	workerID := uuid.New()
	// Hold the worker's whole share first: a gate that ran after admission would answer 429 busy
	// instead of the not-owned 404.
	var share []func()
	for i := 0; i < 2; i++ {
		rel, ok := h.wsvc.AcquireCheckpointPublishSlot(workerID)
		if !ok {
			t.Fatalf("pre-acquire %d for the worker was refused", i)
		}
		share = append(share, rel)
	}
	body := &recordingBody{r: strings.NewReader("pack")}
	for i := 0; i < 8; i++ { // more attempts than the per-worker share and the global cap together
		runID := uuid.New()
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		req := httptest.NewRequest(http.MethodPost, "/api/worker/x", body)
		req.Header.Set("X-Uzi-Checkpoint-Tip", validTip)
		ctx := mw.ContextWithWorker(req.Context(), store.Worker{ID: workerID, UserID: uuid.New()})
		req = req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.WorkerRunPublish(rec, req)
		if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "run not found for this worker") {
			t.Fatalf("attempt %d: status = %d body %q, want the not-owned 404", i, rec.Code, rec.Body.String())
		}
	}
	if n := body.reads.Load(); n != 0 {
		t.Fatalf("the unowned requests' body was read %d times", n)
	}
	if n := stub.calls.Load(); n != 0 {
		t.Fatalf("the publisher ran %d times for an unowned run", n)
	}
	for _, rel := range share {
		rel()
	}
	// The worker's full share is still available, and so is the rest of the process-wide cap.
	var releases []func()
	for i := 0; i < 2; i++ {
		rel, ok := h.wsvc.AcquireCheckpointPublishSlot(workerID)
		if !ok {
			t.Fatalf("acquire %d for the worker was refused: an unowned request held a slot", i)
		}
		releases = append(releases, rel)
	}
	for i := 0; i < 2; i++ {
		rel, ok := h.wsvc.AcquireCheckpointPublishSlot(uuid.New())
		if !ok {
			t.Fatalf("acquire %d for another worker was refused: the process-wide cap was consumed", i)
		}
		releases = append(releases, rel)
	}
	for _, rel := range releases {
		rel()
	}
}

// Four in-flight publishes from distinct workers exhaust the global cap: a fifth worker gets 429.
func TestWorkerRunPublishAdmissionGlobalCap(t *testing.T) {
	rig := newPublishAdmissionRig(t)
	var held []<-chan *httptest.ResponseRecorder
	for i := 0; i < 4; i++ {
		held = append(held, rig.start(uuid.New()))
	}
	rig.waitEntered(t, 4)

	body := &recordingBody{r: strings.NewReader("pack")}
	rec := httptest.NewRecorder()
	rig.router.ServeHTTP(rec, publishAdmissionReq(uuid.New(), body))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("status = %d Retry-After %q, want 429 and 5", rec.Code, rec.Header().Get("Retry-After"))
	}
	if body.reads.Load() != 0 || rig.stub.calls.Load() != 4 {
		t.Fatalf("refused request reached the body (%d reads) or publisher (%d calls)", body.reads.Load(), rig.stub.calls.Load())
	}
	rig.release()
	for _, ch := range held {
		waitRecorder(t, ch)
	}
}
