package workersvc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Worker hibernation state machine (PRD #4205 M2).
//
// Every operation here is one READ COMMITTED transaction whose FIRST statement is
// GetWorkerForUpdate: the same row lock ClaimRun's claimant CTE takes, so admission
// serializes against claims. All predicates are read from the locked row (or from queries
// that run after the lock, each with a fresh READ COMMITTED snapshot), never before it.

const (
	// HibernationPreparingDeadline is how long a worker may sit in preparing before the
	// settle pass withdraws it to waking.
	HibernationPreparingDeadline = 10 * time.Minute
	// HibernationWakeTimeout is how long a wake may wait for confirmation before
	// wake_error is set. It is the fixed worker_wake_timeout of the PRD.
	HibernationWakeTimeout = 15 * time.Minute
	// HibernationDisableWakesPerPass paces the wakes that follow switching the
	// worker_hibernation_enabled setting off.
	HibernationDisableWakesPerPass = 10

	hibernationAdvertisedVersion = 1
	hibernationTextMax           = 500
	// hibernationResourceVersionMax bounds the opaque Kubernetes resourceVersion.
	hibernationResourceVersionMax = 64
	// hibernationPollClampMin/Max bound the controller-reported poll interval used to
	// size the advertisement freshness window.
	hibernationPollClampMin = 1
	hibernationPollClampMax = 300
)

// Refusal codes: the stable strings returned in the 409 body of a hibernate refusal.
const (
	RefusalSettingOff             = "setting_off"
	RefusalNotPersistentHosted    = "not_persistent_hosted"
	RefusalAlreadyActive          = "already_hibernating"
	RefusalNoPodIdentity          = "no_pod_identity"
	RefusalControllerNotAdvertise = "controller_not_advertising"
	RefusalBusy                   = "busy"
	RefusalCustodyHold            = "custody_hold"
	RefusalIntakeFenced           = "intake_fenced"
	RefusalPendingWakeDemand      = "pending_wake_demand"
)

// HibernationRefusal is the typed reason an admission was refused.
type HibernationRefusal struct{ Code string }

func (e *HibernationRefusal) Error() string { return "hibernation refused: " + e.Code }

// ErrHibernationConflict is a stale op id, a transition that is illegal from the current
// state, or a wake of a worker that is not asleep (HTTP 409).
var ErrHibernationConflict = errors.New("hibernation precondition failed")

// ErrHibernationUnavailable means no transaction beginner is wired.
var ErrHibernationUnavailable = errors.New("hibernation unavailable")

// HibernationValidationError is a malformed controller transition (HTTP 400).
type HibernationValidationError struct{ Msg string }

func (e *HibernationValidationError) Error() string { return e.Msg }

// HibernationTransition is one controller-reported step. It mirrors
// hostedsvc.HibernationTransition field for field (hostedsvc imports this package, so it
// cannot be imported here); the handler converts, and a handler test pins the Transition*
// constants to hostedsvc's.
type HibernationTransition struct {
	OpID            string
	Transition      string
	DeploymentUID   string
	ResourceVersion string
	PodUID          string
	Reason          string
}

// Transition values, equal to hostedsvc.Transition*.
const (
	TransitionRefused     = "refused"
	TransitionScalingDown = "scaling_down"
	TransitionScaledDown  = "scaled_down"
	TransitionReady       = "ready"
)

// HibernationActor identifies the admin behind an audited action.
type HibernationActor struct {
	ID    uuid.UUID
	Label string
}

// HibernationSettings is the narrow settings view hibernation reads. *settings.Cache
// satisfies it. A nil reader is treated as "off".
type HibernationSettings interface {
	WorkerHibernationEnabled(ctx context.Context) (bool, error)
}

// SetHibernationSettings wires the settings reader. Call once at startup.
func (s *Service) SetHibernationSettings(r HibernationSettings) { s.hibernationSettings = r }

func hibernationUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }

