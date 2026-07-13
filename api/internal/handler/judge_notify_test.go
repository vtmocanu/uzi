package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/workersvc"
)

// TestBuildReviewNotificationSummaryAndScrub covers the amended producer shape (PRD
// #46 M4): the notification carries the verdict, the SCRUBBED summary, and the
// recommendation count + category enums — but NEVER the recommendation target/rationale
// free text (those stay on the run page). A secret embedded in the summary is re-scrubbed
// at the producer (belt-and-suspenders over the ingest scrub), since the payload path is
// stored/served verbatim (audit M2 headline). The deep link is server-built.
func TestBuildReviewNotificationSummaryAndScrub(t *testing.T) {
	owner, target, reviewID := uuid.New(), uuid.New(), uuid.New()
	const rationaleSentinel = "RATIONALE-SENTINEL-should-not-leak"
	const targetSentinel = "TARGET-SENTINEL-should-not-leak"
	const secret = "sk-ant-api03-DEADBEEFsecretkeyvalue" // an Anthropic-family token ScrubSecrets redacts
	sub := workersvc.ReviewSubmission{
		Verdict:   "issues",
		SummaryMd: "Missing a tool and leaked " + secret + " into a log line",
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

	blob, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("marshal notification: %v", err)
	}
	// The secret is scrubbed everywhere in the payload path (belt and suspenders).
	if strings.Contains(string(blob), secret) {
		t.Errorf("notification leaked a secret into the payload path: %s", blob)
	}
	// Recommendation target/rationale free text is never copied into the notification.
	for _, sentinel := range []string{rationaleSentinel, targetSentinel} {
		if strings.Contains(string(blob), sentinel) {
			t.Errorf("notification leaked recommendation free text %q: %s", sentinel, blob)
		}
	}

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
	// The (scrubbed) summary is carried and surfaced in the body.
	summary, _ := payload["summary"].(string)
	if !strings.Contains(summary, "Missing a tool") || strings.Contains(summary, secret) {
		t.Errorf("payload summary = %q, want the scrubbed summary text", summary)
	}
	body, _ := payload["body"].(string)
	if !strings.Contains(body, "issues") || !strings.Contains(body, "2 recommendations") || !strings.Contains(body, "Missing a tool") {
		t.Errorf("body = %q, want verdict + count + summary", body)
	}
	// The recommendation category enums are carried (closed set, safe raw).
	cats, _ := payload["recommendation_categories"].([]string)
	if len(cats) != 2 || cats[0] != "install_worker_tool" || cats[1] != "improve_uzi" {
		t.Errorf("recommendation_categories = %v, want the two enums", cats)
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
	single := reviewNotificationBody("ok", 1, "")
	if !strings.Contains(single, "1 recommendation") || strings.Contains(single, "recommendations") {
		t.Errorf("singular body = %q, want '1 recommendation'", single)
	}
}
