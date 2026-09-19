// Package milestonelanes derives a run's per-milestone LIVE LANES — the set of
// subagents currently working each in-progress milestone (PRD #1353).
//
// This file provides the dispatch-description milestone-tag binding (D1): a
// subagent's frames correlate to the lead's `Agent` dispatch by
// agent_instance = parent_tool_use_id, and the dispatch's `description` carries a
// leading `[<id>]` milestone tag that the derivation reads. The lead writes the
// milestone id as a leading convention token into the description it already fills
// with the task label (e.g. "[m2] Review the limiter"), so the binding rides an
// existing, uzi-authored field with no new frame field, no new column, and no SDK
// schema risk.
//
// The `Derive` function that back-joins live frames to their dispatch frame and reads
// this tag lives in a sibling file (derive.go); this file (tag.go) provides only the
// dispatch-description tag parser and the membership validator that Derive calls.
//
// The package is SERVER-ONLY: it is not linked by the CLI leaf api/cmd/uzi (which
// consumes the derived milestones_live DTO field, never this rule), so it is free to
// import server packages — derive.go pulls in apitypes and runactivity (and, through
// the stdlib, encoding/json, sort and time). It is NOT stdlib-only.
package milestonelanes

import "strings"

// ParseMilestoneTag extracts a leading "[<id>] <rest>" milestone tag from a subagent
// dispatch description (PRD #1353 D1). It returns the raw bracketed id, the remaining
// description text (the task label), and ok=true when a well-formed non-empty [id] leads
// the string. Membership is NOT validated here — a non-member id is dropped by the caller
// (D7). When there is no leading [id] tag, ok is false, id is "", and rest is the input.
//
// NOTE: the returned `rest` is UNVALIDATED, unsanitized, model-authored text — only `id` is
// membership-validated (by MilestoneTagBinding). Its terminal-safety is applied downstream:
// Derive sanitizes the lane label via runactivity.Sanitize before it lands on the wire.
func ParseMilestoneTag(description string) (id, rest string, ok bool) {
	trimmed := strings.TrimLeft(description, " \t")
	if !strings.HasPrefix(trimmed, "[") {
		// No leading tag: return the ORIGINAL (untrimmed) description as rest.
		return "", description, false
	}
	end := strings.IndexByte(trimmed, ']')
	if end < 0 {
		return "", description, false
	}
	id = strings.TrimSpace(trimmed[1:end])
	if id == "" {
		return "", description, false
	}
	rest = strings.TrimSpace(trimmed[end+1:])
	return id, rest, true
}

// MilestoneTagBinding parses a dispatch description and validates its milestone tag against
// the run's in-progress milestone set (PRD #1353 D7: a non-member tag is dropped, never
// bound). It returns the bound milestone id, the task label (the description minus the tag),
// and bound=true only when the description carries a well-formed [id] tag whose id is a
// member of inProgress. Otherwise it returns ("", "", false). A nil inProgress map is
// treated as "no members", so every tag is dropped.
//
// NOTE: the returned `label` is UNVALIDATED, unsanitized, model-authored text — only `id` is
// membership-validated. Its terminal-safety is applied downstream: Derive sanitizes the lane
// label via runactivity.Sanitize before it lands on the wire.
func MilestoneTagBinding(description string, inProgress map[string]bool) (id, label string, bound bool) {
	parsedID, rest, ok := ParseMilestoneTag(description)
	if !ok {
		return "", "", false
	}
	if !inProgress[parsedID] {
		return "", "", false
	}
	return parsedID, rest, true
}
