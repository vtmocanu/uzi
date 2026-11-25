package workersvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// provider_source.go is PRD #4320 M4 (module 4, "Selection"): the owner's engine provider source,
// its usability rules, the typed refusals, and the run-creation helpers (freeze, inherited
// re-check, model-map check) createRunAtomic composes. The api is the destination authority: the
// allowlist is read from Params, never from the worker.

// Typed refusals. Every one is a sentinel (errors.Is) so the handler maps it to its stable reason.
var (
	// ErrProviderSourceUnusable: the owner's engine provider source is set but cannot serve a run
	// (retired, URL no longer allowlisted, key missing or disabled). Never falls back to a vendor
	// credential or to the other engine. 409 provider_source_unusable.
	ErrProviderSourceUnusable = errors.New("provider_source_unusable")
	// ErrProviderLaneUnsupported: job, cross-check and isolated (egress-profile) runs do not
	// support a provider-sourced owner until PRD #4322, and neither does a Codex judge for an owner
	// with a Codex source (until #4574); a Claude judge is provider-bound (PRD #4322 M1a).
	// 409 provider_lane_unsupported.
	ErrProviderLaneUnsupported = errors.New("provider_lane_unsupported")
	// ErrProviderModelUnmapped: a model the run would use is neither a mapped alias nor a mapped
	// target of the bound provider's model map. 409 provider_model_unmapped.
	ErrProviderModelUnmapped = errors.New("provider_model_unmapped")
	// ErrProviderBindingStale: a copied (inherited) binding's key was rotated, disabled or deleted
	// since the source run froze it. 409 provider_binding_stale.
	ErrProviderBindingStale = errors.New("provider_binding_stale")
	// ErrProviderDestinationNotAllowed: a copied binding's base URL is no longer on the operator
	// allowlist. 409 provider_destination_not_allowed.
	ErrProviderDestinationNotAllowed = errors.New("provider_destination_not_allowed")
	// errProviderCreateRequiresTx: a provider-sourced creation needs the atomic transaction (the
	// freeze locks and the insert must share one). Fails closed rather than inserting non-atomically.
	errProviderCreateRequiresTx = errors.New("provider-sourced run creation requires an atomic transaction (no tx beginner wired)")
	// errProviderBindingMismatch is an internal invariant failure: the inserted row's binding does
	// not equal the resolution's. It exists so an insert closure that forgets to pass the five
	// binding params (sqlc.narg: unstamped = NULL = a silent vendor run) fails closed.
	errProviderBindingMismatch = errors.New("provider binding: inserted run does not carry the resolved binding")
)

// Reasons naming why a set source is unusable (the detail of ProviderSourceUnusableError).
const (
	SourceUnusableProviderMissing = "provider_missing"
	SourceUnusableRetired         = "retired"
	SourceUnusableBaseURL         = "base_url_not_allowed"
	SourceUnusableKeyMissing      = "key_missing"
	SourceUnusableKeyDisabled     = "key_disabled"
	SourceUnusableInvalid         = "provider_invalid"
)

// ProviderSourceUnusableError carries the nonsecret reason. errors.Is(err, ErrProviderSourceUnusable).
type ProviderSourceUnusableError struct {
	ProviderID uuid.UUID
	Reason     string
}

func (e *ProviderSourceUnusableError) Error() string {
	return fmt.Sprintf("%s: %s", ErrProviderSourceUnusable.Error(), e.Reason)
}

func (e *ProviderSourceUnusableError) Is(target error) bool {
	return target == ErrProviderSourceUnusable
}

// ProviderModelUnmappedError names the offending model. errors.Is(err, ErrProviderModelUnmapped).
type ProviderModelUnmappedError struct {
	Model string
	// Source says where the model came from ("run", "user default", "template <name>").
	Source string
}

func (e *ProviderModelUnmappedError) Error() string {
	return fmt.Sprintf("%s: model %q (%s) is not mapped by the provider", ErrProviderModelUnmapped.Error(), e.Model, e.Source)
}

func (e *ProviderModelUnmappedError) Is(target error) bool { return target == ErrProviderModelUnmapped }

// providerSource is a usable engine provider source: everything a run freezes.
type providerSource struct {
	Engine      string
	ProviderID  uuid.UUID
	Revision    int32
	BaseURL     string // the matched allowlist entry, the form a run is bound to
	AuthStyle   string
	ModelMap    providers.ModelMap
	KeySecretID uuid.UUID
	KeyRev      int64
}

// ProviderSourceState is the three-way answer of the source resolution.
type ProviderSourceState int

