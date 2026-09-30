package main

// run_recovery_test.go covers `uzi run recovery` and `uzi run discard` (PRD #1349 M5, D7/D9):
// the owner custody-hold LIST render and run filter, and the exact hold DISCARD's confirmation
// contract — interactive prompt, cancellation performing NO mutation, non-TTY hard-refusal
// without --yes, and --yes mapping straight to the discard call.

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// countingRecoveryClient keeps the call-count seam local to these command tests.
type countingRecoveryClient struct {
	uzicli.Client
	holdsCalls    int
	archivesCalls int
}

func (c *countingRecoveryClient) RecoveryHolds(ctx context.Context) (apitypes.RecoveryCustodyHoldsDTO, error) {
	c.holdsCalls++
	return c.Client.RecoveryHolds(ctx)
}

func (c *countingRecoveryClient) RecoveryArchives(ctx context.Context, runID string) (apitypes.RecoveryArchiveSummaryDTO, error) {
	c.archivesCalls++
	return c.Client.RecoveryArchives(ctx, runID)
}

func ownerRecoveryFixture() apitypes.RecoveryCustodyHoldsDTO {
	old := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	newer := old.Add(time.Hour)
	return apitypes.RecoveryCustodyHoldsDTO{
		Aggregate: apitypes.RecoveryCustodyAggregateDTO{OpenHolds: 3, CustodyHoldLimit: 8, DecisionNeeded: 2, BlockedRuns: 1},
		Holds: []apitypes.RecoveryCustodyHoldDTO{
			{ID: "hold-z", RunID: "run-z", Generation: 3, State: "open", Attention: "active",
				WorkerID: "worker-z", CreatedAt: newer},
			{ID: "hold-released", RunID: "run-r", Generation: 1, State: "released", Attention: "settled",
				WorkerID: "worker-r", CreatedAt: old.Add(-time.Hour)},
			{ID: "hold-discarded", RunID: "run-d", Generation: 1, State: "discarded", Attention: "settled",
				WorkerID: "worker-d", CreatedAt: old.Add(-2 * time.Hour)},
			{ID: "hold-b", RunID: "run-b", Generation: 2, State: "open", Attention: "needs_action",
				WorkerID: "worker-b", WorkerName: "beta", CreatedAt: old},
			{ID: "hold-a", RunID: "run-a", Generation: 1, State: "open", Attention: "source_only",
				WorkerID: "worker-a", CreatedAt: old},
		},
	}
}

func TestOwnerRecoveryHuman(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: ownerRecoveryFixture(),
		RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitGeneric, "archives must not be read")}
	client := &countingRecoveryClient{Client: fc}
	out, errb, code := runCLI(t, fakeEnv(client), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	if client.holdsCalls != 1 || client.archivesCalls != 0 {
		t.Fatalf("calls: holds=%d archives=%d; want 1, 0", client.holdsCalls, client.archivesCalls)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if got := strings.Fields(lines[0]); !reflect.DeepEqual(got, []string{"RUN", "ID", "HOLD", "ID", "GEN", "DISPOSITION", "ARCHIVE", "WORKER", "AGE"}) {
		t.Fatalf("headers = %q", lines[0])
	}
	for i, want := range [][]string{
		{"run-a", "hold-a", "1", "source_only", "false", "worker-a"},
		{"run-b", "hold-b", "2", "needs_action", "false", "beta"},
		{"run-z", "hold-z", "3", "active", "false", "worker-z"},
	} {
		got := strings.Fields(lines[i+1])
		if len(got) != 7 || !reflect.DeepEqual(got[:6], want) || got[6] == "-" {
			t.Errorf("row %d = %q, want %v plus age", i, lines[i+1], want)
		}
	}
	if strings.Contains(out, "hold-released") || strings.Contains(out, "hold-discarded") ||
		strings.Contains(out, "run-r") || strings.Contains(out, "run-d") {
		t.Errorf("settled hold in human view: %q", out)
	}
	if !strings.Contains(out, "open_holds: 3  custody_hold_limit: 8  decision_needed: 2  blocked_runs: 1") {
		t.Errorf("aggregate absent: %q", out)
	}
	if !strings.Contains(out, "2 hold(s) await a decision") || !strings.Contains(out, "run discard <run-id> --hold <hold-id> --yes") {
		t.Errorf("decision hint absent: %q", out)
	}
}

func TestOwnerRecoveryJSON(t *testing.T) {
	dto := ownerRecoveryFixture()
	fc := &uzicli.FakeClient{RecoveryHoldsResult: dto,
		RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitGeneric, "archives must not be read")}
	client := &countingRecoveryClient{Client: fc}
	out, errb, code := runCLI(t, fakeEnv(client), "run", "recovery", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, stderr=%q", code, errb)
	}
	if client.holdsCalls != 1 || client.archivesCalls != 0 {
		t.Fatalf("calls: holds=%d archives=%d; want 1, 0", client.holdsCalls, client.archivesCalls)
	}
	var got apitypes.RecoveryCustodyHoldsDTO
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, dto) {
		t.Errorf("JSON changed owner DTO: got %+v, want %+v", got, dto)
	}
	if strings.Contains(out, "captures") {
		t.Errorf("owner JSON gained per-run capture join: %s", out)
	}
}

