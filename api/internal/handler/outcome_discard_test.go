package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/outcomeloss"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func putBody(body string) (*httptest.ResponseRecorder, *http.Request) {
	rec := httptest.NewRecorder()
	return rec, httptest.NewRequest(http.MethodPut, "/api/worker/blocked-outcomes", strings.NewReader(body))
}

func incompleteReport(run string, gen int) string {
	return `{"run_id":"` + run + `","claim_generation":` + strconv.Itoa(gen) + `,"journal_sha256":"` + strings.Repeat("a", 64) +
		`","blocked_reason":"run_not_owned","loss_complete":false,"loss_sha256":"","incomplete_reason":"x","loss":null}`
}

func TestDecodeBlockedOutcomesPut(t *testing.T) {
	run := uuid.NewString()
	one := incompleteReport(run, 1)
	nine := strings.Repeat(one+",", 8) + one
	cases := []struct {
		name string
		body string
		want error
		n    int
	}{
		{"empty set", `{"outcomes":[]}`, nil, 0},
		{"one", `{"outcomes":[` + one + `]}`, nil, 1},
		{"eight", `{"outcomes":[` + strings.Repeat(one+",", 7) + one + `]}`, nil, 8},
		{"nine", `{"outcomes":[` + nine + `]}`, errBlockedOutcomesInvalid, 0},
		{"unknown top-level field", `{"outcomes":[],"x":1}`, errBlockedOutcomesInvalid, 0},
		{"unknown item field", `{"outcomes":[` + strings.Replace(one, `"loss":null`, `"loss":null,"extra":1`, 1) + `]}`, errBlockedOutcomesInvalid, 0},
		{"null outcomes", `{"outcomes":null}`, errBlockedOutcomesInvalid, 0},
		{"wrong key", `{"rejections":[]}`, errBlockedOutcomesInvalid, 0},
		{"trailing value", `{"outcomes":[]}{"outcomes":[]}`, errBlockedOutcomesInvalid, 0},
		{"trailing garbage", `{"outcomes":[]} x`, errBlockedOutcomesInvalid, 0},
		{"not an object", `[]`, errBlockedOutcomesInvalid, 0},
		{"item not an object", `{"outcomes":[1]}`, errBlockedOutcomesInvalid, 0},
		{"empty body", ``, errBlockedOutcomesInvalid, 0},
		{"one item over the per-outcome cap", `{"outcomes":[{"run_id":"` + strings.Repeat("a", outcomeloss.MaxReportBytes) + `"}]}`, errBlockedOutcomesTooLarge, 0},
		{"body over the cap", `{"outcomes":[` + incompleteReport(run, 1)[:len(one)-1] + `,"pad":"` + strings.Repeat("a", outcomeloss.MaxPutBodyBytes) + `"}]}`, errBlockedOutcomesTooLarge, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, req := putBody(tc.body)
			got, err := decodeBlockedOutcomesPut(rec, req)
			if err != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if err == nil && len(got) != tc.n {
				t.Fatalf("decoded %d, want %d", len(got), tc.n)
			}
		})
	}
}

// The handler-level 400s that need no database: confirm first, then body, grammar and evidence.
func TestDiscardWorkerBlockedOutcomeRefusesBeforeAnyLookup(t *testing.T) {
	h := &Handler{wsvc: workersvc.New(nil, nil, workersvc.Params{})}
	run, worker := uuid.NewString(), uuid.NewString()
	good := apitypes.OutcomeDiscardRequest{
		RunID: run, ClaimGeneration: 1, JournalSHA256: strings.Repeat("a", 64), LossSHA256: strings.Repeat("b", 64),
		Evidence: apitypes.OutcomeDiscardEvidenceInformed,
	}
	with := func(f func(r *apitypes.OutcomeDiscardRequest)) string {
		r := good
		f(&r)
		b, _ := json.Marshal(r)
		return string(b)
	}
	cases := []struct {
		name, query, body string
	}{
		{"missing confirm", "", with(func(*apitypes.OutcomeDiscardRequest) {})},
		{"wrong confirm", "?confirm=yes", with(func(*apitypes.OutcomeDiscardRequest) {})},
		{"no evidence", "?confirm=discard", with(func(r *apitypes.OutcomeDiscardRequest) { r.Evidence = "" })},
		{"server-held evidence is not accepted", "?confirm=discard", with(func(r *apitypes.OutcomeDiscardRequest) { r.Evidence = "recovery_capture" })},
		{"negative generation", "?confirm=discard", with(func(r *apitypes.OutcomeDiscardRequest) { r.ClaimGeneration = -1 })},
		{"unsafe generation", "?confirm=discard", with(func(r *apitypes.OutcomeDiscardRequest) { r.ClaimGeneration = 1 << 53 })},
		{"uppercase sha", "?confirm=discard", with(func(r *apitypes.OutcomeDiscardRequest) { r.JournalSHA256 = strings.ToUpper(r.JournalSHA256) })},
		{"short loss sha", "?confirm=discard", with(func(r *apitypes.OutcomeDiscardRequest) { r.LossSHA256 = "abc" })},
		{"bad run id", "?confirm=discard", with(func(r *apitypes.OutcomeDiscardRequest) { r.RunID = "nope" })},
		{"unknown field", "?confirm=discard", strings.Replace(with(func(*apitypes.OutcomeDiscardRequest) {}), "{", `{"force":true,`, 1)},
		{"trailing data", "?confirm=discard", with(func(*apitypes.OutcomeDiscardRequest) {}) + `{}`},
		{"empty body", "?confirm=discard", ``},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/workers/"+worker+"/blocked-outcomes/discard"+tc.query, strings.NewReader(tc.body))
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", worker)
			ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
			ctx = mw.ContextWithUser(ctx, store.User{ID: uuid.New()})
			rec := httptest.NewRecorder()
			h.DiscardWorkerBlockedOutcome(rec, req.WithContext(ctx))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestWorkerOutcomeDiscardRoutesRequireWorkerAuth(t *testing.T) {
	h := &Handler{wsvc: workersvc.New(nil, nil, workersvc.Params{})}
	for name, fn := range map[string]http.HandlerFunc{
		"put": h.WorkerBlockedOutcomesPut, "list": h.WorkerOutcomeDiscardsList, "begin": h.WorkerOutcomeDiscardBegin,
		"complete": h.WorkerOutcomeDiscardComplete, "reconcile": h.WorkerOutcomeDiscardReconcile,
	} {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a worker context = %d, want 401", name, rec.Code)
		}
	}
}

func TestOutcomeDiscardErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
		body string
	}{
		{&workersvc.OutcomeDiscardRefusal{Code: "loss_changed"}, 409, `"code":"loss_changed"`},
		{workersvc.ErrRunNotFound, 404, ""},
		{workersvc.ErrWorkerNotFound, 404, ""},
		{workersvc.ErrOutcomeDiscardNotFound, 404, ""},
		{workersvc.ErrOutcomeDiscardConflict, 409, `"code":"state_conflict"`},
		{workersvc.ErrInvalidOutcomeDiscardRequest, 400, ""},
		{workersvc.ErrInvalidBlockedOutcome, 400, ""},
	} {
		rec := httptest.NewRecorder()
		if !outcomeDiscardError(rec, tc.err) || rec.Code != tc.code || !strings.Contains(rec.Body.String(), tc.body) {
			t.Errorf("%v -> %d %s, want %d %s", tc.err, rec.Code, rec.Body.String(), tc.code, tc.body)
		}
	}
	if outcomeDiscardError(httptest.NewRecorder(), context.DeadlineExceeded) {
		t.Error("an unrelated error must fall through to the 500 path")
	}
}
