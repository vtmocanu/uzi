package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestBaseDivergedCommands(t *testing.T) {
	for _, tc := range []struct{ name, status, origin, stop, landing, want string }{
		{"rejection", "failed", "base_diverged", "", "", "rejected"},
		{"landing", "failed", "base_diverged", "", "needs_landing", "rejected"},
		{"agent", "failed", "agent_failure", "", "", "failed"},
		{"cancel", "cancelled", "base_diverged", "", "needs_landing", "stopped (cancelled)"},
		{"human", "failed", "base_diverged", "cancelled", "needs_landing", "stopped (cancelled)"},
		{"plan", "failed", "base_diverged", "plan_rejected", "needs_landing", "stopped (plan rejected)"},
		{"running", "running", "base_diverged", "", "needs_landing", "running"},
		{"completed", "completed", "base_diverged", "", "needs_landing", "completed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := apitypes.RunDTO{ID: "control", Kind: "issue", Status: tc.status, FailOrigin: sp(tc.origin), StopKind: sp(tc.stop), LandingState: tc.landing, IssueTitle: "control title"}
			rows := []apitypes.RunListItemDTO{{RunDTO: r}}
			fc := &uzicli.FakeClient{Runs: rows, AdminRuns: rows, RunByID: map[string]apitypes.RunDTO{r.ID: r}}
			for _, args := range [][]string{{"run", "get", r.ID}, {"run", "list"}, {"admin", "runs"}} {
				out, stderr, rc := runCLI(t, fakeEnv(fc), args...)
				if rc != 0 {
					t.Fatalf("%v: rc=%d stderr=%s", args, rc, stderr)
				}
				key := r.ID
				if args[1] == "get" {
					key = "STATUS"
				}
				row := strings.Join(strings.Fields(lineWith(t, out, key)), " ")
				if args[1] == "get" {
					if row != "STATUS "+tc.want {
						t.Errorf("%v: %q want STATUS %s", args, row, tc.want)
					}
					if origin := strings.Join(strings.Fields(lineWith(t, out, "FAIL_ORIGIN")), " "); origin != "FAIL_ORIGIN "+tc.origin {
						t.Errorf("raw origin: %q", origin)
					}
				} else if !strings.Contains(row, "issue "+tc.want+" ") {
					t.Errorf("%v: %q want %q", args, row, tc.want)
				}
			}
			out, stderr, rc := runCLI(t, fakeEnv(fc), "run", "get", r.ID, "--field", "status", "--field", "fail_origin")
			if rc != 0 || out != tc.status+"\n"+tc.origin+"\n" {
				t.Fatalf("scalar: rc=%d out=%q stderr=%s", rc, out, stderr)
			}
			for _, args := range [][]string{{"run", "get", r.ID, "--json"}, {"run", "list", "--json"}, {"admin", "runs", "--json"}} {
				out, stderr, rc := runCLI(t, fakeEnv(fc), args...)
				if rc != 0 {
					t.Fatalf("JSON rc=%d stderr=%s", rc, stderr)
				}
				var got apitypes.RunDTO
				if args[1] == "get" {
					if err := json.Unmarshal([]byte(out), &got); err != nil {
						t.Fatal(err)
					}
				} else {
					var list []apitypes.RunListItemDTO
					if err := json.Unmarshal([]byte(out), &list); err != nil {
						t.Fatal(err)
					}
					if len(list) != 1 {
						t.Fatalf("JSON rows=%d", len(list))
					}
					got = list[0].RunDTO
				}
				if got.Status != tc.status || got.FailOrigin == nil || *got.FailOrigin != tc.origin {
					t.Fatalf("raw JSON status/origin=%s/%v", got.Status, got.FailOrigin)
				}
			}
		})
	}
}

func TestBaseDivergedStatusGuard(t *testing.T) {
	for _, tc := range []struct {
		status             string
		planning, revising bool
		want               string
	}{
		{"running", false, false, "running"},
		{"completed", true, true, "completed"},
		{"running", true, false, "planning"},
		{"awaiting_approval", false, true, "revising"},
	} {
		if got := displayRunStatus(tc.status, tc.planning, tc.revising, "needs_landing", "", "base_diverged"); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.status, got, tc.want)
		}
	}
}

