package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// t0 is a fixed "now" so every age is deterministic.
var t0 = time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

// healthFakeStore is a minimal Store for the detector: the three health reads/writes
// plus the queued-run worker count. Embedding Store means any other method a stray
// path reaches panics, keeping the tests honest about what the detector touches.
type healthFakeStore struct {
	Store
	active        []store.ListActiveRunsForHealthRow
	window        map[uuid.UUID][]store.ListRunToolWindowRow
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
}

func (f *healthFakeStore) ListActiveRunsForHealth(context.Context) ([]store.ListActiveRunsForHealthRow, error) {
	return f.active, nil
}
func (f *healthFakeStore) ListRunToolWindow(_ context.Context, arg store.ListRunToolWindowParams) ([]store.ListRunToolWindowRow, error) {
	if f.windowCalls == nil {
		f.windowCalls = map[uuid.UUID]int{}
	}
	f.windowCalls[arg.RunID]++
	return f.window[arg.RunID], nil
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
	enabled                                 bool
	stall, slow, queued, approval, cooldown int
}

func (s fakeHealthSettings) HealthEnabled(context.Context) (bool, error)      { return s.enabled, nil }
func (s fakeHealthSettings) HealthStallSeconds(context.Context) (int, error)  { return s.stall, nil }
func (s fakeHealthSettings) HealthSlowSeconds(context.Context) (int, error)   { return s.slow, nil }
func (s fakeHealthSettings) HealthQueuedSeconds(context.Context) (int, error) { return s.queued, nil }
func (s fakeHealthSettings) HealthApprovalSeconds(context.Context) (int, error) {
	return s.approval, nil
}
func (s fakeHealthSettings) HealthNudgeCooldownSeconds(context.Context) (int, error) {
	return s.cooldown, nil
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

// defaultHealthSettings mirrors the compiled-in defaults (5m / 45m / 10m / 1h / 30m).
func defaultHealthSettings() fakeHealthSettings {
	return fakeHealthSettings{enabled: true, stall: 300, slow: 2700, queued: 600, approval: 3600, cooldown: 1800}
}

func healthSvc(fs Store, st Settings) *Service {
	svc := New(fs, nil, testParams())
	svc.healthSettings = st
	return svc
}

func ago(d time.Duration) pgtype.Timestamptz { return pgTime(t0.Add(-d)) }

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
	r.StartedAt = ago(20 * time.Minute)      // under the 45m slow cap, so slow never masks the clear
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
	r.HealthReason = pgText(reasonStalled)
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
	svc := healthSvc(fs, fakeHealthSettings{enabled: true, stall: 300}) // slow disabled

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

func TestHealthSlowFlagsWithRecentActivity(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(50 * time.Minute)     // > 45m slow
	r.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthSlow {
		t.Fatalf("health = %q, want slow", w.Health)
	}
}

func TestHealthStalledBeatsSlow(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(50 * time.Minute)      // slow
	r.LastActivityAt = ago(10 * time.Minute) // also stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs, r.ID); w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled (priority over slow)", w.Health)
	}
}

