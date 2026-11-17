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
// M2 (same issue) adds the owner-clock long rule for pooled workers (GetWorkerLongIdleness under
// the owner lock, admit origin auto_long) and the warm wake (decideWarm, in hibernation_warm.go):
// both run in the same owner-locked transaction shape and share the surplus pass's budget.

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
	decisionWarmWake    = "warm_wake"
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
// anchor first, then the oldest created_at, then the lowest id.
func pickKeeper(rows []poolRow, now time.Time, stale time.Duration) (uuid.UUID, bool) {
	var ready []poolRow
	for _, r := range rows {
		if memberReady(r, now, stale) {
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
	Target uuid.UUID // the sleeper to wake (warm_wake only)
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
	// Rank every ready idle member, the candidate included: the candidate is never the keeper,
	// so when it ranks first no other member is the deterministic keeper and nothing is decided.
	keeper, ok := pickKeeper(members, now, stale)
	if !ok || keeper == candidate {
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
	pooled    bool // pooled by the SQL form of poolKey (or listed as a surplus candidate)
	surplus   bool // listed by ListHibernationPoolCandidates: idle past the surplus threshold
	long      bool // long-idle: by the long anchor when pooled, by today's anchor when not
	warm      bool // a sleeper that may be warm woken (ListHibernationWarmTargets)
}

// surplusPassParams are the instants and thresholds one surplus pass uses.
type surplusPassParams struct {
	now                                     time.Time
	idleAfter, threshold                    time.Duration
	surplusCutoff, longCutoff, recentCutoff time.Time
}

// hibernationSurplusPass is HibernationAutoPass while surplus mode is on. Candidates are the
// surplus candidates, the long-rule candidates (pooled workers judged by the owner-clock long
// anchor, unpooled ones by today's per-worker anchor) and the warm-wake targets (one sleeper per
// pool). They are grouped by owner; owners are served oldest-pending-first; each owner gets at
// most one state change per pass (an admission or a warm wake) and the whole pass at most
// HibernationAutoAdmissionsPerPass, shared by every kind of change. A refused or skipped candidate
// is not a state change, so the owner's next candidate is tried. The result counts state changes.
//
// Uncertain outcomes (PRD D21): a non-nil Commit error is genuinely ambiguous, since a commit can
// succeed on the server and still return an error. An error returned from the attempt body (a
// 55P03 raised inside the body included) never commits, because the transaction is rolled back,
// but it is treated as uncertain too, conservatively, by the owner's decision. Both surface as
// *hibernationCommitUncertainError. Such an attempt consumes one unit of the budget, is recorded
// in the joined error, is not counted as a change, and ends that owner for the pass. Errors from
// steps before the body (begin, SET LOCAL, the owner advisory lock, GetWorkerForUpdate) keep
// skipping the candidate and spend nothing.
func (s *Service) hibernationSurplusPass(ctx context.Context, p surplusPassParams) (int64, error) {
	work, err := s.listSurplusWork(ctx, p)
	if err != nil {
		return 0, err
	}
	if s.autoPassAfterList != nil {
		s.autoPassAfterList()
	}
	var changes int64
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
				var unc *hibernationCommitUncertainError
				if errors.As(err, &unc) {
					// The write may or may not have landed: count it against the budget, record it,
					// and stop this owner for the pass so it cannot get a second change.
					errs = append(errs, fmt.Errorf("worker %s: %w", w.id, err))
					budget--
					slog.Warn("worker hibernation: uncertain outcome, budget consumed", "worker_id", w.id,
						"owner_id", w.owner, "error", err)
					continue owners
				}
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
				changes++
				budget--
				slog.Info("worker hibernation: automatic change", "worker_id", w.id, "owner_id", w.owner,
					"idle_since", w.anchor, "pooled", w.pooled, "surplus", res.surplus, "warm_wake", res.warm)
				continue owners // one state change per owner per pass
			}
		}
	}
	if len(errs) > 0 {
		slog.Warn("worker hibernation auto pass", "errors", len(errs), "first", errs[0])
		return changes, errors.Join(errs...)
	}
	return changes, nil
}

// groupByOwner groups work by owner and orders each group warm wakes first (restoring the reserve
// outranks saving a pod), then oldest anchor first, then id; the groups are ordered by their
// oldest anchor (then owner id), so the owner waiting longest is served first.
func groupByOwner(work []poolWork) [][]poolWork {
	byOwner := map[uuid.UUID][]poolWork{}
	for _, w := range work {
		byOwner[w.owner] = append(byOwner[w.owner], w)
	}
	type group struct {
		work   []poolWork
		oldest time.Time
		owner  uuid.UUID
	}
	groups := make([]group, 0, len(byOwner))
	for owner, g := range byOwner {
		slices.SortFunc(g, func(a, b poolWork) int {
			if a.warm != b.warm {
				if a.warm {
					return -1
				}
				return 1
			}
			if c := a.anchor.Compare(b.anchor); c != 0 {
				return c
			}
			return cmp.Compare(a.id.String(), b.id.String())
		})
		oldest := g[0].anchor
		for _, w := range g {
			if w.anchor.Before(oldest) {
				oldest = w.anchor
			}
		}
		groups = append(groups, group{work: g, oldest: oldest, owner: owner})
	}
	sort.Slice(groups, func(i, j int) bool {
		if c := groups[i].oldest.Compare(groups[j].oldest); c != 0 {
			return c < 0
		}
		return groups[i].owner.String() < groups[j].owner.String()
	})
	out := make([][]poolWork, len(groups))
	for i, g := range groups {
		out[i] = g.work
	}
	return out
}

