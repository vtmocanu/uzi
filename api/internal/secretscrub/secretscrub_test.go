// Package secretscrub_test binds the minted-credential prefixes uzi issues to BOTH
// scrub paths — secretscrub.Scrub (outbound Slack / persisted text) and
// workersvc.ScrubKnownTokens (CI failure snapshots) — so a new minted prefix or a
// changed prefix VALUE that lacks a matching scrub pattern reddens the gate
// (PRD #954 M1, S2). It is an EXTERNAL test package on purpose (D4): it imports
// clitoken, jointoken and workersvc, none of which secretscrub may import back —
// secretscrub is deliberately import-light because low-level paths depend on it.
package secretscrub_test

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/jointoken"
	"github.com/vtmocanu/uzi/api/internal/secretscrub"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// body is a realistic ≥16-char token body of [A-Za-z0-9] — long enough to satisfy
// every family's {16,} anchor and free of the '-' that the anchored GitHub families
// exclude, so one body works for all prefixes.
const body = "A1b2C3d4E5f6G7h8i9j0k1l2"

// assertScrubbedBothPaths is the "one literal, both paths" idiom
// (handler/cli_redactor_test.go:18-37): a credential-shaped token must be absent
// from, and replaced by the placeholder of, BOTH scrub paths.
func assertScrubbedBothPaths(t *testing.T, token string) {
	t.Helper()
	in := "leak " + token + " end"

	scrub := secretscrub.Scrub(in)
	if strings.Contains(scrub, token) {
		t.Errorf("secretscrub.Scrub left %q in %q", token, scrub)
	}
	if !strings.Contains(scrub, "[redacted]") {
		t.Errorf("secretscrub.Scrub produced no [redacted] placeholder for %q: %q", token, scrub)
	}

	snap := workersvc.ScrubKnownTokens(in)
	if strings.Contains(snap, token) {
		t.Errorf("workersvc.ScrubKnownTokens left %q in %q", token, snap)
	}
	if !strings.Contains(snap, "[REDACTED]") {
		t.Errorf("workersvc.ScrubKnownTokens produced no [REDACTED] placeholder for %q: %q", token, snap)
	}
}

// TestMintedPrefixesScrubbedOnBothPaths ranges over the EXPORTED minted-prefix
// constants (not string copies) so a fourth prefix added to clitoken.Prefixes —
// the same line a new prefix is registered — automatically extends this binding.
func TestMintedPrefixesScrubbedOnBothPaths(t *testing.T) {
	// clitoken.Prefixes = {uzc_, uza_}; jointoken.Prefix = uzw_ (const, no slice needed).
	prefixes := append(append([]string{}, clitoken.Prefixes...), jointoken.Prefix)
	for _, p := range prefixes {
		t.Run(p, func(t *testing.T) {
			assertScrubbedBothPaths(t, p+body)
		})
	}
}

// TestForgePATFamiliesScrubbedOnBothPaths pins every forge PAT family on both paths:
// GitLab, the GitHub classic + fine-grained families (the live gap S2 closes), and
// Anthropic keys.
func TestForgePATFamiliesScrubbedOnBothPaths(t *testing.T) {
	tokens := []string{
		"glpat-" + body,      // GitLab (both lists carry the 9-family form after change A)
		"ghp_" + body,        // GitHub classic PAT
		"gho_" + body,        // GitHub OAuth
		"ghu_" + body,        // GitHub user-to-server
		"ghs_" + body,        // GitHub server-to-server
		"ghr_" + body,        // GitHub refresh
		"github_pat_" + body, // GitHub fine-grained
		"sk-ant-" + body,     // Anthropic
	}
	for _, tok := range tokens {
		t.Run(tok[:strings.IndexByte(tok, body[0])], func(t *testing.T) {
			assertScrubbedBothPaths(t, tok)
		})
	}
}

