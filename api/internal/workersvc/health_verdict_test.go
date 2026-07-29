package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// Issue #182: an awaiting_approval run whose owner ALREADY answered this gate is waiting
// on its worker, not on its human, and must say so.
//
// SCOPE OF THIS FILE, because the split matters for what a green run here proves: these
// are arm tests. They pin the MAPPING (lookup true → waiting_worker, false →
// approval_idle), the ARGUMENTS the lookup is issued with, and the fact that the three
// pre-existing guards short-circuit ahead of it. The predicate the lookup answers — the
// four-of-six kind list and the `>=` boundary, including the discriminating
// created_at < updated_at case — is SQL, and is pinned against a real Postgres by
// store.TestRunHasVerdictSinceGateOpenedLiveDB. Neither half is sufficient alone.

// gateRunRow is an awaiting_approval run 90m into a 1h approval threshold, i.e. past the
// guard and squarely inside the arm under test.
func gateRunRow() store.ListActiveRunsForHealthRow {
	r := runRow("awaiting_approval")
	r.UpdatedAt = ago(90 * time.Minute)
	return r
}

func gateSvc(r store.ListActiveRunsForHealthRow, answered bool) (*healthFakeStore, *Service) {
	fs := &healthFakeStore{
		active:       []store.ListActiveRunsForHealthRow{r},
		verdictSince: map[uuid.UUID]bool{r.ID: answered},
	}
	return fs, healthSvc(fs, defaultHealthSettings())
}

func TestHealthGateVerdictUndeliveredFlagsWaitingWorker(t *testing.T) {
	r := gateRunRow()
	fs, svc := gateSvc(r, true)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker {
		t.Fatalf("health = %q, want waiting_worker — the owner answered, the worker has not acted", w.Health)
	}
	if w.HealthReason.String != reasonVerdictUndelivered {
		t.Fatalf("reason = %q, want %q", w.HealthReason.String, reasonVerdictUndelivered)
	}
	// The queued arm's reason describes an UNCLAIMED run; this run's worker holds it.
	if w.HealthReason.String == reasonWaitingWorker {
		t.Fatal("reused the queued arm's reason, which describes an unclaimed run")
	}
}

func TestHealthGateNoVerdictStaysApprovalIdle(t *testing.T) {
	r := gateRunRow()
	fs, svc := gateSvc(r, false)

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthApprovalIdle || w.HealthReason.String != reasonApprovalIdle {
		t.Fatalf("got %q/%q, want approval_idle/%q — nobody has responded to this gate",
			w.Health, w.HealthReason.String, reasonApprovalIdle)
	}
}

// The lookup must ask about THIS gate. r.UpdatedAt is the episode boundary the guard
// above it just aged, so passing anything else would let the arm and its own threshold
// check describe different episodes.
func TestHealthGateVerdictLookupAsksAboutThisGate(t *testing.T) {
	r := gateRunRow()
	fs, svc := gateSvc(r, true)

	svc.detectRunHealth(context.Background(), t0)
	if len(fs.verdictCalls) != 1 {
		t.Fatalf("issued %d verdict lookups, want exactly 1", len(fs.verdictCalls))
	}
	got := fs.verdictCalls[0]
	if got.RunID != r.ID {
		t.Fatalf("looked up run %s, want %s", got.RunID, r.ID)
	}
	if !got.GateOpenedAt.Valid || !got.GateOpenedAt.Time.Equal(r.UpdatedAt.Time) {
		t.Fatalf("gate_opened_at = %v, want the run's updated_at %v (the episode boundary the threshold guard used)",
			got.GateOpenedAt, r.UpdatedAt)
	}
}

// The lookup is a sequential scan on an unbounded table (run_user_inputs' only index is
// partial on pending rows). Affordable ONLY because the three guards ahead of it reject
// nearly every run before it runs — so "no query issued" is the assertion, not "flag ok".
func TestHealthGateGuardsShortCircuitBeforeLookup(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*store.ListActiveRunsForHealthRow)
		setting fakeHealthSettings
	}{
		{
			name:    "auto_approve run",
			mutate:  func(r *store.ListActiveRunsForHealthRow) { r.AutoApprove = true },
			setting: defaultHealthSettings(),
		},
		{
			name:    "approval signal disabled",
			mutate:  func(*store.ListActiveRunsForHealthRow) {},
			setting: fakeHealthSettings{enabled: true, approval: 0},
		},
		{
			name:    "gate younger than the threshold",
			mutate:  func(r *store.ListActiveRunsForHealthRow) { r.UpdatedAt = ago(10 * time.Minute) },
			setting: defaultHealthSettings(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gateRunRow()
			tc.mutate(&r)
			// Answered, so a lookup that DID run would flip the flag — the case would
			// then fail on the flag too, not only on the call count.
			fs := &healthFakeStore{
				active:       []store.ListActiveRunsForHealthRow{r},
				verdictSince: map[uuid.UUID]bool{r.ID: true},
			}
			svc := healthSvc(fs, tc.setting)

			if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
				t.Fatalf("changed = %d, want 0 (guarded out)", n)
			}
			if len(fs.verdictCalls) != 0 {
				t.Fatalf("issued %d verdict lookups behind a guard that must short-circuit first", len(fs.verdictCalls))
			}
		})
	}
}

