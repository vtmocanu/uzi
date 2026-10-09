package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

var codeFindingID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
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
		return append(rows, []string{label + "_FINDINGS", "Findings unavailable"})
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
		if f.Line != nil && *f.Line > 0 {
			rows = append(rows, []string{prefix + "_LINE", fmt.Sprint(*f.Line)})
		}
	}
	if len(findings) > 20 {
		rows = append(rows, []string{label + "_MORE", fmt.Sprintf("%d findings omitted", len(findings)-20)})
	}
	return rows
}
