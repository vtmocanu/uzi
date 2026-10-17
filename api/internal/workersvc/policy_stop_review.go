package workersvc

import (
	"bytes"
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
	"github.com/vtmocanu/uzi/api/internal/capability"
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
	if err != nil || source.Gate == nil || r.GateRevision <= 0 || !r.GatePresentationID.Valid || !r.PolicyLastOutcomeID.Valid ||
		!r.PolicyLastSourceGeneration.Valid || r.PolicyLastSourceGeneration.Int64 <= 0 || r.ClaimGeneration <= 0 || !r.StatusSince.Valid {
		return bad()
	}
	gate := apitypes.PolicyStopGateContext{Revision: stopDecimal(r.GateRevision), PresentationID: uuid.UUID(r.GatePresentationID.Bytes).String(), PayloadDigest: hex.EncodeToString(r.GatePayloadDigest)}
	if *source.Gate != gate {
		return bad()
	}
	payload, ok, err := decodeGatePresentedPayload(r.GatePresentedPayload)
	if err != nil || !ok {
		return bad()
	}
	_, sum, err := payload.digest()
	if err != nil || !bytes.Equal(sum, r.GatePayloadDigest) {
		return bad()
	}
	// Compare the persisted visible candidate, without witness-only presentation metadata.
	current := gatePayloadFromRow(r)
	visible := payload
	visible.ReviseCount = nil
	visible.CheckpointWitness = nil
	visible.WitnessWorkerID = nil
	visible.WitnessClaimGeneration = nil
	a, err := visible.canonicalJSON()
	if err != nil {
		return bad()
	}
	b, err := current.canonicalJSON()
	if err != nil || !bytes.Equal(a, b) {
		return bad()
	}
	frozen, err := DecodeMilestones(r.MilestonesFrozen)
	if err != nil || len(frozen) == 0 {
		return bad()
	}
	completed, err := DecodeMilestoneIDs(r.MilestonesCompleted)
	if err != nil || len(completed) == 0 || len(completed) > math.MaxInt32 {
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
	execution := apitypes.PolicyStopExecution{Contract: contract, Frozen: frozen, CompletedIDs: completed, ProposedCeiling: int32(len(completed))}
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
	m = apitypes.PolicyStopManifest{Version: 1, RunID: r.ID.String(), OutcomeID: uuid.UUID(r.PolicyLastOutcomeID.Bytes).String(), SourceGeneration: stopDecimal(r.PolicyLastSourceGeneration.Int64), ReviewGeneration: stopDecimal(r.ClaimGeneration), SourceContext: source, Hold: apitypes.PolicyStopHoldSnapshot{Reason: textPtr(r.HoldReason), PolicyDetail: textPtr(r.PolicyHoldDetail), Since: r.StatusSince.Time}, Gate: &apitypes.PolicyStopGateSnapshot{PolicyStopGateContext: gate, Payload: wp}, Execution: execution, HumanFeedback: []apitypes.PolicyStopHumanFeedback{}, CrossCheckFeedback: []apitypes.PolicyStopCrossCheckFeedback{}}
	inputs, err := qtx.ListPolicyStopRevisionInputs(ctx, r.ID)
	if err != nil {
		return m, err
	}
	primaryFound := source.PrimaryRevision == nil || source.PrimaryRevision.Source != "run_user_input"
	for _, input := range inputs {
		primary := source.PrimaryRevision != nil && source.PrimaryRevision.InputID != nil && *source.PrimaryRevision.InputID == stopDecimal(input.ID)
		if input.GateBinding.Valid && input.GateBinding.String == "bound" && input.GateRevision.Valid {
			if input.GateRevision.Int64 != r.GateRevision {
				if primary {
					return bad()
				}
				continue
			}
		} else {
			// Source context alone cannot prove an unbound row was delivered at this gate.
			if !primary || !input.ConsumedAt.Valid || !input.ConsumedWorkerID.Valid || !input.ConsumedClaimGeneration.Valid ||
				input.ConsumedClaimGeneration.Int64 != r.PolicyLastSourceGeneration.Int64 {
				return bad()
			}
		}
		if input.ID <= 0 || input.IncludedAt.Valid || input.Disposition.Valid {
			return bad()
		}
		if primary {
			primaryFound = true
		}
		m.HumanFeedback = append(m.HumanFeedback, apitypes.PolicyStopHumanFeedback{
			InputID: stopDecimal(input.ID), Body: textPtr(input.Body), CreatedAt: stopTime(input.CreatedAt), GateBinding: textPtr(input.GateBinding), GateRevision: stopOptionalDecimal(input.GateRevision),
			ConsumedAt: stopTime(input.ConsumedAt), ConsumedClaimGeneration: stopOptionalDecimal(input.ConsumedClaimGeneration), ConsumedWorkerID: stopUUID(input.ConsumedWorkerID),
			AppliedAt: stopTime(input.AppliedAt), IncludedAt: stopTime(input.IncludedAt), InclusionReported: input.InclusionReported, Disposition: textPtr(input.Disposition),
			SettledAt: stopTime(input.SettledAt), SupersededByStopInputID: stopOptionalDecimal(input.SupersededByStopInputID)})
	}
	if !primaryFound {
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
		if cc.LeadRunID != r.ID || cc.Stage != "plan" || cc.Round < 1 || cc.LeadClaimGeneration <= 0 ||
			(cc.Verdict != "revise" && cc.Verdict != "approve") || !cc.DecidedAt.Valid || cc.RevisionSettledAt.Valid || cc.RevisionDisposition.Valid {
			return bad()
		}
		candidate := PlanCrossCheckCandidate{PlanMd: cc.PlanMd.String, Milestones: cc.Milestones, RequiredCapabilities: cc.RequiredCapabilities, RequiredTools: cc.RequiredTools, SizeClass: cc.SizeClass.String, BaseCommit: cc.BaseCommit.String, PlanningDiff: cc.PlanningDiff.String}
		digest, err := candidate.Digest()
		if err != nil || !bytes.Equal(digest, cc.CandidateDigest) {
			return bad()
		}
		projection := gatePresentedPayload{PlanMd: textPtr(cc.PlanMd), Milestones: cc.Milestones, RequiredCapabilities: capability.Filter(cc.RequiredCapabilities), RequiredTools: capability.FilterTools(cc.RequiredTools), SizeClass: nonEmptyPtr(cc.SizeClass.String)}
		projected, err := projection.canonicalJSON()
		if err != nil || !bytes.Equal(a, projected) {
			return bad()
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
