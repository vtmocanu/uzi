package handler

import (
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
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("X-Uzi-Claim-Kind") != "" || rec.Body.Len() > 1024 {
		t.Fatal("oversized checker envelope escaped the independent kind guard")
	}
	// Unmarked ordinary claims retain their existing JSON response behavior.
	p.Kind = "issue"
	p.CrossCheck = nil
	p.IssueTitle = strings.Repeat("x", 4097)
	rec = httptest.NewRecorder()
	writeWorkerRunClaim(rec, p)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Uzi-Claim-Kind") != "" || rec.Body.Len() <= 2<<20 {
		t.Fatal("ordinary response changed")
	}
	p.Kind = "cross_check"
	rec = httptest.NewRecorder()
	writeWorkerRunClaim(rec, p)
	if rec.Code != http.StatusInternalServerError || rec.Header().Get("X-Uzi-Claim-Kind") != "" {
		t.Fatal("checker without input delivered")
	}
}
