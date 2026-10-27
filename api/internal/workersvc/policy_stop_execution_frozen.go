package workersvc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// policyStopFrozenExecutionTarget checks execution identity only. The caller must
// already hold the current run lock, authenticate its source/gate, and parse the
// immutable accepted receipt. q must be bound to that same transaction; this
// interface cannot prove transaction custody. Success grants no Claim/ACK or
// approval permission. Mutable planning is consulted only for enrollment.
func policyStopFrozenExecutionTarget(ctx context.Context, q policyRevisionIdentityReader, r store.Run, e apitypes.PolicyStopExecution, noGate bool) (*apitypes.SemanticCandidate, bool, error) {
	if q == nil || (reflect.ValueOf(q).Kind() == reflect.Ptr && reflect.ValueOf(q).IsNil()) {
		return nil, false, nil
	}
	frozen, err := DecodeMilestones(r.MilestonesFrozen)
	if err != nil || len(frozen) == 0 || len(e.Frozen) == 0 {
		return nil, false, nil
	}
	if _, ok := validateMilestones(frozen); !ok {
		return nil, false, nil
	}
	if _, ok := validateMilestones(e.Frozen); !ok || !slices.Equal(frozen, e.Frozen) {
		return nil, false, nil
	}
	completed, err := DecodeMilestoneIDs(r.MilestonesCompleted)
	if err != nil || !policyStopFrozenCompletedEqual(frozen, completed, e.CompletedIDs) ||
		int64(e.ProposedCeiling) != int64(len(completed)) ||
		!r.ScopeCeiling.Valid || r.ScopeCeiling.Int32 != e.ProposedCeiling {
		return nil, false, nil
	}
	if (len(bytes.TrimSpace(r.CompletionContract)) != 0 && !json.Valid(r.CompletionContract)) || (len(bytes.TrimSpace(e.Contract)) != 0 && !json.Valid(e.Contract)) {
		return nil, false, nil
	}
	live, err := canonicalRawJSON(r.CompletionContract)
	if err != nil {
		return nil, false, nil
	}
	saved, err := canonicalRawJSON(e.Contract)
	if err != nil || !bytes.Equal(live, saved) ||
		r.CompletionContractVersion.Valid != (e.ContractVersion != nil) ||
		r.ContractRevision.Valid != (e.ContractRevision != nil) ||
		r.CompletionContractVersion.Valid != r.ContractRevision.Valid {
		return nil, false, nil
	}
	if e.ContractVersion != nil {
		revision, err := strconv.ParseInt(string(*e.ContractRevision), 10, 32)
		if err != nil || e.ContractRevision.Validate() != nil || r.CompletionContractVersion.Int32 != *e.ContractVersion || r.ContractRevision.Int32 != int32(revision) {
			return nil, false, nil
		}
	}
	_, enrolled, err := policyRevisionPlanning(ctx, q, r)
	if errors.Is(err, ErrSemanticGateMismatch) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	routed := semanticCompletionRequiresLock(r)
	if !r.CompletionContractVersion.Valid {
		return nil, bytes.Equal(live, []byte("null")) && !enrolled && !routed, nil
	}
	if r.CompletionContractVersion.Int32 != 1 || r.ContractRevision.Int32 < 1 {
		return nil, false, nil
	}
	c, err := parseCompletionContract(r.CompletionContract)
	if err != nil {
		return nil, false, nil
	}
	if _, err := completionPendingFromContract(r, c); err != nil {
		return nil, false, nil
	}
	switch c.Profile {
	case contractProfileStructural:
		if enrolled || routed {
			return nil, false, nil
		}
		_, valid := structuralAuthority(r, frozen)
		return nil, valid, nil
	case "semantic":
		target := c.SemanticPresentation.Candidate
		valid, err := policyRevisionSemanticTarget(ctx, q, r, &target)
		if err != nil || !valid {
			return nil, false, err
		}
		if noGate && validateSemanticGateFields(policyAutopilotPayloadFromRow(r), c.SemanticPresentation) != nil {
			return nil, false, nil
		}
		return &target, true, nil
	default:
		return nil, false, nil
	}
}

// Both lists are bounded by the validated frozen list (at most 50). Every
// duplicate/foreign ID refuses the whole comparison; order alone is irrelevant.
func policyStopFrozenCompletedEqual(frozen []Milestone, live, saved []string) bool {
	if len(live) == 0 || len(live) != len(saved) || len(live) > len(frozen) {
		return false
	}
	members := make(map[string]bool, len(frozen))
	for _, m := range frozen {
		members[m.ID] = true
	}
	seen := make(map[string]bool, len(live))
	for _, id := range live {
		if !members[id] || seen[id] {
			return false
		}
		seen[id] = true
	}
	for _, id := range saved {
		if !seen[id] {
			return false
		}
		delete(seen, id)
	}
	return len(seen) == 0
}