// warnIfCandidateListFull logs when a bounded candidate list came back at the bound: candidates
// beyond it are not seen this pass, so a fleet larger than HibernationPoolCandidateRows must be
// visible. Once per list per pass (listSurplusWork calls it once per list).
func warnIfCandidateListFull(list string, n int) {
	if n >= HibernationPoolCandidateRows {
		slog.Warn("worker hibernation: candidate list is full, later candidates wait for a following pass",
			"list", list, "rows", n, "bound", HibernationPoolCandidateRows)
	}
}

// listSurplusWork reads the three candidate lists in one transaction and merges them. The long
// list carries the owner, so no separate owner lookup is needed.
func (s *Service) listSurplusWork(ctx context.Context, p surplusPassParams) ([]poolWork, error) {
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	surplus, err := q.ListHibernationPoolCandidates(ctx, store.ListHibernationPoolCandidatesParams{
		SurplusCutoff: pgconv.Time(p.surplusCutoff), RecentCutoff: pgconv.Time(p.recentCutoff),
		MaxRows: HibernationPoolCandidateRows,
	})
	if err != nil {
		return nil, err
	}
	long, err := q.ListAutoHibernationCandidatesSurplusMode(ctx, store.ListAutoHibernationCandidatesSurplusModeParams{
		Cutoff: pgconv.Time(p.longCutoff), RecentCutoff: pgconv.Time(p.recentCutoff), MaxRows: HibernationPoolCandidateRows,
	})
	if err != nil {
		return nil, err
	}
	warm, err := q.ListHibernationWarmTargets(ctx, store.ListHibernationWarmTargetsParams{
		OwnerActiveCutoff: pgconv.Time(p.longCutoff), MaxRows: HibernationPoolCandidateRows,
	})
	if err != nil {
		return nil, err
	}
	warnIfCandidateListFull("surplus", len(surplus))
	warnIfCandidateListFull("long", len(long))
	warnIfCandidateListFull("warm", len(warm))
	byID := make(map[uuid.UUID]*poolWork, len(surplus)+len(long))
	var out []*poolWork
	for _, c := range long {
		w := &poolWork{id: c.ID, owner: c.UserID, anchor: c.Anchor.Time, pooled: c.Pooled, long: true}
		byID[c.ID] = w
		out = append(out, w)
	}
	for _, c := range surplus {
		if w, ok := byID[c.ID]; ok {
			// Also long-listed. When the exact SQL pooledness says it is unpooled it can never be
			// surplus; otherwise it is both, judged by the older of its two anchors.
			if w.pooled {
				w.surplus = true
				if c.Anchor.Time.Before(w.anchor) {
					w.anchor = c.Anchor.Time
				}
			}
			continue
		}
		w := &poolWork{id: c.ID, owner: c.UserID, anchor: c.Anchor.Time, pooled: true, surplus: true}
		byID[c.ID] = w
		out = append(out, w)
	}
	for _, c := range warm {
		out = append(out, &poolWork{id: c.ID, owner: c.UserID, anchor: c.Anchor.Time, pooled: true, warm: true})
	}
	res := make([]poolWork, len(out))
	for i, w := range out {
		res[i] = *w
	}
	return res, nil
}

// workResult is what one candidate attempt did.
type workResult struct {
	admitted bool // a state change was written: an admission or a warm wake
	surplus  bool // the admission was a surplus admission
	warm     bool // the change was a warm wake
	stop     bool // the pass should stop: a setting was switched off
	busy     bool // the owner lock (or the candidate row lock) timed out
}

// surplusWork runs one candidate: a warm-wake target and a pooled candidate under the owner lock,
// an unpooled long-rule candidate exactly as today's pass does.
func (s *Service) surplusWork(ctx context.Context, w poolWork, p surplusPassParams) (workResult, error) {
	switch {
	case w.warm:
		return s.warmAttempt(ctx, w, p)
	case !w.pooled:
		var res workResult
		err := s.hibernationTx(ctx, w.id, func(q *store.Queries, row store.Worker) error {
			// The list saw this worker unpooled. If it became pooled since (an equal-key sibling
			// registered, or keys changed), the per-worker clock no longer applies and no owner
			// lock is held here: change nothing and spend no budget. The next pass lists it as
			// pooled and decides under the owner lock.
			idle, e := q.GetWorkerLongIdleness(ctx, row.ID)
			if e != nil {
				return e
			}
			if idle.Pooled {
				return nil
			}
			res.admitted, res.stop, e = s.autoAdmit(ctx, q, row, p.now, p.longCutoff, p.recentCutoff, p.idleAfter)
			return e
		})
		return res, err
	}
	return s.poolAttempt(ctx, w, p)
}

