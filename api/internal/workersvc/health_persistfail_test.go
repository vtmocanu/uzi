package workersvc

// PRD #108 M4 — the persistence-failure arm of runningTarget.
//
// The incident this closes reported `slow` at 45 minutes for a run that was neither
// slow nor working: `looping` reads its evidence from persisted run_messages, and
// the wedge IS a failure to persist them, so that arm is blind BY CONSTRUCTION.
// These tests drive the new arm's evidence source directly — the in-process streak —
// because that is the whole point: the signal does not travel through the database.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// wedge drives n identical failures for a run, the last of them `age` before t0, at
// the incident's observed ~2 Hz. Returns the run row so the caller can stage it.
func wedge(svc *Service, runID uuid.UUID, n int, since time.Duration) {
	start := t0.Add(-since)
	for i := 0; i < n; i++ {
		svc.persistFail.recordFailure(runID, persistFailUnstorable, 273, start.Add(time.Duration(i)*500*time.Millisecond))
	}
}

func TestHealthPersistFailingFlagsLoopingWithATruthfulReason(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(3 * time.Minute)      // nowhere near the 45m slow threshold
	r.LastActivityAt = ago(3 * time.Minute) // and under the 5m stall threshold
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	wedge(svc, r.ID, persistFlagStreak, persistFlagWindow+time.Second)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthLooping {
		t.Fatalf("health = %q, want looping", w.Health)
	}
	if !w.HealthReason.Valid || w.HealthReason.String != reasonPersistFailing {
		t.Fatalf("health_reason = %q, want %q: the incident's whole complaint was that the run reported the right vagueness for the wrong reason",
			w.HealthReason.String, reasonPersistFailing)
	}
}

func TestHealthPersistFailingNeedsBothStreakAndWindow(t *testing.T) {
	// The conjunction, one leg at a time. Either alone flagging would make a single
	// slow query or one blip look like a wedge.
	cases := []struct {
		name   string
		streak int
		since  time.Duration
	}{
		{"streak one short", persistFlagStreak - 1, persistFlagWindow + time.Minute},
		{"window one second short", persistFlagStreak * 4, persistFlagWindow - time.Second},
		{"no failures at all", 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := runRow("running")
			r.StartedAt = ago(3 * time.Minute)
			r.LastActivityAt = ago(3 * time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc := healthSvc(fs, defaultHealthSettings())
			wedge(svc, r.ID, c.streak, c.since)

			if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
				w := lastWrite(t, fs, r.ID)
				t.Fatalf("changed = %d (health=%q reason=%q), want 0: streak %d over %v must not reach the flag (needs >= %d AND >= %v)",
					n, w.Health, w.HealthReason.String, c.streak, c.since, persistFlagStreak, persistFlagWindow)
			}
		})
	}
}

