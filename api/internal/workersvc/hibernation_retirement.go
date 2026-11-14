package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// The dry-run volume-retirement evaluator (PRD #4208 M1). It READS the stored inventory evidence
// and the worker row and reports, per hibernated worker, what a later milestone would have to
// verify before it could retire the worker's volumes. It never writes, never reserves and never
// deletes anything, and its best verdict is ConditionalRetire: the actual PV reclaim policy and the
// absence of an intervening pod between the controller's observations are not evidence M1 has, so
// they are named as unverified instead of being assumed.

const (
	// HibernationRetirementPeriod is the continuous hibernation period (21 days) the dry run applies.
	HibernationRetirementPeriod = 504 * time.Hour
	// RetirementEvaluationLimit bounds how many candidates one evaluation looks at, oldest first.
	RetirementEvaluationLimit = 50
	// retirementBlockerCap bounds the blockers kept per verdict; BlockerCount carries the full count.
	retirementBlockerCap = 16
	// retirementSourceMax bounds a blocker source rendered into an admin surface.
	retirementSourceMax = 120
)

// Retirement verdict outcomes.
const (
	RetirementConditionalRetire = "conditional_retire"
	RetirementBlocked           = "blocked"
	RetirementCancelled         = "cancelled"
	RetirementNotYetEligible    = "not_yet_eligible"
)

// What M2 must still verify before a conditional_retire may become a deletion.
const (
	RetirementUnverifiedPVReclaimPolicy        = "pv_reclaim_policy"
	RetirementUnverifiedInterveningPodExcluded = "intervening_pod_exclusion"
)

// Blocker codes. inventory_invalid carries its reason after a colon.
const (
	BlockerLaneWithoutWriterExclusion = "lane_without_writer_exclusion"
	BlockerNoInventory                = "no_inventory"
	BlockerInventoryIncomplete        = "inventory_incomplete"
	BlockerInventoryInvalidPrefix     = "inventory_invalid:"
	BlockerExclusionUntilTermination  = "exclusion_until_termination_unproven"
	BlockerScaleDownPodUnproven       = "scale_down_pod_unproven"
	BlockerHoldChainStale             = "hold_chain_stale"
	BlockerNonTerminalRun             = "non_terminal_run"
	BlockerOpenCustodyHold            = "open_custody_hold"
	BlockerLocalReplicaCapture        = "local_replica_capture"
	BlockerPublicationResumeCustody   = "publication_resume_custody"
	BlockerManualCordon               = "manual_cordon"
	BlockerDraining                   = "draining"
	BlockerMaintenanceFenced          = "maintenance_fenced"
	BlockerMaintenancePhase           = "maintenance_phase"
	BlockerProtectedSource            = "protected_source"
	BlockerUncoveredWorktreeBytes     = "uncovered_worktree_bytes"
	BlockerNoReceipt                  = "no_receipt"
	BlockerDindState                  = "dind_state"
)

// Entry vocabulary shared with the agent census. Anything outside these sets is treated as unknown.
var (
	retirementKinds = []string{"runtime_lock", "outbox", "agent_home", "jobs", "lost_found", "bare_repo", "runner_ref",
		"worktree", "canonical_clone", "retained_attempt", "journal", "unsettled_op", "runner_scratch", "unknown_path",
		InventoryKindDockerState}
	retirementContentStates = []string{"content_proven", "unproven_worktree_bytes", "ref", "none"}
)

// RetirementBlocker is one reason a worker's volumes would be kept.
type RetirementBlocker struct {
	Code   string `json:"code"`
	Source string `json:"source"`
}

// ClaimReclaimPolicy is the StorageClass reclaim policy the controller observed for one volume
// claim. It is a proxy: the PV's own policy is not observed in M1.
type ClaimReclaimPolicy struct {
	Role   string `json:"role"`
	Policy string `json:"policy"`
}

// RetirementVerdict is one worker's dry-run result.
type RetirementVerdict struct {
	WorkerID uuid.UUID `json:"worker_id"`
	Outcome  string    `json:"outcome"`
	// Blockers holds at most retirementBlockerCap blockers in the stable guard order; BlockerCount is
	// the number found. The first blocker is the one a report shows.
	Blockers     []RetirementBlocker `json:"blockers"`
	BlockerCount int                 `json:"blocker_count"`
	// PastPeriod is true once the worker has been hibernated for HibernationRetirementPeriod.
	PastPeriod bool `json:"past_period"`
	// Remaining is the time left of the period (zero when PastPeriod).
	Remaining time.Duration `json:"remaining"`
	// Unverified is set for conditional_retire: what M2 must verify.
	Unverified []string `json:"unverified,omitempty"`
	// ClassPolicies are the per-claim class reclaim policies; Retain and Unknown flag the worker.
	ClassPolicies []ClaimReclaimPolicy `json:"class_policies,omitempty"`
	Retain        bool                 `json:"retain"`
	Unknown       bool                 `json:"unknown"`
}

