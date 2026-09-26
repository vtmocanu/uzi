package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// t0 is a fixed "now" so every age is deterministic.
var t0 = time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

// healthFakeStore is a minimal Store for the detector: the three health reads/writes
// plus the queued-run worker count. Embedding Store means any other method a stray
// path reaches panics, keeping the tests honest about what the detector touches.
type healthFakeStore struct {
	Store
	active []store.ListActiveRunsForHealthRow
	window map[uuid.UUID][]store.ListRunToolWindowRow
	// messages is an optional per-run run_messages log (seq ascending or not; the
	// fake orders it). When a run has one, BOTH tool-window reads derive from it with
	// the real queries' filter + ORDER BY seq DESC + LIMIT semantics, so a test can
	// model nested subagent rows (agentInstance set) crowding the mixed window (issue
	// #1394). When unset, ListRunToolWindow returns window verbatim and
	// ListRunLeadToolWindow falls back to window's rows as lead-lane rows.
	messages      map[uuid.UUID][]fakeRunMessage
	onlineWorkers int64
	// freeSlotWorkers is the canned CountOnlineWorkersWithFreeSlotForUser answer (PRD
	// #216): how many online workers still have room. 0 with onlineWorkers>0 is the
	// saturated fleet that drives reasonAllWorkersBusy.
	freeSlotWorkers int64
	// eligibleWorkers is the canned CountOnlineEligibleWorkersForRepo answer (PRD #361):
	// how many online workers fn_worker_can_claim accepts for the run's repo/kind. 0 with
	// onlineWorkers>0 and a non-allowlisted repo drives reasonRepoNotDockerAllowed.
	eligibleWorkers int64
	eligibleErr     error
	// eligCalls records every CountOnlineEligibleWorkersForRepo lookup's params, mirroring
	// capsCalls: it lets a test prove rung 5 threads RequiredCapabilities and CapabilityAware
	// (issue #512 M2) so its claim-time count agrees with the claim path.
	eligCalls []store.CountOnlineEligibleWorkersForRepoParams
	writes    []store.SetRunHealthParams
	// leftStatus marks run ids whose SetRunHealth returns 0 rows — the exit race,
	// where the run changed status between the list read and the health write.
	leftStatus map[uuid.UUID]bool
	// windowCalls counts ListRunToolWindow fetches per run. PRD #108 M4's arm
	// returns above that query, so this is how a test proves the wedged case stops
	// issuing one per tick — an assertion the returned flag alone cannot make.
	windowCalls map[uuid.UUID]int
	// verdictSince is the canned RunHasVerdictSinceGateOpened answer per run id
	// (issue #182: "the owner already answered THIS gate"), and verdictErr forces the
	// read to fail. The predicate itself is NOT reimplemented here on purpose — a
	// per-kind assertion against a fake that re-derives the answer would only test the
	// fake. The kind list and the >= boundary are pinned against a real Postgres by
	// store.TestRunHasVerdictSinceGateOpenedLiveDB; this side pins the ARM.
	verdictSince map[uuid.UUID]bool
	verdictErr   error
	// verdictCalls records every lookup's params, which is how a test proves the arm
	// asks about THIS gate (GateOpenedAt == the run's updated_at) and that the three
	// guards ahead of it short-circuit before any query is issued at all.
	verdictCalls []store.RunHasVerdictSinceGateOpenedParams
	// priorityClass is the canned RunPriorityClassForRun answer per run id (PRD #320
	// D9: normal|background|expedited|restored), and priorityErr forces the read to
	// fail. The demotion predicate is NOT reimplemented here on purpose — the same
	// reasoning as verdictSince above: a fake that re-derives the class would only test
	// the fake. fn_run_priority_class itself is pinned against a real Postgres by the
	// store package's M1 tests; this side pins the ARM (class → reason mapping).
	priorityClass map[uuid.UUID]string
	priorityErr   error
	// priorityCalls records every lookup's params, so a test can prove the arm builds
	// the cutoff from WorkerBackgroundGrace and that the queued-threshold guard
	// short-circuits ahead of it (no query for a freshly-queued run).
	priorityCalls []store.RunPriorityClassForRunParams
	// satisfyingCaps is the canned CountOnlineWorkersSatisfyingCaps answer (PRD #84 M3):
	// how many online, non-draining workers have effective caps covering the run's
	// required set. 0 with a non-empty requirement is the unplaceable run that drives
	// reasonNoEligibleWorker. satisfyingCapsErr forces the read to fail (falls through to
	// the generic queuedReason). The subset predicate itself is NOT reimplemented here —
	// it is pinned against a real Postgres by store.TestCountOnlineWorkersSatisfyingCaps*
	// LiveDB; this side pins the ARM (flag/count → reason mapping).
	satisfyingCaps    int64
	satisfyingCapsErr error
	// capsCalls records every lookup's params, so a test can prove the arm asks about
	// THIS run's user and required set, and that the guards ahead of it short-circuit.
	capsCalls []store.CountOnlineWorkersSatisfyingCapsParams
	// satisfyingProtocol is the canned CountOnlineWorkersSatisfyingProtocol answer (PRD #1226
	// M1): how many online, non-draining workers self-report the completion protocol. 0 for an
	// INTERLOCKED run drives reasonNoCompletionCapableWorker. satisfyingProtocolErr forces the
	// read to fail (falls through to the generic queuedReason). The ANY(protocol_capabilities)
	// predicate itself is NOT reimplemented here — it is pinned against a real Postgres by the
	// queuedReason completion-capability LiveDB tests; this side pins the ARM.
	satisfyingProtocol    int64
	satisfyingProtocolErr error
	// protocolCalls records every lookup's user id, so a test can prove the rung asks about THIS
	// run's user and that the guards ahead of it (and the non-interlocked case) short-circuit.
	protocolCalls []uuid.UUID
	// custodyHolds is the canned GetCustodyAdmissionForRun open_holds answer (PRD #1296 M4): the
	// owner's OPEN custody-hold count. >= custodyHoldLimit drives reasonCustodyLimit unless
	// custodyExempt (issue #1751 continuation exemption) is set.
	// custodyErr forces the read to fail (falls through to the generic queuedReason). The count
	// predicate itself is pinned against a real Postgres by the store package; this side pins the
	// ARM (count vs limit → reason mapping).
	custodyHolds  int64
	custodyExempt bool
	custodyErr    error
	// custodyCalls records every lookup's user id, so a test can prove the rung asks about THIS
	// run's owner (the SAME predicate the claim gates on) and that vault-lock short-circuits it.
	custodyCalls []uuid.UUID
	// custodyRunCalls records every lookup's run id (issue #1751: the exemption is per run).
	custodyRunCalls []uuid.UUID
}

