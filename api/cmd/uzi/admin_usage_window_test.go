package main

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

type usageWindowClient struct {
	uzicli.FakeClient
	requests int
}

func (c *usageWindowClient) AdminUsage(context.Context) (apitypes.AdminUsageDTO, error) {
	c.requests++
	return c.AdminUsageV, nil
}

func usageWindowFixture() apitypes.AdminUsageDTO {
	at := time.Now().Add(-6 * time.Hour)
	return apitypes.AdminUsageDTO{
		Factory: apitypes.SelfUsageDTO{
			Lifetime:  apitypes.UsageDTO{InputTokens: 999, CacheReadTokens: 888, CacheCreationTokens: 777, OutputTokens: 666, CostUSD: 196},
			Last7Days: apitypes.UsageDTO{InputTokens: 33, CacheReadTokens: 36, CacheCreationTokens: 39, OutputTokens: 50, CostUSD: 6},
			RunCount:  99,
			Outcomes: apitypes.RunOutcomeWindowsDTO{
				Lifetime:  apitypes.RunOutcomesDTO{Finished: 100, Failed: 10, NeedsLanding: 5, LastFailedAt: &at},
				Last7Days: apitypes.RunOutcomesDTO{Finished: 12, Failed: 3, NeedsLanding: 1},
			},
			LifetimeSubscriptionRunCount: 4, LifetimeUnreportedRunCount: 5,
			Last7SubscriptionRunCount: 2, Last7UnreportedRunCount: 3,
		},
		Users: []apitypes.AdminUserUsageDTO{
			{UserID: "b", Email: "b@example.com", Usage: apitypes.UsageDTO{CostUSD: 90}, RunCount: 40,
				Outcomes:      apitypes.RunOutcomesDTO{Finished: 50, Failed: 10, LastFailedAt: &at},
				Last7Days:     &apitypes.UsageDTO{InputTokens: 11, CacheReadTokens: 12, CacheCreationTokens: 13, OutputTokens: 20, CostUSD: 2},
				Last7RunCount: 3, Last7Outcomes: apitypes.RunOutcomesDTO{Finished: 4, Failed: 1, NeedsLanding: 1},
				SubscriptionRunCount: 7, UnreportedRunCount: 8, Last7SubscriptionRunCount: 2, Last7UnreportedRunCount: 3},
			{UserID: "c", Email: "c@example.com", Usage: apitypes.UsageDTO{CostUSD: 5},
				Last7Days:     &apitypes.UsageDTO{InputTokens: 11, CacheReadTokens: 12, CacheCreationTokens: 13, OutputTokens: 10, CostUSD: 2},
				Last7RunCount: 1, Last7Outcomes: apitypes.RunOutcomesDTO{Finished: 2}},
			{UserID: "a", Email: "a@example.com", Usage: apitypes.UsageDTO{CostUSD: 1},
				Last7Days:     &apitypes.UsageDTO{InputTokens: 11, CacheReadTokens: 12, CacheCreationTokens: 13, OutputTokens: 20, CostUSD: 2},
				Last7RunCount: 2, Last7Outcomes: apitypes.RunOutcomesDTO{Finished: 4, Failed: 1}},
			{UserID: "o", Email: "outcomes@example.com", Last7Days: &apitypes.UsageDTO{},
				Last7Outcomes: apitypes.RunOutcomesDTO{Finished: 2, Failed: 1}},
			{UserID: "l", Email: "lifetime@example.com", Usage: apitypes.UsageDTO{CostUSD: 100},
				RunCount: 20, Outcomes: apitypes.RunOutcomesDTO{Finished: 20}, Last7Days: &apitypes.UsageDTO{}},
		},
	}
}

func TestAdminUsageWindowCommands(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"default", nil, "factory (lifetime): input=999 cache_read=888 cache_creation=777 output=666 cost=$196.00 (API-equivalent estimate incomplete; legacy runs have tokens without an estimate; unreported partial costs included: subscription_runs=4 unreported_runs=5) (runs=99) finished=100 failed=10 (5 recoverable) fail_rate=10.0% since_last_failure=6h"},
		{"explicit lifetime", []string{"--window", "lifetime"}, "factory (lifetime): input=999"},
		{"seven days", []string{"--window", "last_7_days"}, "factory (last_7_days): input=33 cache_read=36 cache_creation=39 output=50 cost=$6.00 (API-equivalent estimate incomplete; legacy runs have tokens without an estimate; unreported partial costs included: subscription_runs=2 unreported_runs=3) (runs=6) finished=12 failed=3 (1 recoverable) fail_rate=25.0% since_last_failure=6h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &usageWindowClient{FakeClient: uzicli.FakeClient{AdminUsageV: usageWindowFixture()}}
			out, stderr, rc := runCLI(t, fakeEnv(c), append([]string{"admin", "usage"}, tc.flags...)...)
			if rc != 0 || stderr != "" || c.requests != 1 || !strings.Contains(out, tc.want) {
				t.Fatalf("rc=%d requests=%d stderr=%q output=%s", rc, c.requests, stderr, out)
			}
		})
	}
}

func TestAdminUsageInvalidWindowBeforeClient(t *testing.T) {
	for _, window := range []string{"", "LAST_7_DAYS", "7d"} {
		for _, jsonFlag := range []bool{false, true} {
			env := fakeEnv(&uzicli.FakeClient{})
			env.NewClient = func(uzicli.Settings) uzicli.Client {
				t.Fatal("invalid window created a client")
				return nil
			}
			args := []string{"admin", "usage", "--window", window}
			if jsonFlag {
				args = append(args, "--json")
			}
			out, stderr, rc := runCLI(t, env, args...)
			if rc != uzicli.ExitUsage || out != "" || !strings.Contains(stderr, "invalid --window") {
				t.Fatalf("window=%q json=%v rc=%d out=%q stderr=%q", window, jsonFlag, rc, out, stderr)
			}
		}
	}
}

