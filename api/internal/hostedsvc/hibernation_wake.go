package hostedsvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// WakeService is the narrow workersvc surface the wake pass drives. *workersvc.Service satisfies
// it; the methods own the transactions and the worker row locks.
type WakeService interface {
	DemandWake(ctx context.Context, runID, workerID uuid.UUID) (workersvc.DemandWakeResult, error)
	ReleaseStaleWake(ctx context.Context, runID uuid.UUID) (bool, error)
}

// WakeSettings is the settings view the wake pass reads: the Docker repository scope that
// ClaimRun's fn_worker_can_claim applies. *settings.Cache satisfies it.
type WakeSettings interface {
	DockerRepoAllowlist(ctx context.Context) ([]uuid.UUID, error)
}

// HibernationWakerConfig carries the claim-side knobs the demand query mirrors. They must equal
// the worker claim configuration, exactly as EphemeralConfig's counterparts do.
type HibernationWakerConfig struct {
	// WorkerAffinityGrace is the cross-check affinity grace (EphemeralConfig.WorkerAffinityGrace).
	WorkerAffinityGrace time.Duration
	// WorkerStaleRequeueGrace is ClaimRun's stale-requeue pin window (#2705).
	WorkerStaleRequeueGrace time.Duration
	// WorkerAffinityCeiling is ClaimRun's affinity ceiling.
	WorkerAffinityCeiling time.Duration
	// WorkerHeartbeatStale is the heartbeat freshness ClaimRun's pin hold uses.
	WorkerHeartbeatStale time.Duration
	// BackgroundGrace mirrors the ordering input of the ephemeral trigger.
	BackgroundGrace time.Duration
	// Lease is the configured ephemeral lease (EphemeralConfig.Lease, UZI_EPHEMERAL_LEASE): a run an
	// online leased ephemeral worker can take through its lease is not demand. Zero disables it.
	Lease time.Duration
}

// HibernationWaker is the demand-wake sweeper pass (PRD #4205 M5). It runs before ephemeral
// provisioning on the same tick, regardless of ephemeral_workers_enabled and
// worker_hibernation_enabled: waking a sleeper never depends on either setting (D7).
type HibernationWaker struct {
	q        *store.Queries
	svc      WakeService
	settings WakeSettings
	cfg      HibernationWakerConfig
	now      func() time.Time
}

// NewHibernationWaker wires the pass.
func NewHibernationWaker(q *store.Queries, svc WakeService, s WakeSettings, cfg HibernationWakerConfig) *HibernationWaker {
	return &HibernationWaker{q: q, svc: svc, settings: s, cfg: cfg, now: time.Now}
}

// WakePass is one tick. It first releases stale wake rows (run no longer queued), then lists the runs that demand a sleeping worker and records one wake per
// run through DemandWake. Demand wakes are not paced; the scan is bounded by ephemeralProvisionBatch
// (a run beyond it is picked up by a later pass, since a woken run leaves the set). One run's
// failure, including a commit of unknown outcome, is collected and never retried in this pass,
// and never blocks the other runs. It returns the number of workers it actually moved to waking; a run attached to an already-waking worker is logged, not counted.
func (w *HibernationWaker) WakePass(ctx context.Context) (int64, error) {
	var errs []error

	stale, err := w.q.ListStaleHibernationWakes(ctx, ephemeralProvisionBatch)
	if err != nil {
		errs = append(errs, fmt.Errorf("hostedsvc: list stale hibernation wakes: %w", err))
	}
	for _, row := range stale {
		if _, rerr := w.svc.ReleaseStaleWake(ctx, row.RunID); rerr != nil {
			errs = append(errs, fmt.Errorf("hostedsvc: release stale wake for run %s: %w", row.RunID, rerr))
		}
	}

	allowlist, aerr := w.settings.DockerRepoAllowlist(ctx)
	if aerr != nil {
		slog.Warn("hibernation waker: read Docker allowlist; continuing without Docker repos", "error", aerr)
	}
	if allowlist == nil {
		allowlist = []uuid.UUID{}
	}
	evaluatedAt := w.now()
	demand, err := w.q.ListHibernationWakeDemand(ctx, store.ListHibernationWakeDemandParams{
		CrossCheckEvaluatedAt:    pgconv.Time(evaluatedAt),
		CrossCheckAffinityCutoff: pgconv.Time(evaluatedAt.Add(-w.cfg.WorkerAffinityGrace)),
		CodexCuratedModels:       workersvc.CodexCuratedModels(),
		AffinityCutoff:           pgconv.Time(evaluatedAt.Add(-w.cfg.WorkerAffinityCeiling)),
		DockerRepoAllowlist:      allowlist,
		HeartbeatCutoff:          pgconv.Time(evaluatedAt.Add(-w.cfg.WorkerHeartbeatStale)),
		StaleRequeueCutoff:       workersvc.StaleRequeueCutoff(evaluatedAt, w.cfg.WorkerStaleRequeueGrace),
		EphemeralLease:           workersvc.LeaseInterval(w.cfg.Lease),
		BackgroundGraceCutoff:    pgconv.Time(evaluatedAt.Add(-w.cfg.BackgroundGrace)),
		MaxRows:                  ephemeralProvisionBatch,
	})
	if err != nil {
		errs = append(errs, fmt.Errorf("hostedsvc: list hibernation wake demand: %w", err))
		return 0, errors.Join(errs...)
	}

	var woke int64
	for _, d := range demand {
		res, derr := w.svc.DemandWake(ctx, d.RunID, d.WorkerID)
		if derr != nil {
			errs = append(errs, fmt.Errorf("hostedsvc: demand wake run %s worker %s: %w", d.RunID, d.WorkerID, derr))
			continue
		}
		switch res {
		case workersvc.DemandWakeWoke:
			woke++
			slog.Info("hibernation waker: woke worker for queued run", "run_id", d.RunID, "worker_id", d.WorkerID)
		case workersvc.DemandWakeAttached:
			slog.Info("hibernation waker: attached queued run to an already-waking worker", "run_id", d.RunID, "worker_id", d.WorkerID)
		}
	}
	return woke, errors.Join(errs...)
}

// wakeSuppressesProvision reports whether run runID's recorded demand wake still covers it, so
// ephemeral provisioning should skip it (PRD #4205 D9). It reads through qtx, inside
// provisionOne's advisory-locked transaction.
func (p *EphemeralProvisioner) wakeSuppressesProvision(ctx context.Context, qtx *store.Queries, runID uuid.UUID) (bool, error) {
	evaluatedAt := p.now()
	return qtx.HibernationWakeSuppresses(ctx, store.HibernationWakeSuppressesParams{
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: evaluatedAt, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: evaluatedAt.Add(-p.cfg.WorkerAffinityGrace), Valid: true},
		CodexCuratedModels:       workersvc.CodexCuratedModels(),
		RunID:                    runID,
		// The lease cannot change the result: the waking worker is always persistent, and the lease only
		// admits ephemeral workers. Passed for consistency with the demand query.
		EphemeralLease: workersvc.LeaseInterval(p.cfg.Lease),
		WakeTimeout:    durationToInterval(workersvc.WorkerWakeTimeout),
	})
}
