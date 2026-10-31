package runpresentation

import (
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/leadcfg"
)

// LeadHistoryCells are the four text columns of one `uzi run lead-config` row (PRD #3244 section 8,
// M3): what was requested, where each requested value came from, what the runtime reported, and
// the evidence for it. The strings carry worker-attested model text, so a terminal renderer must
// sanitize them before printing.
type LeadHistoryCells struct {
	Requested string
	Source    string
	Observed  string
	Evidence  string
}

// LeadHistoryRowCells renders one recorded row with the same labels and rules as the run header
// (modelText, rowLines): the observed model and effort get their own cells.
// The one place the requested cell reads the observation is modelText: a builtin model source with
// no requested model shows the observed model there (PRD section 8 rule 4).
func LeadHistoryRowCells(p apitypes.LeadPhaseConfigDTO) LeadHistoryCells {
	runtimeDefault := leadcfg.SourceLabel("effort", string(leadcfg.EffortModeRuntimeDefault), p.Harness)

	reqEffort := runtimeDefault
	effortSource := runtimeDefault
	if p.RequestedEffortMode != string(leadcfg.EffortModeRuntimeDefault) && p.RequestedEffort != nil && *p.RequestedEffort != "" {
		reqEffort = *p.RequestedEffort
		effortSource = leadcfg.SourceLabel("effort", p.RequestedEffortSource, p.Harness)
	}

	modelSource := leadcfg.SourceLabel("model", p.RequestedModelSource, p.Harness)
	if (p.RequestedModel == nil || *p.RequestedModel == "") && p.RequestedModelSource == string(leadcfg.ModelSourceBuiltin) {
		modelSource = leadcfg.SDKDefaultModelLabel
	}

	obsModel := "-"
	if p.ObservedModel != nil && *p.ObservedModel != "" {
		obsModel = *p.ObservedModel
	}
	obsEffort := "-"
	if p.ObservedEffort != nil && *p.ObservedEffort != "" {
		obsEffort = *p.ObservedEffort
		if obsEffort == leadcfg.ObservedEffortOther {
			obsEffort = leadcfg.ObservedEffortOtherLabel
		}
	} else {
		switch leadcfg.EffortEvidence(derefStr(p.ObservedEffortEvidence)) {
		case leadcfg.EffortEvidenceClaudeInit, leadcfg.EffortEvidenceClaudeGetSettings:
			obsEffort = leadcfg.EffortNotSentLabel
		}
	}

	return LeadHistoryCells{
		Requested: modelText(p) + " · " + reqEffort,
		Source:    "model: " + modelSource + "; effort: " + effortSource,
		Observed:  obsModel + " · " + obsEffort,
		Evidence: "model: " + leadcfg.EvidenceLabel(derefStr(p.ObservedModelEvidence)) +
			"; effort: " + leadcfg.EvidenceLabel(derefStr(p.ObservedEffortEvidence)),
	}
}
