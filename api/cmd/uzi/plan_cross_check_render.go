package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
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

// crossCheckEffortSource also accepts the unpinned cross-check default, which only
// an effort can carry.
func crossCheckEffortSource(source *string) string {
	if source != nil && *source == "cross-check default" {
		return *source
	}
	return crossCheckSource(source)
}

// maxCrossCheckRounds bounds the rounds a client renders, matching the server's cap.
const maxCrossCheckRounds = 5

func planCrossCheckRows(r apitypes.RunDTO) [][]string {
	var rows [][]string
	if r.PlanCrossCheckGated {
		rows = append(rows, []string{"PLAN_CHECK_MODE", "checked before your review"})
	}
	if r.Status == "awaiting_approval" && (r.PlanCrossCheckRequired || r.PlanCrossCheckGated) && r.PlanCrossCheckGateReason != nil {
		if reason := crossCheckPlain(*r.PlanCrossCheckGateReason); strings.TrimSpace(reason) != "" {
			text := "plan cross-check: " + strings.ReplaceAll(reason, "_", " ")
			if r.PlanCrossCheckGated && reason == "worker_unsupported" {
				text = "plan cross-check: unavailable, not supported on this worker"
			}
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
		// Not "not checked" while the gated run is still before its first gate (the web's
		// pending rule: not terminal, not at the gate, no plan presented yet).
		presented := r.Status == "awaiting_approval" || uzicli.IsTerminalRunStatus(r.Status) ||
			(r.PlanMd != nil && strings.TrimSpace(*r.PlanMd) != "")
		if presented && r.PlanCrossCheckGated && r.PlanCrossCheckGateReason == nil {
			rows = append(rows, []string{"PLAN_CROSS_CHECK", "plan cross-check: not checked"})
		}
		return rows
	}
	if r.PlanCrossCheckGated && r.PlanCrossCheckGateReason == nil && s.Historical {
		rows = append(rows, []string{"PLAN_CROSS_CHECK", "plan cross-check: checked an earlier version of this plan"})
	}
	// Shared "passed" rule (any status): a gated run with no gate reason, whose latest
	// check approved the current plan (not historical), may say it passed.
	if r.PlanCrossCheckGated && r.PlanCrossCheckGateReason == nil &&
		!s.Historical && s.Verdict == "approve" && s.Round > 0 {
		word := "rounds"
		if s.Round == 1 {
			word = "round"
		}
		rows = append(rows, []string{"PLAN_CROSS_CHECK", fmt.Sprintf("plan cross-check: passed after %d %s", s.Round, word)})
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
		[]string{label + "_EFFORT_SOURCE", crossCheckEffortSource(s.CheckerEffortSource)})
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
	if s.DeadlineAt != nil {
		rows = append(rows, []string{label + "_DEADLINE", s.DeadlineAt.UTC().Format(time.RFC3339)})
	}
	if len(s.Rounds) > 0 {
		rounds := s.Rounds[max(0, len(s.Rounds)-maxCrossCheckRounds):]
		rows = append(rows, []string{label + "_ROUNDS", fmt.Sprintf("%d", len(rounds))})
		for _, rd := range rounds {
			// The summary rows above already show the latest round's findings.
			rows = append(rows, planCrossCheckRoundRows(label, rd, s.Findings != nil && rd.Round == s.Round)...)
		}
	}
	return rows
}

// planCrossCheckRoundRows renders one round. The lead's response is untrusted
// prose, so it goes through crossCheckPlain like every checker string; the label
// says it is a claim, never a verified resolution.
func planCrossCheckRoundRows(label string, rd apitypes.PlanCrossCheckRoundDTO, findingsShown bool) [][]string {
	p := fmt.Sprintf("%s_ROUND_%d", label, rd.Round)
	rows := [][]string{{p + "_RESULT", crossCheckPlain(rd.Verdict)}}
	if f := rd.Findings; f != nil && !findingsShown {
		rows = append(rows, []string{p + "_SUMMARY", crossCheckPlain(f.Summary)})
		for i, item := range f.Items[:min(len(f.Items), 20)] {
			ip := fmt.Sprintf("%s_%d", p, i+1)
			rows = append(rows,
				[]string{ip + "_FILE", crossCheckPlain(item.File)},
				[]string{ip + "_SEVERITY", crossCheckPlain(item.Severity)},
				[]string{ip + "_SUMMARY", crossCheckPlain(item.Summary)},
				[]string{ip + "_RATIONALE", crossCheckPlain(item.Rationale)})
		}
		if len(f.Items) > 20 {
			rows = append(rows, []string{p + "_MORE", fmt.Sprintf("%d findings omitted", len(f.Items)-20)})
		}
	}
	if rd.Response.State != "" {
		rows = append(rows, []string{p + "_RESPONSE_STATE", crossCheckPlain(rd.Response.State)})
		if t := crossCheckPlain(rd.Response.Text); strings.TrimSpace(t) != "" {
			rows = append(rows, []string{p + "_RESPONSE", "lead's response (unverified claims): " + t})
		}
	}
	return rows
}
