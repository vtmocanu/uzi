package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/httpx"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/schedsvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// effective_settings_schedule.go is the schedule half of the effective-settings view (PRD #3254):
// what firing a schedule now, and claiming the run, would use. It applies the rules in the order a
// schedule save and then a fire would: the save step (model syntax, a supplied credential override
// against the schedule's lane and effective harness), then the fire step (the harness through
// PredictHarness, the stored override's fire-time checks, the lead and subagent prediction). It
// reads only.

// schedulePreviewBodyLimit bounds the schedule-preview request body (strict decode).
const schedulePreviewBodyLimit = 2 << 10

const (
	harnessSourceSchedulePin = "schedule_pin"
	sourceSchedulePin        = "schedule_pin"

	noteSchedulePinIgnored   = "schedule_pin_ignored"
	noteIssueLabelsMayChoose = "issue_labels_may_select_harness"
	noteTokenFixed           = "token_fixed"

	refusalInvalidModel          = "invalid_model"
	refusalLaneNotSwitchable     = "credential_override_lane_not_switchable"
	refusalCodexOverrideConflict = "codex_override_conflict"
)

// scheduleDraft is a schedule as a fire would see it: the target, the harness pin and model pin,
// the subagent toggle and the credential override a fire would thread (nil = inherit).
type scheduleDraft struct {
	userID           uuid.UUID
	target           string
	pin              *workersvc.Harness
	model            *string
	overrideSubagent bool
	override         *workersvc.CredentialOverride
	// once is set for a saved one-time schedule. The request carries no timing, so a draft without
	// schedule_id is never once.
	once bool
}

// draftOutcome is the result of the request-to-draft step: a draft, a save-step refusal, or the
// HTTP status of a bad request or a missing schedule.
type draftOutcome struct {
	draft   scheduleDraft
	refused refusal
	status  int
	msg     string
}