const (
	// ProviderSourceNone: the requested engine has no selected provider; the whole vendor ladder applies.
	ProviderSourceNone ProviderSourceState = iota
	// ProviderSourceUsable: set and usable.
	ProviderSourceUsable
	// ProviderSourceUnusable: set but unusable; Reason says why.
	ProviderSourceUnusable
)

// ProviderSourceResult is the owner's engine provider source: none | usable | unusable(reason).
type ProviderSourceResult struct {
	State      ProviderSourceState
	ProviderID uuid.UUID
	Reason     string
	// Source is populated when State is ProviderSourceUsable.
	Source providerSource
}

// Set reports whether the owner selected a provider (usable or not).
func (r ProviderSourceResult) Set() bool { return r.State != ProviderSourceNone }

// Err is the typed refusal for an unusable result, nil otherwise.
func (r ProviderSourceResult) Err() error {
	if r.State != ProviderSourceUnusable {
		return nil
	}
	return &ProviderSourceUnusableError{ProviderID: r.ProviderID, Reason: r.Reason}
}

// ProviderAllowFunc maps a base URL to the allowlist entry it equals exactly (config.ProviderBaseURLAllowed).
type ProviderAllowFunc func(candidate string) (entry string, ok bool)

func providerKeyKind(engine string) string {
	if engine == providers.EngineCodex {
		return store.KindOpenAICompatibleKey
	}
	return store.KindAnthropicCompatibleKey
}

type engineProviderSourceReader interface {
	GetUserProviderSource(context.Context, store.GetUserProviderSourceParams) (store.GetUserProviderSourceRow, error)
}

func ResolveProviderSource(ctx context.Context, q engineProviderSourceReader, userID uuid.UUID, engine string, allow ProviderAllowFunc) (ProviderSourceResult, error) {
	if engine != providers.EngineClaude && engine != providers.EngineCodex {
		return ProviderSourceResult{}, fmt.Errorf("unsupported provider engine %q", engine)
	}
	row, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: userID, Engine: engine})
	if err != nil {
		return ProviderSourceResult{}, fmt.Errorf("%s provider source: %w", engine, err)
	}
	return EvaluateProviderSource(engine, row, allow), nil
}

func EvaluateProviderSource(engine string, row store.GetUserProviderSourceRow, allow ProviderAllowFunc) ProviderSourceResult {
	if engine != providers.EngineClaude && engine != providers.EngineCodex {
		return ProviderSourceResult{State: ProviderSourceUnusable, Reason: SourceUnusableInvalid}
	}
	if !row.ProviderID.Valid {
		return ProviderSourceResult{State: ProviderSourceNone}
	}
	pid := uuid.UUID(row.ProviderID.Bytes)
	unusable := func(reason string) ProviderSourceResult {
		return ProviderSourceResult{State: ProviderSourceUnusable, ProviderID: pid, Reason: reason}
	}
	if !row.CurrentRevision.Valid {
		return unusable(SourceUnusableProviderMissing)
	}
	if row.RetiredAt.Valid {
		return unusable(SourceUnusableRetired)
	}
	if !row.Engine.Valid || row.Engine.String != engine || !row.BaseUrl.Valid || !row.AuthStyle.Valid {
		return unusable(SourceUnusableInvalid)
	}
	entry, ok := "", false
	if allow != nil {
		entry, ok = allow(row.BaseUrl.String)
	}
	if !ok {
		return unusable(SourceUnusableBaseURL)
	}
	if !row.KeySecretID.Valid || !row.KeyKind.Valid || row.KeyKind.String != providerKeyKind(engine) ||
		!row.KeyProviderID.Valid || uuid.UUID(row.KeyProviderID.Bytes) != pid || !row.KeyEnablementRev.Valid {
		return unusable(SourceUnusableKeyMissing)
	}
	if row.KeyDisabledAt.Valid {
		return unusable(SourceUnusableKeyDisabled)
	}
	if providers.ValidateAuthStyle(row.AuthStyle.String) != nil {
		return unusable(SourceUnusableInvalid)
	}
	var mm providers.ModelMap
	if err := json.Unmarshal(row.ModelMap, &mm); err != nil || !row.Protocol.Valid || providers.ValidateProfile(engine, row.Protocol.String, row.AuthStyle.String, mm) != nil {
		return unusable(SourceUnusableInvalid)
	}
	return ProviderSourceResult{State: ProviderSourceUsable, ProviderID: pid, Source: providerSource{
		Engine:      engine,
		ProviderID:  pid,
		Revision:    row.CurrentRevision.Int32,
		BaseURL:     entry,
		AuthStyle:   row.AuthStyle.String,
		ModelMap:    mm,
		KeySecretID: uuid.UUID(row.KeySecretID.Bytes),
		KeyRev:      row.KeyEnablementRev.Int64,
	}}
}