func (f *healthFakeStore) ListActiveRunsForHealth(context.Context, []string) ([]store.ListActiveRunsForHealthRow, error) {
	return f.active, nil
}
func (f *healthFakeStore) ListRunToolWindow(_ context.Context, arg store.ListRunToolWindowParams) ([]store.ListRunToolWindowRow, error) {
	if f.windowCalls == nil {
		f.windowCalls = map[uuid.UUID]int{}
	}
	f.windowCalls[arg.RunID]++
	msgs, ok := f.messages[arg.RunID]
	if !ok {
		return f.window[arg.RunID], nil
	}
	var out []store.ListRunToolWindowRow
	for _, m := range newestFirst(msgs) {
		if m.kind != "tool_use" && m.kind != "tool_result" {
			continue
		}
		if int32(len(out)) >= arg.Lim { //nolint:gosec // G115: test fixture sizes are tiny
			break
		}
		out = append(out, store.ListRunToolWindowRow{Seq: m.seq, Kind: m.kind, Payload: m.payload})
	}
	return out, nil
}

// fakeRunMessage is one run_messages row for healthFakeStore.messages. agentInstance
// "" models SQL NULL (the lead lane); a non-empty value is a nested subagent frame.
type fakeRunMessage struct {
	seq           int32
	kind          string
	agentInstance string
	payload       []byte
}

// leadWindowKind mirrors ListRunLeadToolWindow's kind/event filter.
func (m fakeRunMessage) leadWindowKind() bool {
	switch m.kind {
	case "tool_use", "tool_result":
		return true
	case "status", "error":
		var p struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(m.payload, &p); err != nil {
			return false
		}
		return p.Event == "init" || p.Event == "result"
	}
	return false
}

// newestFirst returns a copy of msgs ordered seq DESC, the queries' ORDER BY.
func newestFirst(msgs []fakeRunMessage) []fakeRunMessage {
	out := append([]fakeRunMessage(nil), msgs...)
	sort.Slice(out, func(i, j int) bool { return out[i].seq > out[j].seq })
	return out
}
func (f *healthFakeStore) ListRunLeadToolWindow(_ context.Context, arg store.ListRunLeadToolWindowParams) ([]store.ListRunLeadToolWindowRow, error) {
	msgs, ok := f.messages[arg.RunID]
	if !ok {
		var out []store.ListRunLeadToolWindowRow
		for _, r := range f.window[arg.RunID] {
			out = append(out, store.ListRunLeadToolWindowRow(r))
		}
		return out, nil
	}
	var out []store.ListRunLeadToolWindowRow
	for _, m := range newestFirst(msgs) {
		if m.agentInstance != "" || !m.leadWindowKind() {
			continue
		}
		if int32(len(out)) >= arg.Lim { //nolint:gosec // G115: test fixture sizes are tiny
			break
		}
		out = append(out, store.ListRunLeadToolWindowRow{Seq: m.seq, Kind: m.kind, Payload: m.payload})
	}
	return out, nil
}
func (f *healthFakeStore) GetCustodyAdmissionForRun(_ context.Context, arg store.GetCustodyAdmissionForRunParams) (store.GetCustodyAdmissionForRunRow, error) {
	f.custodyCalls = append(f.custodyCalls, arg.UserID)
	f.custodyRunCalls = append(f.custodyRunCalls, arg.RunID)
	if f.custodyErr != nil {
		return store.GetCustodyAdmissionForRunRow{}, f.custodyErr
	}
	return store.GetCustodyAdmissionForRunRow{OpenHolds: f.custodyHolds, ContinuationExempt: f.custodyExempt}, nil
}
func (f *healthFakeStore) CountOnlineWorkersForUser(context.Context, uuid.UUID) (int64, error) {
	return f.onlineWorkers, nil
}
func (f *healthFakeStore) CountOnlineWorkersWithFreeSlotForUser(context.Context, uuid.UUID) (int64, error) {
	return f.freeSlotWorkers, nil
}
func (f *healthFakeStore) CountOnlineWorkersSatisfyingCaps(_ context.Context, arg store.CountOnlineWorkersSatisfyingCapsParams) (int64, error) {
	f.capsCalls = append(f.capsCalls, arg)
	if f.satisfyingCapsErr != nil {
		return 0, f.satisfyingCapsErr
	}
	return f.satisfyingCaps, nil
}
func (f *healthFakeStore) CountOnlineWorkersSatisfyingProtocol(_ context.Context, userID uuid.UUID) (int64, error) {
	f.protocolCalls = append(f.protocolCalls, userID)
	if f.satisfyingProtocolErr != nil {
		return 0, f.satisfyingProtocolErr
	}
	return f.satisfyingProtocol, nil
}
func (f *healthFakeStore) CountOnlineEligibleWorkersForRepo(_ context.Context, arg store.CountOnlineEligibleWorkersForRepoParams) (int64, error) {
	f.eligCalls = append(f.eligCalls, arg)
	return f.eligibleWorkers, f.eligibleErr
}
func (f *healthFakeStore) RunHasVerdictSinceGateOpened(_ context.Context, arg store.RunHasVerdictSinceGateOpenedParams) (bool, error) {
	f.verdictCalls = append(f.verdictCalls, arg)
	if f.verdictErr != nil {
		return false, f.verdictErr
	}
	return f.verdictSince[arg.RunID], nil
}
func (f *healthFakeStore) RunPriorityClassForRun(_ context.Context, arg store.RunPriorityClassForRunParams) (string, error) {
	f.priorityCalls = append(f.priorityCalls, arg)
	if f.priorityErr != nil {
		return "", f.priorityErr
	}
	return f.priorityClass[arg.RunID], nil
}
func (f *healthFakeStore) SetRunHealth(_ context.Context, arg store.SetRunHealthParams) (int64, error) {
	f.writes = append(f.writes, arg)
	if f.leftStatus[arg.ID] {
		return 0, nil
	}
	return 1, nil
}

// fakeHealthSettings is a static health-settings source. All accessors are error-free;
// the zero value has the detector disabled, so tests opt in explicitly.
type fakeHealthSettings struct {
	enabled                                           bool
	stall, nearTimeoutPct, queued, approval, cooldown int
	// PRD #1189: the per-run extension cap the `extend` SubmitInput branch reads. Zero (the
	// default) means extending is disabled, so a test opts in with a non-zero cap.
	runExtensionCap int
}

func (s fakeHealthSettings) HealthEnabled(context.Context) (bool, error)     { return s.enabled, nil }
func (s fakeHealthSettings) HealthStallSeconds(context.Context) (int, error) { return s.stall, nil }
func (s fakeHealthSettings) HealthNearTimeoutPct(context.Context) (int, error) {
	return s.nearTimeoutPct, nil
}
func (s fakeHealthSettings) HealthQueuedSeconds(context.Context) (int, error) { return s.queued, nil }
func (s fakeHealthSettings) HealthApprovalSeconds(context.Context) (int, error) {
	return s.approval, nil
}
func (s fakeHealthSettings) HealthNudgeCooldownSeconds(context.Context) (int, error) {
	return s.cooldown, nil
}
func (s fakeHealthSettings) RunExtensionCapSeconds(context.Context) (int, error) {
	return s.runExtensionCap, nil
}

