package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// check_activity.go renders the cross-check activity evidence (PRD #3166): the three
// `status` events a checker emits (`check_activity_start`, `check_activity`,
// `check_activity_summary`). The wording mirrors the agent's formatActivityLine /
// formatActivitySummary (agent/src/cross-check-activity.ts) so the web page, `run logs` and
// `run get` read the same. Every payload is untrusted worker output: fields are decoded into
// typed values, op / outcome / category are matched against closed sets (a value outside them
// falls back to the generic payload row), and the finished line still goes through
// capCell(cellText(...)) at the call site.

const (
	activityEventStart   = "check_activity_start"
	activityEvent        = "check_activity"
	activityEventSummary = "check_activity_summary"
)

var (
	activityOutcomes   = map[string]bool{"ok": true, "denied": true, "oversize": true, "limit": true, "error": true}
	activityOps        = map[string]bool{"read": true, "search": true}
	activityCategories = map[string]string{
		"outside_checkout": "outside checkout",
		"tool_output":      "tool output",
		"policy":           "policy",
	}
)

// activityPayload decodes a status payload into its raw fields and event name. ok is false
// when the payload is not a JSON object with a string `event`.
func activityPayload(raw json.RawMessage) (fields map[string]json.RawMessage, event string, ok bool) {
	if json.Unmarshal(raw, &fields) != nil {
		return nil, "", false
	}
	if json.Unmarshal(fields["event"], &event) != nil {
		return nil, "", false
	}
	return fields, event, true
}

