package workersvc

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// The volume inventory taken while a worker is preparing to hibernate (PRD #4208 M1).
//
// Locking: every write to worker_hibernation_inventories here runs inside a transaction whose
// first statement was GetWorkerForUpdate (hibernationTx, or the Register transaction), so the
// lock order is workers -> inventory and every predicate below is read from the locked row.
//
// The service only RECORDS evidence: it never decides that volumes may be retired. Each
// invalidation is monotonic (the first reason sticks), and a transition that cannot be shown to
// hold the invariant invalidates rather than errors, so the controller's ordinary flow is never
// blocked by the evidence trail.

const (
	// HibernationInventoryTimeout is how long an awaiting inventory may take to arrive.
	HibernationInventoryTimeout = 3 * time.Minute
	// inventoryGapPolls is the number of controller poll intervals a held observation may skip
	// before the gap invalidates the inventory.
	inventoryGapPolls = 3
)

// currentInventory reads the locked worker's inventory row for its CURRENT op. A missing row, or
// a row of another op, reports ok=false.
func currentInventory(ctx context.Context, q *store.Queries, w store.Worker) (store.GetHibernationInventoryStateForWorkerRow, bool, error) {
	if !w.HibernationOpID.Valid {
		return store.GetHibernationInventoryStateForWorkerRow{}, false, nil
	}
	inv, err := q.GetHibernationInventoryStateForWorker(ctx, w.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return inv, false, nil
	}
	if err != nil {
		return inv, false, err
	}
	return inv, inv.OpID == uuid.UUID(w.HibernationOpID.Bytes), nil
}

func invalidateInventory(ctx context.Context, q *store.Queries, w store.Worker, reason string, now time.Time) error {
	if reason == "" {
		return nil
	}
	_, err := q.InvalidateHibernationInventory(ctx, store.InvalidateHibernationInventoryParams{
		Reason: reason, At: pgconv.Time(now), WorkerID: w.ID, OpID: w.HibernationOpID.Bytes,
	})
	return err
}

// firstReason returns the first non-empty reason, so callers list candidates in priority order.
func firstReason(reasons ...string) string {
	for _, r := range reasons {
		if r != "" {
			return r
		}
	}
	return ""
}

func parsedPods(set *[]string) []uuid.UUID {
	if set == nil {
		return nil
	}
	out := make([]uuid.UUID, 0, len(*set))
	for _, s := range *set {
		if id, err := uuid.Parse(s); err == nil {
			out = append(out, id)
		}
	}
	return out
}

// anyOtherThan reports whether set holds a pod other than bound.
func anyOtherThan(set *[]string, bound uuid.UUID) bool {
	return slices.ContainsFunc(parsedPods(set), func(id uuid.UUID) bool { return id != bound })
}

// reasonIf is reason when cond holds, else "".
func reasonIf(cond bool, reason string) string {
	if cond {
		return reason
	}
	return ""
}

