package workersvc

import (
	"context"
	"fmt"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
)

// tooLargeSummaryForge refuses the merge-request summary as oversized (#3226) and
// fails the test if the proof reads anything after that refusal.
type tooLargeSummaryForge struct {
	forgetest.BaseFake
	t *testing.T
}

func (f *tooLargeSummaryForge) GetMergeRequestSummary(context.Context, int64, int64) (forge.MergeRequestSummary, error) {
	return forge.MergeRequestSummary{}, fmt.Errorf("gitlab: get merge request summary: %w", &forge.ResponseTooLargeError{})
}

func (f *tooLargeSummaryForge) BranchHead(context.Context, int64, string) (string, error) {
	f.t.Fatal("BranchHead read after the summary was refused")
	return "", nil
}

// An oversized merge-request summary is unverified evidence: no receipt head, and the
// same retryable reason as any other forge error, never mr_missing.
func TestProveCompletedPublicationTooLargeSummary(t *testing.T) {
	iid := int64(9)
	id := apitypes.CompletionIdentity{
		ProjectID: 7,
		MRIID:     &iid,
		Branch:    "agent/issue-1",
		FinalHead: "0123456789abcdef0123456789abcdef01234567",
	}
	head, reason := proveCompletedPublication(context.Background(), &tooLargeSummaryForge{t: t}, id)
	if head != "" || reason != "ancestry_unknown" {
		t.Fatalf("proveCompletedPublication = (%q, %q), want (\"\", \"ancestry_unknown\")", head, reason)
	}
}
