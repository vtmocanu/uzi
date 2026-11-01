package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Publication-phase forge park (issue #1324 D3, D8). A run with an active publication intent that
// meets a forge outage while publishing parks on recovery_wait / forge_unreachable with
// recovery_wait_phase 'publication'. The park never touches forge_park_count, so the pre-clone
// forge cap (RUN_FORGE_UNREACHABLE_MAX_PARKS) neither ends nor changes it, and it never releases
// the custody hold: the unpublished source stays with the worker that holds it.

// ErrPublicationEnrolled refuses a legacy (untyped) forge park from a publication-capable worker
// for a run that has an active intent: the worker must report recovery_phase "publication" so the
// park is accounted as a publication park (409 reason publication_enrolled; nothing mutated).
var ErrPublicationEnrolled = errors.New("publication_enrolled")

// ErrPublicationNotEnrolled refuses a typed publication park (recovery_phase "publication") for a
// run with no active intent: the typed phase must never be dropped into the pre-clone path, which
// would release custody and count a forge park (409 reason publication_not_enrolled; nothing
// mutated).
var ErrPublicationNotEnrolled = errors.New("publication_not_enrolled")

const (
	publicationPhase = "publication"

	// publicationHoldRetryExhausted is the side-row hold reason of an owner hold entered because
	// the publication wait reached RUN_PROVIDER_PARK_MAX_DURATION.
	publicationHoldRetryExhausted = "publication_retry_exhausted"
)

var (
	publicationSurfaces = []string{"git", "api"}
	publicationSteps    = []string{"floor_refresh", "push", "mr_create"}

	// publicationWorkerHoldReasons are the hold reasons a worker may report for a publication-only
	// resume that cannot publish without model work or source. publication_custodian_unavailable
	// and publication_retry_exhausted are server-only and refused from a worker.
	publicationWorkerHoldReasons = []string{
		"publication_model_required", "publication_source_unavailable", "publication_intent_mismatch",
	}
)

// validatePublicationPark checks the publication fields of a state report before any SQL. The
// fields describe a forge_unreachable park and mean nothing elsewhere, so any of them on another
// report, an unknown enum value, or a handoff tip that is not a full object id is a 400. A hold
// report (publication_hold_reason) replaces the timed park's surface and step, which it must omit.
func validatePublicationPark(req StateRequest) error {
	if req.RecoveryPhase == nil && req.PublicationSurface == nil && req.PublicationStep == nil && req.HandoffCheckpointTip == nil &&
		req.PublicationHoldReason == nil {
		return nil
	}
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidState, msg) }
	if req.State != "recovery_wait" || req.RecoveryCause == nil || *req.RecoveryCause != "forge_unreachable" {
		return bad("publication park fields require a forge_unreachable recovery_wait report")
	}
	if req.RecoveryPhase == nil || *req.RecoveryPhase != publicationPhase {
		return bad("recovery_phase must be \"publication\"")
	}
	if req.PublicationHoldReason != nil {
		if !slices.Contains(publicationWorkerHoldReasons, *req.PublicationHoldReason) {
			return bad("publication_hold_reason must be publication_model_required, publication_source_unavailable or publication_intent_mismatch")
		}
		if req.PublicationSurface != nil || req.PublicationStep != nil {
			return bad("a publication hold report must not carry publication_surface or publication_step")
		}
	} else {
		if req.PublicationSurface == nil || !slices.Contains(publicationSurfaces, *req.PublicationSurface) {
			return bad("publication_surface must be git or api")
		}
		if req.PublicationStep == nil || !slices.Contains(publicationSteps, *req.PublicationStep) {
			return bad("publication_step must be floor_refresh, push or mr_create")
		}
	}
	if req.ClaimGeneration == nil || *req.ClaimGeneration <= 0 {
		return bad("publication park requires a positive claim_generation")
	}
	if req.HandoffCheckpointTip != nil && !gitOIDHexRe.MatchString(*req.HandoffCheckpointTip) {
		return bad("handoff_checkpoint_tip must be a 40-character lowercase object id")
	}
	return nil
}