// inventoryTransition records what a LEGAL controller transition proves about the volume
// inventory of the worker's current op. t has already passed validateHibernationTransition, and
// w is the locked row (after the state machine's own update). Without an inventory row for the
// op every transition but `observed` is a no-op, so a worker that never reported an observation
// behaves exactly as before.
func (s *Service) inventoryTransition(ctx context.Context, q *store.Queries, w store.Worker, t HibernationTransition, pod pgtype.UUID, now time.Time) error {
	switch t.Transition {
	case TransitionObserved:
		return s.inventoryObserved(ctx, q, w, t, pod, now)
	case TransitionScalingDown, TransitionScaledDown, TransitionHeld:
	default:
		return nil
	}
	inv, ok, err := currentInventory(ctx, q, w)
	if err != nil || !ok {
		return err
	}
	bound := inv.PodUid
	gap, err := s.inventoryGap(ctx, q, inv, now)
	if err != nil {
		return err
	}

	// Generation and replica facts. The fence is the deployment generation the controller recorded
	// when it scaled the pod down. Rules, all of which only ever INVALIDATE:
	//   - a fence is in force once scaling_down carried one (the first one sticks);
	//   - a later report that carries a generation must equal the fence, and a RE-SENT report
	//     (one that arrives when a fence is already recorded) that carries no generation is
	//     unproven, hence deployment_changed;
	//   - a report that carries a replica count must say 0;
	//   - scaled_down and held must prove the fence: generation equal to it and replicas 0, an
	//     absent figure counting as a change because nothing then shows the deployment is still scaled down.
	fence := inv.FenceGeneration
	if !fence.Valid && t.FenceGeneration != nil {
		fence = pgtype.Int8{Int64: *t.FenceGeneration, Valid: true}
	}
	resend := inv.FenceGeneration.Valid
	var changed, unprovenFence bool
	switch t.Transition {
	case TransitionScalingDown:
		if fence.Valid {
			if t.FenceGeneration != nil && inv.FenceGeneration.Valid && *t.FenceGeneration != inv.FenceGeneration.Int64 {
				changed = true
			}
			if t.DeploymentGeneration != nil {
				changed = changed || *t.DeploymentGeneration != fence.Int64
			} else if resend {
				changed = true
			}
		}
		changed = changed || (t.DeploymentReplicas != nil && *t.DeploymentReplicas != 0)
	default:
		unprovenFence = !fence.Valid || !inv.ScalingDownPodUid.Valid
		if fence.Valid {
			changed = t.DeploymentGeneration == nil || *t.DeploymentGeneration != fence.Int64
		}
		changed = changed || t.DeploymentReplicas == nil || *t.DeploymentReplicas != 0
	}

	// Pod sets. While scaling down the bound pod may still be running (subset of {bound}); after
	// the exit both sets must be proven empty. Absent means unobserved, never empty.
	var otherPod, podUnproven bool
	switch t.Transition {
	case TransitionScalingDown:
		otherPod = pod.Bytes != bound || anyOtherThan(t.PodUIDs, bound) || anyOtherThan(t.PVCUserPods, bound)
	default:
		otherPod = t.PodUIDs != nil && len(*t.PodUIDs) > 0 || t.PVCUserPods != nil && len(*t.PVCUserPods) > 0
	}
	podUnproven = t.PodUIDs == nil || t.PVCUserPods == nil

	reason := firstReason(
		reasonIf(gap, InvalidatedObservationGap),
		reasonIf(otherPod, InvalidatedOtherPod),
		reasonIf(podUnproven, InvalidatedPodSetUnproven),
		reasonIf(unprovenFence, InvalidatedScaleDownPodUnproven),
		reasonIf(changed, InvalidatedDeploymentChanged),
	)

	switch t.Transition {
	case TransitionScalingDown:
		_, err = q.RecordHibernationInventoryScaleDown(ctx, store.RecordHibernationInventoryScaleDownParams{
			ScalingDownPodUid: pod, FenceGeneration: pgtypeInt8(t.FenceGeneration), LastHeldAt: pgconv.Time(now),
			WorkerID: w.ID, OpID: w.HibernationOpID.Bytes,
		})
	case TransitionScaledDown:
		_, err = q.RecordHibernationInventoryScaledDown(ctx, store.RecordHibernationInventoryScaledDownParams{
			ScaledDownAt: pgconv.Time(now), LastHeldAt: pgconv.Time(now), WorkerID: w.ID, OpID: w.HibernationOpID.Bytes,
		})
	default:
		_, err = q.RecordHibernationInventoryHeld(ctx, store.RecordHibernationInventoryHeldParams{
			LastHeldAt: pgconv.Time(now), WorkerID: w.ID, OpID: w.HibernationOpID.Bytes,
		})
	}
	if err != nil {
		return err
	}
	if t.Transition != TransitionHeld && inv.Status == InventoryAwaiting {
		// The scale-down closes the observation window: a delayed inventory can no longer be bound.
		if _, err := q.SetHibernationInventoryStatus(ctx, store.SetHibernationInventoryStatusParams{
			Status: InventoryClosedByScaleDown, WorkerID: w.ID, OpID: w.HibernationOpID.Bytes,
		}); err != nil {
			return err
		}
	}
	return invalidateInventory(ctx, q, w, reason, now)
}

func pgtypeInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}

