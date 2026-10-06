package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestCrossCheckSubmitEnvelopeBound(t *testing.T) {
	// Escaped prose exceeds the ordinary route cap while decoded fields fit.
	c := workersvc.PlanCrossCheckCandidate{PlanMd: strings.Repeat("<", 256<<10),
		PlanningDiff: strings.Repeat("<", 512<<10), Milestones: json.RawMessage("[]"),
		SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
	gen := int64(1)
	raw, err := json.Marshal(planCrossCheckRequest{Stage: "plan", ClaimGeneration: &gen, PlanCrossCheckCandidate: c})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 1<<20 {
		t.Fatal("fixture did not exceed ordinary cap")
	}
	var got planCrossCheckRequest
	if err := httpx.DecodeJSONStrictBounded(httptest.NewRequest("POST", "/", bytes.NewReader(raw)), &got, 8<<20); err != nil {
		t.Fatal(err)
	}
	if got.PlanMd != c.PlanMd || got.PlanningDiff != c.PlanningDiff {
		t.Fatal("escaped prose changed on decode")
	}
	if err := httpx.DecodeJSONStrict(httptest.NewRequest("POST", "/", bytes.NewReader(raw)), &got); err == nil {
		t.Fatal("ordinary route cap widened")
	}
	for _, body := range [][]byte{append(append([]byte{}, raw...), []byte(" {}")...), []byte(`{"unknown":1}`)} {
		if err := httpx.DecodeJSONStrictBounded(httptest.NewRequest("POST", "/", bytes.NewReader(body)), &got, 8<<20); err == nil {
			t.Fatal("unknown/trailing JSON accepted")
		}
	}
	exact := append(append([]byte{}, raw...), bytes.Repeat([]byte(" "), (8<<20)-len(raw))...)
	if err := httpx.DecodeJSONStrictBounded(httptest.NewRequest("POST", "/", bytes.NewReader(exact)), &got, 8<<20); err != nil {
		t.Fatal(err)
	}
	overflowReq := httptest.NewRequest("POST", "/", bytes.NewReader(append(append([]byte{}, exact...), '{')))
	overflowReq.ContentLength = 1 // The encoded stream, not this header, owns the cap.
	var overflow *http.MaxBytesError
	if err := httpx.DecodeJSONStrictBounded(overflowReq, &got, 8<<20); !errors.As(err, &overflow) || overflow.Limit != 8<<20 {
		t.Fatalf("cap+1 did not return typed 8 MiB overflow: %v", err)
	}
	ordinary := append([]byte(`{}`), bytes.Repeat([]byte(" "), (1<<20)-2)...)
	var ordinaryValue map[string]any
	if err := httpx.DecodeJSONStrict(httptest.NewRequest("POST", "/", bytes.NewReader(ordinary)), &ordinaryValue); err != nil {
		t.Fatal(err)
	}
	if err := httpx.DecodeJSONStrict(httptest.NewRequest("POST", "/", bytes.NewReader(append(ordinary, '{'))), &ordinaryValue); !errors.As(err, &overflow) || overflow.Limit != 1<<20 {
		t.Fatalf("ordinary strict cap+1: %v", err)
	}
	h := &Handler{}
	for _, tc := range []struct {
		body   []byte
		status int
		reason string
	}{
		{append(exact, ' '), http.StatusRequestEntityTooLarge, "envelope_too_large"},
		{append(append([]byte{}, raw...), []byte(" {}")...), http.StatusBadRequest, "candidate_invalid"},
		{[]byte(`{"stage":"plan","claim_generation":1,"unknown":1}`), http.StatusBadRequest, "candidate_invalid"},
		{[]byte(`{"stage":"plan"}`), http.StatusBadRequest, "candidate_invalid"},
		{[]byte(`{"stage":"plan","claim_generation":1,"milestones":[]}`), http.StatusBadRequest, "candidate_invalid"},
		{[]byte(`{"stage":"plan","claim_generation":1,"plan_md":"` + strings.Repeat("a", (256<<10)+1) + `"}`), http.StatusBadRequest, "candidate_too_large"},
		{[]byte(`{"stage":"plan","claim_generation":1,"milestones":[],"required_tools":["bad\u202eid"]}`), http.StatusBadRequest, "candidate_invalid"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(tc.body))
		route := chi.NewRouteContext()
		route.URLParams.Add("id", uuid.NewString())
		req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New(), UserID: uuid.New()}))
		rec := httptest.NewRecorder()
		h.WorkerSubmitPlanCrossCheck(rec, req)
		var result struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &result) != nil || rec.Code != tc.status || result.Reason != tc.reason {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
}

func TestCrossCheckNormalizedResponseFitsClientBound(t *testing.T) {
	c := workersvc.PlanCrossCheckCandidate{PlanMd: strings.Repeat("<", 256<<10),
		PlanningDiff: strings.Repeat("<", 512<<10), SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
	// Milestone JSON is capped after encoding, independently of prose limits.
	c.Milestones = json.RawMessage(`[{"title":"` + strings.Repeat("<", ((256<<10)-14)/6) + `"}]`)
	for i := 0; i < 64; i++ {
		c.RequiredCapabilities = append(c.RequiredCapabilities, strings.Repeat("<", 256))
		c.RequiredTools = append(c.RequiredTools, strings.Repeat("<", 256))
	}
	normalized, err := workersvc.NormalizePlanCrossCheckCandidate(c)
	if err != nil {
		t.Fatal(err)
	}
	cc := store.CrossCheck{Round: 1, LeadClaimGeneration: 1, Verdict: "failed",
		PlanMd: pgtype.Text{String: normalized.PlanMd, Valid: true}, PlanningDiff: pgtype.Text{String: normalized.PlanningDiff, Valid: true},
		Milestones: normalized.Milestones, RequiredCapabilities: normalized.RequiredCapabilities,
		RequiredTools: normalized.RequiredTools, SizeClass: pgtype.Text{String: "s", Valid: true},
		BaseCommit: pgtype.Text{String: c.BaseCommit, Valid: true}, CandidateDigest: make([]byte, 32),
		Findings: json.RawMessage(`{"summary":"` + strings.Repeat("a", (32<<10)-14) + `"}`)}
	raw, err := json.Marshal(crossCheckResponse(cc, 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 4<<20 || len(raw) >= 6<<20 {
		t.Fatalf("escape-heavy normalized response=%d bytes, want between 4 and 6 MiB", len(raw))
	}
}