// providerAllow is the service's allowlist matcher (Params mirrors config's operator settings).
func (s *Service) providerAllow(candidate string) (string, bool) {
	return providers.MatchAllowed(s.p.ProviderAllowedBaseURLs, candidate, s.p.ProviderAllowLoopback)
}

// bindingParams are the five binding columns an insert closure passes; the zero value is a vendor
// run (all NULL).
type bindingParams struct {
	ProviderID pgtype.UUID
	Revision   pgtype.Int4
	KeyMode    pgtype.Text
	KeySecret  pgtype.UUID
	KeyRev     pgtype.Int8
}

const providerKeyModeUser = "user_key"

// bindingOf is the binding a resolution freezes: all NULL without a provider source.
func bindingOf(res resolvedHarness) bindingParams {
	if res.ProviderSource == nil {
		return bindingParams{}
	}
	src := res.ProviderSource
	return bindingParams{
		ProviderID: pgconv.UUID(src.ProviderID),
		Revision:   pgtype.Int4{Int32: src.Revision, Valid: true},
		KeyMode:    pgtype.Text{String: providerKeyModeUser, Valid: true},
		KeySecret:  pgconv.UUID(src.KeySecretID),
		KeyRev:     pgtype.Int8{Int64: src.KeyRev, Valid: true},
	}
}

// bindingOfRun is the binding columns of an existing run row, copied verbatim (inherited mode).
func bindingOfRun(r store.Run) bindingParams {
	return bindingParams{
		ProviderID: r.ModelProviderID,
		Revision:   r.ModelProviderRevision,
		KeyMode:    r.ModelProviderKeyMode,
		KeySecret:  r.ModelProviderKeySecretID,
		KeyRev:     r.ModelProviderKeyRev,
	}
}

func (b bindingParams) equalsRun(r store.Run) bool { return b == bindingOfRun(r) }

// providerBoundRun reports whether the run carries a provider binding.
func providerBoundRun(r store.Run) bool { return r.ModelProviderID.Valid }

// freezeProviderSource re-reads and re-validates the owner's source under the freeze locks: the
// per-user secret-mutation lock (shared) first, THEN the provider row's share lock, the order every
// edit takes (edits hold the secret lock, then the provider row), so the two never cycle. A source
// that turned unusable, or changed, since the pre-lock read is refused or replaced by what is
// committed now.
func (s *Service) freezeProviderSource(ctx context.Context, tx store.DBTX, q *store.Queries, userID uuid.UUID, engines ...Harness) (providerSource, error) {
	engine := providers.EngineClaude
	if len(engines) > 0 {
		engine = string(engines[0])
	}
	if err := store.LockSecretMutationShared(ctx, tx, userID); err != nil {
		return providerSource{}, fmt.Errorf("provider freeze: secret lock: %w", err)
	}
	pre, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: userID, Engine: engine})
	if err != nil {
		return providerSource{}, fmt.Errorf("provider freeze: read source: %w", err)
	}
	if !pre.ProviderID.Valid {
		// Cleared since the resolution: the owner no longer has a provider; refuse rather than
		// freeze a binding the owner just removed or silently turn this into a vendor run.
		return providerSource{}, &ProviderSourceUnusableError{Reason: SourceUnusableProviderMissing}
	}
	if _, err := q.LockModelProviderForShare(ctx, store.LockModelProviderForShareParams{ID: uuid.UUID(pre.ProviderID.Bytes), OwnerUserID: userID}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return providerSource{}, &ProviderSourceUnusableError{ProviderID: uuid.UUID(pre.ProviderID.Bytes), Reason: SourceUnusableProviderMissing}
		}
		return providerSource{}, fmt.Errorf("provider freeze: lock provider: %w", err)
	}
	// Re-read under the lock: the provider row cannot change from here to the end of the tx.
	row, err := q.GetUserProviderSource(ctx, store.GetUserProviderSourceParams{ID: userID, Engine: engine})
	if err != nil {
		return providerSource{}, fmt.Errorf("provider freeze: re-read source: %w", err)
	}
	res := EvaluateProviderSource(engine, row, s.providerAllow)
	if res.State != ProviderSourceUsable {
		if res.State == ProviderSourceNone {
			return providerSource{}, &ProviderSourceUnusableError{Reason: SourceUnusableProviderMissing}
		}
		return providerSource{}, res.Err()
	}
	return res.Source, nil
}

