package workersvc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Warm-reserve surplus hibernation (issue #4395, M1).
//
// With worker_hibernation_surplus on, one idle ready worker per owner per equivalence pool is
// the keeper that stays awake, and every other idle member of the pool is hibernated after the
// (short) surplus threshold. The decision is a pure function of one owner's pool rows
// (decideSurplus); the pass in hibernation_auto.go lists candidates, groups them by owner and
// runs each decision in hibernationOwnerTx: the owner advisory lock first, then the candidate
// row lock, then ONE fresh read of the owner's pool, the decision, and the write.
//
// M2 extension points (not implemented here): the decision's action set (decisionNone,
// decisionAutoSurplus) gains auto_long and a warm wake; poolWork.long is where the owner-clock
// long rule replaces today's per-worker long rule for pooled workers.

const (
	// HibernationPoolCandidateRows bounds the candidate lists of a surplus pass (fleet size).
	HibernationPoolCandidateRows = 512
	// HibernationOwnerLockTimeout is how long a decision waits for the owner lock or the
	// candidate row lock before the owner is skipped for this pass.
	HibernationOwnerLockTimeout = 5 * time.Second
)

// Decision actions.
const (
	decisionNone        = "none"
	decisionAutoSurplus = "auto_surplus"
)

// Reasons a decision is none, for logs and tests.
const (
	noneNotCandidate     = "not_candidate"
	noneNoPool           = "no_pool"
	noneNoKeeper         = "no_keeper"
	noneMemoryAuthUnsure = "memory_authority_unknown"
)

// errHibernationOwnerBusy reports that the owner or candidate lock was not granted within the
// lock timeout (SQLSTATE 55P03); the owner is skipped for this pass.
var errHibernationOwnerBusy = errors.New("hibernation owner lock busy")

// SetActiveSnapshotDisabled tells the pool policy that the api runs with the active-snapshot
// kill switch (UZI_ACTIVE_SNAPSHOT_DISABLED) on. In that mode memory authority is never
// persisted (see persistMemoryAuthority), so the policy cannot tell whether a memory-capable
// member lost it and makes no surplus decision for such pools. Call once at startup.
func (s *Service) SetActiveSnapshotDisabled(disabled bool) { s.memoryAuthorityUnpersisted = disabled }

func (s *Service) hibernationOwnerLockTimeout() time.Duration {
	if s.ownerLockTimeout > 0 {
		return s.ownerLockTimeout
	}
	return HibernationOwnerLockTimeout
}

// ---- pure functions ----

// poolRow is one persistent hosted worker of an owner as the pool policy reads it.
type poolRow = store.GetOwnerHibernationPoolInputRow

// sortedSet returns the sorted, deduplicated copy of in.
func poolSortedSet(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func quotedList(in []string) string {
	q := make([]string, len(in))
	for i, s := range in {
		q[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(q, ",") + "]"
}

// poolKey is the equivalence-pool key of a worker row: exact equality over the owner, the
// declared template and size, the isolated lane, docker, the effective capability set, the
// protocol capability set, the slot counts (NULL matches only NULL). ok is false when the worker
// is in no pool: ephemeral, not hosted, or never registered (NULL template, size or docker).
func poolKey(r poolRow) (key string, ok bool) {
	if r.Ephemeral || r.Kind != "hosted" ||
		!r.TemplateDeclared.Valid || !r.HostedSize.Valid || !r.DockerEnabled.Valid {
		return "", false
	}
	maxRuns, maxCross := "null", "null"
	if r.MaxConcurrentRuns.Valid {
		maxRuns = fmt.Sprint(r.MaxConcurrentRuns.Int32)
	}
	if r.MaxCrossCheckSlots.Valid {
		maxCross = fmt.Sprint(r.MaxCrossCheckSlots.Int32)
	}
	return fmt.Sprintf("%s|%q|%q|%t|%t|%s|%s|%s|%s", r.UserID, r.TemplateDeclared.String, r.HostedSize.String,
		r.IsolatedLane, r.DockerEnabled.Bool, quotedList(poolSortedSet(r.EffectiveCaps)),
		quotedList(poolSortedSet(r.ProtocolCapabilities)), maxRuns, maxCross), true
}

// memberReady reports whether an awake pool member can take the next run now and is idle (the
// reserve is an idle worker). It fails closed: anything not positively established is not ready.
// Custody does not make a member not ready, and the shared gates (a locked vault, the custody
// hold limit, the instance intake pause) are deliberately absent: they affect every member
// alike and are not keeper inputs (PRD D17).
func memberReady(r poolRow, now time.Time, stale time.Duration) bool {
	if r.Ephemeral || r.Kind != "hosted" || r.Status != "online" || r.HibernationState != "none" {
		return false
	}
	if stale <= 0 || !r.LastHeartbeatAt.Valid || now.Sub(r.LastHeartbeatAt.Time) >= stale {
		return false
	}
	if !store.WorkerAcceptsNewIntake(store.Worker{
		ManualCordonedAt: r.ManualCordonedAt, DrainingSince: r.DrainingSince,
		MaintenanceFenced: r.MaintenanceFenced, MaintenancePhase: r.MaintenancePhase,
		HibernationState: r.HibernationState,
	}) {
		return false
	}
	if r.Nonterminal != 0 {
		return false
	}
	// The agent pauses claims while a pending-outcome overflow closure is unexpired.
	if r.PendingOverflowUntil.Valid && r.PendingOverflowUntil.Time.After(now) {
		return false
	}
	// Only a positively settled cleanup phase counts; an unknown value is not settled.
	if r.CleanupPhase != CleanupPhaseNone && r.CleanupPhase != CleanupPhaseSettled {
		return false
	}
	return !r.MemoryAuthorityLostAt.Valid
}

// memoryAuthorityCapable reports whether a worker's protocol capabilities make the claim gate
// judge its memory authority (both capabilities, as memoryAuthorityLost reads them).
func memoryAuthorityCapable(protocolCaps []string) bool {
	return slices.Contains(protocolCaps, capability.WorkerMemoryPressureV1) &&
		slices.Contains(protocolCaps, capability.WorkerMemoryAuthorityClaimV1)
}

func anchorOf(r poolRow) time.Time {
	if r.Anchor.Valid {
		return r.Anchor.Time
	}
	return time.Time{}
}

// pickKeeper chooses the keeper among the ready idle members of rows: the most recent surplus
// anchor first, then the oldest created_at, then the lowest id. exclude is never the keeper.
func pickKeeper(rows []poolRow, exclude uuid.UUID, now time.Time, stale time.Duration) (uuid.UUID, bool) {
	var ready []poolRow
	for _, r := range rows {
		if r.ID != exclude && memberReady(r, now, stale) {
			ready = append(ready, r)
		}
	}
	if len(ready) == 0 {
		return uuid.Nil, false
	}
	slices.SortFunc(ready, func(a, b poolRow) int {
		if c := anchorOf(b).Compare(anchorOf(a)); c != 0 { // most recent anchor first
			return c
		}
		if c := a.CreatedAt.Time.Compare(b.CreatedAt.Time); c != 0 { // oldest created first
			return c
		}
		return cmp.Compare(a.ID.String(), b.ID.String())
	})
	return ready[0].ID, true
}

// poolDecision is the outcome of one policy decision for one candidate.
type poolDecision struct {
	Action string
	Keeper uuid.UUID // the member that stays awake (auto_surplus only)
	Why    string    // why the action is none
}

// decideSurplus decides whether candidate, a member of the owner's rows, is surplus: it is
// pooled, awake and idle with its surplus anchor at or before surplusCutoff, and another member
// of its pool is the awake ready idle keeper. memoryAuthorityPersisted is false while the api
// does not persist memory authority (the active-snapshot kill switch): a pool whose members are
// memory-capable then has unknown readiness and gets no decision at all.
func decideSurplus(rows []poolRow, candidate uuid.UUID, now time.Time, stale time.Duration, surplusCutoff time.Time, memoryAuthorityPersisted bool) poolDecision {
	idx := slices.IndexFunc(rows, func(r poolRow) bool { return r.ID == candidate })
	if idx < 0 {
		return poolDecision{Action: decisionNone, Why: noneNotCandidate}
	}
	c := rows[idx]
	key, ok := poolKey(c)
	if !ok {
		return poolDecision{Action: decisionNone, Why: noneNoPool}
	}
	if c.HibernationState != "none" || c.Nonterminal != 0 || !c.Anchor.Valid || c.Anchor.Time.After(surplusCutoff) {
		return poolDecision{Action: decisionNone, Why: noneNotCandidate}
	}
	if !memoryAuthorityPersisted && memoryAuthorityCapable(c.ProtocolCapabilities) {
		return poolDecision{Action: decisionNone, Why: noneMemoryAuthUnsure}
	}
	var members []poolRow
	for _, r := range rows {
		if k, ok := poolKey(r); ok && k == key {
			members = append(members, r)
		}
	}
	keeper, ok := pickKeeper(members, candidate, now, stale)
	if !ok {
		return poolDecision{Action: decisionNone, Why: noneNoKeeper}
	}
	return poolDecision{Action: decisionAutoSurplus, Keeper: keeper}
}

// surplusThreshold is the surplus idle threshold the pass uses: min(surplus, idle_after).
func surplusThreshold(surplus, idleAfter time.Duration) time.Duration { return min(surplus, idleAfter) }

// surplusHibernationReason is the hibernation_reason recorded for a surplus admission.
func surplusHibernationReason(threshold time.Duration) string {
	return "automatic: surplus in pool, idle " + idleSpan(threshold)
}

// ---- pass ----

// poolWork is one candidate of a surplus pass.
type poolWork struct {
	id, owner uuid.UUID
	anchor    time.Time
	pooled    bool // listed by ListHibernationPoolCandidates
	long      bool // idle past the long threshold: today's per-worker long rule applies
}

// surplusPassParams are the instants and thresholds one surplus pass uses.
type surplusPassParams struct {
	now                                     time.Time
	idleAfter, threshold                    time.Duration
	surplusCutoff, longCutoff, recentCutoff time.Time
}

// hibernationSurplusPass is HibernationAutoPass while surplus mode is on. Candidates are the
// pool candidates (surplus rule, and today's long rule for pooled workers in M1) plus the
// long-rule candidates that are in no pool. They are grouped by owner; owners are served
// oldest-pending-first; each owner gets at most one state change per pass and the whole pass at
// most HibernationAutoAdmissionsPerPass, shared by every kind of admission. A refused or
// skipped candidate is not a state change, so the owner's next candidate is tried.
func (s *Service) hibernationSurplusPass(ctx context.Context, p surplusPassParams) (int64, error) {
	work, err := s.listSurplusWork(ctx, p)
	if err != nil {
		return 0, err
	}
	if s.autoPassAfterList != nil {
		s.autoPassAfterList()
	}
	var admitted int64
	var errs []error
	budget := HibernationAutoAdmissionsPerPass
owners:
	for _, g := range groupByOwner(work) {
		if budget <= 0 {
			break
		}
		for _, w := range g {
			res, err := s.surplusWork(ctx, w, p)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) { // the worker was deleted since the list
					errs = append(errs, fmt.Errorf("worker %s: %w", w.id, err))
				}
				continue
			}
			if res.busy {
				continue owners // the owner lock is held elsewhere: skip the owner this pass
			}
			if res.stop {
				break owners
			}
			if res.admitted {
				admitted++
				budget--
				slog.Info("worker hibernation: automatic admission", "worker_id", w.id,
					"owner_id", w.owner, "idle_since", w.anchor, "pooled", w.pooled, "surplus", res.surplus)
				continue owners // one state change per owner per pass
			}
		}
	}
	if len(errs) > 0 {
		slog.Warn("worker hibernation auto pass", "errors", len(errs), "first", errs[0])
		return admitted, errors.Join(errs...)
	}
	return admitted, nil
}

// groupByOwner groups work by owner, each group oldest anchor first (then id), and orders the
// groups by their oldest anchor (then owner id), so the owner waiting longest is served first.
func groupByOwner(work []poolWork) [][]poolWork {
	byOwner := map[uuid.UUID][]poolWork{}
	for _, w := range work {
		byOwner[w.owner] = append(byOwner[w.owner], w)
	}
	groups := make([][]poolWork, 0, len(byOwner))
	for _, g := range byOwner {
		slices.SortFunc(g, func(a, b poolWork) int {
			if c := a.anchor.Compare(b.anchor); c != 0 {
				return c
			}
			return cmp.Compare(a.id.String(), b.id.String())
		})
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool {
		if c := groups[i][0].anchor.Compare(groups[j][0].anchor); c != 0 {
			return c < 0
		}
		return groups[i][0].owner.String() < groups[j][0].owner.String()
	})
	return groups
}

// listSurplusWork reads both candidate lists in one transaction and merges them.
func (s *Service) listSurplusWork(ctx context.Context, p surplusPassParams) ([]poolWork, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	pooled, err := q.ListHibernationPoolCandidates(ctx, store.ListHibernationPoolCandidatesParams{
		SurplusCutoff: pgconv.Time(p.surplusCutoff), RecentCutoff: pgconv.Time(p.recentCutoff),
		MaxRows: HibernationPoolCandidateRows,
	})
	if err != nil {
		return nil, err
	}
	long, err := q.ListAutoHibernationCandidates(ctx, store.ListAutoHibernationCandidatesParams{
		Cutoff: pgconv.Time(p.longCutoff), RecentCutoff: pgconv.Time(p.recentCutoff), MaxRows: HibernationPoolCandidateRows,
	})
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]*poolWork, len(pooled)+len(long))
	var out []*poolWork
	for _, c := range pooled {
		w := &poolWork{id: c.ID, owner: c.UserID, anchor: c.Anchor.Time, pooled: true,
			long: !c.Anchor.Time.After(p.longCutoff)}
		byID[c.ID] = w
		out = append(out, w)
	}
	var unknownOwner []uuid.UUID
	for _, c := range long {
		if _, ok := byID[c.ID]; ok {
			continue
		}
		w := &poolWork{id: c.ID, anchor: c.Anchor.Time, long: true}
		byID[c.ID] = w
		out = append(out, w)
		unknownOwner = append(unknownOwner, c.ID)
	}
	if len(unknownOwner) > 0 {
		owners, err := q.ListWorkerOwners(ctx, unknownOwner)
		if err != nil {
			return nil, err
		}
		for _, o := range owners {
			byID[o.ID].owner = o.UserID
		}
	}
	res := make([]poolWork, 0, len(out))
	for _, w := range out {
		if w.owner != uuid.Nil { // a long candidate deleted since the list has no owner row
			res = append(res, *w)
		}
	}
	return res, nil
}