// FirstBlocker returns the first blocker in the stable guard order.
func (v RetirementVerdict) FirstBlocker() (RetirementBlocker, bool) {
	if len(v.Blockers) == 0 {
		return RetirementBlocker{}, false
	}
	return v.Blockers[0], true
}

// RetirementReport is the result of one evaluation.
type RetirementReport struct {
	// Total counts every candidate (hibernated or waking persistent hosted workers).
	Total int64 `json:"total"`
	// Evaluated is how many were evaluated: at most RetirementEvaluationLimit, oldest hibernated first.
	Evaluated int                 `json:"evaluated"`
	Verdicts  []RetirementVerdict `json:"verdicts"`
}

type retirementEntry struct {
	Kind           string  `json:"kind"`
	Path           string  `json:"path"`
	Classification string  `json:"classification"`
	RunID          *string `json:"run_id"`
	Tip            *string `json:"tip"`
	ContentState   *string `json:"content_state"`
}

type retirementPVC struct {
	Role               string `json:"role"`
	ClassReclaimPolicy string `json:"class_reclaim_policy"`
}

// retirementEvidence is one worker's evidence, gathered before the guards run.
type retirementEvidence struct {
	inv      *store.WorkerHibernationInventory
	entries  []retirementEntry
	pvcs     []retirementPVC
	entryErr bool
	// cancelled: waking, no row for the current op, and the latest row (any op) was accepted.
	cancelled bool
}

