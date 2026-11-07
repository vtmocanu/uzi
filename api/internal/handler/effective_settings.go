package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// effective_settings.go composes the effective-settings view (PRD #3254): what model, effort and
// harness a run would use, built from the claim's own rules (workersvc.PredictHarness and
// PredictLead), the shared settings-cell builders and the credential-override validator. It reads
// only; nothing here writes, locks or calls a provider.

// startPreviewBodyLimit bounds the start-preview request body (strict decode).
const startPreviewBodyLimit = 1 << 10

// Wire vocabulary of the effective-settings view. Tokens only; labels live in
// fixtures/model-settings/contract.json.
const (
	harnessSourceRunOverride      = "run_override"
	harnessSourceUserDefault      = "user_default"
	harnessSourceAutomatic        = "automatic"
	harnessSourceChosenAtCreate   = "chosen_at_create"
	sourceCrossCheckPin           = "cross_check_pin"
	sourceCrossCheckWorkerDefault = "cross_check_worker_default"
	sourceCrossCheckDefault       = "cross_check_default"
	sourceBuiltin                 = "builtin"

	noteSDKDefault             = "sdk_default"
	noteNotConfigurable        = "not_configurable"
	noteNotSent                = "not_sent"
	noteDefaultHarnessUnusable = "user_default_harness_unusable"
	noteInstanceUnavailable    = "instance_settings_unavailable"

	refusalNoCredentialForHarness = "no_credential_for_harness"
	refusalNoUsableCredential     = "no_usable_credential"
	refusalCredentialDisabled     = "credential_disabled"
	refusalCredentialNotFound     = "credential_not_found"
	refusalOverrideUnsupported    = "credential_override_harness_unsupported"
	refusalOverrideInvalid        = "credential_override_invalid"
	detailCredentialDisabled      = "credential_disabled"
)

// refusal is a refusal token with its optional detail ("" = none).
type refusal struct {
	Token  string
	Detail string
}

func strPtr(s string) *string { return &s }

func (r refusal) ptrs() (token, detail *string) {
	if r.Token == "" {
		return nil, nil
	}
	token = strPtr(r.Token)
	if r.Detail != "" {
		detail = strPtr(r.Detail)
	}
	return token, detail
}

// startRefusal maps the sentinels creation answers to the closed refusal enum. The harness
// sentinels wrapping ErrNoCredentialForHarness are tested first. ok is false for any other error,
// which the caller treats as a failure.
func startRefusal(err error) (refusal, bool) {
	switch {
	case errors.Is(err, workersvc.ErrHarnessCredentialDisabled):
		return refusal{refusalNoCredentialForHarness, detailCredentialDisabled}, true
	case errors.Is(err, workersvc.ErrCodexPoolUnavailable):
		return refusal{refusalNoCredentialForHarness, codexPoolUnavailableCode}, true
	case errors.Is(err, workersvc.ErrNoCredentialForHarness):
		return refusal{Token: refusalNoCredentialForHarness}, true
	case errors.Is(err, workersvc.ErrNoUsableCredential):
		return refusal{Token: refusalNoUsableCredential}, true
	case errors.Is(err, workersvc.ErrCredentialDisabled):
		return refusal{Token: refusalCredentialDisabled}, true
	case errors.Is(err, workersvc.ErrCredentialOverrideSecretNotFound):
		return refusal{Token: refusalCredentialNotFound}, true
	case errors.Is(err, workersvc.ErrCredentialOverrideHarnessUnsupported):
		return refusal{Token: refusalOverrideUnsupported}, true
	case errors.Is(err, workersvc.ErrCredentialOverridePinnedNeedsSecret),
		errors.Is(err, workersvc.ErrCredentialOverrideInvalidMode):
		return refusal{Token: refusalOverrideInvalid}, true
	}
	return refusal{}, false
}

func credentialDTO(c *workersvc.CredentialPrediction) *apitypes.CredentialDTO {
	if c == nil {
		return nil
	}
	out := &apitypes.CredentialDTO{Kind: c.Kind, Source: c.Source, FixedAt: c.FixedAt}
	if c.AuthMode != "" {
		out.AuthMode = strPtr(c.AuthMode)
	}
	return out
}