// workResult is what one candidate attempt did.
type workResult struct {
	admitted bool
	surplus  bool // the admission was a surplus admission
	stop     bool // the pass should stop: a setting was switched off
	busy     bool // the owner lock (or the candidate row lock) timed out
}

// surplusWork runs one candidate: a pooled one under the owner lock, an unpooled long-rule
// candidate exactly as today's pass does.
func (s *Service) surplusWork(ctx context.Context, w poolWork, p surplusPassParams) (workResult, error) {
	if !w.pooled {
		var res workResult
		err := s.hibernationTx(ctx, w.id, func(q *store.Queries, row store.Worker) error {
			var e error
			res.admitted, res.stop, e = s.autoAdmit(ctx, q, row, p.now, p.longCutoff, p.recentCutoff, p.idleAfter)
			return e
		})
		return res, err
	}
	return s.poolAttempt(ctx, w, p)
}

// poolAttempt is one pooled candidate's decision: owner lock, candidate row lock, one fresh pool
// read, the decision, and the write, in one transaction (hibernationOwnerTx).
func (s *Service) poolAttempt(ctx context.Context, w poolWork, p surplusPassParams) (workResult, error) {
	var res workResult
	err := s.hibernationOwnerTx(ctx, w.owner, w.id, func(q *store.Queries, row store.Worker) error {
		if row.UserID != w.owner || row.Kind != "hosted" || row.Ephemeral || row.HibernationState != "none" {
			return nil
		}
		if row.HibernationBlockedReason.Valid && row.HibernationBlockedReason.String == BlockedControllerRefused &&
			row.HibernationBlockedAt.Valid && row.HibernationBlockedAt.Time.After(p.recentCutoff) {
			return nil
		}
		// A separate statement after both locks: its own READ COMMITTED snapshot sees every
		// commit the previous owner-lock holder made.
		rows, err := q.GetOwnerHibernationPoolInput(ctx, w.owner)
		if err != nil {
			return err
		}
		dec := decideSurplus(rows, w.id, p.now, s.p.WorkerHeartbeatStale, p.surplusCutoff, !s.memoryAuthorityUnpersisted)
		if s.poolPassAfterDecide != nil {
			s.poolPassAfterDecide()
		}
		switch dec.Action {
		case decisionAutoSurplus:
			res.surplus = true
			res.admitted, res.stop, err = s.admitAutomatic(ctx, q, row, p.now, surplusHibernationReason(p.threshold), true)
			return err
		default:
			// M2: the owner-clock long rule (auto_long) and the warm wake decide here. In M1 a
			// pooled worker keeps today's per-worker long rule, under the owner lock so it
			// serializes with surplus decisions on the same owner.
			if !w.long {
				return nil
			}
			res.admitted, res.stop, err = s.autoAdmit(ctx, q, row, p.now, p.longCutoff, p.recentCutoff, p.idleAfter)
			return err
		}
	})
	if errors.Is(err, errHibernationOwnerBusy) {
		return workResult{busy: true}, nil
	}
	return res, err
}

