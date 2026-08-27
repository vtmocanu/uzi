package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #320 D9: a queued run the kind-derived priority DEMOTED (judge/self_improve, no
// manual override, not yet past the background grace) must read to its owner as
// yielding to interactive work, not stuck; once past the grace it reads as restored.
//
// SCOPE OF THIS FILE, like its sibling health_verdict_test.go: these are ARM tests.
// They pin the MAPPING (class background → reasonDeprioritized, restored →
// reasonRestored, normal/expedited/error → the generic queuedReason), the ARGUMENTS
// the lookup is issued with (the cutoff built from WorkerBackgroundGrace), and the fact
// that the queued-threshold guard short-circuits ahead of it. The class itself —
// fn_run_priority_class's normal|background|expedited|restored decision — is SQL, pinned
// against a real Postgres by the store package's PRD #320 M1 tests. Neither half alone
// is sufficient, and neither reimplements the other.

// queuedRunPastThreshold is a queued run 15m into a 10m queued threshold: past the guard
// and squarely inside the arm under test.
func queuedRunPastThreshold() store.ListActiveRunsForHealthRow {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	return r
}

// prioritySvc wires a queued run whose canned priority class is `class`. onlineWorkers /
// freeSlotWorkers are set so the fall-through queuedReason resolves to reasonWaitingWorker
// (an online worker with a free slot), letting a test tell "deprioritized" apart from the
// ordinary wait rather than from a no-worker/busy reason.
func prioritySvc(r store.ListActiveRunsForHealthRow, class string) (*healthFakeStore, *Service) {
	fs := &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		priorityClass:   map[uuid.UUID]string{r.ID: class},
		onlineWorkers:   1,
		freeSlotWorkers: 1,
	}
	return fs, healthSvc(fs, defaultHealthSettings())
}

// class `background` — demoted, still within the grace — reads as deprioritized.
func TestHealthQueuedBackgroundFlagsDeprioritized(t *testing.T) {
	r := queuedRunPastThreshold()
	fs, svc := prioritySvc(r, "background")

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker — the flag must not change, only the reason", w.Health)
	}
	if w.HealthReason.String != reasonDeprioritized {
		t.Fatalf("reason = %q, want %q — a demoted run yields to interactive work, it is not stuck", w.HealthReason.String, reasonDeprioritized)
	}
	// It must NOT reuse the generic wait, which reads as stuck.
	if w.HealthReason.String == reasonWaitingWorker {
		t.Fatal("used the generic queued reason for a deprioritized run, which reads as stuck")
	}
}

// class `restored` — the D4 fail-open past the grace — reads as no longer yielding.
func TestHealthQueuedRestoredFlagsRestored(t *testing.T) {
	r := queuedRunPastThreshold()
	fs, svc := prioritySvc(r, "restored")

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker", w.Health)
	}
	if w.HealthReason.String != reasonRestored {
		t.Fatalf("reason = %q, want %q — past the background grace the run no longer yields", w.HealthReason.String, reasonRestored)
	}
}

// class `normal` — an ordinary interactive queued run — is unchanged: it falls through to
// the existing queuedReason (here reasonWaitingWorker, since a worker is online with a slot).
func TestHealthQueuedNormalUsesGenericReason(t *testing.T) {
	r := queuedRunPastThreshold()
	fs, svc := prioritySvc(r, "normal")

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker || w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("got %q/%q, want waiting_worker/%q — a normal run keeps the generic queued reason",
			w.Health, w.HealthReason.String, reasonWaitingWorker)
	}
}

// class `expedited` — a manually-prioritised (priority=2) run — is NOT deprioritized; it
// too falls through to the generic reason. Nothing but `background`/`restored` re-labels.
func TestHealthQueuedExpeditedIsNotDeprioritized(t *testing.T) {
	r := queuedRunPastThreshold()
	fs, svc := prioritySvc(r, "expedited")

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String == reasonDeprioritized || w.HealthReason.String == reasonRestored {
		t.Fatalf("reason = %q, want the generic queued reason — an expedited run is not a background one", w.HealthReason.String)
	}
	if w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonWaitingWorker)
	}
}

