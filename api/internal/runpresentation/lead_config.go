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
	leadPreClaimChipModelTBD  = "lead: model resolves at claim · "
	leadNotRecordedChip       = "lead: not recorded"
	leadDefaultModelText      = "default"
	leadStatusQueued          = "queued"
	leadStatusPaused          = "paused"
)

// LeadPreClaim is what the run itself says before any lead row exists: the model and effort chosen at
// create. ModelSource and EffortSource name their writers (run_override or schedule_pin for the model,
// run_override for the effort). A Model with no ModelSource is a legacy row an older binary wrote and
// reads as schedule_pin (PRD #3250 DC-P8).
type LeadPreClaim struct {
	Model        string `json:"model,omitempty"`
	ModelSource  string `json:"modelSource,omitempty"`
	Effort       string `json:"effort,omitempty"`
	EffortSource string `json:"effortSource,omitempty"`
}

func (p LeadPreClaim) modelSource() string {
	if p.ModelSource == "" {
		return string(leadcfg.ModelSourceSchedulePin)
	}
	return p.ModelSource
}

func (p LeadPreClaim) effortSource() string {
	if p.EffortSource == "" {
		return string(leadcfg.EffortSourceRunOverride)
	}
	return p.EffortSource
}

func (p LeadPreClaim) hasRunOverride() bool {
	return (p.Model != "" && p.modelSource() == string(leadcfg.ModelSourceRunOverride)) || p.Effort != ""
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

// LeadConfigViewInputFromRun builds the view input from a run: the pre-claim model and effort are the
// run's create-time choice with their recorded sources; a model with a NULL source reads as schedule_pin.
func LeadConfigViewInputFromRun(run apitypes.RunDTO) LeadConfigViewInput {
	in := LeadConfigViewInput{
		Status:     run.Status,
		Harness:    run.Harness,
		LeadConfig: run.LeadConfig,
	}
	if run.Model != nil {
		in.PreClaim.Model = *run.Model
		in.PreClaim.ModelSource = string(leadcfg.ModelSourceSchedulePin)
		if run.ModelSource != nil && *run.ModelSource != "" {
			in.PreClaim.ModelSource = *run.ModelSource
		}
	}
	if run.Effort != nil {
		in.PreClaim.Effort = *run.Effort
		in.PreClaim.EffortSource = string(leadcfg.EffortSourceRunOverride)
		if run.EffortSource != nil && *run.EffortSource != "" {
			in.PreClaim.EffortSource = *run.EffortSource
		}
	}
	return in
}

// LeadConfigView renders PRD #3244 section 8 rules 1 to 7.
func LeadConfigView(input LeadConfigViewInput) LeadConfigViewResult {
	lc := input.LeadConfig
	if lc == nil || !lc.Applicable {
		return LeadConfigViewResult{}
	}
	if input.Status == leadStatusQueued || (input.Status == leadStatusPaused && len(lc.Phases) == 0 && input.PreClaim.hasRunOverride()) {
		// A run held before its first report (run_override_unavailable) shows the create-time choice
		// as a queued run does.
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
	chip := preClaimChip(input.PreClaim)
	lines := preClaimLines(input)
	if phases := input.LeadConfig.Phases; len(phases) > 0 {
		_, chips := shownRows(phases)
		lines = append(lines, "previous attempt: "+strings.Join(chips, " → "))
	}
	return LeadConfigViewResult{Chip: chip, Lines: append(lines, leadSubagentNote)}
}

func preClaimChip(p LeadPreClaim) string {
	switch {
	case p.Model != "" && p.Effort != "":
		return "lead: " + p.Model + " · " + p.Effort
	case p.Model != "":
		return "lead: " + p.Model + leadPreClaimChipEffortTBD
	case p.Effort != "":
		return leadPreClaimChipModelTBD + p.Effort
	}
	return leadPreClaimChipUnfixed
}

func preClaimLines(input LeadConfigViewInput) []string {
	var lines []string
	if p := input.PreClaim; p.Model != "" {
		lines = append(lines, valueLine(p.Model, leadcfg.SourceLabel("model", p.modelSource(), input.Harness)))
	}
	if p := input.PreClaim; p.Effort != "" {
		lines = append(lines, valueLine(p.Effort, leadcfg.SourceLabel("effort", p.effortSource(), input.Harness)))
	}
	return lines
}

func rowsLeadView(input LeadConfigViewInput) LeadConfigViewResult {
	lc := input.LeadConfig
	rows, chips := shownRows(lc.Phases)
	prefixed := len(rows) > 1

	var lines []string
	truncated := false
	for _, p := range rows {
		prefix := ""
		if prefixed {
			prefix = p.Phase + " "
		}
		for _, l := range rowLines(p, input.Harness) {
			lines = append(lines, prefix+l)
		}
		truncated = truncated || p.Truncated
	}
	if !lc.Current {
		lines = append(lines, leadEarlierAttemptNote)
	}
	if truncated {
		lines = append(lines, leadTruncatedNote)
	}
	return LeadConfigViewResult{Chip: "lead: " + strings.Join(chips, " → "), Lines: append(lines, leadSubagentNote)}
}

// shownRows collapses identical rows to the last one and returns the rows shown with their chip segments
// (phase-prefixed only when more than one row is shown); the chip and the queued "previous attempt" line share it.
func shownRows(phases []apitypes.LeadPhaseConfigDTO) ([]apitypes.LeadPhaseConfigDTO, []string) {
	rows := phases
	if len(rows) > 1 && allSameText(rows) {
		rows = rows[len(rows)-1:]
	}
	chips := make([]string, 0, len(rows))
	for _, p := range rows {
		prefix := ""
		if len(rows) > 1 {
			prefix = p.Phase + " "
		}
		chips = append(chips, prefix+rowText(p))
	}
	return rows, chips
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
