package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #3454 (Visibility): the CLI shows the create-time notice on stderr, the derived queue
// reason on `run get`, and the read-only `uzi admin maintenance status`.

const (
	hostileANSI = "\x1b[2Jevil"
	hostileBidi = "a\u202eb"
)

func maintenanceDoc(clear bool) apitypes.MaintenanceAdminDTO {
	doc := apitypes.MaintenanceAdminDTO{
		State: apitypes.MaintenanceAdminStateDTO{Active: true, Since: strp("2026-10-26T10:00:00Z"),
			Reason: strp("node drain"), ByLabel: strp("Ada Admin")},
		Readout: apitypes.MaintenanceReadoutDTO{
			AsOf: "2026-10-26T10:30:00Z", Clear: clear,
			Active:           apitypes.MaintenanceSectionDTO{Items: []apitypes.MaintenanceItemDTO{}},
			Held:             apitypes.MaintenanceSectionDTO{Items: []apitypes.MaintenanceItemDTO{}},
			Custody:          apitypes.MaintenanceSectionDTO{Items: []apitypes.MaintenanceItemDTO{}},
			Queued:           apitypes.MaintenanceSectionDTO{Count: 4, Items: []apitypes.MaintenanceItemDTO{}},
			IdleBoundWorkers: apitypes.MaintenanceWorkersDTO{Count: 1, Items: []apitypes.MaintenanceWorkerDTO{}},
		},
	}
	if !clear {
		doc.Readout.Active = apitypes.MaintenanceSectionDTO{Count: 1, Items: []apitypes.MaintenanceItemDTO{
			{RunID: "run-active-1", Kind: "issue", Status: "running", OwnerLabel: "Bob", Repo: strp("acme/widgets"), Since: strp("2026-10-26T08:30:00Z")},
		}}
		doc.Readout.Held = apitypes.MaintenanceSectionDTO{Count: 2, Items: []apitypes.MaintenanceItemDTO{
			{RunID: "run-held-1", Kind: "chat", Status: "awaiting_input", OwnerLabel: "Cy", Since: strp("2026-10-26T10:15:00Z"), Tag: strp("needs_owner")},
		}}
		doc.Readout.Custody = apitypes.MaintenanceSectionDTO{Count: 1, Items: []apitypes.MaintenanceItemDTO{
			{RunID: "run-cust-1", Kind: "task", Status: "failed", OwnerLabel: "Di", Repo: strp("acme/gadgets"), Since: strp("2026-10-24T10:30:00Z"), Tag: strp("progressing"), HoldID: strp("h1")},
		}}
	}
	return doc
}

func TestAdminMaintenanceStatusNotClear(t *testing.T) {
	fc := &uzicli.FakeClient{MaintenanceV: maintenanceDoc(false)}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "maintenance", "status")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 even when not clear", code)
	}
	for _, want := range []string{
		"Maintenance mode: ON", "2026-10-26T10:00:00Z", "Ada Admin", "node drain", "As of:  2026-10-26T10:30:00Z",
		"NOT CLEAR: 1 active, 2 held and 1 in recovery custody", "re-run this command right before evicting",
		"Active runs:        1", "Held runs:          2", "Recovery custody:   1", "Queued (waiting):   4", "Idle bound workers: 1",
		"Active (showing 1 of 1)", "Held (showing 1 of 2)", "Recovery custody (showing 1 of 1)",
		"run-active-1", "acme/widgets", "2h", "run-held-1", "needs_owner", "15m", "run-cust-1", "2d", "progressing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "CLEAR: no running") {
		t.Errorf("a not-clear readout printed the CLEAR line:\n%s", out)
	}
}

func TestAdminMaintenanceStatusClear(t *testing.T) {
	fc := &uzicli.FakeClient{MaintenanceV: maintenanceDoc(true)}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "maintenance", "status")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"CLEAR: no running or held work is visible (as of 2026-10-26T10:30:00Z).", "re-run this command right before evicting"} {
		if !strings.Contains(out, want) {
			t.Errorf("clear output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "NOT CLEAR") || strings.Contains(out, "RUN  ") {
		t.Errorf("clear output printed a blocker table or NOT CLEAR:\n%s", out)
	}
}

func TestAdminMaintenanceStatusOff(t *testing.T) {
	doc := maintenanceDoc(true)
	doc.State = apitypes.MaintenanceAdminStateDTO{}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{MaintenanceV: doc}), "admin", "maintenance", "status")
	if code != uzicli.ExitOK || !strings.Contains(out, "Maintenance mode: OFF") || strings.Contains(out, "Since:") {
		t.Fatalf("off state mis-rendered (exit %d):\n%s", code, out)
	}
}