// inheritProviderBinding re-validates a source run's binding for an inherited successor, WITHOUT
// consulting the owner's current source: the revision row still exists, the key row still is the
// bound anthropic_compatible_key of that provider with the frozen enablement_rev and is enabled,
// and the revision's URL is still allowlisted. Takes the shared secret-mutation lock first, so a
// concurrent rotation either precedes it (stale) or follows the successor's commit.
func (s *Service) inheritProviderBinding(ctx context.Context, tx store.DBTX, q *store.Queries, src store.Run) (providerSource, error) {
	if !src.ModelProviderID.Valid || !src.ModelProviderRevision.Valid || !src.ModelProviderKeySecretID.Valid || !src.ModelProviderKeyRev.Valid {
		return providerSource{}, errProviderBindingMismatch
	}
	if err := store.LockSecretMutationShared(ctx, tx, src.UserID); err != nil {
		return providerSource{}, fmt.Errorf("provider inherit: secret lock: %w", err)
	}
	pid := uuid.UUID(src.ModelProviderID.Bytes)
	rev, err := q.GetModelProviderRevisionForOwner(ctx, store.GetModelProviderRevisionForOwnerParams{
		ProviderID: pid, Revision: src.ModelProviderRevision.Int32, OwnerUserID: src.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return providerSource{}, fmt.Errorf("%w: the bound revision no longer exists", ErrProviderBindingStale)
		}
		return providerSource{}, fmt.Errorf("provider inherit: read revision: %w", err)
	}
	entry, ok := s.providerAllow(rev.BaseUrl)
	if !ok {
		return providerSource{}, ErrProviderDestinationNotAllowed
	}
	key, err := q.GetProviderBindingKeyByID(ctx, store.GetProviderBindingKeyByIDParams{ID: uuid.UUID(src.ModelProviderKeySecretID.Bytes), UserID: src.UserID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return providerSource{}, fmt.Errorf("%w: the bound key no longer exists", ErrProviderBindingStale)
		}
		return providerSource{}, fmt.Errorf("provider inherit: read key: %w", err)
	}
	if key.Kind != providerKeyKind(src.Harness) || !key.ProviderID.Valid || uuid.UUID(key.ProviderID.Bytes) != pid ||
		key.DisabledAt.Valid || key.EnablementRev != src.ModelProviderKeyRev.Int64 {
		return providerSource{}, fmt.Errorf("%w: the bound key was rotated, disabled or replaced", ErrProviderBindingStale)
	}
	var mm providers.ModelMap
	if err := json.Unmarshal(rev.ModelMap, &mm); err != nil || providers.ValidateProfile(src.Harness, rev.Protocol, rev.AuthStyle, mm) != nil {
		return providerSource{}, errProviderBindingMismatch
	}
	return providerSource{
		Engine: src.Harness, ProviderID: pid, Revision: rev.Revision, BaseURL: entry, AuthStyle: rev.AuthStyle, ModelMap: mm,
		KeySecretID: uuid.UUID(src.ModelProviderKeySecretID.Bytes), KeyRev: src.ModelProviderKeyRev.Int64,
	}, nil
}

// providerDefaultModelAlias is the alias an unset lead model resolves through: with no model the
// Claude CLI uses its default, which in provider mode is the SONNET slot (ANTHROPIC_DEFAULT_SONNET_MODEL),
// so a provider whose map lacks sonnet cannot serve a run that sets no model. The choice fails
// closed: an unmapped default would send the CLI's vendor model id to the gateway.
const providerDefaultModelAlias = "sonnet"

// modelMapped reports whether model resolves through the map: empty means the default alias; an
// alias must be present in the map; any other value must equal a mapped target exactly.
func modelMapped(m providers.ModelMap, model string) bool {
	model = strings.TrimSpace(model)
	if model == "" {
		model = providerDefaultModelAlias
	}
	for _, a := range providers.ModelAliases {
		if model == a {
			_, ok := m[a]
			return ok
		}
	}
	for _, e := range m {
		if e.Model == model {
			return true
		}
	}
	return false
}

