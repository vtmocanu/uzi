package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #1171 M1 — the Bearer-only Codex credential release/refresh routes. These are the
// TRUST-BOUNDARY tests that need no DB: they prove the request is rejected fail-closed
// BEFORE the service is ever called (missing worker, bad path run id, an unknown/duplicate
// run-id-in-body, a missing capability, a malformed operation id), plus the pure
// error→HTTP mapping leaks no provider text or secret. The service BEHAVIOUR (real
// release/refresh, generation, quarantine, two-stale-clients, api_key-zero-refresh,
// codexauth.Client behavior) is proven in workersvc's *_livedb_test.go against a real
// Postgres. Production main injection is pinned separately by
// cmd/server/codex_budget_test.go; this package's routed LiveDB test proves the real
// WorkerRoutes → RequireWorker → handler → workersvc chain.

// codexWorkerReq builds a worker-authenticated POST to a codex route with the {id} chi
// param and a raw JSON body. withWorker=false omits the worker context (the 401 case).
func codexWorkerReq(withWorker bool, runID, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	ctx := req.Context()
	if withWorker {
		ctx = mw.ContextWithWorker(ctx, store.Worker{ID: uuid.New(), UserID: uuid.New()})
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", runID)
	ctx = context.WithValue(ctx, chi.RouteCtxKey, rctx)
	return req.WithContext(ctx)
}

// codexHandlers is the (name, handler) pair so every pre-service guard is asserted on BOTH
// routes with one table — the two share the identical worker/path/decode preamble.
func codexHandlers(h *Handler) []struct {
	name string
	fn   func(http.ResponseWriter, *http.Request)
} {
	return []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"release", h.WorkerCodexRelease},
		{"refresh", h.WorkerCodexRefresh},
	}
}

// A valid-enough body per route so a test isolating ONE rejection reason does not trip a
// different guard first. h.wsvc is nil in these tests; every case here must fail BEFORE the
// service call, so a nil service is never dereferenced.
func validCodexBody(route string) string {
	if route == "refresh" {
		return fmt.Sprintf(`{"capability":"1.cap","operation_id":%q,"observed_generation":0}`, uuid.New().String())
	}
	return `{"capability":"1.cap"}`
}

// ── 401: no worker context (a cookie/session/CSRF-only caller never reaches here) ──
// The router mounts these under RequireWorker, so a non-worker caller is stopped by the
// middleware; the handler's own WorkerFromContext check is the belt-and-braces 401 this
// asserts directly.
func TestWorkerCodexRoutesRequireWorker(t *testing.T) {
	h := &Handler{}
	for _, c := range codexHandlers(h) {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.fn(rec, codexWorkerReq(false, uuid.New().String(), validCodexBody(c.name)))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 (no worker), body %q", rec.Code, rec.Body.String())
			}
			if got := errorField(t, rec.Body.Bytes()); got != codexErrAuth {
				t.Errorf("error = %q, want %q", got, codexErrAuth)
			}
		})
	}
}

