package workersvc

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4257: publication_withheld is a running-run health arm below every liveness arm and
// above the final near-timeout backstop. The row's PublicationWithheldSince is the fenced
// projection the SQL read already applied, so these cases pin only the arm's precedence.

func pwSince() time.Time { return t0.Add(-20 * time.Minute) }

// pwRun is a running run with a projected withheld fact, last active a minute ago.
func pwRun() store.ListActiveRunsForHealthRow {
	r := runRow("running")
	r.Kind = "issue"
	r.StartedAt = ago(30 * time.Minute)
	r.LastActivityAt = ago(time.Minute)
	r.PublicationWithheldSince = pgconv.Time(pwSince())
	return r
}

func pwDetect(t *testing.T, fs *healthFakeStore, st fakeHealthSettings) (string, string) {
	t.Helper()
	svc := healthSvc(fs, st)
	svc.detectRunHealth(context.Background(), t0)
	r := fs.active[0]
	if len(fs.writes) == 0 {
		return r.Health, r.HealthReason.String
	}
	w := lastWrite(t, fs, r.ID)
	return w.Health, w.HealthReason.String
}

func TestHealthPublicationWithheldFlagsWithFixedReason(t *testing.T) {
	r := pwRun()
	fs, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	wantHealth(t, "withheld", w.Health, w.HealthReason.String, healthPublicationWithheld, apitypes.ReasonCheckpointPublicationWithheld)
	// health_since is the clamped worker since, not the detector's first tick.
	if !w.HealthSince.Valid || !w.HealthSince.Time.Equal(pwSince()) {
		t.Fatalf("health_since = %+v, want the worker's since %v", w.HealthSince, pwSince())
	}
	// Slack is out of scope: the state is written and broadcast, never nudged or stamped.
	if w.HealthNotifiedAt.Valid || len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("nudged or stamped: write=%+v nudges=%v", w, b.healthNudges)
	}
	if len(b.healths) != 1 || b.healths[0] != healthPublicationWithheld {
		t.Fatalf("broadcast healths = %v", b.healths)
	}
}

func TestHealthPublicationWithheldOlderWorkerKeepsToday(t *testing.T) {
	r := pwRun()
	r.PublicationWithheldSince = pgtype.Timestamptz{}
	h, why := pwDetect(t, &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}, defaultHealthSettings())
	wantHealth(t, "no fact", h, why, healthOK, "")
}

func TestHealthPublicationWithheldPrecedence(t *testing.T) {
	// stalled beats withheld, and withheld returns once the run is active again.
	r := pwRun()
	r.LastActivityAt = ago(10 * time.Minute)
	h, why := pwDetect(t, &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}, defaultHealthSettings())
	wantHealth(t, "stalled", h, why, healthStalled, reasonStalled)
	r.LastActivityAt = ago(time.Minute)
	h, why = pwDetect(t, &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}, defaultHealthSettings())
	wantHealth(t, "active again", h, why, healthPublicationWithheld, apitypes.ReasonCheckpointPublicationWithheld)

	// a long tool call and a silent delegation both beat withheld.
	r = pwRun()
	r.LastActivityAt = ago(25 * time.Minute)
	long := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r},
		messages: map[uuid.UUID][]fakeRunMessage{r.ID: {lcUse(t, 1, "a", "Bash", 25*time.Minute)}}}
	h, why = pwDetect(t, long, longCallSettings())
	wantHealth(t, "long tool call", h, why, healthStalled, reasonLongToolCall)
	deleg := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r},
		messages: map[uuid.UUID][]fakeRunMessage{r.ID: {lcUse(t, 1, "d", "Agent", 25*time.Minute)}}}
	h, why = pwDetect(t, deleg, longCallSettings())
	wantHealth(t, "silent delegation", h, why, healthStalled, reasonSilentDelegation)

	// a live plan cross-check wait beats withheld; withheld returns after it settles.
	r = pwRun()
	plan := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}, liveCrossCheck: map[uuid.UUID]bool{r.ID: true}}
	h, why = pwDetect(t, plan, defaultHealthSettings())
	wantHealth(t, "plan cross-check", h, why, healthWaitingWorker, reasonPlanCrossCheckWaiting)
	plan.liveCrossCheck = map[uuid.UUID]bool{}
	plan.writes = nil
	h, why = pwDetect(t, plan, defaultHealthSettings())
	wantHealth(t, "plan cross-check settled", h, why, healthPublicationWithheld, apitypes.ReasonCheckpointPublicationWithheld)

	// a live code cross-check wait beats withheld; withheld returns after it ends.
	code := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r},
		liveCodeCrossCheckDeadlines: map[uuid.UUID]pgtype.Timestamptz{r.ID: pgconv.Time(t0.Add(7 * time.Minute))}}
	h, why = pwDetect(t, code, defaultHealthSettings())
	wantHealth(t, "code cross-check", h, why, healthWaitingWorker, "waiting until 2026-07-12T12:07:00Z for code cross-check")
	code.liveCodeCrossCheckDeadlines = nil
	code.writes = nil
	h, why = pwDetect(t, code, defaultHealthSettings())
	wantHealth(t, "code cross-check ended", h, why, healthPublicationWithheld, apitypes.ReasonCheckpointPublicationWithheld)
}

