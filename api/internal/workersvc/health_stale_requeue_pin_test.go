package workersvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Issue #2705 M3: the health detector explains a stale-requeued run that ClaimRun is holding for
// its returning worker, on a controlled clock (t0 is "now" unless a case moves it).

const pinTestGrace = 20 * time.Minute

// pinnedRun is a queued run requeued one minute before t0 whose owner row exists.
func pinnedRun() store.ListActiveRunsForHealthRow {
	r := runRow("queued")
	r.StatusSince = ago(time.Minute)
	r.UpdatedAt = ago(time.Minute)
	r.StaleRequeuePinnable = true
	r.StaleRequeueWorkerName = "w1"
	return r
}

func pinSvc(fs *healthFakeStore, settings fakeHealthSettings) (*Service, *fakeBroadcaster) {
	svc := healthSvc(fs, settings)
	svc.p.WorkerStaleRequeueGrace = pinTestGrace
	svc.p.WorkerAffinityCeiling = 2 * time.Hour
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)
	return svc, b
}

// The reason shows inside the grace, before the queued threshold would flag anything, with the
// exact deadline, and the per-run nudge is suppressed.
func TestHealthStaleRequeuePinReasonInsideGraceNoNudge(t *testing.T) {
	r := pinnedRun() // 1m old: far below the 10m queued threshold
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, b := pinSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	want := "waiting until 2026-07-12T12:19:00Z for its previous worker w1 to return; another worker may take it after that"
	if w.Health != healthWaitingWorker || w.HealthReason.String != want {
		t.Fatalf("write = (%q, %q), want (waiting_worker, %q)", w.Health, w.HealthReason.String, want)
	}
	if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("nudge fired for the pin reason: notifiedAt=%v nudges=%v", w.HealthNotifiedAt, b.healthNudges)
	}

	// Stable across ticks for one episode: the already-written row produces no new write.
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, w.HealthReason, w.HealthSince
	fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc2, _ := pinSvc(fs2, defaultHealthSettings())
	if n := svc2.detectRunHealth(context.Background(), t0.Add(30*time.Second)); n != 0 {
		t.Fatalf("second tick rewrote the reason (changed=%d, writes=%v)", n, fs2.writes)
	}
}

// A nameless owner still renders, without a dangling name.
func TestHealthStaleRequeuePinReasonWithoutName(t *testing.T) {
	r := pinnedRun()
	r.StaleRequeueWorkerName = ""
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, _ := pinSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	got := lastWrite(t, fs, r.ID).HealthReason.String
	want := "waiting until 2026-07-12T12:19:00Z for its previous worker to return; another worker may take it after that"
	if got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
}

// The reason clears when the run is claimed (it leaves queued): the running arm self-clears the
// stale flag.
func TestHealthStaleRequeuePinClearsOnClaim(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(time.Minute)
	r.LastActivityAt = ago(5 * time.Second)
	r.Health = healthWaitingWorker
	r.HealthReason = pgtype.Text{String: staleRequeuePinReason(t0.Add(time.Minute), "w1"), Valid: true}
	r.HealthSince = ago(time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, _ := pinSvc(fs, defaultHealthSettings())

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (self-clear)", n)
	}
	if w := lastWrite(t, fs, r.ID); w.Health != healthOK || w.HealthReason.Valid {
		t.Fatalf("write = (%q, %v), want cleared", w.Health, w.HealthReason)
	}
}

