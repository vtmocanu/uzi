package workersvc

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Warm reserve (issue #4395 M2): when an owner's pool has no awake ready idle member, no healthy
// waking member and no pending demand wake while the owner is active, the pass moves exactly one
// automatically hibernated (or still preparing) pool member to waking through moveToWaking with
// wake origin warm, in the owner-locked transaction of hibernationOwnerTx. It writes no
// hibernation_wakes row (the receipts of PRD #4205 stay per queued run), calls neither WakeWorker
// nor HibernateWorker, and inserts no event row.

// warmReserveReason is the hibernation_reason recorded on a warm wake.
const warmReserveReason = "automatic: warm reserve"

// Reasons a warm decision is none, for logs and tests.
const (
	noneReserveExists = "reserve_exists"
	noneWakingHealthy = "waking_member"
	nonePendingDemand = "pending_demand_wake"
	noneOwnerInactive = "owner_inactive"
	noneSharedGate    = "shared_gate"
	noneNotEligible   = "not_eligible"
	noneNotBestTarget = "not_best_target"
	nonePoolTooSmall  = "pool_too_small"
	noneNotSleeper    = "not_a_sleeper"
)

// underMaintenance reports whether a worker's maintenance fence or phase is in effect, the same
// predicate refineIntakeFenced uses to name the maintenance blocker.
func underMaintenance(fenced bool, phase string) bool {
	switch phase {
	case "requested", "ready", "stopping", "recycling":
		return true
	}
	return fenced
}

// wakeTargetEligible reports whether a sleeper may be warm woken "as if awake" (PRD D10): it is
// preparing or hibernated, its current op was admitted by the automatic pass (a valid origin
// marker with admit origin auto_long or auto_surplus; a manual admission, a legacy op and an op
// minted without markers all read as not eligible), it is not manually cordoned, not under
// maintenance and has no wake_error.
//
// A draining sleeper (draining_since set outside maintenance) deliberately stays eligible (issue
// #4442). The drain's fate depends on which settle path of trySettle the wake takes:
//   - replacement pod: the controller's ready report plus a matching Register settle the wake, and
//     that Register's clear-on-roll (PRD #422) clears a non-maintenance draining_since, so the
//     worker can become a ready keeper;
//   - original pod retained: a withdrawn preparation whose scale-down was never recorded settles
//     on the same pod without a Register, so draining_since stays until the drain's own lifecycle
//     clears it and the woken worker is not a ready keeper meanwhile.
func wakeTargetEligible(r poolRow) bool {
	if r.HibernationState != "preparing" && r.HibernationState != "hibernated" {
		return false
	}
	if r.AdmitOrigin != AdmitOriginAutoLong && r.AdmitOrigin != AdmitOriginAutoSurplus {
		return false
	}
	return !r.ManualCordonedAt.Valid && !underMaintenance(r.MaintenanceFenced, r.MaintenancePhase) && !r.WakeError.Valid
}

// pickWakeTarget chooses the sleeper to warm wake among the eligible members of rows: preparing
// members first (withdrawn through moveToWaking), then hibernated; within each the most recent
// surplus anchor first, then the oldest created_at, then the lowest id.
func pickWakeTarget(rows []poolRow) (uuid.UUID, bool) {
	var eligible []poolRow
	for _, r := range rows {
		if wakeTargetEligible(r) {
			eligible = append(eligible, r)
		}
	}
	if len(eligible) == 0 {
		return uuid.Nil, false
	}
	rank := func(r poolRow) int {
		if r.HibernationState == "preparing" {
			return 0
		}
		return 1
	}
	slices.SortFunc(eligible, func(a, b poolRow) int {
		if c := cmp.Compare(rank(a), rank(b)); c != 0 {
			return c
		}
		if c := anchorOf(b).Compare(anchorOf(a)); c != 0 { // most recent anchor first
			return c
		}
		if c := a.CreatedAt.Time.Compare(b.CreatedAt.Time); c != 0 { // oldest created first
			return c
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	return eligible[0].ID, true
}

// ownerActiveWithin reports whether the owner is active now or its owner activity anchor is after
// now - window. Warm and system wakes never enter the anchor: it reads runs only.
func ownerActiveWithin(r poolRow, now time.Time, window time.Duration) bool {
	if r.OwnerActiveNow {
		return true
	}
	return r.OwnerAnchor.Valid && r.OwnerAnchor.Time.After(now.Add(-window))
}

// decideWarm decides whether target, a member of the owner's rows, is the sleeper to warm wake.
// All of these must hold: the pool (equal key, at least two members) has no awake ready idle
// member, no waking member without wake_error (a healthy waking member counts toward the reserve)
// and no member with a pending demand wake; the owner is active within idleAfter (or active now);
// no shared gate the database can establish makes a wake useless (paused instance intake); and target is the best eligible sleeper of the pool.
// memoryAuthorityPersisted is false while the api does not persist memory authority: a pool of
// memory-capable members then has unknown readiness and gets no decision, as in decideSurplus.
func decideWarm(rows []poolRow, target uuid.UUID, now time.Time, stale, idleAfter time.Duration, memoryAuthorityPersisted bool) poolDecision {
	idx := slices.IndexFunc(rows, func(r poolRow) bool { return r.ID == target })
	if idx < 0 {
		return poolDecision{Action: decisionNone, Why: noneNotCandidate}
	}
	c := rows[idx]
	key, ok := poolKey(c)
	if !ok {
		return poolDecision{Action: decisionNone, Why: noneNoPool}
	}
	if c.HibernationState != "preparing" && c.HibernationState != "hibernated" {
		return poolDecision{Action: decisionNone, Why: noneNotSleeper}
	}
	var members []poolRow
	for _, r := range rows {
		if k, ok := poolKey(r); ok && k == key {
			members = append(members, r)
		}
	}
	if len(members) < 2 {
		return poolDecision{Action: decisionNone, Why: nonePoolTooSmall}
	}
	if !memoryAuthorityPersisted && memoryAuthorityCapable(c.ProtocolCapabilities) {
		return poolDecision{Action: decisionNone, Why: noneMemoryAuthUnsure}
	}
	for _, m := range members {
		if m.PendingWakeDemand {
			return poolDecision{Action: decisionNone, Why: nonePendingDemand}
		}
		if m.HibernationState == "waking" && !m.WakeError.Valid {
			return poolDecision{Action: decisionNone, Why: noneWakingHealthy}
		}
	}
	if _, ok := pickKeeper(members, now, stale); ok {
		return poolDecision{Action: decisionNone, Why: noneReserveExists}
	}
	if !ownerActiveWithin(c, now, idleAfter) {
		return poolDecision{Action: decisionNone, Why: noneOwnerInactive}
	}
	// The one shared gate established from persisted state: the instance intake pause. A locked
	// owner vault is deliberately NOT a gate here: the database holds only the vault mode
	// (protected or not), while whether the DEK is cached is the in-memory vault.Vault cache of one
	// api process (RequiresUnlock), so another replica serving a worker's claim may disagree with
	// the sweeper's replica. A locked vault therefore only costs the existing demand-wake behaviour
	// (a useless warm wake), never a wrong suppression. A vault-locked owner's claims return idle
	// before the claim transaction, but persistMemoryAuthorityVaultLocked (issue #4492) still
	// records their memory-authority report (receipt or loss) on the worker row, so a locked
	// owner's pool is judged on the same evidence as an unlocked one. The owner at the custody admission limit is
	// likewise NOT a gate: ClaimRun exempts cross_check runs and continuation-exempt requeues, and
	// ClaimChatRun has no custody predicate, so a warm worker is still useful at the limit.
	if c.IntakePaused {
		return poolDecision{Action: decisionNone, Why: noneSharedGate}
	}
	best, ok := pickWakeTarget(members)
	switch {
	case !ok:
		return poolDecision{Action: decisionNone, Why: noneNotEligible}
	case best != target:
		return poolDecision{Action: decisionNone, Why: noneNotBestTarget}
	}
	return poolDecision{Action: decisionWarmWake, Target: target}
}

// warmReserveOn re-reads the three switches the warm wake depends on: hibernation, automatic mode
// and surplus mode.
func (s *Service) warmReserveOn(ctx context.Context) (bool, error) {
	if s.hibernationSettings == nil || s.hibernationAuto == nil {
		return false, nil
	}
	for _, read := range []func(context.Context) (bool, error){
		s.hibernationSettings.WorkerHibernationEnabled,
		s.hibernationAuto.WorkerHibernationAuto,
		s.hibernationAuto.WorkerHibernationSurplus,
	} {
		on, err := read(ctx)
		if err != nil || !on {
			return false, err
		}
	}
	return true, nil
}

// warmAttempt is one warm-wake target's decision in the lock order of the PRD: the owner lock,
// then the target's row lock, then ONE fresh read of the owner's pool, the decision, the write.
func (s *Service) warmAttempt(ctx context.Context, w poolWork, p surplusPassParams) (workResult, error) {
	var res workResult
	err := s.hibernationOwnerTx(ctx, w.owner, w.id, func(q *store.Queries, row store.Worker) error {
		p := p.at(s.now()) // decision time: after both locks (surplusPassParams.at)
		if row.UserID != w.owner || row.Kind != "hosted" || row.Ephemeral {
			return nil
		}
		rows, err := q.GetOwnerHibernationPoolInput(ctx, w.owner)
		if err != nil {
			return err
		}
		dec := decideWarm(rows, w.id, p.now, s.p.WorkerHeartbeatStale, p.idleAfter, !s.memoryAuthorityUnpersisted)
		if s.poolPassAfterDecide != nil {
			s.poolPassAfterDecide()
		}
		if dec.Action != decisionWarmWake {
			return nil
		}
		on, err := s.warmReserveOn(ctx)
		if err != nil {
			return err
		}
		if !on {
			res.stop = true
			return nil
		}
		if !moveToWaking(&row, p.now, warmReserveReason, WakeOriginWarm) {
			return nil
		}
		if _, err := writeHibernation(ctx, q, row); err != nil {
			return err
		}
		res.admitted, res.warm = true, true
		return nil
	})
	if errors.Is(err, errHibernationOwnerBusy) {
		return workResult{busy: true}, nil
	}
	return res, err
}