// inventoryGap reports whether the previous held observation is older than three controller poll
// intervals, clamped exactly as the advertisement freshness window is.
func (s *Service) inventoryGap(ctx context.Context, q *store.Queries, inv store.GetHibernationInventoryStateForWorkerRow, now time.Time) (bool, error) {
	if !inv.LastHeldAt.Valid {
		return false, nil
	}
	adv, err := q.GetControllerHibernationAdvertisement(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return now.Sub(inv.LastHeldAt.Time) > inventoryGapPolls*hibernationPollInterval(adv), nil
}

// inventoryObserved handles the pre-mutation identity report. The caller has proven the worker is
// preparing with no scale-down recorded.
func (s *Service) inventoryObserved(ctx context.Context, q *store.Queries, w store.Worker, t HibernationTransition, pod pgtype.UUID, now time.Time) error {
	inv, ok, err := currentInventory(ctx, q, w)
	if err != nil {
		return err
	}
	refs := make([]HibernationPVCRef, 0, len(t.PVCs))
	for _, p := range t.PVCs {
		refs = append(refs, HibernationPVCRef{Role: p.Role, Name: p.Name, UID: p.UID})
	}
	refs = sortedPVCRefs(refs)
	same := false
	if ok && inv.DeploymentUid == t.DeploymentUID && inv.PodUid == uuid.UUID(pod.Bytes) {
		stored, err := storedPVCRefs(inv.Pvcs)
		same = err == nil && sameUIDsByRole(stored, refs)
	}
	if !same {
		status := InventoryAwaiting
		switch {
		case ok && inv.Status == InventoryAccepted:
			// An accepted inventory was bound to another identity: the evidence is not replaced
			// silently, and the gate no longer holds the scale-down back.
			status = InventoryIdentityChanged
		case !slices.Contains(w.ProtocolCapabilities, capability.HibernationInventoryV1):
			status = InventoryNoCapability
		}
		pvcs, err := marshalPVCs(t.PVCs)
		if err != nil {
			return err
		}
		if _, err := q.UpsertHibernationInventoryObserved(ctx, store.UpsertHibernationInventoryObservedParams{
			WorkerID: w.ID, OpID: w.HibernationOpID.Bytes, ObservedAt: pgconv.Time(now),
			DeploymentUid: t.DeploymentUID, DeploymentResourceVersion: t.ResourceVersion,
			PodUid: pod.Bytes, Pvcs: pvcs, Status: status,
		}); err != nil {
			return err
		}
	}
	bound := uuid.UUID(pod.Bytes)
	exactlyBound := t.PodUIDs != nil && !anyOtherThan(t.PodUIDs, bound) && slices.Contains(parsedPods(t.PodUIDs), bound)
	reason := firstReason(
		reasonIf(t.PodUIDs != nil && !exactlyBound || anyOtherThan(t.PVCUserPods, bound), InvalidatedOtherPod),
		reasonIf(t.PodUIDs == nil || t.PVCUserPods == nil, InvalidatedPodSetUnproven),
	)
	return invalidateInventory(ctx, q, w, reason, now)
}

func sameUIDsByRole(a, b []HibernationPVCRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Role != b[i].Role || a[i].UID != b[i].UID {
			return false
		}
	}
	return true
}

// inventoryOnRegister records a worker Register against the inventory of the worker's current op.
// It runs in the Register transaction (row lock held); w is the post-RegisterWorker row, so
// SnapshotRegisterNonce is the freshly rotated nonce. An accepted inventory is bound to one pod
// and one register nonce: another pod proves another writer, and the same pod under a new nonce is
// a restarted process whose exclusion is gone.
func (s *Service) inventoryOnRegister(ctx context.Context, q *store.Queries, w store.Worker, pod pgtype.UUID) error {
	inv, ok, err := currentInventory(ctx, q, w)
	if err != nil || !ok {
		return err
	}
	var reason string
	switch inv.Status {
	case InventoryAccepted:
		switch {
		case !pod.Valid:
			reason = InvalidatedPodSetUnproven
		case pod.Bytes != inv.PodUid:
			reason = InvalidatedOtherPod
		case !w.SnapshotRegisterNonce.Valid || !inv.RegisterNonce.Valid || w.SnapshotRegisterNonce.String != inv.RegisterNonce.String:
			reason = InvalidatedProcessRestarted
		}
	case InventoryAwaiting:
		if w.HibernationState == "preparing" && pod.Valid && pod.Bytes != inv.PodUid {
			reason = InvalidatedOtherPod
		}
	}
	return invalidateInventory(ctx, q, w, reason, s.now())
}

