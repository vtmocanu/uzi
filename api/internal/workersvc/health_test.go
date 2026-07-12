package workersvc

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
	writes        []store.SetRunHealthParams
	// leftStatus marks run ids whose SetRunHealth returns 0 rows — the exit race,
	// where the run changed status between the list read and the health write.
	leftStatus map[uuid.UUID]bool
}

func (f *healthFakeStore) ListActiveRunsForHealth(context.Context) ([]store.ListActiveRunsForHealthRow, error) {
	return f.active, nil
}
func (f *healthFakeStore) ListRunToolWindow(_ context.Context, arg store.ListRunToolWindowParams) ([]store.ListRunToolWindowRow, error) {
	return f.window[arg.RunID], nil
}
func (f *healthFakeStore) CountOnlineWorkersForUser(context.Context, uuid.UUID) (int64, error) {
	return f.onlineWorkers, nil
}
func (f *healthFakeStore) SetRunHealth(_ context.Context, arg store.SetRunHealthParams) (int64, error) {
	f.writes = append(f.writes, arg)
	if f.leftStatus[arg.ID] {
		return 0, nil
	}
	return 1, nil
}

// fakeSettings is a static health-settings source. All accessors are error-free;
// the zero value has the detector disabled, so tests opt in explicitly.
type fakeSettings struct {
	enabled                       bool
	stall, slow, queued, approval int
}

func (s fakeSettings) HealthEnabled(context.Context) (bool, error)        { return s.enabled, nil }
func (s fakeSettings) HealthStallSeconds(context.Context) (int, error)    { return s.stall, nil }
func (s fakeSettings) HealthSlowSeconds(context.Context) (int, error)     { return s.slow, nil }
func (s fakeSettings) HealthQueuedSeconds(context.Context) (int, error)   { return s.queued, nil }
func (s fakeSettings) HealthApprovalSeconds(context.Context) (int, error) { return s.approval, nil }

// defaultHealthSettings mirrors the compiled-in defaults (5m / 45m / 10m / 1h).
func defaultHealthSettings() fakeSettings {
	return fakeSettings{enabled: true, stall: 300, slow: 2700, queued: 600, approval: 3600}
}

func healthSvc(fs Store, st Settings) *Service {
	svc := New(fs, nil, testParams())
	svc.settings = st
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
	svc := healthSvc(fs, fakeSettings{enabled: false, stall: 300})

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
	svc := healthSvc(fs, fakeSettings{enabled: true, stall: 300}) // slow disabled

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
	svc := healthSvc(fs, fakeSettings{enabled: true, stall: 0, slow: 0, queued: 600, approval: 3600})

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 with both running signals disabled", n)
	}
}

func TestHealthQueuedReasons(t *testing.T) {
	cases := []struct {
		name    string
		workers int64
		want    string
	}{
		{"no worker online", 0, reasonNoWorker},
		{"worker online, still waiting", 1, reasonWaitingWorker},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.UpdatedAt = ago(15 * time.Minute) // > 10m queued
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: tc.workers}
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

func TestHealthQueuedBelowThresholdNotFlagged(t *testing.T) {
	r := runRow("queued")
	r.UpdatedAt = ago(5 * time.Minute) // < 10m
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 for a freshly queued run", n)
	}
}

func TestHealthApprovalIdleFlags(t *testing.T) {
	r := runRow("awaiting_approval")
	r.UpdatedAt = ago(90 * time.Minute) // > 1h
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
	r.UpdatedAt = ago(90 * time.Minute)
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
	r.UpdatedAt = ago(15 * time.Minute)
	r.Health = healthWaitingWorker
	r.HealthReason = pgText(reasonNoWorker)
	r.HealthSince = original
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1}
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
	// testParams RunTimeout is 2h; a 3h slow threshold forces the read-time clamp.
	svc := healthSvc(fs, fakeSettings{enabled: true, slow: 3 * 60 * 60})
	ctx := context.Background()

	if d := svc.slowThreshold(ctx); d != testParams().RunTimeout-time.Minute {
		t.Fatalf("clamped = %v, want RunTimeout-1m", d)
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
	svc.settings = fakeSettings{enabled: true, slow: 300}
	if d := svc.slowThreshold(ctx); d != 5*time.Minute {
		t.Fatalf("slow = %v, want 5m (no clamp)", d)
	}
	if svc.lastSlowClampWarn != 0 {
		t.Fatalf("lastSlowClampWarn = %v, want reset to 0 once configured below the timeout", svc.lastSlowClampWarn)
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
