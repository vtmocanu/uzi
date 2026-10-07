package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// renderUsageToString runs renderAdminUsage into a non-TTY, non-JSON, no-colour
// printer and returns the captured text, matching the render_test.go pattern.
func renderUsageToString(t *testing.T, u apitypes.AdminUsageDTO) string {
	t.Helper()
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false) // non-tty, non-json, no colour
	if err := renderAdminUsage(p, u); err != nil {
		t.Fatalf("renderAdminUsage: %v", err)
	}
	return buf.String()
}

// TestRenderAdminUsageOutcomes covers the PRD #1293 M3 CLI additions: the factory
// line's finished/failed/fail_rate fields and the reordered per-user table columns
// (EMAIL RUNS FAILED FAIL% SINCE INPUT OUTPUT COST), mirroring the web column order (D8).
func TestRenderAdminUsageOutcomes(t *testing.T) {
	u := apitypes.AdminUsageDTO{
		Factory: apitypes.SelfUsageDTO{
			Lifetime: apitypes.UsageDTO{
				InputTokens:         1000,
				CacheReadTokens:     200,
				CacheCreationTokens: 50,
				OutputTokens:        300,
				CostUSD:             12.34,
			},
			RunCount: 38,
			Outcomes: apitypes.RunOutcomeWindowsDTO{
				Lifetime: apitypes.RunOutcomesDTO{
					Finished:     1024,
					Completed:    861,
					Cancelled:    45,
					PlanRejected: 12,
					Failed:       106,
				},
			},
		},
		Users: []apitypes.AdminUserUsageDTO{
			{
				Email:    "alice@example.com",
				Usage:    apitypes.UsageDTO{InputTokens: 500, OutputTokens: 150, CostUSD: 6.00},
				RunCount: 20,
				Outcomes: apitypes.RunOutcomesDTO{Finished: 200, Failed: 40},
			},
		},
	}

	out := renderUsageToString(t, u)

	// Factory line carries the new outcome fields with correct values.
	// 106/1024*100 = 10.3515... -> "10.4%".
	for _, want := range []string{"finished=1024", "failed=106", "fail_rate=10.4%"} {
		if !strings.Contains(out, want) {
			t.Errorf("factory line missing %q\noutput:\n%s", want, out)
		}
	}

	// Table header is exactly EMAIL RUNS FAILED FAIL% SINCE INPUT OUTPUT COST in order.
	header := firstHeaderLine(t, out)
	wantOrder := []string{"EMAIL", "RUNS", "FAILED", "FAIL%", "SINCE", "INPUT", "OUTPUT", "COST"}
	if got := strings.Fields(header); !equalStringSlices(got, wantOrder) {
		t.Errorf("table header = %v, want %v\nheader line: %q", got, wantOrder, header)
	}

	// The per-user row renders RUNS in second position, FAILED, then the rate.
	// 40/200*100 = 20.0 -> "20.0%".
	if !strings.Contains(out, "20.0%") {
		t.Errorf("per-user fail rate cell missing 20.0%%\noutput:\n%s", out)
	}
}

// TestRenderAdminUsageZeroFinished checks that a scope with no finished runs renders
// the fail rate as "-" (a hyphen), never a fabricated "0.0%" (PRD #1293 D6).
func TestRenderAdminUsageZeroFinished(t *testing.T) {
	u := apitypes.AdminUsageDTO{
		Factory: apitypes.SelfUsageDTO{
			Lifetime: apitypes.UsageDTO{InputTokens: 0, OutputTokens: 0, CostUSD: 0},
			RunCount: 0,
			Outcomes: apitypes.RunOutcomeWindowsDTO{
				Lifetime: apitypes.RunOutcomesDTO{Finished: 0, Failed: 0},
			},
		},
		Users: []apitypes.AdminUserUsageDTO{
			{
				Email:    "bob@example.com",
				Usage:    apitypes.UsageDTO{},
				RunCount: 0,
				Outcomes: apitypes.RunOutcomesDTO{Finished: 0, Failed: 0},
			},
		},
	}

	out := renderUsageToString(t, u)

	if !strings.Contains(out, "fail_rate=-") {
		t.Errorf("factory fail_rate should be \"-\" at zero finished, got:\n%s", out)
	}
	if strings.Contains(out, "fail_rate=0.0%") {
		t.Errorf("factory fail_rate must not be a fabricated 0.0%% at zero finished:\n%s", out)
	}
	if strings.Contains(out, "0.0%") {
		t.Errorf("no fail-rate cell should render 0.0%% at zero finished:\n%s", out)
	}
}

