package handler

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Risk 14 (PRD #64 M5): a uzc_/uza_ CLI token this API mints must never survive into
// either credential-scrubbing surface. The CLI PRD tells users to put UZI_TOKEN in a
// GitLab CI variable, which creates the echo-into-a-trace path, so BOTH the failure-
// snapshot ingest (workersvc.ScrubKnownTokens / snapshotSecretPatterns) and the outbound Slack
// scrub (slacksvc.ScrubSecrets) must strip it. One literal, both paths — because M5
// owns the redactor precisely because M5 is what mints these tokens.
func TestCLITokenSealedThroughBothScrubPaths(t *testing.T) {
	// A fake uzc_ token with a realistic ≥16-char base64url body.
	const uzc = "uzc_A1b2C3d4E5f6G7h8i9j0k1lM"

	t.Run("failure-snapshot ingest", func(t *testing.T) {
		out := workersvc.ScrubKnownTokens("+ uzi run list\nUZI_TOKEN=" + uzc + "\nexit status 1")
		if strings.Contains(out, uzc) {
			t.Errorf("ScrubKnownTokens left the uzc_ token in the snapshot tail: %q", out)
		}
		if !strings.Contains(out, "[REDACTED]") {
			t.Errorf("ScrubKnownTokens produced no [REDACTED] placeholder: %q", out)
		}
	})

	t.Run("outbound Slack scrub", func(t *testing.T) {
		out := slacksvc.ScrubSecrets("run failed while calling uzi with " + uzc)
		if strings.Contains(out, uzc) {
			t.Errorf("slacksvc.ScrubSecrets left the uzc_ token in the Slack message: %q", out)
		}
	})
}
