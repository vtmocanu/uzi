package handler

import (
	"fmt"
	"strings"
	"testing"
	"unicode"

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

// Issue #124 at the SOURCE. Cc and Cf are DISJOINT categories, so the IsControl-only
// predicate this replaced never saw a bidi override — and judge output is LLM-derived text
// influenced by whatever the run looked at, so an approving sentence could be persisted in a
// form that RENDERS inside a rejecting review. Trojan Source (CVE-2021-42574).
//
// Both scrubbers on this path are exercised here, because they are different functions with
// different whitespace postures and only one of them carries the filename:
// termsafe.SanitizeBounded for Summary/Rationale (keeps \n and \t), sanitizeSelfReported for
// Target and Model (single-line, keeps neither). Corpus mirrored from
// TestSanitizeTTYStripsControlAndFormatChars (api/cmd/uzi/tui_render_test.go:61).
func TestValidateAndScrubReviewStripsFormatChars(t *testing.T) {
	req := validReview()
	req.Summary = "The review \u202Eapproved\u202C this\u200B change"
	req.Model = "haiku\u200D-x"
	req.Recommendations[0].Rationale = "line1\nline2\ttabbed \u2066spoofed\u2069"
	// The headline case: a target is a FILENAME, so a surviving override lets a review name
	// one file while pointing at another. The bidi override is stripped by
	// sanitizeSelfReported, which runs BEFORE canonicalizeTarget (issue #232) \u2014 so the
	// no-Cf-survives property below still holds on the target even though canonicalization
	// additionally folds the path punctuation into spaces (asserted by value further down).
	req.Recommendations[0].Target = "api/internal/\u202Eforge/gitlab.go"
	sub, err := validateAndScrubReview(req)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	// The PROPERTY, over every free-text field the submission carries: nothing that
	// survives is Cc or Cf, except the two whitespace characters the markdown fields keep.
	// Asserted as a property rather than per-codepoint because a table only ever covers
	// what someone thought of.
	fields := map[string]string{"summary": sub.SummaryMd, "model": sub.JudgeModel}
	for i, rec := range sub.Recommendations {
		fields[fmt.Sprintf("rec[%d].target", i)] = rec.Target
		fields[fmt.Sprintf("rec[%d].rationale", i)] = rec.RationaleMd
	}
	for name, v := range fields {
		for _, r := range v {
			if r == '\n' || r == '\t' {
				continue
			}
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				t.Errorf("%s: %U survived ingest (value %q)", name, r, v)
			}
		}
	}

	// …and the text is still the text, not mangled: stripping must remove the invisible
	// characters and nothing else.
	if sub.SummaryMd != "The review approved this change" {
		t.Errorf("summary = %q, want the same prose with the format chars gone", sub.SummaryMd)
	}
	// The override is gone (the honesty fix) AND the target is now CANONICAL (issue #232):
	// casing/whitespace/ASCII-punctuation folded, so "api/internal/forge/gitlab.go" collapses
	// to space-separated tokens. This is the (category, target) coordinate the cross-run dedup
	// keys on; the fold is what makes two runs' cosmetically-different phrasings one row.
	if sub.Recommendations[0].Target != "api internal forge gitlab go" {
		t.Errorf("target = %q, want the canonicalized coordinate", sub.Recommendations[0].Target)
	}
	if sub.JudgeModel != "haiku-x" {
		t.Errorf("model = %q, want %q", sub.JudgeModel, "haiku-x")
	}
	// The whitespace posture is per-function and must not drift: the MARKDOWN field keeps
	// \n and \t (its sink is whitespace-pre-wrap), which is the whole reason
	// termsafe.SanitizeBounded is separate from the single-line sanitizeSelfReported.
	if !strings.Contains(sub.Recommendations[0].RationaleMd, "\n") ||
		!strings.Contains(sub.Recommendations[0].RationaleMd, "\t") {
		t.Errorf("rationale lost its newline/tab: %q", sub.Recommendations[0].RationaleMd)
	}
	// Trim order, same defect as sanitizeSelfReported: an edge Cf shielded the adjacent
	// space from the TrimSpace that runs before the strip. Cosmetic on a pre-wrap sink,
	// fixed for consistency — two scrubbers three files apart disagreeing on this is how
	// the next reader concludes one of them is wrong.
	reqTrim := validReview()
	reqTrim.Summary = "\u200b  a summary with padded edges  \u200b"
	reqTrim.Recommendations[0].Rationale = "  plain surrounding spaces  "
	subTrim, err := validateAndScrubReview(reqTrim)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if subTrim.SummaryMd != "a summary with padded edges" {
		t.Errorf("Cf-padded summary kept its edge whitespace: %q", subTrim.SummaryMd)
	}
	// The control that makes the row above a defect rather than a choice.
	if subTrim.Recommendations[0].RationaleMd != "plain surrounding spaces" {
		t.Errorf("plain whitespace must still trim: %q", subTrim.Recommendations[0].RationaleMd)
	}

	// Cn/Co are NOT stripped — the predicate is Cc|Cf, not the whole C category. U+E000 is
	// private use, which Unicode will never assign, so this fixture cannot rot.
	req2 := validReview()
	req2.Summary = "a\ue000b"
	sub2, err := validateAndScrubReview(req2)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if sub2.SummaryMd != "a\ue000b" {
		t.Errorf("a private-use code point must survive: got %q", sub2.SummaryMd)
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