// writeHibernation persists every hibernation column of w.
func writeHibernation(ctx context.Context, q *store.Queries, w store.Worker) (store.Worker, error) {
	return q.WriteWorkerHibernation(ctx, store.WriteWorkerHibernationParams{
		HibernationState:           w.HibernationState,
		HibernationOpID:            w.HibernationOpID,
		HibernationRequestedAt:     w.HibernationRequestedAt,
		HibernatedAt:               w.HibernatedAt,
		WakeRequestedAt:            w.WakeRequestedAt,
		WakeError:                  w.WakeError,
		HibernationReason:          w.HibernationReason,
		HibernationAdmissionPodUid: w.HibernationAdmissionPodUid,
		HibernationScalingDownAt:   w.HibernationScalingDownAt,
		WakeReadyOpID:              w.WakeReadyOpID,
		WakeReadyPodUid:            w.WakeReadyPodUid,
		WakeReadyResourceVersion:   w.WakeReadyResourceVersion,
		WakeRegisterOpID:           w.WakeRegisterOpID,
		WakeRegisterPodUid:         w.WakeRegisterPodUid,
		ID:                         w.ID,
	})
}

func clearWakeEvidence(w *store.Worker) {
	w.WakeReadyOpID = pgtype.UUID{}
	w.WakeReadyPodUid = pgtype.UUID{}
	w.WakeReadyResourceVersion = pgtype.Text{}
	w.WakeRegisterOpID = pgtype.UUID{}
	w.WakeRegisterPodUid = pgtype.UUID{}
	w.WakeError = pgtype.Text{}
}

// moveToWaking applies the Wake rules to a locked row that is not awake: fresh op id,
// fresh wake_requested_at, cleared wake evidence and error; the admission pod uid and
// scaling_down_at are KEPT because settlement depends on them. A row already waking is
// left exactly as it is. It reports whether the row changed.
func moveToWaking(w *store.Worker, now time.Time, reason string) bool {
	if w.HibernationState == "waking" {
		return false
	}
	w.HibernationState = "waking"
	w.HibernationOpID = hibernationUUID(uuid.New())
	w.WakeRequestedAt = pgconv.Time(now)
	clearWakeEvidence(w)
	if reason != "" {
		w.HibernationReason = pgconv.TextOrNull(truncateRunes(reason, hibernationTextMax))
	}
	return true
}

// trySettle moves a waking row to none when the recorded evidence satisfies a
// settlement rule. Writing the replica count is never enough: it needs the controller's
// ready transition for the current op.
//
//	(a) the original pod never stopped: ready names the admission pod, no scaling_down was
//	    ever recorded, and the worker heartbeat is fresh;
//	(b) a replacement pod: ready and a worker Register both carry the current op and the
//	    same pod uid, which differs from the admission pod. A heartbeat carries no pod
//	    identity and never settles this case.
func (s *Service) trySettle(w *store.Worker, now time.Time) bool {
	if w.HibernationState != "waking" || !w.HibernationOpID.Valid || !w.WakeReadyOpID.Valid ||
		w.WakeReadyOpID.Bytes != w.HibernationOpID.Bytes || !w.WakeReadyPodUid.Valid {
		return false
	}
	samePod := w.HibernationAdmissionPodUid.Valid && w.WakeReadyPodUid.Bytes == w.HibernationAdmissionPodUid.Bytes
	settled := false
	switch {
	case samePod:
		settled = !w.HibernationScalingDownAt.Valid && w.LastHeartbeatAt.Valid &&
			now.Sub(w.LastHeartbeatAt.Time) < s.p.WorkerHeartbeatStale
	default:
		settled = w.WakeRegisterOpID.Valid && w.WakeRegisterOpID.Bytes == w.HibernationOpID.Bytes &&
			w.WakeRegisterPodUid.Valid && w.WakeRegisterPodUid.Bytes == w.WakeReadyPodUid.Bytes
	}
	if settled {
		w.HibernationState = "none"
		w.WakeError = pgtype.Text{}
	}
	return settled
}

