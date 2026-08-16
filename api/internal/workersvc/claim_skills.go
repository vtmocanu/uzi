package workersvc

import (
	"sort"

	"github.com/google/uuid"

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
)

// scopeRank orders skill scopes for name-collision precedence and cap eviction:
// user (3) > global (2) > builtin (1). An unknown scope ranks 0 (should never
// occur; the DB CHECK constrains scope).
func scopeRank(scope string) int {
	switch scope {
	case "user":
		return 3
	case "global":
		return 2
	case "builtin":
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