func TestHealthThresholdDisablePerSignal(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(2 * time.Hour)
	r.LastActivityAt = ago(time.Hour) // very silent
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	// stall disabled (0), slow disabled (0) → healthy despite the long silence.
	svc := healthSvc(fs, fakeHealthSettings{enabled: true, stall: 0, slow: 0, queued: 600, approval: 3600})

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
	r.HealthReason = pgText(reasonStalled)
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
	r.HealthReason = pgText(reasonStalled)
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
	r.HealthReason = pgText(reasonNoWorker)
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

func TestHealthSlowClampWarnsOncePerValue(t *testing.T) {
	fs := &healthFakeStore{}
	// testParams RunTimeout is 2h; a 3h slow threshold at/above the GLOBAL timeout is the
	// operator misconfiguration this warns on. PRD #122 M2 (Decision 5b): slowThreshold
	// now returns the RAW value (the per-run clamp moved to clampSlow/runningTarget), but
	// the once-per-value warn stays here.
	svc := healthSvc(fs, fakeHealthSettings{enabled: true, slow: 3 * 60 * 60})
	ctx := context.Background()

	if d := svc.slowThreshold(ctx); d != 3*time.Hour {
		t.Fatalf("raw = %v, want the unclamped 3h", d)
	}
	if svc.lastSlowClampWarn != 3*time.Hour {
		t.Fatalf("lastSlowClampWarn = %v, want 3h recorded after the first clamp warn", svc.lastSlowClampWarn)
	}
	// A repeat pass at the same misconfigured value keeps the tracker stable — the
	// warn is not re-armed, so it logs once per distinct value, not every tick.
	svc.slowThreshold(ctx)
	if svc.lastSlowClampWarn != 3*time.Hour {
		t.Fatalf("lastSlowClampWarn = %v after a repeat pass, want a stable 3h", svc.lastSlowClampWarn)
	}
	// Reconfiguring below the timeout clears the tracker so a later re-break warns again.
	svc.healthSettings = fakeHealthSettings{enabled: true, slow: 300}
	if d := svc.slowThreshold(ctx); d != 5*time.Minute {
		t.Fatalf("slow = %v, want 5m raw", d)
	}
	if svc.lastSlowClampWarn != 0 {
		t.Fatalf("lastSlowClampWarn = %v, want reset to 0 once configured below the timeout", svc.lastSlowClampWarn)
	}
}

// clampSlow is the per-run clamp (PRD #122 M2, Decision 5b): raw unchanged below the
// effective timeout, pulled just under at/above it, 0 stays 0.
func TestClampSlow(t *testing.T) {
	cases := []struct {
		name     string
		raw, eff time.Duration
		want     time.Duration
	}{
		{"raw below eff is unchanged", 45 * time.Minute, 2 * time.Hour, 45 * time.Minute},
		{"raw at eff pulls under", 2 * time.Hour, 2 * time.Hour, 2*time.Hour - time.Minute},
		{"raw above eff pulls under", 3 * time.Hour, 2 * time.Hour, 2*time.Hour - time.Minute},
		{"zero raw stays zero", 0, 2 * time.Hour, 0},
		{"tiny eff falls back to eff/2", 30 * time.Second, 30 * time.Second, 15 * time.Second},
		{"scaled eff keeps a big raw", 3 * time.Hour, 8 * time.Hour, 3 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampSlow(tc.raw, tc.eff); got != tc.want {
				t.Fatalf("clampSlow(%v,%v) = %v, want %v", tc.raw, tc.eff, got, tc.want)
			}
		})
	}
}

// TestHealthSlowClampsPerRunBudget (Decision 5b): a scaled run (budget_wall = 8h) with
// a raw slow threshold >= the GLOBAL RUN_TIMEOUT and started 3h ago is NOT slow — its
// effective ceiling is 8h, so the clamp lands near 8h, not near 2h. The SAME run with a
// NULL budget (global 2h) started the same 3h ago IS slow.
func TestHealthSlowClampsPerRunBudget(t *testing.T) {
	// Raw slow = 4h: above the global 2h RUN_TIMEOUT (misconfig path exercised) and above
	// the 3h elapsed. For the scaled run (8h budget) issue #323 first scales the raw
	// threshold by the budget ratio (4h × 8h/2h = 16h), which clampSlow then pulls to the
	// 8h ceiling (~8h after scaling) — so it has not fired at 3h, while the unscaled run's
	// (clamped to ~2h) has.
	settings := fakeHealthSettings{enabled: true, slow: 4 * 60 * 60}

	scaled := runRow("running")
	scaled.StartedAt = ago(3 * time.Hour)
	scaled.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
	scaled.BudgetWallSeconds = pgtype.Int4{Int32: 8 * 60 * 60, Valid: true}
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{scaled}}
	svc := healthSvc(fs, settings)
	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		w := lastWrite(t, fs, scaled.ID)
		t.Fatalf("a scaled run 3h into an 8h budget must not flag; wrote %q", w.Health)
	}

	unscaled := runRow("running")
	unscaled.StartedAt = ago(3 * time.Hour)
	unscaled.LastActivityAt = ago(1 * time.Minute)
	// BudgetWallSeconds left zero/invalid → global 2h ceiling, clamp near 2h-1m.
	fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{unscaled}}
	svc2 := healthSvc(fs2, settings)
	if n := svc2.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (an unscaled run 3h in is slow)", n)
	}
	if w := lastWrite(t, fs2, unscaled.ID); w.Health != healthSlow {
		t.Fatalf("health = %q, want slow for the NULL-budget run", w.Health)
	}
}

