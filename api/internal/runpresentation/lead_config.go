package runpresentation

import (
	"strings"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
)

// The lead-config display rule (PRD #3244 section 8), shared by the run header chip and `uzi run get`.
// It is pinned to the display_cases of fixtures/lead-config/contract.json together with its TypeScript twin
// in web/src/lib/leadConfig.ts. Every label comes from leadcfg's label table, never from a literal here.
const (
	leadSubagentNote          = "This is the lead agent; subagents with their own pin may differ."
	leadNotRecordedReason     = "the worker did not record it (an older worker, or the run ended before its first lead turn)"
	leadEarlierAttemptNote    = "recorded by an earlier attempt; this attempt has not recorded yet"
	leadTruncatedNote         = "(later changes not recorded)"
	leadPreClaimChipUnfixed   = "lead: model and effort resolve at claim"
	leadPreClaimChipEffortTBD = " · effort resolves at claim"
	leadNotRecordedChip       = "lead: not recorded"
	leadDefaultModelText      = "default"
	leadStatusQueued          = "queued"
)

// LeadPreClaim is what the run itself says before any lead row exists. ModelSource names the writer of
// Model; H's adapter sets it to schedule_pin, the only writer today.
type LeadPreClaim struct {
	Model       string `json:"model,omitempty"`
	ModelSource string `json:"modelSource,omitempty"`
}

// LeadConfigViewInput is the neutral input of LeadConfigView; it is the JSON shape of a display_cases input.
type LeadConfigViewInput struct {
	Status     string                     `json:"status"`
	Harness    string                     `json:"harness"`
	PreClaim   LeadPreClaim               `json:"preClaim"`
	LeadConfig *apitypes.RunLeadConfigDTO `json:"leadConfig,omitempty"`
}

// LeadConfigViewResult is the presentation: Chip is the visible text ("" = show nothing) and Lines are the
// tooltip / LEAD_SOURCE lines, the last always the subagent note when a chip exists.
type LeadConfigViewResult struct {
	Chip  string
	Lines []string
}

// LeadConfigViewInputFromRun builds the view input from a run: the pre-claim model is run.Model, whose only
// writer today is a schedule pin.
func LeadConfigViewInputFromRun(run apitypes.RunDTO) LeadConfigViewInput {
	in := LeadConfigViewInput{
		Status:     run.Status,
		Harness:    run.Harness,
		PreClaim:   LeadPreClaim{ModelSource: string(leadcfg.ModelSourceSchedulePin)},
		LeadConfig: run.LeadConfig,
	}
	if run.Model != nil {
		in.PreClaim.Model = *run.Model
	}
	return in
}

// LeadConfigView renders PRD #3244 section 8 rules 1 to 7.
func LeadConfigView(input LeadConfigViewInput) LeadConfigViewResult {
	lc := input.LeadConfig
	if lc == nil || !lc.Applicable {
		return LeadConfigViewResult{}
	}
	if input.Status == leadStatusQueued {
		return queuedLeadView(input)
	}
	if len(lc.Phases) == 0 {
		lines := []string{leadNotRecordedReason}
		lines = append(lines, preClaimLines(input)...)
		return LeadConfigViewResult{Chip: leadNotRecordedChip, Lines: append(lines, leadSubagentNote)}
	}
	return rowsLeadView(input)
}

func queuedLeadView(input LeadConfigViewInput) LeadConfigViewResult {
	chip := leadPreClaimChipUnfixed
	if input.PreClaim.Model != "" {
		chip = "lead: " + input.PreClaim.Model + leadPreClaimChipEffortTBD
	}
	lines := preClaimLines(input)
	if phases := input.LeadConfig.Phases; len(phases) > 0 {
		lines = append(lines, "previous attempt: "+rowText(phases[len(phases)-1]))
	}
	return LeadConfigViewResult{Chip: chip, Lines: append(lines, leadSubagentNote)}
}

func preClaimLines(input LeadConfigViewInput) []string {
	if input.PreClaim.Model == "" {
		return nil
	}
	label := leadcfg.SourceLabel("model", input.PreClaim.ModelSource, input.Harness)
	return []string{valueLine(input.PreClaim.Model, label)}
}

func rowsLeadView(input LeadConfigViewInput) LeadConfigViewResult {
	lc := input.LeadConfig
	rows := lc.Phases
	if len(rows) > 1 && allSameText(rows) {
		rows = rows[len(rows)-1:]
	}
	prefixed := len(rows) > 1

	var chips, lines []string
	truncated := false
	for _, p := range rows {
		prefix := ""
		if prefixed {
			prefix = p.Phase + " "
		}
		chips = append(chips, prefix+rowText(p))
		for _, l := range rowLines(p, input.Harness) {
			lines = append(lines, prefix+l)
		}
		truncated = truncated || p.Truncated
	}
	if truncated {
		lines = append(lines, leadTruncatedNote)
	}
	if !lc.Current {
		lines = append(lines, leadEarlierAttemptNote)
	}
	return LeadConfigViewResult{Chip: "lead: " + strings.Join(chips, " → "), Lines: append(lines, leadSubagentNote)}
}