func TestHealthPublicationWithheldBeatsFinalNearTimeout(t *testing.T) {
	r := pwRun()
	r.StartedAt = ago(95 * time.Second)
	r.BudgetWallSeconds = pgtype.Int4{Int32: 100, Valid: true}
	h, why := pwDetect(t, &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}, defaultHealthSettings())
	wantHealth(t, "near timeout with a fact", h, why, healthPublicationWithheld, apitypes.ReasonCheckpointPublicationWithheld)
	r.PublicationWithheldSince = pgtype.Timestamptz{}
	h, why = pwDetect(t, &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}, defaultHealthSettings())
	wantHealth(t, "near timeout without a fact", h, why, healthSlow, reasonNearTimeout)
}

func TestHealthPublicationWithheldAccountCooldownKeepsExistingResult(t *testing.T) {
	r := pwRun()
	r.AccountCooldownClass = pgconv.TextOrNull("rate_limit")
	r.AccountCooldownEligibleAt = pgconv.Time(t0.Add(time.Minute))
	h, why := pwDetect(t, &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}, defaultHealthSettings())
	wantHealth(t, "cooldown with a fact", h, why, healthWaitingAccountCooldown, apitypes.AccountCooldownReason)
	// wall exhaustion stays actionable while the cooldown waits.
	r.Kind = "issue"
	r.StartedAt = ago(101 * time.Second)
	r.BudgetWallSeconds = pgtype.Int4{Int32: 100, Valid: true}
	h, why = pwDetect(t, &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}, defaultHealthSettings())
	wantHealth(t, "cooldown wall exhaustion with a fact", h, why, healthSlow, reasonNearTimeout)
}

func TestHealthPublicationWithheldEpisodeAndExit(t *testing.T) {
	// Already flagged with the same reason: nothing to write.
	r := pwRun()
	r.Health = healthPublicationWithheld
	r.HealthReason = pgconv.Text(apitypes.ReasonCheckpointPublicationWithheld)
	r.HealthSince = pgconv.Time(pwSince())
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	if n := healthSvc(fs, defaultHealthSettings()).detectRunHealth(context.Background(), t0); n != 0 || len(fs.writes) != 0 {
		t.Fatalf("unchanged flag wrote: n=%d writes=%d", n, len(fs.writes))
	}
	// The fact clears (or is fenced out): the flag returns to ok.
	r.PublicationWithheldSince = pgtype.Timestamptz{}
	fs = &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	healthSvc(fs, defaultHealthSettings()).detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthOK || w.HealthReason.Valid || w.HealthSince.Valid {
		t.Fatalf("exit write = %+v", w)
	}
	// Entering from another flag takes the worker's since rather than the tick time.
	r = pwRun()
	r.Health = healthStalled
	r.HealthReason = pgconv.Text(reasonStalled)
	r.HealthSince = ago(time.Hour)
	fs = &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	healthSvc(fs, defaultHealthSettings()).detectRunHealth(context.Background(), t0)
	w = lastWrite(t, fs, r.ID)
	if w.Health != healthPublicationWithheld || !w.HealthSince.Time.Equal(pwSince()) {
		t.Fatalf("stalled to withheld write = %+v", w)
	}
}