// harnessValue maps an exact or chosen-at-create prediction to the wire. explicitSource is the
// harness_source of an explicit choice.
func harnessValue(p workersvc.HarnessPrediction, explicitSource string) *apitypes.HarnessValueDTO {
	notes := []string{}
	if p.ChosenAtCreate {
		return &apitypes.HarnessValueDTO{
			Source: harnessSourceChosenAtCreate, FixedAt: workersvc.FixedAtCreate, Notes: append(notes, p.Policy),
		}
	}
	source := harnessSourceAutomatic
	switch p.Rule {
	case workersvc.PredictRuleExplicit:
		source = explicitSource
	case workersvc.PredictRuleUserDefault:
		source = harnessSourceUserDefault
	}
	if p.DefaultSkipped {
		notes = append(notes, noteDefaultHarnessUnusable)
	}
	return &apitypes.HarnessValueDTO{Value: strPtr(string(p.Harness)), Source: source, FixedAt: workersvc.FixedAtCreate, Notes: notes}
}

// startOutcome is one start choice's prediction or refusal.
type startOutcome struct {
	pred    workersvc.HarnessPrediction
	refused refusal
}

// predictStart runs PredictHarness; a create refusal is an outcome, any other error a failure.
func (h *Handler) predictStart(ctx context.Context, userID uuid.UUID, explicit *workersvc.Harness, o workersvc.PredictOrigin) (startOutcome, error) {
	pred, err := h.wsvc.PredictHarness(ctx, userID, explicit, o)
	if err != nil {
		if r, ok := startRefusal(err); ok {
			return startOutcome{refused: r}, nil
		}
		return startOutcome{}, err
	}
	return startOutcome{pred: pred}, nil
}

func harnessPtr(h workersvc.Harness) *workersvc.Harness { return &h }

