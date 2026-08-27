package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// PRD #84 M3: a queued run whose required_capabilities are not a subset of ANY online
// worker's effective caps must surface reasonNoEligibleWorker — a capability-specific
// "no eligible worker" reason — instead of the generic wait, gated by the
// capability-aware kill-switch (default ON). It stays queued and consumes no claim.
//
// SCOPE OF THIS FILE, like its sibling health_priority_test.go: these are ARM tests.
// They pin the MAPPING (flag ON + required non-empty + count 0 → reasonNoEligibleWorker;
// flag ON + count > 0 → the generic reason; flag OFF → never this reason; required empty
// → never this reason), the ARGUMENTS the lookup is issued with, and the guards that
// short-circuit ahead of it. The subset predicate itself — the effective-caps fold — is
// SQL, pinned against a real Postgres by store.TestCountOnlineWorkersSatisfyingCaps*LiveDB.
// Neither half alone is sufficient, and neither reimplements the other.

// fakeCapabilitySettings is a static CapabilityScheduleReader: `on` is the canned flag,
// `err` forces the read to fail (the arm treats a read error as ON, same fail direction
// as the claim path).
type fakeCapabilitySettings struct {
	on  bool
	err error
}

func (f fakeCapabilitySettings) CapabilityAwareScheduling(context.Context) (bool, error) {
	return f.on, f.err
}

// capSvc wires a queued-past-threshold run requiring `req`, a canned satisfying-worker
// count, and a capability-aware flag reader. onlineWorkers/freeSlotWorkers are set so the
// fall-through queuedReason resolves to reasonWaitingWorker (an online worker with a free
// slot) — the DIFFERENT reason a fall-through must land on, so a passing test cannot be
// vacuous (asserting the same string in both branches).
func capSvc(req []string, count int64, flag *fakeCapabilitySettings) (*healthFakeStore, *Service, store.ListActiveRunsForHealthRow) {
	r := queuedRunPastThreshold()
	r.RequiredCapabilities = req
	fs := &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		satisfyingCaps:  count,
		onlineWorkers:   1,
		freeSlotWorkers: 1,
	}
	svc := healthSvc(fs, defaultHealthSettings())
	if flag != nil {
		svc.capabilitySettings = *flag
	}
	return fs, svc, r
}

// flag ON + required non-empty + no worker satisfies → the capability-specific reason.
func TestHealthQueuedNoEligibleWorkerFlagsCapabilityReason(t *testing.T) {
	fs, svc, r := capSvc([]string{"docker"}, 0, &fakeCapabilitySettings{on: true})

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker — the flag maps to the existing enum, only the reason differs", w.Health)
	}
	if w.HealthReason.String != reasonNoEligibleWorker {
		t.Fatalf("reason = %q, want %q — an unplaceable run must say so", w.HealthReason.String, reasonNoEligibleWorker)
	}
	// Not vacuous: the generic wait it would otherwise emit is a DIFFERENT string.
	if w.HealthReason.String == reasonWaitingWorker {
		t.Fatal("used the generic queued reason for an unplaceable run, which does not name the missing capability")
	}
	// The lookup asked about THIS run's user and required set.
	if len(fs.capsCalls) != 1 {
		t.Fatalf("issued %d caps lookups, want exactly 1", len(fs.capsCalls))
	}
	if fs.capsCalls[0].UserID != r.UserID || len(fs.capsCalls[0].RequiredCapabilities) != 1 || fs.capsCalls[0].RequiredCapabilities[0] != "docker" {
		t.Fatalf("caps lookup = %+v, want user %s / [docker]", fs.capsCalls[0], r.UserID)
	}
}

// flag ON + a worker DOES satisfy → falls through to the generic reason (a capable
// worker exists; the run is just waiting for it to claim).
func TestHealthQueuedEligibleWorkerFallsThrough(t *testing.T) {
	fs, svc, r := capSvc([]string{"docker"}, 1, &fakeCapabilitySettings{on: true})

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String == reasonNoEligibleWorker {
		t.Fatal("emitted the no-eligible-worker reason while a capable worker is online")
	}
	if w.Health != healthWaitingWorker || w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("got %q/%q, want waiting_worker/%q — a placeable run keeps the generic reason", w.Health, w.HealthReason.String, reasonWaitingWorker)
	}
}

// flag OFF → the reason is NEVER emitted (best-effort claiming, no eligibility concept),
// and the arm short-circuits BEFORE issuing the Count.
func TestHealthQueuedFlagOffNeverCapabilityReason(t *testing.T) {
	fs, svc, r := capSvc([]string{"docker"}, 0, &fakeCapabilitySettings{on: false})

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String == reasonNoEligibleWorker {
		t.Fatal("emitted the no-eligible-worker reason with the kill-switch OFF")
	}
	if w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("reason = %q, want %q (the generic fall-through)", w.HealthReason.String, reasonWaitingWorker)
	}
	if len(fs.capsCalls) != 0 {
		t.Fatalf("issued %d caps lookups with the flag OFF, want 0 (short-circuit before the query)", len(fs.capsCalls))
	}
}