func TestOwnerRecoveryEmptyAndNoHint(t *testing.T) {
	for _, holds := range [][]apitypes.RecoveryCustodyHoldDTO{nil, {{ID: "settled", RunID: "run-x", State: "released", Attention: "needs_action"}}} {
		fc := &uzicli.FakeClient{RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{
			Aggregate: apitypes.RecoveryCustodyAggregateDTO{CustodyHoldLimit: 8}, Holds: holds,
		}}
		out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery")
		if code != uzicli.ExitOK || !strings.Contains(out, "no open custody holds") ||
			!strings.Contains(out, "open_holds: 0  custody_hold_limit: 8  decision_needed: 0  blocked_runs: 0") ||
			strings.Contains(out, "await a decision") || strings.Contains(out, "settled") {
			t.Errorf("empty human view: code=%d output=%q", code, out)
		}
		out, _, code = runCLI(t, fakeEnv(fc), "run", "recovery", "--json")
		if code != uzicli.ExitOK || strings.Contains(out, `"holds": null`) {
			t.Errorf("empty JSON view: code=%d output=%q", code, out)
		}
		if holds == nil && !strings.Contains(out, `"holds": []`) {
			t.Errorf("nil holds did not render as []: %q", out)
		}
	}
}

func TestOwnerRecoveryActiveHasNoDecisionHint(t *testing.T) {
	dto := ownerRecoveryFixture()
	dto.Holds = []apitypes.RecoveryCustodyHoldDTO{dto.Holds[0]}
	dto.Aggregate.OpenHolds, dto.Aggregate.DecisionNeeded = 1, 0
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK || !strings.Contains(out, "hold-z") || strings.Contains(out, "await a decision") {
		t.Errorf("active-only view: code=%d output=%q", code, out)
	}
}

func TestOwnerRecoveryHelp(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "recovery", "--help")
	if code != uzicli.ExitOK {
		t.Fatalf("help exit = %d", code)
	}
	for _, want := range []string{"recovery [run-id]", "open custody holds", "including settled holds", "With a run id"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q: %s", want, out)
		}
	}
}

func TestOwnerRecoverySanitizesCells(t *testing.T) {
	dto := ownerRecoveryFixture()
	dto.Holds = []apitypes.RecoveryCustodyHoldDTO{{
		ID: "hold\nforged", RunID: "run\x1b[31m", Generation: 1, State: "open",
		Attention: "active\tbad", WorkerName: "worker\nforged", CreatedAt: time.Now().Add(-time.Hour),
	}}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.ContainsAny(out, "\x1b") || strings.Count(out, "\n") != 3 {
		t.Errorf("server fields escaped table cells: %q", out)
	}
}

