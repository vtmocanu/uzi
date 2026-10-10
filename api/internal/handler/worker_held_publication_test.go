package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #2545 M2: the step-A route. The service is a fake (heldPublisher), so these tests pin the
// HTTP contract: header validation, the 413 cap, the order (the pre-body gate before the body is
// read), the reason-to-status mapping, and, through a REAL http.Server, the route's own
// deadlines.

const (
	heldTipHdr = "0123456789abcdef0123456789abcdef01234567"
	heldCovHdr = "89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
)

type fakeHeldSvc struct {
	mu       sync.Mutex
	slotOK   bool
	gate     func(ctx context.Context, req workersvc.HeldPublishRequest) (*workersvc.HeldPublishResult, error)
	publish  func(ctx context.Context, req workersvc.HeldPublishRequest, pack []byte) (workersvc.HeldPublishResult, error)
	gateN    int
	publishN int
	releases int
}

func newFakeHeldSvc() *fakeHeldSvc { return &fakeHeldSvc{slotOK: true} }

func (f *fakeHeldSvc) AcquireHeldPublishSlot() (func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.slotOK {
		return nil, false
	}
	return func() { f.mu.Lock(); f.releases++; f.mu.Unlock() }, true
}

func (f *fakeHeldSvc) HeldPublishGate(ctx context.Context, _ store.Worker, _ uuid.UUID, req workersvc.HeldPublishRequest) (*workersvc.HeldPublishResult, error) {
	f.mu.Lock()
	f.gateN++
	fn := f.gate
	f.mu.Unlock()
	if fn == nil {
		return nil, nil
	}
	return fn(ctx, req)
}

func (f *fakeHeldSvc) PublishHeld(ctx context.Context, _ store.Worker, _ uuid.UUID, req workersvc.HeldPublishRequest, pack []byte) (workersvc.HeldPublishResult, error) {
	f.mu.Lock()
	f.publishN++
	fn := f.publish
	f.mu.Unlock()
	if fn == nil {
		return workersvc.HeldPublishResult{}, errors.New("no publish fake")
	}
	return fn(ctx, req, pack)
}

func (f *fakeHeldSvc) calls() (gate, publish int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gateN, f.publishN
}

func heldHandler(f *fakeHeldSvc) *Handler { return &Handler{heldSvc: f} }

func heldReq(body io.Reader) *http.Request {
	req := reqWithWorkerAndParam(http.MethodPost, "/api/worker/runs/x/held-publication", body, uuid.New())
	req.Header.Set("X-Uzi-Held-Tip", heldTipHdr)
	req.Header.Set("X-Uzi-Held-Generation", "2")
	req.Header.Set("X-Uzi-Held-Coverage", heldCovHdr)
	return req
}

func okResult() workersvc.HeldPublishResult {
	return workersvc.HeldPublishResult{PublicationID: uuid.New(), Ref: "refs/uzi-held/r/2", Tip: heldTipHdr, State: "created"}
}

func TestWorkerRunHeldPublicationHeaderValidation(t *testing.T) {
	for name, mutate := range map[string]func(*http.Request){
		"no tip":             func(r *http.Request) { r.Header.Del("X-Uzi-Held-Tip") },
		"uppercase tip":      func(r *http.Request) { r.Header.Set("X-Uzi-Held-Tip", strings.ToUpper(heldTipHdr)) },
		"short tip":          func(r *http.Request) { r.Header.Set("X-Uzi-Held-Tip", "0123") },
		"48-hex tip":         func(r *http.Request) { r.Header.Set("X-Uzi-Held-Tip", strings.Repeat("a", 48)) },
		"no generation":      func(r *http.Request) { r.Header.Del("X-Uzi-Held-Generation") },
		"zero generation":    func(r *http.Request) { r.Header.Set("X-Uzi-Held-Generation", "0") },
		"negative gen":       func(r *http.Request) { r.Header.Set("X-Uzi-Held-Generation", "-3") },
		"non-numeric gen":    func(r *http.Request) { r.Header.Set("X-Uzi-Held-Generation", "two") },
		"no coverage":        func(r *http.Request) { r.Header.Del("X-Uzi-Held-Coverage") },
		"short coverage":     func(r *http.Request) { r.Header.Set("X-Uzi-Held-Coverage", heldTipHdr) },
		"uppercase coverage": func(r *http.Request) { r.Header.Set("X-Uzi-Held-Coverage", strings.ToUpper(heldCovHdr)) },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeHeldSvc()
			req := heldReq(strings.NewReader("pack"))
			mutate(req)
			rec := httptest.NewRecorder()
			heldHandler(f).WorkerRunHeldPublication(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %q", rec.Code, rec.Body.String())
			}
			if g, p := f.calls(); g+p != 0 {
				t.Fatalf("the service was reached with a bad header: gate=%d publish=%d", g, p)
			}
		})
	}
	t.Run("64-hex tip is accepted", func(t *testing.T) {
		f := newFakeHeldSvc()
		f.publish = func(_ context.Context, req workersvc.HeldPublishRequest, _ []byte) (workersvc.HeldPublishResult, error) {
			if len(req.Tip) != 64 {
				t.Errorf("tip = %q", req.Tip)
			}
			return okResult(), nil
		}
		req := heldReq(strings.NewReader("pack"))
		req.Header.Set("X-Uzi-Held-Tip", strings.Repeat("a", 64))
		rec := httptest.NewRecorder()
		heldHandler(f).WorkerRunHeldPublication(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
	})
}