// TestRenderAdminUsageRecoverable covers issue #1418: the recoverable subset is
// descriptive, shown beside both factory and per-user failures only when positive.
func TestRenderAdminUsageRecoverable(t *testing.T) {
	u := apitypes.AdminUsageDTO{
		Factory: apitypes.SelfUsageDTO{
			Lifetime: apitypes.UsageDTO{InputTokens: 1000, OutputTokens: 300, CostUSD: 12.34},
			RunCount: 38,
			Outcomes: apitypes.RunOutcomeWindowsDTO{
				Lifetime: apitypes.RunOutcomesDTO{Finished: 1024, Failed: 106, NeedsLanding: 7},
			},
		},
		Users: []apitypes.AdminUserUsageDTO{
			{
				Email:    "alice@example.com",
				Usage:    apitypes.UsageDTO{InputTokens: 500, OutputTokens: 150, CostUSD: 6.00},
				RunCount: 20,
				Outcomes: apitypes.RunOutcomesDTO{Finished: 200, Failed: 40, NeedsLanding: 3},
			},
			{
				// A user with failures but none human-landable: the FAILED cell is a bare count.
				Email:    "bob@example.com",
				Usage:    apitypes.UsageDTO{InputTokens: 10, OutputTokens: 5, CostUSD: 1.00},
				RunCount: 5,
				Outcomes: apitypes.RunOutcomesDTO{Finished: 5, Failed: 2, NeedsLanding: 0},
			},
		},
	}

	out := renderUsageToString(t, u)

	// Factory line: the failed figure carries the inline sub-cut.
	if !strings.Contains(out, "failed=106 (7 recoverable)") {
		t.Errorf("factory line missing inline recoverable subset \"failed=106 (7 recoverable)\":\n%s", out)
	}
	// The recoverable subset rides the FAILED cell; recency has its own SINCE column.
	header := firstHeaderLine(t, out)
	wantOrder := []string{"EMAIL", "RUNS", "FAILED", "FAIL%", "SINCE", "INPUT", "OUTPUT", "COST"}
	if got := strings.Fields(header); !equalStringSlices(got, wantOrder) {
		t.Errorf("table header = %v, want %v\nheader line: %q", got, wantOrder, header)
	}
	// alice's FAILED cell carries the sub-cut; bob's (zero recoverable) stays a bare count.
	if alice := lineWith(t, out, "alice@example.com"); !strings.Contains(alice, "40 (3 recoverable)") {
		t.Errorf("alice FAILED cell missing \"40 (3 recoverable)\": %q", alice)
	}
	bob := lineWith(t, out, "bob@example.com")
	if f := strings.Fields(bob); len(f) <= 2 || f[2] != "2" {
		t.Errorf("bob FAILED field = %v, want 2", f)
	}
	if strings.Contains(bob, "recoverable") {
		t.Errorf("bob has zero recoverable work and must show a bare FAILED count, got: %q", bob)
	}
}

// firstHeaderLine returns the table header line (the first line beginning with EMAIL).
func firstHeaderLine(t *testing.T, out string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "EMAIL") {
			return line
		}
	}
	t.Fatalf("no table header line found in output:\n%s", out)
	return ""
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRenderAdminUsageFailureRecency(t *testing.T) {
	at := time.Now().Add(-6 * time.Hour)
	o := apitypes.RunOutcomesDTO{Finished: 20, Failed: 1, LastFailedAt: &at}
	u := apitypes.AdminUsageDTO{Factory: apitypes.SelfUsageDTO{Outcomes: apitypes.RunOutcomeWindowsDTO{Lifetime: o}}, Users: []apitypes.AdminUserUsageDTO{{Email: "failed@example.com", Outcomes: o}, {Email: "clean@example.com", Outcomes: apitypes.RunOutcomesDTO{Finished: 4}}, {Email: "empty@example.com"}}}
	out := renderUsageToString(t, u)
	for _, want := range []string{"since_last_failure=6h", "SINCE"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q: %s", want, out)
		}
	}
	for _, tc := range []struct{ email, want string }{{"failed@example.com", "6h"}, {"clean@example.com", "no failures"}, {"empty@example.com", "-"}} {
		if line := lineWith(t, out, tc.email); !strings.Contains(line, tc.want) {
			t.Errorf("row missing %q: %s", tc.want, line)
		}
	}
}

func TestSinceLastFailureBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		age  time.Duration
		want string
	}{{-time.Minute, "<1m"}, {0, "<1m"}, {59 * time.Second, "<1m"}, {time.Minute, "1m"}, {38 * time.Minute, "38m"}, {time.Hour, "1h"}, {6 * time.Hour, "6h"}, {24 * time.Hour, "1d"}, {3 * 24 * time.Hour, "3d"}} {
		at := now.Add(-tc.age)
		if got := sinceLastFailure(apitypes.RunOutcomesDTO{LastFailedAt: &at}, now); got != tc.want {
			t.Errorf("age %s: %s, want %s", tc.age, got, tc.want)
		}
	}
}