// hibernationTx runs fn in one READ COMMITTED transaction after locking the worker row.
func (s *Service) hibernationTx(ctx context.Context, workerID uuid.UUID, fn func(q *store.Queries, w store.Worker) error) error {
	if s.txBeginner == nil {
		return ErrHibernationUnavailable
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	w, err := q.GetWorkerForUpdate(ctx, workerID)
	if err != nil {
		return err
	}
	if err := fn(q, w); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// sanitizeHibernationText strips terminal sequences and flattens raw, then reports
// whether the sanitized text exceeds hibernationTextMax runes. The length check runs on
// the sanitized-but-untruncated text so an over-long reason is refused, never silently cut.
func sanitizeHibernationText(raw string) (string, bool) {
	text := store.ManualCordonText(raw, len(raw)+1)
	return text, len([]rune(text)) > hibernationTextMax
}

// HibernateWorker admits none -> preparing, or returns a *HibernationRefusal.
func (s *Service) HibernateWorker(ctx context.Context, actor HibernationActor, workerID uuid.UUID, reason string) (store.Worker, error) {
	reason, tooLong := sanitizeHibernationText(reason)
	if tooLong {
		return store.Worker{}, &HibernationValidationError{Msg: "reason must be at most 500 characters"}
	}
	var out store.Worker
	err := s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		now := s.now()
		if code, err := s.hibernateRefusal(ctx, q, w, now); err != nil {
			return err
		} else if code != "" {
			return &HibernationRefusal{Code: code}
		}
		op := uuid.New()
		w.HibernationState = "preparing"
		w.HibernationOpID = hibernationUUID(op)
		w.HibernationRequestedAt = pgconv.Time(now)
		w.HibernatedAt = pgtype.Timestamptz{}
		w.WakeRequestedAt = pgtype.Timestamptz{}
		w.HibernationAdmissionPodUid = w.RegisteredPodUid
		w.HibernationScalingDownAt = pgtype.Timestamptz{}
		w.HibernationReason = pgtype.Text{}
		clearWakeEvidence(&w)
		row, err := writeHibernation(ctx, q, w)
		if err != nil {
			return err
		}
		out = row
		return q.InsertWorkerHibernationEvent(ctx, store.InsertWorkerHibernationEventParams{
			OwnerID: w.UserID, WorkerID: w.ID, ActorID: actor.ID, ActorLabel: actorLabel(actor),
			Action: "hibernate", OpID: op, Reason: pgconv.TextOrNull(reason), OccurredAt: pgconv.Time(now),
		})
	})
	return out, err
}

func actorLabel(a HibernationActor) string {
	l := store.ManualCordonText(a.Label, 200)
	if l == "" {
		l = a.ID.String()
	}
	return l
}

