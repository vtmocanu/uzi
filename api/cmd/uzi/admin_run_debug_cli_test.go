package main

// admin_run_debug_cli_test.go covers the CLI side of the admin run debug exception (issue #4198):
// `run recovery <id>` for another user's run and `--user`, the unattached-capture listing,
// `admin runs --all`, the read-only export refusal, the TUI marker and the prerequisites hint.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

const (
	debugOwnerID  = "0b8b6a8a-0000-4000-8000-000000000001"
	debugOtherID  = "0b8b6a8a-0000-4000-8000-000000000002"
	debugCaptured = "2026-09-01T12:30:00Z"
)

func debugCapture(id, holdID string) apitypes.RecoveryArchiveDTO {
	return apitypes.RecoveryArchiveDTO{
		ID: id, HoldID: holdID, State: "available", SourceSha: "abcdef0123456789",
		ByteSize: i64(8), CreatedAt: time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC),
	}
}

func debugHold(id, runID string) apitypes.RecoveryCustodyHoldDTO {
	return apitypes.RecoveryCustodyHoldDTO{
		ID: id, RunID: runID, Generation: 1, State: "open", Attention: "needs_action",
		HasAvailableCapture: true, WorkerName: "alpha",
	}
}

// rawElements decodes a JSON array into its elements with insignificant whitespace removed, so
// two runs' elements compare byte for byte.
func rawElements(t *testing.T, out string) []string {
	t.Helper()
	var raws []json.RawMessage
	if err := json.Unmarshal([]byte(out), &raws); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, out)
	}
	got := make([]string, len(raws))
	for i, r := range raws {
		var buf bytes.Buffer
		if err := json.Compact(&buf, r); err != nil {
			t.Fatal(err)
		}
		got[i] = buf.String()
	}
	return got
}

func TestRunRecoveryCapturesOnlyJSON(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			recoveryHeldRunID: {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{debugCapture("cap-orphan", "released-hold")}},
		},
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID, "--json")
	if code != uzicli.ExitOK || errb != "" {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	var got []any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	want := []any{map[string]any{
		"run_id": recoveryHeldRunID, "unattached": true,
		"captures": []any{map[string]any{
			"id": "cap-orphan", "state": "available", "source_sha": "abcdef0123456789",
			"byte_size": float64(8), "created_at": debugCaptured,
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stdout = %s\nwant exactly the one unattached element %v", out, want)
	}
}

func TestRunRecoveryMixedJSONKeepsHoldElementAndAppendsUnattached(t *testing.T) {
	holds := apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-1", recoveryHeldRunID)}}
	run := func(archives ...apitypes.RecoveryArchiveDTO) []string {
		fc := &uzicli.FakeClient{
			RecoveryHoldsResult: holds,
			RunByID:             map[string]apitypes.RunDTO{recoveryHeldRunID: {ID: recoveryHeldRunID, Status: "completed"}},
			RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
				recoveryHeldRunID: {Supported: true, Archives: archives},
			},
		}
		out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID, "--json")
		if code != uzicli.ExitOK || errb != "" {
			t.Fatalf("exit=%d stderr=%q", code, errb)
		}
		return rawElements(t, out)
	}
	before := run(debugCapture("cap-1", "hold-1"))
	after := run(debugCapture("cap-1", "hold-1"), debugCapture("cap-orphan", "unknown-hold"))
	if len(before) != 1 || len(after) != 2 {
		t.Fatalf("elements before=%d after=%d; want 1 and 2", len(before), len(after))
	}
	if before[0] != after[0] {
		t.Errorf("hold element changed:\n before %s\n after  %s", before[0], after[0])
	}
	var un map[string]any
	if err := json.Unmarshal([]byte(after[1]), &un); err != nil {
		t.Fatal(err)
	}
	caps, _ := un["captures"].([]any)
	if un["unattached"] != true || len(caps) != 1 || caps[0].(map[string]any)["id"] != "cap-orphan" {
		t.Errorf("unattached element = %s, want exactly cap-orphan", after[1])
	}
	for _, k := range []string{"id", "generation", "state"} {
		if _, ok := un[k]; ok {
			t.Errorf("unattached element carries hold key %q: %s", k, after[1])
		}
	}
}

func TestRunRecoveryUnattachedHumanTable(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			recoveryHeldRunID: {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{debugCapture("cap-orphan", "released-hold")}},
		},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d", code)
	}
	for _, want := range []string{"no retained custody holds", "captures (no listed hold)", "cap-orphan", "available", "abcdef012345", "8 B"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRunRecoveryForeignRunViaScopedRead(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult:       recoveryHoldsFixtureOtherRunOnly(),
		RecoveryHoldsScopedResult: apitypes.RecoveryCustodyHoldsDTO{OwnerID: debugOwnerID, Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-foreign", recoveryHeldRunID)}},
	}
	client := &countingRecoveryClient{Client: fc}
	out, errb, code := runCLI(t, fakeEnv(client), "run", "recovery", recoveryHeldRunID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "hold-foreign") {
		t.Errorf("the foreign run's hold is not listed:\n%s", out)
	}
	if strings.Contains(out, "run discard") || strings.Contains(out, "uzi run export") {
		t.Errorf("owner-only hints shown on a read-only view:\n%s", out)
	}
	if strings.Count(errb, adminRunDebugReadOnlyLine) != 1 {
		t.Errorf("stderr = %q, want the read-only line exactly once", errb)
	}
	if !reflect.DeepEqual(fc.RecoveryHoldsScopedCalls, []uzicli.RecoveryHoldsScopedCall{{RunID: recoveryHeldRunID}}) {
		t.Errorf("scoped calls = %+v, want one ?run= read", fc.RecoveryHoldsScopedCalls)
	}
}

func TestRunRecoveryArchivesMarkerAlsoMakesTheViewReadOnly(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-1", recoveryHeldRunID)}},
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			recoveryHeldRunID: {Supported: true, AdminDebugRead: true, Archives: []apitypes.RecoveryArchiveDTO{debugCapture("cap-1", "hold-1")}},
		},
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.Contains(out, "run discard") || strings.Contains(out, "uzi run export") || strings.Count(errb, adminRunDebugReadOnlyLine) != 1 {
		t.Errorf("marker did not make the view read-only:\nstdout=%s\nstderr=%q", out, errb)
	}
}