// The lookup must ask about THIS run with the cutoff built from WorkerBackgroundGrace —
// the SAME way service.go builds ClaimRun's — so the pill and the claim order agree on
// the D4 fail-open boundary.
func TestHealthQueuedPriorityLookupCutoff(t *testing.T) {
	r := queuedRunPastThreshold()
	fs, svc := prioritySvc(r, "background")

	svc.detectRunHealth(context.Background(), t0)
	if len(fs.priorityCalls) != 1 {
		t.Fatalf("issued %d priority lookups, want exactly 1", len(fs.priorityCalls))
	}
	got := fs.priorityCalls[0]
	if got.RunID != r.ID {
		t.Fatalf("looked up run %s, want %s", got.RunID, r.ID)
	}
	wantCutoff := t0.Add(-testParams().WorkerBackgroundGrace)
	if !got.BackgroundGraceCutoff.Valid || !got.BackgroundGraceCutoff.Time.Equal(wantCutoff) {
		t.Fatalf("background_grace_cutoff = %v, want now - WorkerBackgroundGrace = %v", got.BackgroundGraceCutoff, wantCutoff)
	}
}

// The lookup runs BEHIND the queued-threshold guard, so a freshly-queued run issues no
// query at all — the affordability argument, same as verdictUndelivered's.
func TestHealthQueuedPriorityGuardShortCircuits(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(1 * time.Minute) // < 10m threshold
	fs := &healthFakeStore{
		active:        []store.ListActiveRunsForHealthRow{r},
		priorityClass: map[uuid.UUID]string{r.ID: "background"}, // would flag if reached
	}
	svc := healthSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 (below threshold)", n)
	}
	if len(fs.priorityCalls) != 0 {
		t.Fatalf("issued %d priority lookups behind a guard that must short-circuit first", len(fs.priorityCalls))
	}
}

// A failed read degrades to the generic queuedReason rather than failing the sweep or
// suppressing the flag — a missed pill is noise, a lost health signal is not.
func TestHealthQueuedPriorityReadErrorDegradesToGenericReason(t *testing.T) {
	r := queuedRunPastThreshold()
	fs := &healthFakeStore{
		active:          []store.ListActiveRunsForHealthRow{r},
		priorityClass:   map[uuid.UUID]string{r.ID: "background"}, // would flag deprioritized if read
		priorityErr:     errors.New("boom"),
		onlineWorkers:   1,
		freeSlotWorkers: 1,
	}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker || w.HealthReason.String != reasonWaitingWorker {
		t.Fatalf("got %q/%q, want waiting_worker/%q when the priority read fails",
			w.Health, w.HealthReason.String, reasonWaitingWorker)
	}
}

// No status other than queued pays for the priority lookup.
func TestHealthQueuedPriorityLookupOnlyInQueuedArm(t *testing.T) {
	for _, status := range []string{"awaiting_approval", "running"} {
		t.Run(status, func(t *testing.T) {
			r := runRow(status)
			r.StatusSince = ago(90 * time.Minute)
			r.StartedAt = ago(90 * time.Minute)
			r.LastActivityAt = ago(90 * time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc := healthSvc(fs, defaultHealthSettings())

			svc.detectRunHealth(context.Background(), t0)
			if len(fs.priorityCalls) != 0 {
				t.Fatalf("a %s run issued %d priority lookups, want 0", status, len(fs.priorityCalls))
			}
		})
	}
}

// The two reason strings are the PRD D9 wording, verbatim. A reword here without matching
// the PRD (and any downstream mirror) reddens deliberately.
func TestReasonDeprioritizedRestoredWording(t *testing.T) {
	if reasonDeprioritized != "deprioritized — yields to interactive work" {
		t.Fatalf("reasonDeprioritized = %q, does not match PRD #320 D9 wording", reasonDeprioritized)
	}
	if reasonRestored != "priority restored — no longer yielding" {
		t.Fatalf("reasonRestored = %q, does not match PRD #320 D9 wording", reasonRestored)
	}
}
