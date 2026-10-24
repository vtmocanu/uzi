package workersvc

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PolicyStopReviewConflict is a read-only preview refusal.
type PolicyStopReviewConflict struct{ Reason string }

func (e *PolicyStopReviewConflict) Error() string { return e.Reason }
func stopReviewConflict(reason string) error      { return &PolicyStopReviewConflict{Reason: reason} }

// PolicyStopReviewForOwner reads a preview under the owner run lock. Activation is
// deliberately independent of the existing hold protocol and defaults to false.
func (s *Service) PolicyStopReviewForOwner(ctx context.Context, owner, runID uuid.UUID, presentedHoldIfMatch string) (apitypes.PolicyStopReview, error) {
	var out apitypes.PolicyStopReview
	if s.txBeginner == nil {
		return out, stopReviewConflict("policy_stop_review_unverifiable")
	}
	tx, err := s.beginReadCommitted(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)
	// This MUST be its own statement: child inventory uses later READ COMMITTED snapshots.
	r, err := qtx.GetRunByIDForUpdate(ctx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrRunNotOwned
	}
	if err != nil {
		return out, err
	}
	if r.UserID != owner {
		return out, ErrRunNotOwned
	}
	if presentedHoldIfMatch == "" || !strings.HasPrefix(presentedHoldIfMatch, "\"policy-") || strings.HasPrefix(presentedHoldIfMatch, "\"policy-stop-") {
		return out, stopReviewConflict("policy_stop_hold_token_required")
	}
	if presentedHoldIfMatch != PolicyHoldActionToken(r) {
		return out, stopReviewConflict("policy_stop_hold_token_stale")
	}
	if !s.p.PolicyStopNegotiated || !s.p.PolicyHoldNegotiated || !s.p.PolicyHoldSnapshotEnabled || s.p.TerminalPendingLease <= 0 ||
		!policyEligible(r) || !PolicyOwnerCanStop(r, s.p.RunTimeout) {
		return out, stopReviewConflict("policy_stop_review_unverifiable")
	}
	manifest, err := policyStopManifestLocked(ctx, qtx, r)
	if err != nil {
		return out, err
	}
	token, err := policyStopManifestToken(manifest)
	if err != nil {
		return out, err
	}
	out = apitypes.PolicyStopReview{Manifest: manifest, StopIfMatch: token}
	// Rollback releases the lock; there are no writes to commit.
	return out, nil
}