func TestRunRecoveryOwnRunKeepsHintsAndSkipsScopedRead(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-1", recoveryHeldRunID)}}}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID)
	if code != uzicli.ExitOK || strings.Contains(errb, adminRunDebugReadOnlyLine) {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "run discard") || !strings.Contains(out, "uzi run export") || len(fc.RecoveryHoldsScopedCalls) != 0 {
		t.Errorf("own-run output changed or the scoped read ran:\n%s", out)
	}
}

func TestRunRecoveryStaleServerScopedHoldsAreFilteredOut(t *testing.T) {
	// A server that ignores ?run= answers owner-wide: holds of other runs must not be shown.
	fc := &uzicli.FakeClient{
		RecoveryHoldsScopedResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-unrelated", recoveryOtherRunID)}},
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.Contains(out, "hold-unrelated") || !strings.Contains(out, "no retained custody holds") || strings.Contains(errb, adminRunDebugReadOnlyLine) {
		t.Errorf("stale-server holds leaked or the view was marked read-only:\nstdout=%s\nstderr=%q", out, errb)
	}
	jsonOut, _, _ := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID, "--json")
	if strings.TrimSpace(jsonOut) != "[]" {
		t.Errorf("json = %q, want []", jsonOut)
	}
}

func TestRunRecoveryScopedReadOtherErrorWarnsAndContinues(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsScopedErr: uzicli.Exitf(uzicli.ExitUnreachable, "boom")}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID, "--json")
	if code != uzicli.ExitOK || strings.TrimSpace(out) != "[]" || !strings.Contains(errb, "warning: could not check admin run debug holds") {
		t.Errorf("exit=%d stdout=%q stderr=%q; want a warning and an empty result", code, out, errb)
	}
}

func recoveryHoldsFixtureOtherRunOnly() apitypes.RecoveryCustodyHoldsDTO {
	return apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-mine", recoveryOtherRunID)}}
}

