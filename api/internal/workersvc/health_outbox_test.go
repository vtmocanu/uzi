package workersvc

// PRD #1391 M5 — the outbox arm of runningTarget's stalled branch. A run whose worker
// is holding its updates during an api outage looks silent to the health detector,
// which would read that silence as `stalled` — the exact wrong signal the outbox
// exists to prevent. These pin that a reported non-zero pending depth turns the reason
// into reasonOutboxQueued, that it reverts to reasonStalled once the depth clears, and
// that the reason is applied ONLY when the run's OWNING worker is the one that reported
// the depth (the trust-boundary owner-gate).

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestHealthStalledBecomesOutboxQueuedWhenDepthReported(t *testing.T) {
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)      // well under the near-timeout threshold
	r.LastActivityAt = ago(10 * time.Minute) // silent 10m > 5m stall
	// The run is owned by worker w — the owner-gate requires the reporting worker to be
	// this same worker.
	w := uuid.New()
	r.WorkerID = pgconv.UUID(w)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	// The OWNING worker reported a non-zero pending outbox depth for THIS run.
	svc.outbox.record(w, []OutboxEntry{
		{RunID: r.ID, PendingMessages: 4, Since: t0.Add(-time.Minute)},
	}, t0)

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("changed = %d, want 1", n)
	}
	got := lastWrite(t, fs, r.ID)
	if got.Health != healthStalled {
		t.Fatalf("health = %q, want stalled (same enum, truthful reason)", got.Health)
	}
	if !got.HealthReason.Valid || got.HealthReason.String != reasonOutboxQueued {
		t.Fatalf("reason = %q, want %q: a stalled run whose OWNING worker is holding its updates must read the queued reason, not the misleading stall",
			got.HealthReason.String, reasonOutboxQueued)
	}
}

func TestHealthOutboxQueuedTrustsOnlyTheOwningWorker(t *testing.T) {
	// TRUST BOUNDARY (F-A): a run owned by worker B, with an outbox entry reported by a
	// DIFFERENT worker A for that run, must NOT get reasonOutboxQueued — it stays on the
	// honest stalled reason. On the owner-blind (unfixed) code this test fails, because
	// runIndex is "last reporter wins" and any worker could flip a victim's reason. The
	// second half proves the SAME run reported by its owner B DOES get the queued reason,
	// so the discriminator is ownership, not the presence of any report.
	base := runRow("running")
	base.StartedAt = ago(20 * time.Minute)
	base.LastActivityAt = ago(10 * time.Minute)
	ownerB := uuid.New()
	base.WorkerID = pgconv.UUID(ownerB)

	// Reported by a foreign worker A (not the owner): stays stalled.
	rForeign := base
	fsForeign := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{rForeign}}
	svcForeign := healthSvc(fsForeign, defaultHealthSettings())
	workerA := uuid.New()
	if workerA == ownerB {
		t.Fatal("test fixture generated identical worker ids")
	}
	svcForeign.outbox.record(workerA, []OutboxEntry{
		{RunID: rForeign.ID, PendingMessages: 4, Since: t0.Add(-time.Minute)},
	}, t0)
	svcForeign.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fsForeign, rForeign.ID).HealthReason.String; got != reasonStalled {
		t.Fatalf("cross-worker report reason = %q, want %q: a non-owning worker must not flip the reason to queued", got, reasonStalled)
	}

	// Reported by the OWNING worker B: becomes queued.
	rOwned := base
	fsOwned := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{rOwned}}
	svcOwned := healthSvc(fsOwned, defaultHealthSettings())
	svcOwned.outbox.record(ownerB, []OutboxEntry{
		{RunID: rOwned.ID, PendingMessages: 4, Since: t0.Add(-time.Minute)},
	}, t0)
	svcOwned.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fsOwned, rOwned.ID).HealthReason.String; got != reasonOutboxQueued {
		t.Fatalf("owner-reported reason = %q, want %q: the run's own worker holding its updates must read as queued", got, reasonOutboxQueued)
	}
}