// policyStopManifestLocked is shared with future atomic confirmation. The caller
// must hold the run lock acquired in a separate statement in READ COMMITTED.
// Source bodies and provenance come exclusively from persisted rows.
func policyStopManifestLocked(ctx context.Context, qtx *store.Queries, r store.Run) (apitypes.PolicyStopManifest, error) {
	var m apitypes.PolicyStopManifest
	bad := func() (apitypes.PolicyStopManifest, error) {
		return apitypes.PolicyStopManifest{}, stopReviewConflict("policy_stop_review_unverifiable")
	}
	source, err := apitypes.ParsePolicyRevisionSourceV1(r.PolicyRevisionSource)
	if err != nil || !r.PolicyLastOutcomeID.Valid ||
		!r.PolicyLastSourceGeneration.Valid || r.PolicyLastSourceGeneration.Int64 <= 0 || r.ClaimGeneration <= 0 || !r.StatusSince.Valid {
		return bad()
	}
	selected := stopUUID(r.PolicyCandidateCrossCheckID)
	if !policyStopSelectionValid(source, selected) {
		return bad()
	}
	noGate := source.Gate == nil
	var gateSnapshot *apitypes.PolicyStopGateSnapshot
	var payload gatePresentedPayload
	matched, matchErr := policyRevisionSourceGateMatches(ctx, qtx, r, source)
	if matchErr != nil {
		return m, matchErr
	}
	if !matched {
		return bad()
	}
	var target *apitypes.SemanticCandidate
	if noGate {
		if source.PrimaryRevision != nil || !policyRevisionNoGatePlanApproved(r) {
			return bad()
		}
		payload = policyAutopilotPayloadFromRow(r)
		var valid bool
		target, valid, err = policyRevisionAcceptedTarget(ctx, qtx, r)
		if err != nil {
			return m, err
		}
		if !valid {
			return bad()
		}
	} else {
		if r.GateRevision <= 0 || !r.GatePresentationID.Valid {
			return bad()
		}
		gate := apitypes.PolicyStopGateContext{Revision: stopDecimal(r.GateRevision), PresentationID: uuid.UUID(r.GatePresentationID.Bytes).String(), PayloadDigest: hex.EncodeToString(r.GatePayloadDigest)}
		if *source.Gate != gate {
			return bad()
		}
		var ok bool
		payload, ok, err = decodeGatePresentedPayload(r.GatePresentedPayload)
		if err != nil || !ok {
			return bad()
		}
		if payload.Semantic != nil {
			target = &payload.Semantic.Candidate
		}
		gateSnapshot = &apitypes.PolicyStopGateSnapshot{PolicyStopGateContext: gate}
	}
	frozen, err := DecodeMilestones(r.MilestonesFrozen)
	if err != nil || len(frozen) == 0 {
		return bad()
	}
	completed, err := DecodeMilestoneIDs(r.MilestonesCompleted)
	if err != nil || len(completed) == 0 {
		return bad()
	}
	members := map[string]bool{}
	for _, v := range frozen {
		if v.ID == "" || members[v.ID] {
			return bad()
		}
		members[v.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range completed {
		if !members[id] || seen[id] {
			return bad()
		}
		seen[id] = true
	}
	slices.Sort(completed)
	contract, err := canonicalRawJSON(r.CompletionContract)
	if err != nil {
		return bad()
	}
	if r.CompletionContractVersion.Valid != r.ContractRevision.Valid {
		return bad()
	}
	if r.CompletionContractVersion.Valid {
		if _, valid := scopeCapEligibility(r); !valid {
			return bad()
		}
	} else if string(contract) != "null" {
		return bad()
	}
	source.AssociatedCrossChecks = append([]apitypes.PolicyRevisionRef{}, source.AssociatedCrossChecks...)
	slices.SortFunc(source.AssociatedCrossChecks, func(a, b apitypes.PolicyRevisionRef) int { return strings.Compare(*a.CrossCheckID, *b.CrossCheckID) })
	milestones, err := canonicalRawJSON(payload.Milestones)
	if err != nil {
		return bad()
	}
	wp := apitypes.PolicyStopGatePayload{ReviseCount: payload.ReviseCount, CheckpointWitness: payload.CheckpointWitness, PlanMd: payload.PlanMd, Milestones: milestones, RequiredCapabilities: sortedSet(payload.RequiredCapabilities), RequiredTools: sortedSet(payload.RequiredTools), SizeClass: payload.SizeClass}
	if payload.WitnessWorkerID != nil {
		v := payload.WitnessWorkerID.String()
		wp.WitnessWorkerID = &v
	}
	if payload.WitnessClaimGeneration != nil {
		v := stopDecimal(*payload.WitnessClaimGeneration)
		if v.Validate() != nil {
			return bad()
		}
		wp.WitnessClaimGeneration = &v
	}
	if gateSnapshot != nil {
		gateSnapshot.Payload = wp
	}
	completedCount := len(completed)
	if completedCount < 1 || completedCount > math.MaxInt32 {
		return bad()
	}
	execution := apitypes.PolicyStopExecution{Contract: contract, Frozen: frozen, CompletedIDs: completed, ProposedCeiling: int32(completedCount)}
	if r.ScopeCeiling.Valid {
		v := r.ScopeCeiling.Int32
		if v < 0 {
			return bad()
		}
		execution.CurrentCeiling = &v
	}
	if r.CompletionContractVersion.Valid {
		v := r.CompletionContractVersion.Int32
		execution.ContractVersion = &v
		rev := stopDecimal(int64(r.ContractRevision.Int32))
		if rev.Validate() != nil {
			return bad()
		}
		execution.ContractRevision = &rev
	}
	m = apitypes.PolicyStopManifest{CandidateCrossCheckID: selected, Version: 1, RunID: r.ID.String(), OutcomeID: uuid.UUID(r.PolicyLastOutcomeID.Bytes).String(), SourceGeneration: stopDecimal(r.PolicyLastSourceGeneration.Int64), ReviewGeneration: stopDecimal(r.ClaimGeneration), SourceContext: source, Hold: apitypes.PolicyStopHoldSnapshot{Reason: textPtr(r.HoldReason), PolicyDetail: textPtr(r.PolicyHoldDetail), Since: r.StatusSince.Time}, Gate: gateSnapshot, Execution: execution, HumanFeedback: []apitypes.PolicyStopHumanFeedback{}, CrossCheckFeedback: []apitypes.PolicyStopCrossCheckFeedback{}}
	inputs, err := qtx.ListPolicyStopRevisionInputs(ctx, r.ID)
	if err != nil {
		return m, err
	}
	primaryFound := source.PrimaryRevision == nil || source.PrimaryRevision.Source != "run_user_input"
	for _, input := range inputs {
		if noGate {
			// At verified gate zero, no positively bound older gate exists.
			// Every inventoried revision is applicable, as in the approval reader.
			return bad()
		}
		primary := source.PrimaryRevision != nil && source.PrimaryRevision.InputID != nil && *source.PrimaryRevision.InputID == stopDecimal(input.ID)
		if input.GateBinding.Valid && input.GateBinding.String == "bound" && input.GateRevision.Valid {
			if input.GateRevision.Int64 != r.GateRevision {
				if primary || input.GateRevision.Int64 <= 0 || input.GateRevision.Int64 > r.GateRevision {
					return bad()
				}
				continue
			}
		} else {
			if (input.GateBinding.Valid && input.GateBinding.String != "unbound") || (input.GateRevision.Valid && (input.GateRevision.Int64 < 0 || input.GateRevision.Int64 > r.GateRevision)) {
				return bad()
			}
			// Source context alone cannot prove an unbound row was delivered at this gate.
			if !primary || !input.ConsumedAt.Valid || !input.ConsumedWorkerID.Valid || !input.ConsumedClaimGeneration.Valid ||
				input.ConsumedClaimGeneration.Int64 != r.PolicyLastSourceGeneration.Int64 {
				return bad()
			}
		}
		if input.ID <= 0 || input.IncludedAt.Valid || input.Disposition.Valid || input.SettledAt.Valid || input.SupersededByStopInputID.Valid {
			return bad()
		}
		if primary {
			primaryFound = true
		}
		m.HumanFeedback = append(m.HumanFeedback, policyStopHumanSnapshot(input))
	}
	var metadata struct {
		Receipt *revisionRetryReceipt `json:"revision_retry_receipt"`
	}
	if !noGate {
		if json.Unmarshal(r.GatePresentedPayload, &metadata) != nil {
			return bad()
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(r.GatePresentedPayload, &fields) != nil {
			return bad()
		}
		if _, present := fields["revision_retry_receipt"]; present && metadata.Receipt == nil {
			return bad()
		}
	}
	var historyID *apitypes.PolicyStopDecimal
	if !primaryFound {
		historyID = source.PrimaryRevision.InputID
	} else if metadata.Receipt != nil {
		id := stopDecimal(metadata.Receipt.RevisionInputID)
		historyID = &id
	}
	if historyID != nil {
		history, valid, err := policyStopHumanHistory(ctx, qtx, r, *historyID)
		if err != nil {
			return m, err
		}
		if !valid {
			return bad()
		}
		for _, input := range history {
			m.HumanFeedback = append(m.HumanFeedback, policyStopHumanSnapshot(input))
		}
	}
	slices.SortFunc(m.HumanFeedback, func(a, b apitypes.PolicyStopHumanFeedback) int {
		ai, _ := strconv.ParseInt(string(a.InputID), 10, 64)
		bi, _ := strconv.ParseInt(string(b.InputID), 10, 64)
		return cmp.Compare(ai, bi)
	})
	if !policyStopHumanSnapshotsValid(m) {
		return bad()
	}
	refs := append([]apitypes.PolicyRevisionRef{}, source.AssociatedCrossChecks...)
	if source.PrimaryRevision != nil && source.PrimaryRevision.Source == "plan_cross_check" {
		refs = append(refs, *source.PrimaryRevision)
	}
	slices.SortFunc(refs, func(a, b apitypes.PolicyRevisionRef) int { return strings.Compare(*a.CrossCheckID, *b.CrossCheckID) })
	// Exactly the finite stored reference set is read once; any bad witness refuses the
	// entire preview. No retry, latest lookup, or history sweep can substitute evidence.
	for _, ref := range refs {
		id, err := uuid.Parse(*ref.CrossCheckID)
		if err != nil {
			return bad()
		}
		cc, err := qtx.GetPolicyStopCrossCheck(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return bad()
		}
		if err != nil {
			return m, err
		}
		if cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round < 1 || cc.LeadClaimGeneration <= 0 || cc.LeadClaimGeneration > r.ClaimGeneration || cc.RevisionSupersededByStopInputID.Valid ||
			(cc.Verdict != "revise" && cc.Verdict != "approve") || !cc.DecidedAt.Valid || cc.RevisionSettledAt.Valid || cc.RevisionDisposition.Valid {
			return bad()
		}
		if noGate && (cc.Verdict != "approve" || cc.LeadClaimGeneration > r.ClaimGeneration || cc.RevisionSupersededByStopInputID.Valid) {
			return bad()
		}
		valid, err := policyRevisionIdentityValid(ctx, qtx, cc)
		if err != nil {
			return m, err
		}
		if !valid {
			return bad()
		}
		if selected != nil && *selected == id.String() {
			applicable, err := policyRevisionIdentityApplicable(ctx, qtx, r, cc, target, noGate)
			if err != nil {
				return m, err
			}
			if !applicable {
				return bad()
			}
		}
		if source.PrimaryRevision != nil && source.PrimaryRevision.CrossCheckID != nil && *source.PrimaryRevision.CrossCheckID == id.String() && cc.Verdict != "revise" {
			return bad()
		}
		// A pass remains in the manifest as an association witness, with supersede=false.
		ms, err := canonicalRawJSON(cc.Milestones)
		if err != nil {
			return bad()
		}
		findings, err := canonicalRawJSON(cc.Findings)
		if err != nil {
			return bad()
		}
		m.CrossCheckFeedback = append(m.CrossCheckFeedback, apitypes.PolicyStopCrossCheckFeedback{
			CrossCheckID: cc.ID.String(), LeadRunID: cc.LeadRunID.String(), Stage: cc.Stage, Round: cc.Round, CandidateGeneration: stopDecimal(cc.LeadClaimGeneration), CandidateDigest: hex.EncodeToString(cc.CandidateDigest),
			Candidate: apitypes.PolicyStopCrossCheckCandidate{PlanMd: textPtr(cc.PlanMd), Milestones: ms, RequiredCapabilities: sortedSet(cc.RequiredCapabilities), RequiredTools: sortedSet(cc.RequiredTools), SizeClass: textPtr(cc.SizeClass), BaseCommit: textPtr(cc.BaseCommit), PlanningDiff: textPtr(cc.PlanningDiff)},
			Verdict:   cc.Verdict, Supersede: cc.Verdict == "revise", ReasonClass: textPtr(cc.ReasonClass), Findings: findings, CreatedAt: stopTime(cc.CreatedAt), DecidedAt: stopTime(cc.DecidedAt), DeadlineAt: stopTime(cc.DeadlineAt), InterruptedAt: stopTime(cc.InterruptedAt), FinalizedAt: stopTime(cc.FinalizedAt),
			AutomaticRevisionLimit: cc.AutomaticRevisionLimit, AutomaticRoundsEnabled: cc.AutomaticRoundsEnabled, CheckerRunID: stopUUID(cc.CheckerRunID), CheckerHarness: textPtr(cc.CheckerHarness), CheckerModel: textPtr(cc.CheckerModel), CheckerEffort: textPtr(cc.CheckerEffort), CheckerModelSource: textPtr(cc.CheckerModelSource), CheckerEffortSource: textPtr(cc.CheckerEffortSource),
			RevisionDisposition: textPtr(cc.RevisionDisposition), RevisionSettledAt: stopTime(cc.RevisionSettledAt), RevisionSupersededByStopInputID: stopOptionalDecimal(cc.RevisionSupersededByStopInputID)})
	}
	return m, nil
}

// policyStopSelectionValid checks selection consistency after closed V1 parsing.
// Row content is validated by policyStopManifestLocked's finite reference traversal.
func policyStopSelectionValid(source apitypes.PolicyRevisionSourceV1, selected *string) bool {
	refs := source.AssociatedCrossChecks
	primary := source.PrimaryRevision
	hasPrimary := primary != nil && primary.Source == "plan_cross_check"
	if selected == nil {
		return !hasPrimary && len(refs) == 0
	}
	id, err := uuid.Parse(*selected)
	if err != nil || id == uuid.Nil || id.String() != *selected {
		return false
	}
	if hasPrimary && primary.CrossCheckID != nil && *primary.CrossCheckID == *selected {
		return true
	}
	for _, ref := range refs {
		if ref.CrossCheckID != nil && *ref.CrossCheckID == *selected {
			return true
		}
	}
	return false
}

func policyStopManifestToken(m apitypes.PolicyStopManifest) (string, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalRawJSON(raw)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("\"policy-stop-v1-%x\"", sum), nil
}

// policyStopHumanHistory reads only the active identity and its canonical finite
// predecessor set. Each is attempted once; any bad proof stops sibling reads and
// database errors abort the caller's transaction.
func policyStopHumanHistory(ctx context.Context, q *store.Queries, r store.Run, active apitypes.PolicyStopDecimal) ([]store.RunUserInput, bool, error) {
	id, err := strconv.ParseInt(string(active), 10, 64)
	if err != nil || active.Validate() != nil {
		return nil, false, nil
	}
	activeRow, err := q.GetPolicyStopRevisionInput(ctx, store.GetPolicyStopRevisionInputParams{RunID: r.ID, InputID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return policyStopHumanHistoryFromActive(ctx, q, r, activeRow)
}

// Validate the already-read active proof before visiting its finite, ordered
// predecessor set. A negative proof stops sibling reads; no identity is retried.
func policyStopHumanHistoryFromActive(ctx context.Context, q *store.Queries, r store.Run, activeRow store.RunUserInput) ([]store.RunUserInput, bool, error) {
	receipt, valid := policyStopHistoryReceipt(r.GatePresentedPayload, activeRow.ID, r.GateRevision)
	if !valid || !policyStopHumanHistorySnapshot(policyStopHumanSnapshot(activeRow), stopDecimal(receipt.SourceRevision), stopDecimal(r.ClaimGeneration), true) {
		return nil, false, nil
	}
	rows := make([]store.RunUserInput, 0, len(receipt.SupersededIDs)+1)
	for _, inputID := range receipt.SupersededIDs {
		row, err := q.GetPolicyStopRevisionInput(ctx, store.GetPolicyStopRevisionInputParams{RunID: r.ID, InputID: inputID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		snapshot := policyStopHumanSnapshot(row)
		if !policyStopHumanHistorySnapshot(snapshot, stopDecimal(receipt.SourceRevision), stopDecimal(r.ClaimGeneration), false) {
			return nil, false, nil
		}
		if row.ConsumedWorkerID != activeRow.ConsumedWorkerID || row.ConsumedClaimGeneration != activeRow.ConsumedClaimGeneration || !row.AppliedAt.Time.Equal(activeRow.AppliedAt.Time) {
			return nil, false, nil
		}
		rows = append(rows, row)
	}
	return append(rows, activeRow), true, nil
}

// Receipt metadata is outside the gate digest, so validate its complete closed
// structure independently. APPLIED alone never proves publication linkage.
func policyStopHistoryReceipt(raw []byte, active, gate int64) (revisionRetryReceipt, bool) {
	var receipt revisionRetryReceipt
	if policyStopUniqueJSON(raw) != nil {
		return receipt, false
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil {
		return receipt, false
	}
	encoded, ok := metadata["revision_retry_receipt"]
	if !ok {
		return receipt, false
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(encoded, &fields) != nil || len(fields) < 2 || len(fields) > 3 {
		return receipt, false
	}
	for key, value := range fields {
		if key != "source_revision" && key != "revision_input_id" && key != "superseded_revision_input_ids" {
			return receipt, false
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return receipt, false
		}
	}
	if _, ok := fields["source_revision"]; !ok {
		return receipt, false
	}
	if _, ok := fields["revision_input_id"]; !ok {
		return receipt, false
	}
	if json.Unmarshal(encoded, &receipt) != nil || active <= 0 || active > maxSafeRevisionID ||
		receipt.RevisionInputID != active || receipt.SourceRevision <= 0 || receipt.SourceRevision >= gate ||
		len(receipt.SupersededIDs) > 1000 {
		return receipt, false
	}
	previous := int64(0)
	for _, id := range receipt.SupersededIDs {
		if id <= previous || id >= active {
			return receipt, false
		}
		previous = id
	}
	return receipt, true
}

func policyStopHumanSnapshot(input store.RunUserInput) apitypes.PolicyStopHumanFeedback {
	return apitypes.PolicyStopHumanFeedback{
		InputID: stopDecimal(input.ID), Body: textPtr(input.Body), CreatedAt: stopTime(input.CreatedAt),
		GateBinding: textPtr(input.GateBinding), GateRevision: stopOptionalDecimal(input.GateRevision),
		ConsumedAt: stopTime(input.ConsumedAt), ConsumedClaimGeneration: stopOptionalDecimal(input.ConsumedClaimGeneration), ConsumedWorkerID: stopUUID(input.ConsumedWorkerID),
		AppliedAt: stopTime(input.AppliedAt), IncludedAt: stopTime(input.IncludedAt), InclusionReported: input.InclusionReported,
		Disposition: textPtr(input.Disposition), SettledAt: stopTime(input.SettledAt), SupersededByStopInputID: stopOptionalDecimal(input.SupersededByStopInputID)}
}

func stopDecimal(v int64) apitypes.PolicyStopDecimal {
	return apitypes.PolicyStopDecimal(strconv.FormatInt(v, 10))
}
func stopOptionalDecimal(v pgtype.Int8) *apitypes.PolicyStopDecimal {
	if !v.Valid {
		return nil
	}
	out := stopDecimal(v.Int64)
	return &out
}
func stopUUID(v pgtype.UUID) *string {
	if !v.Valid {
		return nil
	}
	out := uuid.UUID(v.Bytes).String()
	return &out
}
func stopTime(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	out := v.Time.UTC()
	return &out
}