func TestRunRecoveryUserHappyPath(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsScopedResult: apitypes.RecoveryCustodyHoldsDTO{
			OwnerID: strings.ToUpper(debugOwnerID),
			Holds:   []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-theirs", recoveryHeldRunID)},
		},
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", "--user", debugOwnerID)
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "hold-theirs") || strings.Contains(out, "run discard") || strings.Contains(out, "uzi run export") {
		t.Errorf("stdout:\n%s", out)
	}
	if strings.Count(errb, adminRunDebugReadOnlyLine) != 1 {
		t.Errorf("stderr = %q, want the read-only line once", errb)
	}
	if !reflect.DeepEqual(fc.RecoveryHoldsScopedCalls, []uzicli.RecoveryHoldsScopedCall{{UserID: debugOwnerID}}) {
		t.Errorf("scoped calls = %+v", fc.RecoveryHoldsScopedCalls)
	}
	jsonOut, jerr, code := runCLI(t, fakeEnv(fc), "run", "recovery", "--user", debugOwnerID, "--json")
	var dto apitypes.RecoveryCustodyHoldsDTO
	if code != uzicli.ExitOK || json.Unmarshal([]byte(jsonOut), &dto) != nil || len(dto.Holds) != 1 || dto.Holds[0].ID != "hold-theirs" {
		t.Errorf("json: exit=%d out=%s", code, jsonOut)
	}
	if strings.Contains(jerr, adminRunDebugReadOnlyLine) {
		t.Errorf("json mode wrote the human notice: %q", jerr)
	}
}

func TestRunRecoveryUserRejectsUnscopedOrMismatchedOwner(t *testing.T) {
	for name, owner := range map[string]string{"missing": "", "mismatched": debugOtherID} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{RecoveryHoldsScopedResult: apitypes.RecoveryCustodyHoldsDTO{
				OwnerID: owner, Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-mine", recoveryHeldRunID)},
			}}
			out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", "--user", debugOwnerID)
			if code != uzicli.ExitGeneric || out != "" ||
				!strings.Contains(errb, "the server did not return holds scoped to user "+debugOwnerID+"; it may predate admin run debug") {
				t.Errorf("exit=%d stdout=%q stderr=%q", code, out, errb)
			}
		})
	}
}

func TestRunRecoveryUserUsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"with a run id": {"run", "recovery", recoveryHeldRunID, "--user", debugOwnerID},
		"not a uuid":    {"run", "recovery", "--user", "alice"},
		"empty":         {"run", "recovery", "--user", ""},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, _, code := runCLI(t, fakeEnv(fc), args...)
			if code != uzicli.ExitUsage || len(fc.RecoveryHoldsScopedCalls) != 0 {
				t.Errorf("exit=%d scoped calls=%d; want a usage error before any read", code, len(fc.RecoveryHoldsScopedCalls))
			}
		})
	}
}

// The prerequisites hint: one case per read that the exception could have served.

func TestAdminRunDebugHintOnRefusedReads(t *testing.T) {
	notFound := uzicli.Exitf(uzicli.ExitNotFound, "run not found")
	forbidden := uzicli.Exitf(uzicli.ExitAuth, "forbidden: your token lacks the required scope")
	for _, tc := range []struct {
		name     string
		fc       *uzicli.FakeClient
		args     []string
		wantCode int
		hint     bool
	}{
		{"run inputs 404", &uzicli.FakeClient{}, []string{"run", "inputs", recoveryHeldRunID}, uzicli.ExitNotFound, true},
		{"run fetches 404", &uzicli.FakeClient{RunFetchesErr: notFound}, []string{"run", "fetches", recoveryHeldRunID}, uzicli.ExitNotFound, true},
		{"run recovery run 404", &uzicli.FakeClient{RecoveryHoldsScopedErr: notFound}, []string{"run", "recovery", recoveryHeldRunID}, uzicli.ExitOK, true},
		{"run recovery user 404", &uzicli.FakeClient{RecoveryHoldsScopedErr: notFound}, []string{"run", "recovery", "--user", debugOwnerID}, uzicli.ExitNotFound, true},
		{"admin runs all 403", &uzicli.FakeClient{AdminRunsDebugErr: forbidden}, []string{"admin", "runs", "--all"}, uzicli.ExitAuth, true},
		{"run recovery run scoped 403", &uzicli.FakeClient{RecoveryHoldsScopedErr: forbidden}, []string{"run", "recovery", recoveryHeldRunID}, uzicli.ExitOK, true},
		{"run recovery user 403", &uzicli.FakeClient{RecoveryHoldsScopedErr: forbidden}, []string{"run", "recovery", "--user", debugOwnerID}, uzicli.ExitAuth, true},
		{"run recovery archives 403", &uzicli.FakeClient{RecoveryArchivesErr: forbidden}, []string{"run", "recovery", recoveryHeldRunID}, uzicli.ExitOK, true},
		{"run recovery archives 404 no holds", &uzicli.FakeClient{RecoveryArchivesErr: notFound}, []string{"run", "recovery", recoveryHeldRunID}, uzicli.ExitOK, true},
		// Controls: a transport failure is not a refusal, so no hint.
		{"run inputs unreachable", &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitUnreachable, "down")}, []string{"run", "inputs", recoveryHeldRunID}, uzicli.ExitUnreachable, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, errb, code := runCLI(t, fakeEnv(tc.fc), tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit=%d want %d; stderr=%q", code, tc.wantCode, errb)
			}
			if got := strings.Contains(errb, adminRunDebugHint); got != tc.hint {
				t.Errorf("hint present = %v, want %v; stderr=%q", got, tc.hint, errb)
			}
		})
	}
	t.Run("admin runs all 403 keeps the server message", func(t *testing.T) {
		_, errb, _ := runCLI(t, fakeEnv(&uzicli.FakeClient{AdminRunsDebugErr: forbidden}), "admin", "runs", "--all")
		if !strings.Contains(errb, "forbidden: your token lacks the required scope") {
			t.Errorf("stderr=%q lost the server message", errb)
		}
	})
}

