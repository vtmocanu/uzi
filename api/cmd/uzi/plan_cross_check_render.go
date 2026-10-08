package main

import (
	"fmt"
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// Limit hostile DTO text before the terminal renderer scans it. Plain still
// performs the terminal/control sanitization and the final cell bound.
func crossCheckPlain(s string) string {
	if len(s) > 8192 {
		s = s[:8192]
	}
	return (&tuiRenderer{}).Plain(s, 200)
}

// crossCheckSource renders only the closed provenance vocabulary. Older
// records and unfamiliar server values cannot assert a known source.
func crossCheckSource(source *string) string {
	if source != nil {
		switch *source {
		case "pin", "worker default":
			return crossCheckPlain(*source)
		}
	}
	return "unknown"
}

func planCrossCheckRows(r apitypes.RunDTO) [][]string {
	var rows [][]string
	if r.Status == "awaiting_approval" && r.PlanCrossCheckRequired && r.PlanCrossCheckGateReason != nil {
		if reason := crossCheckPlain(*r.PlanCrossCheckGateReason); strings.TrimSpace(reason) != "" {
			text := "plan cross-check: " + strings.ReplaceAll(reason, "_", " ")
			if reason == "planning_diff_refused" && r.PlanCrossCheckDiffRefusal != nil {
				if sub := crossCheckPlain(*r.PlanCrossCheckDiffRefusal); strings.TrimSpace(sub) != "" {
					text += " (" + strings.ReplaceAll(sub, "_", " ") + ")"
				}
			}
			rows = append(rows, []string{"PLAN_CROSS_CHECK", text})
		}
	}
	s := r.PlanCrossCheckSummary
	if s == nil {
		return rows
	}
	label := "PLAN_CHECK"
	if s.Historical {
		label = "EARLIER_PLAN_CHECK"
		rows = append(rows, []string{label, "Earlier-plan findings; the current plan is not certified by this check"})
	}
	rows = append(rows, []string{label + "_RESULT", crossCheckPlain(s.Verdict)})
	if s.ReasonClass != nil {
		rows = append(rows, []string{label + "_REASON", crossCheckPlain(*s.ReasonClass)})
	}
	if s.CheckerRunID != nil && *s.CheckerRunID != "" {
		rows = append(rows, []string{label + "_CHECKER_ID", crossCheckPlain(*s.CheckerRunID)})
	}
	if s.CheckerModel != nil {
		rows = append(rows, []string{label + "_MODEL", crossCheckPlain(*s.CheckerModel)})
	}
	if s.CheckerEffort != nil {
		rows = append(rows, []string{label + "_EFFORT", crossCheckPlain(*s.CheckerEffort)})
	}
	rows = append(rows,
		[]string{label + "_MODEL_SOURCE", crossCheckSource(s.CheckerModelSource)},
		[]string{label + "_EFFORT_SOURCE", crossCheckSource(s.CheckerEffortSource)})
	cost := "cost unavailable"
	if s.Usage != nil {
		cost = costDetailCell(*s.Usage)
		rows = append(rows, []string{label + "_TOKENS", fmt.Sprintf("%d in / %d out / %d cache read / %d cache creation",
			s.Usage.InputTokens, s.Usage.OutputTokens, s.Usage.CacheReadTokens, s.Usage.CacheCreationTokens)})
	}
	rows = append(rows, []string{label + "_COST", cost})
	if f := s.Findings; f != nil {
		rows = append(rows, []string{label + "_SUMMARY", crossCheckPlain(f.Summary)})
		// A malformed client response cannot turn the detail table into an
		// unbounded findings stream; every displayed field uses Plain.
		for i, item := range f.Items[:min(len(f.Items), 20)] {
			prefix := fmt.Sprintf("%s_%d", label, i+1)
			rows = append(rows,
				[]string{prefix + "_FILE", crossCheckPlain(item.File)},
				[]string{prefix + "_SEVERITY", crossCheckPlain(item.Severity)},
				[]string{prefix + "_SUMMARY", crossCheckPlain(item.Summary)},
				[]string{prefix + "_RATIONALE", crossCheckPlain(item.Rationale)})
		}
		if len(f.Items) > 20 {
			rows = append(rows, []string{label + "_MORE", fmt.Sprintf("%d findings omitted", len(f.Items)-20)})
		}
	}
	return rows
}