func TestWorkerRunHeldPublicationNoWorker401(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("pack"))
	rec := httptest.NewRecorder()
	heldHandler(newFakeHeldSvc()).WorkerRunHeldPublication(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The body is capped at maxPackBytes: a stream past it is a truthful 413 and the service never
// sees a truncated pack.
func TestWorkerRunHeldPublicationOversize413(t *testing.T) {
	f := newFakeHeldSvc()
	rec := httptest.NewRecorder()
	heldHandler(f).WorkerRunHeldPublication(rec, heldReq(infReader{}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413, body %q", rec.Code, rec.Body.String())
	}
	if _, p := f.calls(); p != 0 {
		t.Fatalf("PublishHeld ran %d times on an over-cap body", p)
	}
}

// poisonReader fails the test when the body is read.
type poisonReader struct{ t *testing.T }

func (p poisonReader) Read([]byte) (int, error) {
	p.t.Helper()
	p.t.Error("the request body was read")
	return 0, io.EOF
}

// The pre-body gate decides first: a refusal is mapped to its status with the reason code, a
// reconcile-only answer is returned as a 200, and neither reads the body. A busy semaphore is a
// retryable 429 before even the gate.
func TestWorkerRunHeldPublicationGateOrder(t *testing.T) {
	refusal := func(reason string) error { return &workersvc.HeldRefusal{Reason: reason} }
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
		retry  bool
	}{
		{"not_failed", refusal(workersvc.HeldReasonNotFailed), http.StatusConflict, "not_failed", false},
		{"excluded_origin", refusal(workersvc.HeldReasonExcludedOrigin), http.StatusConflict, "excluded_origin", false},
		{"generation_mismatch", refusal(workersvc.HeldReasonGenerationMismatch), http.StatusConflict, "generation_mismatch", false},
		{"identity_changed", refusal(workersvc.HeldReasonIdentityChanged), http.StatusConflict, "identity_changed", false},
		{"unsupported", refusal(workersvc.HeldReasonUnsupported), http.StatusUnprocessableEntity, "unsupported", false},
		{"forge_unavailable", refusal(workersvc.HeldReasonForgeUnavailable), http.StatusServiceUnavailable, "forge_unavailable", true},
		{"foreign run", workersvc.ErrRunNotOwned, http.StatusNotFound, "", false},
		{"unexpected", errors.New("db down"), http.StatusInternalServerError, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeHeldSvc()
			f.gate = func(context.Context, workersvc.HeldPublishRequest) (*workersvc.HeldPublishResult, error) {
				return nil, tc.err
			}
			rec := httptest.NewRecorder()
			heldHandler(f).WorkerRunHeldPublication(rec, heldReq(poisonReader{t}))
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d, body %q", rec.Code, tc.status, rec.Body.String())
			}
			var body map[string]string
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			if body["reason"] != tc.reason {
				t.Fatalf("reason = %q, want %q (body %q)", body["reason"], tc.reason, rec.Body.String())
			}
			if (rec.Header().Get("Retry-After") != "") != tc.retry {
				t.Fatalf("Retry-After = %q, want set=%t", rec.Header().Get("Retry-After"), tc.retry)
			}
			if tc.status == http.StatusInternalServerError && strings.Contains(rec.Body.String(), "db down") {
				t.Fatalf("an internal error leaked: %q", rec.Body.String())
			}
			if _, p := f.calls(); p != 0 {
				t.Fatalf("PublishHeld ran after a refusing gate")
			}
		})
	}
	t.Run("reconcile-only answer", func(t *testing.T) {
		f := newFakeHeldSvc()
		want := workersvc.HeldPublishResult{PublicationID: uuid.New(), Ref: "refs/uzi-held/r/2", Tip: heldTipHdr, State: "invoked"}
		f.gate = func(context.Context, workersvc.HeldPublishRequest) (*workersvc.HeldPublishResult, error) {
			return &want, nil
		}
		rec := httptest.NewRecorder()
		heldHandler(f).WorkerRunHeldPublication(rec, heldReq(poisonReader{t}))
		var got apitypes.HeldPublicationResponse
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got.State != "invoked" ||
			got.PublicationID != want.PublicationID.String() || got.Ref != want.Ref || got.Tip != want.Tip {
			t.Fatalf("status %d body %q", rec.Code, rec.Body.String())
		}
		if _, p := f.calls(); p != 0 {
			t.Fatal("PublishHeld ran for a reconcile-only answer")
		}
	})
	t.Run("busy", func(t *testing.T) {
		f := newFakeHeldSvc()
		f.slotOK = false
		rec := httptest.NewRecorder()
		heldHandler(f).WorkerRunHeldPublication(rec, heldReq(poisonReader{t}))
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("status = %d, Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
		}
		if g, p := f.calls(); g+p != 0 {
			t.Fatalf("service reached while busy: gate=%d publish=%d", g, p)
		}
	})
}