// timeOutInventory is the settle pass's per-worker step: an inventory still awaiting after
// HibernationInventoryTimeout becomes timed_out. The locked row is re-read, and only an awaiting
// row of the worker's current op moves.
func (s *Service) timeOutInventory(ctx context.Context, q *store.Queries, w store.Worker, now time.Time) (bool, error) {
	inv, ok, err := currentInventory(ctx, q, w)
	if err != nil || !ok || inv.Status != InventoryAwaiting || now.Sub(inv.ObservedAt.Time) < HibernationInventoryTimeout {
		return false, err
	}
	n, err := q.SetHibernationInventoryStatus(ctx, store.SetHibernationInventoryStatusParams{
		Status: InventoryTimedOut, StatusReason: pgconv.TextOrNull("inventory not received within 3m"),
		WorkerID: w.ID, OpID: w.HibernationOpID.Bytes,
	})
	return n > 0, err
}

// boundAwaitingInventory returns the inventory row a worker report may bind to: the worker is
// preparing with no scale-down recorded, the body names its current op and register nonce, and the
// row is awaiting, not invalidated, and bound to the same pod. Anything else is a conflict and
// nothing is written.
func boundAwaitingInventory(ctx context.Context, q *store.Queries, w store.Worker, op, pod uuid.UUID, nonce string) (store.GetHibernationInventoryStateForWorkerRow, error) {
	var none store.GetHibernationInventoryStateForWorkerRow
	if w.Kind != "hosted" || w.Ephemeral || w.HibernationState != "preparing" || w.HibernationScalingDownAt.Valid ||
		!w.HibernationOpID.Valid || w.HibernationOpID.Bytes != op ||
		!w.SnapshotRegisterNonce.Valid || w.SnapshotRegisterNonce.String != nonce {
		return none, ErrHibernationConflict
	}
	inv, ok, err := currentInventory(ctx, q, w)
	if err != nil {
		return none, err
	}
	if !ok || inv.Status != InventoryAwaiting || inv.InvalidatedReason.Valid || inv.PodUid != pod {
		return none, ErrHibernationConflict
	}
	return inv, nil
}

// AcceptHibernationInventory binds a worker's inventory to its awaiting observation. Any mismatch
// of op, pod, register nonce, volumes or deployment resource version, a missing, answered,
// timed-out, scaled-down or invalidated observation, is ErrHibernationConflict and writes nothing.
func (s *Service) AcceptHibernationInventory(ctx context.Context, workerID uuid.UUID, rep HibernationInventoryReport) error {
	return s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		inv, err := boundAwaitingInventory(ctx, q, w, rep.OpID, rep.PodUID, rep.RegisterNonce)
		if err != nil {
			return err
		}
		stored, err := storedPVCRefs(inv.Pvcs)
		if err != nil || inv.DeploymentResourceVersion != rep.DeploymentResourceVersion || !slices.Equal(stored, sortedPVCRefs(rep.PVCs)) {
			return ErrHibernationConflict
		}
		n, err := q.AcceptHibernationInventory(ctx, store.AcceptHibernationInventoryParams{
			RegisterNonce: rep.RegisterNonce, ReceivedAt: pgconv.Time(s.now()), Complete: rep.Complete,
			Entries: rep.Entries, ReceiptKeys: rep.ReceiptKeys, Digest: rep.Digest, Fingerprint: rep.Fingerprint,
			Lane: pgconv.TextOrNull(rep.Lane), Mechanism: pgconv.TextOrNull(rep.Mechanism),
			WorkerID: w.ID, OpID: rep.OpID, PodUid: rep.PodUID,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrHibernationConflict
		}
		return nil
	})
}

// ReportHibernationInventoryStatus records a worker-reported non-inventory status under the same
// identity checks as an accept.
func (s *Service) ReportHibernationInventoryStatus(ctx context.Context, workerID uuid.UUID, rep HibernationInventoryStatusReport) error {
	return s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		if _, err := boundAwaitingInventory(ctx, q, w, rep.OpID, rep.PodUID, rep.RegisterNonce); err != nil {
			return err
		}
		n, err := q.SetHibernationInventoryStatus(ctx, store.SetHibernationInventoryStatusParams{
			Status: rep.Status, StatusReason: pgconv.TextOrNull(rep.Reason), WorkerID: w.ID, OpID: rep.OpID,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrHibernationConflict
		}
		return nil
	})
}

