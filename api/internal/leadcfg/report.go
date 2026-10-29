package leadcfg

import (
	"fmt"
	"slices"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// MaxPhaseEntry and MaxSeq bound a row's phase_entry and seq (the table CHECKs of migration 00379).
const (
	MaxPhaseEntry = 8
	MaxSeq        = 32
)

// FieldError names the request field a report failed on and why. It never carries the offending value,
// so the 400 it becomes cannot echo worker-attested text.
type FieldError struct {
	Field  string
	Reason string
}

func (e *FieldError) Error() string {
	return fmt.Sprintf("invalid lead config: %s %s", e.Field, e.Reason)
}

// Report is a validated lead-config report: bounds and closed enums checked, model strings trimmed.
type Report struct {
	ClaimGeneration        int64
	Phase                  Phase
	PhaseEntry             int16
	Seq                    int16
	Truncated              bool
	RequestedModel         *string
	RequestedModelSource   ModelSource
	RequestedEffort        *string
	RequestedEffortMode    EffortMode
	RequestedEffortSource  EffortSource
	ObservedModel          *string
	ObservedModelEvidence  *string
	ObservedEffort         *string
	ObservedEffortEvidence *string
}

// ValidateReport checks every bound of the section 1 table and returns the normalized report, or a
// *FieldError. The one table check it cannot make is the harness-dependent one (a Codex row must carry a
// model), which needs runs.harness and stays in the database as run_lead_configs_requested_model_check.
// A missing or sub-1 claim_generation is reported on the field claim_generation (the handler answers it
// as claim_generation_required).
func ValidateReport(req apitypes.LeadConfigReportRequest) (Report, error) {
	var r Report
	if req.ClaimGeneration == nil {
		return r, &FieldError{"claim_generation", "is required"}
	}
	if *req.ClaimGeneration < 1 {
		return r, &FieldError{"claim_generation", "must be at least 1"}
	}
	r.ClaimGeneration = *req.ClaimGeneration

	if !slices.Contains(Phases(), Phase(req.Phase)) {
		return r, &FieldError{"phase", "is not a known phase"}
	}
	r.Phase = Phase(req.Phase)
	if req.PhaseEntry < 1 || req.PhaseEntry > MaxPhaseEntry {
		return r, &FieldError{"phase_entry", fmt.Sprintf("must be between 1 and %d", MaxPhaseEntry)}
	}
	r.PhaseEntry = int16(req.PhaseEntry)
	if req.Seq < 1 || req.Seq > MaxSeq {
		return r, &FieldError{"seq", fmt.Sprintf("must be between 1 and %d", MaxSeq)}
	}
	r.Seq = int16(req.Seq)
	r.Truncated = req.Truncated
	if r.Truncated && req.Seq != MaxSeq {
		return r, &FieldError{"truncated", "is only allowed on the final row"}
	}

	var err error
	if r.RequestedModel, err = validateModel("requested_model", req.RequestedModel); err != nil {
		return r, err
	}
	if !slices.Contains(ModelSources(), ModelSource(req.RequestedModelSource)) {
		return r, &FieldError{"requested_model_source", "is not a known source"}
	}
	r.RequestedModelSource = ModelSource(req.RequestedModelSource)
	// A NULL model is only the built-in Claude SDK default; whether the run is Claude is checked in the DB.
	if r.RequestedModel == nil && r.RequestedModelSource != ModelSourceBuiltin {
		return r, &FieldError{"requested_model", "is required unless the source is builtin"}
	}

	if req.RequestedEffort != nil && !slices.Contains(RequestedEfforts(), *req.RequestedEffort) {
		return r, &FieldError{"requested_effort", "is not a known level"}
	}
	r.RequestedEffort = req.RequestedEffort
	if !slices.Contains(EffortModes(), EffortMode(req.RequestedEffortMode)) {
		return r, &FieldError{"requested_effort_mode", "is not a known mode"}
	}
	r.RequestedEffortMode = EffortMode(req.RequestedEffortMode)
	if (r.RequestedEffortMode == EffortModeExplicit) != (r.RequestedEffort != nil) {
		return r, &FieldError{"requested_effort_mode", "must be explicit exactly when requested_effort is set"}
	}
	if !slices.Contains(EffortSources(), EffortSource(req.RequestedEffortSource)) {
		return r, &FieldError{"requested_effort_source", "is not a known source"}
	}
	r.RequestedEffortSource = EffortSource(req.RequestedEffortSource)

	if r.ObservedModel, err = validateModel("observed_model", req.ObservedModel); err != nil {
		return r, err
	}
	if req.ObservedModelEvidence != nil && !slices.Contains(ModelEvidences(), ModelEvidence(*req.ObservedModelEvidence)) {
		return r, &FieldError{"observed_model_evidence", "is not a known evidence token"}
	}
	r.ObservedModelEvidence = req.ObservedModelEvidence
	// An observed model needs real evidence; no model goes with NULL or unknown evidence.
	realModelEvidence := r.ObservedModelEvidence != nil && *r.ObservedModelEvidence != string(ModelEvidenceUnknown)
	if (r.ObservedModel != nil) != realModelEvidence {
		return r, &FieldError{"observed_model_evidence", "must be a runtime token exactly when observed_model is set"}
	}

	if req.ObservedEffort != nil && !slices.Contains(ObservedEfforts(), *req.ObservedEffort) {
		return r, &FieldError{"observed_effort", "is not a known level"}
	}
	r.ObservedEffort = req.ObservedEffort
	if req.ObservedEffortEvidence != nil && !slices.Contains(effortEvidenceStrings(), *req.ObservedEffortEvidence) {
		return r, &FieldError{"observed_effort_evidence", "is not a known evidence token"}
	}
	r.ObservedEffortEvidence = req.ObservedEffortEvidence
	if err := checkObservedEffortPairing(r.ObservedEffort, r.ObservedEffortEvidence); err != nil {
		return r, err
	}
	return r, nil
}

// checkObservedEffortPairing mirrors run_lead_configs_observed_effort_pairing_check: a level needs real
// evidence; a NULL level is "not sent" with Claude evidence (D10), unobserved with NULL or unknown
// evidence, and forbidden with Codex evidence (a Codex null maps to unknown).
func checkObservedEffortPairing(level, evidence *string) error {
	if level != nil {
		if evidence == nil || *evidence == string(EffortEvidenceUnknown) {
			return &FieldError{"observed_effort_evidence", "must be a runtime token when observed_effort is set"}
		}
		return nil
	}
	if evidence != nil && (*evidence == string(EffortEvidenceCodexSent) || *evidence == string(EffortEvidenceCodexRuntimeDefault)) {
		return &FieldError{"observed_effort_evidence", "cannot describe a missing level"}
	}
	return nil
}

func effortEvidenceStrings() []string {
	all := EffortEvidences()
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = string(e)
	}
	return out
}

// validateModel applies agenttmpl.ValidateModel to a non-nil model; an empty or blank value is an error
// (a NULL model is expressed as null, never as an empty string).
func validateModel(field string, raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	m, err := agenttmpl.ValidateModel(*raw)
	if err != nil {
		return nil, &FieldError{field, "is not a valid model token"}
	}
	if m == "" {
		return nil, &FieldError{field, "must not be empty"}
	}
	return &m, nil
}