// GetEffectiveSettings serves GET /api/me/effective-settings.
func (h *Handler) GetEffectiveSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if h.wsvc == nil {
		slog.Error("effective settings: no worker service")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	dto, err := h.buildEffectiveSettings(r.Context(), user.ID)
	if err != nil {
		slog.Error("effective settings", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

// buildEffectiveSettings composes the whole GET body. Any store error fails the whole request so
// no partial body is written; an instance-settings read error does not (the built-in tier plus a
// note on the affected cells, as a claim would resolve).
func (h *Handler) buildEffectiveSettings(ctx context.Context, userID uuid.UUID) (apitypes.EffectiveSettingsDTO, error) {
	origin := workersvc.IssueStartOrigin()
	var starts [3]startOutcome
	for i, explicit := range []*workersvc.Harness{nil, harnessPtr(workersvc.HarnessClaude), harnessPtr(workersvc.HarnessCodex)} {
		out, err := h.predictStart(ctx, userID, explicit, origin)
		if err != nil {
			return apitypes.EffectiveSettingsDTO{}, fmt.Errorf("predict start: %w", err)
		}
		starts[i] = out
	}

	leads := map[workersvc.Harness]workersvc.LeadPrediction{}
	reviews := map[workersvc.Harness]workersvc.LeadPrediction{}
	for _, hn := range []workersvc.Harness{workersvc.HarnessClaude, workersvc.HarnessCodex} {
		lead, err := h.wsvc.PredictLead(ctx, userID, hn, nil, false, false)
		if err != nil {
			return apitypes.EffectiveSettingsDTO{}, fmt.Errorf("predict lead %s: %w", hn, err)
		}
		leads[hn] = lead
		review, err := h.wsvc.PredictLead(ctx, userID, hn, nil, false, true)
		if err != nil {
			return apitypes.EffectiveSettingsDTO{}, fmt.Errorf("predict review %s: %w", hn, err)
		}
		reviews[hn] = review
	}

	s, err := h.q.GetUserSettings(ctx, userID)
	if err != nil {
		return apitypes.EffectiveSettingsDTO{}, fmt.Errorf("get user settings: %w", err)
	}
	pins, _, err := h.crossCheckPinSettings(ctx, userID)
	if err != nil {
		return apitypes.EffectiveSettingsDTO{}, fmt.Errorf("get cross-check pins: %w", err)
	}
	checkModels, checkEfforts, err := crossCheckEffectiveCells(pins)
	if err != nil {
		return apitypes.EffectiveSettingsDTO{}, err
	}

	dto := apitypes.EffectiveSettingsDTO{Version: apitypes.EffectiveSettingsVersion}
	dto.Harnesses = []apitypes.EffectiveHarnessDTO{effectiveHarness(workersvc.HarnessClaude, starts[1]), effectiveHarness(workersvc.HarnessCodex, starts[2])}
	for i, choice := range []string{"default", "claude", "codex"} {
		dto.Start = append(dto.Start, effectiveStartChoice(choice, starts[i]))
	}

	// Worker, judge, summary and task review, each Claude then Codex.
	dto.Models = append(dto.Models, workerModelCell(leads[workersvc.HarnessClaude]), workerModelCell(leads[workersvc.HarnessCodex]))
	if h.settings != nil {
		instance, notes := h.effectiveInstanceModels(ctx)
		judgeLane := textPtrValue(s.JudgeModel.Valid, s.JudgeModel.String)
		codexJudgeLane := textPtrValue(s.CodexJudgeModel.Valid, s.CodexJudgeModel.String)
		summaryLane := textPtrValue(s.SummaryModel.Valid, s.SummaryModel.String)
		codexSummaryLane := textPtrValue(s.CodexSummaryModel.Valid, s.CodexSummaryModel.String)
		for _, c := range judgeModelCells(judgeLane, codexJudgeLane, instance) {
			dto.Models = append(dto.Models, claimedModelCell("judge_model", c, notes))
		}
		for _, c := range summaryModelCells(summaryLane, codexSummaryLane, instance) {
			dto.Models = append(dto.Models, claimedModelCell("summary_model", c, notes))
		}
	}
	dto.Models = append(dto.Models, reviewModelCell(reviews[workersvc.HarnessClaude]), reviewModelCell(reviews[workersvc.HarnessCodex]))

	claudeEffort := effortSettingCell("claude", leads[workersvc.HarnessClaude].UserEffort)
	codexEffort := effortSettingCell("codex", leads[workersvc.HarnessCodex].UserEffort)
	codexEffort.CatalogDefault = h.codexCatalogDefaultForModel(ctx, userID, leads[workersvc.HarnessCodex].Model)
	reviewCodexEffort := reviewEffortCell("codex")
	reviewCodexEffort.CatalogDefault = h.codexCatalogDefaultForModel(ctx, userID, reviews[workersvc.HarnessCodex].Model)
	dto.Efforts = []apitypes.EffectiveEffortCellDTO{
		{EffortSettingCellDTO: claudeEffort, Setting: "worker_effort", FixedAt: workersvc.FixedAtClaim, Notes: []string{}},
		{EffortSettingCellDTO: codexEffort, Setting: "worker_effort", FixedAt: workersvc.FixedAtClaim, Notes: []string{}},
		{EffortSettingCellDTO: reviewEffortCell("claude"), Setting: "task_review_effort", FixedAt: workersvc.FixedAtClaim, Notes: []string{noteNotSent}},
		{EffortSettingCellDTO: reviewCodexEffort, Setting: "task_review_effort", FixedAt: workersvc.FixedAtClaim, Notes: []string{noteNotSent}},
	}
	dto.CrossCheck = apitypes.EffectiveCrossCheckDTO{Models: checkModels, Efforts: checkEfforts}
	return dto, nil
}

// effectiveInstanceModels reads every instance model key once. On an error it follows the shared
// posture (the instance tier is unset, so the built-in) and returns the note the affected cells
// carry; a warm cache serves its last map and returns no error.
func (h *Handler) effectiveInstanceModels(ctx context.Context) (map[string]string, []string) {
	vals, err := h.settings.InstanceModels(ctx, []string{
		settings.KeyJudgeModel, settings.KeyCodexJudgeModel,
		settings.KeySummaryModel, settings.KeyCodexSummaryModel,
	})
	if err != nil {
		slog.Error("effective settings: read instance models", "error", err)
		return map[string]string{}, []string{noteInstanceUnavailable}
	}
	return vals, nil
}

func effectiveHarness(hn workersvc.Harness, o startOutcome) apitypes.EffectiveHarnessDTO {
	out := apitypes.EffectiveHarnessDTO{Harness: string(hn), Usable: o.refused.Token == ""}
	if !out.Usable {
		out.Refusal, out.RefusalDetail = o.refused.ptrs()
		return out
	}
	out.Credential = credentialDTO(o.pred.Credential)
	return out
}

// effectiveStartChoice maps one start choice. An Anthropic override is accepted by a choice that
// resolves to Claude, or by a chosen-at-create choice whose preferred harness is Claude (the
// automatic policy). An overflow with Codex preferred is not: any non-inherit override makes the
// creation origin ineligible, so creation stays on exact Codex and refuses the override. The GET
// itself decides only "no override".
func effectiveStartChoice(choice string, o startOutcome) apitypes.EffectiveStartChoiceDTO {
	out := apitypes.EffectiveStartChoiceDTO{Choice: choice}
	if o.refused.Token != "" {
		out.Refusal, out.RefusalDetail = o.refused.ptrs()
		return out
	}
	out.Harness = harnessValue(o.pred, harnessSourceRunOverride)
	out.AnthropicOverrideAllowed = o.pred.Harness == workersvc.HarnessClaude ||
		(o.pred.ChosenAtCreate && o.pred.Preferred == workersvc.HarnessClaude)
	return out
}

// modelNotes are the notes every model cell carries: the runtime decides when nothing resolves.
func modelNotes(resolved string, extra ...string) []string {
	notes := append([]string{}, extra...)
	if resolved == "" {
		notes = append(notes, noteSDKDefault)
	}
	return notes
}

// workerModelCell is the lead model cell of one harness: the stored lane, what Default means
// (the lead template, then the built-in) and what the claim and the worker resolve. It is read
// when a worker claims the run.
func workerModelCell(p workersvc.LeadPrediction) apitypes.EffectiveModelCellDTO {
	return apitypes.EffectiveModelCellDTO{
		ModelSettingCellDTO: apitypes.ModelSettingCellDTO{
			Harness: string(p.Harness), Model: p.LaneModel,
			DefaultModel: p.DefaultModel, DefaultSource: p.DefaultModelSource,
			ResolvedModel: p.Model, Source: p.ModelSource,
		},
		Setting: "worker_model", FixedAt: workersvc.FixedAtClaim, Notes: modelNotes(p.Model),
	}
}

// claimedModelCell wraps a shared settings cell (judge, summary) read at claim.
func claimedModelCell(setting string, c apitypes.ModelSettingCellDTO, notes []string) apitypes.EffectiveModelCellDTO {
	return apitypes.EffectiveModelCellDTO{
		ModelSettingCellDTO: c, Setting: setting, FixedAt: workersvc.FixedAtClaim,
		Notes: modelNotes(c.ResolvedModel, notes...),
	}
}

// reviewModelCell is the task-review model: a built-in the user cannot configure. The Claude
// review sends no model, so the SDK decides.
func reviewModelCell(p workersvc.LeadPrediction) apitypes.EffectiveModelCellDTO {
	cell := apitypes.EffectiveModelCellDTO{
		ModelSettingCellDTO: apitypes.ModelSettingCellDTO{
			Harness: string(p.Harness), DefaultModel: p.DefaultModel, DefaultSource: p.DefaultModelSource,
			ResolvedModel: p.Model, Source: p.ModelSource,
		},
		Setting: "task_review_model", FixedAt: workersvc.FixedAtClaim,
	}
	if p.Harness == workersvc.HarnessCodex {
		cell.Notes = []string{noteNotConfigurable}
	} else {
		cell.Notes = modelNotes(p.Model)
	}
	return cell
}

// reviewEffortCell is the task-review effort: neither review pass sends one.
func reviewEffortCell(harness string) apitypes.EffortSettingCellDTO {
	return apitypes.EffortSettingCellDTO{
		Harness: harness, DefaultSource: sourceBuiltin, Source: sourceBuiltin, Mode: agenttmpl.EffortModeRuntimeDefault,
	}
}

// crossCheckEffectiveCells maps the cross-check pin cells onto the model and effort cell shapes,
// plan before code and Claude before Codex. An unknown provenance token is an error, never a
// guessed label.
func crossCheckEffectiveCells(pins []crossCheckPinDTO) ([]apitypes.EffectiveModelCellDTO, []apitypes.EffectiveEffortCellDTO, error) {
	byKey := map[string]crossCheckPinDTO{}
	for _, p := range pins {
		byKey[p.Stage+"/"+p.Harness] = p
	}
	var models []apitypes.EffectiveModelCellDTO
	var efforts []apitypes.EffectiveEffortCellDTO
	for _, stage := range []string{"plan", "code"} {
		for _, hn := range []string{"claude", "codex"} {
			p, ok := byKey[stage+"/"+hn]
			if !ok {
				return nil, nil, fmt.Errorf("cross-check cell %s/%s is missing", stage, hn)
			}
			modelSource, err := crossCheckModelSource(p.ModelSource)
			if err != nil {
				return nil, nil, err
			}
			effortSource, err := crossCheckEffortSource(p.EffortSource)
			if err != nil {
				return nil, nil, err
			}
			resolved := ""
			if p.ResolvedModel != nil {
				resolved = *p.ResolvedModel
			}
			def := ""
			if p.WorkerDefaultModel != nil {
				def = *p.WorkerDefaultModel
			}
			setting := "cross_check_" + stage
			models = append(models, apitypes.EffectiveModelCellDTO{
				ModelSettingCellDTO: apitypes.ModelSettingCellDTO{
					Harness: hn, Model: p.Model, DefaultModel: def, DefaultSource: sourceCrossCheckWorkerDefault,
					ResolvedModel: resolved, Source: modelSource,
				},
				Setting: setting, FixedAt: workersvc.FixedAtClaim, Notes: modelNotes(resolved),
			})
			level := p.ResolvedEffort
			efforts = append(efforts, apitypes.EffectiveEffortCellDTO{
				EffortSettingCellDTO: apitypes.EffortSettingCellDTO{
					Harness: hn, Effort: p.Effort, DefaultEffort: agenttmpl.CrossCheckDefaultEffort,
					DefaultSource: sourceCrossCheckDefault, ResolvedEffort: &level, Source: effortSource,
					Mode: agenttmpl.EffortModeExplicit,
				},
				Setting: setting, FixedAt: workersvc.FixedAtClaim, Notes: []string{},
			})
		}
	}
	return models, efforts, nil
}

func crossCheckModelSource(s string) (string, error) {
	switch s {
	case agenttmpl.CrossCheckPin:
		return sourceCrossCheckPin, nil
	case agenttmpl.CrossCheckWorkerDefault:
		return sourceCrossCheckWorkerDefault, nil
	}
	return "", fmt.Errorf("unknown cross-check model source %q", s)
}

func crossCheckEffortSource(s string) (string, error) {
	switch s {
	case agenttmpl.CrossCheckPin:
		return sourceCrossCheckPin, nil
	case agenttmpl.CrossCheckDefaultSource:
		return sourceCrossCheckDefault, nil
	}
	return "", fmt.Errorf("unknown cross-check effort source %q", s)
}

// previewOverride is a validated preview credential-override request.
type previewOverride struct {
	Mode     string
	SecretID *uuid.UUID
}

// parsePreviewHarness parses the optional harness enum of a preview request.
func parsePreviewHarness(raw *string) (*workersvc.Harness, bool) {
	if raw == nil {
		return nil, true
	}
	switch *raw {
	case string(workersvc.HarnessClaude):
		return harnessPtr(workersvc.HarnessClaude), true
	case string(workersvc.HarnessCodex):
		return harnessPtr(workersvc.HarnessCodex), true
	}
	return nil, false
}

// parsePreviewOverride validates the credential-override request shape: mode in the closed set and
// a well-formed uuid. A pinned mode without a secret id is a valid request that the validator
// refuses, as creation answers.
func parsePreviewOverride(in *apitypes.PreviewCredentialOverrideRequest) (*previewOverride, bool) {
	if in == nil {
		return nil, true
	}
	switch in.Mode {
	case workersvc.CredentialOverrideModeInherit, workersvc.CredentialOverrideModeAuto,
		workersvc.CredentialOverrideModeDefault, workersvc.CredentialOverrideModePinned:
	default:
		return nil, false
	}
	out := &previewOverride{Mode: in.Mode}
	if in.SecretID != nil {
		id, err := uuid.Parse(strings.TrimSpace(*in.SecretID))
		if err != nil {
			return nil, false
		}
		out.SecretID = &id
	}
	return out, true
}

// PreviewStartSettings serves POST /api/me/effective-settings/start-preview: the harness and
// credential a manual issue start with this harness and token choice would get, validated by the
// same code creation runs, without creating anything.
func (h *Handler) PreviewStartSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req apitypes.StartPreviewRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, startPreviewBodyLimit); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	explicit, ok := parsePreviewHarness(req.Harness)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "harness must be one of claude, codex")
		return
	}
	override, ok := parsePreviewOverride(req.CredentialOverride)
	if !ok {
		httpx.Error(w, http.StatusBadRequest, "credential_override must have a mode of inherit, auto, default or pinned and a valid secret_id")
		return
	}
	if h.wsvc == nil {
		slog.Error("start preview: no worker service")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	dto, err := h.buildStartPreview(r.Context(), user.ID, explicit, override)
	if err != nil {
		slog.Error("start preview", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

// buildStartPreview predicts the harness with the override in the creation context (any non-inherit
// override makes the creation-time policies ineligible), then validates the override against the
// resulting harness, or, when the harness is chosen at creation, against each alternative.
func (h *Handler) buildStartPreview(ctx context.Context, userID uuid.UUID, explicit *workersvc.Harness, ov *previewOverride) (apitypes.StartPreviewDTO, error) {
	dto := apitypes.StartPreviewDTO{Version: apitypes.EffectiveSettingsVersion}
	origin := workersvc.IssueStartOrigin()
	origin.AnthropicOverride = ov != nil && ov.Mode != workersvc.CredentialOverrideModeInherit
	out, err := h.predictStart(ctx, userID, explicit, origin)
	if err != nil {
		return dto, err
	}
	if out.refused.Token != "" {
		dto.Refusal, dto.RefusalDetail = out.refused.ptrs()
		return dto, nil
	}
	pred := out.pred
	dto.Harness = harnessValue(pred, harnessSourceRunOverride)
	if !pred.ChosenAtCreate {
		cred, ref, err := h.previewCredential(ctx, userID, pred.Harness, pred.Credential, ov)
		if err != nil {
			return dto, err
		}
		if ref.Token != "" {
			dto.Refusal, dto.RefusalDetail = ref.ptrs()
			return dto, nil
		}
		dto.Credential = cred
		return dto, nil
	}

	// Chosen at creation: each harness as if it were chosen. An inherit override is normalised
	// away on the Codex alternative exactly where creation normalises it: under the automatic
	// policy, and under overflow when Claude is preferred (clearsInheritOverride). With Codex
	// preferred, overflow moves to Claude, so the Codex alternative is the stay-on-Codex
	// outcome, which creation refuses.
	for _, hn := range []workersvc.Harness{workersvc.HarnessClaude, workersvc.HarnessCodex} {
		alt := apitypes.StartPreviewAlternativeDTO{Harness: string(hn)}
		branch, err := h.predictStart(ctx, userID, harnessPtr(hn), origin)
		if err != nil {
			return dto, err
		}
		if branch.refused.Token != "" {
			alt.Refusal, alt.RefusalDetail = branch.refused.ptrs()
			dto.Alternatives = append(dto.Alternatives, alt)
			continue
		}
		effective := ov
		if ov != nil && ov.Mode == workersvc.CredentialOverrideModeInherit && hn == workersvc.HarnessCodex &&
			(pred.Policy == workersvc.PredictPolicyAutomaticWeeklyReset ||
				(pred.Policy == workersvc.PredictPolicyOverflow && pred.Preferred == workersvc.HarnessClaude)) {
			effective = nil
		}
		cred, ref, err := h.previewCredential(ctx, userID, hn, branch.pred.Credential, effective)
		if err != nil {
			return dto, err
		}
		alt.Credential = cred
		alt.Refusal, alt.RefusalDetail = ref.ptrs()
		dto.Alternatives = append(dto.Alternatives, alt)
	}
	if ref := commonAlternativeRefusal(dto.Alternatives); ref.Token != "" {
		dto.Refusal, dto.RefusalDetail = ref.ptrs()
	}
	return dto, nil
}

// commonAlternativeRefusal is the top-level refusal of a chosen-at-create start-preview response
// (see commonRefusal).
func commonAlternativeRefusal(alts []apitypes.StartPreviewAlternativeDTO) refusal {
	rs := make([]refusal, len(alts))
	for i, a := range alts {
		if a.Refusal != nil {
			rs[i] = refusal{Token: *a.Refusal}
			if a.RefusalDetail != nil {
				rs[i].Detail = *a.RefusalDetail
			}
		}
	}
	return commonRefusal(rs)
}

// commonRefusal is the top-level refusal of a chosen-at-create response, given each branch's
// refusal (a zero refusal is a valid branch): set only when every branch is refused for the same
// reason, token and detail. With differing reasons, or any valid branch, no harness is decided and
// nothing is promoted, so the per-branch reasons stand alone.
func commonRefusal(rs []refusal) refusal {
	var common refusal
	for i, r := range rs {
		if r.Token == "" || (i > 0 && r != common) {
			return refusal{}
		}
		common = r
	}
	return common
}

// previewCredential validates the override the way creation does (the one validator, for an issue
// run on the resolved harness) and returns the credential the run would use: the base credential
// when no override applies, otherwise the one the override selects. A validator refusal is a
// refusal; any other error fails the request.
func (h *Handler) previewCredential(ctx context.Context, userID uuid.UUID, hn workersvc.Harness, base *workersvc.CredentialPrediction, ov *previewOverride) (*apitypes.CredentialDTO, refusal, error) {
	if ov == nil {
		return credentialDTO(base), refusal{}, nil
	}
	resolved, err := h.wsvc.ResolveCredentialOverride(ctx, userID, runkind.Issue, string(hn), ov.Mode, ov.SecretID)
	if err != nil {
		if r, ok := startRefusal(err); ok {
			return nil, r, nil
		}
		return nil, refusal{}, err
	}
	if resolved == nil {
		return credentialDTO(base), refusal{}, nil
	}
	c, err := overrideCredential(resolved.Mode)
	if err != nil {
		return nil, refusal{}, err
	}
	return credentialDTO(c), refusal{}, nil
}

// overrideCredential is the Anthropic credential a validated override mode selects: auto ranks the
// tokens at claim, default uses the owner default at claim, and a pinned token is fixed when the
// run is created.
func overrideCredential(mode string) (*workersvc.CredentialPrediction, error) {
	c := workersvc.CredentialPrediction{Kind: workersvc.CredentialKindAnthropic, FixedAt: workersvc.FixedAtClaim}
	switch mode {
	case workersvc.CredentialOverrideModeAuto:
		c.Source = workersvc.CredentialSourceAutoSelection
	case workersvc.CredentialOverrideModeDefault:
		c.Source = workersvc.CredentialSourceOwnerDefault
	case workersvc.CredentialOverrideModePinned:
		c.Source, c.FixedAt = workersvc.CredentialSourceTokenOverride, workersvc.FixedAtCreate
	default:
		return nil, fmt.Errorf("credential override mode %q has no credential source", mode)
	}
	return &c, nil
}
