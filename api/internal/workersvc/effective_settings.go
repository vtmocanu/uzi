package workersvc

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/agenttmpl"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// effective_settings.go holds the read-only prediction primitives behind the effective-settings
// routes (PRD #3254 section 2). workersvc cannot import schedsvc or handler, so it exposes only
// what the claim and create paths already decide; the handler composes them. Nothing here writes,
// takes a lock, reads limit evidence or runs chooseAutomaticHarness.

// Harness prediction rules: which D11 step decided an exact harness. The handler maps them onto
// the wire harness_source (an explicit harness is run_override or, for a schedule, schedule_pin).
const (
	PredictRuleExplicit    = "explicit"
	PredictRuleUserDefault = "user_default"
	PredictRuleAutomatic   = "automatic"
)

// Creation-time harness policies a prediction can name. They are the wire note codes.
const (
	PredictPolicyAutomaticWeeklyReset = "automatic_weekly_reset"
	PredictPolicyOverflow             = "overflow"
)

// Credential prediction vocabulary (the wire credential kind, source and fixed_at tokens).
const (
	CredentialKindAnthropic = "anthropic"
	CredentialKindCodex     = "codex"

	CredentialSourceClaimSelection = "claim_selection"
	CredentialSourceCodexDefault   = "codex_default_credential"
	CredentialSourceChosenAtCreate = "chosen_at_create"
	CredentialSourceAutoSelection  = "auto_selection"
	CredentialSourceOwnerDefault   = "owner_default"
	CredentialSourceTokenOverride  = "token_override"

	FixedAtCreate = "create"
	FixedAtClaim  = "claim"
)

const (
	maxPredictedPinnedSubagents = 32
	predictLeadSourceBuiltin    = agenttmpl.WorkerDefaultSourceBuiltin
	predictReviewEffortSource   = "builtin"
	predictIssueKind            = "issue"
)

// PredictOrigin is what the creation-time harness policies read about the request: the run kind,
// the trigger source, a frozen run model and whether a non-inherit Anthropic override is
// requested. It mirrors harnessCreationOrigin without the explicit harness, which is the
// separate PredictHarness argument.
type PredictOrigin struct {
	Kind              string
	TriggerSource     string
	Model             *string
	AnthropicOverride bool
}

// IssueStartOrigin is the origin of a manual issue start with no model and no override: what the
// board, the issue page and the chat card send.
func IssueStartOrigin() PredictOrigin {
	return PredictOrigin{Kind: predictIssueKind, TriggerSource: "manual"}
}

// CredentialPrediction is the credential a run on the predicted harness would use, as wire
// tokens. It never carries an id, a label or a value.
type CredentialPrediction struct {
	Kind     string
	Source   string
	FixedAt  string
	AuthMode string // codex only
}

// HarnessPrediction is the read-only answer to "which harness would this creation use".
type HarnessPrediction struct {
	// Harness is the exact harness; empty exactly when ChosenAtCreate.
	Harness Harness
	// ChosenAtCreate is true when a creation-time policy (Policy) can choose the harness at
	// creation, so no harness is promised.
	ChosenAtCreate bool
	// Policy is PredictPolicyAutomaticWeeklyReset or PredictPolicyOverflow with ChosenAtCreate,
	// else empty.
	Policy string
	// Preferred is the pure D11 result the creation decision starts from (the harness an overflow
	// would move away from).
	Preferred Harness
	// Rule is the PredictRule* that decided Preferred.
	Rule string
	// DefaultSkipped is true when a stored default harness was unusable and skipped.
	DefaultSkipped bool
	// Credential is the credential of the exact harness; nil with ChosenAtCreate.
	Credential *CredentialPrediction
}

// predictStore is the read surface PredictHarness needs beyond the D11 resolver's.
type predictStore interface {
	harnessResolverStore
	GetHarnessCreationPolicy(ctx context.Context, id uuid.UUID) (store.GetHarnessCreationPolicyRow, error)
	GetCodexDefaultSelectionCandidate(ctx context.Context, arg store.GetCodexDefaultSelectionCandidateParams) (store.GetCodexDefaultSelectionCandidateRow, error)
}

