package workersvc

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Hibernation cache prune attempt (PRD #4206).
//
// One attempt per hibernation, on the worker row, locked by hibernationTx. The api is the only
// authority on its phase. A worker agent runs a prune command only after a heartbeat response
// shows phase "started" for its op id and register nonce; the controller never learns anything
// except that the attempt is unsettled ("cleaning") and, once it is unknown, which pod uid to look
// for. The claim fence (hibernation_state preparing or waking, which trySettle and ApplyTransition
// honour) outlives the attempt until it settles.

// Attempt phases (workers.cleanup_phase).
const (
	CleanupPhaseNone      = "none"
	CleanupPhaseRequested = "requested"
	CleanupPhaseStarted   = "started"
	CleanupPhaseUnknown   = "unknown"
	CleanupPhaseSettled   = "settled"
)

// Outcomes (workers.hibernation_cleanup).
const (
	HibernationCleanupPruned  = "pruned"
	HibernationCleanupSkipped = "skipped"
	HibernationCleanupFailed  = "failed"
)

// Reasons recorded on a settled attempt or a withdrawn hibernation, and the refusal reasons an
// agent may report. The first six are the closed set of the agent's refused report.
const (
	CleanupReasonDaemonUnreachable = "daemon_unreachable"
	CleanupReasonCustodyStale      = "custody_stale"
	CleanupReasonActiveWork        = "active_work"
	CleanupReasonClaimInFlight     = "claim_in_flight"
	CleanupReasonGateNotExclusive  = "gate_not_exclusive"
	CleanupReasonCancelled         = "cancelled"

	CleanupReasonUnsupported       = "unsupported"
	CleanupReasonWorkerRestarted   = "worker_restarted"
	CleanupReasonInterrupted       = "interrupted"
	CleanupReasonCustodyHold       = "custody_hold"
	CleanupReasonPendingWakeDemand = "pending_wake_demand"
	CleanupReasonMaintenanceActive = "maintenance_active"
)

// Report kinds and step vocabulary of the agent's hibernation_cache_prune_report.
const (
	CleanupReportRefused = "refused"
	CleanupReportReady   = "ready"
	CleanupReportResult  = "result"

	CleanupCommandImagePruneDangling = "image_prune_dangling"
	CleanupCommandBuilderPrune       = "builder_prune"

	CleanupStepNotStarted = "not_started"
	CleanupStepExitedOK   = "exited_ok"
	CleanupStepUnknown    = "unknown"
)

const (
	// HibernationCleanupResultTimeout is how long a started attempt may stay silent before it
	// is treated as unknown.
	HibernationCleanupResultTimeout = 10 * time.Minute
	// HibernationCleanupReconcileEvery paces the reconciliation requests for an unknown attempt.
	HibernationCleanupReconcileEvery = 5 * time.Minute
	// hibernationCleanupEvidenceFresh bounds the age of the agent's custody evidence, as the
	// maintenance ready ACK does.
	hibernationCleanupEvidenceFresh = 45 * time.Second
)

// ErrHibernationCleanupConflict is a cache prune report that is stale, mismatched, malformed or
// not allowed from the current state. The row is left exactly as it was (HTTP 409).
var ErrHibernationCleanupConflict = errors.New("hibernation cache prune precondition failed")

// ErrWorkerHibernationCleanupUnsettled refuses a worker delete while a cache prune attempt is
// unsettled (HTTP 409).
var ErrWorkerHibernationCleanupUnsettled = errors.New("worker hibernation cache prune is not settled")

// WorkerHibernationCleanupUnsettledError carries the phase that blocks a worker delete.
type WorkerHibernationCleanupUnsettledError struct{ Phase string }

func (e *WorkerHibernationCleanupUnsettledError) Error() string {
	return ErrWorkerHibernationCleanupUnsettled.Error()
}
func (e *WorkerHibernationCleanupUnsettledError) Unwrap() error {
	return ErrWorkerHibernationCleanupUnsettled
}

// HibernationCachePrune is the heartbeat response member hibernation_cache_prune: the agent's view
// of its attempt. Phase is requested|started|unknown|settled.
type HibernationCachePrune struct {
	OpID            string `json:"op_id"`
	RegisterNonce   string `json:"register_nonce"`
	Phase           string `json:"phase"`
	CancelRequested bool   `json:"cancel_requested"`
	ReconcileSeq    int32  `json:"reconcile_seq"`
}

// HibernationCachePruneStep is one command's state in a result report.
type HibernationCachePruneStep struct {
	Command string `json:"command"`
	State   string `json:"state"`
}

// HibernationCachePruneReport is the heartbeat request member hibernation_cache_prune_report.
// Kind selects which of the other fields apply: refused{reason}; ready{local_claims,
// local_executions, custody_clear, custody_checked_at, gate_exclusive}; result{steps,
// gate_exclusive_throughout}. Every report is bound to {op_id, register_nonce}.
type HibernationCachePruneReport struct {
	OpID          string `json:"op_id"`
	RegisterNonce string `json:"register_nonce"`
	Kind          string `json:"kind"`
	Reason        string `json:"reason,omitempty"`

	LocalClaims      *int       `json:"local_claims,omitempty"`
	LocalExecutions  *int       `json:"local_executions,omitempty"`
	CustodyClear     bool       `json:"custody_clear,omitempty"`
	CustodyCheckedAt *time.Time `json:"custody_checked_at,omitempty"`
	GateExclusive    bool       `json:"gate_exclusive,omitempty"`

	Steps                   []HibernationCachePruneStep `json:"steps,omitempty"`
	GateExclusiveThroughout bool                        `json:"gate_exclusive_throughout,omitempty"`
}

// HibernationCachePruneFromWorker projects the row's attempt for the agent; nil when the worker
// has none.
func HibernationCachePruneFromWorker(w store.Worker) *HibernationCachePrune {
	if w.CleanupPhase == "" || w.CleanupPhase == CleanupPhaseNone || !w.CleanupOpID.Valid || !w.CleanupRegisterNonce.Valid {
		return nil
	}
	return &HibernationCachePrune{
		OpID: uuid.UUID(w.CleanupOpID.Bytes).String(), RegisterNonce: w.CleanupRegisterNonce.String,
		Phase: w.CleanupPhase, CancelRequested: w.CleanupCancelRequested, ReconcileSeq: w.CleanupReconcileSeq,
	}
}

// cleanupUnsettled reports whether phase still fences the worker.
func cleanupUnsettled(phase string) bool {
	return phase == CleanupPhaseRequested || phase == CleanupPhaseStarted || phase == CleanupPhaseUnknown
}

// cleanupSafetyReason reports whether a skipped reason withdraws the hibernation instead of
// letting it proceed: the evidence that a sleeping worker is safe is then in doubt.
func cleanupSafetyReason(reason string) bool {
	switch reason {
	case CleanupReasonCustodyStale, CleanupReasonActiveWork, CleanupReasonClaimInFlight,
		CleanupReasonGateNotExclusive, CleanupReasonCustodyHold, CleanupReasonPendingWakeDemand,
		CleanupReasonMaintenanceActive:
		return true
	}
	return false
}

func cleanupAgentRefusalReason(reason string) bool {
	switch reason {
	case CleanupReasonDaemonUnreachable, CleanupReasonCustodyStale, CleanupReasonActiveWork,
		CleanupReasonClaimInFlight, CleanupReasonGateNotExclusive, CleanupReasonCancelled:
		return true
	}
	return false
}

// issueCleanupAttempt resets the cleanup columns of a row entering preparing and issues the
// attempt: a Docker worker that advertises the capability gets one, a Docker worker that does not
// is recorded as skipped/unsupported, and a plain worker has nothing to prune (outcome NULL).
func issueCleanupAttempt(w *store.Worker, now time.Time) {
	w.HibernationCleanup = pgtype.Text{}
	w.HibernationCleanupReason = pgtype.Text{}
	w.CleanupOpID = pgtype.UUID{}
	w.CleanupPhase = CleanupPhaseNone
	w.CleanupRegisterNonce = pgtype.Text{}
	w.CleanupPodUid = pgtype.UUID{}
	w.CleanupRequestedAt = pgtype.Timestamptz{}
	w.CleanupStartedAt = pgtype.Timestamptz{}
	w.CleanupUnknownAt = pgtype.Timestamptz{}
	w.CleanupSettledAt = pgtype.Timestamptz{}
	w.CleanupCancelRequested = false
	w.CleanupReconcileSeq = 0
	w.CleanupReconciledAt = pgtype.Timestamptz{}
	if !w.DockerEnabled.Valid || !w.DockerEnabled.Bool {
		return
	}
	if !slices.Contains(w.ProtocolCapabilities, capability.HibernationCachePruneV1) ||
		!w.SnapshotRegisterNonce.Valid || w.SnapshotRegisterNonce.String == "" {
		w.HibernationCleanup = pgconv.TextOrNull(HibernationCleanupSkipped)
		w.HibernationCleanupReason = pgconv.TextOrNull(CleanupReasonUnsupported)
		return
	}
	w.CleanupOpID = hibernationUUID(uuid.New())
	w.CleanupPhase = CleanupPhaseRequested
	w.CleanupRegisterNonce = w.SnapshotRegisterNonce
	w.CleanupRequestedAt = pgconv.Time(now)
}

// settleCleanup records the confirmed settlement of an unsettled attempt.
func settleCleanup(w *store.Worker, now time.Time, outcome, reason string) {
	w.CleanupPhase = CleanupPhaseSettled
	w.HibernationCleanup = pgconv.TextOrNull(outcome)
	w.HibernationCleanupReason = pgconv.TextOrNull(reason)
	w.CleanupSettledAt = pgconv.Time(now)
}

// requestCleanupCancel is the cancellation half of moveToWaking. It never reopens claims: a
// requested attempt has run nothing, so it settles at once as skipped/cancelled; a started or
// unknown one only has cancellation requested and stays fenced until its settlement is confirmed.
func requestCleanupCancel(w *store.Worker, now time.Time) bool {
	switch w.CleanupPhase {
	case CleanupPhaseRequested:
		settleCleanup(w, now, HibernationCleanupSkipped, CleanupReasonCancelled)
		return true
	case CleanupPhaseStarted, CleanupPhaseUnknown:
		if !w.CleanupCancelRequested {
			w.CleanupCancelRequested = true
			return true
		}
	}
	return false
}

// noteCleanupRegistered applies a worker Register (post-RegisterWorker row w) to the attempt: the
// agent that held the attempt is gone. A requested attempt never ran anything and settles as
// skipped/worker_restarted. A started one may have a command still running in the daemon, so it
// becomes unknown; only identity-bound evidence about that same attempt settles it.
func noteCleanupRegistered(w *store.Worker, now time.Time) bool {
	if !cleanupUnsettled(w.CleanupPhase) || w.SnapshotRegisterNonce.String == w.CleanupRegisterNonce.String {
		return false
	}
	switch w.CleanupPhase {
	case CleanupPhaseRequested:
		settleCleanup(w, now, HibernationCleanupSkipped, CleanupReasonWorkerRestarted)
		return true
	case CleanupPhaseStarted:
		markCleanupUnknown(w, now)
		return true
	}
	return false
}

func markCleanupUnknown(w *store.Worker, now time.Time) {
	w.CleanupPhase = CleanupPhaseUnknown
	w.CleanupUnknownAt = pgconv.Time(now)
	w.CleanupReconciledAt = pgtype.Timestamptz{}
}

// reconcileCleanup is the settle pass's step for one locked row: a started attempt silent past
// HibernationCleanupResultTimeout becomes unknown, and an unknown one whose last reconciliation
// request is at least HibernationCleanupReconcileEvery old gets a new reconcile_seq, which the
// agent and the controller each read as "look again". It reports whether the row changed.
func reconcileCleanup(w *store.Worker, now time.Time) bool {
	changed := false
	if w.CleanupPhase == CleanupPhaseStarted && w.CleanupStartedAt.Valid &&
		now.Sub(w.CleanupStartedAt.Time) >= HibernationCleanupResultTimeout {
		markCleanupUnknown(w, now)
		changed = true
	}
	if w.CleanupPhase == CleanupPhaseUnknown &&
		(!w.CleanupReconciledAt.Valid || now.Sub(w.CleanupReconciledAt.Time) >= HibernationCleanupReconcileEvery) {
		w.CleanupReconcileSeq++
		w.CleanupReconciledAt = pgconv.Time(now)
		changed = true
	}
	return changed
}

// preparingDeadlineStart is when the preparing deadline starts counting: the later of the
// hibernation request and the cleanup settlement, so a long prune does not eat the window the
// controller needs to scale down.
func preparingDeadlineStart(w store.Worker) time.Time {
	start := w.HibernationRequestedAt.Time
	if w.CleanupSettledAt.Valid && w.CleanupSettledAt.Time.After(start) {
		start = w.CleanupSettledAt.Time
	}
	return start
}

// applyCleanupStopped handles the controller's cleanup_stopped transition: it observed the pod
// that ran the unknown attempt present and stopped, which is positive evidence that the attempt's
// command is no longer running. Pod absence, a new pod uid and a later prune are not.
func applyCleanupStopped(w *store.Worker, now time.Time, cleanupOpID uuid.UUID, podUID pgtype.UUID) (bool, error) {
	if !w.CleanupOpID.Valid || w.CleanupOpID.Bytes != cleanupOpID {
		return false, ErrHibernationConflict
	}
	if w.CleanupPhase == CleanupPhaseSettled && w.HibernationCleanup.String == HibernationCleanupFailed &&
		w.HibernationCleanupReason.String == CleanupReasonInterrupted {
		return false, nil // replay
	}
	if w.CleanupPhase != CleanupPhaseUnknown || !w.CleanupPodUid.Valid || !podUID.Valid ||
		w.CleanupPodUid.Bytes != podUID.Bytes {
		return false, ErrHibernationConflict
	}
	settleCleanup(w, now, HibernationCleanupFailed, CleanupReasonInterrupted)
	return true, nil
}

// cleanupServerCheck re-evaluates the admission predicates that make a sleeping worker safe, on
// the locked row. It returns the reason of the first failure, or "".
func (s *Service) cleanupServerCheck(ctx context.Context, q *store.Queries, w store.Worker) (string, error) {
	n, err := q.CountWorkerNonTerminalRuns(ctx, store.CountWorkerNonTerminalRunsParams{
		WorkerID: hibernationUUID(w.ID), UserID: w.UserID})
	if err != nil {
		return "", err
	}
	if n > 0 {
		return CleanupReasonActiveWork, nil
	}
	held, err := q.WorkerHasOpenCustodyHold(ctx, hibernationUUID(w.ID))
	if err != nil {
		return "", err
	}
	if held {
		return CleanupReasonCustodyHold, nil
	}
	if w.MaintenanceFenced || maintenancePending(w) {
		return CleanupReasonMaintenanceActive, nil
	}
	pending, err := q.WorkerHasPendingWakeDemand(ctx, w.ID)
	if err != nil {
		return "", err
	}
	if pending {
		return CleanupReasonPendingWakeDemand, nil
	}
	return "", nil
}

// withdrawForCleanup moves a preparing row to waking because the cache prune showed the
// hibernation is no longer safe. A row already waking is left as it is.
func withdrawForCleanup(w *store.Worker, now time.Time, reason string) {
	moveToWaking(w, now, "hibernation cache cleanup: "+reason)
}

// ReportHibernationCachePrune applies one agent report in its own transaction, under the worker
// row lock. Anything stale, mismatched or not allowed from the current state is
// ErrHibernationCleanupConflict and leaves the row unchanged. It returns the committed row.
func (s *Service) ReportHibernationCachePrune(ctx context.Context, workerID uuid.UUID, rep HibernationCachePruneReport) (store.Worker, error) {
	opID, err := uuid.Parse(rep.OpID)
	if err != nil || rep.RegisterNonce == "" {
		return store.Worker{}, ErrHibernationCleanupConflict
	}
	var out store.Worker
	err = s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		out = w
		if w.Kind != "hosted" || w.Ephemeral || !w.CleanupOpID.Valid || w.CleanupOpID.Bytes != opID ||
			!w.CleanupRegisterNonce.Valid || w.CleanupRegisterNonce.String != rep.RegisterNonce {
			return ErrHibernationCleanupConflict
		}
		now := s.now()
		changed, err := s.applyCleanupReport(ctx, q, &w, rep, now)
		if err != nil || !changed {
			return err
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

func (s *Service) applyCleanupReport(ctx context.Context, q *store.Queries, w *store.Worker, rep HibernationCachePruneReport, now time.Time) (bool, error) {
	switch rep.Kind {
	case CleanupReportRefused:
		if !cleanupAgentRefusalReason(rep.Reason) {
			return false, ErrHibernationCleanupConflict
		}
		switch w.CleanupPhase {
		case CleanupPhaseRequested:
			settleCleanup(w, now, HibernationCleanupSkipped, rep.Reason)
			if cleanupSafetyReason(rep.Reason) {
				withdrawForCleanup(w, now, rep.Reason)
			}
			return true, nil
		case CleanupPhaseSettled:
			return false, nil // replay
		}
		return false, ErrHibernationCleanupConflict
	case CleanupReportReady:
		switch w.CleanupPhase {
		case CleanupPhaseRequested:
			return s.startCleanup(ctx, q, w, rep, now)
		case CleanupPhaseStarted, CleanupPhaseUnknown, CleanupPhaseSettled:
			return false, nil // replay: ready carries no settlement evidence
		}
		return false, ErrHibernationCleanupConflict
	case CleanupReportResult:
		switch w.CleanupPhase {
		case CleanupPhaseStarted, CleanupPhaseUnknown:
			return s.applyCleanupResult(ctx, q, w, rep, now)
		case CleanupPhaseSettled:
			return false, nil // replay
		}
		return false, ErrHibernationCleanupConflict
	}
	return false, ErrHibernationCleanupConflict
}

// startCleanup is the start fence: it moves requested to started only when the worker is still
// preparing, no cancellation was requested, the registration is the attempt's, the server's own
// checks are clear, and the agent's fresh evidence shows it is quiet and exclusive.
func (s *Service) startCleanup(ctx context.Context, q *store.Queries, w *store.Worker, rep HibernationCachePruneReport, now time.Time) (bool, error) {
	ca := rep.CustodyCheckedAt
	if w.HibernationState != "preparing" || w.CleanupCancelRequested || !w.RegisteredPodUid.Valid ||
		w.SnapshotRegisterNonce.String != w.CleanupRegisterNonce.String ||
		rep.LocalClaims == nil || *rep.LocalClaims != 0 || rep.LocalExecutions == nil || *rep.LocalExecutions != 0 ||
		!rep.CustodyClear || !rep.GateExclusive || ca == nil ||
		!ca.After(w.CleanupRequestedAt.Time) || ca.After(now) || now.Sub(*ca) > hibernationCleanupEvidenceFresh {
		return false, ErrHibernationCleanupConflict
	}
	reason, err := s.cleanupServerCheck(ctx, q, *w)
	if err != nil {
		return false, err
	}
	if reason != "" {
		settleCleanup(w, now, HibernationCleanupSkipped, reason)
		withdrawForCleanup(w, now, reason)
		return true, nil
	}
	w.CleanupPhase = CleanupPhaseStarted
	w.CleanupStartedAt = pgconv.Time(now)
	w.CleanupPodUid = w.RegisteredPodUid
	return true, nil
}

func (s *Service) applyCleanupResult(ctx context.Context, q *store.Queries, w *store.Worker, rep HibernationCachePruneReport, now time.Time) (bool, error) {
	seen := map[string]bool{}
	anyRan, anyUnknown := false, false
	for _, st := range rep.Steps {
		if (st.Command != CleanupCommandImagePruneDangling && st.Command != CleanupCommandBuilderPrune) || seen[st.Command] {
			return false, ErrHibernationCleanupConflict
		}
		seen[st.Command] = true
		switch st.State {
		case CleanupStepNotStarted:
		case CleanupStepExitedOK:
			anyRan = true
		case CleanupStepUnknown:
			anyUnknown = true
		default:
			return false, ErrHibernationCleanupConflict
		}
	}
	if len(seen) != 2 {
		return false, ErrHibernationCleanupConflict
	}
	if anyUnknown {
		// A step whose outcome is unknown is not settlement evidence: the fence stays.
		if w.CleanupPhase == CleanupPhaseStarted {
			markCleanupUnknown(w, now)
			return true, nil
		}
		return false, nil
	}
	outcome, reason := HibernationCleanupPruned, ""
	if !anyRan {
		outcome, reason = HibernationCleanupSkipped, CleanupReasonCancelled
	}
	settleCleanup(w, now, outcome, reason)
	if w.HibernationState != "preparing" {
		return true, nil // already withdrawn: settling is all that was missing
	}
	if !rep.GateExclusiveThroughout {
		withdrawForCleanup(w, now, CleanupReasonGateNotExclusive)
		return true, nil
	}
	bad, err := s.cleanupServerCheck(ctx, q, *w)
	if err != nil {
		return false, err
	}
	if bad != "" {
		withdrawForCleanup(w, now, bad)
	}
	return true, nil
}
