package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// scalePublishBudgets divides the production budgets (upload 60 s : service 190 s : grace 15 s)
// by div, keeping the ratio, and returns the matching base listener timeout (httpx constants / div).
func scalePublishBudgets(t *testing.T, div time.Duration) (base time.Duration) {
	t.Helper()
	u, s, g := publishUploadBudget, publishServiceBudget, publishResponseGrace
	publishUploadBudget, publishServiceBudget, publishResponseGrace = u/div, s/div, g/div
	t.Cleanup(func() { publishUploadBudget, publishServiceBudget, publishResponseGrace = u, s, g })
	return httpx.ServerWriteTimeout / div
}

type fakePublisher struct {
	fn    func(ctx context.Context, call int) (workersvc.PublishResult, error)
	calls atomic.Int32
}

func (f *fakePublisher) PublishAttested(ctx context.Context, _ store.Worker, _ uuid.UUID, _ string, _ *string, _ []byte, _ *int64) (workersvc.PublishResult, error) {
	n := int(f.calls.Add(1))
	return f.fn(ctx, n)
}

func okPublish() (workersvc.PublishResult, error) {
	return workersvc.PublishResult{Published: true, Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
}

// publishTransport is a real server for the publish route over one protocol.
type publishTransport struct {
	srv         *httptest.Server
	client      *http.Client
	handlerDone chan struct{} // one send per WorkerRunPublish return
	major       int
}

func newPublishTransport(t *testing.T, h *Handler, h2 bool, base time.Duration) *publishTransport {
	t.Helper()
	pt := &publishTransport{handlerDone: make(chan struct{}, 16), major: 1}
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()})))
		})
	})
	r.Post("/api/worker/runs/{id}/publish", func(w http.ResponseWriter, req *http.Request) {
		defer func() { pt.handlerDone <- struct{}{} }()
		h.WorkerRunPublish(w, req)
	})
	// A sibling worker route that does not extend its deadlines.
	r.Post("/api/worker/runs/{id}/sibling", func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		time.Sleep(base * 3)
		_, _ = w.Write([]byte("late"))
	})
	pt.srv = httptest.NewUnstartedServer(r)
	pt.srv.Config.ReadTimeout = httpx.ServerReadTimeout / (httpx.ServerWriteTimeout / base)
	pt.srv.Config.WriteTimeout = base
	if h2 {
		pt.srv.EnableHTTP2 = true
		pt.srv.StartTLS()
		pt.major = 2
	} else {
		pt.srv.Start()
	}
	t.Cleanup(pt.srv.Close)
	c := *pt.srv.Client()
	c.Timeout = 30 * time.Second
	pt.client = &c
	return pt
}

func (pt *publishTransport) post(t *testing.T, path string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, pt.srv.URL+"/api/worker/runs/"+uuid.NewString()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Uzi-Checkpoint-Tip", validTip)
	resp, err := pt.client.Do(req)
	if err == nil && resp.ProtoMajor != pt.major {
		t.Fatalf("protocol major = %d, want %d", resp.ProtoMajor, pt.major)
	}
	return resp, err
}

func (pt *publishTransport) waitHandler(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-pt.handlerDone:
	case <-time.After(within):
		t.Fatalf("the handler did not return within %v", within)
	}
}

func eachTransport(t *testing.T, fn func(t *testing.T, h2 bool)) {
	t.Helper()
	t.Run("h2", func(t *testing.T) { fn(t, true) })
	t.Run("h1", func(t *testing.T) { fn(t, false) })
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func fakeHandler(f *fakePublisher) *Handler { return &Handler{publishSvc: f} }

// (1) The REAL workersvc path: a slow broker outliving the base write timeout still delivers its
// 200. Without the per-route deadline extension the base WriteTimeout cuts the answer.
func TestWorkerRunPublishDeadlineSlowBrokerRealService(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	eachTransport(t, func(t *testing.T, h2 bool) {
		box := newBox(t)
		sealed, err := box.Seal([]byte("pat"))
		if err != nil {
			t.Fatal(err)
		}
		st := &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}
		var ctxErr atomic.Value
		publish := func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
			time.Sleep(base * 4)
			if err := ctx.Err(); err != nil {
				ctxErr.Store(err)
			}
			return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
		}
		h := newPublishHandler(t, st, box, func(string) bool { return true }, publish)
		pt := newPublishTransport(t, h, h2, base)
		resp, err := pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("the answer did not arrive: %v", err)
		}
		var got map[string]any
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || json.Unmarshal([]byte(body), &got) != nil || got["published"] != true {
			t.Fatalf("status %d body %q", resp.StatusCode, body)
		}
		if v := ctxErr.Load(); v != nil {
			t.Fatalf("the service context ended early: %v", v)
		}
	})
}