func recoveryHoldsFixture() apitypes.RecoveryCustodyHoldsDTO {
	return apitypes.RecoveryCustodyHoldsDTO{
		Aggregate: apitypes.RecoveryCustodyAggregateDTO{OpenHolds: 2, CustodyHoldLimit: 8, DecisionNeeded: 1, BlockedRuns: 0},
		Holds: []apitypes.RecoveryCustodyHoldDTO{
			{ID: "hold-run1-gen1", RunID: "run1", Generation: 1, State: "open", Attention: "source_only",
				WorkerID: "w1", WorkerName: "alpha", CaptureState: ""},
			{ID: "hold-run2-gen1", RunID: "run2", Generation: 1, State: "open", Attention: "active",
				WorkerID: "w2", WorkerName: "beta"},
		},
	}
}

// TestRunRecoveryRenders proves `uzi run recovery <run-id>` renders the run's holds — the exact
// hold id, generation and disposition — and filters OUT holds belonging to other runs.
func TestRunRecoveryRenders(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "hold-run1-gen1") || !strings.Contains(out, "source_only") {
		t.Errorf("run recovery output missing the run's hold id/disposition: %q", out)
	}
	// The other run's hold must NOT appear — the endpoint is owner-wide, the command narrows.
	if strings.Contains(out, "hold-run2-gen1") {
		t.Errorf("run recovery leaked another run's hold: %q", out)
	}
}

// TestRunRecoveryJSON proves --json emits the run-filtered hold DTOs (never null), and only
// the requested run's holds.
func TestRunRecoveryJSON(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var holds []apitypes.RecoveryCustodyHoldDTO
	if err := json.Unmarshal([]byte(out), &holds); err != nil {
		t.Fatalf("run recovery --json is not a hold array: %v\n%s", err, out)
	}
	if len(holds) != 1 || holds[0].ID != "hold-run1-gen1" {
		t.Errorf("run recovery --json = %+v, want only run1's single hold", holds)
	}
}

// TestRunRecoveryJSONCheckpointRef (PRD #1810 M3): a hold whose server JSON carries the run's
// retained checkpoint location (checkpoint_ref/tip/state) round-trips through the DTO into
// `run recovery --json`, so an agent reads where the work lives on origin (the recovery ref once
// superseded). A hold without a record omits the keys.
func TestRunRecoveryJSONCheckpointRef(t *testing.T) {
	const serverJSON = `{"aggregate":{"open_holds":2,"custody_hold_limit":8,"decision_needed":1,"blocked_runs":0},
	  "holds":[
	    {"id":"hold-run1-gen1","run_id":"run1","generation":1,"state":"open","attention":"source_only",
	     "worker_id":"w1","has_available_capture":false,"created_at":"2026-09-27T10:00:00Z","updated_at":"2026-09-27T10:00:00Z",
	     "checkpoint_ref":"refs/uzi-recovery/run1","checkpoint_tip":"2222222222222222222222222222222222222222",
	     "checkpoint_state":"superseded"},
	    {"id":"hold-run1-gen2","run_id":"run1","generation":2,"state":"open","attention":"active",
	     "worker_id":"w1","has_available_capture":false,"created_at":"2026-09-27T10:00:00Z","updated_at":"2026-09-27T10:00:00Z"}
	  ]}`
	var dto apitypes.RecoveryCustodyHoldsDTO
	if err := json.Unmarshal([]byte(serverJSON), &dto); err != nil {
		t.Fatalf("decode server JSON: %v", err)
	}
	fc := &uzicli.FakeClient{RecoveryHoldsResult: dto}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var holds []map[string]any
	if err := json.Unmarshal([]byte(out), &holds); err != nil {
		t.Fatalf("run recovery --json is not a hold array: %v\n%s", err, out)
	}
	if len(holds) != 2 {
		t.Fatalf("run recovery --json = %d holds, want 2:\n%s", len(holds), out)
	}
	if holds[0]["checkpoint_ref"] != "refs/uzi-recovery/run1" ||
		holds[0]["checkpoint_tip"] != "2222222222222222222222222222222222222222" ||
		holds[0]["checkpoint_state"] != "superseded" {
		t.Errorf("hold 1 checkpoint location = %v/%v/%v, want the recovery ref, its tip and superseded",
			holds[0]["checkpoint_ref"], holds[0]["checkpoint_tip"], holds[0]["checkpoint_state"])
	}
	for _, k := range []string{"checkpoint_ref", "checkpoint_tip", "checkpoint_state"} {
		if _, ok := holds[1][k]; ok {
			t.Errorf("hold 2 has no retention record but --json carries %q", k)
		}
	}
}