func TestBaseDivergedUsageCommands(t *testing.T) {
	at := time.Now().Add(-6 * time.Hour)
	lifetime := apitypes.RunOutcomesDTO{Finished: 10, Completed: 5, BaseDiverged: 3, Failed: 2, LastFailedAt: &at}
	recent := apitypes.RunOutcomesDTO{Finished: 5, Completed: 2, BaseDiverged: 2, Failed: 1}
	refused := apitypes.RunOutcomesDTO{Finished: 4, BaseDiverged: 4}
	u := apitypes.AdminUsageDTO{
		Factory: apitypes.SelfUsageDTO{Outcomes: apitypes.RunOutcomeWindowsDTO{Lifetime: lifetime, Last7Days: recent}},
		Users: []apitypes.AdminUserUsageDTO{
			{UserID: "mixed", Email: "mixed@example.com", Outcomes: lifetime, Last7Outcomes: recent, Last7Days: &apitypes.UsageDTO{}},
			{UserID: "refused", Email: "refused@example.com", Outcomes: refused, Last7Outcomes: refused, Last7Days: &apitypes.UsageDTO{}},
			{UserID: "zero", Email: "zero@example.com", Last7Days: &apitypes.UsageDTO{}},
		},
	}
	for _, tc := range []struct{ window, factory, mixed string }{
		{"lifetime", "finished=10 failed=2 fail_rate=20.0% since_last_failure=6h", "mixed@example.com 0 2 20.0% 6h"},
		{"last_7_days", "finished=5 failed=1 fail_rate=20.0% since_last_failure=6h", "mixed@example.com 0 1 20.0% 6h"},
	} {
		t.Run(tc.window, func(t *testing.T) {
			fc := &uzicli.FakeClient{AdminUsageV: u}
			out, stderr, rc := runCLI(t, fakeEnv(fc), "admin", "usage", "--window", tc.window)
			if rc != 0 || !strings.Contains(strings.Split(out, "\n")[0], tc.factory) {
				t.Fatalf("rc=%d stderr=%s out=%s", rc, stderr, out)
			}
			for _, want := range []string{tc.mixed, "refused@example.com 0 0 0.0% no failures", "zero@example.com 0 0 - -"} {
				if !strings.Contains(strings.Join(strings.Fields(lineWith(t, out, strings.Fields(want)[0])), " "), want) {
					t.Errorf("missing %q: %s", want, out)
				}
			}
			out, stderr, rc = runCLI(t, fakeEnv(fc), "admin", "usage", "--window", tc.window, "--json")
			if rc != 0 {
				t.Fatalf("JSON rc=%d stderr=%s", rc, stderr)
			}
			// Decode generic JSON to prove numeric zero is present, not silently omitted.
			var got struct {
				Users []struct {
					Outcomes      map[string]json.RawMessage
					Last7Outcomes map[string]json.RawMessage `json:"last7_outcomes"`
				}
			}
			// Use explicit wire keys for the window whose Go name differs.
			var wire map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &wire); err != nil {
				t.Fatal(err)
			}
			var factory struct {
				Outcomes map[string]map[string]json.RawMessage
			}
			if err := json.Unmarshal(wire["factory"], &factory); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			check := func(m map[string]json.RawMessage, want string) {
				t.Helper()
				if string(m["base_diverged"]) != want {
					t.Errorf("base_diverged=%s want numeric %s", m["base_diverged"], want)
				}
			}
			check(factory.Outcomes["lifetime"], "3")
			check(factory.Outcomes["last_7_days"], "2")
			for i, want := range []string{"3", "4", "0"} {
				check(got.Users[i].Outcomes, want)
			}
			for i, want := range []string{"2", "4", "0"} {
				check(got.Users[i].Last7Outcomes, want)
			}
		})
	}
}