// fakeAllowlistReader is a static DockerAllowlistReader (PRD #361): ids is the canned
// docker-worker repo allowlist, err forces the read to fail so the arm's degrade path
// can be exercised.
type fakeAllowlistReader struct {
	ids []uuid.UUID
	err error
}

func (f fakeAllowlistReader) DockerRepoAllowlist(context.Context) ([]uuid.UUID, error) {
	return f.ids, f.err
}

// Sentinel errors for the queued-reason degrade-path cases (PRD #361): an allowlist
// read failure and an eligible-count failure must each fall through to the generic
// free-slot logic, never a spurious docker reason.
var (
	errFakeAllowlist = errors.New("fake allowlist read error")
	errFakeEligible  = errors.New("fake eligible-count error")
)

// defaultHealthSettings mirrors the compiled-in defaults (stall 5m / near-timeout 85% /
// queued 10m / approval 1h / cooldown 30m).
func defaultHealthSettings() fakeHealthSettings {
	return fakeHealthSettings{enabled: true, stall: 300, nearTimeoutPct: 85, queued: 600, approval: 3600, cooldown: 1800}
}

func healthSvc(fs Store, st Settings) *Service {
	svc := New(fs, nil, testParams())
	svc.healthSettings = st
	return svc
}

func ago(d time.Duration) pgtype.Timestamptz { return pgconv.Time(t0.Add(-d)) }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func useMsg(t *testing.T, seq int32, id, name string, input any) store.ListRunToolWindowRow {
	return store.ListRunToolWindowRow{Seq: seq, Kind: "tool_use", Payload: mustJSON(t, map[string]any{"id": id, "name": name, "input": input})}
}
func resultMsg(t *testing.T, seq int32, useID string) store.ListRunToolWindowRow {
	return store.ListRunToolWindowRow{Seq: seq, Kind: "tool_result", Payload: mustJSON(t, map[string]any{"tool_use_id": useID})}
}

// lastWrite returns the SetRunHealth params written for a run id, or fails.
func lastWrite(t *testing.T, fs *healthFakeStore, id uuid.UUID) store.SetRunHealthParams {
	t.Helper()
	for i := len(fs.writes) - 1; i >= 0; i-- {
		if fs.writes[i].ID == id {
			return fs.writes[i]
		}
	}
	t.Fatalf("no health write for run %s", id)
	return store.SetRunHealthParams{}
}

func runRow(status string) store.ListActiveRunsForHealthRow {
	return store.ListActiveRunsForHealthRow{ID: uuid.New(), UserID: uuid.New(), Status: status, Health: healthOK}
}

// -------------------------------------------------------------------------

func TestHealthDisabledIsNoop(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(2 * time.Hour)
	r.LastActivityAt = ago(time.Hour)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, fakeHealthSettings{enabled: false, stall: 300})

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 when disabled", n)
	}
	if len(fs.writes) != 0 {
		t.Fatalf("wrote %d health rows, want 0 when disabled", len(fs.writes))
	}
}

func TestHealthStalledFlagsThenClearsOnResume(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)      // well under the near-timeout threshold, so it never masks the clear
	r.LastActivityAt = ago(10 * time.Minute) // silent 10m > 5m stall
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	// First pass: flag stalled.
	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled", w.Health)
	}
	if !w.HealthReason.Valid || w.HealthReason.String != reasonStalled {
		t.Fatalf("reason = %+v, want %q", w.HealthReason, reasonStalled)
	}
	if !w.HealthSince.Valid {
		t.Fatal("health_since not stamped on flag")
	}
	if w.Status != "running" {
		t.Fatalf("write not status-scoped: status = %q", w.Status)
	}

	// Resume: activity is fresh and the row now reads 'stalled'. The detector must
	// self-clear back to ok.
	fs.writes = nil
	r.Health = healthStalled
	r.HealthReason = pgconv.TextOrNull(reasonStalled)
	r.LastActivityAt = ago(30 * time.Second) // fresh
	fs.active = []store.ListActiveRunsForHealthRow{r}

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("clear pass changed = %d, want 1", n)
	}
	w = lastWrite(t, fs, r.ID)
	if w.Health != healthOK || w.HealthReason.Valid || w.HealthSince.Valid {
		t.Fatalf("self-clear wrote %+v, want ok/NULL/NULL", w)
	}
}

func TestHealthStalledSuppressedWhileToolInFlight(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(time.Hour)
	r.LastActivityAt = ago(20 * time.Minute) // long silent, would be stalled
	fs := &healthFakeStore{
		active: []store.ListActiveRunsForHealthRow{r},
		// Newest message is a tool_use with no matching tool_result → in flight.
		window: map[uuid.UUID][]store.ListRunToolWindowRow{
			r.ID: {useMsg(t, 10, "call_A", "Bash", map[string]any{"command": "go build ./..."})},
		},
	}
	svc := healthSvc(fs, fakeHealthSettings{enabled: true, stall: 300}) // near-timeout disabled (pct 0)

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 (stalled suppressed while a tool is in flight)", n)
	}

	// Once the result lands, the same silence is genuinely stalled.
	fs.writes = nil
	fs.window[r.ID] = []store.ListRunToolWindowRow{
		resultMsg(t, 11, "call_A"),
		useMsg(t, 10, "call_A", "Bash", map[string]any{"command": "go build ./..."}),
	}
	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 once the tool call completed", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled after result landed", w.Health)
	}
}

// frozenBudget stamps a run's effective wall-clock budget in seconds (PRD #1170); a
// zero/omitted value leaves the run on the global RUN_TIMEOUT (NULL budget).
func frozenBudget(hours int) pgtype.Int4 {
	return pgtype.Int4{Int32: int32(hours) * 60 * 60, Valid: true} //nolint:gosec // G115: hours is a small test-fixture value, always well within int32
}

// TestHealthNearTimeoutFlagsWithRecentActivity: a run 7h into a frozen 8h budget (87.5%,
// past the 85% default) is flagged near timeout even though it is actively emitting —
// the arm is budget-relative, not activity-based (PRD #1170). Reason is the static
// near-timeout string; the live countdown rides deadline_at on the DTO.
func TestHealthNearTimeoutFlagsWithRecentActivity(t *testing.T) {
	r := runRow("running")
	r.BudgetWallSeconds = frozenBudget(8)
	r.StartedAt = ago(7 * time.Hour)        // 87.5% of the 8h budget
	r.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthSlow {
		t.Fatalf("health = %q, want the raw slow enum (kept on the wire, D1)", w.Health)
	}
	if !w.HealthReason.Valid || w.HealthReason.String != reasonNearTimeout {
		t.Fatalf("reason = %+v, want %q", w.HealthReason, reasonNearTimeout)
	}
}