// poolAttempt is one pooled candidate's decision: owner lock, candidate row lock, then either the
// long rule (one GetWorkerLongIdleness read) or one fresh pool read and the surplus decision, and
// the write, in one transaction (hibernationOwnerTx). A pooled worker that is long-idle is
// admitted as auto_long even if it is also surplus: it needs no keeper.
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
		if w.long {
			judged, admitted, stop, err := s.admitPooledLong(ctx, q, row, p)
			if err != nil || judged {
				res.admitted, res.stop = admitted, stop
				return err
			}
		}
		if !w.surplus {
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
		if dec.Action != decisionAutoSurplus {
			return nil
		}
		res.surplus = true
		res.admitted, res.stop, err = s.admitAutomatic(ctx, q, row, p.now, surplusHibernationReason(p.threshold), true)
		return err
	})
	if errors.Is(err, errHibernationOwnerBusy) {
		return workResult{busy: true}, nil
	}
	return res, err
}

// admitPooledLong is the long rule for a pooled worker on its locked row, in surplus mode: one
// GetWorkerLongIdleness read (the long anchor, see the query), then the shared admission with
// admit origin auto_long. judged is false when the recheck finds the worker not long-idle (a run
// or activity landed since the list) or no longer pooled (the unpooled list picks it up next pass),
// so the caller may still try the surplus rule.
func (s *Service) admitPooledLong(ctx context.Context, q *store.Queries, row store.Worker, p surplusPassParams) (judged, admitted, stop bool, err error) {
	idle, err := q.GetWorkerLongIdleness(ctx, row.ID)
	if err != nil {
		return false, false, false, err
	}
	if !idle.Pooled || idle.Nonterminal > 0 || idle.OwnerActiveNow || !idle.Anchor.Valid || idle.Anchor.Time.After(p.longCutoff) {
		return false, false, false, nil
	}
	// The owner-wide long clock only applies while surplus mode is on. If it was switched off since
	// the list, stop the pass without admitting: today's per-worker anchor would keep a just
	// warm-woken worker awake (PRD Example 5). admitAutomatic re-reads the enabled and auto switches.
	on, err := s.hibernationAuto.WorkerHibernationSurplus(ctx)
	if err != nil {
		return false, false, false, err
	}
	if !on {
		return true, false, true, nil
	}
	admitted, stop, err = s.admitAutomatic(ctx, q, row, p.now, autoHibernationReason(p.idleAfter), false)
	return true, admitted, stop, err
}

// hibernationOwnerTx runs fn in one READ COMMITTED transaction under the owner's hibernation
// lock. The order is fixed (lock-order invariant of the PRD): lock_timeout, the owner advisory
// lock as the FIRST locking statement, then the candidate row lock. If the owner lock or the
// candidate row lock is not granted within the lock timeout (SQLSTATE 55P03), it rolls the
// transaction back and returns errHibernationOwnerBusy. Any error from fn (a 55P03 inside fn
// included) and any Commit error is returned as *hibernationCommitUncertainError instead. It never calls WakeWorker or HibernateWorker (each opens its own
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
	return finishHibernationTx(ctx, tx, func() error { return fn(q, w) }, s.hibernationAfterCommit, candidate)
}

// hibernationCommitUncertainError wraps an error from the attempt body or from Commit of a
// hibernation transaction: the outcome of the write is unknown (a commit can succeed on the server
// and still return an error), so the caller must not treat it as "nothing changed". It never
// matches errHibernationOwnerBusy.
type hibernationCommitUncertainError struct{ err error }

func (e *hibernationCommitUncertainError) Error() string {
	return "hibernation write outcome uncertain: " + e.err.Error()
}

func (e *hibernationCommitUncertainError) Unwrap() error { return e.err }

// finishHibernationTx runs the attempt body, then commits. Any error from the body or from Commit,
// and any error from the test-only afterCommit hook, is returned as *hibernationCommitUncertainError.
func finishHibernationTx(ctx context.Context, tx pgx.Tx, body func() error, afterCommit func(uuid.UUID) error, id uuid.UUID) error {
	if err := body(); err != nil {
		return &hibernationCommitUncertainError{err: err}
	}
	if err := tx.Commit(ctx); err != nil {
		return &hibernationCommitUncertainError{err: err}
	}
	if afterCommit != nil {
		if err := afterCommit(id); err != nil {
			return &hibernationCommitUncertainError{err: err}
		}
	}
	return nil
}

// ownerLockErr maps a lock_timeout failure (SQLSTATE 55P03) to errHibernationOwnerBusy.
func ownerLockErr(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "55P03" {
		return errHibernationOwnerBusy
	}
	return err
}
