package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestScheduleRemoveLabelRequests(t *testing.T) {
	for _, mode := range []string{"create", "user", "default"} {
		for _, stored := range []bool{false, true} {
			for _, tc := range []struct {
				name  string
				flags []string
				wire  string
			}{
				{"omitted", nil, ""},
				{"true", []string{"--remove-label-on-dispatch"}, "true"},
				{"explicit true", []string{"--remove-label-on-dispatch=true"}, "true"},
				{"false", []string{"--remove-label-on-dispatch=false"}, "false"},
			} {
				t.Run(mode+"/"+boolStr(stored)+"/"+tc.name, func(t *testing.T) {
					fc := &uzicli.FakeClient{ScheduleByID: map[string]apitypes.ScheduleDTO{
						"s": {ID: "s", Origin: mode, Target: "sweep", Labels: []string{"on-deck"}, Timing: "recurring", CronExpr: "0 * * * *", RemoveLabelOnDispatch: stored},
					}}
					args := []string{"schedule", "edit", "s"}
					if mode == "create" {
						args = []string{"schedule", "create", "--repo", "r", "--sweep", "--label", "on-deck", "--cron", "0 * * * *"}
					} else if tc.wire == "" {
						args = append(args, "--cron", "0 2 * * *")
					}
					args = append(args, tc.flags...)
					_, errOut, code := runCLI(t, fakeEnv(fc), args...)
					if code != uzicli.ExitOK {
						t.Fatalf("exit=%d stderr=%s", code, errOut)
					}
					req := fc.LastPatchSchedReq
					if mode == "create" {
						req = fc.LastCreateSchedReq
					}
					if tc.wire == "" {
						if req.RemoveLabelOnDispatch != nil {
							t.Fatalf("omitted flag restated stored value: %v", *req.RemoveLabelOnDispatch)
						}
					} else if req.RemoveLabelOnDispatch == nil || *req.RemoveLabelOnDispatch != (tc.wire == "true") {
						t.Fatalf("explicit value lost: %+v", req.RemoveLabelOnDispatch)
					}
					raw, err := json.Marshal(req)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(raw, &fields); err != nil {
						t.Fatal(err)
					}
					if got := string(fields["remove_label_on_dispatch"]); got != tc.wire {
						t.Fatalf("wire value=%q want=%q body=%s", got, tc.wire, raw)
					}
				})
			}
		}
	}
}

func TestScheduleRemoveLabelOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		started apitypes.LastFireStarted
		want    string
	}{
		{"success snapshot", apitypes.LastFireStarted{LabelRemoved: true, SelectorLabel: "on-deck"}, "on-deck removed"},
		{"failure snapshot", apitypes.LastFireStarted{LabelRemoveFailed: true, SelectorLabel: "on-deck"}, "on-deck could not be removed (the run started; remove it by hand)"},
		{"legacy success", apitypes.LastFireStarted{LabelRemoved: true}, "label removed"},
		{"legacy failure", apitypes.LastFireStarted{LabelRemoveFailed: true}, "label could not be removed (the run started; remove it by hand)"},
		{"no outcome", apitypes.LastFireStarted{SelectorLabel: "on-deck"}, ""},
		{"legacy no outcome", apitypes.LastFireStarted{}, ""},
		{"hostile snapshot", apitypes.LastFireStarted{LabelRemoveFailed: true, SelectorLabel: "on-\x1b[31mdeck\nforged\u202e"}, "on-[31mdeck forged could not be removed (the run started; remove it by hand)"},
	} {
		for _, verb := range []string{"get", "run-now"} {
			for _, enabled := range []bool{false, true} {
				t.Run(tc.name+"/"+verb+"/"+boolStr(enabled), func(t *testing.T) {
					st := tc.started
					st.IssueIID = ptrInt64(42)
					st.RunID = "run-42"
					st.Title = "Fix it"
					fc := &uzicli.FakeClient{
						ScheduleByID: map[string]apitypes.ScheduleDTO{"s": {
							ID: "s", Target: "sweep", Labels: []string{"current-selector"}, RemoveLabelOnDispatch: enabled,
							LastFire: &apitypes.LastFire{Started: []apitypes.LastFireStarted{st}},
						}},
						RunNowResult: apitypes.RunNowResponse{Created: 1, RunIDs: []string{"run-42"}, Started: []apitypes.LastFireStarted{st}},
					}
					out, errOut, code := runCLI(t, fakeEnv(fc), "schedule", verb, "s")
					if code != uzicli.ExitOK {
						t.Fatalf("exit=%d stderr=%s", code, errOut)
					}
					wantLine := "#42 → run run-42  Fix it"
					if tc.want != "" {
						wantLine += " · " + tc.want
					}
					found := false
					for _, line := range strings.Split(out, "\n") {
						if strings.Contains(line, "→ run") {
							found = true
							if strings.TrimSpace(line) != wantLine {
								t.Errorf("started line=%q want=%q", line, wantLine)
							}
						}
						if verb == "get" && strings.HasPrefix(line, "REMOVE_LABEL_ON_DISPATCH") {
							if got := strings.Fields(line); len(got) != 2 || got[1] != boolStr(enabled) {
								t.Errorf("config row=%q", line)
							}
						}
					}
					if !found {
						t.Fatalf("missing started line: %s", out)
					}
					if verb == "get" && !strings.Contains(out, "REMOVE_LABEL_ON_DISPATCH") {
						t.Errorf("missing config row: %s", out)
					}
					for _, bad := range []string{"\x1b", "\u202e", "\nforged"} {
						if strings.Contains(out, bad) {
							t.Errorf("unsafe terminal output %q", out)
						}
					}
				})
			}
		}
	}
}