// A full request: the service sees the validated headers and the exact body under a deadline of
// heldServiceBudget, the slot is released, and the ACK is the strict DTO.
func TestWorkerRunHeldPublicationSuccess(t *testing.T) {
	f := newFakeHeldSvc()
	want := okResult()
	var gotReq workersvc.HeldPublishRequest
	var gotPack []byte
	var deadline time.Duration
	f.publish = func(ctx context.Context, req workersvc.HeldPublishRequest, pack []byte) (workersvc.HeldPublishResult, error) {
		gotReq, gotPack = req, pack
		d, ok := ctx.Deadline()
		if !ok {
			t.Error("no service deadline")
		}
		deadline = time.Until(d)
		return want, nil
	}
	rec := httptest.NewRecorder()
	heldHandler(f).WorkerRunHeldPublication(rec, heldReq(bytes.NewReader([]byte("PACKDATA"))))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
	}
	if gotReq.Tip != heldTipHdr || gotReq.Generation != 2 || gotReq.Coverage != heldCovHdr || string(gotPack) != "PACKDATA" {
		t.Fatalf("service saw %+v %q", gotReq, gotPack)
	}
	if deadline <= 0 || deadline > heldServiceBudget {
		t.Fatalf("service deadline = %v, want within %v", deadline, heldServiceBudget)
	}
	dec := json.NewDecoder(rec.Body)
	dec.DisallowUnknownFields()
	var got apitypes.HeldPublicationResponse
	if err := dec.Decode(&got); err != nil || got != (apitypes.HeldPublicationResponse{PublicationID: want.PublicationID.String(), Ref: want.Ref, Tip: want.Tip, State: "created"}) {
		t.Fatalf("body %q: %+v %v", rec.Body.String(), got, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releases != 1 {
		t.Fatalf("slot releases = %d, want 1", f.releases)
	}
}

// ---- real transport: the route's deadlines against a server with short base timeouts -------

// scaleHeldBudgets divides the production budgets (upload 120 s : service 150 s : grace 30 s)
// by div, preserving the ratio, and returns the matching base listener timeout (production 15 s).
func scaleHeldBudgets(t *testing.T, div time.Duration) (base time.Duration) {
	t.Helper()
	u, s, g := heldUploadBudget, heldServiceBudget, heldResponseGrace
	heldUploadBudget, heldServiceBudget, heldResponseGrace = u/div, s/div, g/div
	t.Cleanup(func() { heldUploadBudget, heldServiceBudget, heldResponseGrace = u, s, g })
	return 15 * time.Second / div
}

func heldTransportServer(t *testing.T, f *fakeHeldSvc, base time.Duration) *httptest.Server {
	t.Helper()
	h := heldHandler(f)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()})))
		})
	})
	r.Post("/api/worker/runs/{id}/held-publication", h.WorkerRunHeldPublication)
	// A sibling worker route that does not extend its deadlines.
	r.Post("/api/worker/runs/{id}/sibling", func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		time.Sleep(base * 3)
		_, _ = w.Write([]byte("late"))
	})
	srv := httptest.NewUnstartedServer(r)
	srv.Config.ReadTimeout, srv.Config.WriteTimeout = base, base
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

func heldPost(t *testing.T, srv *httptest.Server, path string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/worker/runs/"+uuid.NewString()+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Uzi-Held-Tip", heldTipHdr)
	req.Header.Set("X-Uzi-Held-Generation", "2")
	req.Header.Set("X-Uzi-Held-Coverage", heldCovHdr)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	return client.Do(req)
}