func TestHealthOutboxQueuedIgnoredForUnclaimedRun(t *testing.T) {
	// A running run with a NULL worker_id (no current owner) can never satisfy the
	// owner-gate, so a reported depth is ignored and the run stays on the honest stall.
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	// r.WorkerID left zero-value (Valid == false).
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.outbox.record(uuid.New(), []OutboxEntry{
		{RunID: r.ID, PendingMessages: 4, Since: t0.Add(-time.Minute)},
	}, t0)

	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonStalled {
		t.Fatalf("reason = %q, want %q: a run with no owning worker cannot be owner-gated to queued", got, reasonStalled)
	}
}

func TestHealthStalledStaysStalledWithoutOutboxDepth(t *testing.T) {
	// Control: the SAME stalled fixture with no reported depth keeps reasonStalled, so
	// the test above proves the outbox arm, not the stall arm.
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	r.WorkerID = pgconv.UUID(uuid.New())
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())

	svc.detectRunHealth(context.Background(), t0)
	w := lastWrite(t, fs, r.ID)
	if w.Health != healthStalled || w.HealthReason.String != reasonStalled {
		t.Fatalf("health=%q reason=%q, want stalled/%q", w.Health, w.HealthReason.String, reasonStalled)
	}
}

func TestHealthStalledIgnoresZeroPendingOutboxDepth(t *testing.T) {
	// A tracked entry with ZERO pending messages (e.g. only stale_retired counted) is
	// not "queued": the arm keys on PendingMessages > 0, so a zero-pending run stays on
	// the plain stalled reason. Owned by the reporting worker so the discriminator is the
	// zero pending count, not the owner-gate.
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	w := uuid.New()
	r.WorkerID = pgconv.UUID(w)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.outbox.record(w, []OutboxEntry{
		{RunID: r.ID, PendingMessages: 0, StaleRetired: 3, Since: t0.Add(-time.Minute)},
	}, t0)

	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonStalled {
		t.Fatalf("reason = %q, want %q: zero pending is not queued", got, reasonStalled)
	}
}

func TestHealthOutboxQueuedRevertsToStalledWhenDrained(t *testing.T) {
	// Depth > 0 → queued; the backlog drains (an empty report clears the worker's set)
	// → normal stalled detection resumes on the next tick.
	r := runRow("running")
	r.StartedAt = ago(20 * time.Minute)
	r.LastActivityAt = ago(10 * time.Minute)
	w := uuid.New()
	r.WorkerID = pgconv.UUID(w)
	fs := &healthFakeStore{active: []store.ListActiveRunsForHealthRow{r}}
	svc := healthSvc(fs, defaultHealthSettings())
	svc.outbox.record(w, []OutboxEntry{{RunID: r.ID, PendingMessages: 4, Since: t0.Add(-time.Minute)}}, t0)

	svc.detectRunHealth(context.Background(), t0)
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonOutboxQueued {
		t.Fatalf("precondition reason = %q, want %q", got, reasonOutboxQueued)
	}

	// Drain: an empty report clears the worker's set. Stage the stored row as the
	// queued reason so the clear pass has a change to write.
	svc.outbox.record(w, nil, t0)
	fs.writes = nil
	r.Health = healthStalled
	r.HealthReason = pgconv.TextOrNull(reasonOutboxQueued)
	fs.active = []store.ListActiveRunsForHealthRow{r}

	if n := svc.detectRunHealth(context.Background(), t0); n != 1 {
		t.Fatalf("clear pass changed = %d, want 1 (queued → stalled)", n)
	}
	if got := lastWrite(t, fs, r.ID).HealthReason.String; got != reasonStalled {
		t.Fatalf("after drain reason = %q, want %q: with the depth cleared, normal stalled detection resumes", got, reasonStalled)
	}
}
