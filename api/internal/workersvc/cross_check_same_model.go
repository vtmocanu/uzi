package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Same-model guard (PRD #4574 M2). A cross-check that involves a provider-bound side must not pair a
// lead and a checker that resolve to the same model. SameModel and Unresolved both decide the check
// failed/checker_unavailable at the actual claim (through errCheckerPinUnavailable, like
// errCheckerRunEnvUnavailable); the detail is server-side only.
var (
	errCheckerSameModel       = fmt.Errorf("%w: same_model", errCheckerPinUnavailable)
	errCheckerModelUnresolved = fmt.Errorf("%w: model_unresolved", errCheckerPinUnavailable)
)

// sameModelReader is the two reads the guard may make. *store.Queries satisfies it; a test supplies a
// recorder, which is how the vendor/vendor no-read property is observable.
type sameModelReader interface {
	GetRunLeadConfigForGeneration(ctx context.Context, arg store.GetRunLeadConfigForGenerationParams) (store.RunLeadConfig, error)
	GetModelProviderRevision(ctx context.Context, arg store.GetModelProviderRevisionParams) (store.GetModelProviderRevisionRow, error)
}

// leadConfigPhase is the lead-config phase the lead reported for a check stage.
func leadConfigPhase(stage string) string {
	if stage == "code" {
		return "implement"
	}
	return "plan"
}

// checkSameModel returns nil when the check may proceed, else an error wrapping
// errCheckerPinUnavailable. lead is the locked lead run row; child is the checker run. A pair where
// neither side is provider-bound returns before any read.
func checkSameModel(ctx context.Context, rd sameModelReader, lead, child store.Run, persisted store.CrossCheck, payload *ClaimPayload, resolution *agenttmpl.CrossCheckResolution) error {
	if !providerBoundRun(lead) && !providerBoundRun(child) {
		return nil // NotEvaluated: no run_lead_configs or revision read
	}
	cfg, err := rd.GetRunLeadConfigForGeneration(ctx, store.GetRunLeadConfigForGenerationParams{
		RunID: lead.ID, ClaimGeneration: persisted.LeadClaimGeneration, Phase: leadConfigPhase(persisted.Stage)})
	if errors.Is(err, pgx.ErrNoRows) {
		return errCheckerModelUnresolved
	}
	if err != nil {
		return err
	}
	leadSide, err := leadIdentitySide(ctx, rd, lead, cfg)
	if err != nil {
		return err
	}
	explicit := resolution != nil && resolution.ModelSource == agenttmpl.CrossCheckPin
	switch providers.CompareCheckerIdentity(leadSide, checkerIdentitySide(child, payload, explicit)) {
	case providers.SameModel:
		return errCheckerSameModel
	case providers.Unresolved:
		return errCheckerModelUnresolved
	}
	return nil
}

// leadIdentitySide resolves the lead's model. A provider-bound lead resolves its REQUESTED model
// through its frozen revision's map (the observed model is not part of its identity); a vendor lead
// uses the observed id, else the requested one. A missing frozen revision is Unresolved, not an error.
func leadIdentitySide(ctx context.Context, rd sameModelReader, lead store.Run, cfg store.RunLeadConfig) (providers.IdentitySide, error) {
	if !providerBoundRun(lead) {
		model := strings.TrimSpace(cfg.ObservedModel.String)
		if !cfg.ObservedModel.Valid || model == "" {
			model = strings.TrimSpace(cfg.RequestedModel.String)
		}
		return providers.IdentitySide{ModelID: model, Resolved: model != ""}, nil
	}
	side := providers.IdentitySide{ProviderBound: true}
	rev, err := rd.GetModelProviderRevision(ctx, store.GetModelProviderRevisionParams{
		ProviderID: lead.ModelProviderID.Bytes, Revision: lead.ModelProviderRevision.Int32})
	if errors.Is(err, pgx.ErrNoRows) {
		return side, nil // Unresolved: the frozen revision is gone (provider deleted, revisions cascade)
	}
	if err != nil {
		return side, err
	}
	var models providers.ModelMap
	if json.Unmarshal(rev.ModelMap, &models) != nil {
		return side, nil
	}
	host, err := providers.NormalizedHost(rev.BaseUrl)
	if err != nil {
		return side, nil
	}
	side.Host = host
	side.ModelID, side.Resolved = providers.ResolveModelID(lead.Harness, models, cfg.RequestedModel.String, false)
	return side, nil
}

// checkerIdentitySide resolves the checker's model from its claim-time resolution: payload.Config's
// default model through the frozen map of payload.Secrets.Provider, explicit iff pinned.
func checkerIdentitySide(child store.Run, payload *ClaimPayload, explicit bool) providers.IdentitySide {
	requested := ""
	if payload.Config.DefaultModel != nil {
		requested = *payload.Config.DefaultModel
	}
	if !providerBoundRun(child) {
		model := strings.TrimSpace(requested)
		return providers.IdentitySide{ModelID: model, Resolved: model != ""}
	}
	side := providers.IdentitySide{ProviderBound: true}
	b := payload.Secrets.Provider
	if b == nil {
		return side
	}
	host, err := providers.NormalizedHost(b.BaseURL)
	if err != nil {
		return side
	}
	side.Host = host
	side.ModelID, side.Resolved = providers.ResolveModelID(child.Harness, b.ModelMap, requested, explicit)
	return side
}
