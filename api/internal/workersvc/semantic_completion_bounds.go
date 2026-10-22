package workersvc

// Recheck M1's decoded field bounds before publishing a stored contract. This is
// structural validation of immutable data, not semantic evidence enforcement.
func validateSemanticCompletionBounds(c completionContract) error {
	fail := ErrSemanticCompletionAuthority
	if len(c.Sources) > 9 {
		return fail
	}
	sources := map[string]int{}
	hashes := map[string]string{}
	for _, s := range c.Sources {
		if !semanticIdentifier(s.ID, 512) || !semanticIdentifier(s.Provenance, 2048) || len(s.Hash) != 64 {
			return fail
		}
		if _, exists := sources[s.ID]; exists {
			return fail
		}
		hashes[s.ID] = s.Hash
		switch s.Kind {
		case "issue":
			sources[s.ID] = 64 * 1024
		case "prd":
			sources[s.ID] = 256 * 1024
		default:
			return fail
		}
	}
	perMilestone := map[string]int{}
	for _, cr := range c.Criteria {
		perMilestone[cr.MilestoneID]++
		if perMilestone[cr.MilestoneID] > 32 || !semanticText(cr.Text, 2048) || !semanticText(cr.DoneWhen, 2048) ||
			len(cr.Citations) < 1 || len(cr.Citations) > 8 || len(cr.EvidenceRequirements) < 1 || len(cr.EvidenceRequirements) > 8 {
			return fail
		}
		switch cr.Kind {
		case "code", "doc", "test":
			if cr.Maintainer != nil {
				return fail
			}
		case "maintainer":
			m := cr.Maintainer
			if m == nil || !semanticText(m.Reason, 2048) || len(m.Conditions) < 1 || len(m.Conditions) > 8 || len(m.VerificationSteps) < 1 || len(m.VerificationSteps) > 8 {
				return fail
			}
			for _, list := range [][]string{m.Conditions, m.VerificationSteps} {
				for _, s := range list {
					if !semanticText(s, 512) {
						return fail
					}
				}
			}
		default:
			return fail
		}
		for _, e := range cr.EvidenceRequirements {
			if !semanticText(e.Description, 512) || (e.Path != "" && !semanticIdentifier(e.Path, 512)) {
				return fail
			}
		}
		for _, cite := range cr.Citations {
			limit, ok := sources[cite.SourceID]
			if !ok || cite.Hash != hashes[cite.SourceID] || cite.Start < 0 || cite.End <= cite.Start || cite.End > limit {
				return fail
			}
		}
	}
	return nil
}