func TestHealthStalledBeatsNearTimeout(t *testing.T) {
	r := runRow("running")
	r.BudgetWallSeconds = frozenBudget(8)
	r.StartedAt = ago(7 * time.Hour)         // near timeout (87.5%)
	r.LastActivityAt = ago(10 * time.Minute) // also stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs, r.ID); w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled (priority over near-timeout)", w.Health)
	}
}

// TestHealthNearTimeoutFrozenBudget pins the 85% boundary against a frozen 8h budget:
// 7h active (87.5%) flags, 6h active (75%) does not.
func TestHealthNearTimeoutFrozenBudget(t *testing.T) {
	cases := []struct {
		name      string
		activeHrs int
		wantFlag  bool
	}{
		{"7h of 8h (87.5%) flags", 7, true},
		{"6h of 8h (75%) does not", 6, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("running")
			r.BudgetWallSeconds = frozenBudget(8)
			r.StartedAt = ago(time.Duration(tc.activeHrs) * time.Hour)
			r.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc := healthSvc(fs, defaultHealthSettings())

			n := svc.detectRunHealth(context.Background(), t0)
			if tc.wantFlag {
				if n != 1 {
					t.Fatalf("changed = %d, want 1 (%s)", n, tc.name)
				}
				if w := lastWrite(t, fs, r.ID); w.Health != healthSlow {
					t.Fatalf("health = %q, want slow", w.Health)
				}
			} else if n != 0 {
				w := lastWrite(t, fs, r.ID)
				t.Fatalf("changed = %d, want 0 (%s); wrote %q", n, tc.name, w.Health)
			}
		})
	}
}

// TestHealthNearTimeoutMovesWithExtension: the near-timeout arm measures against the
// EXTENDED budget (PRD #1189 M1). A frozen 8h budget with a 2h extension has an effective
// 10h wall, so 85% is 8h30m: a run 7h active (70% of 10h) does NOT flag, and the same run
// 8h30m active (exactly 85%) DOES. Without the extension a 7h-of-8h run would already flag
// at 87.5%, so this pins that runWallClock folds budget_extension_seconds into effTimeout.
// Mutation check: dropping `effTimeout += budget_extension_seconds` reddens both rows (the
// 7h case flags spuriously and the 8h30m case would flag against the wrong 8h base).
func TestHealthNearTimeoutMovesWithExtension(t *testing.T) {
	cases := []struct {
		name     string
		active   time.Duration
		wantFlag bool
	}{
		{"7h of 8h+2h (70%) does not flag", 7 * time.Hour, false},
		{"8h30m of 8h+2h (85%) flags", 8*time.Hour + 30*time.Minute, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("running")
			r.BudgetWallSeconds = frozenBudget(8)         // 28800s frozen budget
			r.BudgetExtensionSeconds = int32(2 * 60 * 60) // +7200s human-granted extension
			r.StartedAt = ago(tc.active)                  // active time since start
			r.LastActivityAt = ago(1 * time.Minute)       // recent → not stalled
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc := healthSvc(fs, defaultHealthSettings())

			n := svc.detectRunHealth(context.Background(), t0)
			if tc.wantFlag {
				if n != 1 {
					t.Fatalf("changed = %d, want 1 (%s)", n, tc.name)
				}
				if w := lastWrite(t, fs, r.ID); w.Health != healthSlow {
					t.Fatalf("health = %q, want slow", w.Health)
				}
			} else if n != 0 {
				w := lastWrite(t, fs, r.ID)
				t.Fatalf("changed = %d, want 0 (%s); wrote %q", n, tc.name, w.Health)
			}
		})
	}
}

// TestHealthNearTimeoutSubtractsPausedSeconds: active running time excludes seconds
// parked at a gate. A 7h-old run with a 2h gate pause has consumed 5h of its 8h budget
// (62.5%), below the 85% threshold, so it is NOT flagged. Mutation check: folding the
// `- budget_paused_seconds` term out of `active` makes this run read 87.5% and reddens.
func TestHealthNearTimeoutSubtractsPausedSeconds(t *testing.T) {
	r := runRow("running")
	r.BudgetWallSeconds = frozenBudget(8)
	r.StartedAt = ago(7 * time.Hour)           // 7h wall clock since start
	r.BudgetPausedSeconds = int32(2 * 60 * 60) // 2h of it was parked at a gate
	r.LastActivityAt = ago(1 * time.Minute)    // recent → not stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		w := lastWrite(t, fs, r.ID)
		t.Fatalf("changed = %d, want 0 (5h of 8h = 62.5%% after the pause); wrote %q", n, w.Health)
	}
}

// TestHealthNearTimeoutNullBudgetUsesRunTimeout: a NULL-budget run has no frozen budget,
// so the arm measures against the global RUN_TIMEOUT (2h in testParams). 110m active is
// 91.6% of 2h → flagged; the 85% threshold is 102m.
func TestHealthNearTimeoutNullBudgetUsesRunTimeout(t *testing.T) {
	r := runRow("running")
	// BudgetWallSeconds left zero/invalid → global RUN_TIMEOUT (2h).
	r.StartedAt = ago(110 * time.Minute)    // 91.6% of the 2h global timeout
	r.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (110m of the 2h global timeout is past 85%%)", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthSlow {
		t.Fatalf("health = %q, want slow", w.Health)
	}
}

// TestHealthNearTimeoutExcludesJudgeAndInteractive mirrors SweepRunningTimeout's
// exclusion set (D3): a judge run and an interactive task NEVER time out, so the arm
// must never flag them — even at 99% of budget.
func TestHealthNearTimeoutExcludesJudgeAndInteractive(t *testing.T) {
	cases := []struct {
		name        string
		kind        string
		interactive bool
	}{
		{"judge run", "judge", false},
		{"interactive task", "task", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("running")
			r.Kind = tc.kind
			r.Interactive = tc.interactive
			r.BudgetWallSeconds = frozenBudget(8)
			r.StartedAt = ago(475 * time.Minute)    // ~99% of the 8h budget
			r.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc := healthSvc(fs, defaultHealthSettings())

			if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
				w := lastWrite(t, fs, r.ID)
				t.Fatalf("changed = %d, want 0 (%s never times out); wrote %q", n, tc.name, w.Health)
			}
		})
	}
}

func TestHealthThresholdDisablePerSignal(t *testing.T) {
	r := runRow("running")
	r.BudgetWallSeconds = frozenBudget(8)
	r.StartedAt = ago(8 * time.Hour)  // 100% of budget — would flag if enabled
	r.LastActivityAt = ago(time.Hour) // very silent
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	// stall disabled (0), near-timeout disabled (pct 0) → healthy despite the long
	// silence and a run at 100% of its budget.
	svc := healthSvc(fs, fakeHealthSettings{enabled: true, stall: 0, nearTimeoutPct: 0, queued: 600, approval: 3600})

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 with both running signals disabled", n)
	}
}