func TestHealthPersistFailingBeatsToolLoopingAndSkipsItsQuery(t *testing.T) {
	// Both arms map to the same `looping` enum, so priority decides which REASON the
	// owner reads. On a run that is genuinely doing both, the persistence wedge is the
	// more specific truth — and the tool-window arm below would hide it.
	//
	// The second assertion is the free half: returning above s.toolWindow means a
	// wedged run stops issuing a ListRunToolWindow query per 15s tick. Only a call
	// counter can show that; the flag alone cannot.
	r := runRow("running")
	r.StartedAt = ago(3 * time.Minute)
	r.LastActivityAt = ago(3 * time.Minute)
	loop := make([]store.ListRunToolWindowRow, 0, 8)
	for i := 0; i < 8; i++ {
		loop = append(loop, useMsg(t, int32(100-i), "call_A", "Bash", map[string]any{"command": "go test ./..."}))
	}
	fs := &healthFakeStore{
		active: []store.ListActiveRunsForHealthRow{r},
		window: map[uuid.UUID][]store.ListRunToolWindowRow{r.ID: loop},
	}
	svc := healthSvc(fs, defaultHealthSettings())

	// Control: with no streak, the SAME fixture flags looping for the OTHER reason.
	// Without this the test could pass on a fixture that never loops at all.
	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs, r.ID); w.Health != healthLooping || w.HealthReason.String != reasonLooping {
		t.Fatalf("control: health=%q reason=%q, want looping/%q — the tool-window fixture must trip the existing arm, or the priority assertion below proves nothing",
			w.Health, w.HealthReason.String, reasonLooping)
	}
	queriesBefore := fs.windowCalls[r.ID]
	if queriesBefore != 1 {
		t.Fatalf("control: ListRunToolWindow called %d times, want 1", queriesBefore)
	}

	fs.writes = nil
	r.Health = healthLooping
	r.HealthReason = pgText(reasonLooping)
	fs.active = []store.ListActiveRunsForHealthRow{r}
	wedge(svc, r.ID, persistFlagStreak, persistFlagWindow+time.Second)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1: the reason must move even though the enum does not", n)
	}
	if w := lastWrite(t, fs, r.ID); w.HealthReason.String != reasonPersistFailing {
		t.Fatalf("health_reason = %q, want %q: the persistence arm must be checked ABOVE the tool-window arm, or a run that is both repeating a call AND failing to persist reports the repeat and hides the wedge",
			w.HealthReason.String, reasonPersistFailing)
	}
	if got := fs.windowCalls[r.ID]; got != queriesBefore {
		t.Fatalf("ListRunToolWindow calls went %d → %d; the persistence arm returns ABOVE that fetch, so a wedged run must issue no tool-window query at all", queriesBefore, got)
	}
}

func TestHealthPersistFailingSelfClearsAfterASuccess(t *testing.T) {
	// The flag is non-terminal and self-clearing like every other (PRD #47). One
	// successful append deletes the streak, so the very next tick returns the run to
	// ok — no separate clearing path, and nothing to leak if the wedge resolves.
	r := runRow("running")
	r.StartedAt = ago(3 * time.Minute)
	r.LastActivityAt = ago(3 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	wedge(svc, r.ID, persistFlagStreak*4, persistFlagWindow+time.Minute)

	svc.detectRunHealth(context.Background(), t0)
	if w := lastWrite(t, fs, r.ID); w.Health != healthLooping {
		t.Fatalf("precondition: health = %q, want looping", w.Health)
	}

	svc.persistFail.recordSuccess(r.ID, t0)
	fs.writes = nil
	r.Health = healthLooping
	r.HealthReason = pgText(reasonPersistFailing)
	fs.active = []store.ListActiveRunsForHealthRow{r}

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (the self-clear)", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthOK {
		t.Fatalf("health = %q, want ok: one success clears the streak and the flag must follow it", w.Health)
	}
}

func TestHealthPersistFailingStillRidesTheHealthToggle(t *testing.T) {
	// Decision 8 draws the line between the FLAG and the KILL: the looping flag may
	// ride health_enabled (it is early warning), and only M5's auto-stop must not.
	// Pinned here so a later reader does not "fix" the flag to bypass the toggle.
	r := runRow("running")
	r.StartedAt = ago(3 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, fakeHealthSettings{enabled: false, stall: 300, slow: 2700})
	wedge(svc, r.ID, persistFlagStreak*4, persistFlagWindow+time.Minute)

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 while health_enabled is false", n)
	}
}

func TestHealthPersistFailingNeverFlagsANonRunningRun(t *testing.T) {
	// The arm lives inside runningTarget, so a queued or awaiting_approval run keeps
	// its own vocabulary. A queued run has no worker appending anything, so a streak
	// on one would be stale state, not evidence.
	for _, status := range []string{"queued", "awaiting_approval"} {
		t.Run(status, func(t *testing.T) {
			r := runRow(status)
			r.UpdatedAt = ago(time.Minute) // under both the queued and approval thresholds
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 1}
			svc := healthSvc(fs, defaultHealthSettings())
			wedge(svc, r.ID, persistFlagStreak*4, persistFlagWindow+time.Minute)

			if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
				w := lastWrite(t, fs, r.ID)
				t.Fatalf("changed = %d (health=%q), want 0: the persistence arm must not reach a %s run", n, w.Health, status)
			}
		})
	}
}
