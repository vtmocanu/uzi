package main

import (
	"bytes"
	"strings"
	"testing"

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
// (EMAIL RUNS FAILED FAIL% INPUT OUTPUT COST), mirroring the web column order (D8).
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

	// Table header is exactly EMAIL RUNS FAILED FAIL% INPUT OUTPUT COST in order.
	header := firstHeaderLine(t, out)
	wantOrder := []string{"EMAIL", "RUNS", "FAILED", "FAIL%", "INPUT", "OUTPUT", "COST"}
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
