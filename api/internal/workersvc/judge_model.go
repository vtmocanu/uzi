package workersvc

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// judgeModelReader is the one per-user read resolveJudgeModelFor needs.
type judgeModelReader interface {
	GetUserJudgeModels(context.Context, uuid.UUID) (store.GetUserJudgeModelsRow, error)
}

// resolveJudgeModelFor resolves the model a judge of the given harness runs (PRD #3245): the run
// owner's lane value > the instance value for that harness > the built-in, via
// agenttmpl.ResolveJudgeModel. A stored value invalid for its lane is reported in dropped (the
// first entry is the one the claim notes) and resolution continues.
//
// strict=false is the claim-time posture for a vendor judge: a read error on either tier is
// logged and that tier treated as unset, so the result always carries a concrete model and err is
// always nil. strict=true is the provider judge's: a read error is returned, so a create validating
// the model rolls back (and a claim fails terminally) rather than check a guessed model against the
// provider's map.
func (s *Service) resolveJudgeModelFor(ctx context.Context, q judgeModelReader, userID uuid.UUID, harness Harness, strict bool) (model string, dropped []string, err error) {
	instanceKey := settings.KeyJudgeModel
	if harness == HarnessCodex {
		instanceKey = settings.KeyCodexJudgeModel
	}
	var userModel string
	if um, uerr := q.GetUserJudgeModels(ctx, userID); uerr != nil {
		if strict {
			return "", nil, uerr
		}
		slog.Warn("judge claim: read user judge models", "user", userID.String(), "error", uerr)
	} else if harness == HarnessCodex {
		userModel = um.CodexJudgeModel.String
	} else {
		userModel = um.JudgeModel.String
	}
	var instanceModel string
	if s.settings != nil {
		m, ierr := s.settings.InstanceModel(ctx, instanceKey)
		if ierr != nil {
			if strict {
				return "", nil, ierr
			}
			slog.Warn("judge claim: read instance judge model", "key", instanceKey, "error", ierr)
			m = ""
		}
		instanceModel = m
	}
	res := agenttmpl.ResolveJudgeModel(string(harness), userModel, instanceModel)
	return res.Model, res.Dropped, nil
}

// checkProviderJudgeModel is the judge arm of verifyRunBinding: a provider-bound judge runs ONE
// model (no lead, planning or template models), resolved strictly, and it must map through the
// frozen provider's model map. An unset judge model resolves to the built-in "opus" alias.
func (s *Service) checkProviderJudgeModel(ctx context.Context, q *store.Queries, run store.Run, src providerSource) error {
	model, _, err := s.resolveJudgeModelFor(ctx, q, run.UserID, Harness(run.Harness), true)
	if err != nil {
		return err
	}
	if !modelMapped(src.ModelMap, model) {
		return &ProviderModelUnmappedError{Model: model, Source: "judge"}
	}
	return nil
}
