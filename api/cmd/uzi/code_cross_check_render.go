package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

var codeFindingID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type codeDisposition struct {
	FindingID   string  `json:"finding_id"`
	Disposition string  `json:"disposition"`
	Reason      *string `json:"reason"`
}

// Validate identities before any presentation sanitizing. A malformed batch
// yields no associations or counts; findings still display independently.
func codeDispositionEvidence(s *apitypes.CodeCrossCheck, ids []string) (map[string]codeDisposition, string) {
	unavailable := "Dispositions unavailable"
	if len(ids) > 20 {
		return nil, unavailable
	}
	known := make(map[string]bool)
	for _, id := range ids {
		if !codeFindingID.MatchString(id) || known[id] {
			return nil, unavailable
		}
		known[id] = true
	}
	if len(s.Dispositions) == 0 || string(s.Dispositions) == "null" {
		return nil, unavailable
	}
	var batch []codeDisposition
	if len(s.Dispositions) > 64*1024 || !utf8.Valid(s.Dispositions) || json.Unmarshal(s.Dispositions, &batch) != nil || batch == nil || len(batch) > 20 {
		return nil, unavailable
	}
	result := make(map[string]codeDisposition)
	for _, d := range batch {
		if !known[d.FindingID] {
			return nil, unavailable
		}
		if _, exists := result[d.FindingID]; exists {
			return nil, unavailable
		}
		if d.Disposition != "addressed" && d.Disposition != "declined" && d.Disposition != "not_reported" {
			return nil, unavailable
		}
		if d.Reason == nil || len(*d.Reason) > 1024 || !utf8.ValidString(*d.Reason) {
			return nil, unavailable
		}
		result[d.FindingID] = d
	}
	if s.FinalizedAt == nil {
		return result, "Pending report"
	}
	if len(result) != len(known) {
		return nil, unavailable
	}
	return result, "Finalized"
}

var codeCheckedSHA = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

func codeCrossCheckRows(r apitypes.RunDTO) [][]string {
	s := r.CodeCrossCheckSummary
	if s == nil {
		if r.CodeCrossCheckRequired {
			if apitypes.IsTerminalRunStatus(r.Status) {
				return [][]string{{"CODE_CHECK", "Incomplete · outcome unavailable"}}
			}
			return [][]string{{"CODE_CHECK", "Pending · evidence unavailable"}}
		}
		return nil
	}
	label, status := "CODE_CHECK", "Incomplete · outcome unavailable"
	if s.InterruptedAt != nil {
		label, status = "EARLIER_CODE_CHECK", "Earlier attempt evidence · incomplete: interrupted"
	} else {
		switch s.Outcome {
		case "pending":
			status = "Pending"
		case "completed":
			status = "Completed"
		case "failed":
			status = "Incomplete"
		}
	}
	rows := [][]string{{label, status}}
	if s.ReasonClass != nil {
		reason := strings.ReplaceAll(crossCheckPlain(*s.ReasonClass), "_", " ")
		rows = append(rows, []string{label + "_REASON", reason})
	}
	if s.HeadCommit != nil && codeCheckedSHA.MatchString(*s.HeadCommit) {
		rows = append(rows, []string{label + "_CHECKED_SHA", crossCheckPlain(*s.HeadCommit)})
	}
	for _, field := range []struct {
		key   string
		value *string
	}{
		{"CHECKER_ID", s.CheckerRunID}, {"FAMILY", s.CheckerHarness},
		{"MODEL", s.CheckerModel}, {"EFFORT", s.CheckerEffort},
	} {
		value := "unavailable"
		if field.value != nil {
			if text := crossCheckPlain(*field.value); strings.TrimSpace(text) != "" {
				value = text
			}
		}
		rows = append(rows, []string{label + "_" + field.key, value})
	}
	cost := "cost unavailable"
	if s.Usage != nil {
		cost = crossCheckPlain(costDetailCell(*s.Usage))
	}
	rows = append(rows, []string{label + "_COST", cost})
	if s.CustomGuidanceApplied {
		digest := []rune(crossCheckPlain(s.GuidanceDigest))
		if len(digest) > 12 {
			digest = digest[:12]
		}
		rows = append(rows, []string{label + "_GUIDANCE", "custom guidance applied (sha256 " + string(digest) + ")"})
	}
	// The raw byte bound precedes parsing, including for legacy responses.
	type finding struct {
		ID       string `json:"id"`
		Severity string `json:"severity"`
		Path     string `json:"path"`
		Line     *int32 `json:"line"`
		Title    string `json:"title"`
		Detail   string `json:"detail"`
	}
	var findings []finding
	if len(s.Findings) > 64*1024 || json.Unmarshal(s.Findings, &findings) != nil || findings == nil {
		return append(rows, []string{label + "_FINDINGS", "Findings unavailable"},
			[]string{label + "_DISPOSITIONS", "Dispositions unavailable"})
	}
	ids := make([]string, len(findings))
	for i, f := range findings {
		ids[i] = f.ID
	}
	dispositions, dispositionState := codeDispositionEvidence(s, ids)
	rows = append(rows, []string{label + "_DISPOSITIONS", dispositionState})
	if dispositionState == "Finalized" && s.Outcome == "completed" && s.InterruptedAt == nil {
		counts := map[string]int{"addressed": 0, "declined": 0, "not_reported": 0}
		for _, d := range dispositions {
			counts[d.Disposition]++
		}
		rows = append(rows, []string{label + "_COUNTS", fmt.Sprintf("%d addressed, %d declined, %d not reported", counts["addressed"], counts["declined"], counts["not_reported"])})
	}
	if len(findings) == 0 {
		return append(rows, []string{label + "_FINDINGS", "No findings recorded"})
	}
	// Display at most 20 items. A rejected ID never becomes a different key;
	// sibling findings still render, but duplicates cannot claim that identity.
	seen := make(map[string]bool)
	for i, f := range findings[:min(len(findings), 20)] {
		id := "ID unavailable"
		if codeFindingID.MatchString(f.ID) && !seen[f.ID] {
			id = f.ID
			seen[f.ID] = true
		}
		prefix := fmt.Sprintf("%s_%d", label, i+1)
		rows = append(rows,
			[]string{prefix + "_ID", crossCheckPlain(id)},
			[]string{prefix + "_SEVERITY", crossCheckPlain(f.Severity)},
			[]string{prefix + "_PATH", crossCheckPlain(f.Path)},
			[]string{prefix + "_TITLE", crossCheckPlain(f.Title)},
			[]string{prefix + "_DETAIL", crossCheckPlain(f.Detail)})
		if d, ok := dispositions[f.ID]; ok && id == f.ID {
			rows = append(rows, []string{prefix + "_DISPOSITION", d.Disposition},
				[]string{prefix + "_DISPOSITION_REASON", crossCheckPlain(*d.Reason)})
		}
		if f.Line != nil && *f.Line > 0 {
			rows = append(rows, []string{prefix + "_LINE", fmt.Sprint(*f.Line)})
		}
	}
	if len(findings) > 20 {
		rows = append(rows, []string{label + "_MORE", fmt.Sprintf("%d findings omitted", len(findings)-20)})
	}
	return rows
}
