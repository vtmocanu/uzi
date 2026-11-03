package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/pushbroker"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The /publish route extends its own deadlines past the base listeners' 15 s (see
// worker_publish_deadline.go). These tests run the real handler and a stubbed broker through a REAL
// http.Server with short base timeouts, over HTTP/1.1 and HTTP/2, the held-publication precedent.

// scalePublishBudgets divides the four production budgets by div, preserving the ratio, and
// returns the matching base listener timeout (production 15 s).
func scalePublishBudgets(t *testing.T, div time.Duration) (base time.Duration) {
	t.Helper()
	u, c, s, g := publishUploadBudget, publishContextBudget, publishServiceBound, publishResponseGrace
	publishUploadBudget, publishContextBudget, publishServiceBound, publishResponseGrace = u/div, c/div, s/div, g/div
	t.Cleanup(func() {
		publishUploadBudget, publishContextBudget, publishServiceBound, publishResponseGrace = u, c, s, g
	})
	return 15 * time.Second / div
}

// publishStub is the broker stub plus its call counter.
type publishStub struct {
	calls atomic.Int64
	fn    func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error)
}

func (p *publishStub) publish(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
	p.calls.Add(1)
	if p.fn == nil {
		return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
	}
	return p.fn(ctx, o)
}

type publishTransport struct {
	srv    *httptest.Server
	client *http.Client
	proto  int
}

// publishTransportServer serves WorkerRunPublish on a chi router (worker injected like the real
// middleware) next to a sibling route that does not extend its deadlines. http2 selects TLS with
// HTTP/2 and the server's own client; otherwise plain HTTP/1.1 without keep-alives.
func publishTransportServer(t *testing.T, http2 bool, base time.Duration, stub *publishStub) *publishTransport {
	t.Helper()
	box := newBox(t)
	sealed, err := box.Seal([]byte("pat"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	st := &publishStore{ownedRun: issueRun(42), claim: issueClaimRow(sealed)}
	h := newPublishHandler(t, st, box, func(u string) bool { return u == "https://gitlab.example.com" }, stub.publish)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()})))
		})
	})
	r.Post("/api/worker/runs/{id}/publish", h.WorkerRunPublish)
	r.Post("/api/worker/runs/{id}/sibling", func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		time.Sleep(base * 3)
		_, _ = w.Write([]byte("late"))
	})
	srv := httptest.NewUnstartedServer(r)
	srv.Config.ReadHeaderTimeout = 10 * time.Second / (15 * time.Second / base)
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = base, base
	pt := &publishTransport{srv: srv}
	if http2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
		pt.client = srv.Client()
		pt.client.Timeout = 30 * time.Second
		pt.proto = 2
	} else {
		srv.Start()
		pt.client = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
		pt.proto = 1
	}
	t.Cleanup(srv.Close)
	return pt
}

func (pt *publishTransport) post(t *testing.T, path string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, pt.srv.URL+"/api/worker/runs/"+uuid.NewString()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Uzi-Checkpoint-Tip", validTip)
	return pt.client.Do(req)
}

// eachProto runs fn as an HTTP/1.1 and an HTTP/2 subtest.
func eachProto(t *testing.T, fn func(t *testing.T, http2 bool)) {
	t.Helper()
	t.Run("http1", func(t *testing.T) { fn(t, false) })
	t.Run("http2", func(t *testing.T) { fn(t, true) })
}

func wantProto(t *testing.T, resp *http.Response, pt *publishTransport) {
	t.Helper()
	if resp.ProtoMajor != pt.proto {
		t.Fatalf("protocol = HTTP/%d, want HTTP/%d", resp.ProtoMajor, pt.proto)
	}
}

func wantPublished(t *testing.T, resp *http.Response, after time.Duration) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var got map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(body, &got) != nil || got["published"] != true {
		t.Fatalf("status %d body %q after %v", resp.StatusCode, body, after)
	}
}

