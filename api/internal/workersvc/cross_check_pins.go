package workersvc

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

var errCrossCheckPinsCapabilityMissing = errors.New("worker lacks cross_check_pins_v1")
var errCheckerPinUnavailable = errors.New("plan cross-check: checker unavailable")

type crossCheckPinReader interface {
	GetCrossCheckPinSnapshot(context.Context, store.GetCrossCheckPinSnapshotParams) (store.GetCrossCheckPinSnapshotRow, error)
}

// The single statement reads the check's actual stage/family and both pins and
// defaults. The returned value is carried through delivery and record, never
// recomputed after credential minting.
func (s *Service) preflightCrossCheckPins(ctx context.Context, worker store.Worker, run store.Run) (*agenttmpl.CrossCheckResolution, error) {
	q, ok := s.q.(crossCheckPinReader)
	if !ok {
		return nil, ErrCrossCheckRefused
	}
	row, err := q.GetCrossCheckPinSnapshot(ctx, store.GetCrossCheckPinSnapshotParams{
		ChildID: run.ID, UserID: worker.UserID, WorkerID: pgconv.UUID(worker.ID), ClaimGeneration: run.ClaimGeneration})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrCrossCheckRefused
	}
	if err != nil {
		return nil, err
	}
	if row.Stage != "plan" || row.CheckerHarness.String != "codex" || run.Harness != row.CheckerHarness.String || run.UserID != worker.UserID {
		return nil, ErrCrossCheckRefused
	}
	for _, field := range []struct {
		name     string
		value    *string
		validate func(string, string) (string, error)
	}{{"model", textPtr(row.Model), agenttmpl.ValidateFamilyModel},
		{"effort", textPtr(row.Effort), agenttmpl.ValidateFamilyEffort}} {
		if field.value == nil {
			continue
		}
		normalized, validationErr := field.validate(row.CheckerHarness.String, *field.value)
		// A stored non-null pin must be a concrete canonical value.
		if validationErr != nil || normalized == "" || normalized != *field.value {
			return nil, fmt.Errorf("%w: %s/%s %s", errCheckerPinUnavailable, row.Stage, row.CheckerHarness.String, field.name)
		}
	}
	if (row.Model.Valid || row.Effort.Valid) && !slices.Contains(worker.ProtocolCapabilities, capability.CrossCheckPinsV1) {
		return nil, errCrossCheckPinsCapabilityMissing
	}
	resolved := agenttmpl.ResolveCrossCheck(row.CheckerHarness.String, textPtr(row.Model), textPtr(row.Effort),
		textPtr(row.WorkerModel), textPtr(row.WorkerEffort), nil)
	if resolved.Model != nil && !agenttmpl.CuratedCodexModels[*resolved.Model] &&
		!slices.Contains(worker.ProtocolCapabilities, capability.CodexCustomModelV1) {
		return nil, errCustomModelCapabilityMissing
	}
	return &resolved, nil
}
