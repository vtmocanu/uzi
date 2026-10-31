package workersvc

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// LeadConfigFieldError is a database CHECK refusal of a validated report, named by the request
// field it concerns (400 invalid_lead_config). It carries the field only, never a value.
type LeadConfigFieldError struct{ Field string }

func (e *LeadConfigFieldError) Error() string { return "invalid lead config: " + e.Field }

// RecordResult is the outcome of an admitted lead-config write.
type RecordResult struct {
	// Inserted is true when a new row was committed, false when the fence admitted an identical
	// row that already existed.
	Inserted bool
}

// leadConfigCheckPrefix and leadConfigColumnCheckSuffix name the run_lead_configs column CHECKs
// (run_lead_configs_<column>_check, PRD #3244 section 1).
const (
	leadConfigCheckPrefix       = "run_lead_configs_"
	leadConfigColumnCheckSuffix = "_check"
)

// RecordLeadConfig records one worker-reported lead-config row (PRD #3244 section 4).
// runOwnedByWorker separates "not this worker's run" (ErrRunNotOwned, 404) from the fenced write,
// which re-checks worker, generation, released claim, status and lane under the run row lock. The
// report body is never logged. A 23514 from the insert is mapped by constraint NAME to the field
// it concerns: the harness-dependent run_lead_configs_requested_model_check (a Codex row needs a
// model) is requested_model; any other named run_lead_configs constraint is its own column.
func (s *Service) RecordLeadConfig(ctx context.Context, wkr store.Worker, runID uuid.UUID, rep leadcfg.Report) (RecordResult, error) {
	if _, err := s.runOwnedByWorker(ctx, runID, wkr); err != nil {
		return RecordResult{}, err
	}
	res, err := s.q.RecordRunLeadConfig(ctx, store.RecordRunLeadConfigParams{
		RunID:                  runID,
		WorkerID:               wkr.ID,
		ClaimGeneration:        rep.ClaimGeneration,
		Phase:                  string(rep.Phase),
		PhaseEntry:             rep.PhaseEntry,
		Seq:                    rep.Seq,
		Truncated:              rep.Truncated,
		RequestedModel:         pgconv.TextPtr(rep.RequestedModel),
		RequestedModelSource:   string(rep.RequestedModelSource),
		RequestedEffort:        pgconv.TextPtr(rep.RequestedEffort),
		RequestedEffortMode:    string(rep.RequestedEffortMode),
		RequestedEffortSource:  string(rep.RequestedEffortSource),
		ObservedModel:          pgconv.TextPtr(rep.ObservedModel),
		ObservedModelEvidence:  pgconv.TextPtr(rep.ObservedModelEvidence),
		ObservedEffort:         pgconv.TextPtr(rep.ObservedEffort),
		ObservedEffortEvidence: pgconv.TextPtr(rep.ObservedEffortEvidence),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			if f := leadConfigCheckField(pgErr.ConstraintName); f != "" {
				return RecordResult{}, &LeadConfigFieldError{Field: f}
			}
		}
		return RecordResult{}, err
	}
	return RecordResult{Inserted: res.Inserted}, nil
}

// leadConfigCheckField maps a run_lead_configs CHECK constraint name to the request field it
// concerns, or "" for a constraint this table does not own (stays a 500).
func leadConfigCheckField(constraint string) string {
	switch constraint {
	case "run_lead_configs_requested_model_check":
		return "requested_model"
	case "run_lead_configs_effort_mode_check":
		return "requested_effort_mode"
	case "run_lead_configs_observed_model_pairing_check":
		return "observed_model_evidence"
	case "run_lead_configs_observed_effort_pairing_check":
		return "observed_effort_evidence"
	case "run_lead_configs_truncated_check":
		return "truncated"
	}
	if !strings.HasPrefix(constraint, leadConfigCheckPrefix) || !strings.HasSuffix(constraint, leadConfigColumnCheckSuffix) {
		return ""
	}
	col := strings.TrimSuffix(strings.TrimPrefix(constraint, leadConfigCheckPrefix), leadConfigColumnCheckSuffix)
	// requested_model_length_check and its siblings name the column plus a "_length" qualifier.
	col = strings.TrimSuffix(col, "_length")
	if col == "" {
		return ""
	}
	return col
}
