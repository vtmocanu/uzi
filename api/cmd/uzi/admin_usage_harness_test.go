package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func harnessUsageFixture() apitypes.AdminUsageDTO {
	return apitypes.AdminUsageDTO{
		Users: []apitypes.AdminUserUsageDTO{
			{UserID: "a", Email: "a@example.com", Usage: apitypes.UsageDTO{CostUSD: 2}, RunCount: 10,
				HarnessRuns:   &apitypes.HarnessRunsDTO{Claude: 7, Codex: 3},
				Last7Days:     &apitypes.UsageDTO{CostUSD: 1},
				Last7RunCount: 4, Last7HarnessRuns: &apitypes.HarnessRunsDTO{Claude: 1, Codex: 3}},
			{UserID: "b", Email: "b@example.com", Usage: apitypes.UsageDTO{CostUSD: 1}, RunCount: 0,
				HarnessRuns: &apitypes.HarnessRunsDTO{}, Last7Days: &apitypes.UsageDTO{},
				Last7HarnessRuns: &apitypes.HarnessRunsDTO{}},
		},
	}
}

func renderUsageWindow(t *testing.T, u apitypes.AdminUsageDTO, window string) string {
	t.Helper()
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	if err := renderAdminUsage(p, u, window); err != nil {
		t.Fatalf("renderAdminUsage(%s): %v", window, err)
	}
	return buf.String()
}

func TestRenderAdminUsageHarnessColumn(t *testing.T) {
	wantHeader := []string{"EMAIL", "RUNS", "CLAUDE/CODEX", "FAILED", "FAIL%", "SINCE", "INPUT", "CACHE_READ", "CACHE_CREATION", "OUTPUT", "COST"}
	for _, tc := range []struct {
		window, a, b string
	}{
		{"lifetime", "a@example.com 10 7/3 ", "b@example.com 0 0/0 "},
		{"last_7_days", "a@example.com 4 1/3 ", "b@example.com 0 0/0 "},
	} {
		u := harnessUsageFixture()
		out := renderUsageWindow(t, u, tc.window)
		// The seven-day rewrite works on a copy: the caller's lifetime split is untouched.
		if h := u.Users[0].HarnessRuns; h == nil || h.Claude != 7 || h.Codex != 3 {
			t.Errorf("%s render mutated the caller's harness_runs: %+v", tc.window, h)
		}
		if got := strings.Fields(firstHeaderLine(t, out)); !equalStringSlices(got, wantHeader) {
			t.Errorf("%s header=%v want %v", tc.window, got, wantHeader)
		}
		for _, want := range []string{tc.a, tc.b} {
			email := strings.Fields(want)[0]
			got := strings.Join(strings.Fields(lineWith(t, out, email)), " ") + " "
			if !strings.HasPrefix(got, want) {
				t.Errorf("%s row=%q want prefix %q", tc.window, got, want)
			}
		}
	}
}

func TestRenderAdminUsageHarnessColumnOmittedWhenAnyRowLacksIt(t *testing.T) {
	for _, window := range []string{"lifetime", "last_7_days"} {
		for _, mutate := range []struct {
			name string
			fn   func(*apitypes.AdminUserUsageDTO)
		}{
			{"lifetime field", func(r *apitypes.AdminUserUsageDTO) { r.HarnessRuns = nil }},
			{"last7 field", func(r *apitypes.AdminUserUsageDTO) { r.Last7HarnessRuns = nil }},
		} {
			u := harnessUsageFixture()
			mutate.fn(&u.Users[1])
			out := renderUsageWindow(t, u, window)
			// The column is gated on the selected window's field only.
			wantPresent := (window == "lifetime") == (mutate.name == "last7 field")
			if got := strings.Contains(out, "CLAUDE/CODEX"); got != wantPresent {
				t.Errorf("%s/%s: header present=%v want %v\n%s", window, mutate.name, got, wantPresent, out)
			}
			if !wantPresent && strings.Contains(out, "7/3") {
				t.Errorf("%s/%s: cells leaked into output\n%s", window, mutate.name, out)
			}
		}
	}
	// Neither field anywhere: exact legacy header.
	u := harnessUsageFixture()
	for i := range u.Users {
		u.Users[i].HarnessRuns, u.Users[i].Last7HarnessRuns = nil, nil
	}
	if strings.Contains(renderUsageWindow(t, u, "lifetime"), "CLAUDE/CODEX") {
		t.Error("column rendered with no harness data")
	}
}

func TestAdminUsageJSONCarriesHarnessRuns(t *testing.T) {
	c := &usageWindowClient{FakeClient: uzicli.FakeClient{AdminUsageV: harnessUsageFixture()}}
	out, stderr, rc := runCLI(t, fakeEnv(c), "admin", "usage", "--json")
	if rc != 0 || stderr != "" {
		t.Fatalf("rc=%d stderr=%q", rc, stderr)
	}
	var raw struct {
		Users []map[string]json.RawMessage `json:"users"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out)
	}
	if len(raw.Users) == 0 {
		t.Fatalf("no users in JSON: %s", out)
	}
	for _, u := range raw.Users {
		if _, ok := u["harness_runs"]; !ok {
			t.Errorf("user missing harness_runs: %v", u)
		}
		if _, ok := u["last7_harness_runs"]; !ok {
			t.Errorf("user missing last7_harness_runs: %v", u)
		}
	}
	for _, want := range []string{`"harness_runs":{"claude":7,"codex":3}`, `"last7_harness_runs":{"claude":1,"codex":3}`} {
		if !strings.Contains(strings.ReplaceAll(strings.ReplaceAll(out, " ", ""), "\n", ""), want) {
			t.Errorf("JSON missing %s", want)
		}
	}
}
