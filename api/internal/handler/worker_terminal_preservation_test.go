package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Issue #4007 slice A, M3: the terminal-preservation route. The service is a fake
// (terminalPublisher), so these tests pin the HTTP contract: the strict body, the validations, the
// order (the capability check before the body is read), the reason-to-status mapping and the cap.

const termHTTPTip = "0123456789abcdef0123456789abcdef01234567"

type fakeTerminalSvc struct {
	mu        sync.Mutex
	slotOK    bool
	admit     error
	publish   func(ctx context.Context, req workersvc.TerminalPublishRequest) (workersvc.TerminalPublishResult, error)
	admitN    int
	publishN  int
	lastReq   workersvc.TerminalPublishRequest
	releases  int
	slotTaken int
}

func newFakeTerminalSvc() *fakeTerminalSvc { return &fakeTerminalSvc{slotOK: true} }

func (f *fakeTerminalSvc) AcquireHeldPublishSlot() (func(), bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.slotOK {
		return nil, false
	}
	f.slotTaken++
	return func() { f.mu.Lock(); f.releases++; f.mu.Unlock() }, true
}

func (f *fakeTerminalSvc) TerminalPreservationAdmit(store.Worker) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.admitN++
	return f.admit
}

func (f *fakeTerminalSvc) PublishTerminal(ctx context.Context, _ store.Worker, _ uuid.UUID, req workersvc.TerminalPublishRequest) (workersvc.TerminalPublishResult, error) {
	f.mu.Lock()
	f.publishN++
	f.lastReq = req
	fn := f.publish
	f.mu.Unlock()
	if fn == nil {
		return workersvc.TerminalPublishResult{PreservationID: uuid.New(), ContributionID: uuid.New(), Ref: "refs/uzi-terminal/x", Tip: req.Tip, State: "published", PreservationState: "live", Disposition: "preserved"}, nil
	}
	return fn(ctx, req)
}

func terminalHandler(f *fakeTerminalSvc) *Handler { return &Handler{terminalSvc: f} }

