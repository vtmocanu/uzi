package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestHealthCustodyRunHoldBoundNudge pins issue #4162: the per-run hold bound is the run's own
// condition, so a run ENTERING it nudges (from ok, or from a waiting_worker flag it already
// carried for another reason, including the owner-level reasonCustodyLimit), while a run already
// carrying the bound reason does not re-nudge and the cooldown still applies. The owner is below
// the limit in every case, so only the per-run bound explains the reason.
func TestHealthCustodyRunHoldBoundNudge(t *testing.T) {
	txt := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	cases := []struct {
		name       string
		health     string
		reason     pgtype.Text
		notifiedAt pgtype.Timestamptz
		wantWrite  bool
		wantNudge  bool
	}{
		{"ok to bound nudges", healthOK, pgtype.Text{}, pgtype.Timestamptz{}, true, true},
		{"waiting_worker custody limit to bound nudges", healthWaitingWorker, txt(reasonCustodyLimit), pgtype.Timestamptz{}, true, true},
		{"bound to bound does not nudge again", healthWaitingWorker, txt(apitypes.ReasonCustodyRunHoldBound), pgtype.Timestamptz{}, false, false},
		{"waiting_worker generic reason to bound nudges", healthWaitingWorker, txt(reasonNoWorker), pgtype.Timestamptz{}, true, true},
		{"waiting_worker maintenance to bound nudges", healthWaitingWorker, txt(reasonMaintenance), pgtype.Timestamptz{}, true, true},
		{"entering bound within cooldown does not nudge", healthWaitingWorker, txt(reasonCustodyLimit), ago(5 * time.Minute), true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := runRow("queued")
			r.StatusSince = ago(15 * time.Minute)
			r.Health = tc.health
			r.HealthReason = tc.reason
			r.HealthNotifiedAt = tc.notifiedAt
			// Owner raw open holds (>= the run's own) reach the limit while the owner's
			// admission-counted units stay below it, as a real same-run group would.
			counted := int64(testCustodyLimit) - 1
			fs := &healthFakeStore{
				active:          []store.ListActiveRunsForHealthRow{r},
				custodyHolds:    int64(testCustodyLimit),
				custodyCounted:  &counted,
				custodyOwnOpen:  int64(testCustodyLimit),
				custodyRunBound: true,
			}
			svc := healthSvc(fs, defaultHealthSettings())
			b := &fakeBroadcaster{}
			svc.SetBroadcaster(b)

			n := svc.detectRunHealth(context.Background(), t0)
			if !tc.wantWrite {
				if n != 0 || len(fs.writes) != 0 || len(b.healthNudges) != 0 {
					t.Fatalf("changed=%d writes=%d broadcasts=%d, want none (state already stored)", n, len(fs.writes), len(b.healthNudges))
				}
				return
			}
			if n != 1 {
				t.Fatalf("changed = %d, want 1", n)
			}
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker || w.HealthReason.String != apitypes.ReasonCustodyRunHoldBound {
				t.Fatalf("write = %q / %q, want waiting_worker / the run-bound reason", w.Health, w.HealthReason.String)
			}
			if len(b.healthNudges) != 1 || b.healthNudges[0] != tc.wantNudge {
				t.Fatalf("broadcast nudge = %v, want [%v]", b.healthNudges, tc.wantNudge)
			}
			if w.HealthNotifiedAt.Valid != tc.wantNudge {
				t.Fatalf("health_notified_at valid = %v, want %v (stamped exactly when nudging)", w.HealthNotifiedAt.Valid, tc.wantNudge)
			}
		})
	}
}

// TestHealthCustodyLimitStaysSuppressedFromBound is the control for the run-bound arm: a run that
// carried the bound reason, whose own holds then settle below the bound while the owner stays at
// the limit, moves to the owner-level reasonCustodyLimit, which still never nudges.
func TestHealthCustodyLimitStaysSuppressedFromBound(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	r.Health = healthWaitingWorker
	r.HealthReason = pgtype.Text{String: apitypes.ReasonCustodyRunHoldBound, Valid: true}
	fs := &healthFakeStore{
		active:         []store.ListActiveRunsForHealthRow{r},
		custodyHolds:   int64(testCustodyLimit),
		custodyOwnOpen: int64(testCustodyLimit) - 1,
	}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.HealthReason.String != reasonCustodyLimit {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonCustodyLimit)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] || w.HealthNotifiedAt.Valid {
		t.Fatalf("nudge = %v notified=%v, want suppressed", b.healthNudges, w.HealthNotifiedAt.Valid)
	}
}

