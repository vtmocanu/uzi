package workersvc

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// WorkerNameForRun returns the display name of a run's worker for the run-detail DTO (PRD #1497
// M1), nil when the run has no worker (unclaimed, or a server-side park nulled worker_id) or the
// worker row is gone. It goes through the service's store so the run-detail handler need not hold a
// query surface of its own. Best-effort: the caller logs a lookup error and leaves worker_name null.
func (s *Service) WorkerNameForRun(ctx context.Context, run store.Run) (*string, error) {
	if !run.WorkerID.Valid {
		return nil, nil
	}
	wkr, err := s.q.GetWorkerByID(ctx, uuid.UUID(run.WorkerID.Bytes))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	name := wkr.Name
	return &name, nil
}

// ReportWallPark is the service seam for the worker's `wall_park` report (PRD #1497 M1, D4/D15). It
// parks an owned run at its wall-clock deadline (running -> paused, hold_reason='budget_exhausted')
// via the fenced SetRunWallPark store transition, and returns SetRunCompletionHold's exact
// (run, applied, err) shape so the handler reuses the park-order ack contract (paused = clean up,
// anything else = retain live / restart).
//
// THE DEADLINE IS THE SOLE AUTHORITY (D15): SetRunWallPark admits a past-deadline row only, so an
// owner who extended in the request-then-park window leaves the row no longer past its (three-term)
// deadline; the guard matches 0 rows, applied is false, and the worker restarts the turn on the
// lifted wall. completion_attempts = 0 is the D14 backstop (the completion hold owns a post-attempt
// row). The captured head is NUL-stripped then trimmed (worker-authored text) and stored NULL when
// empty (the degraded park), matching the completion-hold path.
//
// IDEMPOTENCY (D16): when the guard rejects because the SERVER already parked the row
// (status paused, hold_reason='budget_exhausted' — e.g. ParkRunsAtWall beat the worker, or a
// re-delivered report), the report is answered as parked (applied=true). When such a report carries
// a captured head the server park recorded none, RecordWallParkCapturedHead keeps it (fenced on the
// generation, deliberately NOT on claim_released_at, so it admits exactly the flight the server
// parked). A genuinely reclaimed run surfaces as ErrRunNotOwned -> the handler's 404.
func (s *Service) ReportWallPark(ctx context.Context, wkr store.Worker, runID uuid.UUID, capturedHead string, published bool, claimGen *int64) (store.Run, bool, error) {
	_ = published // informational (the degraded-park feed message, M2); the store transition ignores it.
	// NUL-strip BEFORE the trim (a NUL is not whitespace), the same order every worker-authored text
	// field uses; hold_captured_head accepts arbitrary worker input and a NUL would raise 22021.
	clean, _ := stripNUL(capturedHead)
	head := pgconv.TextOrNull(strings.TrimSpace(clean))
	run, err := s.q.SetRunWallPark(ctx, store.SetRunWallParkParams{
		ID:                   runID,
		WorkerID:             pgconv.UUID(wkr.ID),
		HoldCapturedHead:     head,
		Now:                  pgconv.Time(s.now()),
		GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
		// The nullable generation fence (D16). A wall_park_v1 worker stamps it; nil is honoured on a
		// live claim and rejected on a released one by the store transition.
		ClaimGeneration: pgconv.Int8Ptr(claimGen),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The guard rejected the park. Re-read the authoritative row so the worker learns its
			// real status through the applied contract. A reclaim re-reads as ErrRunNotOwned (-> 404).
			current, rerr := s.runOwnedByWorker(ctx, runID, wkr)
			if rerr != nil {
				return store.Run{}, false, rerr
			}
			// D16 idempotency: the server already parked this row. Treat the report as parked, and
			// keep a captured head the server park did not have (only ADDS information, never changes
			// status).
			if current.Status == "paused" && current.HoldReason.Valid && current.HoldReason.String == "budget_exhausted" {
				if head.Valid && claimGen != nil && !current.HoldCapturedHead.Valid {
					if _, herr := s.q.RecordWallParkCapturedHead(ctx, store.RecordWallParkCapturedHeadParams{
						ID:              runID,
						WorkerID:        pgconv.UUID(wkr.ID),
						ClaimGeneration: *claimGen,
						Head:            head,
					}); herr != nil {
						slog.Warn("record wall park captured head", "run", runID, "error", herr)
					} else if refreshed, rerr2 := s.runOwnedByWorker(ctx, runID, wkr); rerr2 == nil {
						current = refreshed
					}
				}
				return current, true, nil
			}
			// Not parked (the owner extended in the window, or the run moved on): retain live, restart.
			return current, false, nil
		}
		return store.Run{}, false, err
	}
	return run, true, nil
}