// TestHealthSlowScalesWithBudget (issue #323): the raw health_slow_seconds threshold is
// scaled UP by a run's budget ratio (effTimeout / RUN_TIMEOUT) before the per-run clamp,
// so a milestone-scaled run is not flagged "slow" at the flat default while it is still
// working. testParams RunTimeout is 2h; an 8h budget → scaled slow = 2700 × 8h/2h = 3h.
func TestHealthSlowScalesWithBudget(t *testing.T) {
	// 1. A scaled run active 90m in — well past the flat 45m default, but under the 3h
	//    scaled threshold — must NOT flag. Fails before the ratio scaling.
	scaledOK := runRow("running")
	scaledOK.BudgetWallSeconds = pgtype.Int4{Int32: 8 * 60 * 60, Valid: true}
	scaledOK.StartedAt = ago(90 * time.Minute)
	scaledOK.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{scaledOK}}
	svc := healthSvc(fs, defaultHealthSettings())
	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		w := lastWrite(t, fs, scaledOK.ID)
		t.Fatalf("a scaled run 90m into an 8h budget must not flag; wrote %q", w.Health)
	}

	// 2. The same scaled run past the 3h scaled threshold IS slow.
	scaledSlow := runRow("running")
	scaledSlow.BudgetWallSeconds = pgtype.Int4{Int32: 8 * 60 * 60, Valid: true}
	scaledSlow.StartedAt = ago(5 * time.Hour)        // past the 3h scaled threshold
	scaledSlow.LastActivityAt = ago(1 * time.Minute) // recent → not stalled
	fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{scaledSlow}}
	svc2 := healthSvc(fs2, defaultHealthSettings())
	if n := svc2.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (a scaled run 5h in is slow)", n)
	}
	if w := lastWrite(t, fs2, scaledSlow.ID); w.Health != healthSlow {
		t.Fatalf("health = %q, want slow for the scaled run past 3h", w.Health)
	}

	// 3. An unscaled (NULL budget) run is unchanged: flagged slow at the flat 45m.
	unscaled := runRow("running")
	unscaled.StartedAt = ago(50 * time.Minute) // > 45m flat default
	unscaled.LastActivityAt = ago(1 * time.Minute)
	fs3 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{unscaled}}
	svc3 := healthSvc(fs3, defaultHealthSettings())
	if n := svc3.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (an unscaled run 50m in is slow)", n)
	}
	if w := lastWrite(t, fs3, unscaled.ID); w.Health != healthSlow {
		t.Fatalf("health = %q, want slow for the NULL-budget run", w.Health)
	}
}

// stalledRunRow is a running run that will flag stalled (silent 10m, under the 45m
// slow cap so slow never masks it).
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
	// A run already flagged slow that becomes stalled: same episode continuing, not an
	// ok→flagged transition, so no fresh nudge.
	r := stalledRunRow()
	r.Health = healthSlow
	r.HealthReason = pgText(reasonSlow)
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
	r.HealthReason = pgText(reasonStalled)
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
	r.HealthReason = pgText(reasonStalled)
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
	// A run flagged slow that becomes stalled is a NEW episode: health_since resets to
	// now, not the old slow timestamp.
	oldSince := ago(30 * time.Minute)
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute) // stalled; not slow (20m < 45m)
	r.Health = healthSlow
	r.HealthReason = pgText(reasonSlow)
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