// (a) A service call that outlives the base WriteTimeout still delivers its ACK, because the
// route extends its own write deadline; the sibling route keeps the base timeouts.
func TestWorkerRunHeldPublicationSlowPushOutlivesBaseTimeout(t *testing.T) {
	base := scaleHeldBudgets(t, 100) // 150 ms base; upload 1.2 s, service 1.5 s, grace 0.3 s
	f := newFakeHeldSvc()
	f.publish = func(ctx context.Context, _ workersvc.HeldPublishRequest, _ []byte) (workersvc.HeldPublishResult, error) {
		time.Sleep(base * 4) // 600 ms: past the base timeout, inside the service budget
		if ctx.Err() != nil {
			t.Errorf("service context ended early: %v", ctx.Err())
		}
		return okResult(), nil
	}
	srv := heldTransportServer(t, f, base)
	resp, err := heldPost(t, srv, "/held-publication", strings.NewReader("pack"))
	if err != nil {
		t.Fatalf("the ACK did not arrive: %v", err)
	}
	defer resp.Body.Close()
	var got apitypes.HeldPublicationResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil || got.State != "created" {
		t.Fatalf("status %d %+v", resp.StatusCode, got)
	}
	if resp, err := heldPost(t, srv, "/sibling", strings.NewReader("x")); err == nil {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if string(body) == "late" {
			t.Fatal("the sibling route outlived the base timeouts: the deadline extension leaked beyond its route")
		}
	}
}

// (b) The combined worst case: a client that trickles the body through most of the upload budget,
// then a slow, successful service call that uses most of the service budget. The upload read
// deadline passes while the service is still running; the request context must stay live (the
// service call is checked for it) and the ACK still fits the write deadline.
func TestWorkerRunHeldPublicationCombinedBudgets(t *testing.T) {
	base := scaleHeldBudgets(t, 100)
	f := newFakeHeldSvc()
	var svcStart time.Time
	f.publish = func(ctx context.Context, _ workersvc.HeldPublishRequest, pack []byte) (workersvc.HeldPublishResult, error) {
		svcStart = time.Now()
		time.Sleep(heldServiceBudget * 8 / 10)
		if err := ctx.Err(); err != nil {
			t.Errorf("service context ended during the slow call: %v", err)
			return workersvc.HeldPublishResult{}, err
		}
		if len(pack) != 10 {
			t.Errorf("pack = %d bytes, want 10", len(pack))
		}
		return okResult(), nil
	}
	srv := heldTransportServer(t, f, base)
	pr, pw := io.Pipe()
	start := time.Now()
	go func() {
		defer pw.Close()
		for i := 0; i < 10; i++ {
			_, _ = pw.Write([]byte("x"))
			time.Sleep(heldUploadBudget * 8 / 10 / 10)
		}
	}()
	resp, err := heldPost(t, srv, "/held-publication", pr)
	if err != nil {
		t.Fatalf("the ACK did not arrive after %v: %v", time.Since(start), err)
	}
	defer resp.Body.Close()
	var got apitypes.HeldPublicationResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&got) != nil || got.State != "created" {
		t.Fatalf("status %d %+v after %v", resp.StatusCode, got, time.Since(start))
	}
	if total := time.Since(start); total <= heldUploadBudget {
		t.Fatalf("test did not cross the upload budget (%v <= %v): it proves nothing", total, heldUploadBudget)
	}
	if upload := svcStart.Sub(start); upload <= base*3 {
		t.Fatalf("the upload took %v, not past the base timeout %v", upload, base)
	}
}

// An upload that stalls past the upload budget is cut off with a timeout, not held open.
func TestWorkerRunHeldPublicationStalledUploadTimesOut(t *testing.T) {
	base := scaleHeldBudgets(t, 100)
	f := newFakeHeldSvc()
	srv := heldTransportServer(t, f, base)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("x"))
		time.Sleep(heldUploadBudget * 3)
		pw.Close()
	}()
	start := time.Now()
	resp, err := heldPost(t, srv, "/held-publication", pr)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestTimeout {
			t.Fatalf("status = %d, want 408", resp.StatusCode)
		}
	}
	if elapsed := time.Since(start); elapsed > heldUploadBudget*2 {
		t.Fatalf("a stalled upload held the request open for %v (budget %v)", elapsed, heldUploadBudget)
	}
	if _, p := f.calls(); p != 0 {
		t.Fatal("PublishHeld ran for an incomplete upload")
	}
}
