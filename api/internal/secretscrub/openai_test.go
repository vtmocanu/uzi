package secretscrub_test

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
)

func TestScrubHistoryOpenAIKeys(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
	}{
		{"classic", "sk-" + strings.Repeat("aB3d", 5) + "T3BlbkFJ" + strings.Repeat("eF7h", 5)},
		// Exact auditor reproduction: 164 bytes, assembled only at runtime.
		{"project-auditor", "sk-" + "proj-" + strings.Repeat("A", 74) + "T3BlbkFJ" + strings.Repeat("B", 74)},
		{"service-account", "sk-" + "svcacct-" + strings.Repeat("aB_9-", 14) + "T3BlbkFJ" + strings.Repeat("cD_8-", 14)},
		{"project-url-safe", "sk-" + "proj-" + strings.Repeat("aB_9-", 14) + "T3BlbkFJ" + strings.Repeat("cD_8-", 14)},
		{"classic-minimum", "sk-" + strings.Repeat("A", 16)},
		{"project-minimum", "sk-" + "proj-" + strings.Repeat("A", 16)},
		{"service-account-minimum", "sk-" + "svcacct-" + strings.Repeat("A", 16)},
		{"gitlab-positive-control", "glpat-" + strings.Repeat("aB3d", 5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "project-auditor" && len(tc.token) != 164 {
				t.Fatal("auditor fixture must be 164 bytes")
			}
			input := "before (" + tc.token + "), after"
			if got := secretscrub.ScrubHistory(input); got != "before ([redacted]), after" {
				t.Fatal("Scrub must replace the entire credential and preserve surrounding text")
			}
			if got := secretscrub.ScrubSlackTokens(input); got != input {
				t.Fatal("Slack-only scrub must leave non-Slack credentials unchanged")
			}
		})
	}
}

func TestScrubHistoryOverlappingCredentials(t *testing.T) {
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
			if tc.name == "project-containing-slack" && len(tc.input) != 164 {
				t.Fatal("overlapping project fixture must be 164 bytes")
			}
			if got := secretscrub.ScrubHistory("before (" + tc.input + "), after"); got != "before ("+tc.want+"), after" {
				t.Fatal("Scrub must remove the union of overlapping credentials and preserve surrounding text")
			}
		})
	}
}

func TestScrubHistoryOpenAIPreservesProseAndDisplayPrefixes(t *testing.T) {
	for _, input := range []string{
		"OpenAI project and service-account keys are configured separately.",
		"Use sk- for a classic key, sk-proj- for a project, sk-svcacct- for a service account.",
		"task-projection and desk-service are harmless prose",
		"sk-" + "a1b2",
		"sk-" + "proj-" + "a1b2",
		"sk-" + "svcacct-" + "a1b2",
		"sk-" + strings.Repeat("A", 15),
		"sk-" + "proj-" + strings.Repeat("A", 15),
		"sk-" + "svcacct-" + strings.Repeat("A", 15),
	} {
		if got := secretscrub.ScrubHistory(input); got != input {
			t.Fatal("Scrub changed harmless prose or a short display prefix")
		}
	}
}

func TestScrubLegacyPreservesWritePathProse(t *testing.T) {
	for _, input := range []string{
		"task-" + "0123456789abcdef",
		"risk-" + "assessmentupdates",
		"desk-" + strings.Repeat("documentation", 8),
		"sk-" + strings.Repeat("A", 48),
		"sk-" + "proj-" + strings.Repeat("B", 160),
		"sk-" + "svcacct-" + strings.Repeat("C", 160),
	} {
		if got := secretscrub.Scrub(input); got != input {
			t.Fatal("legacy Scrub changed write-path prose or OpenAI behavior")
		}
	}
	// Legacy replacement order intentionally differs from history's span union.
	input := "sk-ant-" + "AAAA" + "xoxb-" + strings.Repeat("B", 20)
	if got := secretscrub.Scrub(input); got != "[redacted][redacted]" {
		t.Fatal("legacy Scrub replacement order changed")
	}
	if got := secretscrub.ScrubHistory(input); got != "[redacted]" {
		t.Fatal("history must retain non-OpenAI overlap protection")
	}
}