// TestRunRecoveryRendersCheckpointRef (PRD #1810 M5): the human render names where a hold's
// retained checkpoint lives on origin, one line per hold carrying a checkpoint ref (ref, the
// 12-char tip and the retention state), and prints no such line for a hold without one.
func TestRunRecoveryRendersCheckpointRef(t *testing.T) {
	dto := recoveryHoldsFixture()
	dto.Holds = append(dto.Holds, apitypes.RecoveryCustodyHoldDTO{
		ID: "hold-run1-gen2", RunID: "run1", Generation: 2, State: "open", Attention: "active",
		WorkerID: "w1", WorkerName: "alpha",
		CheckpointRef:   "refs/uzi-recovery/run1",
		CheckpointTip:   "2222222222222222222222222222222222222222",
		CheckpointState: "superseded",
	})
	fc := &uzicli.FakeClient{RecoveryHoldsResult: dto}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	const want = "hold hold-run1-gen2 checkpoint: refs/uzi-recovery/run1 @ 222222222222 (superseded)\n"
	if !strings.Contains(out, want) {
		t.Errorf("run recovery output missing the checkpoint line %q:\n%s", want, out)
	}
	if n := strings.Count(out, "checkpoint:"); n != 1 {
		t.Errorf("run recovery printed %d checkpoint lines, want 1 (hold-run1-gen1 has no ref):\n%s", n, out)
	}
}

// TestRunRecoveryCheckpointLineSanitizes proves the server-supplied checkpoint fields cannot
// smuggle a newline or an escape sequence into the rendered line.
func TestRunRecoveryCheckpointLineSanitizes(t *testing.T) {
	got := checkpointLine(apitypes.RecoveryCustodyHoldDTO{
		ID:              "h1",
		CheckpointRef:   "refs/uzi-recovery/r1\nhold forged checkpoint: x",
		CheckpointTip:   "\x1b[31mabc",
		CheckpointState: "retained",
	})
	if strings.ContainsAny(got, "\n\x1b") {
		t.Errorf("checkpointLine leaked a control byte: %q", got)
	}
	if !strings.HasPrefix(got, "hold h1 checkpoint: refs/uzi-recovery/r1") || !strings.HasSuffix(got, "(retained)") {
		t.Errorf("checkpointLine = %q, want the sanitized ref and state", got)
	}
	if checkpointLine(apitypes.RecoveryCustodyHoldDTO{ID: "h2", CheckpointTip: "abc"}) != "" {
		t.Error("checkpointLine rendered a line for a hold with no checkpoint ref")
	}
}

// TestRunRecoveryEmptyJSON proves --json emits [] (never null) for a run with no holds.
func TestRunRecoveryEmptyJSON(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run-none", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(out) != "[]" {
		t.Errorf("run recovery --json for a run with no holds = %q, want []", strings.TrimSpace(out))
	}
}

