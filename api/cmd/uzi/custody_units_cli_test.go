package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// Issue #4162: owner custody admission counts admission UNITS (one run's open holds on one
// original worker form one unit) and a run whose own holds reach the limit is refused with its
// own health reason. These tests drive the CLI over the shared api-contract fixtures and over an
// old-server shape, so a new client and an old server stay compatible.

func readContractFixture(t *testing.T, name string, into any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", "api-contract", name)) //nolint:gosec // G304: test reads a fixed repo-relative fixture path
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("decode fixture %s: %v", name, err)
	}
}

func TestCustodyUnitsGroupedFixtureRenders(t *testing.T) {
	var dto apitypes.RecoveryCustodyHoldsDTO
	readContractFixture(t, "recovery_custody_holds.grouped.json", &dto)
	out, errb, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	for _, want := range []string{
		"open_holds: 6  admission_units (admission_counted_holds): 3  custody_hold_limit: 3",
		"not counted (same_run_group)",
		"counted (run_not_live)",
		"not counted (live_claim)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Two grouped holds are non-representative members.
	if n := strings.Count(out, "not counted (same_run_group)"); n != 2 {
		t.Errorf("same_run_group cells = %d, want 2:\n%s", n, out)
	}
	jsonOut, errb, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery", "--json")
	if code != uzicli.ExitOK || !strings.Contains(jsonOut, `"admission_counted_holds": 3`) {
		t.Errorf("json exit=%d stderr=%q out=%s", code, errb, jsonOut)
	}
}

func TestCustodyUnitsGroupedZeroFixtureRenders(t *testing.T) {
	var dto apitypes.RecoveryCustodyHoldsDTO
	readContractFixture(t, "recovery_custody_holds.grouped-zero.json", &dto)
	out, errb, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	if !strings.Contains(out, "no open custody holds") ||
		!strings.Contains(out, "open_holds: 0  admission_units (admission_counted_holds): 0  custody_hold_limit: 3") {
		t.Errorf("zero grouped view: %q", out)
	}
}

func TestCustodyUnitsAdmissionCellReasons(t *testing.T) {
	now := time.Now().Add(-time.Hour)
	mk := func(id string, counts *bool, reason string) apitypes.RecoveryCustodyHoldDTO {
		return apitypes.RecoveryCustodyHoldDTO{ID: id, RunID: "run-" + id, Generation: 1, State: "open", Attention: "source_only",
			WorkerID: "w-" + id, CreatedAt: now, CountsTowardAdmission: counts, AdmissionReason: reason}
	}
	dto := apitypes.RecoveryCustodyHoldsDTO{
		Aggregate: apitypes.RecoveryCustodyAggregateDTO{OpenHolds: 4, AdmissionCountedHolds: 0, CustodyHoldLimit: 8},
		Holds: []apitypes.RecoveryCustodyHoldDTO{
			mk("grp", boolPtr(false), "same_run_group"),
			mk("unk", boolPtr(false), "future_reason\x1b[31m"),
			mk("emp", boolPtr(false), ""),
			mk("old", nil, ""),
		},
	}
	out, errb, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("unsanitized reason reached the table: %q", out)
	}
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		for _, id := range []string{"grp", "unk", "emp", "old"} {
			if strings.HasPrefix(strings.TrimSpace(line), "run-"+id+" ") {
				rows[id] = line
			}
		}
	}
	if !strings.Contains(rows["grp"], "not counted (same_run_group)") {
		t.Errorf("grouped row = %q", rows["grp"])
	}
	if !strings.Contains(rows["unk"], "not counted (future_reason") {
		t.Errorf("unknown-reason row = %q", rows["unk"])
	}
	if !strings.Contains(rows["emp"], "not counted") || strings.Contains(rows["emp"], "not counted (") {
		t.Errorf("empty-reason row = %q, want bare \"not counted\"", rows["emp"])
	}
	if strings.Contains(rows["old"], "not counted") || strings.Contains(rows["old"], "counted (") {
		t.Errorf("nil-membership (old server) row = %q, want an unknown marker", rows["old"])
	}
}

func TestCustodyUnitsOldServerShapeRenders(t *testing.T) {
	const serverJSON = `{"aggregate":{"open_holds":2,"custody_hold_limit":8,"decision_needed":1,"blocked_runs":0},
	  "holds":[
	    {"id":"hold-a","run_id":"run-a","generation":1,"state":"open","attention":"source_only","worker_id":"w1",
	     "has_available_capture":false,"created_at":"2026-09-27T10:00:00Z","updated_at":"2026-09-27T10:00:00Z"},
	    {"id":"hold-b","run_id":"run-b","generation":1,"state":"open","attention":"needs_action","worker_id":"w2",
	     "has_available_capture":false,"created_at":"2026-09-27T10:00:00Z","updated_at":"2026-09-27T10:00:00Z"}
	  ]}`
	var dto apitypes.RecoveryCustodyHoldsDTO
	if err := json.Unmarshal([]byte(serverJSON), &dto); err != nil {
		t.Fatal(err)
	}
	out, errb, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	if !strings.Contains(out, "open_holds: 2  admission_units (admission_counted_holds): 0  custody_hold_limit: 8") ||
		strings.Contains(out, "not counted") {
		t.Errorf("old-server view: %q", out)
	}
}

func TestCustodyRunHoldBoundFixtureRenders(t *testing.T) {
	var run apitypes.RunDTO
	readContractFixture(t, "run.custody_run_hold_bound.json", &run)
	if run.HealthReason == nil || *run.HealthReason != apitypes.ReasonCustodyRunHoldBound {
		t.Fatalf("fixture health_reason = %v, want the run-bound reason", run.HealthReason)
	}
	var detail bytes.Buffer
	if err := renderRunDetail(uzicli.NewPrinter(&detail, false, false, true, false), run); err != nil {
		t.Fatal(err)
	}
	if out := detail.String(); !strings.Contains(out, "HEALTH_REASON") || !strings.Contains(out, "custody hold limit") {
		t.Fatalf("run detail lacks the bound health reason:\n%s", out)
	}
	// The retained queued blocker survives the compact status cell like the owner custody reason.
	want := "queued (" + apitypes.ReasonCustodyRunHoldBound + ")"
	if got := runStatusCellAt(apitypes.RunListItemDTO{RunDTO: run}, time.Now()); got != want {
		t.Fatalf("status cell = %q, want %q", got, want)
	}
}
