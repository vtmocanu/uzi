package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestPolicyOwnerStopPresentedHeaderLiveDB(t *testing.T) {
	f := newPolicyStopConfirmEnv(t, 1, "revise")
	review := f.review()
	if len(review.Manifest.HumanFeedback) != 1 || len(review.Manifest.CrossCheckFeedback) != 1 {
		t.Fatalf("review omitted seeded feedback: %+v", review.Manifest)
	}
	human, automatic := review.Manifest.HumanFeedback[0], review.Manifest.CrossCheckFeedback[0]
	if string(human.InputID) != strconv.FormatInt(f.humanID, 10) || human.Body == nil || *human.Body != "  exact human body\n\n" ||
		automatic.CrossCheckID != f.ccID.String() || automatic.Verdict != "revise" || !automatic.Supersede {
		t.Fatalf("review changed feedback identity or content: %+v", review.Manifest)
	}
	var findings []struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(automatic.Findings, &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Body != "exact finding\n" {
		t.Fatalf("review omitted complete automatic feedback: %s", automatic.Findings)
	}
	f.reject(f.owner, "", http.StatusConflict, "policy_stop_token_required")
	f.reject(f.owner, "stale", http.StatusConflict, "policy_stop_token_stale")
	f.reject(f.owner, f.holdToken, http.StatusConflict, "policy_stop_token_stale")
	f.setFeature(false)
	f.reject(f.owner, review.StopIfMatch, http.StatusConflict, "policy_stop_review_unverifiable")
	f.setFeature(true)
	rec := f.stop(f.owner, review.StopIfMatch)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid reviewed Stop: status=%d body=%s", rec.Code, rec.Body)
	}
	var response apitypes.RunInputResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Resumed || response.PolicyStop == nil {
		t.Fatalf("Stop omitted resumed acceptance receipt: %s", rec.Body)
	}
	receipt := *response.PolicyStop
	if receipt.Disposition != "accepted" || receipt.ScopeCeiling != 1 || receipt.AcceptedAt.IsZero() {
		t.Fatalf("missing accepted receipt: %+v", receipt)
	}
	if f.scalar(`SELECT (policy_stop_resolution->>'resolution_id'=$2
 AND policy_stop_resolution->>'scope_input_id'=$3)::text FROM runs WHERE id=$1`,
		f.id, receipt.ResolutionID, string(receipt.ScopeInputID)) != "true" {
		t.Fatal("accepted receipt differs from durable resolution")
	}
	got, err := f.race.GetRunByID(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || !got.ScopeCeiling.Valid || got.ScopeCeiling.Int32 != 1 || got.PolicyRefusalCount != 1 || len(got.PolicyStopResolution) == 0 {
		t.Fatalf("Stop did not durably preserve and resume: status=%s ceiling=%v count=%d resolution=%s",
			got.Status, got.ScopeCeiling, got.PolicyRefusalCount, got.PolicyStopResolution)
	}
}