// (2) A client that sends a few bytes and stalls past the upload budget ends the request
// without reaching the publisher.
func TestWorkerRunPublishDeadlineStalledUpload(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	upload := publishUploadBudget
	eachTransport(t, func(t *testing.T, h2 bool) {
		f := &fakePublisher{fn: func(context.Context, int) (workersvc.PublishResult, error) { return okPublish() }}
		pt := newPublishTransport(t, fakeHandler(f), h2, base)
		pr, pw := io.Pipe()
		t.Cleanup(func() { _ = pw.Close() })
		go func() { _, _ = pw.Write([]byte("abc")) }()
		start := time.Now()
		resp, err := pt.post(t, "/publish", pr)
		if err == nil {
			_ = readBody(t, resp)
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("a stalled upload answered 200")
			}
		}
		if el := time.Since(start); el > upload*10 {
			t.Fatalf("the request took %v, far past the upload budget %v", el, upload)
		}
		pt.waitHandler(t, 10*time.Second)
		if n := f.calls.Load(); n != 0 {
			t.Fatalf("the publisher was called %d times for a stalled upload", n)
		}
	})
}

// (3) Trickled upload through most of the upload budget, then a slow successful publisher using
// most of the service budget: the answer still fits the write deadline with a live context.
func TestWorkerRunPublishDeadlineCombinedBudgets(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	upload, service := publishUploadBudget, publishServiceBudget
	eachTransport(t, func(t *testing.T, h2 bool) {
		var ctxErr atomic.Value
		f := &fakePublisher{fn: func(ctx context.Context, _ int) (workersvc.PublishResult, error) {
			time.Sleep(service * 8 / 10)
			if err := ctx.Err(); err != nil {
				ctxErr.Store(err)
			}
			return okPublish()
		}}
		pt := newPublishTransport(t, fakeHandler(f), h2, base)
		pr, pw := io.Pipe()
		step := upload * 8 / 10 / 10
		go func() {
			defer func() { _ = pw.Close() }()
			for i := 0; i < 10; i++ {
				_, _ = pw.Write([]byte("x"))
				time.Sleep(step)
			}
		}()
		start := time.Now()
		resp, err := pt.post(t, "/publish", pr)
		if err != nil {
			t.Fatalf("the answer did not arrive after %v: %v", time.Since(start), err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"published":true`) {
			t.Fatalf("status %d body %q after %v", resp.StatusCode, body, time.Since(start))
		}
		if v := ctxErr.Load(); v != nil {
			t.Fatalf("the publisher context ended early: %v", v)
		}
		if total := time.Since(start); total <= upload {
			t.Fatalf("the test did not cross the upload budget (%v <= %v)", total, upload)
		}
	})
}

// (4) A cooperative publisher that outlives the service budget gets a cancelled context; the
// route answers 500 "internal error" and the handler returns.
func TestWorkerRunPublishDeadlineCooperativeOverBudget(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	eachTransport(t, func(t *testing.T, h2 bool) {
		f := &fakePublisher{fn: func(ctx context.Context, _ int) (workersvc.PublishResult, error) {
			<-ctx.Done()
			return workersvc.PublishResult{}, ctx.Err()
		}}
		pt := newPublishTransport(t, fakeHandler(f), h2, base)
		resp, err := pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "internal error") {
			t.Fatalf("status %d body %q", resp.StatusCode, body)
		}
		pt.waitHandler(t, 5*time.Second)
	})
}

// (5) A publisher that ignores its context still gets a clean 500 at the service deadline, and
// its goroutine finishes once released.
func TestWorkerRunPublishDeadlineContextIgnoringPublisher(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	eachTransport(t, func(t *testing.T, h2 bool) {
		release := make(chan struct{})
		finished := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		f := &fakePublisher{fn: func(context.Context, int) (workersvc.PublishResult, error) {
			defer close(finished)
			<-release
			return okPublish()
		}}
		pt := newPublishTransport(t, fakeHandler(f), h2, base)
		resp, err := pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("transport error instead of a 500: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "internal error") {
			t.Fatalf("status %d body %q", resp.StatusCode, body)
		}
		pt.waitHandler(t, 5*time.Second)
		select {
		case <-finished:
			t.Fatal("the publisher finished before it was released")
		default:
		}
		once.Do(func() { close(release) })
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("the publisher goroutine did not finish after release")
		}
	})
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func captureSlog(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// (6a) A panic before the deadline is a 500, is logged, and the server keeps serving.
func TestWorkerRunPublishDeadlinePanicBeforeDeadline(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	eachTransport(t, func(t *testing.T, h2 bool) {
		logs := captureSlog(t)
		f := &fakePublisher{fn: func(_ context.Context, call int) (workersvc.PublishResult, error) {
			if call == 1 {
				panic("boom")
			}
			return okPublish()
		}}
		pt := newPublishTransport(t, fakeHandler(f), h2, base)
		resp, err := pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "internal error") {
			t.Fatalf("status %d body %q", resp.StatusCode, body)
		}
		if out := logs.String(); !strings.Contains(out, "worker run publish panic") || !strings.Contains(out, "boom") {
			t.Fatalf("the panic was not logged: %q", out)
		}
		resp, err = pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("follow-up request failed: %v", err)
		}
		if body := readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("follow-up status %d body %q", resp.StatusCode, body)
		}
	})
}

// (6b) A panic after the handler already answered (a publisher that ignored the deadline) is
// recovered: the process survives and the goroutine ends.
func TestWorkerRunPublishDeadlinePanicAfterAnswer(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	eachTransport(t, func(t *testing.T, h2 bool) {
		logs := captureSlog(t)
		release := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(release) }) })
		f := &fakePublisher{fn: func(_ context.Context, call int) (workersvc.PublishResult, error) {
			if call == 1 {
				<-release
				panic("late boom")
			}
			return okPublish()
		}}
		pt := newPublishTransport(t, fakeHandler(f), h2, base)
		resp, err := pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("transport error: %v", err)
		}
		if _ = readBody(t, resp); resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status %d, want 500", resp.StatusCode)
		}
		pt.waitHandler(t, 5*time.Second)
		once.Do(func() { close(release) })
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(logs.String(), "late boom") {
			if time.Now().After(deadline) {
				t.Fatalf("the late panic was never recovered and logged: %q", logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		resp, err = pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("server did not survive the late panic: %v", err)
		}
		if _ = readBody(t, resp); resp.StatusCode != http.StatusOK {
			t.Fatalf("follow-up status %d", resp.StatusCode)
		}
	})
}

// (7) A sibling worker route on the same server keeps the base timeouts: the extension does not
// leak beyond /publish.
func TestWorkerRunPublishDeadlineSiblingKeepsBaseTimeout(t *testing.T) {
	base := scalePublishBudgets(t, 50)
	eachTransport(t, func(t *testing.T, h2 bool) {
		f := &fakePublisher{fn: func(context.Context, int) (workersvc.PublishResult, error) { return okPublish() }}
		pt := newPublishTransport(t, fakeHandler(f), h2, base)
		if resp, err := pt.post(t, "/sibling", strings.NewReader("x")); err == nil {
			if readBody(t, resp) == "late" {
				t.Fatal("the sibling route outlived the base timeouts")
			}
		}
	})
}
