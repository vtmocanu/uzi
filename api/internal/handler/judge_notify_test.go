package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestBuildReviewNotificationStructuredOnly is the M2-audit-headline guard for the
// judge producer (PRD #46 M4): the notification the judge fires must copy NO judge
// free text into the verbatim payload path. The summary + each rationale are given
// deliberately-distinctive sentinels; neither may appear anywhere in the marshaled
// notification (payload OR Slack render) — only the verdict enum + recommendation
// count do. The deep link is server-built from the base URL + target UUID.
func TestBuildReviewNotificationStructuredOnly(t *testing.T) {
	owner, target, reviewID := uuid.New(), uuid.New(), uuid.New()
	const summarySentinel = "SUMMARY-SENTINEL-should-not-leak"
	const rationaleSentinel = "RATIONALE-SENTINEL-should-not-leak"
	const targetSentinel = "TARGET-SENTINEL-should-not-leak"
	sub := workersvc.ReviewSubmission{
		Verdict:   "issues",
		SummaryMd: summarySentinel,
		Status:    "complete",
		Recommendations: []workersvc.ReviewRecommendation{
			{Category: "install_worker_tool", Target: targetSentinel, RationaleMd: rationaleSentinel, Confidence: "high"},
			{Category: "improve_uzi", Target: "x", RationaleMd: "y", Confidence: "low"},
		},
	}
	res := workersvc.ReviewResult{OwnerID: owner, ReviewID: reviewID}

	n := buildReviewNotification("https://uzi.example.com/", target, res, sub)

	if n.UserID != owner {
		t.Errorf("notification owner = %v, want %v (owner-only)", n.UserID, owner)
	}
	if n.Kind != judgeReviewNotificationKind {
		t.Errorf("kind = %q, want %q", n.Kind, judgeReviewNotificationKind)
	}
	if n.RunID == nil || *n.RunID != target {
		t.Errorf("run anchor = %v, want %v", n.RunID, target)
	}
	if n.ReviewID == nil || *n.ReviewID != reviewID {
		t.Errorf("review anchor = %v, want %v", n.ReviewID, reviewID)
	}
	// Deep link is server-built from the operator base + target UUID, trailing slash
	// trimmed — never any LLM text.
	wantLink := "https://uzi.example.com/runs/" + target.String()
	if n.Slack == nil || n.Slack.Link != wantLink {
		t.Errorf("slack link = %v, want %q", n.Slack, wantLink)
	}

	// The whole notification, marshaled, must not contain any judge free text.
	blob, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	for _, sentinel := range []string{summarySentinel, rationaleSentinel, targetSentinel} {
		if strings.Contains(string(blob), sentinel) {
			t.Errorf("notification leaked judge free text %q into the payload path: %s", sentinel, blob)
		}
	}

	// The structured body carries the verdict enum + the recommendation count.
	payload, ok := n.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map", n.Payload)
	}
	if payload["verdict"] != "issues" {
		t.Errorf("payload verdict = %v, want issues", payload["verdict"])
	}
	if payload["recommendation_count"] != 2 {
		t.Errorf("payload recommendation_count = %v, want 2", payload["recommendation_count"])
	}
	body, _ := payload["body"].(string)
	if !strings.Contains(body, "issues") || !strings.Contains(body, "2 recommendations") {
		t.Errorf("body = %q, want the verdict + count", body)
	}
}

// TestBuildReviewNotificationNoBaseURL: an unset/empty public base URL yields no deep
// link (rather than a bare "/runs/..." path), so the notification simply has no link.
func TestBuildReviewNotificationNoBaseURL(t *testing.T) {
	sub := workersvc.ReviewSubmission{Verdict: "ideal", Status: "complete"}
	n := buildReviewNotification("  ", uuid.New(), workersvc.ReviewResult{OwnerID: uuid.New()}, sub)
	if n.Slack == nil || n.Slack.Link != "" {
		t.Errorf("slack link = %v, want empty (no base URL)", n.Slack)
	}
	// One-recommendation singular vs plural wording.
	single := reviewNotificationBody("ok", 1)
	if !strings.Contains(single, "1 recommendation") || strings.Contains(single, "recommendations") {
		t.Errorf("singular body = %q, want '1 recommendation'", single)
	}
}