// hibernationOwnerTx runs fn in one READ COMMITTED transaction under the owner's hibernation
// lock. The order is fixed (lock-order invariant of the PRD): lock_timeout, the owner advisory
// lock as the FIRST locking statement, then the candidate row lock. A lock that is not granted
// within the lock timeout (SQLSTATE 55P03) rolls the transaction back and returns
// errHibernationOwnerBusy. It never calls WakeWorker or HibernateWorker (each opens its own
// transaction and would wait on the candidate lock held here) and inserts no event row.
func (s *Service) hibernationOwnerTx(ctx context.Context, owner, candidate uuid.UUID, fn func(q *store.Queries, w store.Worker) error) error {
	if s.txBeginner == nil {
		return ErrHibernationUnavailable
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ms := max(s.hibernationOwnerLockTimeout().Milliseconds(), 1)
	if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", ms)); err != nil {
		return err
	}
	if s.poolPassBeforeOwnerLock != nil {
		var pid int32
		if err := tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
			return err
		}
		s.poolPassBeforeOwnerLock(pid)
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)",
		store.HibernationOwnerLockClass, store.HibernationOwnerLockObjID(owner)); err != nil {
		return ownerLockErr(err)
	}
	q := store.New(tx)
	w, err := q.GetWorkerForUpdate(ctx, candidate)
	if err != nil {
		return ownerLockErr(err)
	}
	if err := fn(q, w); err != nil {
		return ownerLockErr(err)
	}
	return tx.Commit(ctx)
}

// ownerLockErr maps a lock_timeout failure (SQLSTATE 55P03) to errHibernationOwnerBusy.
func ownerLockErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "55P03" {
		return errHibernationOwnerBusy
	}
	return err
}
