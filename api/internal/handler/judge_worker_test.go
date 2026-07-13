package handler

import (
	"strings"
	"testing"
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