// TestDeliberatePerListDivergences records the extras that REMAIN per list after
// change A (PRD #954 D2 — do NOT converge the lists). These are asserted intentional
// divergences, not drift: the snapshot list owns the CI-trace header/Bearer shapes,
// and secretscrub owns the Slack families.
func TestDeliberatePerListDivergences(t *testing.T) {
	// Header-line + bare-Bearer live ONLY in the snapshot list (a `curl -v`/`set -x`
	// echo is a CI-trace shape). secretscrub does not carry them by design.
	t.Run("header and bearer: snapshot only", func(t *testing.T) {
		header := "Authorization: Bearer " + body + "extra"
		if out := workersvc.ScrubKnownTokens(header); strings.Contains(out, body) {
			t.Errorf("ScrubKnownTokens did not redact the Authorization header line: %q", out)
		}
		bare := "using Bearer " + body + "more now"
		if out := workersvc.ScrubKnownTokens(bare); strings.Contains(out, body) {
			t.Errorf("ScrubKnownTokens did not redact the bare Bearer token: %q", out)
		}
		// secretscrub deliberately leaves a bare "Bearer <token>" alone (it is not a
		// minted credential SHAPE, and this text surface differs). Recorded divergence.
		if out := secretscrub.Scrub("using Bearer " + body + "more now"); !strings.Contains(out, body) {
			t.Errorf("secretscrub.Scrub unexpectedly redacted a bare Bearer token (list drift?): %q", out)
		}
	})

	// Slack tokens live ONLY in secretscrub — its text surface is outbound Slack /
	// persisted answers. The snapshot list carries NO Slack pattern at all: neither
	// the xoxb-/xapp- families nor the xoxe- refresh family change A added to
	// secretscrub. Both are asserted here so "Slack tokens only in secretscrub"
	// (D2) is an explicit recorded divergence, not drift.
	t.Run("slack xoxb: secretscrub only", func(t *testing.T) {
		xoxb := "xoxb-123-abc-" + body
		if out := secretscrub.Scrub("token " + xoxb + " here"); strings.Contains(out, xoxb) {
			t.Errorf("secretscrub.Scrub did not redact the Slack bot token: %q", out)
		}
		// Recorded divergence: the snapshot list does not carry xoxb-/xapp-.
		if out := workersvc.ScrubKnownTokens("token " + xoxb + " here"); !strings.Contains(out, xoxb) {
			t.Errorf("ScrubKnownTokens unexpectedly redacted a Slack bot token (list drift?): %q", out)
		}
	})
	t.Run("slack xoxe refresh: secretscrub only", func(t *testing.T) {
		xoxe := "xoxe-1-abc-" + body
		if out := secretscrub.Scrub("refresh " + xoxe + " here"); strings.Contains(out, xoxe) {
			t.Errorf("secretscrub.Scrub did not redact the Slack refresh token: %q", out)
		}
		// The Slack-only path (slacksvc.ScrubTokens -> ScrubSlackTokens) must carry the
		// refresh family too; it did not on the first cut of change A (PR #968 review).
		if out := secretscrub.ScrubSlackTokens("refresh " + xoxe + " here"); out != "refresh [redacted] here" {
			t.Errorf("secretscrub.ScrubSlackTokens should yield the exact redacted text, got %q", out)
		}
		// Recorded divergence: xoxe- is a Slack family, so it lives only in
		// secretscrub — the snapshot list must NOT redact it.
		if out := workersvc.ScrubKnownTokens("refresh " + xoxe + " here"); !strings.Contains(out, xoxe) {
			t.Errorf("ScrubKnownTokens unexpectedly redacted a Slack refresh token (list drift?): %q", out)
		}
	})
}

// TestGitHubRegexesDoNotOverMatch mirrors slacksvc/redact_test.go:57-68's under-match
// negative: the anchored GitHub families must NOT redact prose or a short display
// stub. "ghost_" is not a token (position 3 is 's', not '_'), and an 8-char stub has
// only 4 body chars (< the {16,} minimum).
func TestGitHubRegexesDoNotOverMatch(t *testing.T) {
	for _, in := range []string{
		"the ghost_ wrote a message to disk",
		"listed as ghp_a1b2 in the token display",
	} {
		if out := secretscrub.Scrub(in); out != in {
			t.Errorf("secretscrub.Scrub over-matched %q -> %q", in, out)
		}
		if out := workersvc.ScrubKnownTokens(in); out != in {
			t.Errorf("workersvc.ScrubKnownTokens over-matched %q -> %q", in, out)
		}
	}
}