// PredictHarness predicts the harness a creation would freeze, without writing anything. It runs
// resolveRunHarnessQ unchanged (so a refusal is the create sentinel) and then reads the
// creation-time policies with the same predicates createRunAtomic uses:
//
//   - the automatic policy (automaticHarnessEligible) applies: the harness is chosen at create;
//   - an unset default with no automatic policy is the pure D11 answer (no overflow);
//   - the opt-in overflow policy applies for an eligible origin when the other harness is usable
//     and, with Claude preferred, the Codex default is structurally available (the structural
//     exclusion evaluateCreationOverflow applies): the harness is chosen at create;
//   - otherwise the exact D11 harness.
//
// explicit is the harness the caller asks for outright; an explicit harness is always exact.
func (s *Service) PredictHarness(ctx context.Context, userID uuid.UUID, explicit *Harness, o PredictOrigin) (HarnessPrediction, error) {
	q, ok := s.q.(predictStore)
	if !ok {
		return HarnessPrediction{}, errHarnessStoreUnavailable
	}
	res, err := s.resolveRunHarnessQ(ctx, userID, explicit, q)
	if err != nil {
		return HarnessPrediction{}, err
	}
	origin := harnessCreationOrigin{
		Kind: o.Kind, TriggerSource: o.TriggerSource, Explicit: explicit, Model: o.Model,
		AnthropicOverride: o.AnthropicOverride,
	}
	out := HarnessPrediction{
		Harness: res.Harness, Preferred: res.Harness, Rule: predictRule(res.Rule),
		DefaultSkipped: res.DefaultSkipped && explicit == nil,
	}

	// Only an issue creation consults the policies; any other kind is never eligible, so skip the read.
	if origin.Kind == predictIssueKind {
		policy, perr := q.GetHarnessCreationPolicy(ctx, userID)
		if perr != nil {
			return HarnessPrediction{}, fmt.Errorf("harness predict: creation policy: %w", perr)
		}
		switch {
		case automaticHarnessEligible(res, explicit, origin, policy):
			out.Harness, out.ChosenAtCreate, out.Policy = "", true, PredictPolicyAutomaticWeeklyReset
		case res.DefaultUnset || !policy.HarnessOverflowEnabled || !origin.eligible(policy):
			// The pure D11 answer stands: no policy can move the harness.
		default:
			applies, aerr := s.overflowApplies(ctx, q, userID, res)
			if aerr != nil {
				return HarnessPrediction{}, aerr
			}
			if applies {
				out.Harness, out.ChosenAtCreate, out.Policy = "", true, PredictPolicyOverflow
			}
		}
	}
	if !out.ChosenAtCreate {
		out.Credential = predictCredential(res)
	}
	return out, nil
}

func predictRule(r harnessRule) string {
	switch r {
	case harnessRuleExplicit:
		return PredictRuleExplicit
	case harnessRuleUserDefault:
		return PredictRuleUserDefault
	default:
		return PredictRuleAutomatic
	}
}

// overflowApplies reports whether evaluateCreationOverflow would go on to compare limit headroom
// for res: the other harness must be usable (else the decision is excluded), and a Claude-preferred
// creation excludes overflow when the default Codex subscription is structurally unavailable (the
// Claude run must not fail on an unavailable destination). It reads no limit evidence.
func (s *Service) overflowApplies(ctx context.Context, q predictStore, userID uuid.UUID, res resolvedHarness) (bool, error) {
	otherHarness := HarnessClaude
	if res.Harness == HarnessClaude {
		otherHarness = HarnessCodex
	}
	other, err := s.resolveRunHarnessQ(ctx, userID, &otherHarness, q)
	if err != nil {
		if errors.Is(err, errNoCredentialForHarness) || errors.Is(err, errNoUsableCredential) || errors.Is(err, errCodexPoolUnavailable) {
			return false, nil
		}
		return false, err
	}
	codex := res
	if res.Harness != HarnessCodex {
		codex = other
	}
	if codex.Codex != nil && codex.Codex.Select != codexChoicePool && codex.Codex.AuthMode == codexAuthModeSubscription {
		row, rerr := q.GetCodexDefaultSelectionCandidate(ctx, store.GetCodexDefaultSelectionCandidateParams{UserID: userID, AliasID: codex.Codex.SecretID})
		if rerr != nil {
			return false, fmt.Errorf("harness predict: default codex candidate: %w", rerr)
		}
		if !codexPoolRowStructural(store.ListCodexPoolAliasesRow(row)) && res.Harness == HarnessClaude {
			return false, nil
		}
	}
	return true, nil
}