// TestRunRecoveryJSONCaptures (issue #1417) proves --json attaches each hold's captures, in
// archive order, joined on hold_id: a capture reserved under another hold, or with no hold_id
// (an older server, kept as documentation of that shape), is attached nowhere, and the hold's own keys stay flat.
func TestRunRecoveryJSONCaptures(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: recoveryHoldsFixture(),
		RecoverySummaries: map[string]apitypes.RecoveryArchiveSummaryDTO{
			"run1": {Supported: true, Archives: []apitypes.RecoveryArchiveDTO{
				{ID: "cap-1", HoldID: "hold-run1-gen1", State: "available", SourceSha: "aaaa", ByteSize: i64(8),
					CreatedAt: time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC)},
				{ID: "cap-other", HoldID: "hold-run1-gen0", State: "expired", SourceSha: "bbbb"},
				{ID: "cap-2", HoldID: "hold-run1-gen1", State: "needs_action", SourceSha: "cccc"},
				{ID: "cap-legacy", State: "available", SourceSha: "dddd"},
			}},
		},
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, errb)
	}
	var holds []struct {
		ID        string `json:"id"`
		Attention string `json:"attention"`
		Captures  []struct {
			ID        string    `json:"id"`
			State     string    `json:"state"`
			SourceSha string    `json:"source_sha"`
			ByteSize  *int64    `json:"byte_size"`
			CreatedAt time.Time `json:"created_at"`
		} `json:"captures"`
	}
	if err := json.Unmarshal([]byte(out), &holds); err != nil {
		t.Fatalf("run recovery --json is not a hold array: %v\n%s", err, out)
	}
	if len(holds) != 1 || holds[0].ID != "hold-run1-gen1" || holds[0].Attention != "source_only" {
		t.Fatalf("run recovery --json = %+v, want run1's single hold with its flat keys", holds)
	}
	caps := holds[0].Captures
	if len(caps) != 2 || caps[0].ID != "cap-1" || caps[1].ID != "cap-2" {
		t.Fatalf("captures = %+v, want exactly cap-1 then cap-2", caps)
	}
	if caps[0].State != "available" || caps[0].SourceSha != "aaaa" || caps[0].ByteSize == nil || *caps[0].ByteSize != 8 {
		t.Errorf("cap-1 metadata wrong: %+v", caps[0])
	}
	if want := time.Date(2026, 9, 1, 12, 30, 0, 0, time.UTC); !caps[0].CreatedAt.Equal(want) {
		t.Errorf("cap-1 created_at = %v, want %v", caps[0].CreatedAt, want)
	}
	if caps[1].ByteSize != nil {
		t.Errorf("cap-2 has no byte size, want byte_size omitted; got %d", *caps[1].ByteSize)
	}
	// cap-legacy (no hold_id) is attached nowhere, so the omission must be said on stderr,
	// never silent; cap-other names a real (other) hold and is not counted.
	if !strings.Contains(errb, "1 capture(s) carry no hold id") || !strings.Contains(errb, "uzi run get run1") {
		t.Errorf("stderr = %q, want the unattributed-capture warning naming 1 capture and the run get listing", errb)
	}
}