// hibernateRefusal evaluates the admission predicates against the locked row and
// returns the first failing refusal code ("" when admitted). Order is stable so the 409
// body is deterministic.
func (s *Service) hibernateRefusal(ctx context.Context, q *store.Queries, w store.Worker, now time.Time) (string, error) {
	if s.hibernationSettings == nil {
		return RefusalSettingOff, nil
	}
	on, err := s.hibernationSettings.WorkerHibernationEnabled(ctx)
	if err != nil {
		return "", err
	}
	if !on {
		return RefusalSettingOff, nil
	}
	if w.Kind != "hosted" || w.Ephemeral {
		return RefusalNotPersistentHosted, nil
	}
	if w.HibernationState != "none" {
		return RefusalAlreadyActive, nil
	}
	if !w.RegisteredPodUid.Valid {
		return RefusalNoPodIdentity, nil
	}
	adv, err := q.GetControllerHibernationAdvertisement(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	if !hibernationAdvertisementFresh(adv, now) {
		return RefusalControllerNotAdvertise, nil
	}
	n, err := q.CountWorkerNonTerminalRuns(ctx, store.CountWorkerNonTerminalRunsParams{
		WorkerID: hibernationUUID(w.ID), UserID: w.UserID})
	if err != nil {
		return "", err
	}
	if n > 0 {
		return RefusalBusy, nil
	}
	held, err := q.WorkerHasOpenCustodyHold(ctx, hibernationUUID(w.ID))
	if err != nil {
		return "", err
	}
	if held {
		return RefusalCustodyHold, nil
	}
	if !store.WorkerAcceptsNewIntake(w) {
		return RefusalIntakeFenced, nil
	}
	// A queued run that already woke this worker keeps its hibernation_wakes row, which suppresses a
	// second wake; hibernating again would strand it until it leaves the queue. Read under the
	// worker lock DemandWake also takes, so a concurrent wake is either seen here or waits.
	pending, err := q.WorkerHasPendingWakeDemand(ctx, w.ID)
	if err != nil {
		return "", err
	}
	if pending {
		return RefusalPendingWakeDemand, nil
	}
	return "", nil
}

func hibernationAdvertisementFresh(adv store.GetControllerHibernationAdvertisementRow, now time.Time) bool {
	if !adv.HibernationVersion.Valid || adv.HibernationVersion.Int32 < hibernationAdvertisedVersion ||
		!adv.HibernationAdvertisedAt.Valid {
		return false
	}
	poll := int64(hibernationPollClampMax)
	if adv.PollIntervalSeconds.Valid {
		poll = min(max(int64(adv.PollIntervalSeconds.Int32), hibernationPollClampMin), hibernationPollClampMax)
	}
	return now.Sub(adv.HibernationAdvertisedAt.Time) < 2*time.Duration(poll)*time.Second
}

// WakeWorker moves any non-none worker to waking. actor is nil for a system wake (no
// audit event). A worker already waking is a no-op that keeps its op. A worker that is
// awake is ErrHibernationConflict. Not gated by the setting.
func (s *Service) WakeWorker(ctx context.Context, actor *HibernationActor, workerID uuid.UUID, reason string) (store.Worker, error) {
	reason, tooLong := sanitizeHibernationText(reason)
	if tooLong {
		return store.Worker{}, &HibernationValidationError{Msg: "reason must be at most 500 characters"}
	}
	var out store.Worker
	err := s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		if w.HibernationState == "none" {
			return ErrHibernationConflict
		}
		now := s.now()
		if !moveToWaking(&w, now, reason) {
			out = w
			return nil
		}
		row, err := writeHibernation(ctx, q, w)
		if err != nil {
			return err
		}
		out = row
		if actor == nil {
			return nil
		}
		return q.InsertWorkerHibernationEvent(ctx, store.InsertWorkerHibernationEventParams{
			OwnerID: w.UserID, WorkerID: w.ID, ActorID: actor.ID, ActorLabel: actorLabel(*actor),
			Action: "wake", OpID: row.HibernationOpID.Bytes, Reason: pgconv.TextOrNull(reason), OccurredAt: pgconv.Time(now),
		})
	})
	return out, err
}

// validateHibernationTransition validates t and rewrites t.Reason to its sanitized form.
func validateHibernationTransition(t *HibernationTransition) (opID uuid.UUID, podUID pgtype.UUID, err error) {
	bad := func(msg string) error { return &HibernationValidationError{Msg: msg} }
	if t.OpID == "" {
		return uuid.Nil, podUID, bad("op_id is required")
	}
	opID, perr := uuid.Parse(t.OpID)
	if perr != nil {
		return uuid.Nil, podUID, bad("op_id must be a uuid")
	}
	switch t.Transition {
	case TransitionRefused, TransitionScalingDown, TransitionScaledDown, TransitionReady:
	case "":
		return uuid.Nil, podUID, bad("transition is required")
	default:
		return uuid.Nil, podUID, bad("unknown transition")
	}
	if t.DeploymentUID == "" {
		return uuid.Nil, podUID, bad("deployment_uid is required")
	}
	if _, perr := uuid.Parse(t.DeploymentUID); perr != nil {
		return uuid.Nil, podUID, bad("deployment_uid must be a uuid")
	}
	if len(t.ResourceVersion) > hibernationResourceVersionMax {
		return uuid.Nil, podUID, bad("resource_version must be at most 64 bytes")
	}
	needsRV := t.Transition != TransitionScaledDown
	needsPod := t.Transition == TransitionScalingDown || t.Transition == TransitionReady
	if needsRV && t.ResourceVersion == "" {
		return uuid.Nil, podUID, bad("resource_version is required")
	}
	if needsPod && t.PodUID == "" {
		return uuid.Nil, podUID, bad("pod_uid is required")
	}
	if t.PodUID != "" {
		p, perr := uuid.Parse(t.PodUID)
		if perr != nil {
			return uuid.Nil, podUID, bad("pod_uid must be a uuid")
		}
		podUID = hibernationUUID(p)
	}
	if t.Transition == TransitionRefused {
		var tooLong bool
		if t.Reason, tooLong = sanitizeHibernationText(t.Reason); t.Reason == "" {
			return uuid.Nil, podUID, bad("reason is required")
		}
		if tooLong {
			return uuid.Nil, podUID, bad("reason must be at most 500 characters")
		}
	}
	return opID, podUID, nil
}

