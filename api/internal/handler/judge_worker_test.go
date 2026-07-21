package handler

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// validReview is the happy-path review the negative cases each break one field of.
func validReview() workerReviewRequest {
	return workerReviewRequest{
		Verdict: "issues",
		Summary: "The run could not find a tool.",
		Model:   "haiku",
		Status:  "complete",
		Recommendations: []workerReviewRec{
			{Category: "install_worker_tool", Target: "shellcheck", Rationale: "missing on the worker", Confidence: "high"},
		},
	}
}

func TestValidateAndScrubReviewAccepts(t *testing.T) {
	sub, err := validateAndScrubReview(validReview())
	if err != nil {
		t.Fatalf("valid review rejected: %v", err)
	}
	if sub.Verdict != "issues" || sub.Status != "complete" || len(sub.Recommendations) != 1 {
		t.Fatalf("unexpected submission: %+v", sub)
	}
}

func TestValidateAndScrubReviewDefaultsStatus(t *testing.T) {
	req := validReview()
	req.Status = ""
	sub, err := validateAndScrubReview(req)
	if err != nil || sub.Status != "complete" {
		t.Fatalf("empty status should default to complete, got %q err=%v", sub.Status, err)
	}
}

func TestValidateAndScrubReviewRejectsBadEnums(t *testing.T) {
	cases := map[string]func(r *workerReviewRequest){
		"bad verdict":    func(r *workerReviewRequest) { r.Verdict = "brilliant" },
		"bad status":     func(r *workerReviewRequest) { r.Status = "pending" },
		"bad category":   func(r *workerReviewRequest) { r.Recommendations[0].Category = "rewrite_everything" },
		"bad confidence": func(r *workerReviewRequest) { r.Recommendations[0].Confidence = "certain" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := validReview()
			mutate(&req)
			if _, err := validateAndScrubReview(req); err == nil {
				t.Fatalf("%s should be rejected", name)
			}
		})
	}
}

func TestValidateAndScrubReviewRejectsTooManyRecs(t *testing.T) {
	req := validReview()
	req.Recommendations = nil
	for i := 0; i < 100; i++ {
		req.Recommendations = append(req.Recommendations, workerReviewRec{Category: "improve_uzi", Rationale: "x"})
	}
	if _, err := validateAndScrubReview(req); err == nil {
		t.Fatal("an over-cap recommendation list should be rejected")
	}
}

func TestValidateAndScrubReviewCapsAndStripsControl(t *testing.T) {
	req := validReview()
	// Oversize summary with an embedded terminal escape and a newline.
	req.Summary = "line1\n\x1b[31mred\x1b[0m\ttabbed" + strings.Repeat("A", 20000)
	sub, err := validateAndScrubReview(req)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(sub.SummaryMd) > 8*1024 {
		t.Errorf("summary not capped: %d bytes", len(sub.SummaryMd))
	}
	if strings.Contains(sub.SummaryMd, "\x1b") {
		t.Error("terminal escape not stripped from summary")
	}
	// Markdown newline/tab preserved.
	if !strings.Contains(sub.SummaryMd, "\n") || !strings.Contains(sub.SummaryMd, "\t") {
		t.Error("markdown newline/tab should be preserved in the summary")
	}
}

func TestValidateAndScrubReviewScrubsSecrets(t *testing.T) {
	req := validReview()
	req.Summary = "the agent logged a token glpat-ABCDEF1234567890 in its output"
	req.Recommendations[0].Rationale = "also saw sk-ant-SECRETKEY0001 here"
	sub, err := validateAndScrubReview(req)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if strings.Contains(sub.SummaryMd, "glpat-") {
		t.Errorf("gitlab PAT not scrubbed from summary: %q", sub.SummaryMd)
	}
	if strings.Contains(sub.Recommendations[0].RationaleMd, "sk-ant-") {
		t.Errorf("anthropic key not scrubbed from rationale: %q", sub.Recommendations[0].RationaleMd)
	}
}

// ---- the Slack deep link (PRD #98 M5, Decision 4) --------------------------------------

// TestReviewDeepLinkTargetsTheJudgeWorkbench pins the retarget the judge DM carries. Before
// M5 this function had NO test at all, which is the only reason the change is worth its own
// case: an untested URL builder is a printed instruction to a human, and this branch has
// already shipped two of those that had never been executed and were false.
//
// The base-URL handling is asserted alongside the path because those are the two ways this
// string goes wrong in production and neither is visible from a caller: a trailing slash
// produces `//judge`, and an unset base must produce "" so the DM carries no link at all
// rather than a link to nowhere.
func TestReviewDeepLinkTargetsTheJudgeWorkbench(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")

	got := reviewDeepLink("https://uzi.example", id)
	want := "https://uzi.example/judge?run=11111111-2222-3333-4444-555555555555"
	if got != want {
		t.Errorf("reviewDeepLink = %q, want %q", got, want)
	}
	// The anchor is what makes the destination usable: /judge alone lands on the whole
	// backlog, where a fresh review's recommendations may be deduped in among many runs'.
	if !strings.Contains(got, "?run=") {
		t.Errorf("deep link %q carries no ?run= anchor — the DM would open the unfiltered backlog", got)
	}
	// ...and it must NOT still be the run page. Asserted explicitly rather than trusting the
	// equality above, because that is the regression a future edit reintroduces.
	if strings.Contains(got, "/runs/") {
		t.Errorf("deep link %q still points at the run page", got)
	}

	if got := reviewDeepLink("https://uzi.example/", id); got != want {
		t.Errorf("trailing slash: reviewDeepLink = %q, want %q", got, want)
	}
	for _, base := range []string{"", "   "} {
		if got := reviewDeepLink(base, id); got != "" {
			t.Errorf("base %q: reviewDeepLink = %q, want \"\" (no link rather than a broken one)", base, got)
		}
	}
}