func TestHealthQueuedReasons(t *testing.T) {
	cases := []struct {
		name    string
		workers int64
		free    int64
		want    string
	}{
		{"no worker online", 0, 0, reasonNoWorker},
		// SC8: an online fleet with no free slot is saturated (add capacity), distinct
		// from an idle worker that simply hasn't claimed yet.
		{"fleet saturated, no free slot", 2, 0, reasonAllWorkersBusy},
		{"worker online with free slot, still waiting", 1, 1, reasonWaitingWorker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute) // > 10m queued
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: tc.workers, freeSlotWorkers: tc.free}
			svc := healthSvc(fs, defaultHealthSettings()) // vlt nil → treated unlocked

			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker {
				t.Fatalf("health = %q, want waiting_worker", w.Health)
			}
			if w.HealthReason.String != tc.want {
				t.Fatalf("reason = %q, want %q", w.HealthReason.String, tc.want)
			}
		})
	}
}

// TestHealthQueuedHandoffSetup drives the PRD #400 Decision 6 / issue #1367 M2 saga-setup
// rung: an UNDISPATCHED task run (kind='task', dispatched_at NULL) is not waiting for a
// worker — ClaimRun never offers it — so queuedReason reports reasonHandoffSetup AHEAD of
// every fleet reason, without consulting any worker count. A DISPATCHED task run and a
// non-task run both skip the short-circuit and fall through to the normal rungs (here an
// idle online worker → reasonWaitingWorker). The FLAG stays healthWaitingWorker throughout
// (the enum never changes; only the reason string differs).
func TestHealthQueuedHandoffSetup(t *testing.T) {
	dispatched := ago(5 * time.Minute)
	cases := []struct {
		name       string
		kind       string
		dispatched pgtype.Timestamptz
		// no online workers: the undispatched-task branch returns BEFORE any worker
		// count, so the fall-through cases set a worker/free slot to reach reasonWaitingWorker.
		workers int64
		free    int64
		want    string
	}{
		// Undispatched task: the honest saga-setup reason, no worker fakes consulted.
		{"undispatched task", "task", pgtype.Timestamptz{}, 0, 0, reasonHandoffSetup},
		// Dispatched task: skips the short-circuit, falls through to the normal rungs.
		{"dispatched task falls through", "task", dispatched, 1, 1, reasonWaitingWorker},
		// Non-task (undispatched) run: the short-circuit is task-only, so it falls through.
		{"non-task run falls through", "issue", pgtype.Timestamptz{}, 1, 1, reasonWaitingWorker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute) // > 10m queued
			r.Kind = tc.kind
			r.DispatchedAt = tc.dispatched
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: tc.workers, freeSlotWorkers: tc.free}
			svc := healthSvc(fs, defaultHealthSettings()) // vlt nil → treated unlocked

			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker {
				t.Fatalf("health = %q, want waiting_worker (the enum never changes)", w.Health)
			}
			if w.HealthReason.String != tc.want {
				t.Fatalf("reason = %q, want %q", w.HealthReason.String, tc.want)
			}
			if tc.want != reasonHandoffSetup && w.HealthReason.String == reasonHandoffSetup {
				t.Fatalf("reason = %q, must NOT be reasonHandoffSetup for %s", w.HealthReason.String, tc.name)
			}
		})
	}
}

// TestHealthQueuedCustodyLimit drives the PRD #1296 M4 (D4) custody-limit rung through
// detectRunHealth: a queued run whose OWNER is at the custody-hold admission limit reports
// reasonCustodyLimit (flag healthWaitingWorker), resolved against the SAME predicate the
// claim gates on (GetCustodyAdmissionForRun vs custodyHoldLimit) and AHEAD of every
// worker-availability reason — so even a zero-worker fleet reports the custody block, because
// bringing a worker online cannot clear it. Below the limit, or on a read error, it falls
// through to the generic worker reasons rather than inventing one.
func TestHealthQueuedCustodyLimit(t *testing.T) {
	cases := []struct {
		name    string
		holds   int64
		exempt  bool
		holdErr error
		// zero online workers, so the fall-through reason is reasonNoWorker: proves the
		// custody rung is checked AHEAD of worker availability (it wins despite 0 workers).
		want string
	}{
		{"at the limit fires custody reason (beats no-worker)", int64(custodyHoldLimit), false, nil, reasonCustodyLimit},
		{"over the limit fires custody reason", int64(custodyHoldLimit) + 3, false, nil, reasonCustodyLimit},
		{"one below the limit falls through", int64(custodyHoldLimit) - 1, false, nil, reasonNoWorker},
		{"read error falls through (no invented reason)", int64(custodyHoldLimit), false, errors.New("boom"), reasonNoWorker},
		// Issue #1751: a continuation-exempt run is admitted by ClaimRun at/over the cap, so
		// the pill must not claim a custody block for it.
		{"continuation-exempt run at the limit falls through", int64(custodyHoldLimit), true, nil, reasonNoWorker},
		{"continuation-exempt run over the limit falls through", int64(custodyHoldLimit) + 3, true, nil, reasonNoWorker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute) // > 10m queued
			fs := &healthFakeStore{
				active:        []store.ListActiveRunsForHealthRow{r},
				custodyHolds:  tc.holds,
				custodyExempt: tc.exempt,
				custodyErr:    tc.holdErr,
				onlineWorkers: 0, // no worker online → fall-through reason is reasonNoWorker
			}
			svc := healthSvc(fs, defaultHealthSettings()) // vlt nil → treated unlocked

			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker {
				t.Fatalf("health = %q, want waiting_worker (the enum never changes)", w.Health)
			}
			if w.HealthReason.String != tc.want {
				t.Fatalf("reason = %q, want %q", w.HealthReason.String, tc.want)
			}
			// The rung asks about THIS run's owner — the same predicate/owner the claim gates on.
			if len(fs.custodyCalls) != 1 || fs.custodyCalls[0] != r.UserID {
				t.Fatalf("custody lookups = %v, want exactly [%s] (the run's owner)", fs.custodyCalls, r.UserID)
			}
			if len(fs.custodyRunCalls) != 1 || fs.custodyRunCalls[0] != r.ID {
				t.Fatalf("custody run lookups = %v, want exactly [%s] (this run)", fs.custodyRunCalls, r.ID)
			}
		})
	}
}