// required empty → the reason is NEVER emitted, and no Count is issued (an unrequired run
// claims anywhere).
func TestHealthQueuedEmptyRequiredNeverCapabilityReason(t *testing.T) {
	fs, svc, r := capSvc(nil, 0, &fakeCapabilitySettings{on: true})

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String == reasonNoEligibleWorker {
		t.Fatal("emitted the no-eligible-worker reason for a run with no required capabilities")
	}
	if w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("reason = %q, want %q (the generic fall-through)", w.HealthReason.String, reasonWaitingWorker)
	}
	if len(fs.capsCalls) != 0 {
		t.Fatalf("issued %d caps lookups for an unrequired run, want 0", len(fs.capsCalls))
	}
}

// A nil capabilitySettings reader DEFAULTS the flag ON — the same fail direction as the
// claim path — so an unplaceable run still surfaces the reason without a wired reader.
func TestHealthQueuedNilReaderDefaultsFlagOn(t *testing.T) {
	fs, svc, r := capSvc([]string{"docker"}, 0, nil) // no reader wired

	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs, r.ID); w.HealthReason.String != reasonNoEligibleWorker {
		t.Fatalf("reason = %q, want %q — a nil reader must default the flag ON", w.HealthReason.String, reasonNoEligibleWorker)
	}
}

// A flag READ ERROR is treated as ON (conservative, matching the claim path), so an
// unplaceable run still surfaces the reason.
func TestHealthQueuedFlagReadErrorTreatedAsOn(t *testing.T) {
	fs, svc, r := capSvc([]string{"docker"}, 0, &fakeCapabilitySettings{err: errors.New("boom")})

	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs, r.ID); w.HealthReason.String != reasonNoEligibleWorker {
		t.Fatalf("reason = %q, want %q — a flag read error must be treated as ON", w.HealthReason.String, reasonNoEligibleWorker)
	}
}

// A Count read error falls through to the generic queuedReason rather than inventing a
// reason on a failed lookup — the conservative degrade the sibling per-run lookups use.
func TestHealthQueuedCapsReadErrorDegradesToGenericReason(t *testing.T) {
	r := queuedRunPastThreshold()
	r.RequiredCapabilities = []string{"docker"}
	fs := &healthFakeStore{
		active:            []store.ListActiveRunsForHealthRow{r},
		satisfyingCapsErr: errors.New("boom"),
		onlineWorkers:     1,
		freeSlotWorkers:   1,
	}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.capabilitySettings = fakeCapabilitySettings{on: true}

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String == reasonNoEligibleWorker {
		t.Fatal("invented the no-eligible-worker reason on a failed Count lookup")
	}
	if w.Health != healthWaitingWorker || w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("got %q/%q, want waiting_worker/%q when the Count read fails", w.Health, w.HealthReason.String, reasonWaitingWorker)
	}
}

// The unplaceable check wins over the priority-class re-label: a demoted (background) run
// that is ALSO unplaceable reports the capability block, the more actionable reason.
func TestHealthQueuedNoEligibleWorkerBeatsPriorityClass(t *testing.T) {
	r := queuedRunPastThreshold()
	r.RequiredCapabilities = []string{"docker"}
	fs := &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		satisfyingCaps:  0,
		priorityClass:   map[uuid.UUID]string{r.ID: "background"}, // would say "deprioritized" if it won
		onlineWorkers:   1,
		freeSlotWorkers: 1,
	}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.capabilitySettings = fakeCapabilitySettings{on: true}

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonNoEligibleWorker {
		t.Fatalf("reason = %q, want %q — the unplaceable block must win over the deprioritized re-label", w.HealthReason.String, reasonNoEligibleWorker)
	}
	if w.HealthReason.String == reasonDeprioritized {
		t.Fatal("a yield message hid a genuine capability block")
	}
	// The priority lookup must not even be reached once the run is unplaceable.
	if len(fs.priorityCalls) != 0 {
		t.Fatalf("issued %d priority lookups after an unplaceable early-return, want 0", len(fs.priorityCalls))
	}
}

// The caps lookup runs BEHIND the queued-threshold guard, so a freshly-queued run issues
// no query at all — the affordability argument, same as queuedPriorityClass's.
func TestHealthQueuedCapsGuardShortCircuits(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(1 * time.Minute) // < 10m threshold
	r.RequiredCapabilities = []string{"docker"}
	fs := &healthFakeStore{
		active:         []store.ListActiveRunsForHealthRow{r},
		satisfyingCaps: 0, // would flag if reached
	}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.capabilitySettings = fakeCapabilitySettings{on: true}

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 (below threshold)", n)
	}
	if len(fs.capsCalls) != 0 {
		t.Fatalf("issued %d caps lookups behind a guard that must short-circuit first", len(fs.capsCalls))
	}
}