// publicationHandoff returns the handoff document to record, or nil. The handoff lets a peer claim
// the run, so it is accepted only when the run's current published checkpoint tip is exactly the
// reported one; anything else leaves the run bound to its custodian.
func publicationHandoff(run store.Run, req StateRequest) []byte {
	if req.HandoffCheckpointTip == nil {
		return nil
	}
	if !run.CheckpointTip.Valid || run.CheckpointTip.String != *req.HandoffCheckpointTip {
		slog.Warn("publication handoff ignored: checkpoint tip is not the published one", "run", run.ID)
		return nil
	}
	raw, err := json.Marshal(map[string]string{"checkpoint_tip": *req.HandoffCheckpointTip})
	if err != nil {
		return nil
	}
	return raw
}

// parkPublicationForgeUnreachable is the D8 writer, and also the D4 writer of a worker-reported
// hold (req.PublicationHoldReason): the same preconditions and the same park statement, with a NULL
// stamp and the reported side-row hold_reason. The caller holds the worker and run locks and
// has verified the generation, that the run is running, the exact custody hold, and the active
// intent. It parks the run (timed retry, or an owner hold once the elapsed bound is reached),
// records the wait clock and counters on the side row, and commits. All time comes from s.now().
//
// The bound is RunProviderParkMaxDuration measured from the side row's wait_started_at (set at
// the first park of the episode, never reset by a later park, a new generation or a new worker);
// zero disables the hold, leaving timed retries only. The failure is also observed on the
// advisory placement cooldown (issue #1324 D9): the stamped Git origin for the git surface, the
// server-derived API origin for the api surface, with reason transient_publication_exhausted. The
// retry stamp is the later of the capped backoff and the failing origin's cooldown deadline, read
// after that observation.
func (s *Service) parkPublicationForgeUnreachable(ctx context.Context, tx pgx.Tx, qtx *store.Queries, wkr store.Worker, run store.Run, intent store.RunPublicationResume, req StateRequest, sessionID pgtype.Text) (store.Run, int64, error) {
	now := s.now()
	start := now
	if intent.WaitStartedAt.Valid {
		start = intent.WaitStartedAt.Time
	}
	apiSurface := req.PublicationSurface != nil && *req.PublicationSurface == "api"
	// A worker-reported hold (publication_hold_reason) is an owner hold at once: NULL stamp, the
	// reported reason, and no forge-origin observation or cooldown, since the forge did not fail.
	workerHold := req.PublicationHoldReason != nil
	var retry pgtype.Timestamptz
	var stamp time.Time
	var holdReason pgtype.Text
	if workerHold {
		holdReason = pgconv.TextOrNull(*req.PublicationHoldReason)
	} else if bound := s.p.RunProviderParkMaxDuration; bound > 0 && now.Sub(start) >= bound {
		holdReason = pgconv.TextOrNull(publicationHoldRetryExhausted)
	} else {
		stamp = now.Add(s.recoveryParkFallbackFor(run.RecoveryWaitCount) + recoveryParkJitter())
		retry = pgconv.Time(stamp)
	}

	parked, err := qtx.ParkRunPublicationForgeUnreachable(ctx, store.ParkRunPublicationForgeUnreachableParams{
		RetryNotBefore: retry, SessionID: sessionID, ID: run.ID,
		WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: run.ClaimGeneration,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return run, 0, ErrForgeParkCustodyUnsettled
		}
		return store.Run{}, 0, err
	}
	if _, err := qtx.MarkPublicationParked(ctx, store.MarkPublicationParkedParams{
		Now: pgconv.Time(now), HoldReason: holdReason, Handoff: publicationHandoff(run, req), RunID: run.ID, WorkerID: wkr.ID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return run, 0, ErrForgeParkCustodyUnsettled
		}
		return store.Run{}, 0, err
	}
	// The observation needs the parked row (status recovery_wait / forge_unreachable), so it follows
	// the park; a cooldown that outlasts the provisional backoff stamp then raises the stamp.
	if !workerHold {
		if err := s.observeForgeOrigin(ctx, qtx, parked, apiSurface, forgeReasonPublicationExhaust); err != nil {
			return store.Run{}, 0, err
		}
	}
	if !stamp.IsZero() {
		cooled, err := s.publicationOriginCooldown(ctx, qtx, run, wkr, apiSurface)
		if err != nil {
			return store.Run{}, 0, err
		}
		if cooled.After(stamp) {
			raised, err := qtx.RaisePublicationRetryNotBefore(ctx, store.RaisePublicationRetryNotBeforeParams{
				RetryNotBefore: pgconv.Time(cooled), ID: run.ID, WorkerID: pgconv.UUID(wkr.ID), ClaimGeneration: run.ClaimGeneration,
			})
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return run, 0, ErrForgeParkCustodyUnsettled
				}
				return store.Run{}, 0, err
			}
			parked = raised
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, 0, err
	}
	return parked, 1, nil
}