// predictCredential is the no-override credential of an exact resolution. A Claude run's token is
// chosen when a worker claims it; a Codex run binds its credential at creation (the default
// credential, or, in account mode auto with a configured pool, an account picked by headroom).
func predictCredential(res resolvedHarness) *CredentialPrediction {
	if res.Harness != HarnessCodex || res.Codex == nil {
		return &CredentialPrediction{Kind: CredentialKindAnthropic, Source: CredentialSourceClaimSelection, FixedAt: FixedAtClaim}
	}
	source := CredentialSourceCodexDefault
	if res.Codex.Select == codexChoicePool {
		source = CredentialSourceChosenAtCreate
	}
	return &CredentialPrediction{Kind: CredentialKindCodex, Source: source, FixedAt: FixedAtCreate, AuthMode: res.Codex.AuthMode}
}

// SubagentPin is one own subagent whose template pins a model for the harness.
type SubagentPin struct {
	Name  string
	Model string
}

// Subagent prediction modes.
const (
	SubagentModeFollowLead = "follow_lead"
	SubagentModeOwnPins    = "own_pins"
)

// SubagentPrediction is how the owner's allocated subagents pick a model.
type SubagentPrediction struct {
	Mode string
	// LeadValue is the lead's model ("" when the SDK decides).
	LeadValue string
	// Pinned lists, for own_pins, every allocated subagent with a pin for the harness, sorted by
	// name and capped; the rest follow the lead (or the SDK default when LeadValue is empty).
	Pinned          []SubagentPin
	PinnedTruncated bool
}

// LeadPrediction is the lead worker model and effort a run on Harness would use, with where each
// value comes from. For a task review it is the reviewer's built-in.
type LeadPrediction struct {
	Harness Harness
	Review  bool
	// LaneModel and UserEffort are the owner's stored values (nil = unset).
	LaneModel  *string
	UserEffort *string
	// ClaimModel is the default_model the claim ships (nil: none, the worker decides) with its
	// default_model_source.
	ClaimModel       *string
	ClaimModelSource string
	// Model is the model the lead runs, "" when the SDK decides; ModelSource is user_default,
	// schedule_pin, template or builtin.
	Model       string
	ModelSource string
	// DefaultModel and DefaultModelSource are the walk without the owner's lane and any frozen model.
	DefaultModel       string
	DefaultModelSource string
	// DroppedRunModel is a frozen model the harness cannot run, ignored in favour of the lane.
	DroppedRunModel *string
	// Effort is the claim's default_effort (nil: nothing is sent); EffortSource its provenance.
	Effort       *string
	EffortSource string
	// Subagents is nil for a review.
	Subagents *SubagentPrediction
}

// leadPredictStore is the read surface PredictLead needs beyond the Store interface.
type leadPredictStore interface {
	ListUserCrossCheckPins(ctx context.Context, userID uuid.UUID) ([]store.ListUserCrossCheckPinsRow, error)
	ListClaimAgentTemplates(ctx context.Context, userID pgtype.UUID) ([]store.AgentTemplate, error)
}