// checkProviderModels refuses (provider_model_unmapped) a creation whose lead model, or any enabled
// agent template's own model, would not resolve through the bound map. The lead model is the one
// claim assembly resolves for a Claude run, through the same resolveLeadSettings the claim calls: a
// compatible frozen model (run override or schedule pin) wins, else the owner's Claude lane default
// (a Codex role token there, or an incompatible frozen model, is dropped exactly as at the claim),
// else the CLI default (the sonnet alias). Templates with no model inherit the lead's, and a template
// the run's agent exclusions drop is not checked. Repo-sourced agents are only known after the
// worker detects the repo roster, and models that only exist at spawn (summary, classifier, advice,
// later agent selection) are checked at their spawn (PRD module 7; worker-side, not here).
func (s *Service) checkProviderModels(ctx context.Context, q *store.Queries, run store.Run, src providerSource) error {
	if src.Engine == providers.EngineCodex {
		if providers.ValidateModelMapForEngine(src.Engine, src.ModelMap) != nil {
			return errProviderBindingMismatch
		}
		if run.Model.Valid && (!run.ModelSource.Valid || run.ModelSource.String == string(leadcfg.ModelSourceRunOverride) || run.ModelSource.String == string(leadcfg.ModelSourceSchedulePin)) {
			if _, ok := src.ModelMap[run.Model.String]; !ok {
				return &ProviderModelUnmappedError{Model: run.Model.String, Source: "run"}
			}
		}
		return nil
	}

	lanes, err := q.GetUserHarnessModelDefaults(ctx, run.UserID)
	if err != nil {
		return fmt.Errorf("provider model check: user defaults: %w", err)
	}
	in := LeadSettingsInput{
		Harness:        HarnessClaude,
		RunModel:       run.Model,
		RunModelSource: run.ModelSource,
		LaneModel:      lanes.DefaultClaudeModel,
	}
	// PRD #3252: a lead-lane claim also ships the owner's planning model, which must map too.
	if leadcfg.LeadLane(run.Kind, run.ReviewTargetRunID.Valid, run.EgressProfileID.Valid) {
		planning, perr := q.GetUserPlanningClaude(ctx, run.UserID)
		if perr != nil {
			return fmt.Errorf("provider model check: planning defaults: %w", perr)
		}
		in.PlanningModel, in.PlanningEffort = planning.PlanningClaudeModel, planning.PlanningClaudeEffort
	}
	settings := resolveLeadSettings(in)
	lead, leadFrom := settings.Model.String, "user default"
	switch settings.ModelSource {
	case leadcfg.ModelSourceRunOverride, leadcfg.ModelSourceSchedulePin:
		leadFrom = "run"
	}
	// An unhonorable run override is refused where the model is validated at create (and parks the
	// claim otherwise); it has no resolved lead to map.
	if settings.Unhonorable == nil && !modelMapped(src.ModelMap, lead) {
		name := strings.TrimSpace(lead)
		if name == "" {
			name = providerDefaultModelAlias
			leadFrom = "default model"
		}
		return &ProviderModelUnmappedError{Model: name, Source: leadFrom}
	}
	if plan := settings.Plan; settings.Unhonorable == nil && plan != nil && plan.Model != nil &&
		plan.ModelSource == string(leadcfg.ModelSourceUserPlanningDefault) && !modelMapped(src.ModelMap, *plan.Model) {
		return &ProviderModelUnmappedError{Model: *plan.Model, Source: "planning default"}
	}
	if run.Kind == runkind.Chat {
		return nil // a chat claim carries no agent templates
	}
	tmpls, err := q.ListClaimAgentTemplates(ctx, pgconv.UUID(run.UserID))
	if err != nil {
		return fmt.Errorf("provider model check: templates: %w", err)
	}
	excluded := map[string]bool{}
	if names, derr := DecodeExclusions(run.AgentExclusions); derr == nil {
		for _, n := range names {
			excluded[n] = true
		}
	}
	for _, t := range tmpls {
		if !t.Model.Valid || strings.TrimSpace(t.Model.String) == "" || excluded[t.Name] {
			continue
		}
		if !modelMapped(src.ModelMap, t.Model.String) {
			return &ProviderModelUnmappedError{Model: strings.TrimSpace(t.Model.String), Source: "template " + t.Name}
		}
	}
	return nil
}

// inheritOrigin is the creation origin of a SUCCESSOR of sourceRunID: it carries the source run row
// so createRunAtomic copies its provider binding verbatim (or resolves a vendor successor without
// the owner's provider source). A surface that is not a live *store.Queries (a unit fake) has no
// bindings and returns the zero origin.
func (s *Service) inheritOrigin(ctx context.Context, userID, sourceRunID uuid.UUID) (harnessCreationOrigin, error) {
	if _, live := s.q.(*store.Queries); !live {
		return harnessCreationOrigin{}, nil
	}
	src, err := s.q.GetRunByID(ctx, sourceRunID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return harnessCreationOrigin{}, ErrRunNotFound
		}
		return harnessCreationOrigin{}, fmt.Errorf("load source run: %w", err)
	}
	if src.UserID != userID {
		return harnessCreationOrigin{}, ErrRunNotFound
	}
	return harnessCreationOrigin{InheritFrom: &src}, nil
}