func TestAdminMaintenanceStatusSanitizesServerStrings(t *testing.T) {
	doc := maintenanceDoc(false)
	doc.State.Reason = strp(hostileANSI)
	doc.State.ByLabel = strp(hostileBidi)
	doc.Readout.Active.Items[0].OwnerLabel = hostileANSI
	doc.Readout.Active.Items[0].Repo = strp(hostileBidi)
	doc.Readout.Held.Items[0].Tag = strp(hostileANSI)
	doc.Readout.Active.Items[0].RunID = hostileANSI
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{MaintenanceV: doc}), "admin", "maintenance", "status")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, '\u202e') {
		t.Errorf("control or bidi characters reached the terminal: %q", out)
	}
}

func TestAdminMaintenanceStatusJSON(t *testing.T) {
	doc := maintenanceDoc(false)
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{MaintenanceV: doc}), "admin", "maintenance", "status", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got apitypes.MaintenanceAdminDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not the raw DTO: %v\n%s", err, out)
	}
	if got.Readout.Clear || got.Readout.Held.Count != 2 || got.State.Reason == nil || *got.State.Reason != "node drain" {
		t.Errorf("json lost fields: %+v", got)
	}
}

func TestAdminMaintenanceStatusError(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{Err: errors.New("boom")}), "admin", "maintenance", "status")
	if code == uzicli.ExitOK {
		t.Fatal("an API error must not exit 0")
	}
}

func TestAdminMaintenanceTree(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))
	m := findCmd(findCmd(root, "admin"), "maintenance")
	if m == nil || findCmd(m, "status") == nil {
		t.Fatal("missing admin maintenance status")
	}
	if len(m.Commands()) != 1 {
		t.Errorf("admin maintenance must stay read-only, has %d subcommands", len(m.Commands()))
	}
}

func TestRunGetQueueReason(t *testing.T) {
	maint := "maintenance"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Status: "queued", QueueReason: &maint},
		"r2": {ID: "r2", Status: "queued"},
		"r3": {ID: "r3", Status: "queued", QueueReason: strp(hostileANSI)},
	}}
	out, _, _ := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if !strings.Contains(out, "QUEUE_REASON") || !strings.Contains(out, "maintenance (uzi is in maintenance mode)") {
		t.Errorf("queue reason not rendered:\n%s", out)
	}
	if out, _, _ = runCLI(t, fakeEnv(fc), "run", "get", "r2"); strings.Contains(out, "QUEUE_REASON") {
		t.Errorf("a run without a queue reason got a row:\n%s", out)
	}
	if out, _, _ = runCLI(t, fakeEnv(fc), "run", "get", "r3"); strings.ContainsRune(out, 0x1b) {
		t.Errorf("queue reason not sanitized: %q", out)
	}
}

// runCLIBuffers runs the CLI and returns separate stdout and stderr.
func runCLIBuffers(t *testing.T, env Env, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	env.Stdout, env.Stderr = &out, &errb
	code := Main(env, args)
	return out.String(), errb.String(), code
}

// `uzi run rework` and `uzi ci fix` create runs too, so the server's maintenance notice must
// reach stderr (sanitized) for them as well, with stdout left clean.
func TestReworkAndCIFixNoticeGoesToStderr(t *testing.T) {
	notice := "uzi is in maintenance mode: new work is queued. Reason: " + hostileANSI
	t.Run("run rework", func(t *testing.T) {
		fc := &uzicli.FakeClient{
			RunByID:       map[string]apitypes.RunDTO{"r1": {ID: "r1"}},
			ReworkRun:     apitypes.RunDTO{ID: "r2", Kind: "mr_rework", Status: "queued"},
			CreatedNotice: notice,
		}
		out, errs, code := runCLIBuffers(t, fakeEnv(fc), "run", "rework", "r1")
		if code != uzicli.ExitOK || !strings.Contains(errs, "warning: uzi is in maintenance mode") || strings.ContainsRune(errs, 0x1b) {
			t.Fatalf("exit %d, stderr = %q", code, errs)
		}
		if strings.Contains(out, "maintenance mode") {
			t.Errorf("notice leaked to stdout:\n%s", out)
		}
	})
	t.Run("ci fix", func(t *testing.T) {
		fc := &uzicli.FakeClient{CIFixRunResult: apitypes.RunDTO{ID: "run-123", Status: "queued"}, CreatedNotice: notice}
		out, errs, code := runCLIBuffers(t, fakeEnv(fc), "ci", "fix", "main", "--repo", "r1")
		if code != uzicli.ExitOK || !strings.Contains(errs, "warning: uzi is in maintenance mode") || strings.ContainsRune(errs, 0x1b) {
			t.Fatalf("exit %d, stderr = %q", code, errs)
		}
		if strings.Contains(out, "maintenance mode") || !strings.Contains(out, "run-123") {
			t.Errorf("stdout = %q", out)
		}
	})
}

