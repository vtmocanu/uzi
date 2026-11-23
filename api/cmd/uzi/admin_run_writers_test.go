package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const writersRepoID = "0b8b6a8a-0000-4000-8000-000000000009"

func writersFake(complete bool, ws ...apitypes.RunWriterDTO) *uzicli.FakeClient {
	if ws == nil {
		ws = []apitypes.RunWriterDTO{}
	}
	return &uzicli.FakeClient{AdminRunWritersOut: apitypes.RunWritersDTO{Writers: ws, Complete: complete}}
}

func oneWriter(ref string, status string, mr *int64) apitypes.RunWriterDTO {
	return apitypes.RunWriterDTO{
		RunID: "run-1", Kind: "ci_fix", Status: status, OwnerID: "owner-1", RepoID: writersRepoID,
		PipelineRef: &ref, MRIID: mr, CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestAdminRunWritersHumanOutput(t *testing.T) {
	fc := writersFake(true)
	out, errb, code := runCLI(t, fakeEnv(fc), "admin", "run-writers", "--repo", writersRepoID, "--branch", "agent/x")
	if code != uzicli.ExitOK || errb != "" || !strings.Contains(out, "no active ci_fix/mr_rework writers (complete, all owners, at this database snapshot)") {
		t.Fatalf("empty: exit=%d out=%q err=%q", code, out, errb)
	}
	if !reflect.DeepEqual(fc.AdminRunWritersCalls, []uzicli.AdminRunWritersCall{{RepoID: writersRepoID, Branch: "agent/x"}}) {
		t.Errorf("calls = %+v", fc.AdminRunWritersCalls)
	}

	mr := int64(7)
	fc = writersFake(true, oneWriter("agent/x", "running", &mr))
	out, _, code = runCLI(t, fakeEnv(fc), "admin", "run-writers", "--repo", writersRepoID, "--branch", "agent/x", "--mr", "7")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d", code)
	}
	for _, want := range []string{"RUN", "KIND", "STATUS", "OWNER", "REPO", "BRANCH", "MR", "run-1", "ci_fix", "running", "owner-1", "agent/x"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "run-1") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 7 || f[4] != writersRepoID || f[len(f)-1] != "7" {
			t.Errorf("row %q: want repo id %q in the REPO column and MR 7 last", line, writersRepoID)
		}
	}
	if fc.AdminRunWritersCalls[0].MRIID != 7 {
		t.Errorf("calls = %+v", fc.AdminRunWritersCalls)
	}
}

func TestAdminRunWritersJSONOutput(t *testing.T) {
	fc := writersFake(true, oneWriter("agent/x", "running", nil))
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "run-writers", "--repo", writersRepoID, "--branch", "agent/x", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d", code)
	}
	var got apitypes.RunWritersDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if !got.Complete || len(got.Writers) != 1 || got.Writers[0].RunID != "run-1" {
		t.Errorf("got = %+v", got)
	}
}

func TestAdminRunWritersIncompleteIsNonZero(t *testing.T) {
	for _, args := range [][]string{nil, {"--json"}} {
		fc := writersFake(false, oneWriter("agent/x", "running", nil))
		argv := append([]string{"admin", "run-writers", "--repo", writersRepoID, "--branch", "agent/x"}, args...)
		out, errb, code := runCLI(t, fakeEnv(fc), argv...)
		if code != uzicli.ExitGeneric {
			t.Errorf("%v: exit=%d, want %d", args, code, uzicli.ExitGeneric)
		}
		if !strings.Contains(errb, "incomplete: more than 500 matching writers; absence is unknown") {
			t.Errorf("%v: stderr=%q lacks the incomplete note", args, errb)
		}
		if !strings.Contains(out, "run-1") {
			t.Errorf("%v: stdout=%q lost the writers", args, out)
		}
	}
	// An empty but incomplete result is not "no writers".
	out, _, code := runCLI(t, fakeEnv(writersFake(false)), "admin", "run-writers", "--repo", writersRepoID, "--branch", "b")
	if code != uzicli.ExitGeneric || strings.Contains(out, "no active ci_fix") {
		t.Errorf("empty incomplete: exit=%d out=%q", code, out)
	}
}

func TestAdminRunWritersUsageRefusals(t *testing.T) {
	base := []string{"admin", "run-writers", "--repo", writersRepoID, "--branch", "b"}
	for name, args := range map[string][]string{
		"mr zero":     append(append([]string{}, base...), "--mr", "0"),
		"mr negative": append(append([]string{}, base...), "--mr", "-1"),
		"bad repo":    {"admin", "run-writers", "--repo", "nope", "--branch", "b"},
		"no repo":     {"admin", "run-writers", "--branch", "b"},
		"no branch":   {"admin", "run-writers", "--repo", writersRepoID},
	} {
		t.Run(name, func(t *testing.T) {
			fc := writersFake(true)
			_, _, code := runCLI(t, fakeEnv(fc), args...)
			if code != uzicli.ExitUsage || len(fc.AdminRunWritersCalls) != 0 {
				t.Errorf("exit=%d calls=%d, want usage and no request", code, len(fc.AdminRunWritersCalls))
			}
		})
	}
}

func TestAdminRunWritersHostileCellsAreSanitized(t *testing.T) {
	w := oneWriter(hostileANSI+"\u202e", "run"+hostileANSI, nil)
	out, _, code := runCLI(t, fakeEnv(writersFake(true, w)), "admin", "run-writers", "--repo", writersRepoID, "--branch", "b")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d", code)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x202e) {
		t.Errorf("raw control or bidi characters reached stdout: %q", out)
	}
}

func TestAdminRunWritersHintOnRefusal(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code int
		hint bool
	}{
		"403":         {uzicli.Exitf(uzicli.ExitAuth, "forbidden"), uzicli.ExitAuth, true},
		"404":         {uzicli.Exitf(uzicli.ExitNotFound, "not found"), uzicli.ExitNotFound, true},
		"unreachable": {uzicli.Exitf(uzicli.ExitUnreachable, "down"), uzicli.ExitUnreachable, false},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{AdminRunWritersErr: tc.err}
			_, errb, code := runCLI(t, fakeEnv(fc), "admin", "run-writers", "--repo", writersRepoID, "--branch", "b")
			if code != tc.code || strings.Contains(errb, adminRunDebugHint) != tc.hint {
				t.Errorf("exit=%d stderr=%q", code, errb)
			}
		})
	}
}
