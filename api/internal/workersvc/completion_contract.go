package workersvc

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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
//
// PRD #1227 M1 extends it with two OPTIONAL top-level blocks — `scope` and `accepted` — that an
// owner decision writes at a bumped revision. Both are `omitempty`: a revision-1 contract (the
// #1226 freeze, which sets neither) marshals BYTE-IDENTICALLY to before, so the freeze goldens and
// the permit-path recompute for an untouched run are unchanged. `criteria` stays full and immutable
// across revisions — an owner decision never rewrites a criterion, it only defers a milestone
// (scope.out) or accepts a criterion id (accepted).
type completionContract struct {
	Profile  string                `json:"profile"`
	Revision int                   `json:"revision"`
	Criteria []completionCriterion `json:"criteria"`
	// Scope is the owner-reduced scope on a #1227 `partial` revision: the milestone ids still
	// in scope (`in`) and the owner-deferred ones (`out`). nil on a revision-1 contract.
	Scope *scopeBlock `json:"scope,omitempty"`
	// Accepted is the list of owner-accepted unmet criteria on a #1227 `accept` revision. nil on
	// a revision-1 contract.
	Accepted []acceptedEntry `json:"accepted,omitempty"`
}

// scopeBlock is the in/out milestone split a `partial` decision records (PRD #1227 D2).
type scopeBlock struct {
	In  []string        `json:"in"`
	Out []deferredEntry `json:"out"`
}

// deferredEntry is one owner-deferred (out-of-scope) milestone: its id, the owner's reason, and the
// contract revision the deferral was recorded at. A milestone deferred in an EARLIER revision keeps
// its ORIGINAL reason/revision when a later `partial` decision revises the scope again.
type deferredEntry struct {
	MilestoneID string `json:"milestone_id"`
	Reason      string `json:"reason"`
	Revision    int    `json:"revision"`
}

// acceptedEntry is one owner-accepted unmet criterion (PRD #1227 D3): the exact criterion id, its
// milestone id and text copied from the matching `criteria[]` entry, the owner's reason, and the
// revision the acceptance was recorded at.
type acceptedEntry struct {
	ID          string `json:"id"`
	MilestoneID string `json:"milestone_id"`
	Text        string `json:"text"`
	Reason      string `json:"reason"`
	Revision    int    `json:"revision"`
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

// parseCompletionContract decodes a run's frozen completion_contract jsonb. It errors on a
// nil/empty column (a never-frozen or split-state run) and on corrupt bytes — the caller decides
// whether that is fail-closed (deny/conflict) or fail-safe (an inert DTO). It is the single decode
// point #1227's revised-contract builder, the owner-decision idempotency check and the owner DTO
// projection share, so the extended shape is read one way everywhere.
func parseCompletionContract(b []byte) (completionContract, error) {
	if len(b) == 0 {
		return completionContract{}, fmt.Errorf("completion contract is empty")
	}
	var c completionContract
	if err := json.Unmarshal(b, &c); err != nil {
		return completionContract{}, fmt.Errorf("decode completion contract: %w", err)
	}
	return c, nil
}

// buildRevisedContract produces the jsonb for contract revision newRev from the PRIOR contract and
// an owner decision (PRD #1227 M1). `criteria` is carried through UNCHANGED (an owner decision never
// rewrites a criterion). The caller validates the decision against the prior contract BEFORE calling
// this; here we only construct the revised shape.
//
//   - partial: scope.in becomes the sorted keep set; scope.out becomes (frozen − keep) with each
//     newly-deferred milestone recorded as {reason, revision:newRev} and each ALREADY-deferred
//     milestone keeping its ORIGINAL entry (reason/revision) from the prior scope.out. accepted is
//     carried through unchanged.
//   - accept: each new criterion id is appended to accepted as {id, milestone_id, text, reason,
//     revision:newRev}, where milestone_id/text are copied from the matching prior criteria[] entry.
//     scope is carried through unchanged.
//
// revision is set to newRev on both. frozenMs is the run's frozen milestone list (the id universe a
// partial splits).
func buildRevisedContract(prior []byte, dec CompletionDecisionInput, frozenMs []Milestone, newRev int) ([]byte, error) {
	c, err := parseCompletionContract(prior)
	if err != nil {
		return nil, err
	}
	c.Revision = newRev

	switch dec.Decision {
	case "partial":
		keep := make(map[string]bool, len(dec.Keep))
		for _, k := range dec.Keep {
			keep[k] = true
		}
		priorOut := map[string]deferredEntry{}
		if c.Scope != nil {
			for _, d := range c.Scope.Out {
				priorOut[d.MilestoneID] = d
			}
		}
		in := make([]string, 0, len(dec.Keep))
		out := make([]deferredEntry, 0, len(frozenMs))
		for _, m := range frozenMs {
			if keep[m.ID] {
				in = append(in, m.ID)
				continue
			}
			if prev, ok := priorOut[m.ID]; ok {
				out = append(out, prev) // an earlier revision's deferral keeps its reason/revision.
				continue
			}
			out = append(out, deferredEntry{MilestoneID: m.ID, Reason: dec.Reason, Revision: newRev})
		}
		sort.Strings(in)
		sort.Slice(out, func(i, j int) bool { return out[i].MilestoneID < out[j].MilestoneID })
		c.Scope = &scopeBlock{In: in, Out: out}
	case "accept":
		already := make(map[string]bool, len(c.Accepted))
		for _, a := range c.Accepted {
			already[a.ID] = true
		}
		critByID := make(map[string]completionCriterion, len(c.Criteria))
		for _, cr := range c.Criteria {
			critByID[cr.ID] = cr
		}
		newIDs := append([]string(nil), dec.Criteria...)
		sort.Strings(newIDs)
		for _, id := range newIDs {
			if already[id] {
				continue
			}
			cr := critByID[id]
			c.Accepted = append(c.Accepted, acceptedEntry{
				ID:          id,
				MilestoneID: cr.MilestoneID,
				Text:        cr.Text,
				Reason:      dec.Reason,
				Revision:    newRev,
			})
			already[id] = true
		}
	default:
		return nil, fmt.Errorf("buildRevisedContract: unsupported decision %q", dec.Decision)
	}

	out, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("marshal revised completion contract: %w", err)
	}
	return out, nil
}

