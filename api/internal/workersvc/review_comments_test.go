package workersvc

import (
	"testing"

	"github.com/vtmocanu/uzi/api/internal/forge"
)

// TestIsActionableReviewComment covers the #1142 trigger classifier: inline findings
// (including a third-party review bot's) always count; a bot walkthrough/summary note
// and a marker-carrying summary from a non-bot login (the GitLab/Forgejo CodeRabbit
// shape) do not; a human top-level note and an unknown review state stay actionable.
func TestIsActionableReviewComment(t *testing.T) {
	summaryMarker := "<!-- This is an auto-generated comment: summarize by coderabbit.ai -->\n\nNo actionable comments were generated."
	walkthroughMarker := "<!-- walkthrough_start -->\n\nWalkthrough."
	cases := []struct {
		name string
		c    ReviewCommentSnapshot
		want bool
	}{
		{"inline human finding", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentInline, AuthorUsername: "carol", Body: "guard nil"}, true},
		{"inline bot finding is actionable", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentInline, AuthorUsername: "coderabbitai[bot]", Body: "guard nil"}, true},
		{"inline CodeRabbit command stays actionable", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentInline, AuthorUsername: "maintainer", Body: "@coderabbitai review"}, true},
		{"summary bot walkthrough by [bot] login", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "coderabbitai[bot]", Body: "here is what changed"}, false},
		{"summary with coderabbit summarize marker, non-bot login", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "coderabbit", Body: summaryMarker}, false},
		{"summary with walkthrough_start marker, non-bot login", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "coderabbit", Body: walkthroughMarker}, false},
		{"human CodeRabbit rate-limit command", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@coderabbitai rate limit"}, false},
		{"human CodeRabbit review command normalizes case and whitespace", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "  @CodeRabbitAI\n review  "}, false},
		{"human CodeRabbit full-review command", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@coderabbitai full review"}, false},
		{"human CodeRabbit quota alias", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@coderabbitai reviews remaining?"}, false},
		{"human CodeRabbit command plus prose", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@coderabbitai review please also rename X"}, true},
		{"human Greptile review command", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@greptileai review"}, false},
		{"human Greptile short-handle review command", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@greptile review"}, false},
		{"human Greptile review command normalizes case and whitespace", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "  @GreptileAI\n review  "}, false},
		{"human Greptile command plus prose", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@greptileai review, also rename X"}, true},
		{"bare Greptile mention stays actionable", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "@greptileai"}, true},
		{"inline Greptile command stays actionable", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentInline, AuthorUsername: "maintainer", Body: "@greptileai review"}, true},
		{"human top-level note", ReviewCommentSnapshot{ReviewState: forge.ReviewCommentSummary, AuthorUsername: "maintainer", Body: "please also rename X"}, true},
		{"unknown review state defaults to actionable", ReviewCommentSnapshot{ReviewState: "", AuthorUsername: "coderabbitai[bot]", Body: summaryMarker}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsActionableReviewComment(tc.c); got != tc.want {
				t.Fatalf("IsActionableReviewComment(%+v) = %v, want %v", tc.c, got, tc.want)
			}
		})
	}
}