// PredictLead predicts the lead model and effort of a run on harness, and its subagents. A
// task-review run never reaches the lead path, so the review branch is decided first: Codex runs
// the built-in review model, Claude sends no model, and neither sends an effort. Otherwise the
// claim's own rule (resolveLeadSettings, over the lanes and efforts ListUserCrossCheckPins reads
// with the lead template's pins) decides, and an empty claim model falls to the worker default
// (agenttmpl.ResolveWorkerDefaultModel). frozenModel is a schedule's frozen run model.
func (s *Service) PredictLead(ctx context.Context, userID uuid.UUID, harness Harness, frozenModel *string, overrideSubagent bool, review bool) (LeadPrediction, error) {
	if review {
		model := agenttmpl.ClaudeTaskReviewModel
		if harness == HarnessCodex {
			model = agenttmpl.CodexTaskReviewModel
		}
		return LeadPrediction{
			Harness: harness, Review: true, Model: model, ModelSource: predictLeadSourceBuiltin,
			DefaultModel: model, DefaultModelSource: predictLeadSourceBuiltin, EffortSource: predictReviewEffortSource,
		}, nil
	}
	q, ok := s.q.(leadPredictStore)
	if !ok {
		return LeadPrediction{}, errHarnessStoreUnavailable
	}
	rows, err := q.ListUserCrossCheckPins(ctx, userID)
	if err != nil {
		return LeadPrediction{}, fmt.Errorf("lead predict: read lanes: %w", err)
	}
	var row store.ListUserCrossCheckPinsRow
	if len(rows) > 0 {
		row = rows[0] // lanes, efforts and the lead template's pins are identical on every row
	}
	lane, effort := row.DefaultClaudeModel, row.DefaultEffort
	if harness == HarnessCodex {
		lane, effort = row.DefaultCodexModel, row.DefaultCodexEffort
	}
	tClaude, tCodex := textPtr(row.TemplateModel), textPtr(row.TemplateCodexModel)

	settings := resolveLeadSettings(LeadSettingsInput{
		Harness: harness, RunModel: pgconv.TextOrNull(derefString(frozenModel)), LaneModel: lane, UserEffort: effort,
	})
	out := LeadPrediction{
		Harness: harness, LaneModel: textPtr(lane), UserEffort: textPtr(effort),
		Effort: settings.Effort, EffortSource: string(settings.EffortSource),
		DroppedRunModel: textPtr(settings.DroppedRunModel),
	}
	out.DefaultModel, out.DefaultModelSource = agenttmpl.ResolveWorkerDefaultModel(string(harness), nil, tClaude, tCodex)
	if settings.Model.Valid {
		m := settings.Model.String
		out.ClaimModel, out.ClaimModelSource = &m, string(settings.ModelSource)
		out.Model, out.ModelSource = m, string(settings.ModelSource)
	} else {
		out.Model, out.ModelSource = out.DefaultModel, out.DefaultModelSource
	}

	subs, err := s.predictSubagents(ctx, q, userID, harness, out.Model, overrideSubagent)
	if err != nil {
		return LeadPrediction{}, err
	}
	out.Subagents = subs
	return out, nil
}

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// predictSubagents follows the executors: Codex clears every subagent pin when the override is on;
// Claude applies it only when the lead model is non-empty. Otherwise each own subagent keeps the
// pin its template carries for the harness (agenttmpl.PinForHarness, as agentsFromTemplates).
func (s *Service) predictSubagents(ctx context.Context, q leadPredictStore, userID uuid.UUID, harness Harness, lead string, overrideSubagent bool) (*SubagentPrediction, error) {
	if overrideSubagent && (harness == HarnessCodex || lead != "") {
		return &SubagentPrediction{Mode: SubagentModeFollowLead, LeadValue: lead}, nil
	}
	templates, err := q.ListClaimAgentTemplates(ctx, pgconv.UUID(userID))
	if err != nil {
		return nil, fmt.Errorf("lead predict: list claim agent templates: %w", err)
	}
	names := make([]string, len(templates))
	for i, t := range templates {
		names[i] = t.Name
	}
	leadIdx, hasLead := agenttmpl.SelectLead(names)
	pins := make([]SubagentPin, 0, len(templates))
	for i, t := range templates {
		if hasLead && i == leadIdx {
			continue
		}
		if m := agenttmpl.PinForHarness(string(harness), derefString(textPtr(t.Model)), derefString(textPtr(t.CodexModel))); m != "" {
			pins = append(pins, SubagentPin{Name: t.Name, Model: m})
		}
	}
	sort.Slice(pins, func(i, j int) bool { return pins[i].Name < pins[j].Name })
	out := &SubagentPrediction{Mode: SubagentModeOwnPins, LeadValue: lead}
	if len(pins) > maxPredictedPinnedSubagents {
		pins, out.PinnedTruncated = pins[:maxPredictedPinnedSubagents], true
	}
	out.Pinned = pins
	return out, nil
}
