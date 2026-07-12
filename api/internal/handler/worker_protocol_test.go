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
	"gitlab.example.com/vtmocanu/uzi/api/internal/secretbox"
	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// protocolStore is a minimal workersvc.Store for the worker-protocol HTTP tests:
// it embeds the interface (unused methods panic) and overrides only the queries
// the tested paths reach.
type protocolStore struct {
	workersvc.Store
	claimErr      error
	ownedRun      store.Run
	ownedErr      error
	completedRows int64
}

func (p *protocolStore) ClaimRun(context.Context, store.ClaimRunParams) (store.Run, error) {
	return store.Run{}, p.claimErr
}
func (p *protocolStore) GetRunOwnedByWorker(context.Context, store.GetRunOwnedByWorkerParams) (store.Run, error) {
	return p.ownedRun, p.ownedErr
}
func (p *protocolStore) SetRunCompleted(context.Context, store.SetRunCompletedParams) (int64, error) {
	return p.completedRows, nil
}

// Register path: orphan recovery + the online transition. The counts are
// irrelevant to the wire-decode test, so they return 0/empty.
func (p *protocolStore) FailWorkerRunsOverCap(context.Context, store.FailWorkerRunsOverCapParams) ([]uuid.UUID, error) {
	return nil, nil
}
func (p *protocolStore) RequeueWorkerRuns(context.Context, store.RequeueWorkerRunsParams) (int64, error) {
	return 0, nil
}
func (p *protocolStore) RegisterWorker(_ context.Context, arg store.RegisterWorkerParams) (store.Worker, error) {
	return store.Worker{ID: arg.ID, Status: "online", Version: arg.Version, TemplateReported: arg.TemplateReported, MaxConcurrentRuns: arg.MaxConcurrentRuns}, nil
}

func newProtocolHandler(t *testing.T, st workersvc.Store) *Handler {
	t.Helper()
	box, err := secretbox.New(make([]byte, secretbox.KeySize))
	if err != nil {
		t.Fatalf("new box: %v", err)
	}
	return &Handler{wsvc: workersvc.New(st, box, workersvc.Params{})}
}

// workerCtx builds a request carrying an authenticated worker plus a chi {id}
// route param, the way RequireWorker + the router would.
func workerReq(method, body string, runID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, "/api/worker/x", strings.NewReader(body))
	ctx := mw.ContextWithWorker(req.Context(), store.Worker{ID: uuid.New(), UserID: uuid.New()})
	if runID != uuid.Nil {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", runID.String())
		ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	}
	return req.WithContext(ctx)
}

