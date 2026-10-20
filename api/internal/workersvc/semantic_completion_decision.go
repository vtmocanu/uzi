package workersvc

import "github.com/vtmocanu/uzi/api/internal/apitypes"

// Later decisions may renew exact existing classifications only. Changing their
// conditions or kind requires the M3 human plan revision, not this decision.
func validateMaintainerDecision(c completionContract, dec CompletionDecisionInput) error {
	if validateSemanticCompletion(c) != nil {
		return ErrCompletionDecisionInvalid
	}
	return validateMaintainerSelection(c, dec)
}

// validateMaintainerSelection checks the exact selection after contract validation.
func validateMaintainerSelection(c completionContract, dec CompletionDecisionInput) error {
	if len(dec.Maintainers) == 0 || len(dec.Maintainers) > 256 ||
		len(dec.Keep) != 0 || len(dec.Criteria) != 0 {
		return ErrCompletionDecisionInvalid
	}
	want := map[string]apitypes.SemanticMaintainerApproval{}
	for _, m := range semanticMaintainers(c) {
		want[m.ID] = m
	}
	seen := map[string]bool{}
	for _, m := range dec.Maintainers {
		frozen, ok := want[m.ID]
		if !ok || seen[m.ID] || !sameSemanticJSON(frozen, m) {
			return ErrCompletionDecisionInvalid
		}
		seen[m.ID] = true
	}
	return nil
}

func authorizeMaintainers(c *completionContract, dec CompletionDecisionInput, revision int) {
	selected := map[string]bool{}
	for _, m := range dec.Maintainers {
		selected[m.ID] = true
	}
	for i := range c.MaintainerAuthorizations {
		if selected[c.MaintainerAuthorizations[i].ID] {
			c.MaintainerAuthorizations[i].Revision = revision
			c.MaintainerAuthorizations[i].DecisionReason = dec.Reason
		}
	}
}

func maintainerDecisionEncoded(c completionContract, dec CompletionDecisionInput, revision int) bool {
	if c.Profile != "semantic" || validateMaintainerSelection(c, dec) != nil {
		return false
	}
	want := map[string]bool{}
	for _, m := range dec.Maintainers {
		want[m.ID] = true
	}
	got := map[string]bool{}
	for _, a := range c.MaintainerAuthorizations {
		if a.Revision == revision {
			if a.DecisionReason != dec.Reason {
				return false
			}
			got[a.ID] = true
		}
	}
	return sameBoolSet(want, got)
}