// ApplyTransition applies one controller-reported step. A malformed request is a
// *HibernationValidationError; a stale op id or an illegal transition is
// ErrHibernationConflict and leaves the row unchanged.
func (s *Service) ApplyTransition(ctx context.Context, workerID uuid.UUID, t HibernationTransition) (store.Worker, error) {
	opID, podUID, err := validateHibernationTransition(&t)
	if err != nil {
		return store.Worker{}, err
	}
	var out store.Worker
	err = s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		if w.Kind != "hosted" || w.Ephemeral || !w.HibernationOpID.Valid || w.HibernationOpID.Bytes != opID {
			return ErrHibernationConflict
		}
		now := s.now()
		changed := false
		switch t.Transition {
		case TransitionRefused:
			if w.HibernationState != "preparing" {
				return ErrHibernationConflict
			}
			if !w.HibernationScalingDownAt.Valid {
				// Positively refused before any patch: restore none directly.
				w.HibernationState = "none"
				w.HibernationReason = pgconv.TextOrNull(t.Reason)
			} else {
				moveToWaking(&w, now, t.Reason)
			}
			changed = true
		case TransitionScalingDown:
			switch w.HibernationState {
			case "preparing":
				if !w.HibernationScalingDownAt.Valid {
					w.HibernationScalingDownAt = pgconv.Time(now)
					changed = true
				}
			case "hibernated":
				// Replay after scaled_down: nothing to record.
			default:
				return ErrHibernationConflict
			}
		case TransitionScaledDown:
			switch w.HibernationState {
			case "preparing":
				w.HibernationState = "hibernated"
				w.HibernatedAt = pgconv.Time(now)
				if !w.HibernationScalingDownAt.Valid {
					w.HibernationScalingDownAt = pgconv.Time(now)
				}
				changed = true
			case "hibernated":
			default:
				return ErrHibernationConflict
			}
		case TransitionReady:
			switch {
			case w.HibernationState == "waking":
				w.WakeReadyOpID = hibernationUUID(opID)
				w.WakeReadyPodUid = podUID
				w.WakeReadyResourceVersion = pgconv.TextOrNull(t.ResourceVersion)
				s.trySettle(&w, now)
				changed = true
			case w.HibernationState == "none" && w.WakeReadyOpID.Valid && w.WakeReadyOpID.Bytes == opID:
				// Replay after settlement.
			default:
				return ErrHibernationConflict
			}
		}
		if !changed {
			out = w
			return nil
		}
		row, err := writeHibernation(ctx, q, w)
		if err != nil {
			return err
		}
		out = row
		return nil
	})
	return out, err
}

// settleRegisteredWaking records a worker Register against a preparing or waking row. For
// a waking row it records the register evidence and tries to settle; for a preparing row
// it only retargets the admission pod (see below). It runs inside the Register
// transaction, which already holds the row lock via GetWorkerForUpdate; w is the
// post-RegisterWorker row.
func (s *Service) settleRegisteredWaking(ctx context.Context, q *store.Queries, w store.Worker, podUID pgtype.UUID) error {
	if w.HibernationState == "preparing" {
		// The pod that was live at admission is gone (a pod that Registers while the
		// worker is still preparing is its replacement): track the live pod, or rule (a)
		// of trySettle could never match the pod the controller later reports ready.
		// Once scaling_down is recorded the controller has fenced that pod, so the
		// admission pod is final; and a Register without a pod identity has nothing to track.
		if !podUID.Valid || w.HibernationScalingDownAt.Valid || (w.HibernationAdmissionPodUid.Valid && w.HibernationAdmissionPodUid.Bytes == podUID.Bytes) {
			return nil
		}
		w.HibernationAdmissionPodUid = podUID
		_, err := writeHibernation(ctx, q, w)
		return err
	}
	if w.HibernationState != "waking" {
		return nil
	}
	now := s.now()
	if !podUID.Valid {
		w.WakeError = pgconv.TextOrNull("worker registered without pod identity")
	} else {
		w.WakeRegisterOpID = w.HibernationOpID
		w.WakeRegisterPodUid = podUID
		s.trySettle(&w, now)
	}
	_, err := writeHibernation(ctx, q, w)
	return err
}