func TestWorkerClaimIdleReturns204NoBody(t *testing.T) {
	h := newProtocolHandler(t, &protocolStore{claimErr: pgx.ErrNoRows})
	rec := httptest.NewRecorder()
	h.WorkerClaim(rec, workerReq(http.MethodPost, "", uuid.Nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("204 must have an empty body, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterAcceptsNameField(t *testing.T) {
	// The M2 worker announces {name, version}; DecodeJSON rejects unknown fields,
	// so register must declare name (accepted, ignored) or every worker 400s on
	// register and never comes online. Posts the worker's exact body.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (register must accept the name field), body %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerRegisterReadsTemplate(t *testing.T) {
	// PRD #18: unlike name, the template field IS read and persisted as
	// template_reported, and echoed back in the worker DTO. DecodeJSON must accept
	// it (no 400) and the value must round-trip.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","template":"jvm"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"template_reported":"jvm"`) {
		t.Fatalf("expected template_reported=jvm in DTO, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterReadsMaxConcurrentRuns(t *testing.T) {
	// PRD #42: the worker advertises its concurrency cap. Register must accept it
	// (no 400 from the unknown-field-rejecting decoder) and round-trip it into the
	// DTO as max_concurrent_runs so the fleet UI can render "N/M runs".
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","max_concurrent_runs":2}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":2`) {
		t.Fatalf("expected max_concurrent_runs=2 in DTO, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterOmitsMaxConcurrentRuns(t *testing.T) {
	// An older image (and every M3a worker) sends no cap ⇒ max_concurrent_runs is
	// null in the DTO, never 0.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":null`) {
		t.Fatalf("absent cap must be null, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterDropsOutOfRangeMaxConcurrentRuns(t *testing.T) {
	// A garbled/hostile worker reports a cap outside the sane [1, 256] band (zero,
	// negative, or absurdly large). Register must still succeed (a soft observability
	// field never wedges the register-retry loop) but the nonsensical value must not
	// reach the DB/UI — dropped to null, so it never flows into the "N/M runs" math.
	for _, body := range []string{
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":0}`,
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":-3}`,
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":100000}`,
		`{"name":"laptop","version":"1.2.3","max_concurrent_runs":257}`,
	} {
		h := newProtocolHandler(t, &protocolStore{})
		rec := httptest.NewRecorder()
		h.WorkerRegister(rec, workerReq(http.MethodPost, body, uuid.Nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a bad cap must not fail register) for %s, body %q", rec.Code, body, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":null`) {
			t.Fatalf("out-of-range cap %s must be dropped to null, got %q", body, rec.Body.String())
		}
	}
}

func TestWorkerRegisterAcceptsCeilingMaxConcurrentRuns(t *testing.T) {
	// The ceiling value itself is in-band and must round-trip (boundary check that
	// the guard rejects 257 but not 256).
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","max_concurrent_runs":256}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"max_concurrent_runs":256`) {
		t.Fatalf("ceiling cap 256 must be accepted, got %q", rec.Body.String())
	}
}

func TestSanitizeSelfReported(t *testing.T) {
	// Trims, strips control chars, caps length.
	if got := sanitizeSelfReported("  1.2.3\x07-m4 \n ", maxSelfReportedBytes); got != "1.2.3-m4" {
		t.Fatalf("sanitizeSelfReported = %q, want %q", got, "1.2.3-m4")
	}
	long := strings.Repeat("a", 100)
	if got := sanitizeSelfReported(long, maxSelfReportedBytes); len(got) > maxSelfReportedBytes+4 {
		t.Fatalf("sanitizeSelfReported did not cap length: got %d bytes", len(got))
	}
	if got := sanitizeSelfReported("", maxSelfReportedBytes); got != "" {
		t.Fatalf("empty in must stay empty, got %q", got)
	}
}

func TestWorkerRegisterSanitizesVersion(t *testing.T) {
	// A hostile worker smuggles a control char (terminal escape) in `version`.
	// Register succeeds and the persisted/echoed version is stripped clean.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, "{\"name\":\"laptop\",\"version\":\"1.2.3\\u0007evil\"}", uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body %q", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "\x07") {
		t.Fatalf("control char must be stripped from version, got %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"version":"1.2.3evil"`) {
		t.Fatalf("expected sanitized version 1.2.3evil, got %q", rec.Body.String())
	}
}

func TestWorkerRegisterDropsMalformedTemplate(t *testing.T) {
	// A hostile/misconfigured worker sends junk in `template`. Register must still
	// succeed (a soft field never wedges the register-retry loop) but the malformed
	// value must NOT reach the DB/UI — it is dropped, so template_reported is null.
	h := newProtocolHandler(t, &protocolStore{})
	rec := httptest.NewRecorder()
	h.WorkerRegister(rec, workerReq(http.MethodPost, `{"name":"laptop","version":"1.2.3","template":"../../etc/passwd"}`, uuid.Nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (malformed template must not fail register), body %q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"template_reported":null`) {
		t.Fatalf("malformed template must be dropped to null, got %q", rec.Body.String())
	}
}

func TestWorkerStateAlreadyTerminalReturns409(t *testing.T) {
	runID := uuid.New()
	// Owned run is cancelled; the guarded completed-update touches 0 rows.
	h := newProtocolHandler(t, &protocolStore{
		ownedRun:      store.Run{ID: runID, Status: "cancelled"},
		completedRows: 0,
	})
	rec := httptest.NewRecorder()
	h.WorkerRunState(rec, workerReq(http.MethodPost, `{"status":"completed"}`, runID))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (already-terminal)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cancelled") {
		t.Fatalf("409 body should echo the run's real status, got %q", rec.Body.String())
	}
}

func TestWorkerMessagesForeignRunReturns404(t *testing.T) {
	runID := uuid.New()
	h := newProtocolHandler(t, &protocolStore{ownedErr: pgx.ErrNoRows})
	rec := httptest.NewRecorder()
	h.WorkerRunMessages(rec, workerReq(http.MethodPost, `{"messages":[{"seq":1,"kind":"text","payload":{}}]}`, runID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (run not owned by this worker)", rec.Code)
	}
}
