package handler

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func checkerResponseFixture(t *testing.T) *workersvc.ClaimPayload {
	t.Helper()
	c := workersvc.PlanCrossCheckCandidate{PlanMd: "plan", Milestones: json.RawMessage("[]"), SizeClass: "s", BaseCommit: strings.Repeat("a", 40)}
	digest, err := c.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return &workersvc.ClaimPayload{Kind: "cross_check", RunID: uuid.NewString(), CrossCheck: &workersvc.ClaimPlanCrossCheck{
		Stage: "plan", Round: 1, LeadRunID: uuid.NewString(), CandidateDigest: hex.EncodeToString(digest), DeadlineAt: time.Now().Add(time.Minute), PlanCrossCheckCandidate: c}}
}

func TestCheckerClaimResponseEnvelopeAndMarker(t *testing.T) {
	p := checkerResponseFixture(t)
	p.IssueDescription = strings.Repeat("<", 262144)
	branch := ""
	p.Branch = &branch
	base, err := workersvc.MarshalCrossCheckClaim(p)
	if err != nil {
		t.Fatal(err)
	}
	branch = strings.Repeat("x", (2<<20)-len(base))
	rec := httptest.NewRecorder()
	writeWorkerRunClaim(rec, p)
	assertUnmarkedClaimBytes(t, rec, p)
	if rec.Code != http.StatusOK || rec.Body.Len() != 2<<20 || rec.Header().Get("X-Uzi-Claim-Kind") != "cross_check" {
		t.Fatalf("exact-bound response: status=%d bytes=%d marker=%q", rec.Code, rec.Body.Len(), rec.Header().Get("X-Uzi-Claim-Kind"))
	}
	var decoded workersvc.ClaimPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil || decoded.Kind != "cross_check" {
		t.Fatal("checker response kind mismatch")
	}
	branch += "x"
	rec = httptest.NewRecorder()
	writeWorkerRunClaim(rec, p)
	assertUnmarkedClaimBytes(t, rec, p)
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("X-Uzi-Claim-Kind") != "" || rec.Body.Len() > 1024 {
		t.Fatal("oversized checker envelope escaped the independent kind guard")
	}
	// Unmarked ordinary claims retain their existing JSON response behavior.
	p.Kind = "issue"
	p.CrossCheck = nil
	p.IssueTitle = strings.Repeat("x", 4097)
	rec = httptest.NewRecorder()
	writeWorkerRunClaim(rec, p)
	assertUnmarkedClaimBytes(t, rec, p)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Uzi-Claim-Kind") != "" || rec.Body.Len() <= 2<<20 {
		t.Fatal("ordinary response changed")
	}
	p.Kind = "cross_check"
	rec = httptest.NewRecorder()
	writeWorkerRunClaim(rec, p)
	assertUnmarkedClaimBytes(t, rec, p)
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("X-Uzi-Claim-Kind") != "" {
		t.Fatal("checker without input delivered")
	}
}

func assertUnmarkedClaimBytes(t *testing.T, rec *httptest.ResponseRecorder, p *workersvc.ClaimPayload) {
	t.Helper()
	if rec.Header().Get("X-Uzi-Candidate-Identity-Version") != "" {
		t.Fatal("legacy or error response emitted semantic marker")
	}
	if rec.Code == http.StatusOK {
		raw, err := json.Marshal(p)
		if err != nil || !bytes.Equal(rec.Body.Bytes(), append(raw, '\n')) {
			t.Fatal("legacy response bytes changed")
		}
	}
}

func TestCodeCheckerClaimResponseKeepsLegacyBytes(t *testing.T) {
	p := checkerResponseFixture(t)
	p.CrossCheck.Stage = "code"
	p.CrossCheck.HeadCommit = strings.Repeat("b", 40)
	p.CrossCheck.CodeContext = json.RawMessage(`{"issue_title":"","issue_description":""}`)
	// Recorded digest for this fixed legacy code candidate.
	p.CrossCheck.CandidateDigest = "4a89fd146fc716236e990fd1bb8b44beb515b7e45768f19c14848c8c2ce91351"
	rec := httptest.NewRecorder()
	writeWorkerRunClaim(rec, p)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Uzi-Claim-Kind") != "cross_check" {
		t.Fatalf("code claim: %d %s", rec.Code, rec.Body.String())
	}
	assertUnmarkedClaimBytes(t, rec, p)
}

func TestCrossCheckStatusLegacyAndErrorRemainUnmarked(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status workersvc.PlanCrossCheckStatusResult
		err    error
	}{
		{name: "structural"},
		{name: "no_row", err: workersvc.ErrCrossCheckNoRow},
		{name: "parked", status: workersvc.PlanCrossCheckStatusResult{Parked: true, Reconciliation: &workersvc.LeadReconciliation{}}},
		{name: "error", err: workersvc.ErrCrossCheckRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeCrossCheckStatus(rec, tc.status, tc.err)
			if rec.Header().Get("X-Uzi-Candidate-Identity-Version") != "" {
				t.Fatal("non-semantic status emitted marker")
			}
			if tc.name == "structural" {
				want, err := json.Marshal(crossCheckResponse(tc.status.CrossCheck, tc.status.LeadLastSeq))
				if err != nil || !bytes.Equal(rec.Body.Bytes(), append(want, '\n')) {
					t.Fatal("structural status bytes changed")
				}
			}
		})
	}
}
