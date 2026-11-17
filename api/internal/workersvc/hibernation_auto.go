package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Automatic hibernation of idle workers (issue #4207).
//
// HibernationAutoPass lists persistent hosted workers that have shown no run activity for the
// configured threshold, then admits each through the same locked admission as the manual
// route. The candidate list is only a hint read without locks: every worker is re-checked on
// its locked row, including idleness, so a claim or a run finish that lands between the list
// and the lock wins.

const (
	// HibernationAutoPassEvery is the minimum gap between two automatic passes that ran.
	HibernationAutoPassEvery = time.Minute
	// HibernationAutoAdmissionsPerPass bounds the workers one pass lists, and so admits.
	HibernationAutoAdmissionsPerPass = 10
	// HibernationAutoRefusalCooldown is how long a worker whose controller refused the
	// hibernation is left out of the candidate list.
	HibernationAutoRefusalCooldown = time.Hour
)

// Blocked reasons recorded on workers.hibernation_blocked_reason when the automatic pass could
// not admit a worker. They are a closed set (HibernationBlockedReasons); the first group are
// the hibernateRefusal codes that the pass records as they are, the second group refine
// RefusalIntakeFenced from the locked row, and BlockedControllerRefused is written by
// ApplyTransition when the controller declines before scaling down.
const (
	BlockedNoPodIdentity          = RefusalNoPodIdentity
	BlockedControllerNotAdvertise = RefusalControllerNotAdvertise
	BlockedCustodyHold            = RefusalCustodyHold
	BlockedPendingWakeDemand      = RefusalPendingWakeDemand
	BlockedIntakeFenced           = RefusalIntakeFenced

	BlockedManualCordon = "manual_cordon"
	BlockedMaintenance  = "maintenance"
	BlockedRoll         = "roll"
	BlockedDraining     = "draining"

	BlockedControllerRefused = "controller_refused"
)

// HibernationBlockedReasons returns the closed set of values hibernation_blocked_reason can hold.
func HibernationBlockedReasons() []string {
	return []string{
		BlockedNoPodIdentity, BlockedControllerNotAdvertise, BlockedCustodyHold, BlockedPendingWakeDemand,
		BlockedIntakeFenced, BlockedManualCordon, BlockedMaintenance, BlockedRoll, BlockedDraining,
		BlockedControllerRefused,
	}
}

// refineIntakeFenced names why store.WorkerAcceptsNewIntake is false for the locked row.
func refineIntakeFenced(w store.Worker) string {
	switch {
	case w.ManualCordonedAt.Valid:
		return BlockedManualCordon
	case underMaintenance(w.MaintenanceFenced, w.MaintenancePhase):
		return BlockedMaintenance
	case w.DrainingSince.Valid && w.DrainingReason.Valid && w.DrainingReason.String == "image_roll":
		return BlockedRoll
	case w.DrainingSince.Valid:
		return BlockedDraining
	}
	return BlockedIntakeFenced
}

// idleSpan renders a threshold in the largest whole unit (days, then hours), else as a Go
// duration. It is the unit logic shared by every automatic hibernation reason.
func idleSpan(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int64(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int64(d/time.Hour))
	}
	return d.String()
}

// autoHibernationReason is the hibernation_reason recorded for an automatic admission.
func autoHibernationReason(idleAfter time.Duration) string {
	return "automatic: no run activity for " + idleSpan(idleAfter)
}

// HibernationAutoPass is the sweeper pass that hibernates idle workers. It does nothing unless
// both worker_hibernation_enabled and worker_hibernation_auto are on, and runs at most once per
// HibernationAutoPassEvery. Each worker is its own transaction; one failure never blocks its
// siblings and the errors are joined. The result counts workers admitted (with surplus mode on,
// state changes: admissions and warm wakes).
func (s *Service) HibernationAutoPass(ctx context.Context) (int64, error) {
	if s.txBeginner == nil || s.hibernationAuto == nil {
		return 0, nil
	}
	on, err := s.hibernationAuto.WorkerHibernationEnabled(ctx)
	if err != nil || !on {
		return 0, err
	}
	auto, err := s.hibernationAuto.WorkerHibernationAuto(ctx)
	if err != nil || !auto {
		return 0, err
	}
	idleAfter, err := s.hibernationAuto.WorkerHibernationIdleAfter(ctx)
	if err != nil {
		return 0, err
	}
	// Surplus mode (issue #4395) is effective only while both switches above are on. With it
	// off every decision below is exactly the PRD #4207 pass.
	surplus, err := s.hibernationAuto.WorkerHibernationSurplus(ctx)
	if err != nil {
		return 0, err
	}
	var surplusAfter time.Duration
	if surplus {
		if surplusAfter, err = s.hibernationAuto.WorkerHibernationSurplusIdleAfter(ctx); err != nil {
			return 0, err
		}
	}

	now := s.now()
	s.autoPassMu.Lock()
	if gap := now.Sub(s.lastAutoPass); !s.lastAutoPass.IsZero() && gap >= 0 && gap < HibernationAutoPassEvery {
		s.autoPassMu.Unlock()
		return 0, nil
	}
	s.lastAutoPass = now
	s.autoPassMu.Unlock()

	cutoff := now.Add(-idleAfter)
	recentCutoff := now.Add(-HibernationAutoRefusalCooldown)
	if surplus {
		threshold := surplusThreshold(surplusAfter, idleAfter)
		return s.hibernationSurplusPass(ctx, surplusPassParams{
			now: now, idleAfter: idleAfter, threshold: threshold,
			surplusCutoff: now.Add(-threshold), longCutoff: cutoff, recentCutoff: recentCutoff,
		})
	}
	candidates, err := s.listAutoHibernationCandidates(ctx, cutoff, recentCutoff)
	if err != nil {
		return 0, err
	}
	if s.autoPassAfterList != nil {
		s.autoPassAfterList()
	}

	var admitted int64
	var errs []error
	for _, c := range candidates {
		var did, stop bool
		err := s.hibernationTx(ctx, c.ID, func(q *store.Queries, w store.Worker) error {
			var e error
			did, stop, e = s.autoAdmit(ctx, q, w, now, cutoff, recentCutoff, idleAfter)
			return e
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) { // the worker was deleted since the list
				errs = append(errs, fmt.Errorf("worker %s: %w", c.ID, err))
			}
			continue
		}
		if did {
			admitted++
			slog.Info("worker hibernation: automatic admission", "worker_id", c.ID, "idle_since", c.Anchor.Time)
		}
		if stop {
			break
		}
	}
	if len(errs) > 0 {
		slog.Warn("worker hibernation auto pass", "errors", len(errs), "first", errs[0])
		return admitted, errors.Join(errs...)
	}
	return admitted, nil
}