// TestHealthCustodyLimitNudgeSuppressed pins PRD #1349 M6 (D10): a queued run whose owner is at
// the custody-hold admission limit still gets its per-run health STATE written and broadcast (the
// pill is unchanged — waiting_worker / reasonCustodyLimit), but its per-run Slack NUDGE is
// SUPPRESSED, because the owner-level custody-episode reconciler coalesces that crossing into one
// owner DM. Without the suppression this run's first flag would nudge (r.Health==ok, no prior
// health_notified_at), so the false nudge below is the discriminating assertion.
func TestHealthCustodyLimitNudgeSuppressed(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute) // > 10m queued → past the threshold
	fs := &healthFakeStore{
		active:        []store.ListActiveRunsForHealthRow{r},
		custodyHolds:  int64(testCustodyLimit), // at the limit → reasonCustodyLimit
		onlineWorkers: 0,
	}
	svc := healthSvc(fs, defaultHealthSettings()) // cooldown 30m, but no prior nudge → would nudge
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (the STATE is still written)", n)
	}
	// STATE/pill unchanged: the waiting_worker flag + the custody reason are persisted.
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker (the pill must be unchanged)", w.Health)
	}
	if w.HealthReason.String != reasonCustodyLimit {
		t.Fatalf("reason = %q, want %q (the pill must be unchanged)", w.HealthReason.String, reasonCustodyLimit)
	}
	// health_notified_at must NOT be stamped — no per-run nudge means the cooldown is not burned,
	// so a later non-custody flag on the same run can still nudge on its own merits.
	if w.HealthNotifiedAt.Valid {
		t.Fatalf("health_notified_at = %v, want NULL (a suppressed nudge must not stamp the cooldown)", w.HealthNotifiedAt)
	}
	// The broadcast fires (the web pill updates) but carries nudge=false, so slacksvc's
	// handleHealth posts no threaded DM for this run.
	if len(b.healths) != 1 || b.healths[0] != healthWaitingWorker {
		t.Fatalf("broadcast healths = %v, want [waiting_worker]", b.healths)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("broadcast nudge = %v, want [false] (the per-run custody nudge is suppressed)", b.healthNudges)
	}
}

// TestHealthHandoffSetupNudgeSuppressed pins issue #1367: an UNDISPATCHED task (handoff) run gets
// its health STATE written and broadcast (waiting_worker / reasonHandoffSetup), but its per-run
// Slack NUDGE is SUPPRESSED — the waiting_worker nudge head ("waiting for a worker to pick it up")
// is FALSE of a run ClaimRun never offers, so a nudge would DM a self-contradicting head + honest
// reason. Without the suppression this first flag would nudge (Health==ok, no prior
// health_notified_at), so the false nudge below is the discriminating assertion.
func TestHealthHandoffSetupNudgeSuppressed(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute) // > 10m queued → past the threshold
	r.Kind = "task"                       // DispatchedAt left zero/invalid → undispatched → reasonHandoffSetup
	fs := &healthFakeStore{
		active:        []store.ListActiveRunsForHealthRow{r},
		onlineWorkers: 0, // irrelevant: the handoff-setup branch returns before any worker count
	}
	svc := healthSvc(fs, defaultHealthSettings()) // no prior nudge → would nudge absent the suppression
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (the STATE is still written)", n)
	}
	// STATE/pill unchanged: the waiting_worker flag + the honest handoff-setup reason are persisted.
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker (the pill must be unchanged)", w.Health)
	}
	if w.HealthReason.String != reasonHandoffSetup {
		t.Fatalf("reason = %q, want %q (the honest reason must still be written)", w.HealthReason.String, reasonHandoffSetup)
	}
	// A suppressed nudge must not stamp the cooldown.
	if w.HealthNotifiedAt.Valid {
		t.Fatalf("health_notified_at = %v, want NULL (a suppressed nudge must not stamp the cooldown)", w.HealthNotifiedAt)
	}
	// The broadcast fires (the web pill updates) but carries nudge=false, so slacksvc posts no
	// threaded DM — the false "waiting for a worker" head is never sent for a handoff-setup run.
	if len(b.healths) != 1 || b.healths[0] != healthWaitingWorker {
		t.Fatalf("broadcast healths = %v, want [waiting_worker]", b.healths)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("broadcast nudge = %v, want [false] (the per-run handoff-setup nudge is suppressed)", b.healthNudges)
	}
}

// TestHealthNonCustodyQueuedReasonStillNudges is the discriminating control: a queued run blocked
// for a NON-custody reason (no worker online) DOES nudge on its first flag. The M6 suppression is
// keyed strictly to reasonCustodyLimit, not to the shared waiting_worker enum, so every other
// queued reason keeps its per-run nudge and stamps the cooldown.
func TestHealthNonCustodyQueuedReasonStillNudges(t *testing.T) {
	r := runRow("queued")
	r.StatusSince = ago(15 * time.Minute)
	fs := &healthFakeStore{
		active:        []store.ListActiveRunsForHealthRow{r},
		custodyHolds:  int64(testCustodyLimit) - 1, // below the limit → custody rung falls through
		onlineWorkers: 0,                           // → reasonNoWorker
	}
	svc := healthSvc(fs, defaultHealthSettings())
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker", w.Health)
	}
	if w.HealthReason.String != reasonNoWorker {
		t.Fatalf("reason = %q, want %q (the control reason)", w.HealthReason.String, reasonNoWorker)
	}
	if len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("broadcast nudge = %v, want [true] (a non-custody queued reason still nudges)", b.healthNudges)
	}
	if !w.HealthNotifiedAt.Valid {
		t.Fatal("health_notified_at not stamped for a real (non-custody) per-run nudge")
	}
}