// HibernationSettlePass is the sweeper pass: it withdraws overdue preparations, paces
// the wakes that follow the setting being switched off, retries settlement of every
// waking worker and flags wakes that were not confirmed in time. Each worker is its own
// transaction; one failure never blocks its siblings. The result counts rows changed.
func (s *Service) HibernationSettlePass(ctx context.Context) (int64, error) {
	if s.txBeginner == nil {
		return 0, nil
	}
	var changed int64
	var errs []error
	each := func(ids []uuid.UUID, fn func(q *store.Queries, w store.Worker) (bool, error)) {
		for _, id := range ids { // ids come back in id order, so locks are taken in id order
			var did bool
			err := s.hibernationTx(ctx, id, func(q *store.Queries, w store.Worker) error {
				var e error
				did, e = fn(q, w)
				return e
			})
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					errs = append(errs, fmt.Errorf("worker %s: %w", id, err))
				}
				continue
			}
			if did {
				changed++
			}
		}
	}
	list := func(f func(q *store.Queries) ([]uuid.UUID, error)) []uuid.UUID {
		tx, err := s.beginReadCommitted(ctx)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		defer func() { _ = tx.Rollback(ctx) }()
		ids, err := f(store.New(tx))
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		return ids
	}
	now := s.now()

	// 1. Preparation deadline.
	cutoff := pgconv.Time(now.Add(-HibernationPreparingDeadline))
	each(list(func(q *store.Queries) ([]uuid.UUID, error) { return q.ListStalePreparingWorkerIDs(ctx, cutoff) }),
		func(q *store.Queries, w store.Worker) (bool, error) {
			if w.HibernationState != "preparing" || !w.HibernationRequestedAt.Valid ||
				now.Sub(w.HibernationRequestedAt.Time) < HibernationPreparingDeadline {
				return false, nil
			}
			moveToWaking(&w, now, "preparation deadline")
			_, err := writeHibernation(ctx, q, w)
			return err == nil, err
		})

	// 2. Setting off: wake sleeping workers, paced.
	if s.hibernationSettings != nil {
		on, err := s.hibernationSettings.WorkerHibernationEnabled(ctx)
		switch {
		case err != nil:
			errs = append(errs, err)
		case !on:
			ids := list(func(q *store.Queries) ([]uuid.UUID, error) {
				return q.ListWorkersToWakeOnDisable(ctx, HibernationDisableWakesPerPass)
			})
			each(ids, func(q *store.Queries, w store.Worker) (bool, error) {
				if w.HibernationState != "hibernated" && w.HibernationState != "preparing" {
					return false, nil
				}
				moveToWaking(&w, now, "hibernation disabled")
				_, err := writeHibernation(ctx, q, w)
				return err == nil, err
			})
		}
	}

	// 3. Waking rows: retry settlement, then flag an unconfirmed wake.
	each(list(func(q *store.Queries) ([]uuid.UUID, error) { return q.ListWakingWorkerIDs(ctx) }),
		func(q *store.Queries, w store.Worker) (bool, error) {
			if w.HibernationState != "waking" {
				return false, nil
			}
			dirty := s.trySettle(&w, now)
			if !dirty && !w.WakeError.Valid && w.WakeRequestedAt.Valid &&
				now.Sub(w.WakeRequestedAt.Time) >= HibernationWakeTimeout {
				w.WakeError = pgconv.TextOrNull("wake not confirmed within 15m")
				dirty = true
			}
			if !dirty {
				return false, nil
			}
			_, err := writeHibernation(ctx, q, w)
			return err == nil, err
		})

	if len(errs) > 0 {
		slog.Warn("worker hibernation settle pass", "errors", len(errs), "first", errs[0])
		return changed, errors.Join(errs...)
	}
	return changed, nil
}