func (s *Service) listAutoHibernationCandidates(ctx context.Context, cutoff, recentCutoff time.Time) ([]store.ListAutoHibernationCandidatesRow, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return store.New(tx).ListAutoHibernationCandidates(ctx, store.ListAutoHibernationCandidatesParams{
		Cutoff: pgconv.Time(cutoff), RecentCutoff: pgconv.Time(recentCutoff), MaxRows: HibernationAutoAdmissionsPerPass,
	})
}

// autoAdmit decides one candidate on its locked row. It reports whether the worker was admitted
// and whether the whole pass should stop (the setting was switched off). A candidate that is no
// longer idle, or no longer eligible, is skipped with no write at all.
func (s *Service) autoAdmit(ctx context.Context, q *store.Queries, w store.Worker, now, cutoff, recentCutoff time.Time, idleAfter time.Duration) (admitted, stop bool, err error) {
	if w.Kind != "hosted" || w.Ephemeral || w.HibernationState != "none" {
		return false, false, nil
	}
	if w.HibernationBlockedReason.Valid && w.HibernationBlockedReason.String == BlockedControllerRefused &&
		w.HibernationBlockedAt.Valid && w.HibernationBlockedAt.Time.After(recentCutoff) {
		return false, false, nil
	}
	// Re-check idleness after the lock, in one statement (see GetWorkerIdleness). A run assigned
	// or finished since the list shows up here; a claim that raced us is serialized by the lock.
	idle, err := q.GetWorkerIdleness(ctx, w.ID)
	if err != nil {
		return false, false, err
	}
	if idle.Nonterminal > 0 || !idle.Anchor.Valid || idle.Anchor.Time.After(cutoff) {
		return false, false, nil
	}
	return s.admitAutomatic(ctx, q, w, now, autoHibernationReason(idleAfter), false)
}

// admitAutomatic is the shared tail of an automatic admission on a locked row that was judged
// idle (and, for a surplus admission, surplus): it re-reads the settings, runs the unchanged
// refusal path and either admits with reason or records the blocker. stop reports that a setting
// was switched off, which ends the pass.
func (s *Service) admitAutomatic(ctx context.Context, q *store.Queries, w store.Worker, now time.Time, reason string, surplus bool) (admitted, stop bool, err error) {
	// Automatic mode may have changed since enumeration or while the row lock was pending.
	// The manual admission check below re-reads only the separate main hibernation switch.
	auto, err := s.hibernationAuto.WorkerHibernationAuto(ctx)
	if err != nil {
		return false, false, err
	}
	if !auto {
		return false, true, nil
	}
	if surplus {
		on, err := s.hibernationAuto.WorkerHibernationSurplus(ctx)
		if err != nil {
			return false, false, err
		}
		if !on {
			return false, true, nil
		}
	}
	code, err := s.hibernateRefusal(ctx, q, w, now)
	if err != nil {
		return false, false, err
	}
	switch code {
	case "":
		origin := AdmitOriginAutoLong
		if surplus {
			origin = AdmitOriginAutoSurplus
		}
		admitHibernation(&w, now, reason, origin)
		if _, err := writeHibernation(ctx, q, w); err != nil {
			return false, false, err
		}
		return true, false, nil
	case RefusalSettingOff:
		return false, true, nil
	case RefusalAlreadyActive, RefusalNotPersistentHosted, RefusalBusy:
		return false, false, nil
	case RefusalIntakeFenced:
		code = refineIntakeFenced(w)
	}
	// Commit the blocked marker: returning the refusal as an error would roll it back.
	err = q.SetWorkerHibernationBlocked(ctx, store.SetWorkerHibernationBlockedParams{ID: w.ID, Reason: code, At: pgconv.Time(now)})
	return false, false, err
}
