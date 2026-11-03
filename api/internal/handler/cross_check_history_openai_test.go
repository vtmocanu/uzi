package handler

import (
	"strings"
	"testing"
)

func TestCrossCheckHistoryTextOpenAICompleteRemoval(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"classic", "sk-" + strings.Repeat("aB3d", 5) + "T3BlbkFJ" + strings.Repeat("eF7h", 5)},
		{"project-auditor", "sk-" + "proj-" + strings.Repeat("A", 74) + "T3BlbkFJ" + strings.Repeat("B", 74)},
		{"service-account", "sk-" + "svcacct-" + strings.Repeat("aB_9-", 14) + "T3BlbkFJ" + strings.Repeat("cD_8-", 14)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := crossCheckHistoryText("before "+tc.token+" after", 512); got != "before [redacted] after" {
				t.Fatal("history text must remove the complete OpenAI credential")
			}
			// Metadata uses the same scrub through historyOptional's 128-byte cap.
			got := historyOptional("before " + tc.token + " after")
			if got == nil || *got != "before [redacted] after" {
				t.Fatal("history metadata must remove the complete OpenAI credential")
			}
			for _, field := range []struct {
				name string
				max  int
			}{
				{"title512", 512},
				{"metadata128", 128},
			} {
				t.Run(field.name, func(t *testing.T) {
					for _, boundary := range []struct {
						name string
						room int
						tail string
					}{
						{"full-placeholder", 12, "[redacted] t"},
						{"partial-placeholder", 8, "[redacte"},
					} {
						t.Run(boundary.name, func(t *testing.T) {
							// The cap falls before the minimum token body can match.
							// Scrubbing first removes the entire token, then caps the
							// replacement and suffix; exact equality catches any fragment.
							keep := strings.Repeat("x", field.max-boundary.room)
							input := keep + tc.token + " tail"
							want := keep + boundary.tail
							got := crossCheckHistoryText(input, field.max)
							if got != want {
								t.Fatal("history text must scrub before bounding at the field cap")
							}
							if field.max == 128 {
								metadata := historyOptional(input)
								if metadata == nil || *metadata != want {
									t.Fatal("history metadata must scrub before its 128-byte bound")
								}
							}
						})
					}
				})
			}
		})
	}
}

func TestCrossCheckHistoryTextOverlappingCredentials(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"project-containing-slack", "sk-" + "proj-" + strings.Repeat("A", 8) + "xoxb-" + strings.Repeat("A", 61) + "T3BlbkFJ" + strings.Repeat("B", 74), "[redacted]"},
		{"gitlab-containing-openai", "glpat-" + "Ab12" + "sk-" + strings.Repeat("C", 20) + "_D34", "[redacted]"},
		{"worker-containing-openai", "uzw_" + strings.Repeat("A", 15) + "sk-" + strings.Repeat("B", 16) + "_" + strings.Repeat("C", 7) + "A", "[redacted]"},
		{"github-crossing-openai", "ghp_" + strings.Repeat("A", 16) + "sk-" + strings.Repeat("B", 20), "[redacted]"},
		{"classic-self-crossing", "sk-" + strings.Repeat("A", 46) + "sk-" + strings.Repeat("B", 48), "[redacted]"},
		{"classic-self-chain", "sk-" + strings.Repeat("A", 46) + "sk-" + strings.Repeat("B", 46) + "sk-" + strings.Repeat("C", 48), "[redacted]"},
		{"separate-credentials", "glpat-" + strings.Repeat("A", 20) + ", " + "sk-" + strings.Repeat("B", 20), "[redacted], [redacted]"},
		{"slack-positive-control", "xoxb-" + strings.Repeat("A", 20), "[redacted]"},
		{"display-prefixes", "glpat-Ab12 sk-" + strings.Repeat("C", 15) + " uzw_A1 ghp_B2", "glpat-Ab12 sk-" + strings.Repeat("C", 15) + " uzw_A1 ghp_B2"},
		{"prose", "OpenAI, GitLab and Slack credentials", "OpenAI, GitLab and Slack credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := "before (" + tc.input + "), after"
			want := "before (" + tc.want + "), after"
			if got := crossCheckHistoryText(input, 512); got != want {
				t.Fatal("history text must remove overlapping credentials and preserve surrounding text")
			}
			metadata := historyOptional(input)
			if metadata == nil || *metadata != want {
				t.Fatal("history metadata must remove overlapping credentials and preserve surrounding text")
			}
		})
	}
}

func TestCrossCheckHistoryTextOpenAINormalization(t *testing.T) {
	token := "sk-" + "proj-" + strings.Repeat("A", 74) + "T3BlbkFJ" + strings.Repeat("B", 74)
	for _, tc := range []struct {
		name  string
		input string
	}{
		// Independent cases: either control must be removed BEFORE scrubbing
		// reconnects the interrupted credential. Newlines fold into word breaks.
		{"ESC", "before " + token[:12] + "\x1b" + token[12:] + " after"},
		{"U202E", "before " + token[:12] + "\u202e" + token[12:] + " after"},
		{"newline", "before\n" + token + "\nafter"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := crossCheckHistoryText(tc.input, 512); got != "before [redacted] after" {
				t.Fatal("history text must normalize controls and redact the complete credential")
			}
		})
	}
}