func debugSummaryRow(id, email, title string) apitypes.RunSummaryItemDTO {
	var r apitypes.RunSummaryItemDTO
	r.ID, r.Kind, r.Status, r.TriggerSource, r.IssueTitle = id, "issue", "failed", "manual", title
	r.OwnerEmail = &email
	return r
}

func TestAdminRunsAllRendersTableAndTruncationNote(t *testing.T) {
	fc := &uzicli.FakeClient{AdminRunsDebugResult: apitypes.AdminRunsDebugDTO{
		Runs:      []apitypes.RunSummaryItemDTO{debugSummaryRow("run-aaa", "a@example.com", "first title")},
		Truncated: true,
	}}
	out, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--user", debugOwnerID, "--since", "2026-01-02T03:04:05Z")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if got := strings.Fields(strings.SplitN(out, "\n", 2)[0]); !reflect.DeepEqual(got, []string{"ID", "OWNER", "KIND", "TRIGGER", "STATUS", "TITLE"}) {
		t.Errorf("headers = %v", got)
	}
	for _, want := range []string{"run-aaa", "a@example.com", "issue", "manual", "failed", "first title"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errb, "note: showing the newest 500 runs; narrow with --user or --since") {
		t.Errorf("stderr = %q, want the truncation note", errb)
	}
	if !reflect.DeepEqual(fc.AdminListRunsDebugCalls, []uzicli.AdminListRunsDebugCall{{UserID: debugOwnerID, Since: "2026-01-02T03:04:05Z"}}) {
		t.Errorf("calls = %+v", fc.AdminListRunsDebugCalls)
	}
	if fc.AdminListRunsCalls != 0 {
		t.Errorf("--all must not use the active listing")
	}
}

func TestAdminRunsAllJSONEnvelope(t *testing.T) {
	fc := &uzicli.FakeClient{AdminRunsDebugResult: apitypes.AdminRunsDebugDTO{}}
	out, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--json")
	if code != uzicli.ExitOK || errb != "" {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if runs, ok := got["runs"].([]any); !ok || len(runs) != 0 || got["truncated"] != false {
		t.Errorf("envelope = %s, want {runs: [], truncated: false}", out)
	}
}

func TestAdminRunsFlagValidation(t *testing.T) {
	for name, args := range map[string][]string{
		"user without all":  {"admin", "runs", "--user", debugOwnerID},
		"since without all": {"admin", "runs", "--since", "2026-01-02T03:04:05Z"},
		"bad user":          {"admin", "runs", "--all", "--user", "alice"},
		"bad since":         {"admin", "runs", "--all", "--since", "yesterday"},
		"date only since":   {"admin", "runs", "--all", "--since", "2026-01-02"},
		"empty user":        {"admin", "runs", "--user", ""},
		"empty since":       {"admin", "runs", "--since", ""},
		"all empty user":    {"admin", "runs", "--all", "--user", ""},
		"all empty since":   {"admin", "runs", "--all", "--since", ""},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{}
			_, _, code := runCLI(t, fakeEnv(fc), args...)
			if code != uzicli.ExitUsage || len(fc.AdminListRunsDebugCalls) != 0 || fc.AdminListRunsCalls != 0 {
				t.Errorf("exit=%d; want a usage error before any read", code)
			}
		})
	}
}

func TestAdminRunsWithoutFlagsUsesTheActiveListing(t *testing.T) {
	fc := &uzicli.FakeClient{}
	if _, _, code := runCLI(t, fakeEnv(fc), "admin", "runs"); code != uzicli.ExitOK || fc.AdminListRunsCalls != 1 || len(fc.AdminListRunsDebugCalls) != 0 {
		t.Errorf("exit=%d active=%d debug=%d", code, fc.AdminListRunsCalls, len(fc.AdminListRunsDebugCalls))
	}
}