// 1: a service call that outlives the base WriteTimeout still delivers its answer.
func TestWorkerRunPublishSlowServiceOutlivesBaseTimeout(t *testing.T) {
	eachProto(t, func(t *testing.T, http2 bool) {
		base := scalePublishBudgets(t, 100) // base 150 ms; upload 0.6 s, context 2.1 s, grace 0.15 s
		stub := &publishStub{}
		stub.fn = func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
			time.Sleep(base * 4) // 600 ms: past the base timeout, inside the context budget
			if ctx.Err() != nil {
				t.Errorf("service context ended early: %v", ctx.Err())
			}
			return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
		}
		pt := publishTransportServer(t, http2, base, stub)
		start := time.Now()
		resp, err := pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("the answer did not arrive: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		wantProto(t, resp, pt)
		wantPublished(t, resp, time.Since(start))
	})
}

// 2: a publisher that outlives the context budget is cancelled; the client gets a 500 before the
// write deadline and no goroutine is left behind.
func TestWorkerRunPublishOverBudgetCancelsService(t *testing.T) {
	eachProto(t, func(t *testing.T, http2 bool) {
		base := scalePublishBudgets(t, 100)
		var slow atomic.Bool
		returned := make(chan error, 1)
		stub := &publishStub{}
		stub.fn = func(ctx context.Context, _ pushbroker.Options) (pushbroker.Result, error) {
			if !slow.Load() {
				return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
			}
			<-ctx.Done()
			returned <- ctx.Err()
			return pushbroker.Result{}, ctx.Err()
		}
		pt := publishTransportServer(t, http2, base, stub)
		// Warm up so the connection goroutines are part of the baseline.
		resp, err := pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("warm-up: %v", err)
		}
		_ = resp.Body.Close()
		baseline := runtime.NumGoroutine()
		slow.Store(true)

		writeDeadline := publishUploadBudget + publishServiceBound + publishResponseGrace
		start := time.Now()
		resp, err = pt.post(t, "/publish", strings.NewReader("pack"))
		if err != nil {
			t.Fatalf("no answer after %v: %v", time.Since(start), err)
		}
		defer func() { _ = resp.Body.Close() }()
		wantProto(t, resp, pt)
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		if elapsed := time.Since(start); elapsed >= writeDeadline {
			t.Fatalf("answer after %v, not before the write deadline %v", elapsed, writeDeadline)
		}
		select {
		case err := <-returned:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("publisher context ended with %v, want a deadline", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the publisher never saw its context end")
		}
		pt.client.CloseIdleConnections()
		deadline := time.Now().Add(5 * time.Second)
		for runtime.NumGoroutine() > baseline+3 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > baseline+3 {
			t.Fatalf("goroutines = %d, baseline %d: the request left goroutines behind", n, baseline)
		}
	})
}

// 3: the deadline extension does not leak to a sibling route on the same server.
func TestWorkerRunPublishSiblingKeepsBaseTimeouts(t *testing.T) {
	eachProto(t, func(t *testing.T, http2 bool) {
		base := scalePublishBudgets(t, 100)
		pt := publishTransportServer(t, http2, base, &publishStub{})
		resp, err := pt.post(t, "/sibling", strings.NewReader("x"))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if string(body) == "late" {
				t.Fatal("the sibling route outlived the base timeouts: the deadline extension leaked beyond its route")
			}
		}
	})
}

// 4: an upload that stalls past the upload budget is cut off, never published.
func TestWorkerRunPublishStalledUploadTimesOut(t *testing.T) {
	eachProto(t, func(t *testing.T, http2 bool) {
		base := scalePublishBudgets(t, 100)
		stub := &publishStub{}
		pt := publishTransportServer(t, http2, base, stub)
		pr, pw := io.Pipe()
		stall := publishUploadBudget * 3 // captured before the goroutine: the cleanup restores the var
		t.Cleanup(func() { _ = pw.Close() })
		go func() {
			_, _ = pw.Write([]byte("x"))
			time.Sleep(stall)
			_ = pw.Close()
		}()
		start := time.Now()
		resp, err := pt.post(t, "/publish", pr)
		if err == nil {
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusRequestTimeout {
				t.Fatalf("status = %d, want 408", resp.StatusCode)
			}
		} else if http2 {
			t.Fatalf("no 408 over HTTP/2: %v", err)
		}
		if elapsed := time.Since(start); elapsed > publishUploadBudget*2 {
			t.Fatalf("a stalled upload held the request open for %v (budget %v)", elapsed, publishUploadBudget)
		}
		if n := stub.calls.Load(); n != 0 {
			t.Fatalf("the publisher ran %d times for an incomplete upload", n)
		}
	})
}