// TestHealthQueuedRepoNotDockerAllowed drives PRD #361's new queued arm through
// detectRunHealth: a repo-bearing run no online worker is eligible to claim (all-Docker
// fleet, repo off the allowlist) reports reasonRepoNotDockerAllowed, while a merely-busy
// eligible worker, an idle eligible worker, and a repo-less run all fall through. The
// FLAG stays healthWaitingWorker in every case (the enum never changes).
func TestHealthQueuedRepoNotDockerAllowed(t *testing.T) {
	validRepo := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	cases := []struct {
		name            string
		repoID          pgtype.UUID
		kind            string
		onlineWorkers   int64
		eligibleWorkers int64
		freeSlotWorkers int64
		allowIDs        []uuid.UUID
		allowErr        error
		eligibleErr     error
		want            string
	}{
		// (a) all-Docker fleet, repo not allowlisted: no eligible worker → the new reason.
		{"all-docker not allowlisted", validRepo, "task", 1, 0, 0, nil, nil, nil, reasonRepoNotDockerAllowed},
		// (b) an eligible worker exists but is busy: eligible>0 must NOT fire the docker
		// reason, and with no free slot it falls through to all-busy.
		{"eligible worker busy", validRepo, "task", 2, 1, 0, nil, nil, nil, reasonAllWorkersBusy},
		// (b2) an eligible worker is idle: eligible>0, free slot → plain wait.
		{"eligible worker idle", validRepo, "task", 1, 1, 1, nil, nil, nil, reasonWaitingWorker},
		// (c) repo-less/judge run: repoID invalid, so the arm is guarded out and the
		// eligibleWorkers=0 is never consulted → plain wait.
		{"repo-less judge run", pgtype.UUID{}, "judge", 1, 0, 1, nil, nil, nil, reasonWaitingWorker},
		// (d) allowlist read error: the arm degrades (never a spurious docker reason)
		// and falls through to the generic free-slot logic. eligibleWorkers=0 is set but
		// must never be consulted (the read failed before the count) → all-busy.
		{"allowlist read error degrades", validRepo, "task", 1, 0, 0, nil, errFakeAllowlist, nil, reasonAllWorkersBusy},
		// (e) eligible-count error: same degrade — the arm falls through rather than
		// firing the docker reason on a failed count → all-busy.
		{"eligible count error degrades", validRepo, "task", 1, 0, 0, nil, nil, errFakeEligible, reasonAllWorkersBusy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute) // > 10m queued
			r.RepoID = tc.repoID
			r.Kind = tc.kind
			// A task run reaching the fleet rungs is DISPATCHED (issue #1367 M2): an
			// undispatched task run short-circuits to reasonHandoffSetup ahead of these
			// arms, so mark it dispatched to exercise the docker-allowlist logic here.
			// Harmless for the non-task case (dispatched_at is only consulted for task).
			r.DispatchedAt = ago(15 * time.Minute)
			fs := &healthFakeStore{
				active:          []store.ListActiveRunsForHealthRow{r},
				onlineWorkers:   tc.onlineWorkers,
				freeSlotWorkers: tc.freeSlotWorkers,
				eligibleWorkers: tc.eligibleWorkers,
				eligibleErr:     tc.eligibleErr,
			}
			svc := healthSvc(fs, defaultHealthSettings()) // vlt nil → treated unlocked
			svc.SetDockerAllowlist(fakeAllowlistReader{ids: tc.allowIDs, err: tc.allowErr})

			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			// Positive control: the enum never changes across any of these reasons.
			if w.Health != healthWaitingWorker {
				t.Fatalf("health = %q, want waiting_worker", w.Health)
			}
			if w.HealthReason.String != tc.want {
				t.Fatalf("reason = %q, want %q", w.HealthReason.String, tc.want)
			}
		})
	}
}

// TestHealthQueuedRepoNotDockerAllowedThreadsCaps is the reviewer's missing CASE A
// (issue #512 M2): rung 5's CountOnlineEligibleWorkersForRepo is now a TRUE claim-time
// count, so a cap-requiring run whose fleet HAS the caps (rung 3 skipped, satisfyingCaps>0)
// but that no worker can actually CLAIM (eligibleWorkers==0, empty allowlist) reports
// reasonRepoNotDockerAllowed with the flag ON — and the recorded lookup proves
// RequiredCapabilities and CapabilityAware were threaded so the count agrees with the claim
// path. The discriminating power needs BOTH satisfyingCaps>0 (else rung 3 would fire the
// capability reason) AND eligibleWorkers==0 (else rung 5 falls through), so both are pinned;
// freeSlotWorkers==0 makes the fall-through reason (all-busy) distinct, keeping the test
// non-vacuous.
func TestHealthQueuedRepoNotDockerAllowedThreadsCaps(t *testing.T) {
	validRepo := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	cases := []struct {
		name string
		req  []string
	}{
		// CASE A: single required cap, fleet has it, but the run is unclaimable at claim time.
		{"single cap", []string{"docker"}},
		// Multi-cap: same, with two required caps threaded verbatim into the count.
		{"multi cap", []string{"docker", "jvm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := queuedRunPastThreshold()
			r.RepoID = validRepo
			r.Kind = "task"
			// Dispatched task run (issue #1367 M2): an undispatched one short-circuits to
			// reasonHandoffSetup ahead of rung 5, so mark it dispatched to reach the arm.
			r.DispatchedAt = ago(15 * time.Minute)
			r.RequiredCapabilities = tc.req
			fs := &healthFakeStore{
				active:          []store.ListActiveRunsForHealthRow{r},
				onlineWorkers:   1,
				satisfyingCaps:  1, // fleet HAS the caps → rung 3 skipped
				eligibleWorkers: 0, // but none can actually claim → rung 5 fires
				freeSlotWorkers: 0, // fall-through would be all-busy (a distinct reason)
			}
			svc := healthSvc(fs, defaultHealthSettings()) // vlt nil → treated unlocked
			svc.capabilitySettings = fakeCapabilitySettings{on: true}
			svc.SetDockerAllowlist(fakeAllowlistReader{}) // empty allowlist

			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker {
				t.Fatalf("health = %q, want waiting_worker", w.Health)
			}
			if w.HealthReason.String != reasonRepoNotDockerAllowed {
				t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonRepoNotDockerAllowed)
			}
			// Rung 5 fired with the flag ON: its recorded lookup threads the run's required
			// set and the capability-aware flag, so the count agrees with the claim path.
			if len(fs.eligCalls) != 1 {
				t.Fatalf("issued %d eligible-worker lookups, want exactly 1", len(fs.eligCalls))
			}
			got := fs.eligCalls[0]
			if !equalStrings(got.RequiredCapabilities, r.RequiredCapabilities) {
				t.Fatalf("eligible lookup RequiredCapabilities = %v, want %v", got.RequiredCapabilities, r.RequiredCapabilities)
			}
			if !got.CapabilityAware {
				t.Fatal("eligible lookup CapabilityAware = false, want true (flag ON must be threaded)")
			}
		})
	}
}

// equalStrings compares two string slices for exact element-wise equality.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHealthQueuedBelowThresholdNotFlagged(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(5 * time.Minute) // < 10m
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 for a freshly queued run", n)
	}
}