// No other status may pay for the lookup.
func TestHealthGateLookupOnlyInApprovalArm(t *testing.T) {
	for _, status := range []string{"queued", "running"} {
		t.Run(status, func(t *testing.T) {
			r := runRow(status)
			r.UpdatedAt = ago(90 * time.Minute)
			r.StartedAt = ago(90 * time.Minute)
			r.LastActivityAt = ago(90 * time.Minute)
			fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
			svc := healthSvc(fs, defaultHealthSettings())

			svc.detectRunHealth(context.Background(), t0)
			if len(fs.verdictCalls) != 0 {
				t.Fatalf("a %s run issued %d verdict lookups, want 0", status, len(fs.verdictCalls))
			}
		})
	}
}

// A failed read degrades to the pre-#182 behaviour rather than suppressing the flag: a
// nudge the owner already answered is noise, but silence on a gate nobody has touched
// hides the very signal this arm exists to raise.
func TestHealthGateVerdictReadErrorDegradesToApprovalIdle(t *testing.T) {
	r := gateRunRow()
	fs := &healthFakeStore{
		active:       []store.ListActiveRunsForHealthRow{r},
		verdictSince: map[uuid.UUID]bool{r.ID: true}, // would flag waiting_worker if read
		verdictErr:   errors.New("boom"),
	}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthApprovalIdle {
		t.Fatalf("health = %q, want approval_idle when the verdict read fails", w.Health)
	}
}

// ok → waiting_worker is a new episode: stamp health_since and nudge.
func TestHealthGateVerdictNudgesFromOK(t *testing.T) {
	r := gateRunRow()
	fs, svc := gateSvc(r, true)
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	svc.detectRunHealth(context.Background(), t0)
	if len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("healthNudges = %v, want [true] on the first flag of this episode", b.healthNudges)
	}
	if w := lastWrite(t, fs, r.ID); !w.HealthSince.Valid || !w.HealthSince.Time.Equal(t0) {
		t.Fatalf("health_since = %v, want now on a raised flag", w.HealthSince)
	}
}

// approval_idle → waiting_worker is an ENUM change, so health_since restamps: "nobody
// responded" and "a response is undelivered" are genuinely different episodes. The
// re-label is silent — a flagged→flagged transition never nudges (Decision 7) — which is
// the existing contract, not a gap in this fix.
func TestHealthGateApprovalIdleBecomesWaitingWorkerSilently(t *testing.T) {
	r := gateRunRow()
	r.Health = healthApprovalIdle
	r.HealthReason = pgText(reasonApprovalIdle)
	r.HealthSince = ago(30 * time.Minute)
	r.HealthNotifiedAt = ago(30 * time.Minute)
	fs, svc := gateSvc(r, true)
	b := &fakeBroadcaster{}
	svc.SetBroadcaster(b)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1 (enum change)", n)
	}
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthWaitingWorker || w.HealthReason.String != reasonVerdictUndelivered {
		t.Fatalf("got %q/%q, want waiting_worker/%q", w.Health, w.HealthReason.String, reasonVerdictUndelivered)
	}
	if !w.HealthSince.Valid || !w.HealthSince.Time.Equal(t0) {
		t.Fatalf("health_since = %v, want now — the enum changed, so this is a new episode", w.HealthSince)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("healthNudges = %v, want [false] — a flagged→flagged re-label must not DM again", b.healthNudges)
	}
}

// Monotone within an episode: once flagged waiting_worker with the verdict still standing,
// the detector re-computes the same flag and writes nothing. There is no
// waiting_worker → approval_idle edge to flap over.
func TestHealthGateWaitingWorkerIsStable(t *testing.T) {
	r := gateRunRow()
	r.Health = healthWaitingWorker
	r.HealthReason = pgText(reasonVerdictUndelivered)
	r.HealthSince = ago(20 * time.Minute)
	fs, svc := gateSvc(r, true)

	if n := svc.detectRunHealth(context.Background(), t0); n != 0 {
		t.Fatalf("changed = %d, want 0 — an unchanged flag must not re-write (and so must not restamp health_since)", n)
	}
	if len(fs.writes) != 0 {
		t.Fatalf("wrote %d rows, want 0", len(fs.writes))
	}
}
