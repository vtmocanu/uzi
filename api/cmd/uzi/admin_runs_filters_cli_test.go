package main

// admin_runs_filters_cli_test.go covers `uzi admin runs --all --status` and `--user <email>`
// (issue #4614): email resolution through `admin users`, the usage errors raised before any
// network call, and the refusal of a server that ignored the status filter.

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestAdminRunsAllStatusSendsDedupedStatuses(t *testing.T) {
	fc := &uzicli.FakeClient{AdminRunsDebugResult: apitypes.AdminRunsDebugDTO{
		Runs:         []apitypes.RunSummaryItemDTO{debugSummaryRow("run-aaa", "a@example.com", "t")},
		StatusFilter: []string{"failed"},
	}}
	_, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--status", "failed,failed")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	want := []uzicli.AdminListRunsDebugCall{{Statuses: []string{"failed"}}}
	if !reflect.DeepEqual(fc.AdminListRunsDebugCalls, want) {
		t.Errorf("calls = %+v, want %+v", fc.AdminListRunsDebugCalls, want)
	}
}

func TestAdminRunsAllStatusRefusesAnOlderServer(t *testing.T) {
	failed := debugSummaryRow("run-aaa", "a@example.com", "t")
	other := debugSummaryRow("run-bbb", "a@example.com", "t")
	other.Status = "completed"
	for name, res := range map[string]apitypes.AdminRunsDebugDTO{
		"missing status_filter": {Runs: []apitypes.RunSummaryItemDTO{failed}},
		"mismatched filter":     {Runs: []apitypes.RunSummaryItemDTO{failed}, StatusFilter: []string{"cancelled"}},
		"row outside the set":   {Runs: []apitypes.RunSummaryItemDTO{failed, other}, StatusFilter: []string{"failed"}},
	} {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{AdminRunsDebugResult: res}
			out, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--status", "failed")
			if code != uzicli.ExitGeneric || !strings.Contains(errb, "this uzi server does not support --status; upgrade the server") {
				t.Errorf("exit=%d stderr=%q", code, errb)
			}
			if strings.Contains(out, "run-aaa") {
				t.Errorf("rows were printed from a refused reply: %q", out)
			}
		})
	}
}

func TestAdminRunsAllUserEmailResolvesCaseInsensitively(t *testing.T) {
	fc := &uzicli.FakeClient{
		AdminUsers: []apitypes.UserDTO{
			{ID: debugOtherID, Email: "bob@example.com"},
			{ID: debugOwnerID, Email: "Alice@Example.com"},
		},
	}
	_, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--user", "aLICE@example.COM")
	if code != uzicli.ExitOK {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if !reflect.DeepEqual(fc.AdminListRunsDebugCalls, []uzicli.AdminListRunsDebugCall{{UserID: debugOwnerID}}) {
		t.Errorf("calls = %+v, want one call for %s", fc.AdminListRunsDebugCalls, debugOwnerID)
	}
}

func TestAdminRunsAllUserEmailFailuresMakeNoListingCall(t *testing.T) {
	cases := map[string]struct {
		users    []apitypes.UserDTO
		wantCode int
		wantMsg  string
	}{
		"unknown": {
			users:    []apitypes.UserDTO{{ID: debugOtherID, Email: "bob@example.com"}},
			wantCode: uzicli.ExitNotFound,
			wantMsg:  "no user with email",
		},
		"ambiguous": {
			users: []apitypes.UserDTO{
				{ID: debugOwnerID, Email: "alice@example.com"},
				{ID: debugOtherID, Email: "ALICE@example.com"},
			},
			wantCode: uzicli.ExitGeneric,
			wantMsg:  debugOwnerID + ", " + debugOtherID,
		},
		"malformed id": {
			users:    []apitypes.UserDTO{{ID: "not-a-uuid", Email: "alice@example.com"}},
			wantCode: uzicli.ExitGeneric,
			wantMsg:  "invalid user id",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{AdminUsers: tc.users}
			_, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--user", "alice@example.com")
			if code != tc.wantCode || !strings.Contains(errb, tc.wantMsg) {
				t.Errorf("exit=%d stderr=%q; want exit %d containing %q", code, errb, tc.wantCode, tc.wantMsg)
			}
			if len(fc.AdminListRunsDebugCalls) != 0 {
				t.Errorf("listing called: %+v", fc.AdminListRunsDebugCalls)
			}
		})
	}
}

func TestAdminRunsAllUserEmailRendersServerValuesSafely(t *testing.T) {
	fc := &uzicli.FakeClient{AdminUsers: []apitypes.UserDTO{
		{ID: "id-1\x1b[31m", Email: "alice@example.com"},
		{ID: "id-2\nforged row", Email: "alice@example.com"},
	}}
	_, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--user", "alice@example.com")
	if code != uzicli.ExitGeneric {
		t.Fatalf("exit=%d stderr=%q", code, errb)
	}
	if strings.ContainsRune(errb, 0x1b) || strings.Contains(errb, "id-2\nforged") {
		t.Errorf("stderr carries raw control bytes: %q", errb)
	}
}

func TestAdminRunsAllUserEmailListErrorGetsTheHint(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitAuth, "forbidden: your token lacks the required scope")}
	_, errb, code := runCLI(t, fakeEnv(fc), "admin", "runs", "--all", "--user", "alice@example.com")
	if code != uzicli.ExitAuth || !strings.Contains(errb, adminRunDebugHint) {
		t.Errorf("exit=%d stderr=%q, want ExitAuth with the prerequisites hint", code, errb)
	}
	if len(fc.AdminListRunsDebugCalls) != 0 {
		t.Errorf("listing called: %+v", fc.AdminListRunsDebugCalls)
	}
}
