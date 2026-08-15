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
	// trimmed — never any LLM text. It points at the Judge workbench anchored to this run
	// (PRD #98 M5, Decision 4), not at the run page; reviewDeepLink's own test covers the
	// base-URL edge cases, and this one pins that the notification the worker builds
	// actually carries that link.
	wantLink := "https://uzi.example.com/judge?run=" + target.String()
	if n.Slack == nil || n.Slack.Link != wantLink {
		t.Errorf("slack link = %v, want %q", n.Slack, wantLink)
	}
	// PRD #268 M3: the Slack render carries the emoji + trusted facts, and the Body is the
	// scrubbed summary preview alone (not the verdict/count one-liner — those are facts now).
	if n.Slack.Emoji != "🔎" {
		t.Errorf("slack emoji = %q, want 🔎", n.Slack.Emoji)
	}
	if n.Slack.Body != n.Payload.(map[string]any)["summary"] {
		t.Errorf("slack body = %q, want the scrubbed summary preview", n.Slack.Body)
	}
	if strings.Contains(n.Slack.Body, secret) {
		t.Errorf("slack body leaked a secret: %q", n.Slack.Body)
	}
	wantFacts := []string{"Verdict ⚠️ *issues*", "2 recommendations", "`install_worker_tool`", "`improve_uzi`"}
	if len(n.Slack.Facts) != len(wantFacts) {
		t.Fatalf("slack facts = %v, want %v", n.Slack.Facts, wantFacts)
	}
	for i, w := range wantFacts {
		if n.Slack.Facts[i] != w {
			t.Errorf("slack fact[%d] = %q, want %q", i, n.Slack.Facts[i], w)
		}
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

// verdictGlyph maps every live verdict (ideal|ok|issues) plus the PRD #268 M3
// forward-compat keys; an unknown value degrades to "" (no glyph).
func TestVerdictGlyph(t *testing.T) {
	for verdict, want := range map[string]string{
		"ideal":         "✅",
		"ok":            "✅",
		"issues":        "⚠️",
		"needs_changes": "⚠️",
		"needs-changes": "⚠️",
		"bad":           "❌",
		"":              "",
		"who_knows":     "",
	} {
		if got := verdictGlyph(verdict); got != want {
			t.Errorf("verdictGlyph(%q) = %q, want %q", verdict, got, want)
		}
	}
}

// recommendationCategoryChips dedupes categories in first-seen order and wraps each in
// a `code` chip; with in-enum data, dedup (not the maxChips backstop) is what bounds the
// result to the distinct categories present. recCountFact does singular/plural.
func TestReviewSlackFactsChipsAndCount(t *testing.T) {
	if got := recCountFact(1); got != "1 recommendation" {
		t.Errorf("recCountFact(1) = %q, want '1 recommendation'", got)
	}
	if got := recCountFact(3); got != "3 recommendations" {
		t.Errorf("recCountFact(3) = %q, want '3 recommendations'", got)
	}
	// Duplicate categories collapse to one chip, in first-seen order.
	recs := []workersvc.ReviewRecommendation{
		{Category: "improve_agent"}, {Category: "improve_uzi"}, {Category: "improve_agent"},
	}
	chips := recommendationCategoryChips(recs)
	if len(chips) != 2 || chips[0] != "`improve_agent`" || chips[1] != "`improve_uzi`" {
		t.Errorf("chips = %v, want the two distinct categories as `code` chips in first-seen order", chips)
	}
	// Many recommendations drawn from the closed enum collapse to exactly the DISTINCT
	// categories present — dedup, not the cap, is what bounds the chips for in-enum data
	// (the enum has 7 members, so a review can never carry more than 7 distinct ones).
	// A submission that repeats every category many times must yield one chip per
	// distinct category, in first-seen order.
	distinct := []string{"enable_tool", "install_worker_tool", "adjust_template", "improve_agent", "add_agent", "improve_uzi", "cost_efficiency"}
	many := make([]workersvc.ReviewRecommendation, 0, len(distinct)*4)
	for i := 0; i < 4; i++ {
		for _, cat := range distinct {
			many = append(many, workersvc.ReviewRecommendation{Category: cat})
		}
	}
	chipsMany := recommendationCategoryChips(many)
	if len(chipsMany) != len(distinct) {
		t.Fatalf("chip count = %d, want %d distinct categories deduped to one chip each", len(chipsMany), len(distinct))
	}
	for i, cat := range distinct {
		if chipsMany[i] != "`"+cat+"`" {
			t.Errorf("chip[%d] = %q, want %q (dedup preserves first-seen order)", i, chipsMany[i], "`"+cat+"`")
		}
	}
}

// TestReviewSummaryPreviewPreservesNewlines pins PRD #292 M4: the summary preview now
// keeps its newlines (so the Slack blockquote/list structure survives when rendered as
// mrkdwn), stays secret-scrubbed, and still honors the 600-rune cap.
func TestReviewSummaryPreviewPreservesNewlines(t *testing.T) {
	const secret = "sk-ant-api03-DEADBEEFsecretkeyvalue" // ScrubSecrets redacts this Anthropic-family token
	multi := "**Summary line**\n- point one\n- leaked " + secret + "\n> quoted"
	got := reviewSummaryPreview(multi)
	if !strings.Contains(got, "\n") {
		t.Errorf("preview dropped newlines: %q", got)
	}
	if strings.Contains(got, secret) {
		t.Errorf("preview leaked a secret: %q", got)
	}
	// The multi-line structure (bold line, bullets, blockquote) survives verbatim except
	// for the scrubbed secret.
	if !strings.Contains(got, "**Summary line**") || !strings.Contains(got, "- point one") || !strings.Contains(got, "> quoted") {
		t.Errorf("preview lost markdown structure: %q", got)
	}

	// The 600-rune cap still applies to the multi-line text, appending the ellipsis.
	long := strings.Repeat("a\n", 700) // 1400 runes, well over the cap
	capped := reviewSummaryPreview(long)
	runes := []rune(capped)
	if len(runes) != reviewSummaryPreviewMaxRunes+1 || runes[len(runes)-1] != '…' {
		t.Errorf("capped preview = %d runes, want %d + ellipsis", len(runes), reviewSummaryPreviewMaxRunes)
	}

	if reviewSummaryPreview("   \n  \n ") != "" {
		t.Errorf("whitespace-only preview should be empty")
	}
}

// TestReviewNotificationBodyCollapsesNewlines pins Decision 7 / SC4: even when the
// summary is multi-line (as it now is), the web-inbox one-liner collapses its
// whitespace/newlines to single spaces so Payload["body"] carries no newline.
func TestReviewNotificationBodyCollapsesNewlines(t *testing.T) {
	multi := "line1\nline2\n\n  line3"
	body := reviewNotificationBody("ok", 1, multi)
	if strings.Contains(body, "\n") {
		t.Errorf("inbox body kept a newline: %q", body)
	}
	if !strings.Contains(body, "verdict: ok") || !strings.Contains(body, "1 recommendation") {
		t.Errorf("inbox body missing verdict/count: %q", body)
	}
	if !strings.Contains(body, "line1 line2 line3") {
		t.Errorf("inbox body did not collapse the multi-line summary to spaces: %q", body)
	}
}