// 5: the combined worst case: a body trickled through most of the upload budget, then a service
// call that outlives the upload deadline and the base timeouts. The read deadline passes while the
// service runs; the context must stay live and the answer still fit the write deadline.
func TestWorkerRunPublishCombinedTrickleAndSlowService(t *testing.T) {
	eachProto(t, func(t *testing.T, http2 bool) {
		base := scalePublishBudgets(t, 100)
		stub := &publishStub{}
		upload := publishUploadBudget
		stub.fn = func(ctx context.Context, o pushbroker.Options) (pushbroker.Result, error) {
			time.Sleep(upload) // past the upload deadline, inside the context budget
			if err := ctx.Err(); err != nil {
				t.Errorf("service context ended during the slow call: %v", err)
				return pushbroker.Result{}, err
			}
			if len(o.Pack) != 10 {
				t.Errorf("pack = %d bytes, want 10", len(o.Pack))
			}
			return pushbroker.Result{Ref: "refs/uzi-checkpoints/agent/issue-42"}, nil
		}
		pt := publishTransportServer(t, http2, base, stub)
		pr, pw := io.Pipe()
		step := upload * 8 / 10 / 10 // captured before the goroutine
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
		defer func() { _ = resp.Body.Close() }()
		wantProto(t, resp, pt)
		elapsed := time.Since(start)
		wantPublished(t, resp, elapsed)
		if elapsed <= upload {
			t.Fatalf("test did not cross the upload budget (%v <= %v): it proves nothing", elapsed, upload)
		}
	})
}

type publishErrReader struct{ err error }

func (e publishErrReader) Read([]byte) (int, error) { return 0, e.err }

type publishTimeoutErr struct{}

func (publishTimeoutErr) Error() string   { return "i/o timeout" }
func (publishTimeoutErr) Timeout() bool   { return true }
func (publishTimeoutErr) Temporary() bool { return true }

// 6: the body-read error mapping. A deadline is a 408, any other read failure stays a 400, the
// over-cap case stays a 413, and the publisher is never reached after a failed read.
func TestWorkerRunPublishBodyReadErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		body io.Reader
		want int
	}{
		{"deadline", publishErrReader{os.ErrDeadlineExceeded}, http.StatusRequestTimeout},
		{"wrapped deadline", publishErrReader{errors.Join(errors.New("read"), os.ErrDeadlineExceeded)}, http.StatusRequestTimeout},
		{"net timeout", publishErrReader{publishTimeoutErr{}}, http.StatusRequestTimeout},
		{"other", publishErrReader{errors.New("connection reset")}, http.StatusBadRequest},
		{"too large", infReader{}, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &publishStub{}
			h := newPublishHandler(t, &publishStore{}, newBox(t), func(string) bool { return true }, stub.publish)
			rec := httptest.NewRecorder()
			h.WorkerRunPublish(rec, publishReq(uuid.New(), validTip, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %q", rec.Code, tc.want, rec.Body.String())
			}
			if n := stub.calls.Load(); n != 0 {
				t.Fatalf("the publisher ran %d times after a failed read", n)
			}
		})
	}
}

// The api write deadline the agent's request allowance is sized against lives in a shared fixture
// (fixtures/checkpoint-publish-budget.json); the agent's own test asserts its side.
func TestPublishBudgetFixture(t *testing.T) {
	raw, err := os.ReadFile("../../../fixtures/checkpoint-publish-budget.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		APIWriteDeadlineMS int64 `json:"api_write_deadline_ms"`
		WorkerAllowanceMS  int64 `json:"worker_allowance_ms"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	got := (publishUploadBudget + publishServiceBound + publishResponseGrace).Milliseconds()
	if got != fx.APIWriteDeadlineMS {
		t.Fatalf("api write deadline = %d ms, fixture api_write_deadline_ms = %d", got, fx.APIWriteDeadlineMS)
	}
	if fx.WorkerAllowanceMS <= got {
		t.Fatalf("worker_allowance_ms = %d, must exceed the api write deadline %d", fx.WorkerAllowanceMS, got)
	}
}
