package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func groupFake() *uzicli.FakeClient {
	fc := findingsFake()
	fc.FindingDrafts = map[string]apitypes.IncidentalFindingIssueDraftDTO{
		"e-1":     {DispositionID: "d-1"},
		"e-1-old": {DispositionID: "d-1"},
		"e-2":     {DispositionID: "d-2"},
	}
	fc.FileFindingGroupResult = apitypes.FindingGroupFileResultDTO{
		OperationID:    "op-1",
		DispositionIDs: []string{"d-1", "d-2"},
		Phase:          "settled",
		Issue:          &apitypes.IncidentalFindingFiledIssueDTO{IID: 9, WebURL: "https://forge/x/9", Title: "grouped"},
	}
	return fc
}

func TestFindingsFileSingleIDSkipsDraftLookup(t *testing.T) {
	fc := groupFake()
	fc.FileFindingResult = apitypes.IncidentalFindingFileResultDTO{Issue: apitypes.IncidentalFindingFiledIssueDTO{IID: 1, Title: "t"}}
	if _, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1"); code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if len(fc.LastFindingDraftIDs) != 0 || fc.LastFileFindingGroupIDs != nil || fc.LastFileFindingID != "e-1" {
		t.Errorf("single id must use the plain file path: drafts=%v group=%v single=%q", fc.LastFindingDraftIDs, fc.LastFileFindingGroupIDs, fc.LastFileFindingID)
	}
}

func TestFindingsFileGroupDedupesResolvedDispositions(t *testing.T) {
	fc := groupFake()
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2", "e-1-old")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if want := []string{"d-1", "d-2"}; !reflect.DeepEqual(fc.LastFileFindingGroupIDs, want) {
		t.Errorf("group ids = %v, want %v", fc.LastFileFindingGroupIDs, want)
	}
	for _, want := range []string{"filed issue #9: grouped", "https://forge/x/9", "linked findings: d-1, d-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestFindingsFileGroupAllSameDispositionFallsBackToSingle(t *testing.T) {
	fc := groupFake()
	fc.FileFindingResult = apitypes.IncidentalFindingFileResultDTO{Issue: apitypes.IncidentalFindingFiledIssueDTO{IID: 3, Title: "single"}}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-1-old")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if fc.LastFileFindingID != "e-1" || fc.LastFileFindingGroupIDs != nil {
		t.Errorf("fallback wrong: single=%q group=%v", fc.LastFileFindingID, fc.LastFileFindingGroupIDs)
	}
	if !strings.Contains(out, "filed issue #3") {
		t.Errorf("output:\n%s", out)
	}
}

func TestFindingsFileGroupJSON(t *testing.T) {
	fc := groupFake()
	fc.FileFindingGroupResult.Warning = "settle failed"
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got apitypes.FindingGroupFileResultDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if got.Issue == nil || got.Issue.IID != 9 || !reflect.DeepEqual(got.DispositionIDs, []string{"d-1", "d-2"}) || got.Warning != "settle failed" {
		t.Errorf("json lost data: %+v", got)
	}
}

func TestFindingsFileGroupWarningQuiet(t *testing.T) {
	fc := groupFake()
	fc.FileFindingGroupResult.Warning = "settle failed"
	out, _, _ := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2")
	if !strings.Contains(out, "warning: settle failed") {
		t.Errorf("warning missing:\n%s", out)
	}
	out, _, code := runCLI(t, fakeEnv(groupWithWarning()), "--quiet", "findings", "file", "e-1", "e-2")
	if code != uzicli.ExitOK || strings.Contains(out, "warning") {
		t.Errorf("--quiet should hide the warning (exit %d):\n%s", code, out)
	}
}

func groupWithWarning() *uzicli.FakeClient {
	fc := groupFake()
	fc.FileFindingGroupResult.Warning = "settle failed"
	return fc
}

func TestFindingsFileGroupAcceptedExit5(t *testing.T) {
	fc := groupFake()
	fc.FileFindingGroupAccepted = true
	fc.FileFindingGroupResult = apitypes.FindingGroupFileResultDTO{
		OperationID: "op-7", DispositionIDs: []string{"d-1", "d-2"}, Phase: "returned_uncertain", Warning: "unsure",
	}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want 5", code)
	}
	for _, want := range []string{"op-7", "returned_uncertain", "unsure", "uzi findings release op-7 --confirm-no-issue"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	out, _, code = runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2", "--json")
	var got apitypes.FindingGroupFileResultDTO
	if code != uzicli.ExitConflict || json.Unmarshal([]byte(out), &got) != nil || got.OperationID != "op-7" {
		t.Errorf("json 202: exit=%d out=%s", code, out)
	}
}