// EvaluateHibernationRetirement is the dry-run retirement report. It is read-only: one read
// transaction, every query batched over the evaluated worker ids except the per-worker inventory
// reads (the inventory row is keyed by worker and op), and one receipt lookup per worker.
func (s *Service) EvaluateHibernationRetirement(ctx context.Context) (RetirementReport, error) {
	var report RetirementReport
	if s.txBeginner == nil {
		return report, ErrHibernationUnavailable
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)

	if report.Total, err = q.CountHibernationRetirementCandidates(ctx); err != nil {
		return report, err
	}
	cands, err := q.ListHibernationRetirementCandidates(ctx, RetirementEvaluationLimit)
	if err != nil {
		return report, err
	}
	report.Evaluated = len(cands)
	report.Verdicts = make([]RetirementVerdict, 0, len(cands))
	if len(cands) == 0 {
		return report, nil
	}
	ids := make([]uuid.UUID, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	nonTerminal, err := workerSet(q.ListWorkersWithNonTerminalRuns(ctx, ids))
	if err != nil {
		return report, err
	}
	holds, err := workerSet(q.ListWorkersWithOpenCustodyHolds(ctx, ids))
	if err != nil {
		return report, err
	}
	replicas, err := workerSet(q.ListWorkersWithLocalReplicaCaptures(ctx, ids))
	if err != nil {
		return report, err
	}
	resume, err := workerSet(q.ListWorkersWithActivePublicationResumeCustody(ctx, ids))
	if err != nil {
		return report, err
	}
	adv, err := q.GetControllerHibernationAdvertisement(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return report, err
	}
	now := s.now()
	staleAfter := inventoryGapPolls * hibernationPollInterval(adv)

	for _, c := range cands {
		ev, err := gatherRetirementEvidence(ctx, q, c)
		if err != nil {
			return report, err
		}
		v := RetirementVerdict{WorkerID: c.ID}
		if ev.cancelled {
			v.Outcome = RetirementCancelled
			report.Verdicts = append(report.Verdicts, v)
			continue
		}
		add := func(code, source string) {
			v.BlockerCount++
			if len(v.Blockers) < retirementBlockerCap {
				v.Blockers = append(v.Blockers, RetirementBlocker{Code: code, Source: retirementSource(source)})
			}
		}

		// Guards 1 and 2: the lane and the inventory's own validity.
		hasDind := slices.ContainsFunc(ev.pvcs, func(p retirementPVC) bool { return p.Role == "dind_data" })
		if ev.inv != nil && !hasDind {
			add(BlockerLaneWithoutWriterExclusion, "no dind_data volume")
		}
		if ev.inv == nil {
			add(BlockerNoInventory, "no inventory for the current op")
		} else {
			inventoryGuards(ev.inv, ev.entryErr, c, now, staleAfter, add)
		}

		// Guard 3: extra refusals from the batched queries and the worker's own fields.
		if nonTerminal[c.ID] {
			add(BlockerNonTerminalRun, "non-terminal run assigned")
		}
		if holds[c.ID] {
			add(BlockerOpenCustodyHold, "open custody hold")
		}
		if replicas[c.ID] {
			add(BlockerLocalReplicaCapture, "local replica of a recovery capture")
		}
		if resume[c.ID] {
			add(BlockerPublicationResumeCustody, "active publication resume custodian")
		}
		if c.ManualCordonedAt.Valid {
			add(BlockerManualCordon, "manual cordon")
		}
		if c.DrainingSince.Valid {
			add(BlockerDraining, "draining")
		}
		if c.MaintenanceFenced {
			add(BlockerMaintenanceFenced, "maintenance fenced")
		}
		switch c.MaintenancePhase {
		case "requested", "ready", "stopping", "recycling":
			add(BlockerMaintenancePhase, c.MaintenancePhase)
		}

		// Guards 4 and 5: every entry, the receipts and the docker state.
		if ev.inv != nil && ev.inv.Status == InventoryAccepted {
			valid, err := validReceipts(ctx, q, ev.entries)
			if err != nil {
				return report, err
			}
			for _, e := range ev.entries {
				if code, source := entryBlocker(e, valid); code != "" {
					add(code, source)
				}
			}
		}

		// Guard 6: the period.
		if c.HibernatedAt.Valid {
			elapsed := now.Sub(c.HibernatedAt.Time)
			v.PastPeriod = elapsed >= HibernationRetirementPeriod
			if !v.PastPeriod {
				v.Remaining = HibernationRetirementPeriod - elapsed
			}
		} else {
			v.Remaining = HibernationRetirementPeriod
		}

		switch {
		case !v.PastPeriod:
			v.Outcome = RetirementNotYetEligible
		case v.BlockerCount > 0:
			v.Outcome = RetirementBlocked
		default:
			v.Outcome = RetirementConditionalRetire
			v.Unverified = []string{RetirementUnverifiedPVReclaimPolicy, RetirementUnverifiedInterveningPodExcluded}
		}
		for _, p := range ev.pvcs {
			v.ClassPolicies = append(v.ClassPolicies, ClaimReclaimPolicy{Role: p.Role, Policy: p.ClassReclaimPolicy})
			v.Retain = v.Retain || p.ClassReclaimPolicy == "Retain"
			v.Unknown = v.Unknown || p.ClassReclaimPolicy == "unknown"
		}
		report.Verdicts = append(report.Verdicts, v)
	}
	return report, nil
}

func workerSet(ids []uuid.UUID, err error) (map[uuid.UUID]bool, error) {
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}

// gatherRetirementEvidence reads the worker's inventory for its current op. A waking worker whose
// current op has no row, but whose most recent row (any op) was accepted, is cancelled: a wake
// rotated the op after the inventory was taken.
func gatherRetirementEvidence(ctx context.Context, q *store.Queries, c store.ListHibernationRetirementCandidatesRow) (retirementEvidence, error) {
	var ev retirementEvidence
	if c.HibernationOpID.Valid {
		row, err := q.GetHibernationInventoryForOp(ctx, store.GetHibernationInventoryForOpParams{
			WorkerID: c.ID, OpID: uuid.UUID(c.HibernationOpID.Bytes)})
		switch {
		case err == nil:
			ev.inv = &row
		case !errors.Is(err, pgx.ErrNoRows):
			return ev, err
		}
	}
	if ev.inv == nil {
		if c.HibernationState == "waking" {
			latest, err := q.GetHibernationInventoryStateForWorker(ctx, c.ID)
			switch {
			case err == nil:
				ev.cancelled = latest.Status == InventoryAccepted
			case !errors.Is(err, pgx.ErrNoRows):
				return ev, err
			}
		}
		return ev, nil
	}
	if err := json.Unmarshal(ev.inv.Pvcs, &ev.pvcs); err != nil {
		ev.pvcs = nil
	}
	if len(ev.inv.Entries) > 0 {
		if err := json.Unmarshal(ev.inv.Entries, &ev.entries); err != nil {
			ev.entryErr = true
		}
	}
	return ev, nil
}

// inventoryGuards applies guard 2 to an existing row.
func inventoryGuards(inv *store.WorkerHibernationInventory, entryErr bool, c store.ListHibernationRetirementCandidatesRow,
	now time.Time, staleAfter time.Duration, add func(code, source string)) {
	if inv.Status != InventoryAccepted {
		// Every non-accepted status carries no usable inventory, awaiting included.
		add(BlockerNoInventory, inv.Status)
		return
	}
	var uncovered []string
	if len(inv.Uncovered) > 0 {
		_ = json.Unmarshal(inv.Uncovered, &uncovered)
	}
	if !inv.Complete.Valid || !inv.Complete.Bool || len(uncovered) > 0 || entryErr || len(inv.Entries) == 0 {
		source := "inventory not complete"
		if len(uncovered) > 0 {
			source = "uncovered: " + strings.Join(uncovered, ",")
		}
		add(BlockerInventoryIncomplete, source)
	}
	if inv.InvalidatedReason.Valid {
		add(BlockerInventoryInvalidPrefix+inv.InvalidatedReason.String, inv.InvalidatedReason.String)
	}
	if !inv.ExclusionFinalAt.Valid {
		add(BlockerExclusionUntilTermination, "no final exclusion report")
	}
	if !inv.ScalingDownPodUid.Valid || uuid.UUID(inv.ScalingDownPodUid.Bytes) != inv.PodUid {
		add(BlockerScaleDownPodUnproven, "scaling_down pod differs from the inventoried pod")
	}
	if c.HibernationState == "hibernated" && (!inv.LastHeldAt.Valid || now.Sub(inv.LastHeldAt.Time) > staleAfter) {
		add(BlockerHoldChainStale, "held chain not observed recently")
	}
}

// validReceipts looks up the (run, tip) pairs of the worker's needs_receipt entries: one call, equal
// length arrays, an empty result when the worker has none.
func validReceipts(ctx context.Context, q *store.Queries, entries []retirementEntry) (map[[2]string]bool, error) {
	var runs []uuid.UUID
	var tips []string
	seen := map[[2]string]bool{}
	for _, e := range entries {
		if e.Classification != "needs_receipt" || e.RunID == nil || e.Tip == nil {
			continue
		}
		id, err := uuid.Parse(*e.RunID)
		key := [2]string{strings.ToLower(*e.RunID), *e.Tip}
		if err != nil || seen[key] {
			continue
		}
		seen[key] = true
		runs, tips = append(runs, id), append(tips, *e.Tip)
	}
	out := map[[2]string]bool{}
	if len(runs) == 0 {
		return out, nil
	}
	rows, err := q.ListValidCaptureReceipts(ctx, store.ListValidCaptureReceiptsParams{RunIds: runs, SourceShas: tips})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[[2]string{r.RunID.String(), r.SourceSha}] = true
	}
	return out, nil
}