func TestHealthApprovalIdleFlags(t *testing.T) {
	r := runRow("awaiting_approval")
	r.StatusSince = ago(90 * time.Minute) // > 1h
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthApprovalIdle || w.HealthReason.String != reasonApprovalIdle {
		t.Fatalf("got %q/%q, want approval_idle/%q", w.Health, w.HealthReason.String, reasonApprovalIdle)
	}
}

func TestHealthApprovalIdleExcludesAutoApprove(t *testing.T) {
	r := runRow("awaiting_approval")
	r.StatusSince = ago(90 * time.Minute)
	r.AutoApprove = true // autopilot self-resolves its gate — never nudge
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 for an auto_approve run", n)
	}
}

func TestHealthExitRaceNoOps(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(time.Hour)
	r.LastActivityAt = ago(10 * time.Minute)
	fs := &healthFakeStore{
		active:     []store.ListActiveRunsForHealthRow{r},
		leftStatus: map[uuid.UUID]bool{r.ID: true}, // run left 'running' between read and write
	}
	svc := healthSvc(fs, defaultHealthSettings())

	// The write is attempted (status-scoped), but matches 0 rows, so it is not
	// counted and nothing panics.
	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 when the status-scoped write no-ops", n)
	}
	if len(fs.writes) != 1 {
		t.Fatalf("attempted %d writes, want 1 (status-scoped attempt)", len(fs.writes))
	}
	if fs.writes[0].Status != "running" {
		t.Fatalf("write status scope = %q, want running", fs.writes[0].Status)
	}
}

func TestHealthSkipsUnchanged(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(time.Hour)
	r.LastActivityAt = ago(10 * time.Minute)
	r.Health = healthStalled // already flagged, same reason
	r.HealthReason = pgconv.TextOrNull(reasonStalled)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 (already stalled, unchanged)", n)
	}
	if len(fs.writes) != 0 {
		t.Fatalf("wrote %d, want 0 — an unchanged flag must not re-write", len(fs.writes))
	}
}

func TestHealthBroadcastsOnChangeNotOnNoop(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute) // stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	// First pass flags stalled → one broadcast carrying the flag.
	svc.detectRunHealth(context.Background(), t0)
	if len(b.healths) != 1 || b.healths[0] != healthStalled {
		t.Fatalf("healths = %v, want [stalled]", b.healths)
	}

	// Second pass with the run already reading stalled → no write, no broadcast.
	b.healths = nil
	r.Health = healthStalled
	r.HealthReason = pgconv.TextOrNull(reasonStalled)
	fs.active = []store.ListActiveRunsForHealthRow{r}
	svc.detectRunHealth(context.Background(), t0)
	if len(b.healths) != 0 {
		t.Fatalf("healths = %v, want none on an unchanged flag", b.healths)
	}
}

func TestHealthExitRaceDoesNotBroadcast(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	fs := &healthFakeStore{
		active:     []store.ListActiveRunsForHealthRow{r},
		leftStatus: map[uuid.UUID]bool{r.ID: true}, // write no-ops
	}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	svc.detectRunHealth(context.Background(), t0)
	if len(b.healths) != 0 {
		t.Fatalf("healths = %v, want none when the status-scoped write lost the exit race", b.healths)
	}
}

func TestHealthQueuedReasonChangeRewrites(t *testing.T) {
	// Same waiting_worker enum, different reason (a worker came online): the detector
	// re-writes so the reason updates, but health_since must be PRESERVED — the run
	// has been stuck since the original flag, so the UI's "stuck for Xm" must not reset.
	original := ago(15 * time.Minute)
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	r.Health = healthWaitingWorker
	r.HealthReason = pgconv.TextOrNull(reasonNoWorker)
	r.HealthSince = original
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1, freeSlotWorkers: 1}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (reason changed)", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonWaitingWorker)
	}
	if !w.HealthSince.Valid || !w.HealthSince.Time.Equal(original.Time) {
		t.Fatalf("health_since = %v, want the preserved original %v (a reason-only change must not reset it)", w.HealthSince, original)
	}
}

// stalledRunRow is a running run that will flag stalled (silent 10m, well under the
// near-timeout threshold so near-timeout never masks it).
func stalledRunRow() store.ListActiveRunsForHealthRow {
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	return r
}

func nudgeSvc(t *testing.T, r store.ListActiveRunsForHealthRow, st fakeHealthSettings) (*healthFakeStore, *Service, *fakeBroadcaster) {
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, st)
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)
	return fs, svc, b
}

func TestHealthNudgeOnFirstFlag(t *testing.T) {
	// ok→flagged with no prior nudge: the sweeper marks it nudge-worthy AND stamps
	// health_notified_at in the same write.
	fs, svc, b := nudgeSvc(t, stalledRunRow(), defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)

	if len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("healthNudges = %v, want [true] on the first flag", b.healthNudges)
	}
	if w := fs.writes[0]; !w.HealthNotifiedAt.Valid || !w.HealthNotifiedAt.Time.Equal(t0) {
		t.Fatalf("health_notified_at = %v, want now stamped on a nudge", w.HealthNotifiedAt)
	}
}

func TestHealthNoNudgeWithinCooldown(t *testing.T) {
	// ok→flagged but the last nudge was 10m ago (< 30m cooldown): the flag is still
	// written, but no nudge and no re-stamp (COALESCE(NULL, …) preserves the stamp).
	r := stalledRunRow()
	r.HealthNotifiedAt = ago(10 * time.Minute)
	fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)

	if len(b.healths) != 1 || b.healths[0] != healthStalled {
		t.Fatalf("healths = %v, want [stalled] (flag still written)", b.healths)
	}
	if b.healthNudges[0] {
		t.Fatal("nudge = true within the cooldown, want false")
	}
	if w := fs.writes[0]; w.HealthNotifiedAt.Valid {
		t.Fatalf("health_notified_at = %v, want NULL param within cooldown (preserve the old stamp)", w.HealthNotifiedAt)
	}
}

func TestHealthNudgeAfterCooldown(t *testing.T) {
	r := stalledRunRow()
	r.HealthNotifiedAt = ago(40 * time.Minute) // > 30m cooldown
	fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)

	if !b.healthNudges[0] {
		t.Fatal("nudge = false after the cooldown elapsed, want true")
	}
	if w := fs.writes[0]; !w.HealthNotifiedAt.Valid {
		t.Fatal("health_notified_at not re-stamped after the cooldown")
	}
}

func TestHealthNoNudgeOnFlagChange(t *testing.T) {
	// A run already flagged near-timeout (raw enum slow) that becomes stalled: same
	// episode continuing, not an ok→flagged transition, so no fresh nudge.
	r := stalledRunRow()
	r.Health = healthSlow
	r.HealthReason = pgconv.TextOrNull(reasonNearTimeout)
	_, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)

	if b.healths[0] != healthStalled {
		t.Fatalf("health = %q, want stalled", b.healths[0])
	}
	if b.healthNudges[0] {
		t.Fatal("nudge = true on a flag→flag change, want false")
	}
}

