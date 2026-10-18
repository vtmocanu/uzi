package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const queuedExplanation = "Queued for uzi: starts on the next sync if autopilot is on\nStarts automatically when autopilot is enabled in Settings and the forge identity matches; otherwise this only labels the issue for uzi.\n"

func TestFindingsFileSendToUziForwarding(t *testing.T) {
	for _, shape := range []struct {
		name  string
		ids   []string
		group bool
	}{
		{"single", []string{"e-1"}, false},
		{"group", []string{"e-1", "e-2"}, true},
		{"same-coordinate", []string{"e-1", "e-1-old"}, false},
	} {
		for _, flag := range []struct {
			name string
			args []string
			want bool
		}{
			{"default", nil, false},
			{"false", []string{"--send-to-uzi=false"}, false},
			{"true", []string{"--send-to-uzi"}, true},
		} {
			t.Run(shape.name+"/"+flag.name, func(t *testing.T) {
				fc := groupFake()
				fc.FileFindingResult.Issue = apitypes.IncidentalFindingFiledIssueDTO{IID: 3, Title: "single"}
				// Seed both captures oppositely so forwarding false must overwrite its own capture.
				fc.LastFileFindingSendToUzi = !flag.want
				fc.LastFileFindingGroupSendToUzi = !flag.want
				args := append([]string{"findings", "file"}, shape.ids...)
				args = append(args, flag.args...)
				out, errb, code := runCLI(t, fakeEnv(fc), args...)
				if code != uzicli.ExitOK {
					t.Fatalf("exit=%d stderr=%s", code, errb)
				}
				if shape.group {
					if fc.LastFileFindingGroupSendToUzi != flag.want || fc.LastFileFindingSendToUzi != !flag.want || fc.LastFileFindingID != "" {
						t.Fatalf("wrong group forwarding: %+v", fc)
					}
				} else if fc.LastFileFindingSendToUzi != flag.want || fc.LastFileFindingGroupSendToUzi != !flag.want || fc.LastFileFindingID != "e-1" || fc.LastFileFindingGroupIDs != nil {
					t.Fatalf("wrong single forwarding")
				}
				if strings.Contains(out, queuedExplanation) != flag.want {
					t.Errorf("queue explanation: %q", out)
				}
			})
		}
	}
}

func TestFindingsFileSendToUziOutput(t *testing.T) {
	for _, tc := range []struct {
		name                                                            string
		group, quiet, json, uncertain, accepted, noIssue, warning, fail bool
	}{
		{name: "single-warning", warning: true},
		{name: "single-quiet", quiet: true, warning: true},
		{name: "single-json", json: true, warning: true},
		{name: "single-json-quiet", json: true, quiet: true},
		{name: "single-error", fail: true},
		{name: "group-warning", group: true, warning: true},
		{name: "group-quiet-warning", group: true, quiet: true, warning: true},
		{name: "group-json", group: true, json: true, warning: true},
		{name: "group-json-quiet", group: true, json: true, quiet: true},
		{name: "group-unlinked", group: true, warning: true, uncertain: true},
		{name: "group-unlinked-quiet", group: true, quiet: true, warning: true, uncertain: true},
		{name: "group-uncertain", group: true, uncertain: true, accepted: true, warning: true},
		{name: "group-uncertain-quiet", group: true, quiet: true, uncertain: true, accepted: true, warning: true},
		{name: "group-uncertain-json", group: true, json: true, uncertain: true, accepted: true},
		{name: "group-no-issue-201", group: true, uncertain: true, noIssue: true},
		{name: "group-error", group: true, fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fc := groupFake()
			fc.FileFindingResult.Issue = apitypes.IncidentalFindingFiledIssueDTO{IID: 3, Title: "single"}
			if tc.warning {
				fc.FileFindingResult.Warning = "settle warning"
				fc.FileFindingGroupResult.Warning = "settle warning"
			}
			if tc.uncertain {
				fc.FileFindingGroupResult.Phase = "returned_uncertain"
			}
			if tc.accepted || tc.noIssue {
				fc.FileFindingGroupResult.Issue = nil
			}
			fc.FileFindingGroupAccepted = tc.accepted
			if tc.fail {
				fc.FileFindingErr = uzicli.Exitf(uzicli.ExitNotFound, "missing")
				fc.FileFindingGroupErr = uzicli.Exitf(uzicli.ExitNotFound, "missing")
			}
			args := []string{"findings", "file", "e-1", "--send-to-uzi"}
			if tc.group {
				args = append(args, "e-2")
			}
			if tc.quiet {
				args = append(args, "--quiet")
			}
			if tc.json {
				args = append(args, "--json")
			}
			out, errb, code := runCLI(t, fakeEnv(fc), args...)
			wantCode := uzicli.ExitOK
			if tc.accepted || tc.noIssue {
				wantCode = uzicli.ExitConflict
			}
			if tc.fail {
				wantCode = uzicli.ExitNotFound
			}
			if code != wantCode {
				t.Fatalf("exit=%d want=%d stderr=%s", code, wantCode, errb)
			}
			wantQueued := !tc.quiet && !tc.json && !tc.accepted && !tc.noIssue && !tc.fail
			if strings.Contains(out, queuedExplanation) != wantQueued || (!wantQueued && strings.Contains(out, "Queued for uzi")) {
				t.Errorf("queue output: %q", out)
			}
			if tc.json {
				var want []byte
				if tc.group {
					want, _ = json.MarshalIndent(fc.FileFindingGroupResult, "", "  ")
				} else {
					want, _ = json.MarshalIndent(fc.FileFindingResult, "", "  ")
				}
				if out != string(want)+"\n" {
					t.Errorf("JSON changed: %q want %s", out, want)
				}
			} else {
				if tc.quiet && !tc.uncertain && out != "" {
					t.Errorf("quiet: %q", out)
				}
				if tc.warning && (!tc.quiet || tc.uncertain) && !strings.Contains(out, "warning: settle warning") {
					t.Errorf("warning lost: %q", out)
				}
				if tc.uncertain && !strings.Contains(out, "operation op-1 (returned_uncertain)") {
					t.Errorf("operation lost: %q", out)
				}
				if (tc.accepted || tc.noIssue) && !strings.Contains(out, "uzi findings release op-1 --confirm-no-issue") {
					t.Errorf("guidance lost: %q", out)
				}
			}
		})
	}
}

func TestFindingsFileSendToUziFlagScope(t *testing.T) {
	for _, args := range [][]string{
		{"review", "file", "f-1", "--send-to-uzi"},
		{"findings", "draft", "e-1", "--send-to-uzi"},
	} {
		_, errb, code := runCLI(t, fakeEnv(groupFake()), args...)
		if code != uzicli.ExitUsage || !strings.Contains(errb, "unknown flag: --send-to-uzi") {
			t.Errorf("%v: exit=%d stderr=%s", args, code, errb)
		}
	}
}