func allSameText(rows []apitypes.LeadPhaseConfigDTO) bool {
	first := rowText(rows[0])
	for _, p := range rows[1:] {
		if rowText(p) != first {
			return false
		}
	}
	return true
}

func rowText(p apitypes.LeadPhaseConfigDTO) string {
	return modelText(p) + " · " + effortText(p)
}

func modelText(p apitypes.LeadPhaseConfigDTO) string {
	switch {
	case p.RequestedModel != nil && *p.RequestedModel != "":
		return *p.RequestedModel
	case p.RequestedModelSource == string(leadcfg.ModelSourceBuiltin) && p.ObservedModel != nil && *p.ObservedModel != "":
		return *p.ObservedModel
	default:
		return leadDefaultModelText
	}
}

// concreteObserved returns the observed effort when it is a real level ("other" is never one).
func concreteObserved(p apitypes.LeadPhaseConfigDTO) (string, bool) {
	if p.ObservedEffort == nil || *p.ObservedEffort == "" || *p.ObservedEffort == leadcfg.ObservedEffortOther {
		return "", false
	}
	return *p.ObservedEffort, true
}

func isRuntimeReport(evidence *string) bool {
	if evidence == nil {
		return false
	}
	switch leadcfg.EffortEvidence(*evidence) {
	case leadcfg.EffortEvidenceClaudeInit, leadcfg.EffortEvidenceClaudeGetSettings, leadcfg.EffortEvidenceCodexRuntimeDefault:
		return true
	}
	return false
}

func effortText(p apitypes.LeadPhaseConfigDTO) string {
	runtimeDefault := leadcfg.SourceLabel("effort", string(leadcfg.EffortModeRuntimeDefault), p.Harness)
	obs, hasObs := concreteObserved(p)
	if p.RequestedEffortMode == string(leadcfg.EffortModeRuntimeDefault) {
		if hasObs {
			return runtimeDefault + ": " + obs
		}
		return runtimeDefault
	}
	if p.RequestedEffort == nil || *p.RequestedEffort == "" {
		return runtimeDefault
	}
	req := *p.RequestedEffort
	if hasObs && obs != req && isRuntimeReport(p.ObservedEffortEvidence) {
		return obs + " (requested " + req + ")"
	}
	return req
}

func rowLines(p apitypes.LeadPhaseConfigDTO, fallbackHarness string) []string {
	harness := p.Harness
	if harness == "" {
		harness = fallbackHarness
	}
	var lines []string

	modelLabel := leadcfg.SourceLabel("model", p.RequestedModelSource, harness)
	if (p.RequestedModel == nil || *p.RequestedModel == "") && p.RequestedModelSource == string(leadcfg.ModelSourceBuiltin) {
		modelLabel = leadcfg.SDKDefaultModelLabel
	}
	lines = append(lines, valueLine(modelText(p), modelLabel))

	if p.RequestedEffortMode == string(leadcfg.EffortModeRuntimeDefault) || p.RequestedEffort == nil || *p.RequestedEffort == "" {
		lines = append(lines, leadcfg.SourceLabel("effort", string(leadcfg.EffortModeRuntimeDefault), harness))
	} else {
		lines = append(lines, valueLine(*p.RequestedEffort, leadcfg.SourceLabel("effort", p.RequestedEffortSource, harness)))
	}

	lines = append(lines, "runtime: "+observedModelText(p), "effort: "+observedEffortText(p))
	return lines
}

func observedModelText(p apitypes.LeadPhaseConfigDTO) string {
	ev := derefStr(p.ObservedModelEvidence)
	if p.ObservedModel == nil || *p.ObservedModel == "" {
		return leadcfg.EvidenceLabel(ev)
	}
	return withEvidence(*p.ObservedModel, ev)
}

func observedEffortText(p apitypes.LeadPhaseConfigDTO) string {
	ev := derefStr(p.ObservedEffortEvidence)
	if p.ObservedEffort == nil || *p.ObservedEffort == "" {
		switch leadcfg.EffortEvidence(ev) {
		case leadcfg.EffortEvidenceClaudeInit, leadcfg.EffortEvidenceClaudeGetSettings:
			return leadcfg.EffortNotSentLabel
		}
		return leadcfg.EvidenceLabel(ev)
	}
	level := *p.ObservedEffort
	if level == leadcfg.ObservedEffortOther {
		level = leadcfg.ObservedEffortOtherLabel
	}
	return withEvidence(level, ev)
}

func withEvidence(value, evidence string) string {
	if label := leadcfg.EvidenceLabel(evidence); label != "" && evidence != "" {
		return value + " (" + label + ")"
	}
	return value
}

func valueLine(value, label string) string {
	if label == "" {
		return value
	}
	return value + ": " + label
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
