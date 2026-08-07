package workersvc

import (
	"encoding/json"
	"regexp"
	"strings"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
)

// Milestone is one entry of a run's milestone list (PRD #122): a small {id, title}
// object a worker proposes at the plan gate and, once approved, the FROZEN list a
// resume replays and the run view shows.
//
// Defined in apitypes (the stdlib-only leaf the uzi CLI links, PRD #64 M1) and
// re-exported here as a type ALIAS, exactly like RepoAgent/AgentSelection: the
// alias keeps every workersvc.Milestone reference and the validate/decode free
// functions below compiling unchanged while apitypes owns only the wire shape.
type Milestone = apitypes.Milestone

const (
	// maxMilestonesPerRun caps a reported milestone list. A whole list over the cap
	// is REJECTED (Decision 12), not truncated.
	maxMilestonesPerRun = 50
	// maxMilestoneIDRunes bounds one milestone id. The id-shape regex already caps at
	// 64 by construction (a leading char plus up to 63 more); the explicit check is
	// kept as a readable statement of the bound.
	maxMilestoneIDRunes = 64
	// maxMilestoneTitleRunes bounds one milestone title, measured in runes (code
	// points) — the unit a human reads, and the one the agent side mirrors.
	maxMilestoneTitleRunes = 200
)

// milestoneIDRe is the milestone id shape: a leading alphanumeric then up to 63
// more of [A-Za-z0-9._-]. Deliberately strict — an id is a stable machine key, not
// display text, so it admits no whitespace, no control or format characters, and no
// path/traversal punctuation. The character class is ASCII, so it also rejects any
// NUL or bidi rune outright.
var milestoneIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// validateMilestones enforces Decision 12 on a worker-reported milestone list. It
// is the single server-side chokepoint: a milestone list arrives from a WORKER,
// which is a program holding a join token, not an authority, so every bound the
// worker claims to have applied is re-checked here before the list reaches the DB,
// the claim payload, the run DTO, and (later) the run view.
//
// It returns the sanitized list and ok=true when the WHOLE list is acceptable, or
// (nil, false) when ANY entry is invalid — the list is rejected as a unit, never
// partially clamped. Rejection is caller-handled as a DROP (the field is not
// persisted) rather than an error, so a bad list never fails an otherwise-fine
// state report (additive-optional).
//
// Rejected when any of: the list exceeds maxMilestonesPerRun; an id is empty, over
// maxMilestoneIDRunes, or fails milestoneIDRe; two ids collide; a title is
// empty/whitespace-only, over maxMilestoneTitleRunes, or carries a NUL or an unsafe
// control/bidi character (hasUnsafeChar, shared with the repo-agent validator).
//
// On success the surviving titles are trimmed and defensively passed through
// stripNUL — belt-and-suspenders, since a NUL-bearing title was already rejected
// above. A nil/empty input is a valid empty list.
func validateMilestones(ms []Milestone) ([]Milestone, bool) {
	if len(ms) > maxMilestonesPerRun {
		return nil, false
	}
	seen := make(map[string]bool, len(ms))
	out := make([]Milestone, 0, len(ms))
	for _, m := range ms {
		if m.ID == "" || len([]rune(m.ID)) > maxMilestoneIDRunes || !milestoneIDRe.MatchString(m.ID) {
			return nil, false
		}
		if seen[m.ID] {
			return nil, false
		}
		seen[m.ID] = true

		if strings.ContainsRune(m.Title, '\x00') || hasUnsafeChar(m.Title) {
			return nil, false
		}
		title, _ := stripNUL(m.Title)
		title = strings.TrimSpace(title)
		if title == "" || len([]rune(title)) > maxMilestoneTitleRunes {
			return nil, false
		}
		out = append(out, Milestone{ID: m.ID, Title: title})
	}
	return out, true
}

// milestonesParam turns a worker-reported milestone list into the jsonb to persist,
// or nil to persist SQL NULL. Two gates DROP the list to NULL rather than failing
// the report (both additive-optional, mirroring validateRepoAgents/prd_done_path):
//   - kind gate (Decision 13): only an issue run may carry milestones; any other
//     kind's list is dropped.
//   - Decision 12 validation: a list that fails validateMilestones is dropped.
//
// An absent field (nil pointer) is also NULL. A present list encodes through
// encodeJSONArray, so a reported-but-empty `[]` stays a distinct, meaningful value
// (as with repo_agents' NULL-vs-`[]`), never JSON null inside the jsonb.
func milestonesParam(kind string, ms *[]Milestone) []byte {
	if ms == nil || kind != RunKindIssue {
		return nil
	}
	sanitized, ok := validateMilestones(*ms)
	if !ok {
		return nil
	}
	encoded, err := encodeJSONArray(sanitized)
	if err != nil {
		return nil
	}
	return encoded
}

// DecodeMilestones reads a runs.milestones_frozen (or _candidate) jsonb column into
// the wire shape. A NULL/empty column yields a NIL slice — "no list", which the
// claim omits (omitempty) and the run DTO renders as JSON null. Malformed jsonb is
// an error the caller degrades to nil-and-log, matching DecodeRepoAgents: the column
// is data a prior write left behind, not an invariant of this read.
func DecodeMilestones(raw []byte) ([]apitypes.Milestone, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []apitypes.Milestone
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}