// Past the grace the pin rung is gone: the run falls to the fleet reasons. Leaving the pin reason
// is a NEW episode: nudge-eligible and health_since restamped.
func TestHealthStaleRequeuePinExpiryStartsNewEpisode(t *testing.T) {
	now := t0.Add(25 * time.Minute) // 26m after status_since, past the 20m grace and the 10m threshold
	prior := pgtype.Text{String: staleRequeuePinReason(t0.Add(19*time.Minute), "w1"), Valid: true}
	oldSince := ago(24 * time.Minute)

	cases := []struct {
		name       string
		notifiedAt pgtype.Timestamptz
		wantNudge  bool
	}{
		{"eligible", pgtype.Timestamptz{}, true},
		{"cooldown still applies", pgconv.Time(now.Add(-5 * time.Minute)), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := pinnedRun()
			r.Health, r.HealthReason, r.HealthSince, r.HealthNotifiedAt = healthWaitingWorker, prior, oldSince, tc.notifiedAt
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 0}
			svc, b := pinSvc(fs, defaultHealthSettings())

			svc.detectRunHealth(context.Background(), now)
			w := lastWrite(t, fs, r.ID)
			if w.HealthReason.String != reasonNoWorker {
				t.Fatalf("reason = %q, want %q after expiry", w.HealthReason.String, reasonNoWorker)
			}
			if !w.HealthSince.Time.Equal(now) {
				t.Fatalf("health_since = %v, want restamped to %v", w.HealthSince.Time, now)
			}
			if got := len(b.healthNudges) == 1 && b.healthNudges[0]; got != tc.wantNudge {
				t.Fatalf("nudge = %v, want %v", b.healthNudges, tc.wantNudge)
			}
		})
	}

	// Contrast: an ordinary reason change within waiting_worker keeps health_since and does not nudge.
	r := pinnedRun()
	r.Health, r.HealthReason, r.HealthSince = healthWaitingWorker, pgtype.Text{String: reasonWaitingWorker, Valid: true}, oldSince
	r.StaleRequeuePinnable = false
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, onlineWorkers: 0}
	svc, b := pinSvc(fs, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), now)
	w := lastWrite(t, fs, r.ID)
	if !w.HealthSince.Time.Equal(oldSince.Time) || (len(b.healthNudges) == 1 && b.healthNudges[0]) {
		t.Fatalf("ordinary reason change restamped/nudged: since=%v nudges=%v", w.HealthSince.Time, b.healthNudges)
	}
}

// A locked vault is more fundamental than the pin: the run cannot start whoever returns. Past the
// queued threshold (10m) the vault reason wins over the pin reason.
func TestHealthStaleRequeuePinVaultLockedWins(t *testing.T) {
	r := pinnedRun()
	r.StatusSince = ago(15 * time.Minute) // past the 10m threshold, inside the 20m grace
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, _ := pinSvc(fs, defaultHealthSettings())
	svc.SetVault(vault.New(newBox(t), newMemVaultStore())) // r.UserID never unlocked

	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonVaultLocked {
		t.Fatalf("reason = %q, want %q", got, reasonVaultLocked)
	}
}

// Below the queued threshold only the pin reason bypasses the age gate: a pinned run with a
// locked vault stays ok, exactly like an unpinned one, so no vault flag or nudge fires on tick 1.
func TestHealthStaleRequeuePinYoungRunWithLockedVaultStaysOK(t *testing.T) {
	r := pinnedRun() // 1m old
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, b := pinSvc(fs, defaultHealthSettings())
	svc.SetVault(vault.New(newBox(t), newMemVaultStore()))

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0; writes=%v", n, fs.writes)
	}
	if len(b.healthNudges) != 0 {
		t.Fatalf("nudged a young pinned run with a locked vault: %v", b.healthNudges)
	}
}

// The grace bound is exclusive and the ceiling bound inclusive (the SQL comparisons
// status_since > cutoff, updated_at >= affinity cutoff), at the exact instants.
func TestStaleRequeuePinBoundaryInstants(t *testing.T) {
	svc, _ := pinSvc(&healthFakeStore{}, defaultHealthSettings())

	r := pinnedRun()
	r.StatusSince = ago(0)
	graceEnd := t0.Add(pinTestGrace)
	if _, pinned := svc.staleRequeuePin(graceEnd, r); pinned {
		t.Fatal("pinned at exactly graceEnd, want released (grace bound exclusive)")
	}
	if _, pinned := svc.staleRequeuePin(graceEnd.Add(-time.Nanosecond), r); !pinned {
		t.Fatal("not pinned just before graceEnd")
	}

	// Ceiling shorter than the grace: the hold ends at updated_at + ceiling, inclusive.
	svc.p.WorkerAffinityCeiling = 5 * time.Minute
	r2 := pinnedRun()
	r2.StatusSince = ago(0)
	r2.UpdatedAt = ago(0)
	ceilingEnd := t0.Add(5 * time.Minute)
	if until, pinned := svc.staleRequeuePin(ceilingEnd, r2); !pinned || !until.Equal(ceilingEnd) {
		t.Fatalf("at exactly ceilingEnd: pinned=%v until=%v, want pinned until %v (ceiling inclusive)", pinned, until, ceilingEnd)
	}
	if _, pinned := svc.staleRequeuePin(ceilingEnd.Add(time.Nanosecond), r2); pinned {
		t.Fatal("pinned after ceilingEnd")
	}
}

