package workersvc

import (
	"sort"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/skilltmpl"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Skill-drop reason codes carried on the claim (ClaimSkillDrop.Reason). Stable
// wire values: the worker maps them to run-message log lines (it owns the gapless
// per-run seq; the server never writes run_messages).
const (
	// DropShadowed: a skill was displaced by a higher-precedence skill of the same
	// name (precedence user > global > builtin). The name is still delivered,
	// backed by the winner's body.
	DropShadowed = "shadowed"
	// DropOverLimit: a skill was dropped because the per-run union exceeded
	// SKILLS_MAX_PER_RUN; lowest-precedence skills are dropped first. The name is
	// not delivered at all.
	DropOverLimit = "over_limit"
	// DropTooLarge: a product skill was dropped because its body exceeds SKILL_MAX_BYTES (the cap
	// was lowered after the set was approved). The worker's own cap check uses the same code.
	DropTooLarge = "too_large"
)

// scopeRank orders skill scopes for name-collision precedence and cap eviction:
// user (3) > global (2) > builtin (1). An unknown scope ranks 0 (should never
// occur; the DB CHECK constrains scope). PRODUCT (PRD #1909) deliberately also ranks 0: a product
// skill never enters this precedence union (ListRunSkillAllocations excludes the scope, and a
// product job's skills are assembled apart, assembleProductSkills), so it has no rank to win or
// lose by. TestScopeRankCoversEveryScope pins the whole table to skilltmpl.Scopes.
func scopeRank(scope string) int {
	switch scope {
	case skilltmpl.ScopeUser:
		return 3
	case skilltmpl.ScopeGlobal:
		return 2
	case skilltmpl.ScopeBuiltin:
		return 1
	default:
		return 0
	}
}

// assembledSkills is the output of assembleRunSkills: the per-run union, the
// per-template surviving skill names, and every drop (shadowed + over-limit).
type assembledSkills struct {
	union       []ClaimSkill
	perTemplate map[string][]string
	dropped     []ClaimSkillDrop
}

// skillCandidate is a distinct skill (deduped by id) considered for the union.
type skillCandidate struct {
	id          uuid.UUID
	name        string
	description string
	body        string
	rank        int
}

// assembleRunSkills builds the per-run skill union from the flat allocation rows
// (shared ∪ the owner's overlay, one row per (template, skill)). It:
//
//  1. dedupes the rows to distinct skills (by id) and records each template's
//     allocated skill names;
//  2. resolves name collisions by precedence (user > global > builtin) — the
//     loser of a name is a DropShadowed drop, the winner carries the delivered
//     body;
//  3. caps the union at maxPerRun (<=0 means no cap), dropping lowest-precedence
//     first as DropOverLimit;
//  4. restricts each template's skill list to names that survived the union.
//
// Output is fully sorted (names ascending; drops by name then reason) so the
// claim payload is byte-stable — the cross-side wire contract depends on it.
func assembleRunSkills(rows []store.ListRunSkillAllocationsRow, maxPerRun int) assembledSkills {
	// (1) Distinct skills by id + per-template allocated id sets (dedup within a
	// template: a skill allocated both shared and as the owner's overlay appears
	// twice).
	byID := map[uuid.UUID]skillCandidate{}
	templateIDs := map[string][]uuid.UUID{}
	seenInTemplate := map[string]map[uuid.UUID]bool{}
	for _, r := range rows {
		if _, ok := byID[r.SkillID]; !ok {
			byID[r.SkillID] = skillCandidate{
				id:          r.SkillID,
				name:        r.SkillName,
				description: r.Description,
				body:        r.Body,
				rank:        scopeRank(r.Scope),
			}
		}
		if seenInTemplate[r.TemplateName] == nil {
			seenInTemplate[r.TemplateName] = map[uuid.UUID]bool{}
		}
		if !seenInTemplate[r.TemplateName][r.SkillID] {
			seenInTemplate[r.TemplateName][r.SkillID] = true
			templateIDs[r.TemplateName] = append(templateIDs[r.TemplateName], r.SkillID)
		}
	}

	// (2) Group by name; the highest-rank candidate wins, others are shadowed.
	byName := map[string][]skillCandidate{}
	for _, c := range byID {
		byName[c.name] = append(byName[c.name], c)
	}
	winners := map[string]skillCandidate{} // name -> winner
	var dropped []ClaimSkillDrop
	for name, group := range byName {
		sort.Slice(group, func(i, j int) bool {
			if group[i].rank != group[j].rank {
				return group[i].rank > group[j].rank // higher rank first
			}
			return group[i].id.String() < group[j].id.String() // deterministic tiebreak
		})
		winners[name] = group[0]
		for _, loser := range group[1:] {
			dropped = append(dropped, ClaimSkillDrop{Name: loser.name, Reason: DropShadowed})
		}
	}

	// (3) Cap the union at maxPerRun, evicting lowest-precedence (then name-desc)
	// first so the survivors are the highest-value skills.
	survivors := make([]skillCandidate, 0, len(winners))
	for _, w := range winners {
		survivors = append(survivors, w)
	}
	sort.Slice(survivors, func(i, j int) bool {
		if survivors[i].rank != survivors[j].rank {
			return survivors[i].rank > survivors[j].rank // keep higher rank
		}
		return survivors[i].name < survivors[j].name
	})
	if maxPerRun > 0 && len(survivors) > maxPerRun {
		for _, over := range survivors[maxPerRun:] {
			dropped = append(dropped, ClaimSkillDrop{Name: over.name, Reason: DropOverLimit})
		}
		survivors = survivors[:maxPerRun]
	}

	survivingNames := map[string]bool{}
	for _, s := range survivors {
		survivingNames[s.name] = true
	}

	// (4) Union, sorted by name for a stable payload.
	sort.Slice(survivors, func(i, j int) bool { return survivors[i].name < survivors[j].name })
	union := make([]ClaimSkill, 0, len(survivors))
	for _, s := range survivors {
		union = append(union, ClaimSkill{Name: s.name, Description: s.description, Body: s.body})
	}

	// Per-template names, mapped id->name, filtered to survivors, unique, sorted.
	perTemplate := map[string][]string{}
	for tmpl, ids := range templateIDs {
		nameSet := map[string]bool{}
		for _, id := range ids {
			nm := byID[id].name
			if survivingNames[nm] {
				nameSet[nm] = true
			}
		}
		names := make([]string, 0, len(nameSet))
		for nm := range nameSet {
			names = append(names, nm)
		}
		sort.Strings(names)
		perTemplate[tmpl] = names
	}

	// Stable drop order: name asc, then reason asc.
	sort.Slice(dropped, func(i, j int) bool {
		if dropped[i].Name != dropped[j].Name {
			return dropped[i].Name < dropped[j].Name
		}
		return dropped[i].Reason < dropped[j].Reason
	})
	if dropped == nil {
		dropped = []ClaimSkillDrop{}
	}

	return assembledSkills{union: union, perTemplate: perTemplate, dropped: dropped}
}

// assembleProductSkills builds the skills of a product JOB (PRD #1909 D9) from the product's
// approved skill rows. A product job receives exactly these and nothing else: no user, global or
// builtin skill and no repo skill, so what shapes the product's output is what the product ships.
// The same caps as a run's union apply: a body over maxBytes (<=0 means no cap) is dropped as
// DropTooLarge, then the set is cut to maxPerRun (<=0 means no cap) in name order, the rest
// dropped as DropOverLimit. Output is sorted by name and the drops by name then reason, so the
// claim payload is byte-stable. Both results are non-nil.
func assembleProductSkills(rows []store.ListProductSkillsForRunRow, maxBytes, maxPerRun int) ([]ClaimSkill, []ClaimSkillDrop) {
	sorted := append([]store.ListProductSkillsForRunRow(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	skills := make([]ClaimSkill, 0, len(sorted))
	dropped := []ClaimSkillDrop{}
	for _, r := range sorted {
		if maxBytes > 0 && len(r.Body) > maxBytes {
			dropped = append(dropped, ClaimSkillDrop{Name: r.Name, Reason: DropTooLarge})
			continue
		}
		if maxPerRun > 0 && len(skills) >= maxPerRun {
			dropped = append(dropped, ClaimSkillDrop{Name: r.Name, Reason: DropOverLimit})
			continue
		}
		skills = append(skills, ClaimSkill{Name: r.Name, Description: r.Description, Body: r.Body})
	}
	return skills, dropped
}
