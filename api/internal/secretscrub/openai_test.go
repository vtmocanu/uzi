package secretscrub_test

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/secretscrub"
)

func TestScrubOpenAIKeys(t *testing.T) {
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
			if got := secretscrub.Scrub(input); got != "before ([redacted]), after" {
				t.Fatal("Scrub must replace the entire credential and preserve surrounding text")
			}
			if got := secretscrub.ScrubSlackTokens(input); got != input {
				t.Fatal("Slack-only scrub must leave non-Slack credentials unchanged")
			}
		})
	}
}

func TestScrubOpenAIPreservesProseAndDisplayPrefixes(t *testing.T) {
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
		if got := secretscrub.Scrub(input); got != input {
			t.Fatal("Scrub changed harmless prose or a short display prefix")
		}
	}
}
