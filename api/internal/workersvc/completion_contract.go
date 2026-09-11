package workersvc

import (
	"encoding/json"
	"fmt"
)

// PRD #1226 M1 (D1): the frozen STRUCTURAL completion contract. Its wire shape is pinned
// here and mirrored verbatim in the migration comment (00211_completion_contract.sql):
//
//	{"profile":"structural","revision":1,
//	 "criteria":[{"id":"m1.c1","milestone_id":"m1","text":"<milestone title>",
//	              "audit":null,"finding_ids":[]}]}
//
// One criterion per entry in the run's frozen milestone list. The structural profile has
// no semantic evidence, so every criterion's `audit` is null and `finding_ids` is the empty
// array — the reserved slots #1230/#1231 later fill WITHOUT replacing the protocol. The
// contract is built in Go at each freeze site from the same milestone source the freeze
// query resolves, and passed to the query as a jsonb param guarded by
// `completion_contract_version IS NOT NULL AND completion_contract IS NULL` so it freezes
// once, idempotently, and only for an interlocked run.
const (
	// contractProfileStructural is the only profile M1 emits. #1230 adds semantic profiles.
	contractProfileStructural = "structural"
	// contractRevisionInitial is the revision a freshly frozen contract carries; a permit
	// fences on it and #1227 bumps it.
	contractRevisionInitial = 1
)

// completionContract is the frozen structural contract's top-level shape.
type completionContract struct {
	Profile  string                `json:"profile"`
	Revision int                   `json:"revision"`
	Criteria []completionCriterion `json:"criteria"`
}

// completionCriterion is one server-minted structural criterion (one per frozen milestone).
// Audit is a nil pointer so it marshals to JSON `null` (a reserved slot, no omitempty);
// FindingIDs is initialized to the empty slice so it marshals to `[]`, never `null`.
type completionCriterion struct {
	ID          string           `json:"id"`
	MilestoneID string           `json:"milestone_id"`
	Text        string           `json:"text"`
	Audit       *json.RawMessage `json:"audit"`
	FindingIDs  []string         `json:"finding_ids"`
}

// buildCompletionContract builds the frozen structural completion contract jsonb (PRD #1226
// M1, D1) from a run's frozen milestone list jsonb (the runs.milestones_frozen /
// milestones_candidate shape: [{"id":..,"title":..}]). It mints one criterion per milestone:
// id = "<milestone_id>.c1", milestone_id = the frozen id, text = the milestone title, audit =
// null, finding_ids = []. A nil/empty list yields a contract with an empty (but non-null)
// criteria array — an interlocked run is NEVER exempted by milestone cardinality (D1), so
// even a 0-milestone run freezes a valid contract. Returns an error only when the milestone
// jsonb cannot be decoded (a corrupt column); the caller treats that best-effort.
func buildCompletionContract(milestonesJSON []byte) ([]byte, error) {
	ms, err := DecodeMilestones(milestonesJSON)
	if err != nil {
		return nil, fmt.Errorf("decode frozen milestones for completion contract: %w", err)
	}
	criteria := make([]completionCriterion, 0, len(ms))
	for _, m := range ms {
		criteria = append(criteria, completionCriterion{
			ID:          m.ID + ".c1",
			MilestoneID: m.ID,
			Text:        m.Title,
			Audit:       nil,
			FindingIDs:  []string{},
		})
	}
	out, err := json.Marshal(completionContract{
		Profile:  contractProfileStructural,
		Revision: contractRevisionInitial,
		Criteria: criteria,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal completion contract: %w", err)
	}
	return out, nil
}