// With the affinity ceiling shorter than the grace, the hold ends at the ceiling instant.
func TestHealthStaleRequeuePinCeilingShorterThanGrace(t *testing.T) {
	r := pinnedRun() // updated_at = t0-1m, so ceiling end = t0+4m with a 5m ceiling
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc, _ := pinSvc(fs, defaultHealthSettings())
	svc.p.WorkerAffinityCeiling = 5 * time.Minute

	svc.detectRunHealth(context.Background(), t0)
	got := lastWrite(t, fs, r.ID).HealthReason.String
	if !strings.HasPrefix(got, "waiting until 2026-07-12T12:04:00Z for ") {
		t.Fatalf("reason = %q, want the ceiling instant 12:04:00Z, not the grace instant", got)
	}

	// Past the ceiling but inside the grace: ClaimRun lets peers take it, so no pin explanation.
	r2 := pinnedRun()
	fs2 := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r2}}
	svc2, _ := pinSvc(fs2, defaultHealthSettings())
	svc2.p.WorkerAffinityCeiling = 5 * time.Minute
	if n := svc2.detectRunHealth(context.Background(), t0.Add(6*time.Minute)); n != 0 {
		t.Fatalf("flagged a run peers may already claim (changed=%d, writes=%v)", n, fs2.writes)
	}
}

// health_queued_seconds = 0 switches every queued flag off, the pin included; a run that is not
// pinnable (or a zero grace) is left to the ordinary age threshold.
func TestHealthStaleRequeuePinDisabledCases(t *testing.T) {
	zeroQueued := defaultHealthSettings()
	zeroQueued.queued = 0
	cases := []struct {
		name     string
		settings fakeHealthSettings
		mutate   func(*store.ListActiveRunsForHealthRow, *Service)
	}{
		{"queued threshold 0", zeroQueued, func(*store.ListActiveRunsForHealthRow, *Service) {}},
		{"not pinnable", defaultHealthSettings(), func(r *store.ListActiveRunsForHealthRow, _ *Service) { r.StaleRequeuePinnable = false }},
		{"zero grace", defaultHealthSettings(), func(_ *store.ListActiveRunsForHealthRow, s *Service) { s.p.WorkerStaleRequeueGrace = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := pinnedRun()
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc, _ := pinSvc(fs, tc.settings)
			tc.mutate(&r, svc)
			fs.active = []store.ListActiveRunsForHealthRow{r}
			if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
				t.Fatalf("changed = %d, want 0 (no flag); writes=%v", n, fs.writes)
			}
		})
	}
}

// Issues #2705 and #2501 both surface a reason below the queued threshold. A repo run that
// is pinned to its returning worker AND blocked by the Docker allowlist (one online worker,
// none eligible) reports the pin reason and skips the Docker/owner probe; once the pin
// lapses the same run reports the owner-role Docker reason.
func TestHealthStaleRequeuePinBeatsEarlyDockerAllowlist(t *testing.T) {
	for _, tc := range []struct {
		name   string
		owner  store.User
		reason string
	}{
		{"member", store.User{}, reasonRepoNotDockerAllowedMember},
		{"admin", store.User{IsAdmin: true}, reasonRepoNotDockerAllowedAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocked := func(pinned bool) (*healthFakeStore, store.ListActiveRunsForHealthRow) {
				r := pinnedRun()
				r.StaleRequeuePinnable = pinned
				r.RepoID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
				return &healthFakeStore{
					active:          []store.ListActiveRunsForHealthRow{r},
					onlineWorkers:   1,
					eligibleWorkers: 0,
					owner:           tc.owner,
				}, r
			}

			fs, r := blocked(true)
			svc, _ := pinSvc(fs, defaultHealthSettings())
			svc.SetDockerAllowlist(fakeAllowlistReader{})
			svc.detectRunHealth(context.Background(), t0)
			w := lastWrite(t, fs, r.ID)
			if w.Health != healthWaitingWorker || !strings.Contains(w.HealthReason.String, "for its previous worker w1 to return") {
				t.Fatalf("pinned: got %q/%q, want the stale-requeue pin reason", w.Health, w.HealthReason.String)
			}
			if len(fs.ownerCalls) != 0 {
				t.Fatalf("pinned: issued %d owner lookups, want 0 (the Docker probe must not run)", len(fs.ownerCalls))
			}

			fs2, r2 := blocked(false)
			svc2, _ := pinSvc(fs2, defaultHealthSettings())
			svc2.SetDockerAllowlist(fakeAllowlistReader{})
			svc2.detectRunHealth(context.Background(), t0)
			w2 := lastWrite(t, fs2, r2.ID)
			if w2.Health != healthWaitingWorker || w2.HealthReason.String != tc.reason {
				t.Fatalf("unpinned: got %q/%q, want waiting_worker/%q", w2.Health, w2.HealthReason.String, tc.reason)
			}
		})
	}
}