func TestAdminUsageWindowJSONComplete(t *testing.T) {
	for _, old := range []bool{false, true} {
		u := usageWindowFixture()
		if old {
			u.Users[1].Last7Days = nil
		}
		for _, window := range []string{"lifetime", "last_7_days"} {
			c := &usageWindowClient{FakeClient: uzicli.FakeClient{AdminUsageV: u}}
			out, stderr, rc := runCLI(t, fakeEnv(c), "admin", "usage", "--json", "--window", window)
			var got apitypes.AdminUsageDTO
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("JSON: %v; rc=%d stderr=%q", err, rc, stderr)
			}
			wantJSON, err := json.Marshal(u)
			if err != nil {
				t.Fatal(err)
			}
			var want apitypes.AdminUsageDTO
			if err := json.Unmarshal(wantJSON, &want); err != nil {
				t.Fatal(err)
			}
			if rc != 0 || stderr != "" || c.requests != 1 || !reflect.DeepEqual(got, want) {
				t.Fatalf("window=%s old=%v rc=%d stderr=%q DTO=%+v", window, old, rc, stderr, got)
			}
		}
	}
}

func TestAdminUsageWindowRowsAndCopy(t *testing.T) {
	u := usageWindowFixture()
	before, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	if err := renderAdminUsage(p, u, "last_7_days"); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		"b@example.com 3 1 (1 recoverable) 25.0% 6h 11 12 13 20 $2.00 (API-equivalent estimate incomplete; legacy runs have tokens without an estimate; unreported partial costs included: subscription_runs=2 unreported_runs=3)",
		"lifetime@example.com 0 0 - no failures 0 0 0 0 $0.00 (API-equivalent)",
		"outcomes@example.com 0 1 50.0% - 0 0 0 0 $0.00 (API-equivalent)",
	} {
		email := strings.Fields(want)[0]
		got := strings.Join(strings.Fields(lineWith(t, out, email)), " ")
		if got != want {
			t.Errorf("row=%q want=%q", got, want)
		}
	}
	assertUsageWindowOrder(t, out, []string{"a@example.com", "b@example.com", "c@example.com", "lifetime@example.com", "outcomes@example.com"})
	after, err := json.Marshal(u)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("renderer mutated the source DTO")
	}
	lifetime := renderUsageToString(t, u)
	assertUsageWindowOrder(t, lifetime, []string{"lifetime@example.com", "b@example.com", "c@example.com", "a@example.com", "outcomes@example.com"})
	if got := strings.Join(strings.Fields(lineWith(t, lifetime, "b@example.com")), " "); got != "b@example.com 40 10 20.0% 6h 0 0 0 0 $90.00 (API-equivalent estimate incomplete; legacy runs have tokens without an estimate; unreported partial costs included: subscription_runs=7 unreported_runs=8)" {
		t.Errorf("lifetime row=%q", got)
	}
}

func assertUsageWindowOrder(t *testing.T, out string, emails []string) {
	t.Helper()
	last := -1
	for _, email := range emails {
		pos := strings.Index(out, email)
		if pos <= last {
			t.Fatalf("wrong order for %v: %s", emails, out)
		}
		last = pos
	}
}

func TestAdminUsageWindowOlderAPIAtomic(t *testing.T) {
	u := usageWindowFixture()
	u.Users[len(u.Users)-1].Last7Days = nil
	var buf bytes.Buffer
	p := uzicli.NewPrinter(&buf, false, false, true, false)
	err := renderAdminUsage(p, u, "last_7_days")
	if err == nil || !strings.Contains(err.Error(), "API upgrade") || buf.Len() != 0 {
		t.Fatalf("err=%v output=%q", err, buf.String())
	}
	c := &usageWindowClient{FakeClient: uzicli.FakeClient{AdminUsageV: u}}
	out, stderr, rc := runCLI(t, fakeEnv(c), "admin", "usage", "--window", "last_7_days")
	if rc == 0 || out != "" || !strings.Contains(stderr, "API upgrade") || c.requests != 1 {
		t.Fatalf("rc=%d out=%q stderr=%q requests=%d", rc, out, stderr, c.requests)
	}
	if out := renderUsageToString(t, u); !strings.Contains(out, "factory (lifetime)") {
		t.Fatal("older response lost lifetime support")
	}
}

func TestAdminUsageWindowZero(t *testing.T) {
	for _, users := range [][]apitypes.AdminUserUsageDTO{nil, {{UserID: "zero", Email: "zero@example.com", Last7Days: &apitypes.UsageDTO{}}}} {
		var buf bytes.Buffer
		p := uzicli.NewPrinter(&buf, false, false, true, false)
		if err := renderAdminUsage(p, apitypes.AdminUsageDTO{Users: users}, "last_7_days"); err != nil {
			t.Fatal(err)
		}
		out := buf.String()
		want := "factory (last_7_days): input=0 cache_read=0 cache_creation=0 output=0 cost=$0.00 (API-equivalent) (runs=0) finished=0 failed=0 fail_rate=- since_last_failure=-"
		if strings.Split(out, "\n")[0] != want || strings.Contains(out, "excluded") {
			t.Fatalf("zero output=%s", out)
		}
		if len(users) > 0 {
			if got := strings.Join(strings.Fields(lineWith(t, out, "zero@example.com")), " "); got != "zero@example.com 0 0 - - 0 0 0 0 $0.00 (API-equivalent)" {
				t.Errorf("zero row=%q", got)
			}
		}
	}
}