func TestFindingsFileGroup409ReportsPendingOperation(t *testing.T) {
	fc := groupFake()
	fc.FileFindingGroupErr = uzicli.Exitf(uzicli.ExitConflict, "finding not fileable")
	op := "op-99"
	fc.FindingsResult.Findings = []apitypes.IncidentalFindingDTO{
		{DispositionID: "d-1"},
		{DispositionID: "d-2", GroupOperationID: &op},
		{DispositionID: "d-other", GroupOperationID: ptr("op-zzz")},
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want 5", code)
	}
	if !strings.Contains(errb, "finding not fileable") || !strings.Contains(errb, "d-2: pending operation op-99") || strings.Contains(errb, "op-zzz") {
		t.Errorf("stderr:\n%s", errb)
	}
}

func TestFindingsFileGroupUnknownEvidence404(t *testing.T) {
	fc := groupFake()
	_, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "nope")
	if code != uzicli.ExitNotFound || fc.LastFileFindingGroupIDs != nil {
		t.Errorf("exit = %d group=%v", code, fc.LastFileFindingGroupIDs)
	}
}

func TestFindingsRelease(t *testing.T) {
	fc := groupFake()
	fc.ReleaseFindingGroupResult = apitypes.FindingGroupReleaseResultDTO{OperationID: "op-1", Phase: "released"}
	_, _, code := runCLI(t, fakeEnv(fc), "findings", "release", "op-1")
	if code != uzicli.ExitUsage || fc.LastReleaseFindingGroupOp != "" {
		t.Fatalf("without flag: exit=%d op=%q", code, fc.LastReleaseFindingGroupOp)
	}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "release", "op-1", "--confirm-no-issue")
	if code != uzicli.ExitOK || fc.LastReleaseFindingGroupOp != "op-1" || !strings.Contains(out, "released operation op-1") {
		t.Errorf("exit=%d op=%q out=%s", code, fc.LastReleaseFindingGroupOp, out)
	}
	out, _, code = runCLI(t, fakeEnv(fc), "findings", "release", "op-1", "--confirm-no-issue", "--json")
	var got apitypes.FindingGroupReleaseResultDTO
	if code != uzicli.ExitOK || json.Unmarshal([]byte(out), &got) != nil || got.Phase != "released" {
		t.Errorf("json: exit=%d out=%s", code, out)
	}
}

func TestFindingsBacklogShowsPendingGroup(t *testing.T) {
	fc := findingsFake()
	fc.FindingsResult.Findings[0].GroupOperationID = ptr("op-5")
	out, _, _ := runCLI(t, fakeEnv(fc), "findings", "list")
	if !strings.Contains(out, "pending group op-5") {
		t.Errorf("state label missing:\n%s", out)
	}
}

// The release hint printed on a 202 is a runnable command: lift it from the real output, run it
// through the real parse and assert it reaches the release write with the printed operation id.
func TestFindingsGroupReleaseHintExecutes(t *testing.T) {
	fc := groupFake()
	fc.FileFindingGroupAccepted = true
	fc.FileFindingGroupResult = apitypes.FindingGroupFileResultDTO{OperationID: "op-8", Phase: "in_flight"}
	fc.ReleaseFindingGroupResult = apitypes.FindingGroupReleaseResultDTO{OperationID: "op-8", Phase: "released"}
	out, _, code := runCLI(t, fakeEnv(fc), "findings", "file", "e-1", "e-2")
	if code != uzicli.ExitConflict {
		t.Fatalf("exit = %d, want 5", code)
	}
	var hint string
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, "`uzi findings release "); i >= 0 {
			rest := line[i+1:]
			hint = rest[:strings.Index(rest, "`")]
		}
	}
	fields := strings.Fields(hint)
	if len(fields) < 4 || fields[0] != "uzi" {
		t.Fatalf("no runnable release hint in output:\n%s", out)
	}
	out2, _, code := runCLI(t, fakeEnv(fc), fields[1:]...)
	if code != uzicli.ExitOK || fc.LastReleaseFindingGroupOp != "op-8" || !strings.Contains(out2, "released operation op-8") {
		t.Errorf("hint %q: exit=%d op=%q out=%s", hint, code, fc.LastReleaseFindingGroupOp, out2)
	}
}