// Exercise the production WorkerRoutes tree, not the handler methods directly. Walking
// the real chi router is load-bearing: an unauthenticated request cannot discriminate a
// missing child route because the parent /worker Bearer middleware returns 401 first.
func TestWorkerCodexRoutesMounted(t *testing.T) {
	router, ok := (&Handler{}).WorkerRoutes(nil).(chi.Routes)
	if !ok {
		t.Fatal("WorkerRoutes did not return a walkable chi router")
	}
	want := map[string]bool{
		"POST /api/worker/runs/{id}/codex/release": false,
		"POST /api/worker/runs/{id}/codex/refresh": false,
	}
	if err := chi.Walk(router, func(method, pattern string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		key := method + " " + pattern
		if _, tracked := want[key]; tracked {
			want[key] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk WorkerRoutes: %v", err)
	}
	for route, found := range want {
		if !found {
			t.Fatalf("production WorkerRoutes is missing %s", route)
		}
	}
}

// ── 400: a malformed path run id ({id} is the SOLE run identity) ──
func TestWorkerCodexRoutesMalformedRunID(t *testing.T) {
	h := &Handler{}
	for _, c := range codexHandlers(h) {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.fn(rec, codexWorkerReq(true, "not-a-uuid", validCodexBody(c.name)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (bad run id), body %q", rec.Code, rec.Body.String())
			}
		})
	}
}

// ── 400: a body-supplied / duplicate run id (or any user/secret/account/token field) is an
// UNKNOWN field to the strict decoder and rejected before the service is called. This is
// the "path {id} is the sole run identity; reject a body-supplied run id" guard. ──
func TestWorkerCodexRoutesRejectBodySuppliedFields(t *testing.T) {
	h := &Handler{}
	// Each body is otherwise valid but carries one forbidden extra field. A run-id field
	// (run_id/id) must never be accepted, nor any credential-coordinate field.
	extras := []string{
		`{"capability":"1.cap","run_id":"%s"}`,
		`{"capability":"1.cap","id":"%s"}`,
		`{"capability":"1.cap","user_id":"%s"}`,
		`{"capability":"1.cap","secret_id":"%s"}`,
		`{"capability":"1.cap","account_id":"%s"}`,
		`{"capability":"1.cap","access_token":"%s"}`,
		`{"capability":"1.cap","refresh_token":"%s"}`,
		`{"capability":"1.cap","previous_account_id":"%s"}`,
	}
	runID := uuid.New().String()
	for _, c := range codexHandlers(h) {
		for _, tmpl := range extras {
			body := fmt.Sprintf(tmpl, runID)
			// The refresh body additionally needs a valid operation_id so the ONLY reason it
			// could fail is the forbidden extra field, not a missing/bad op id.
			if c.name == "refresh" {
				body = strings.TrimSuffix(body, "}") + fmt.Sprintf(`,"operation_id":%q}`, uuid.New().String())
			}
			t.Run(c.name+" "+tmpl, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c.fn(rec, codexWorkerReq(true, runID, body))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (unknown body field rejected), body %q", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// A second JSON value used to be ignored after the first authorized object. Both worker
// credential routes are authority boundaries, so the entire body must be exactly one
// value, apart from trailing whitespace.
func TestWorkerCodexRoutesRejectTrailingJSONValue(t *testing.T) {
	h := &Handler{}
	runID := uuid.New().String()
	for _, c := range codexHandlers(h) {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			body := validCodexBody(c.name) + fmt.Sprintf(` {"run_id":%q}`, uuid.New().String())
			c.fn(rec, codexWorkerReq(true, runID, body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for a trailing JSON value, body %q", rec.Code, rec.Body.String())
			}
		})
	}
}

// ── 400: a missing capability (the one required scoped field) ──
func TestWorkerCodexRoutesRequireCapability(t *testing.T) {
	h := &Handler{}
	runID := uuid.New().String()
	for _, c := range codexHandlers(h) {
		body := `{"capability":""}`
		if c.name == "refresh" {
			body = fmt.Sprintf(`{"capability":"","operation_id":%q,"observed_generation":0}`, uuid.New().String())
		}
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c.fn(rec, codexWorkerReq(true, runID, body))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (empty capability), body %q", rec.Code, rec.Body.String())
			}
		})
	}
}

// ── 400: refresh with a malformed operation id (never silently substituted with zero) ──
func TestWorkerCodexRefreshMalformedOperationID(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.WorkerCodexRefresh(rec, codexWorkerReq(true, uuid.New().String(),
		`{"capability":"1.cap","operation_id":"not-a-uuid","observed_generation":0}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (bad operation id), body %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerCodexRefreshRejectsNegativeGeneration(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	h.WorkerCodexRefresh(rec, codexWorkerReq(true, uuid.New().String(),
		fmt.Sprintf(`{"capability":"1.cap","operation_id":%q,"observed_generation":-1}`, uuid.New().String())))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a negative generation, body %q", rec.Code, rec.Body.String())
	}
}

func TestWorkerCodexRefreshRequiresGenerationPresence(t *testing.T) {
	h := &Handler{}
	for _, body := range []string{
		fmt.Sprintf(`{"capability":"1.cap","operation_id":%q}`, uuid.New().String()),
		fmt.Sprintf(`{"capability":"1.cap","operation_id":%q,"observed_generation":null}`, uuid.New().String()),
	} {
		rec := httptest.NewRecorder()
		h.WorkerCodexRefresh(rec, codexWorkerReq(true, uuid.New().String(), body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 when observed_generation is omitted/null, body %q", rec.Code, rec.Body.String())
		}
	}
}

func TestWorkerCodexRefreshRejectsUnadvanceableSafeIntegerMaximum(t *testing.T) {
	h := &Handler{}
	rec := httptest.NewRecorder()
	body := fmt.Sprintf(`{"capability":"1.cap","operation_id":%q,"observed_generation":%d}`,
		uuid.New().String(), maxCodexRefreshObservedGeneration)
	h.WorkerCodexRefresh(rec, codexWorkerReq(true, uuid.New().String(), body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 before service/provider work at max safe generation", rec.Code)
	}
}

func TestCodexCredentialResponseWireModes(t *testing.T) {
	t.Run("subscription includes verified account, generation, and explicit plan null", func(t *testing.T) {
		b, err := json.Marshal(codexSubscriptionResponse{ //nolint:gosec // G117: "ACCESS-PLACEHOLDER" is a hardcoded test placeholder, not a real credential; this test asserts the JSON wire shape only.
			AuthMode:         "subscription",
			AccessToken:      "ACCESS-PLACEHOLDER",
			Generation:       0,
			ChatGPTAccountID: "verified-account",
			ChatGPTPlanType:  nil,
		})
		if err != nil {
			t.Fatalf("marshal subscription response: %v", err)
		}
		want := `{"auth_mode":"subscription","access_token":"ACCESS-PLACEHOLDER","generation":0,"chatgpt_account_id":"verified-account","chatgpt_plan_type":null}`
		if string(b) != want {
			t.Fatalf("subscription response = %s, want %s", b, want)
		}
	})

	t.Run("api_key omits every subscription field", func(t *testing.T) {
		b, err := json.Marshal(codexAPIKeyResponse{AuthMode: "api_key", AccessToken: "KEY-PLACEHOLDER"}) //nolint:gosec // G117: "KEY-PLACEHOLDER" is a hardcoded test placeholder, not a real credential; this test asserts the JSON wire shape only.
		if err != nil {
			t.Fatalf("marshal api_key response: %v", err)
		}
		want := `{"auth_mode":"api_key","access_token":"KEY-PLACEHOLDER"}`
		if string(b) != want {
			t.Fatalf("api_key response = %s, want %s", b, want)
		}
	})

	t.Run("refresh repeats subscription authority", func(t *testing.T) {
		b, err := json.Marshal(codexRefreshResponse{ //nolint:gosec // G117: "ACCESS-PLACEHOLDER" is a hardcoded test placeholder, not a real credential; this test asserts the JSON wire shape only.
			AuthMode:         "subscription",
			AccessToken:      "ACCESS-PLACEHOLDER",
			Generation:       4,
			ChatGPTAccountID: "verified-account",
			ChatGPTPlanType:  nil,
			Outcome:          "advanced",
		})
		if err != nil {
			t.Fatalf("marshal refresh response: %v", err)
		}
		want := `{"auth_mode":"subscription","access_token":"ACCESS-PLACEHOLDER","generation":4,"chatgpt_account_id":"verified-account","chatgpt_plan_type":null,"outcome":"advanced"}`
		if string(b) != want {
			t.Fatalf("refresh response = %s, want %s", b, want)
		}
	})
}

// ── no-store on every response, even the pre-service rejections (a success body is
// secret-bearing; setting it unconditionally at the top guarantees it covers that body). ──
func TestWorkerCodexRoutesSetNoStore(t *testing.T) {
	h := &Handler{}
	for _, c := range codexHandlers(h) {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			// Any request reaches the header set (it is the first statement); use the 401 path.
			c.fn(rec, codexWorkerReq(false, uuid.New().String(), validCodexBody(c.name)))
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

func TestCodexWorkerOperationTimeoutLeavesResponseMargin(t *testing.T) {
	if codexWorkerOperationTimeout != 7500*time.Millisecond {
		t.Fatalf("API operation timeout = %s, want 7.5s below the worker's 8s HTTP budget", codexWorkerOperationTimeout)
	}
}

// ── the pure error→HTTP mapping: every mapped sentinel gets its fixed status + body, and
// NO mapping — mapped or generic — ever carries provider text or a secret. ──
func TestCodexHTTPErrorMapping(t *testing.T) {
	cases := []struct {
		err        error
		wantStatus int
		wantMsg    string
	}{
		{workersvc.ErrRunNotOwned, http.StatusNotFound, codexErrRunNotFound},
		{workersvc.ErrCodexRunNotBound, http.StatusNotFound, codexErrRunNotFound},
		{workersvc.ErrCodexWorkerMismatch, http.StatusNotFound, codexErrRunNotFound},
		{workersvc.ErrCodexCapabilityMismatch, http.StatusForbidden, codexErrNotAuthorized},
		{workersvc.ErrCodexCapabilityEpoch, http.StatusForbidden, codexErrNotAuthorized},
		{workersvc.ErrCodexScopeNotApplicable, http.StatusForbidden, codexErrNotAuthorized},
		{workersvc.ErrCodexKindModeMismatch, http.StatusForbidden, codexErrNotAuthorized},
		{workersvc.ErrCodexRefreshContended, http.StatusConflict, codexErrRefreshContended},
		{workersvc.ErrCodexRefreshQuarantined, http.StatusConflict, codexErrRefreshUnavailable},
		{workersvc.ErrCodexRefreshUnrecoverable, http.StatusConflict, codexErrRefreshUnavailable},
		{workersvc.ErrCodexRefreshNoToken, http.StatusConflict, codexErrRefreshUnavailable},
		{workersvc.ErrCodexRefreshNoClient, http.StatusServiceUnavailable, codexErrRefreshNoClient},
		{workersvc.ErrCodexRunNotActivelyClaimed, http.StatusConflict, codexErrCredUnavailable},
		{workersvc.ErrCodexMaterialRevisionStale, http.StatusConflict, codexErrCredUnavailable},
		{workersvc.ErrCodexAccountKeyUnfrozen, http.StatusConflict, codexErrCredUnavailable},
		{workersvc.ErrCodexAccountTupleMismatch, http.StatusConflict, codexErrCredUnavailable},
		{workersvc.ErrCodexAccountRevisionStale, http.StatusConflict, codexErrCredUnavailable},
		{workersvc.ErrCodexAccountQuarantined, http.StatusConflict, codexErrCredUnavailable},
		{workersvc.ErrCodexBindingConflict, http.StatusConflict, codexErrCredUnavailable},
	}
	for _, tc := range cases {
		status, msg := codexHTTPError(tc.err)
		if status != tc.wantStatus || msg != tc.wantMsg {
			t.Errorf("codexHTTPError(%v) = (%d, %q), want (%d, %q)", tc.err, status, msg, tc.wantStatus, tc.wantMsg)
		}
	}

	// The generic bucket: a wrapped provider-exchange error carrying a provider host, status
	// and a secret-shaped token must map to a fixed 500 whose body contains NONE of that text.
	// The token-shaped fragment is assembled from parts at runtime so no contiguous
	// secret-looking literal lives in tracked source.
	secretShaped := "sk-" + "super-secret-abc"
	leaky := fmt.Errorf("codex refresh: provider exchange: %w",
		errors.New("GET https://auth.openai.com/oauth/token returned 401: refresh_token="+secretShaped))
	status, msg := codexHTTPError(leaky)
	if status != http.StatusInternalServerError || msg != codexErrInternal {
		t.Fatalf("generic mapping = (%d, %q), want (500, %q)", status, msg, codexErrInternal)
	}
	for _, leak := range []string{"auth.openai.com", "oauth", "401", secretShaped, "refresh_token", "provider"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("mapped message %q leaks %q from the wrapped provider error", msg, leak)
		}
	}

	// Every fixed message is coordinate-free: none carries a run/user/account/secret token
	// or a provider host. (A cheap regression against someone later interpolating a value.)
	for _, m := range []string{
		codexErrAuth, codexErrInvalid, codexErrRunNotFound, codexErrNotAuthorized,
		codexErrCredUnavailable, codexErrRefreshContended, codexErrRefreshUnavailable,
		codexErrRefreshNoClient, codexErrInternal,
	} {
		for _, forbidden := range []string{"://", "token", "secret", "user_id", "account", "sk-"} {
			if strings.Contains(m, forbidden) {
				t.Errorf("fixed codex error %q contains a coordinate/secret substring %q", m, forbidden)
			}
		}
	}
}

// ── the refresh outcome renderer is total and stable ──
func TestCodexRefreshOutcomeString(t *testing.T) {
	cases := map[workersvc.CodexRefreshOutcome]string{
		workersvc.CodexRefreshAdvanced:       "advanced",
		workersvc.CodexRefreshReplayed:       "replayed",
		workersvc.CodexRefreshReconciled:     "reconciled",
		workersvc.CodexRefreshContended:      "contended",
		workersvc.CodexRefreshQuarantined:    "quarantined",
		workersvc.CodexRefreshOutcomeUnknown: "unknown",
	}
	for o, want := range cases {
		if got := codexRefreshOutcomeString(o); got != want {
			t.Errorf("codexRefreshOutcomeString(%v) = %q, want %q", o, got, want)
		}
	}
}