func TestRunExportRefusesAnotherUsersRunBeforeDownload(t *testing.T) {
	data := []byte("bundle")
	fc := &uzicli.FakeClient{
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"r1": {Supported: true, AdminDebugRead: true, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", data)}},
		},
		RecoveryBytes: map[string][]byte{"cap-1": data},
	}
	dest := filepath.Join(t.TempDir(), "out.bundle")
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "export", "r1", "--output", dest)
	if code != uzicli.ExitNotFound || !strings.Contains(errb, "run export of another user's run is refused: admin run debug is metadata-only; captures stay owner-only") {
		t.Errorf("exit=%d stderr=%q", code, errb)
	}
	if len(fc.RecoveryDownloadCalls) != 0 {
		t.Errorf("download attempted: %v", fc.RecoveryDownloadCalls)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("a file exists at the destination: %v", err)
	}
}

func TestRunGetLandingHintOmitsExportForAnotherUsersRun(t *testing.T) {
	const id = "00000000-0000-0000-0000-000000000016"
	for _, debug := range []bool{false, true} {
		fc := &uzicli.FakeClient{
			RunByID: map[string]apitypes.RunDTO{id: {ID: id, Kind: "issue", Status: "failed", LandingState: "needs_landing"}},
			RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
				id: {Supported: true, AdminDebugRead: debug, Archives: []apitypes.RecoveryArchiveDTO{availableCapture("cap-1", []byte("bundle"))}},
			},
		}
		out, _, code := runCLI(t, fakeEnv(fc), "run", "get", id)
		if code != uzicli.ExitOK {
			t.Fatalf("exit=%d", code)
		}
		hasExport := strings.Contains(out, "`uzi run export "+id+"`")
		if hasExport == debug {
			t.Errorf("admin debug=%v: export suggestion present=%v:\n%s", debug, hasExport, out)
		}
	}
}

func TestSteerAccessIsReadOnlyUnderAdminDebugMarker(t *testing.T) {
	const runID = "r-debug"
	for _, tc := range []struct {
		name  string
		debug bool
		want  steerAccess
	}{
		{"owner read", false, steerAllowed},
		{"admin debug read", true, steerNotOwner},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &uzicli.FakeClient{
				InputsByID:          map[string][]apitypes.SteerInputDTO{runID: {{ID: 1, Kind: "follow_up"}}},
				RunInputsAdminDebug: map[string]bool{runID: tc.debug},
			}
			m := tuiTestModel(t, fake, runID)
			m = applyDetail(m, ownedRun(runID), nil)
			// Through the real probe command, so the marker must survive fetchInputsCmd.
			next, _ := m.Update(m.fetchInputsCmd(runID)())
			m = next.(tuiModel)
			if m.detail.steer.access != tc.want {
				t.Fatalf("access = %v, want %v", m.detail.steer.access, tc.want)
			}
			if tc.debug {
				if len(m.detail.steer.queue) != 1 {
					t.Errorf("the queue card must stay visible read-only; queue = %+v", m.detail.steer.queue)
				}
				for _, k := range []string{"f", "x", "y", "n"} {
					if nm, cmd := m.handleKey(k); cmd != nil || nm.(tuiModel).detail.steer.mode != steerIdle {
						t.Errorf("key %q was not inert under admin debug", k)
					}
				}
			}
		})
	}
}

// The prerequisites hint prints at most once per command, even when both the scoped holds read and
// the archives read are refused.
func TestRunRecoveryHintPrintsOnce(t *testing.T) {
	notFound := uzicli.Exitf(uzicli.ExitNotFound, "run not found")
	fc := &uzicli.FakeClient{RecoveryHoldsScopedErr: notFound, RecoveryArchivesErr: notFound}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID)
	if code != uzicli.ExitOK || strings.Count(errb, adminRunDebugHint) != 1 {
		t.Errorf("exit=%d stderr=%q; want exactly one hint", code, errb)
	}
}

// An own run whose holds are listed keeps today's silent degrade on an archives 404 (a hold can
// outlive its run): no admin-debug hint there.
func TestRunRecoveryArchives404WithOwnHoldsHasNoHint(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{debugHold("hold-own", recoveryHeldRunID)}},
		RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitNotFound, "run not found"),
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", recoveryHeldRunID)
	if code != uzicli.ExitOK || strings.Contains(errb, adminRunDebugHint) {
		t.Errorf("exit=%d stderr=%q; want no hint", code, errb)
	}
}
