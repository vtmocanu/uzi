package workersvc

import (
	"encoding/json"
	"errors"
	"sort"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// No evidence audit is performed here. These checks preserve the frozen worker
// obligations and exact human authority inherited from semanticGateContract.
var ErrSemanticCompletionAuthority = errors.New("semantic completion authority unavailable")

type maintainerAuthorization struct {
	apitypes.SemanticMaintainerApproval
	Revision       int    `json:"revision"`
	DecisionReason string `json:"decision_reason,omitempty"`
}

func completionHasPending(run store.Run) bool {
	pending, err := CompletionPending(run)
	return err == nil && pending != nil && len(pending.Criteria) > 0
}

func semanticMaintainers(c completionContract) []apitypes.SemanticMaintainerApproval {
	out := []apitypes.SemanticMaintainerApproval{}
	for _, cr := range c.Criteria {
		if cr.Kind == "maintainer" && cr.Maintainer != nil {
			out = append(out, apitypes.SemanticMaintainerApproval{ID: cr.ID, SemanticMaintainer: *cr.Maintainer})
		}
	}
	return out
}

func freezeMaintainerAuthorization(c *completionContract) error {
	want := semanticMaintainers(*c)
	if c.SemanticPresentation == nil || !sameSemanticJSON(want, c.SemanticPresentation.Maintainers) {
		return ErrSemanticCompletionAuthority
	}
	if len(want) > 0 && (c.SemanticApproval == nil ||
		!sameSemanticJSON(want, c.SemanticApproval.Maintainers)) {
		return ErrSemanticCompletionAuthority
	}
	for _, m := range want {
		c.MaintainerAuthorizations = append(c.MaintainerAuthorizations, maintainerAuthorization{SemanticMaintainerApproval: m, Revision: 1})
	}
	return nil
}

// validateSemanticCompletion compares all immutable semantic fields against the
// frozen presentation, then checks every authorization, including foreign and
// duplicate entries. Work is bounded by M1's 256 criteria and 50 milestones.
func validateSemanticCompletion(c completionContract) error {
	if c.Profile != "semantic" || c.Revision < 1 || c.Criteria == nil || c.SemanticPresentation == nil {
		return ErrSemanticCompletionAuthority
	}
	baseRaw, err := buildSemanticCompletionContract(c.SemanticPresentation.Candidate)
	if err != nil {
		return ErrSemanticCompletionAuthority
	}
	var base completionContract
	if json.Unmarshal(baseRaw, &base) != nil {
		return ErrSemanticCompletionAuthority
	}
	immutable := c
	immutable.Revision = 1
	immutable.Scope, immutable.Accepted = nil, nil
	immutable.SemanticApproval, immutable.SemanticPresentation = nil, nil
	immutable.MaintainerAuthorizations = nil
	if !sameSemanticJSON(immutable, base) {
		return ErrSemanticCompletionAuthority
	}
	want := semanticMaintainers(c)
	if !sameSemanticJSON(want, c.SemanticPresentation.Maintainers) ||
		(len(want) > 0 && (c.SemanticApproval == nil || !sameSemanticJSON(want, c.SemanticApproval.Maintainers))) {
		return ErrSemanticCompletionAuthority
	}
	if c.SemanticPresentation.Binding != SemanticCoverageBinding(c.SemanticPresentation.Candidate) {
		return ErrSemanticCompletionAuthority
	}
	if err := validateSemanticCompletionBounds(c); err != nil {
		return err
	}
	if c.SemanticApproval != nil {
		a, p := c.SemanticApproval, c.SemanticPresentation
		if a.PresentationRevision < 1 || a.PresentationDigest == "" ||
			a.CandidateIdentityVersion != p.Candidate.CandidateIdentityVersion ||
			a.CandidateRevision != p.Candidate.Revision ||
			a.SemanticCoverageBinding != p.Binding || a.ClaimGeneration != p.ClaimGeneration ||
			a.ReviewID != p.ReviewID || a.InvocationID != p.InvocationID || a.ReportDigest != p.ReportDigest {
			return ErrSemanticCompletionAuthority
		}
	}
	if len(want) != len(c.MaintainerAuthorizations) {
		return ErrSemanticCompletionAuthority
	}
	byID := make(map[string]apitypes.SemanticMaintainerApproval, len(want))
	for _, m := range want {
		byID[m.ID] = m
	}
	seen := map[string]bool{}
	for _, a := range c.MaintainerAuthorizations {
		m, ok := byID[a.ID]
		if !ok || seen[a.ID] || a.Revision < 1 || a.Revision > c.Revision ||
			!sameSemanticJSON(m, a.SemanticMaintainerApproval) || (a.Revision == 1 && a.DecisionReason != "") || (a.Revision > 1 && !semanticText(a.DecisionReason, maxCompletionReasonBytes)) {
			return ErrSemanticCompletionAuthority
		}
		seen[a.ID] = true
	}
	// Semantic accept cannot waive worker work or claim a maintainer performed it.
	if len(c.Accepted) != 0 {
		return ErrSemanticCompletionAuthority
	}
	return nil
}

// CompletionPending is the same-contract projector used by claim, ACK, permit,
// and full owner reads. A corrupt authorization returns an error, never [].
func CompletionPending(run store.Run) (*apitypes.CompletionPendingDTO, error) {
	if len(run.CompletionContract) == 0 {
		return nil, nil
	}
	c, err := parseCompletionContract(run.CompletionContract)
	if err != nil {
		return nil, err
	}
	if c.Profile == contractProfileStructural {
		if hasSemanticCompletionFields(c) {
			return nil, ErrSemanticCompletionAuthority
		}
		return nil, nil
	}
	if !run.ContractRevision.Valid || c.Revision != int(run.ContractRevision.Int32) ||
		!run.CompletionContractVersion.Valid || run.CompletionContractVersion.Int32 != 1 ||
		validateSemanticCompletion(c) != nil {
		return nil, ErrSemanticCompletionAuthority
	}
	ms, err := DecodeMilestones(run.MilestonesFrozen)
	if _, valid := validateMilestones(ms); err != nil || !valid {
		return nil, ErrSemanticCompletionAuthority
	}
	members := map[string]bool{}
	for _, m := range ms {
		if m.ID == "" || members[m.ID] {
			return nil, ErrSemanticCompletionAuthority
		}
		members[m.ID] = true
	}
	for _, cr := range c.Criteria {
		if !members[cr.MilestoneID] {
			return nil, ErrSemanticCompletionAuthority
		}
	}
	if c.Scope != nil {
		partition := map[string]bool{}
		for _, id := range c.Scope.In {
			if !members[id] || partition[id] {
				return nil, ErrSemanticCompletionAuthority
			}
			partition[id] = true
		}
		for _, d := range c.Scope.Out {
			if !members[d.MilestoneID] || partition[d.MilestoneID] || d.Reason == "" || d.Revision < 1 || d.Revision > c.Revision {
				return nil, ErrSemanticCompletionAuthority
			}
			partition[d.MilestoneID] = true
		}
		if len(partition) != len(members) {
			return nil, ErrSemanticCompletionAuthority
		}
	}
	out := &apitypes.CompletionPendingDTO{Version: 1, ContractRevision: c.Revision, Criteria: []apitypes.CompletionPendingCriterion{}}
	revisions := map[string]int{}
	for _, a := range c.MaintainerAuthorizations {
		revisions[a.ID] = a.Revision
	}
	for _, cr := range c.Criteria {
		if cr.Kind != "maintainer" {
			continue
		}
		out.Criteria = append(out.Criteria, apitypes.CompletionPendingCriterion{
			CriterionID: cr.ID, MilestoneID: cr.MilestoneID, Kind: "maintainer", Text: cr.Text,
			SemanticMaintainer: *cr.Maintainer, AuthorizationRevision: revisions[cr.ID], Status: "unverified",
		})
	}
	sort.Slice(out.Criteria, func(i, j int) bool { return out.Criteria[i].CriterionID < out.Criteria[j].CriterionID })
	return out, nil
}

// semanticUnmetMilestones keeps milestones without a criterion as worker
// obligations. A pending-only milestone may be delivered without declaring it
// completed; a mixed milestone still needs the worker's completion declaration.
func semanticUnmetMilestones(run store.Run, c completionContract) ([]string, bool) {
	if _, err := CompletionPending(run); err != nil {
		return frozenMilestoneIDs(run), false
	}
	ms, err := DecodeMilestones(run.MilestonesFrozen)
	if err != nil {
		return frozenMilestoneIDs(run), false
	}
	completed, err := DecodeMilestoneIDs(run.MilestonesCompleted)
	if err != nil {
		return frozenMilestoneIDs(run), false
	}
	done, deferred := map[string]bool{}, map[string]bool{}
	for _, id := range completed {
		done[id] = true
	}
	if c.Scope != nil {
		for _, d := range c.Scope.Out {
			deferred[d.MilestoneID] = true
		}
	}
	have, worker := map[string]bool{}, map[string]bool{}
	for _, cr := range c.Criteria {
		have[cr.MilestoneID] = true
		if cr.Kind != "maintainer" {
			worker[cr.MilestoneID] = true
		}
	}
	unmet := []string{}
	for _, m := range ms {
		if (!have[m.ID] || worker[m.ID]) && !done[m.ID] && !deferred[m.ID] {
			unmet = append(unmet, m.ID)
		}
	}
	sort.Strings(unmet)
	return unmet, true
}

// semanticCompletionRequiresLock recognizes frozen semantic authority and structural
// contamination before an outer mutation can enter the unlocked legacy fallback.
// Locked inner helpers deliberately do not call this guard: their caller owns the tx.
func semanticCompletionRequiresLock(run store.Run) bool {
	var c completionContract
	if json.Unmarshal(run.CompletionContract, &c) != nil {
		return false
	}
	return c.Profile == "semantic" || hasSemanticCompletionFields(c)
}

func hasSemanticCompletionFields(c completionContract) bool {
	if c.SemanticApproval != nil || c.SemanticPresentation != nil || len(c.MaintainerAuthorizations) != 0 || c.CandidateIdentityVersion != "" || len(c.Sources) != 0 || c.PlanHash != "" || c.SourceOutcome != "" || c.Coverage != nil {
		return true
	}
	for _, cr := range c.Criteria {
		if cr.Kind != "" || cr.Maintainer != nil || cr.DoneWhen != "" || len(cr.Citations) != 0 || len(cr.EvidenceRequirements) != 0 {
			return true
		}
	}
	return false
}