// entryBlocker applies guards 4 and 5 to one entry; an empty code means the entry does not block.
func entryBlocker(e retirementEntry, valid map[[2]string]bool) (code, source string) {
	where := e.Kind + ":" + e.Path
	if e.Kind == InventoryKindDockerState {
		return BlockerDindState, e.Path
	}
	if !slices.Contains(retirementKinds, e.Kind) {
		return BlockerProtectedSource, where
	}
	if e.ContentState != nil && !slices.Contains(retirementContentStates, *e.ContentState) {
		return BlockerProtectedSource, where
	}
	proven := e.ContentState != nil && *e.ContentState == "content_proven"
	switch e.Classification {
	case "disposable":
		if (e.Kind == "canonical_clone" || e.Kind == "worktree") && !proven {
			return BlockerUncoveredWorktreeBytes, where
		}
	case "needs_receipt":
		if e.ContentState == nil || (*e.ContentState != "content_proven" && *e.ContentState != "ref") {
			return BlockerUncoveredWorktreeBytes, where
		}
		if e.RunID == nil || e.Tip == nil {
			return BlockerNoReceipt, where
		}
		if !valid[[2]string{strings.ToLower(*e.RunID), *e.Tip}] {
			return BlockerNoReceipt, *e.RunID
		}
	default:
		// protected, unknown, unrecoverable and any word this api does not know.
		return BlockerProtectedSource, where
	}
	return "", ""
}

// retirementSource flattens and bounds worker-written text before it reaches a report.
func retirementSource(raw string) string {
	text, _ := sanitizeHibernationText(raw)
	return truncateRunes(text, retirementSourceMax)
}

// String renders a blocker as the report shows it: "code (source)".
func (b RetirementBlocker) String() string {
	if b.Source == "" {
		return b.Code
	}
	return fmt.Sprintf("%s (%s)", b.Code, b.Source)
}