// ResumePublicationForgeHold is the owner's Resume of a publication-phase owner hold. In one
// transaction (worker lock, run, side row) it queues the run and starts a new episode: the wait
// clock restarts at the next park, while the intent, lifetime diagnostics, handoff, custody and
// source are kept. It grants nothing else. It returns false when no such hold matches (the run
// moved, was not a publication hold, has no active intent, or has no budget left).
func (s *Service) ResumePublicationForgeHold(ctx context.Context, userID uuid.UUID, run store.Run) (bool, error) {
	if s.txBeginner == nil {
		return false, errors.New("publication resume transaction unavailable")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)
	if run.WorkerID.Valid {
		// Lock order: worker, run, side row. A deleted worker row is not an error here.
		if err := optInRunFences(ctx, qtx, uuid.UUID(run.WorkerID.Bytes), false); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	if _, err := qtx.ResumePublicationForgeHold(ctx, store.ResumePublicationForgeHoldParams{
		ID: run.ID, UserID: userID, GlobalTimeoutSeconds: int32(s.p.RunTimeout.Seconds()),
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	if _, err := qtx.AdvancePublicationEpisode(ctx, run.ID); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// publicationCustodianHolder is the optional statement surface of the custodian pass; a store
// without it (a fake) skips the pass.
type publicationCustodianHolder interface {
	HoldPublicationRunsWithoutCustodian(context.Context, store.HoldPublicationRunsWithoutCustodianParams) ([]store.HoldPublicationRunsWithoutCustodianRow, error)
}

// holdPublicationRunsWithoutCustodian is the D5 sweep pass: a queued run with an active intent and
// no handoff can be claimed only by its custodian worker, so when that worker cannot serve it (see
// the query for the exact arms) the run becomes an owner hold with hold_reason
// publication_custodian_unavailable instead of waiting forever. It returns the held run count.
func (s *Service) holdPublicationRunsWithoutCustodian(ctx context.Context, now time.Time) (int64, error) {
	q, ok := s.q.(publicationCustodianHolder)
	if !ok {
		return 0, nil
	}
	rows, err := q.HoldPublicationRunsWithoutCustodian(ctx, store.HoldPublicationRunsWithoutCustodianParams{
		HeartbeatCutoff:    pgconv.Time(now.Add(-s.p.WorkerHeartbeatStale)),
		AffinityCutoff:     pgconv.Time(now.Add(-s.p.WorkerAffinityCeiling)),
		StaleRequeueCutoff: StaleRequeueCutoff(now, s.p.WorkerStaleRequeueGrace),
	})
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		s.publishSwept(r.ID, "recovery_wait")
	}
	return int64(len(rows)), nil
}

// publicationOriginCooldown returns the cooldown deadline of the run's failing origin (its stamped
// Git origin, or its server-derived API origin for the api surface), or the zero time when none
// applies.
func (s *Service) publicationOriginCooldown(ctx context.Context, q *store.Queries, run store.Run, wkr store.Worker, api bool) (time.Time, error) {
	surface := "git"
	if api {
		surface = "api"
	}
	until, err := q.GetRunForgeOriginCooldownUntil(ctx, store.GetRunForgeOriginCooldownUntilParams{RunID: run.ID, WorkerID: wkr.ID, Surface: surface})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if !until.Valid {
		return time.Time{}, nil
	}
	return until.Time, nil
}

// publicationParkRefusal decides what a forge park report for an ENROLLED run does before the
// legacy release step. It returns ErrPublicationEnrolled for a legacy report from a worker that
// holds the capability (it must send the typed phase), and otherwise nil: a typed report, or a
// legacy one from a worker without the capability, goes to the publication writer.
func publicationParkRefusal(wkr store.Worker, req StateRequest) error {
	if req.RecoveryPhase == nil && slices.Contains(wkr.ProtocolCapabilities, PublicationResumeFeature) {
		return ErrPublicationEnrolled
	}
	return nil
}
