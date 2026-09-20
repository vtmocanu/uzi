package main

import (
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// cliCwin builds a Codex window with a reading and an optional reset-after (0 = none).
func cliCwin(pct float64, resetAfter int64) *apitypes.CodexRateLimitWindowDTO {
	w := &apitypes.CodexRateLimitWindowDTO{UsedPercent: &pct}
	if resetAfter != 0 {
		w.ResetAfterSeconds = &resetAfter
	}
	return w
}

// TestRateLimitsClaudeTable — `uzi rate-limits` (default provider) renders the owner's
// Anthropic meters as TOKEN/STATUS/5H%/7D%, default-badged.
func TestRateLimitsClaudeTable(t *testing.T) {
	fc := &uzicli.FakeClient{SelfMeters: []apitypes.TokenRateLimitDTO{
		{SecretID: "sec-1", Label: "personal", IsDefault: true, Limits: apitypes.RateLimitDTO{
			Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 33}, SevenDay: &apitypes.RateLimitWindow{Pct: 61}}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "rate-limits")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"TOKEN", "STATUS", "5H%", "7D%", "personal (default)", "ok", "33", "61"} {
		if !strings.Contains(out, want) {
			t.Errorf("owner claude table missing %q:\n%s", want, out)
		}
	}
}

// TestRateLimitsClaudeJSON — `uzi rate-limits --json` passes the raw []TokenRateLimitDTO.
func TestRateLimitsClaudeJSON(t *testing.T) {
	fc := &uzicli.FakeClient{SelfMeters: []apitypes.TokenRateLimitDTO{
		{SecretID: "sec-1", Label: "personal", IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "ok"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "rate-limits", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"label": "personal"`) || !strings.Contains(out, `"status": "ok"`) {
		t.Errorf("owner claude --json missing raw meter fields:\n%s", out)
	}
}

// TestRateLimitsCodexTable — `uzi rate-limits --provider codex` renders the owner's Codex
// accounts one row per (account, bucket), aliases joined + default-badged, a nil window as
// "—" (never 0), and a reset countdown.
func TestRateLimitsCodexTable(t *testing.T) {
	fc := &uzicli.FakeClient{SelfCodexMeters: []apitypes.CodexAccountRateLimitDTO{
		{
			AccountID: "cx-1", Aliases: []string{"work"}, IsDefault: true, Status: "fresh",
			Buckets: []apitypes.CodexRateLimitBucketDTO{
				{ID: "5h", DisplayName: "5h", Primary: cliCwin(42.4, 7200), Secondary: nil},
			},
		},
		{
			AccountID: "cx-2", Aliases: []string{"team", "team-alt"}, Status: "stale",
			Buckets: []apitypes.CodexRateLimitBucketDTO{
				{ID: "wk", DisplayName: "weekly", Primary: cliCwin(90, 0), Secondary: cliCwin(12, 0)},
			},
		},
		// A bucket-less account still gets one row so its status shows.
		{AccountID: "cx-3", Aliases: []string{"pending"}, Status: "pending"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "rate-limits", "--provider", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"ACCOUNT", "STATUS", "BUCKET", "PRIMARY", "SECONDARY", "RESET",
		"work (default)", "fresh", "42%", // primary rounded
		"team, team-alt", "stale", "weekly", "90%", "12%",
		"2h",      // reset countdown from 7200s
		"pending", // bucket-less account status
	} {
		if !strings.Contains(out, want) {
			t.Errorf("owner codex table missing %q:\n%s", want, out)
		}
	}
	// The nil secondary of cx-1 renders "—", never 0%.
	if !strings.Contains(out, "—") {
		t.Errorf("owner codex table must render a nil window as em dash:\n%s", out)
	}
}

// TestRateLimitsCodexJSON — `uzi rate-limits --provider codex --json` passes the raw
// []CodexAccountRateLimitDTO.
func TestRateLimitsCodexJSON(t *testing.T) {
	fc := &uzicli.FakeClient{SelfCodexMeters: []apitypes.CodexAccountRateLimitDTO{
		{AccountID: "cx-1", Aliases: []string{"work"}, IsDefault: true, Status: "fresh"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "rate-limits", "--provider", "codex", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{`"account_id": "cx-1"`, `"aliases"`, `"status": "fresh"`} {
		if !strings.Contains(out, want) {
			t.Errorf("owner codex --json missing %q:\n%s", want, out)
		}
	}
}

// TestRateLimitsBadProvider — an unknown --provider is a usage error (exit 2), no request made.
func TestRateLimitsBadProvider(t *testing.T) {
	_, errOut, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "rate-limits", "--provider", "gemini")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage(%d)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "unknown --provider") {
		t.Errorf("want an unknown-provider usage message, got:\n%s", errOut)
	}
}

// TestAdminRateLimitsClaudeUnchanged — the existing `admin rate-limits` Claude output is
// byte-identical whether --provider is absent or explicitly "claude", and it renders the
// Anthropic columns/values — never the Codex data even when Codex rows are also fixtured.
func TestAdminRateLimitsClaudeUnchanged(t *testing.T) {
	fc := &uzicli.FakeClient{
		RateLimits: []apitypes.AdminRateLimitRowDTO{
			{Email: "a@example.com", VaultLocked: false, Tokens: []apitypes.TokenRateLimitDTO{
				{Label: "personal", IsDefault: true, Limits: apitypes.RateLimitDTO{
					Status: "ok", FiveHour: &apitypes.RateLimitWindow{Pct: 12}, SevenDay: &apitypes.RateLimitWindow{Pct: 34}}},
			}},
			{Email: "b@example.com", VaultLocked: true}, // token-less user → one no_token row
		},
		// Codex data is present but MUST NOT surface on the claude path.
		CodexRateLimits: []apitypes.CodexAdminRateLimitRowDTO{
			{Email: "c@example.com", Accounts: []apitypes.CodexAccountRateLimitDTO{
				{AccountID: "cx-9", Aliases: []string{"codexonly"}, Status: "fresh"}}},
		},
	}
	absent, _, code := runCLI(t, fakeEnv(fc), "admin", "rate-limits")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	explicit, _, code := runCLI(t, fakeEnv(fc), "admin", "rate-limits", "--provider", "claude")
	if code != uzicli.ExitOK {
		t.Fatalf("--provider claude exit = %d, want 0", code)
	}
	if absent != explicit {
		t.Errorf("admin rate-limits differs between absent and --provider claude:\nABSENT:\n%s\nEXPLICIT:\n%s", absent, explicit)
	}
	for _, want := range []string{"EMAIL", "VAULT", "TOKEN", "STATUS", "5H%", "7D%",
		"a@example.com", "unlocked", "personal (default)", "ok", "12", "34",
		"b@example.com", "locked", "no_token"} {
		if !strings.Contains(absent, want) {
			t.Errorf("admin claude table missing %q:\n%s", want, absent)
		}
	}
	if strings.Contains(absent, "codexonly") || strings.Contains(absent, "cx-9") {
		t.Errorf("the claude admin path leaked Codex data:\n%s", absent)
	}
}

// TestAdminRateLimitsCodex — `admin rate-limits --provider codex` renders the factory-wide
// per-user Codex view, grouped by user, with the EMAIL/VAULT prefix and a nil window "—".
func TestAdminRateLimitsCodex(t *testing.T) {
	fc := &uzicli.FakeClient{CodexRateLimits: []apitypes.CodexAdminRateLimitRowDTO{
		{Email: "a@example.com", VaultLocked: true, Accounts: []apitypes.CodexAccountRateLimitDTO{
			{
				AccountID: "cx-1", Aliases: []string{"work"}, IsDefault: true, Status: "fresh",
				Buckets: []apitypes.CodexRateLimitBucketDTO{
					{ID: "5h", DisplayName: "5h", Primary: cliCwin(55, 0), Secondary: nil},
				},
			},
		}},
		// A user with no linked Codex accounts still appears once.
		{Email: "b@example.com", VaultLocked: false},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "rate-limits", "--provider", "codex")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{
		"EMAIL", "VAULT", "ACCOUNT", "STATUS", "BUCKET", "PRIMARY", "SECONDARY", "RESET",
		"a@example.com", "locked", "work (default)", "fresh", "55%",
		"b@example.com", "unlocked",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("admin codex table missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "—") {
		t.Errorf("admin codex table must render a nil window as em dash:\n%s", out)
	}
}

// TestAdminRateLimitsCodexJSON — the codex admin --json passes the raw
// []CodexAdminRateLimitRowDTO envelope element through.
func TestAdminRateLimitsCodexJSON(t *testing.T) {
	fc := &uzicli.FakeClient{CodexRateLimits: []apitypes.CodexAdminRateLimitRowDTO{
		{ID: "u1", Email: "a@example.com", Accounts: []apitypes.CodexAccountRateLimitDTO{
			{AccountID: "cx-1", Aliases: []string{"work"}, Status: "fresh"}}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "rate-limits", "--provider", "codex", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{`"email": "a@example.com"`, `"account_id": "cx-1"`, `"vault_locked"`} {
		if !strings.Contains(out, want) {
			t.Errorf("admin codex --json missing %q:\n%s", want, out)
		}
	}
}

// TestAdminRateLimitsBadProvider — an unknown --provider on the admin command is a usage
// error too.
func TestAdminRateLimitsBadProvider(t *testing.T) {
	_, errOut, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "admin", "rate-limits", "--provider", "gemini")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want ExitUsage(%d)", code, uzicli.ExitUsage)
	}
	if !strings.Contains(errOut, "unknown --provider") {
		t.Errorf("want an unknown-provider usage message, got:\n%s", errOut)
	}
}