// PreviewScheduleSettings serves POST /api/me/effective-settings/schedule-preview.
func (h *Handler) PreviewScheduleSettings(w http.ResponseWriter, r *http.Request) {
	user, ok := mw.UserFromContext(r.Context())
	if !ok {
		httpx.Error(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req apitypes.ScheduleSettingsPreviewRequest
	if err := httpx.DecodeJSONStrictBounded(r, &req, schedulePreviewBodyLimit); err != nil {
		httpx.Error(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if h.wsvc == nil || h.q == nil {
		slog.Error("schedule preview: no worker service")
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	out, err := h.scheduleDraftFromRequest(r.Context(), user.ID, req)
	if err != nil {
		slog.Error("schedule preview", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	if out.status != 0 {
		httpx.Error(w, out.status, out.msg)
		return
	}
	dto := apitypes.ScheduleSettingsPreviewDTO{Version: apitypes.EffectiveSettingsVersion, Notes: scheduleTargetNotes(out.draft.target)}
	if out.refused.Token != "" {
		dto.Refusal, dto.RefusalDetail = out.refused.ptrs()
		httpx.JSON(w, http.StatusOK, dto)
		return
	}
	dto, err = h.buildScheduleSettingsPreview(r.Context(), out.draft)
	if err != nil {
		slog.Error("schedule preview", "error", err)
		httpx.Error(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpx.JSON(w, http.StatusOK, dto)
}

func validScheduleTarget(t string) bool {
	switch t {
	case "issue", "sweep", "prompt", "self_improve", "ci_watch":
		return true
	}
	return false
}

// storedScheduleOverride is the override a fire threads from the stored columns, as the scheduler
// reads them. A pinned mode whose token was deleted keeps the mode with no id (the FK nulled it).
func storedScheduleOverride(s store.RunSchedule) *workersvc.CredentialOverride {
	if !s.CredentialOverrideMode.Valid || s.CredentialOverrideMode.String == "" {
		return nil
	}
	o := &workersvc.CredentialOverride{Mode: s.CredentialOverrideMode.String}
	if s.CredentialOverrideSecretID.Valid {
		id := uuid.UUID(s.CredentialOverrideSecretID.Bytes)
		o.SecretID = &id
	}
	return o
}

// scheduleSaveRefusal maps the override validator's sentinels to the save-step refusal enum.
func scheduleSaveRefusal(err error) (refusal, bool) {
	switch {
	case errors.Is(err, workersvc.ErrCredentialOverrideSecretNotFound):
		return refusal{Token: refusalCredentialNotFound}, true
	case errors.Is(err, workersvc.ErrCredentialOverrideLaneNotSwitchable):
		return refusal{Token: refusalLaneNotSwitchable}, true
	case errors.Is(err, workersvc.ErrCredentialOverrideHarnessUnsupported):
		return refusal{Token: refusalOverrideUnsupported}, true
	case errors.Is(err, workersvc.ErrCredentialOverridePinnedNeedsSecret), errors.Is(err, workersvc.ErrCredentialOverrideInvalidMode):
		return refusal{Token: refusalOverrideInvalid}, true
	case errors.Is(err, workersvc.ErrCredentialDisabled):
		return refusal{Token: refusalCredentialDisabled}, true
	}
	return refusal{}, false
}

func badDraft(status int, msg string) draftOutcome { return draftOutcome{status: status, msg: msg} }

// scheduleDraftFromRequest loads the stored schedule (owner-scoped; a missing or foreign id is a
// 404), overlays the supplied fields and runs the save step for what the request supplies. Supplied
// fields replace stored ones; an absent field is as stored. A store error is returned as an error.
func (h *Handler) scheduleDraftFromRequest(ctx context.Context, userID uuid.UUID, req apitypes.ScheduleSettingsPreviewRequest) (draftOutcome, error) {
	var sched *store.RunSchedule
	if req.ScheduleID != nil {
		id, err := uuid.Parse(strings.TrimSpace(*req.ScheduleID))
		if err != nil {
			return badDraft(http.StatusBadRequest, "schedule_id must be a valid uuid"), nil
		}
		s, err := h.q.GetRunScheduleForUser(ctx, store.GetRunScheduleForUserParams{ID: id, UserID: userID})
		if errors.Is(err, pgx.ErrNoRows) {
			return badDraft(http.StatusNotFound, "schedule not found"), nil
		}
		if err != nil {
			return draftOutcome{}, fmt.Errorf("get schedule: %w", err)
		}
		sched = &s
	}

	d := scheduleDraft{userID: userID, once: sched != nil && sched.Timing == "once"}
	switch {
	case req.Target != nil:
		d.target = strings.TrimSpace(*req.Target)
		if !validScheduleTarget(d.target) {
			return badDraft(http.StatusBadRequest, "target must be one of: issue, sweep, prompt, self_improve, ci_watch"), nil
		}
	case sched != nil:
		d.target = sched.Target
	default:
		return badDraft(http.StatusBadRequest, "target is required without schedule_id"), nil
	}

	var harnessCol pgtype.Text
	switch {
	case req.Harness.Present:
		if req.Harness.Value != nil {
			v := strings.TrimSpace(*req.Harness.Value)
			if workersvc.Harness(v) != workersvc.HarnessClaude && workersvc.Harness(v) != workersvc.HarnessCodex {
				return badDraft(http.StatusBadRequest, "harness must be one of: claude, codex"), nil
			}
			harnessCol = pgtype.Text{String: v, Valid: true}
		}
	case sched != nil:
		harnessCol = sched.Harness
	}
	if harnessCol.Valid && harnessCol.String != "" {
		d.pin = harnessPtr(workersvc.Harness(harnessCol.String))
	}

	// Save step: model syntax, as the schedule save validates it. A blank model is inherit.
	switch {
	case req.Model.Present:
		if req.Model.Value != nil {
			normalized, err := agenttmpl.ValidateModel(*req.Model.Value)
			if err != nil {
				return draftOutcome{draft: d, refused: refusal{Token: refusalInvalidModel}}, nil
			}
			if normalized != "" {
				d.model = &normalized
			}
		}
	case sched != nil && sched.Model.Valid && sched.Model.String != "":
		m := sched.Model.String
		d.model = &m
	}

	switch {
	case req.OverrideSubagentModel != nil:
		d.overrideSubagent = *req.OverrideSubagentModel
	case sched != nil:
		d.overrideSubagent = sched.OverrideSubagentModel
	}

	// Save step: a supplied override goes through the one validator against the schedule's lane and
	// effective harness, an explicit null as inherit. Omitted means the stored one, which a fire
	// threads as stored.
	switch {
	case req.CredentialOverride.Present:
		parsed, ok := parsePreviewOverride(req.CredentialOverride.Value)
		if !ok {
			return badDraft(http.StatusBadRequest, "credential_override must have a mode of inherit, auto, default or pinned and a valid secret_id"), nil
		}
		mode, secretID := workersvc.CredentialOverrideModeInherit, (*uuid.UUID)(nil)
		if parsed != nil {
			mode, secretID = parsed.Mode, parsed.SecretID
		}
		effective, err := h.scheduleEffectiveHarness(ctx, h.q, store.RunSchedule{UserID: userID, Harness: harnessCol})
		if err != nil {
			return draftOutcome{}, fmt.Errorf("schedule effective harness: %w", err)
		}
		resolved, err := h.wsvc.ResolveCredentialOverride(ctx, userID, scheduleLaneKind(d.target), effective, mode, secretID)
		if err != nil {
			if ref, ok := scheduleSaveRefusal(err); ok {
				return draftOutcome{draft: d, refused: ref}, nil
			}
			return draftOutcome{}, fmt.Errorf("validate credential override: %w", err)
		}
		d.override = resolved
	case sched != nil:
		d.override = storedScheduleOverride(*sched)
	}
	if d.target == "self_improve" {
		// A self_improve run follows its own ladder and never carries one. A supplied override
		// (even inherit or null) was refused above as credential_override_lane_not_switchable, so
		// only the stored one reaches here and is dropped.
		d.override = nil
	}
	return draftOutcome{draft: d}, nil
}

// scheduleTargetNotes are the notes the target alone decides.
func scheduleTargetNotes(target string) []string {
	notes := []string{}
	if target == "issue" || target == "sweep" {
		notes = append(notes, noteIssueLabelsMayChoose)
	}
	return notes
}

// fireOrigin is the creation origin a fire of the draft presents: issue and sweep fire issue runs
// (the creation-time policies read the trigger, the frozen model and a threaded override), every
// other target creates a run kind no policy applies to.
func (d scheduleDraft) fireOrigin() workersvc.PredictOrigin {
	o := workersvc.PredictOrigin{Kind: runkind.Issue, TriggerSource: "schedule", Model: d.model, AnthropicOverride: d.override != nil}
	switch d.target {
	case "prompt":
		o.Kind = runkind.Prompt
	case "self_improve":
		o.Kind = runkind.SelfImprove
	case "ci_watch":
		o.Kind = runkind.CIFix
	}
	return o
}

// scheduleFireRefusal maps a fire-time error to the closed schedule refusal enum through the
// scheduler's own skip-reason map, plus the hard no_credential_for_harness an unusable explicit
// harness gets. ok is false for anything else, which the caller treats as a failure.
func scheduleFireRefusal(err error) (refusal, bool) {
	if reason, ok := schedsvc.SkipReasonForErr(err); ok {
		switch reason {
		case schedsvc.SkipCredentialDisabled, schedsvc.SkipNoUsableCredential,
			schedsvc.SkipCodexOverrideConflict, schedsvc.SkipCodexPoolUnavailable:
			return refusal{Token: string(reason)}, true
		}
		return refusal{}, false
	}
	if errors.Is(err, workersvc.ErrNoCredentialForHarness) {
		return refusal{Token: refusalNoCredentialForHarness}, true
	}
	return refusal{}, false
}

// predictFire runs PredictHarness for a fire; a fire refusal is an outcome, any other error a failure.
func (h *Handler) predictFire(ctx context.Context, d scheduleDraft, explicit *workersvc.Harness) (workersvc.HarnessPrediction, refusal, error) {
	pred, err := h.wsvc.PredictHarness(ctx, d.userID, explicit, d.fireOrigin())
	if err != nil {
		if r, ok := scheduleFireRefusal(err); ok {
			return workersvc.HarnessPrediction{}, r, nil
		}
		return workersvc.HarnessPrediction{}, refusal{}, err
	}
	return pred, refusal{}, nil
}

// scheduleOverrideRefusal applies the fire-time rules for the override a fire threads, once the
// harness is hn. An unpinned schedule that meets Codex fails closed first (the scheduler's
// codexOverrideConflict pre-check, which every target runs, a CI watch included); then a stored
// pinned token disabled since refuses; then any Anthropic override on a Codex run is refused by the
// create seam. All three are the schedule skip reasons. A one-time schedule runs the disabled-token
// check before the fire, so for it the disabled token comes first. A deleted pinned token has no
// id left, so it inherits on Claude; on Codex the override still conflicts.
func (h *Handler) scheduleOverrideRefusal(ctx context.Context, d scheduleDraft, hn workersvc.Harness) (refusal, error) {
	if d.override == nil {
		return refusal{}, nil
	}
	conflict := refusal{Token: refusalCodexOverrideConflict}
	onCodex := hn == workersvc.HarnessCodex
	if onCodex && d.pin == nil && !d.once {
		return conflict, nil
	}
	if err := h.wsvc.CheckStoredCredentialOverrideEnabled(ctx, d.userID, d.override); err != nil {
		if r, ok := scheduleFireRefusal(err); ok {
			return r, nil
		}
		return refusal{}, err
	}
	if onCodex {
		return conflict, nil
	}
	return refusal{}, nil
}

// onceDisabledHold is the scheduler's pre-fire check for a one-time schedule (process calls
// ScheduleCredentialDisabled before firing): a disabled stored pin, or an unusable pinned harness,
// holds the row. Any other schedule, or an error the tick does not hold on, is no hold.
func (h *Handler) onceDisabledHold(ctx context.Context, d scheduleDraft) (refusal, error) {
	if !d.once {
		return refusal{}, nil
	}
	if err := h.wsvc.ScheduleCredentialDisabled(ctx, d.userID, d.override, d.pin); err != nil {
		if r, ok := scheduleFireRefusal(err); ok {
			return r, nil
		}
	}
	return refusal{}, nil
}

// scheduleBranch is what a fire would use on one harness, or that harness's fire refusal.
type scheduleBranch struct {
	harness    workersvc.Harness
	model      *apitypes.EffectiveModelCellDTO
	effort     *apitypes.EffectiveEffortCellDTO
	credential *apitypes.CredentialDTO
	subagents  *apitypes.EffectiveSubagentsDTO
	refused    refusal
}

// scheduleBranch predicts the harness-dependent fields for a fire that lands on hn. base is the
// no-override credential of that harness.
func (h *Handler) scheduleBranch(ctx context.Context, d scheduleDraft, hn workersvc.Harness, base *workersvc.CredentialPrediction) (scheduleBranch, error) {
	b := scheduleBranch{harness: hn}
	ref, err := h.scheduleOverrideRefusal(ctx, d, hn)
	if err != nil {
		return b, err
	}
	if ref.Token != "" {
		b.refused = ref
		return b, nil
	}
	lead, err := h.wsvc.PredictLead(ctx, d.userID, hn, d.model, d.overrideSubagent, false)
	if err != nil {
		return b, fmt.Errorf("predict lead %s: %w", hn, err)
	}

	model := workerModelCell(lead)
	if lead.DroppedRunModel != nil {
		model.Ignored = &apitypes.EffectiveIgnoredDTO{Value: *lead.DroppedRunModel, Source: sourceSchedulePin}
		model.Notes = modelNotes(lead.Model, noteSchedulePinIgnored)
	}
	if lead.ModelSource == sourceSchedulePin {
		// An honoured pin is frozen into runs.model when the schedule fires; a default chosen after
		// an ignored pin is still read at the claim.
		model.FixedAt = workersvc.FixedAtCreate
	}
	b.model = &model

	effort := effortSettingCell(string(hn), lead.UserEffort)
	if hn == workersvc.HarnessCodex {
		effort.CatalogDefault = h.codexCatalogDefaultForModel(ctx, d.userID, lead.Model)
	}
	b.effort = &apitypes.EffectiveEffortCellDTO{EffortSettingCellDTO: effort, Setting: "worker_effort", FixedAt: workersvc.FixedAtClaim, Notes: []string{}}

	cred := base
	switch {
	case d.target == "self_improve" && hn == workersvc.HarnessClaude:
		// A self_improve claim follows the owner's judge binding, not a worker's.
		if cred, err = h.wsvc.PredictSelfImproveCredential(ctx, d.userID); err != nil {
			return b, err
		}
	case d.override != nil && hn == workersvc.HarnessClaude && !(d.override.Mode == workersvc.CredentialOverrideModePinned && d.override.SecretID == nil):
		if cred, err = overrideCredential(d.override.Mode); err != nil {
			return b, err
		}
	}
	b.credential = credentialDTO(cred)
	b.subagents = subagentsDTO(lead.Subagents)
	return b, nil
}

func subagentsDTO(p *workersvc.SubagentPrediction) *apitypes.EffectiveSubagentsDTO {
	if p == nil {
		return nil
	}
	out := &apitypes.EffectiveSubagentsDTO{Mode: p.Mode, LeadValue: p.LeadValue, Pinned: []apitypes.EffectiveSubagentPinDTO{}, PinnedTruncated: p.PinnedTruncated,
		Notes: []string{"repo_agents_not_predicted"}}
	for _, pin := range p.Pinned {
		out.Pinned = append(out.Pinned, apitypes.EffectiveSubagentPinDTO{Name: pin.Name, Model: pin.Model})
	}
	return out
}

// scheduleAlternatives maps the branches of a chosen-at-create fire onto the response, Claude first,
// and the top-level refusal: set only when every branch is refused for the same reason.
func scheduleAlternatives(branches []scheduleBranch) ([]apitypes.ScheduleAlternativeDTO, refusal) {
	alts := make([]apitypes.ScheduleAlternativeDTO, 0, len(branches))
	refusals := make([]refusal, 0, len(branches))
	for _, b := range branches {
		alt := apitypes.ScheduleAlternativeDTO{Harness: string(b.harness)}
		if b.refused.Token != "" {
			alt.Refusal, alt.RefusalDetail = b.refused.ptrs()
		} else {
			alt.WorkerModel, alt.WorkerEffort, alt.Credential, alt.Subagents = b.model, b.effort, b.credential, b.subagents
		}
		alts = append(alts, alt)
		refusals = append(refusals, b.refused)
	}
	return alts, commonRefusal(refusals)
}

// buildScheduleSettingsPreview is the fire step. A failure before the harness is decided is the
// top-level refusal with no harness; a refusal on a known harness keeps the harness and nulls the
// harness-dependent fields; a chosen-at-create harness answers per branch.
func (h *Handler) buildScheduleSettingsPreview(ctx context.Context, d scheduleDraft) (apitypes.ScheduleSettingsPreviewDTO, error) {
	dto := apitypes.ScheduleSettingsPreviewDTO{Version: apitypes.EffectiveSettingsVersion, Notes: scheduleTargetNotes(d.target)}
	// A one-time schedule's tick runs the disabled-credential hold before it fires, so it outranks
	// any harness refusal. The harness is still reported when it resolves.
	hold, err := h.onceDisabledHold(ctx, d)
	if err != nil {
		return dto, err
	}
	pred, ref, err := h.predictFire(ctx, d, d.pin)
	if err != nil {
		return dto, err
	}
	if hold.Token != "" {
		if ref.Token == "" {
			dto.Harness = harnessValue(pred, harnessSourceSchedulePin)
		}
		dto.Refusal, dto.RefusalDetail = hold.ptrs()
		return dto, nil
	}
	if ref.Token != "" {
		dto.Refusal, dto.RefusalDetail = ref.ptrs()
		return dto, nil
	}
	dto.Harness = harnessValue(pred, harnessSourceSchedulePin)

	if !pred.ChosenAtCreate {
		b, err := h.scheduleBranch(ctx, d, pred.Harness, pred.Credential)
		if err != nil {
			return dto, err
		}
		if b.refused.Token != "" {
			dto.Refusal, dto.RefusalDetail = b.refused.ptrs()
			return dto, nil
		}
		dto.WorkerModel, dto.WorkerEffort, dto.Credential, dto.Subagents = b.model, b.effort, b.credential, b.subagents
		// The judge binding fixes the token; an auto binding picks one at the claim instead.
		if d.target == "self_improve" && pred.Harness == workersvc.HarnessClaude && dto.Credential != nil && dto.Credential.Source != workersvc.CredentialSourceAutoSelection {
			dto.Notes = append(dto.Notes, noteTokenFixed)
		}
		return dto, nil
	}

	var branches []scheduleBranch
	for _, hn := range []workersvc.Harness{workersvc.HarnessClaude, workersvc.HarnessCodex} {
		branch, ref, err := h.predictFire(ctx, d, harnessPtr(hn))
		if err != nil {
			return dto, err
		}
		if ref.Token != "" {
			branches = append(branches, scheduleBranch{harness: hn, refused: ref})
			continue
		}
		b, err := h.scheduleBranch(ctx, d, hn, branch.Credential)
		if err != nil {
			return dto, err
		}
		branches = append(branches, b)
	}
	var common refusal
	dto.Alternatives, common = scheduleAlternatives(branches)
	if common.Token != "" {
		dto.Refusal, dto.RefusalDetail = common.ptrs()
	}
	return dto, nil
}