// contractHasDeferrals reports whether a contract records any owner-deferred (out-of-scope)
// milestone — true iff scope.out is non-empty. A nil/empty/corrupt contract is fail-safe false.
// (The #1227 M2 agent settlement reads this to decide a partial-delivery PR body; kept beside the
// contract shape so the single decode point is reused.)
func contractHasDeferrals(b []byte) bool {
	c, err := parseCompletionContract(b)
	if err != nil {
		return false
	}
	return c.Scope != nil && len(c.Scope.Out) > 0
}

// CompletionScopeView projects a run's frozen completion_contract into the owner-facing DTO slices
// (PRD #1227 M1): the deferred (out-of-scope) milestones and the owner-accepted criteria. Both are
// ALWAYS non-nil — `[]` when the contract encodes none, is unfrozen, or is corrupt — so the wire
// carries a stable array (the completion_unmet convention), never null. It is the SINGLE source of
// the contract-derived DTO shape: the handler's runToDTO calls it rather than re-decoding the jsonb,
// so the wire projection and the contract shape cannot drift.
func CompletionScopeView(contract []byte) (deferred []apitypes.CompletionDeferredDTO, accepted []apitypes.CompletionAcceptedDTO) {
	deferred = []apitypes.CompletionDeferredDTO{}
	accepted = []apitypes.CompletionAcceptedDTO{}
	c, err := parseCompletionContract(contract)
	if err != nil {
		return deferred, accepted
	}
	if c.Scope != nil {
		for _, d := range c.Scope.Out {
			deferred = append(deferred, apitypes.CompletionDeferredDTO{
				MilestoneID: d.MilestoneID, Reason: d.Reason, Revision: d.Revision,
			})
		}
	}
	for _, a := range c.Accepted {
		accepted = append(accepted, apitypes.CompletionAcceptedDTO{
			ID: a.ID, MilestoneID: a.MilestoneID, Text: a.Text, Reason: a.Reason, Revision: a.Revision,
		})
	}
	return deferred, accepted
}