// The two-word reason string is the PRD #84 M3 wording, verbatim. A reword here without
// matching the PRD (and any downstream mirror) reddens deliberately.
func TestReasonNoEligibleWorkerWording(t *testing.T) {
	const want = "no online worker can run this — it needs a capability none of your workers has; provision a capable worker"
	if reasonNoEligibleWorker != want {
		t.Fatalf("reasonNoEligibleWorker = %q, does not match the PRD #84 M3 wording", reasonNoEligibleWorker)
	}
}

// ORDERING (review finding #2): vault-lock and no-online-worker are MORE FUNDAMENTAL than
// the capability reason and must be resolved AHEAD of it. Before the reorder the capability
// early-return ran in healthTargetFor, ahead of queuedReason where those two checks live, so
// a docker-requiring run whose vault is locked (or whose fleet is empty) mis-reported
// reasonNoEligibleWorker. These three cases pin the corrected precedence; (a) and (b) fail on
// the pre-reorder ordering and pass after it, (c) guards that the reorder did not swallow the
// capability reason on the path where it still applies.

// (a) A docker-requiring queued run whose owner's VAULT IS LOCKED reports reasonVaultLocked,
// not the capability reason — a locked vault means the run cannot start at all. The caps
// lookup must not even be reached.
func TestHealthQueuedLockedVaultBeatsCapabilityReason(t *testing.T) {
	box := newBox(t)
	r := queuedRunPastThreshold()
	r.RequiredCapabilities = []string{"docker"}
	fs := &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		satisfyingCaps:  0, // would flag reasonNoEligibleWorker if the caps check were reached
		onlineWorkers:   1,
		freeSlotWorkers: 1,
	}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	svc.SetVault(vault.New(box, newMemVaultStore())) // r.UserID never unlocked → locked

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonVaultLocked {
		t.Fatalf("reason = %q, want %q — a locked vault must preempt the capability reason", w.HealthReason.String, reasonVaultLocked)
	}
	if w.HealthReason.String == reasonNoEligibleWorker {
		t.Fatal("capability reason preempted a locked vault (the more fundamental block)")
	}
	if len(fs.capsCalls) != 0 {
		t.Fatalf("issued %d caps lookups after a vault-lock early-return, want 0", len(fs.capsCalls))
	}
}

// (b) A docker-requiring queued run with ZERO online workers reports reasonNoWorker, not the
// capability reason — an empty fleet is the more fundamental block (and the caps count is 0
// with no workers anyway). The caps lookup must not be reached.
func TestHealthQueuedNoOnlineWorkerBeatsCapabilityReason(t *testing.T) {
	r := queuedRunPastThreshold()
	r.RequiredCapabilities = []string{"docker"}
	fs := &healthFakeStore{
		active:         []store.ListActiveRunsForHealthRow{r},
		satisfyingCaps: 0, // 0 satisfying — but 0 online is the real reason
		onlineWorkers:  0, // no worker online at all
	}
	svc := healthSvc(fs, defaultHealthSettings()) // vlt nil → treated unlocked
	svc.capabilitySettings = fakeCapabilitySettings{on: true}

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonNoWorker {
		t.Fatalf("reason = %q, want %q — zero online workers must preempt the capability reason", w.HealthReason.String, reasonNoWorker)
	}
	if w.HealthReason.String == reasonNoEligibleWorker {
		t.Fatal("capability reason preempted a no-online-worker block")
	}
	if len(fs.capsCalls) != 0 {
		t.Fatalf("issued %d caps lookups after a no-worker early-return, want 0", len(fs.capsCalls))
	}
}

// (c) With the vault UNLOCKED and a worker ONLINE, the capability reason is still emitted for
// a run no online worker can satisfy — the hoisted vault-lock and no-worker guards return
// control to the capability check when they do not apply, so the reorder does not swallow the
// capability reason.
func TestHealthQueuedCapabilityReasonSurvivesReorder(t *testing.T) {
	box := newBox(t)
	r := queuedRunPastThreshold()
	r.RequiredCapabilities = []string{"docker"}
	fs := &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		satisfyingCaps:  0,
		onlineWorkers:   1,
		freeSlotWorkers: 1,
	}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.capabilitySettings = fakeCapabilitySettings{on: true}
	svc.SetVault(unlockedVault(t, r.UserID, box)) // r.UserID explicitly unlocked

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonNoEligibleWorker {
		t.Fatalf("reason = %q, want %q — an unlocked vault + online worker must still reach the capability reason", w.HealthReason.String, reasonNoEligibleWorker)
	}
	if len(fs.capsCalls) != 1 {
		t.Fatalf("issued %d caps lookups, want 1 (the capability check must run past the vault/no-worker guards)", len(fs.capsCalls))
	}
}