// nonNegInt decodes a JSON number that is a whole, non-negative, exactly representable
// integer. Anything else (null, string, fraction, negative, huge) reports false.
func nonNegInt(raw json.RawMessage) (int64, bool) {
	var f float64
	// json.Unmarshal of a JSON null is a no-op that reports success, which would read as 0.
	if string(raw) == "null" || json.Unmarshal(raw, &f) != nil || f < 0 || f != math.Trunc(f) || f > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// activityMessageLine returns the human line for a status message that carries one of the
// activity events. handled is false for every other message and for an activity payload that
// does not validate, so the caller keeps its generic compactPayload row.
func activityMessageLine(kind string, raw json.RawMessage) (line string, handled bool) {
	if kind != "status" {
		return "", false
	}
	fields, event, ok := activityPayload(raw)
	if !ok {
		return "", false
	}
	switch event {
	case activityEventStart:
		return "activity capture started", true
	case activityEvent:
		return activityLine(fields)
	case activityEventSummary:
		text, ok := activitySummaryText(fields)
		if !ok {
			return "", false
		}
		return "activity summary: " + text, true
	}
	return "", false
}

// activityLine renders one `check_activity` payload, e.g. `read api/x.go L1-200 · ok`,
// `search api/internal/forge · 3 matches · ok`, `read denied (outside checkout)`.
func activityLine(fields map[string]json.RawMessage) (string, bool) {
	var op, outcome string
	if json.Unmarshal(fields["op"], &op) != nil || !activityOps[op] {
		return "", false
	}
	if json.Unmarshal(fields["outcome"], &outcome) != nil || !activityOutcomes[outcome] {
		return "", false
	}
	var rawCategory, path string
	_ = json.Unmarshal(fields["category"], &rawCategory)
	category := activityCategories[rawCategory] // "" for absent or unknown
	_ = json.Unmarshal(fields["path"], &path)
	if outcome == "denied" && path == "" {
		if category != "" {
			return fmt.Sprintf("%s denied (%s)", op, category), true
		}
		return op + " denied", true
	}
	subject := op
	switch {
	case path != "":
		subject += " " + path
	case category != "":
		subject += " (" + category + ")"
	}
	var lines []json.RawMessage
	if json.Unmarshal(fields["lines"], &lines) == nil && len(lines) == 2 {
		start, okStart := nonNegInt(lines[0])
		end, okEnd := nonNegInt(lines[1])
		if okStart && okEnd {
			subject += fmt.Sprintf(" L%d-%d", start, end)
		}
	}
	parts := []string{subject}
	if matches, ok := nonNegInt(fields["matches"]); ok {
		var truncated bool
		_ = json.Unmarshal(fields["matches_truncated"], &truncated)
		word, plus := "matches", ""
		if truncated {
			plus = "+"
		} else if matches == 1 {
			word = "match"
		}
		parts = append(parts, fmt.Sprintf("%d%s %s", matches, plus, word))
	}
	parts = append(parts, outcome)
	return strings.Join(parts, " · "), true
}

// outcomeTotal sums the five closed outcome counters of a `reads` / `searches` object.
func outcomeTotal(raw json.RawMessage) int64 {
	var counts map[string]json.RawMessage
	if json.Unmarshal(raw, &counts) != nil {
		return 0
	}
	var sum int64
	for key := range activityOutcomes {
		if n, ok := nonNegInt(counts[key]); ok {
			sum += n
		}
	}
	return sum
}

// activitySummaryText renders `4 reads, 0 searches, 0 dropped, complete` from a summary
// payload. ok is false when the payload carries no `reads`/`searches` object at all.
func activitySummaryText(fields map[string]json.RawMessage) (string, bool) {
	if _, ok := fields["reads"]; !ok {
		return "", false
	}
	if _, ok := fields["searches"]; !ok {
		return "", false
	}
	reads, searches := outcomeTotal(fields["reads"]), outcomeTotal(fields["searches"])
	dropped, _ := nonNegInt(fields["dropped"])
	var complete bool
	_ = json.Unmarshal(fields["complete"], &complete)
	state := "incomplete"
	if complete {
		state = "complete"
	}
	return fmt.Sprintf("%d %s, %d %s, %d dropped, %s",
		reads, plural(reads, "read", "reads"), searches, plural(searches, "search", "searches"), dropped, state), true
}

func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// checkActivityLine derives the single `ACTIVITY` line `uzi run get` prints for a cross-check
// run from its messages. gen is the run's CURRENT claim generation (RunDTO.ClaimGeneration;
// nil from an older API), terminal whether the run finished, claimed whether it ever started
// (FirstStartedAt set). A marker or summary counts as current only when its generation equals
// gen: an older attempt's (lower generation) summary is shown, labelled as that attempt's, and a
// higher generation (a re-claim between reads) is ignored. Returns ""
// when there is nothing to say.
func checkActivityLine(messages []apitypes.MessageDTO, gen *int64, terminal, claimed bool) string {
	type summary struct {
		gen  *int64
		text string
	}
	markers := map[int64]bool{}
	summaries := []summary{} // in message order
	for _, m := range messages {
		fields, event, ok := activityPayload(m.Payload)
		if m.Kind != "status" || !ok {
			continue
		}
		g, hasGen := nonNegInt(fields["generation"])
		switch event {
		case activityEventStart:
			if hasGen {
				markers[g] = true
			}
		case activityEventSummary:
			text, ok := activitySummaryText(fields)
			if !ok {
				continue
			}
			s := summary{text: text}
			if hasGen {
				s.gen = &g
			}
			summaries = append(summaries, s)
		}
	}
	if gen == nil {
		// Older API: no provenance, so even the newest summary cannot be called current.
		if len(summaries) == 0 {
			return ""
		}
		return "historical; current capture unknown: " + summaries[len(summaries)-1].text
	}
	for i := len(summaries) - 1; i >= 0; i-- {
		if s := summaries[i]; s.gen != nil && *s.gen == *gen {
			return s.text
		}
	}
	var current string
	switch {
	case markers[*gen] && terminal:
		current = "activity capture incomplete"
	case markers[*gen]:
		current = "activity capture in progress"
	case terminal && claimed:
		current = "activity capture unavailable"
	}
	earlier := ""
	for i := len(summaries) - 1; i >= 0; i-- {
		if s := summaries[i]; s.gen != nil && *s.gen < *gen {
			earlier = fmt.Sprintf("earlier attempt (generation %d): %s", *s.gen, s.text)
			break
		}
	}
	switch {
	case current != "" && earlier != "":
		return current + "; " + earlier
	case current != "":
		return current
	}
	return earlier
}