func termBody(t *testing.T, mutate func(m map[string]any)) string {
	t.Helper()
	m := map[string]any{
		"generation":  2,
		"tip":         termHTTPTip,
		"disposition": "preserve",
		"manifest": map[string]any{
			"run_id": uuid.NewString(), "generation": 2, "worker_id": uuid.NewString(),
			"roots":     []map[string]any{{"source_id": "s", "attempt_id": "a", "commit": termHTTPTip, "tree": termHTTPTip}},
			"abandoned": []map[string]any{{"class": "ignored", "source_id": "s", "extents": map[string]any{"files": 1, "bytes": 2}}},
		},
	}
	if mutate != nil {
		mutate(m)
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func termReq(body string) *http.Request {
	return reqWithWorkerAndParam(http.MethodPost, "/api/worker/runs/x/terminal-preservation", strings.NewReader(body), uuid.New())
}

func termWireReason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var b struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.Reason
}

func TestWorkerRunTerminalPreservationBodyValidation(t *testing.T) {
	roots := func(n int) []map[string]any {
		out := make([]map[string]any, n)
		for i := range out {
			out[i] = map[string]any{"source_id": "s", "attempt_id": "a", "commit": termHTTPTip, "tree": termHTTPTip}
		}
		return out
	}
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	for name, tc := range map[string]struct {
		body string
	}{
		"unknown field":          {termBody(t, set("force", true))},
		"unknown manifest field": {termBody(t, func(m map[string]any) { m["manifest"].(map[string]any)["extra"] = 1 })},
		"unknown root field":     {termBody(t, func(m map[string]any) { m["manifest"].(map[string]any)["roots"].([]map[string]any)[0]["path"] = "x" })},
		"unknown extents field": {termBody(t, func(m map[string]any) {
			m["manifest"].(map[string]any)["abandoned"].([]map[string]any)[0]["extents"].(map[string]any)["x"] = 1
		})},
		"trailing data":         {termBody(t, nil) + `{}`},
		"not json":              {"nope"},
		"no generation":         {termBody(t, func(m map[string]any) { delete(m, "generation") })},
		"zero generation":       {termBody(t, set("generation", 0))},
		"negative generation":   {termBody(t, set("generation", -1))},
		"string generation":     {termBody(t, set("generation", "2"))},
		"no tip":                {termBody(t, func(m map[string]any) { delete(m, "tip") })},
		"uppercase tip":         {termBody(t, set("tip", strings.ToUpper(termHTTPTip)))},
		"64-hex tip":            {termBody(t, set("tip", strings.Repeat("a", 64)))},
		"bad prior tip":         {termBody(t, set("prior_tip", "abc"))},
		"no disposition":        {termBody(t, func(m map[string]any) { delete(m, "disposition") })},
		"abandon disposition":   {termBody(t, set("disposition", "abandon"))},
		"preserved disposition": {termBody(t, set("disposition", "preserved"))},
		"too many roots": {termBody(t, func(m map[string]any) {
			m["manifest"].(map[string]any)["roots"] = roots(apitypes.TerminalManifestMaxRoots + 1)
		})},
		"too many abandoned": {termBody(t, func(m map[string]any) {
			m["manifest"].(map[string]any)["abandoned"] = make([]map[string]any, apitypes.TerminalManifestMaxAbandoned+1)
		})},
		"invalid base64 pack":       {termBody(t, set("pack", "not base64!!"))},
		"url-safe base64 pack":      {termBody(t, set("pack", "-_-_"))},
		"manifest as a string":      {termBody(t, set("manifest", "x"))},
		"null manifest is rejected": {termBody(t, set("manifest", 5))},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeTerminalSvc()
			rec := httptest.NewRecorder()
			terminalHandler(f).WorkerRunTerminalPreservation(rec, termReq(tc.body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body %q", rec.Code, rec.Body.String())
			}
			if f.publishN != 0 {
				t.Fatal("the service was reached with an invalid body")
			}
		})
	}

	t.Run("a valid body reaches the service with the decoded pack", func(t *testing.T) {
		f := newFakeTerminalSvc()
		rec := httptest.NewRecorder()
		body := termBody(t, func(m map[string]any) {
			m["pack"] = base64.StdEncoding.EncodeToString([]byte("PACKDATA"))
			m["prior_tip"] = strings.Repeat("b", 40)
		})
		terminalHandler(f).WorkerRunTerminalPreservation(rec, termReq(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body %q", rec.Code, rec.Body.String())
		}
		if string(f.lastReq.Pack) != "PACKDATA" || f.lastReq.Generation != 2 || f.lastReq.Tip != termHTTPTip || f.lastReq.PriorTip != strings.Repeat("b", 40) ||
			len(f.lastReq.Manifest.Roots) != 1 {
			t.Fatalf("service request = %+v", f.lastReq)
		}
		var resp apitypes.TerminalPreservationResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.State != "published" || resp.PreservationState != "live" || resp.Disposition != "preserved" {
			t.Fatalf("response = %+v err=%v", resp, err)
		}
		if f.releases != 1 || f.slotTaken != 1 {
			t.Fatalf("slot taken=%d released=%d, want 1/1", f.slotTaken, f.releases)
		}
	})

	t.Run("a body without a pack and without a prior tip is valid", func(t *testing.T) {
		f := newFakeTerminalSvc()
		rec := httptest.NewRecorder()
		terminalHandler(f).WorkerRunTerminalPreservation(rec, termReq(termBody(t, nil)))
		if rec.Code != http.StatusOK || len(f.lastReq.Pack) != 0 || f.lastReq.PriorTip != "" {
			t.Fatalf("status = %d req = %+v", rec.Code, f.lastReq)
		}
	})
}

func TestWorkerRunTerminalPreservationNoWorker401(t *testing.T) {
	rec := httptest.NewRecorder()
	terminalHandler(newFakeTerminalSvc()).WorkerRunTerminalPreservation(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// The capability check precedes the body: an unsupported worker never uploads anything, and a busy
// slot pool is a retryable 429 before even that.
func TestWorkerRunTerminalPreservationGateOrder(t *testing.T) {
	t.Run("unsupported is 422 and the body is never read", func(t *testing.T) {
		f := newFakeTerminalSvc()
		f.admit = &workersvc.TerminalRefusal{Reason: apitypes.TerminalReasonUnsupported}
		req := reqWithWorkerAndParam(http.MethodPost, "/x", poisonReader{t}, uuid.New())
		rec := httptest.NewRecorder()
		terminalHandler(f).WorkerRunTerminalPreservation(rec, req)
		if rec.Code != http.StatusUnprocessableEntity || termWireReason(t, rec) != "unsupported" {
			t.Fatalf("status = %d body %q", rec.Code, rec.Body.String())
		}
		if f.publishN != 0 || f.releases != 1 {
			t.Fatalf("publish=%d releases=%d", f.publishN, f.releases)
		}
	})
	t.Run("busy", func(t *testing.T) {
		f := newFakeTerminalSvc()
		f.slotOK = false
		req := reqWithWorkerAndParam(http.MethodPost, "/x", poisonReader{t}, uuid.New())
		rec := httptest.NewRecorder()
		terminalHandler(f).WorkerRunTerminalPreservation(rec, req)
		if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" || termWireReason(t, rec) != "busy" || f.admitN != 0 {
			t.Fatalf("status = %d admit=%d body %q", rec.Code, f.admitN, rec.Body.String())
		}
	})
}

// The whole body is capped: a stream past the cap is a truthful 413 and the service never runs.
func TestWorkerRunTerminalPreservationOversize413(t *testing.T) {
	old := terminalMaxBodyBytes
	terminalMaxBodyBytes = 1 << 10
	t.Cleanup(func() { terminalMaxBodyBytes = old })
	f := newFakeTerminalSvc()
	rec := httptest.NewRecorder()
	terminalHandler(f).WorkerRunTerminalPreservation(rec, termReq(termBody(t, func(m map[string]any) { m["pack"] = strings.Repeat("A", 4<<10) })))
	if rec.Code != http.StatusRequestEntityTooLarge || f.publishN != 0 {
		t.Fatalf("status = %d publish=%d, want 413 and no service call", rec.Code, f.publishN)
	}
}

func TestWorkerRunTerminalPreservationErrorMapping(t *testing.T) {
	refusal := func(r string) error { return &workersvc.TerminalRefusal{Reason: r} }
	for _, tc := range []struct {
		name   string
		err    error
		status int
		reason string
		retry  bool
	}{
		{"unsupported", refusal(apitypes.TerminalReasonUnsupported), http.StatusUnprocessableEntity, "unsupported", false},
		{"hold_missing", refusal(apitypes.TerminalReasonHoldMissing), http.StatusConflict, "hold_missing", false},
		{"identity_changed", refusal(apitypes.TerminalReasonIdentityChanged), http.StatusConflict, "identity_changed", false},
		{"excluded_origin", refusal(apitypes.TerminalReasonExcludedOrigin), http.StatusConflict, "excluded_origin", false},
		{"manifest_invalid", refusal(apitypes.TerminalReasonManifestInvalid), http.StatusUnprocessableEntity, "manifest_invalid", false},
		{"manifest_mismatch", refusal(apitypes.TerminalReasonManifestMismatch), http.StatusConflict, "manifest_mismatch", false},
		{"generation_bound", refusal(apitypes.TerminalReasonGenerationBound), http.StatusConflict, "generation_bound", false},
		{"prior_tip_mismatch", refusal(apitypes.TerminalReasonPriorTipMismatch), http.StatusConflict, "prior_tip_mismatch", false},
		{"preservation_retired", refusal(apitypes.TerminalReasonPreservationRetired), http.StatusConflict, "preservation_retired", false},
		{"publication_in_flight", refusal(apitypes.TerminalReasonPublicationInFlight), http.StatusConflict, "publication_in_flight", true},
		{"pack_invalid", refusal(apitypes.TerminalReasonPackInvalid), http.StatusUnprocessableEntity, "pack_invalid", false},
		{"pack_too_large", refusal(apitypes.TerminalReasonPackTooLarge), http.StatusRequestEntityTooLarge, "pack_too_large", false},
		{"tip_missing", refusal(apitypes.TerminalReasonTipMissing), http.StatusUnprocessableEntity, "tip_missing", false},
		{"ci_config_guard", refusal(apitypes.TerminalReasonCIConfigGuard), http.StatusConflict, "ci_config_guard", false},
		{"not_descendant", refusal(apitypes.TerminalReasonNotDescendant), http.StatusUnprocessableEntity, "not_descendant", false},
		{"root_unreachable", refusal(apitypes.TerminalReasonRootUnreachable), http.StatusUnprocessableEntity, "root_unreachable", false},
		{"tree_mismatch", refusal(apitypes.TerminalReasonTreeMismatch), http.StatusUnprocessableEntity, "tree_mismatch", false},
		{"walk_too_large", refusal(apitypes.TerminalReasonWalkTooLarge), http.StatusUnprocessableEntity, "walk_too_large", false},
		{"foreign_tip", refusal(apitypes.TerminalReasonForeignTip), http.StatusConflict, "foreign_tip", false},
		{"forge_unavailable", refusal(apitypes.TerminalReasonForgeUnavailable), http.StatusServiceUnavailable, "forge_unavailable", true},
		{"stale claim", workersvc.ErrStaleClaim, http.StatusConflict, "stale_claim", false},
		{"missing generation", workersvc.ErrMissingClaimGeneration, http.StatusConflict, "missing_claim_generation", false},
		{"foreign run", workersvc.ErrRunNotOwned, http.StatusNotFound, "", false},
		{"unexpected", errors.New("db down glpat-" + strings.Repeat("aB3d", 5)), http.StatusInternalServerError, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeTerminalSvc()
			f.publish = func(context.Context, workersvc.TerminalPublishRequest) (workersvc.TerminalPublishResult, error) {
				return workersvc.TerminalPublishResult{}, tc.err
			}
			rec := httptest.NewRecorder()
			terminalHandler(f).WorkerRunTerminalPreservation(rec, termReq(termBody(t, nil)))
			if rec.Code != tc.status || termWireReason(t, rec) != tc.reason || (rec.Header().Get("Retry-After") != "") != tc.retry {
				t.Fatalf("status=%d reason=%q retry=%q body %q", rec.Code, termWireReason(t, rec), rec.Header().Get("Retry-After"), rec.Body.String())
			}
			if tc.status == http.StatusInternalServerError && (strings.Contains(rec.Body.String(), "db down") || strings.Contains(rec.Body.String(), "glpat")) {
				t.Fatalf("an internal error leaked detail: %q", rec.Body.String())
			}
		})
	}
}