func TestCreateNoticeGoesToStderrSanitized(t *testing.T) {
	notice := "uzi is in maintenance mode: this run is queued. Reason: " + hostileANSI
	t.Run("run create", func(t *testing.T) {
		t.Setenv("UZI_TOKEN", "")
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "queued"}, CreatedNotice: notice}
		out, errs, code := runCLIBuffers(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42")
		if code != uzicli.ExitOK || !strings.Contains(errs, "warning: uzi is in maintenance mode") || strings.ContainsRune(errs, 0x1b) {
			t.Fatalf("exit %d, stderr = %q", code, errs)
		}
		if strings.Contains(out, "maintenance mode") {
			t.Errorf("notice leaked to stdout:\n%s", out)
		}
	})
	t.Run("run create json", func(t *testing.T) {
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "queued"}, CreatedNotice: "plain notice"}
		out, errs, code := runCLIBuffers(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--json")
		var env struct {
			Run    apitypes.RunDTO `json:"run"`
			Notice string          `json:"notice"`
		}
		if err := json.Unmarshal([]byte(out), &env); err != nil || code != uzicli.ExitOK || env.Run.ID != "r1" || env.Notice != "plain notice" {
			t.Fatalf("json envelope = %q (%v, exit %d)", out, err, code)
		}
		if !strings.Contains(errs, "warning: plain notice") {
			t.Errorf("stderr = %q", errs)
		}
	})
	t.Run("no notice", func(t *testing.T) {
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "queued"}}
		out, errs, _ := runCLIBuffers(t, fakeEnv(fc), "run", "create", "--repo", "p1", "--issue", "42", "--json")
		if strings.Contains(errs, "warning") || strings.Contains(out, "notice") {
			t.Errorf("unexpected notice: stdout %q stderr %q", out, errs)
		}
	})
	t.Run("job create", func(t *testing.T) {
		j := sampleJob()
		j.Notice = &notice
		_, errs, code := runCLIBuffers(t, fakeEnv(&uzicli.FakeClient{JobCreated: j}), "job", "create", "--type", "research", "--prompt", "p")
		if code != uzicli.ExitOK || !strings.Contains(errs, "warning: uzi is in maintenance mode") || strings.ContainsRune(errs, 0x1b) {
			t.Fatalf("exit %d, stderr = %q", code, errs)
		}
	})
	t.Run("schedule run-now", func(t *testing.T) {
		fc := &uzicli.FakeClient{RunNowResult: apitypes.RunNowResponse{Created: 1, RunIDs: []string{"r1"}, Notice: &notice}}
		_, errs, code := runCLIBuffers(t, fakeEnv(fc), "schedule", "run-now", "sch1")
		if code != uzicli.ExitOK || !strings.Contains(errs, "warning: uzi is in maintenance mode") || strings.ContainsRune(errs, 0x1b) {
			t.Fatalf("exit %d, stderr = %q", code, errs)
		}
	})
	t.Run("handoff", func(t *testing.T) {
		fc := &uzicli.FakeClient{
			CreatedTaskRun: taskRun("r1", "uzi/task/r1"), DispatchedRun: taskRun("r1", "uzi/task/r1"), CreatedNotice: notice,
		}
		env, _ := handoffEnv(fc, &handoffRecorder{gitOut: map[string]string{"rev-parse HEAD": "c0ffee1234"}})
		_, errs, code := runCLIBuffers(t, env, "handoff", "--repo", "p1", "-m", "do X")
		if code != uzicli.ExitOK || !strings.Contains(errs, "warning: uzi is in maintenance mode") || strings.ContainsRune(errs, 0x1b) {
			t.Fatalf("exit %d, stderr = %q", code, errs)
		}
	})
}
