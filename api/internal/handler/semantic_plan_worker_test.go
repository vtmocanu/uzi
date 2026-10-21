package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestSemanticWorkerProposalEnvelope(t *testing.T) {
	h, _ := semanticBodyHandler(true)
	// The existing compiler admits a complete raw proposal up to 48 MiB.
	// Generation zero stops before storage, after the real route's strict decode.
	proposal := `{"criteria":[]}`
	for _, tc := range []struct {
		name  string
		bytes int
		total bool
		want  string
	}{
		{"raw proposal ceiling plus transport", 48 << 20, false, "invalid semantic candidate"},
		{"above envelope", (64 << 20) + 1, true, "invalid semantic request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const prefix = `{"claim_generation":0,"structural":{},"proposal":`
			const suffix = `,"sources":[]}`
			padding := tc.bytes - len(proposal)
			if tc.total {
				padding -= len(prefix) + len(suffix)
			}
			body := prefix + proposal[:len(proposal)-1] + strings.Repeat(" ", padding) + "}" + suffix
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			route := chi.NewRouteContext()
			route.URLParams.Add("id", uuid.NewString())
			req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New()}))
			rec := httptest.NewRecorder()
			h.WorkerSemanticCandidate(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// semanticEscapedRequest generates the largest mandatory-escaping regression
// with valid field maxima, eight milestones and 32 distinct criteria each.
func semanticEscapedRequest(t *testing.T) semanticCandidateRequest {
	t.Helper()
	fill := func(prefix string, n int) string { return prefix + strings.Repeat("\x01", n-len(prefix)) }
	id := strings.Repeat("\"", 512)
	source := []byte("requirement")
	hash := sha256.Sum256(source)
	structural := workersvc.PlanCrossCheckCandidate{
		PlanMd:       "plan" + strings.Repeat("\t", (256<<10)-4),
		PlanningDiff: "diff" + strings.Repeat("\n", (512<<10)-4),
		BaseCommit:   strings.Repeat("a", 40), SizeClass: "s",
		RequiredCapabilities: []string{}, RequiredTools: []string{},
	}
	var milestones []workersvc.Milestone
	for i := range 8 {
		milestones = append(milestones, workersvc.Milestone{ID: fmt.Sprintf("m%d", i), Title: "Milestone"})
	}
	var err error
	structural.Milestones, err = json.Marshal(milestones)
	if err != nil {
		t.Fatal(err)
	}
	proposal := apitypes.SemanticProposal{}
	for i := range 256 {
		c := apitypes.ProposedSemanticCriterion{
			MilestoneID: fmt.Sprintf("m%d", i/32), Kind: "maintainer",
			Text: fill(fmt.Sprintf("%03d", i), 2048), DoneWhen: fill("", 2048),
			Maintainer: &apitypes.SemanticMaintainer{Reason: fill("", 2048)},
		}
		for j := range 8 {
			c.Citations = append(c.Citations, apitypes.SemanticCitation{SourceID: id, Hash: hex.EncodeToString(hash[:]), Start: 0, End: j + 1})
			c.EvidenceRequirements = append(c.EvidenceRequirements, apitypes.SemanticEvidence{Description: fill(fmt.Sprint(j), 512), Path: id})
			c.Maintainer.Conditions = append(c.Maintainer.Conditions, fill(fmt.Sprint(j), 512))
			c.Maintainer.VerificationSteps = append(c.Maintainer.VerificationSteps, fill(fmt.Sprint(j), 512))
		}
		proposal.Criteria = append(proposal.Criteria, c)
	}
	raw, err := json.Marshal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(1)
	return semanticCandidateRequest{ClaimGeneration: &generation, Structural: structural, Proposal: raw,
		Sources: []apitypes.SemanticSourceCapture{{ID: id, Kind: "issue", Provenance: "worker:capture", Completeness: "complete", Bytes: source}}}
}

func TestSemanticWorkerMandatoryEscapedAdmission(t *testing.T) {
	h, _ := semanticBodyHandler(true)
	body := semanticEscapedRequest(t)
	// Generation zero exits after actual handler admission, before storage.
	*body.ClaimGeneration = 0
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= 32<<20 || len(raw) >= 64<<20 {
		t.Fatalf("regression envelope bytes=%d", len(raw))
	}
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	route := chi.NewRouteContext()
	route.URLParams.Add("id", uuid.NewString())
	req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New()}))
	rec := httptest.NewRecorder()
	h.WorkerSemanticCandidate(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid semantic candidate") {
		t.Fatalf("valid escaped envelope refused before generation validation: %d %s", rec.Code, rec.Body.String())
	}
	t.Logf("field-valid envelope admitted: proposal=%d request=%d", len(body.Proposal), len(raw))
}