// pwLeaving is a run stored as withheld that now reads stalled (silent for 10 minutes, fact present).
func pwLeaving(notified pgtype.Timestamptz) store.ListActiveRunsForHealthRow {
	r := pwRun()
	r.Health = healthPublicationWithheld
	r.HealthReason = pgconv.Text(apitypes.ReasonCheckpointPublicationWithheld)
	r.HealthSince = pgconv.Time(pwSince())
	r.LastActivityAt = ago(10 * time.Minute)
	r.HealthNotifiedAt = notified
	return r
}

func TestHealthPublicationWithheldThenStalledNudges(t *testing.T) {
	// Entering withheld from ok never nudges; a later fault out of withheld is a new episode and does.
	r := pwRun()
	_, svc, b := nudgeSvc(t, r, defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	if len(b.healthNudges) != 1 || b.healthNudges[0] {
		t.Fatalf("ok to withheld nudged: %v", b.healthNudges)
	}
	fs, svc, b := nudgeSvc(t, pwLeaving(pgtype.Timestamptz{}), defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, fs.active[0].ID)
	wantHealth(t, "stalled", w.Health, w.HealthReason.String, healthStalled, reasonStalled)
	if len(b.healthNudges) != 1 || !b.healthNudges[0] {
		t.Fatalf("withheld to stalled did not nudge: %v", b.healthNudges)
	}
	if !w.HealthNotifiedAt.Valid || !w.HealthNotifiedAt.Time.Equal(t0) {
		t.Fatalf("health_notified_at = %v, want %v", w.HealthNotifiedAt, t0)
	}
}

func TestHealthPublicationWithheldThenStalledHonoursCooldown(t *testing.T) {
	fs, svc, b := nudgeSvc(t, pwLeaving(ago(10*time.Minute)), defaultHealthSettings())
	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, fs.active[0].ID)
	if w.Health != healthStalled {
		t.Fatalf("health = %q, want stalled", w.Health)
	}
	if len(b.healthNudges) != 1 || b.healthNudges[0] || w.HealthNotifiedAt.Valid {
		t.Fatalf("nudged inside the cooldown: nudges=%v notified=%v", b.healthNudges, w.HealthNotifiedAt)
	}
}

func TestHealthPublicationWithheldFromCooldownKeepsProjectedSince(t *testing.T) {
	// The cooldown ended while the fact stays present: the run moves into withheld and the pill
	// ages from the worker's since, not from the restamp a cooldown exit normally gets.
	r := pwRun()
	r.Health = healthWaitingAccountCooldown
	r.HealthReason = pgconv.Text(apitypes.AccountCooldownReason)
	r.HealthSince = ago(5 * time.Minute)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	healthSvc(fs, defaultHealthSettings()).detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthPublicationWithheld || !w.HealthSince.Valid || !w.HealthSince.Time.Equal(pwSince()) {
		t.Fatalf("cooldown to withheld write = %+v, want since %v", w, pwSince())
	}
}

func TestHealthPublicationWithheldStaleSinceIsRewritten(t *testing.T) {
	// The fact was cleared and re-withheld with a fresh since inside one tick: same enum and
	// reason, but the stored health_since is stale, so the write must not be skipped.
	r := pwRun()
	r.Health = healthPublicationWithheld
	r.HealthReason = pgconv.Text(apitypes.ReasonCheckpointPublicationWithheld)
	r.HealthSince = ago(time.Hour)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	if n := healthSvc(fs, defaultHealthSettings()).detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("stale since was skipped: n=%d", n)
	}
	w := lastWrite(t, fs, r.ID)
	if !w.HealthSince.Time.Equal(pwSince()) {
		t.Fatalf("since = %+v, want %v", w.HealthSince, pwSince())
	}
}