// RecordHibernationExclusionFinal records the worker's last exclusion report against an accepted
// inventory. A release of the exclusion, or a digest or fingerprint that differs from the accepted
// inventory, proves the volume was written after the inventory.
func (s *Service) RecordHibernationExclusionFinal(ctx context.Context, workerID uuid.UUID, rep HibernationExclusionFinal) error {
	return s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		if w.Kind != "hosted" || w.Ephemeral || (w.HibernationState != "preparing" && w.HibernationState != "hibernated") ||
			!w.HibernationOpID.Valid || w.HibernationOpID.Bytes != rep.OpID {
			return ErrHibernationConflict
		}
		inv, ok, err := currentInventory(ctx, q, w)
		if err != nil {
			return err
		}
		if !ok || inv.Status != InventoryAccepted || inv.PodUid != rep.PodUID || !inv.RegisterNonce.Valid || inv.RegisterNonce.String != rep.RegisterNonce {
			return ErrHibernationConflict
		}
		now := s.now()
		if _, err := q.RecordHibernationExclusionFinal(ctx, store.RecordHibernationExclusionFinalParams{
			At: pgconv.Time(now), Digest: rep.Digest, Fingerprint: rep.Fingerprint, WorkerID: w.ID, OpID: rep.OpID,
		}); err != nil {
			return err
		}
		reason := firstReason(
			reasonIf(!rep.Held, InvalidatedExclusionLost),
			reasonIf(!inv.Digest.Valid || rep.Digest != inv.Digest.String || !inv.Fingerprint.Valid || rep.Fingerprint != inv.Fingerprint.String, InvalidatedWrittenAfterInventory),
		)
		return invalidateInventory(ctx, q, w, reason, now)
	})
}

// HibernationHeartbeat is the worker heartbeat's hibernation half: it applies the optional
// hibernation_exclusion report and returns the hibernation_inventory member the response carries,
// or nil. It runs in its own transaction that locks the worker row first (hibernationTx), which
// is why a heartbeat carrying hibernation_exclusion can never be a lockless liveness write only:
// the handler calls this beside Heartbeat, so the exclusion evidence is recorded under the same
// worker lock as every other inventory write. A report for another op is ignored.
func (s *Service) HibernationHeartbeat(ctx context.Context, workerID uuid.UUID, excl *HibernationExclusion) (*HibernationInventoryMember, error) {
	var member *HibernationInventoryMember
	err := s.hibernationTx(ctx, workerID, func(q *store.Queries, w store.Worker) error {
		if !w.HibernationOpID.Valid || w.Kind != "hosted" || w.Ephemeral {
			return nil
		}
		op := uuid.UUID(w.HibernationOpID.Bytes)
		if excl != nil && !excl.Held && excl.OpID == op && (w.HibernationState == "preparing" || w.HibernationState == "hibernated") {
			inv, ok, err := currentInventory(ctx, q, w)
			if err != nil {
				return err
			}
			if ok && (inv.Status == InventoryAwaiting || inv.Status == InventoryAccepted) {
				if err := invalidateInventory(ctx, q, w, InvalidatedExclusionLost, s.now()); err != nil {
					return err
				}
			}
		}
		if w.HibernationState != "preparing" || !slices.Contains(w.ProtocolCapabilities, capability.HibernationInventoryV1) || !w.SnapshotRegisterNonce.Valid {
			return nil
		}
		gate, err := q.GetHibernationInventoryGateForOp(ctx, store.GetHibernationInventoryGateForOpParams{WorkerID: w.ID, OpID: op})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		// An awaiting inventory that was already invalidated can never be accepted, so the worker
		// is not asked to take it.
		if gate.Status != InventoryAccepted && (gate.Status != InventoryAwaiting || gate.InvalidatedReason.Valid) {
			return nil
		}
		refs, err := storedPVCRefs(gate.Pvcs)
		if err != nil {
			return err
		}
		member = &HibernationInventoryMember{
			OpID: op.String(), Phase: gate.Status, RegisterNonce: w.SnapshotRegisterNonce.String,
			PodUID: gate.PodUid.String(), DeploymentResourceVersion: gate.DeploymentResourceVersion, PVCs: refs,
		}
		return nil
	})
	return member, err
}