type semanticCountingReader struct{ bytes int64 }

func (r *semanticCountingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	r.bytes += int64(len(p))
	return len(p), nil
}

func TestSemanticWorkerImmediateBodyBound(t *testing.T) {
	reader := &semanticCountingReader{}
	req := httptest.NewRequest(http.MethodPost, "/", io.NopCloser(reader))
	rec := httptest.NewRecorder()
	var body semanticCandidateRequest
	if readSemanticWorkerBody(rec, req, 64<<20, &body) || rec.Code != http.StatusBadRequest {
		t.Fatal("oversized body admitted")
	}
	if reader.bytes != (64<<20)+1 {
		t.Fatalf("reader consumed %d bytes", reader.bytes)
	}
}

func TestSemanticWorkerAuthentication(t *testing.T) {
	h := &Handler{}
	for _, call := range []func(http.ResponseWriter, *http.Request){
		h.WorkerSemanticCandidate, h.WorkerSemanticCoverageStatus, h.WorkerSemanticCoverageReserve, h.WorkerSemanticCoverageSettle, h.WorkerSemanticCoveragePublish,
	} {
		rec := httptest.NewRecorder()
		call(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status=%d", rec.Code)
		}
	}
}

func TestSemanticWorkerStrictEnvelope(t *testing.T) {
	h, _ := semanticBodyHandler(true)
	for _, call := range []func(http.ResponseWriter, *http.Request){
		h.WorkerSemanticCandidate, h.WorkerSemanticCoverageReserve, h.WorkerSemanticCoverageSettle, h.WorkerSemanticCoveragePublish,
	} {
		for _, body := range []string{
			"{}", "null", `{"claim_generation":1,"claim_generation":2}`,
			`{"Claim_generation":1}`, `{"claim_generation":1,"nested":{"a":1,"a":2}}`,
			`{"claim_generation":1,"nested":{"a":"\ud800"}}`,
			`{"claim_generation":null}`, `{"claim_generation":1} {}`,
			string([]byte{'{', '"', 0xff, '"', '}'}), strings.Repeat(" ", (4<<20)+1),
		} {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			route := chi.NewRouteContext()
			route.URLParams.Add("id", uuid.NewString())
			req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New()}))
			rec := httptest.NewRecorder()
			call(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%.80s", rec.Code, body)
			}
		}
	}
}

func TestSemanticWorkerStatusGeneration(t *testing.T) {
	h := &Handler{}
	for _, query := range []string{"", "?claim_generation=0", "?claim_generation=-1", "?claim_generation=x", "?claim_generation=1&claim_generation=2", "?claim_generation=1&extra=x"} {
		req := httptest.NewRequest(http.MethodGet, "/"+query, nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", uuid.NewString())
		req = req.WithContext(mw.ContextWithWorker(context.WithValue(req.Context(), chi.RouteCtxKey, route), store.Worker{ID: uuid.New()}))
		rec := httptest.NewRecorder()
		h.WorkerSemanticCoverageStatus(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%s status=%d", query, rec.Code)
		}
	}
}