func TestHealthNoNudgeOnClear(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(30 * time.Second) // fresh → ok
	r.Health = healthStalled
	r.HealthReason = pgconv.TextOrNull(reasonStalled)
	r.HealthSince = ago(10 * time.Minute)
	_, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)

	if b.healths[0] != healthOK {
		t.Fatalf("health = %q, want ok (cleared)", b.healths[0])
	}
	if b.healthNudges[0] {
		t.Fatal("nudge = true on a clear, want false")
	}
}

func TestHealthCooldownZeroAlwaysNudges(t *testing.T) {
	// cooldown 0 disables the damping: an ok→flagged transition nudges even if the
	// last nudge was seconds ago.
	r := stalledRunRow()
	r.HealthNotifiedAt = ago(1 * time.Minute)
	_, svc, b := nudgeSvc(t, r, fakeHealthSettings{enabled: true, stall: 300, cooldown: 0})
	svc.detectRunHealth(context.Background(), t0)

	if !b.healthNudges[0] {
		t.Fatal("nudge = false with cooldown 0, want true (no damping)")
	}
}

func TestHealthRestartNoDupeNudge(t *testing.T) {
	// After an API restart the detector re-evaluates a still-stalled run: it reads the
	// persisted health=stalled, computes stalled again → unchanged → no write, no
	// broadcast, no nudge. The persisted health_notified_at is untouched.
	r := stalledRunRow()
	r.Health = healthStalled
	r.HealthReason = pgconv.TextOrNull(reasonStalled)
	r.HealthNotifiedAt = ago(1 * time.Minute)
	fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)

	if len(b.healthNudges) != 0 {
		t.Fatalf("re-nudged an already-flagged run after restart: %v", b.healthNudges)
	}
	if len(fs.writes) != 0 {
		t.Fatalf("re-wrote an unchanged flag after restart: %d writes", len(fs.writes))
	}
}

func TestHealthEnumChangeResetsSince(t *testing.T) {
	// A run flagged near-timeout (raw enum slow) that becomes stalled is a NEW episode:
	// health_since resets to now, not the old timestamp.
	oldSince := ago(30 * time.Minute)
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute) // stalled; not near-timeout (20m well under the threshold)
	r.Health = healthSlow
	r.HealthReason = pgconv.TextOrNull(reasonNearTimeout)
	r.HealthSince = oldSince
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled", w.Health)
	}
	if !w.HealthSince.Valid || !w.HealthSince.Time.Equal(t0) {
		t.Fatalf("health_since = %v, want now (%v) on an enum change", w.HealthSince, t0)
	}
}

// TestRunDeadline pins the one server-computed wall-clock deadline every surface shares
// (PRD #1170 D9): started_at + COALESCE(budget_wall_seconds, globalTimeout) +
// budget_paused_seconds, and nil when the run has no wall deadline (no started_at, a
// chat/judge/interactive run, or any non-running status).
func TestRunDeadline(t *testing.T) {
	const globalTimeout = 2 * time.Hour
	started := pgconv.Time(t0)
	budget8h := pgtype.Int4{Int32: 8 * 60 * 60, Valid: true}
	nullBudget := pgtype.Int4{}

	cases := []struct {
		name        string
		started     pgtype.Timestamptz
		budget      pgtype.Int4
		paused      int32
		ext         int32 // PRD #1189 M1: budget_extension_seconds, added to the sum
		fin         int32 // PRD #1497 M1: budget_finalize_seconds, the third additive term
		kind        string
		interactive bool
		status      string
		wantNil     bool
		want        time.Time
	}{
		{"8h budget + 32m pause", started, budget8h, int32(32 * 60), 0, 0, "issue", false, "running", false, t0.Add(8*time.Hour + 32*time.Minute)},
		{"null budget uses globalTimeout", started, nullBudget, 0, 0, 0, "issue", false, "running", false, t0.Add(globalTimeout)},
		// PRD #1189 M1: the extension is added on top of the frozen budget AND the pause.
		// 8h wall + 30m pause + 2h extension = 10h30m. Mutation check: dropping the
		// `effTimeout += budget_extension_seconds` fold in runWallClock reddens this case.
		{"8h budget + 30m pause + 2h extension", started, budget8h, int32(30 * 60), int32(2 * 60 * 60), 0, "issue", false, "running", false, t0.Add(10*time.Hour + 30*time.Minute)},
		{"extension alone, no pause", started, budget8h, 0, int32(2 * 60 * 60), 0, "issue", false, "running", false, t0.Add(10 * time.Hour)},
		// PRD #1497 M1: budget_finalize_seconds is the third additive term. 8h wall + 30m finalize =
		// 8h30m. Mutation check: dropping the `effTimeout += budget_finalize_seconds` fold reddens this.
		{"8h budget + 30m finalize", started, budget8h, 0, 0, int32(30 * 60), "issue", false, "running", false, t0.Add(8*time.Hour + 30*time.Minute)},
		// All three terms: 8h wall + 15m pause + 1h extension + 30m finalize = 9h45m.
		{"8h + pause + extension + finalize", started, budget8h, int32(15 * 60), int32(60 * 60), int32(30 * 60), "issue", false, "running", false, t0.Add(9*time.Hour + 45*time.Minute)},
		// A run with no wall deadline (chat) stays nil even when an extension is present:
		// the extension only moves an EXISTING deadline, it never creates one.
		{"chat with extension stays nil", started, budget8h, 0, int32(2 * 60 * 60), 0, "chat", false, "running", true, time.Time{}},
		{"null started_at", pgtype.Timestamptz{}, budget8h, 0, 0, 0, "issue", false, "running", true, time.Time{}},
		{"chat", started, budget8h, 0, 0, 0, "chat", false, "running", true, time.Time{}},
		{"judge", started, budget8h, 0, 0, 0, "judge", false, "running", true, time.Time{}},
		{"interactive", started, budget8h, 0, 0, 0, "task", true, "running", true, time.Time{}},
		{"queued", started, budget8h, 0, 0, 0, "issue", false, "queued", true, time.Time{}},
		{"awaiting_approval", started, budget8h, 0, 0, 0, "issue", false, "awaiting_approval", true, time.Time{}},
		{"completed", started, budget8h, 0, 0, 0, "issue", false, "completed", true, time.Time{}},
		{"failed", started, budget8h, 0, 0, 0, "issue", false, "failed", true, time.Time{}},
		{"cancelled", started, budget8h, 0, 0, 0, "issue", false, "cancelled", true, time.Time{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := RunDeadline(tc.started, tc.budget, tc.paused, tc.kind, tc.interactive, tc.status, globalTimeout, tc.ext, tc.fin)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("RunDeadline = %v, want nil", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("RunDeadline = nil, want %v", tc.want)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("RunDeadline = %v, want %v", *got, tc.want)
			}
		})
	}
}