// TestRunRecoveryJSONNoCaptures proves a hold with no captures carries "captures": [] (never
// null), so a consuming agent iterates it unconditionally.
func TestRunRecoveryJSONNoCaptures(t *testing.T) {
	fc := &uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"captures": []`) {
		t.Errorf("a capture-less hold should emit \"captures\": []; got:\n%s", out)
	}
}

// TestRunRecoveryJSONArchivesError proves a failed (non-404) archives read fails --json
// rather than emitting holds with silently empty captures.
func TestRunRecoveryJSONArchivesError(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: recoveryHoldsFixture(),
		RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitGeneric, "archives read failed"),
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code == uzicli.ExitOK {
		t.Fatalf("exit = 0 on an archives read error, want non-zero; stdout=%q", out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("a failed archives read must print no holds; stdout=%q", out)
	}
}

// TestRunRecoveryJSONDeletedRunDegrades proves a 404 archives read (a released/discarded hold
// that outlived its deleted run) degrades to every hold listing "captures": [] instead of
// failing the listing the holds endpoint still serves.
func TestRunRecoveryJSONDeletedRunDegrades(t *testing.T) {
	fc := &uzicli.FakeClient{
		RecoveryHoldsResult: recoveryHoldsFixture(),
		RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitNotFound, "run not found"),
	}
	out, errb, code := runCLI(t, fakeEnv(fc), "run", "recovery", "run1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 on a 404 archives read (stderr: %s)", code, errb)
	}
	var holds []struct {
		ID       string            `json:"id"`
		Captures []json.RawMessage `json:"captures"`
	}
	if err := json.Unmarshal([]byte(out), &holds); err != nil {
		t.Fatalf("run recovery --json is not a hold array: %v\n%s", err, out)
	}
	if len(holds) != 1 || holds[0].ID != "hold-run1-gen1" {
		t.Fatalf("run recovery --json = %+v, want run1's single hold", holds)
	}
	if holds[0].Captures == nil || len(holds[0].Captures) != 0 || !strings.Contains(out, `"captures": []`) {
		t.Errorf("a deleted run's hold should list \"captures\": []; got:\n%s", out)
	}
}

// TestRunRecoveryNoArchivesReadWithoutNeed proves the archives are read only when they are
// joined: the human table and a --json run with no holds both succeed with the read failing.
func TestRunRecoveryNoArchivesReadWithoutNeed(t *testing.T) {
	for _, args := range [][]string{
		{"run", "recovery", "run1"},
		{"run", "recovery", "run-none", "--json"},
	} {
		fc := &uzicli.FakeClient{
			RecoveryHoldsResult: recoveryHoldsFixture(),
			RecoveryArchivesErr: uzicli.Exitf(uzicli.ExitGeneric, "archives read failed"),
		}
		if _, errb, code := runCLI(t, fakeEnv(fc), args...); code != uzicli.ExitOK {
			t.Errorf("%v: exit = %d, want 0 (no archives read needed); stderr=%q", args, code, errb)
		}
	}
}

// TestRunDiscardRequiresHold proves `uzi run discard <run-id>` with no --hold is a usage error
// that mutates nothing.
func TestRunDiscardRequiresHold(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "discard", "run1", "--yes")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Errorf("missing --hold must not attempt a discard; got %+v", fc.DiscardHoldCalls)
	}
}

// TestRunDiscardYesMapsToDiscard proves --yes discards exactly the named (run, hold) with no
// prompt — the unattended path.
func TestRunDiscardYesMapsToDiscard(t *testing.T) {
	fc := &uzicli.FakeClient{}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "discard", "run1", "--hold", "hold-x", "--yes")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(fc.DiscardHoldCalls) != 1 || fc.DiscardHoldCalls[0] != (uzicli.DiscardHoldCall{RunID: "run1", HoldID: "hold-x"}) {
		t.Fatalf("discard calls = %+v, want exactly one (run1, hold-x)", fc.DiscardHoldCalls)
	}
}

// TestRunDiscardNonTTYRefusesWithoutYes proves that without --yes and without a TTY the command
// HARD-REFUSES (usage error) and mutates NOTHING — a possible only copy is never destroyed
// unattended (D9).
func TestRunDiscardNonTTYRefusesWithoutYes(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = false // no terminal
	_, errb, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage refusal)", code, uzicli.ExitUsage)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Errorf("a non-TTY refusal must mutate nothing; got %+v", fc.DiscardHoldCalls)
	}
	if !strings.Contains(errb, "--yes") {
		t.Errorf("non-TTY refusal should tell the user to pass --yes; stderr=%q", errb)
	}
}

// TestRunDiscardConfirmCancelled proves that a declined interactive prompt (a bare "n") performs
// NO mutation and exits 0.
func TestRunDiscardConfirmCancelled(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("n\n")
	out, errb, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 on a declined prompt\nstderr=%q", code, errb)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Fatalf("a cancelled prompt must perform NO mutation; got %+v", fc.DiscardHoldCalls)
	}
	if !strings.Contains(errb, "aborted") {
		t.Errorf("a declined discard should say it aborted; stderr=%q stdout=%q", errb, out)
	}
}

// TestRunDiscardWarnsCheckpointRefDeletion proves both the interactive prompt and the help
// text say that discarding a run's last open hold also deletes its retained checkpoint ref on
// the forge (PRD #1810: retention follows custody).
func TestRunDiscardWarnsCheckpointRefDeletion(t *testing.T) {
	const want = "last open hold also deletes its retained checkpoint ref on the forge"
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("n\n")
	_, errb, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 on a declined prompt\nstderr=%q", code, errb)
	}
	if !strings.Contains(errb, "[y/N]") || !strings.Contains(errb, want) {
		t.Errorf("prompt should warn about the checkpoint ref; stderr=%q", errb)
	}

	out, errb, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "discard", "--help")
	if code != uzicli.ExitOK {
		t.Fatalf("help exit = %d, want 0\nstderr=%q", code, errb)
	}
	if !strings.Contains(out, "Discard ONE exact custody hold") || !strings.Contains(out, want) {
		t.Errorf("help should warn about the checkpoint ref; stdout=%q", out)
	}
}

// TestRunDiscardConfirmAccepted proves that an accepted interactive prompt ("y") discards the
// exact hold.
func TestRunDiscardConfirmAccepted(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("y\n")
	_, _, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(fc.DiscardHoldCalls) != 1 || fc.DiscardHoldCalls[0].HoldID != "hold-x" {
		t.Fatalf("accepted prompt should discard hold-x once; got %+v", fc.DiscardHoldCalls)
	}
}

// TestRunDiscardConfirmEOFDeclines proves that EOF on stdin (an empty piped confirmation under a
// TTY-claimed env) declines rather than proceeds — the safe default for a destructive action.
func TestRunDiscardConfirmEOFDeclines(t *testing.T) {
	fc := &uzicli.FakeClient{}
	env := fakeEnv(fc)
	env.StdinTTY = true
	env.Stdin = strings.NewReader("") // immediate EOF
	_, _, code := runCLI(t, env, "run", "discard", "run1", "--hold", "hold-x")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(fc.DiscardHoldCalls) != 0 {
		t.Fatalf("EOF must decline and mutate nothing; got %+v", fc.DiscardHoldCalls)
	}
}

// TestOwnerRecoveryHintsSplitByArchive proves the owner listing offers `run export` only for
// the archive_ready hold, discard for the source_only and needs_action holds (neither has an
// archive, as the server never pairs those dispositions with one), and explains the
// source_only hold's missing archive with a custody line.
func TestOwnerRecoveryHintsSplitByArchive(t *testing.T) {
	dto := ownerRecoveryFixture()
	dto.Holds = append(dto.Holds, apitypes.RecoveryCustodyHoldDTO{
		ID: "hold-c", RunID: "run-c", Generation: 1, State: "open", Attention: "archive_ready",
		WorkerID: "worker-c", HasAvailableCapture: true, CreatedAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)})
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"\nrun run-a hold hold-a: no recovery archive; custody of worker worker-a's local source is retained (export unavailable; it may be the only copy)",
		"1 hold(s) have a recovery archive: recover with `uzi run export`",
		"2 hold(s) await a decision: discard with `uzi run discard <run-id> --hold <hold-id> --yes`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "or discard with") || strings.Count(out, "custody of worker") != 1 {
		t.Errorf("stale hint or custody line for a non-source_only hold:\n%s", out)
	}
}

// TestOwnerRecoveryHintsDefensiveDoubleCount deliberately pins a state the server never
// emits (needs_action WITH an available archive): the CLI counts such a hold in both hints
// rather than hiding either, so a server change surfaces instead of silently dropping one.
func TestOwnerRecoveryHintsDefensiveDoubleCount(t *testing.T) {
	dto := apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{
		{ID: "hold-x", RunID: "run-x", Generation: 1, State: "open", Attention: "needs_action",
			WorkerID: "w", HasAvailableCapture: true, CreatedAt: time.Now().Add(-time.Hour)}}}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "1 hold(s) have a recovery archive") || !strings.Contains(out, "1 hold(s) await a decision") {
		t.Errorf("defensive double count changed:\n%s", out)
	}
}

// TestOwnerRecoveryFoldsRunIDInCustodyPrefix proves a newline in the run id cannot forge a
// line through the owner view's `run %s` custody prefix.
func TestOwnerRecoveryFoldsRunIDInCustodyPrefix(t *testing.T) {
	dto := apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{
		{ID: "h1", RunID: "run1\nforged", Generation: 1, State: "open", Attention: "source_only",
			WorkerName: "alpha", CreatedAt: time.Now().Add(-time.Hour)}}}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "\nforged") || !strings.Contains(out, "run run1") {
		t.Errorf("run id newline not folded in custody prefix:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "forged") {
			t.Errorf("forged line: %q", l)
		}
	}
}

// TestRecoveryHelpTiesExportToArchiveReady pins the help text's disposition guidance: export
// belongs to archive_ready holds; source_only and needs_action holds await a discard decision.
func TestRecoveryHelpTiesExportToArchiveReady(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "recovery", "--help")
	if code != uzicli.ExitOK {
		t.Fatalf("help exit = %d", code)
	}
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range []string{
		"An `archive_ready` hold has a recovery archive: recover it with `run export`",
		"A `source_only` or `needs_action` hold has no archive and awaits your decision to discard it",
		"`source_only` means no archive exists and custody of the worker's local source is retained",
		"it may be the only copy",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(flat, "recover the archive with `run export` when one is available") {
		t.Errorf("stale export guidance in help:\n%s", out)
	}
}

func TestOwnerRecoveryQuietSuppressesGuidance(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: ownerRecoveryFixture()}), "--quiet", "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "custody of worker") || strings.Contains(out, "await a decision") ||
		strings.Contains(out, "recovery archive") {
		t.Errorf("--quiet printed guidance:\n%s", out)
	}
}

func TestRunRecoverySourceOnlyNoExportSuggestion(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"hold hold-run1-gen1: no recovery archive; custody of worker alpha's local source is retained (export unavailable; it may be the only copy)",
		"1 hold(s) await a decision: discard with `uzi run discard run1 --hold <hold-id> --yes`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "uzi run export") || strings.Contains(out, "have a recovery archive") {
		t.Errorf("export suggested for a hold with no archive:\n%s", out)
	}
}

func TestRunRecoveryArchiveReadyGetsExportHint(t *testing.T) {
	dto := recoveryHoldsFixture()
	dto.Holds = []apitypes.RecoveryCustodyHoldDTO{
		{ID: "hold-r", RunID: "run1", Generation: 1, State: "open", Attention: "archive_ready",
			WorkerID: "w1", WorkerName: "alpha", HasAvailableCapture: true, CaptureState: "available"},
	}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "1 hold(s) have a recovery archive: recover with `uzi run export`") {
		t.Errorf("export hint absent:\n%s", out)
	}
	if strings.Contains(out, "await a decision") || strings.Contains(out, "custody of worker") {
		t.Errorf("archive_ready hold got decision/custody guidance:\n%s", out)
	}
}

func TestRunRecoveryNeedsActionGetsDiscard(t *testing.T) {
	dto := recoveryHoldsFixture()
	dto.Holds = []apitypes.RecoveryCustodyHoldDTO{
		{ID: "hold-n", RunID: "run1", Generation: 1, State: "open", Attention: "needs_action", WorkerID: "w1"},
	}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(out, "1 hold(s) await a decision: discard with `uzi run discard run1 --hold <hold-id> --yes`") ||
		strings.Contains(out, "uzi run export") || strings.Contains(out, "custody of worker") {
		t.Errorf("needs_action guidance wrong:\n%s", out)
	}
}

func TestRunRecoveryQuietSuppressesGuidance(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: recoveryHoldsFixture()}), "--quiet", "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(out, "custody of worker") || strings.Contains(out, "await a decision") {
		t.Errorf("--quiet printed guidance:\n%s", out)
	}
}

// TestSourceOnlyLineSanitizes proves a hostile hold id / worker name cannot forge lines or
// inject escapes through the custody line, in either renderer.
func TestSourceOnlyLineSanitizes(t *testing.T) {
	h := apitypes.RecoveryCustodyHoldDTO{
		ID: "h1\nforged line", RunID: "run1\x1b[31m", Generation: 1, State: "open", Attention: "source_only",
		WorkerName: "evil\r\x1b]0;x\x07name\nforged", CreatedAt: time.Now().Add(-time.Hour),
	}
	if got := sourceOnlyLine(h); strings.ContainsAny(got, "\n\r\x1b\x07") {
		t.Errorf("sourceOnlyLine leaked control bytes: %q", got)
	}
	dto := apitypes.RecoveryCustodyHoldsDTO{Holds: []apitypes.RecoveryCustodyHoldDTO{h}}
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: dto}), "run", "recovery")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.ContainsAny(out, "\x1b\r\x07") || strings.Contains(out, "\nforged") {
		t.Errorf("owner listing leaked hostile bytes: %q", out)
	}
	h.RunID = "run1"
	out, _, code = runCLI(t, fakeEnv(&uzicli.FakeClient{RecoveryHoldsResult: apitypes.RecoveryCustodyHoldsDTO{
		Holds: []apitypes.RecoveryCustodyHoldDTO{h}}}), "run", "recovery", "run1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if strings.ContainsAny(out, "\x1b\r\x07") || strings.Contains(out, "\nforged") {
		t.Errorf("run view leaked hostile bytes: %q", out)
	}
}
