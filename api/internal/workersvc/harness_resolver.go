package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/codexselect"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// harness_resolver.go is the PRD #1332 (M5A / D4) harness-routing contract: a
// pure D11 resolver plus the store-backed availability/credential-selection layer that
// feeds it. PRD #1429 routes production run creation through this resolver, including
// manual, chat, schedule, autopilot, self-improve, CI-fix and derived-run origins.

// Harness is a run's execution harness. The two values are the runs.harness /
// run_usage.harness CHECK vocabulary; they are BUILT from the unexported string
// literals usage_fold.go already owns (harnessClaude/harnessCodex), so the schema
// literal lives in exactly one place and a drift is a compile error, not a silent
// mismatch.
type Harness string

const (
	HarnessClaude Harness = harnessClaude
	HarnessCodex  Harness = harnessCodex
)

// harnessAvailability is the pair of "does this harness have a usable credential right
// now" facts the resolver decides over. It is a plain value so the whole resolver is a
// pure, database-free function that the unit tests drive exhaustively.
type harnessAvailability struct {
	claudeUsable bool
	codexUsable  bool
}

// usable reports whether the named harness has a usable credential. An unknown harness
// value (impossible under the runs CHECK, but a caller could pass a stray explicit
// value) is never usable — the safe direction, since it makes an unrecognised explicit
// selection a no_credential_for_harness refusal rather than a silent pick.
func (a harnessAvailability) usable(h Harness) bool {
	switch h {
	case HarnessClaude:
		return a.claudeUsable
	case HarnessCodex:
		return a.codexUsable
	default:
		return false
	}
}

// resolveHarnessInput is the full input to the pure resolver: an optional explicit
// selection (M5B's request field, always nil in M5A), an optional usable-user-default
// preference (users.default_harness, always NULL outside direct test setup in M5A), and
// the availability facts.
type resolveHarnessInput struct {
	// explicit is the harness the caller asked for outright. When set it is
	// authoritative and NEVER falls back to the other harness (D4).
	explicit *Harness
	// userDefault is the user's persisted default_harness preference, or nil for "no
	// preference". A default whose harness has no usable credential is SKIPPED (the
	// resolver falls through), never an error.
	userDefault *Harness
	avail       harnessAvailability
}

var (
	// errNoCredentialForHarness is the typed refusal when an EXPLICIT harness was asked
	// for but that harness has no usable credential. Explicit selection never falls back
	// to the other harness (D4), so this is terminal for that request — the resolver does
	// NOT silently substitute Claude or Codex.
	errNoCredentialForHarness = errors.New("no_credential_for_harness")

	// errNoUsableCredential is the typed refusal when NEITHER harness has a usable
	// credential. It WRAPS the existing errCredentialUnavailable so a consumer that keys
	// on errCredentialUnavailable keeps the existing refusal/failure behavior (D4: "no
	// usable credential retains the existing refusal/failure behavior") while a resolver
	// unit test can still assert this precise sentinel. M5B may remap it to a surface
	// error; the terminal-failure semantics are already correct via the wrap.
	errNoUsableCredential = fmt.Errorf("%w: no usable harness credential for this user", errCredentialUnavailable)

	// errHarnessCredentialDisabled is errNoCredentialForHarness for an EXPLICIT harness whose
	// credential slot holds credentials, every one of them disabled (PRD #1732 D15: presence
	// tells a stored-but-disabled credential apart from an absent one). It WRAPS
	// errNoCredentialForHarness, so every consumer keying on that keeps refusing the request
	// with no fallback; the scheduler alone tells it apart to record the credential_disabled
	// skip rather than a hard, non-advancing refusal.
	errHarnessCredentialDisabled = fmt.Errorf("%w: the selected harness's credentials are all disabled; enable one in Settings", errNoCredentialForHarness)

	// errCodexPoolUnavailable is errNoCredentialForHarness for an explicit Codex harness, or a
	// pooled pick that fails under the lock, whose
	// user runs codex_account_mode 'auto' with a configured pool in which no alias can be bound
	// (every pooled alias is unlinked, quarantined or needs re-authentication; issue #3719). It
	// WRAPS errNoCredentialForHarness, so every consumer keying on that keeps refusing the
	// request with no fallback; the scheduler and handlers alone tell it apart to say why. The
	// message is nonsecret and points at the remedy.
	errCodexPoolUnavailable = fmt.Errorf("%w: no pooled Codex account is available; check Codex accounts in Settings", errNoCredentialForHarness)
)

// harnessRule names the resolveHarness step that decided the harness.
type harnessRule string

const (
	harnessRuleExplicit    harnessRule = "explicit"
	harnessRuleUserDefault harnessRule = "user_default"
	harnessRuleSoleUsable  harnessRule = "sole_usable"
	harnessRuleBothClaude  harnessRule = "both_claude"
)

// resolveHarness is the PURE D11 harness resolver (PRD #1332 D4, parent D11). It takes
// no context and no store and touches nothing external, so the whole decision matrix is
// unit-testable against hand-written inputs. The order is EXACTLY:
//
//  1. an explicit harness is used, or returns errNoCredentialForHarness if that harness
//     has no usable credential — explicit selection NEVER falls back to the other one;
//  2. otherwise a usable user default is used (a set-but-unusable default falls through,
//     it is not an error);
//  3. otherwise the sole harness that has a usable credential is used;
//  4. otherwise (both usable, no usable default) Claude is chosen (the usage-free answer;
//     an eligible automatic creation may choose by weekly limits, see harness_automatic.go);
//  5. otherwise (neither usable) errNoUsableCredential.
//
// It also returns the rule that fired (harnessRule), empty on error, so a read-only preview can
// say why the harness was chosen without a second implementation of the order.
func resolveHarness(in resolveHarnessInput) (Harness, harnessRule, error) {
	// (1) Explicit selection is authoritative and terminal — used if usable, else a
	// typed refusal with NO fallback to the other harness.
	if in.explicit != nil {
		h := *in.explicit
		if in.avail.usable(h) {
			return h, harnessRuleExplicit, nil
		}
		return "", "", errNoCredentialForHarness
	}

	// (2) A usable user default wins next. A default set to a harness with no usable
	// credential is skipped so the resolver falls through to the availability rules,
	// rather than refusing.
	if in.userDefault != nil && in.avail.usable(*in.userDefault) {
		return *in.userDefault, harnessRuleUserDefault, nil
	}

	// (3)-(5) Availability rules.
	switch {
	case in.avail.claudeUsable && !in.avail.codexUsable:
		return HarnessClaude, harnessRuleSoleUsable, nil // (3) sole Claude
	case in.avail.codexUsable && !in.avail.claudeUsable:
		return HarnessCodex, harnessRuleSoleUsable, nil // (3) sole Codex
	case in.avail.claudeUsable && in.avail.codexUsable:
		return HarnessClaude, harnessRuleBothClaude, nil // (4) both usable, no usable default → Claude (usage-free answer; eligible automatic creation may choose by weekly limits, harness_automatic.go)
	default:
		return "", "", errNoUsableCredential // (5) neither usable
	}
}

// codexCredentialChoice is the Codex credential the resolver selected for a run: the
// alias secret id and its auth mode (codexAuthModeSubscription / codexAuthModeAPIKey),
// plus the label for the snapshot the binding records. It is exactly what M5B needs to
// hand FreezeCodexBinding (secretID + authMode); the immutable-binding invariants are
// then enforced at claim time by the existing GetRunCodexAuthContext authority path,
// not here.
type codexCredentialChoice struct {
	SecretID uuid.UUID
	Label    string
	AuthMode string
	// Select says how createRunAtomic binds the alias (issue #3719): codexChoiceDefault and
	// codexChoicePoolEmpty bind SecretID as today; codexChoicePool means SecretID is NOT yet
	// chosen and the creation transaction picks a pooled alias under the secret-mutation lock.
	Select codexChoiceKind
}

// codexChoiceKind marks how a resolved Codex choice is bound (issue #3719). The first two
// values double as the codexselect.Reason recorded for them.
type codexChoiceKind string

const (
	// codexChoiceDefault: users.codex_account_mode is 'default' (today's path, untouched).
	codexChoiceDefault codexChoiceKind = "default"
	// codexChoicePoolEmpty: mode 'auto' but no alias is pooled, so the default is bound.
	codexChoicePoolEmpty codexChoiceKind = "pool_empty"
	// codexChoicePool: mode 'auto' with a configured pool that has a structurally eligible alias.
	codexChoicePool codexChoiceKind = "pool"
)

// codexAccountModeAuto is users.codex_account_mode's opt-in value; every other value reads as
// 'default'.
const codexAccountModeAuto = "auto"

// resolvedHarness is the full result of resolveRunHarness: the chosen harness and, when
// that harness uses vendor Codex, the selected credential. Provider runs leave Codex nil.
type resolvedHarness struct {
	Harness        Harness
	Codex          *codexCredentialChoice
	CreationPolicy *store.GetHarnessCreationPolicyRow
	Decision       *harnessCreationDecision
	// DefaultUnset is true when users.default_harness is NULL (the "Default · automatic" choice,
	// #3845). It reads the raw column: userDefaultHarness's nil also maps an unknown value.
	DefaultUnset bool
	// BothUsable is true when both harnesses have a usable credential. The pure resolveHarness
	// never reads either field, so every usage-independent reader is unchanged.
	BothUsable bool
	// Rule is the resolveHarness rule that fired. Existing readers ignore it.
	Rule harnessRule
	// DefaultSkipped is true when users.default_harness named a harness with no usable credential
	// and no explicit harness was given, so the resolver fell through to the availability rules.
	DefaultSkipped bool
	// ClaudeSourceSet is true when the owner selected a Claude provider source (usable or not;
	// PRD #4320 M4). Either source flag excludes automatic harness choice and
	// for creation overflow in either direction.
	ClaudeSourceSet bool
	CodexSourceSet  bool
	// ProviderSource is the usable provider source a run will freeze; nil for a vendor run
	// (null source or an inherited vendor successor).
	ProviderSource *providerSource
}

// automaticHarnessEligible reports whether creation picks the harness by soonest weekly reset
// (#3845): an unset default, no explicit harness, both harnesses usable and an eligible origin.
func automaticHarnessEligible(res resolvedHarness, explicit *Harness, origin harnessCreationOrigin, p store.GetHarnessCreationPolicyRow) bool {
	// A set provider source is never compared with vendor weekly readings.
	return res.DefaultUnset && explicit == nil && res.BothUsable && !res.ClaudeSourceSet && !res.CodexSourceSet && origin.eligible(p)
}

// harnessResolverStore is the narrow C1-frozen read surface the store-backed resolver
// consumes. *store.Queries satisfies it; the production Service's s.q is a *store.Queries
// underneath, reached via harnessStore() below. Keeping it a local narrow interface
// (the same idiom codexauthz.go's codexAuthzStore uses) means this dark layer reuses
// ONLY existing C1 queries without widening the broad Store interface — GetUserDefaultHarness
// and CountCodexSecrets are C1 additions not on that interface, and this file must not edit it.
//
// PRD #1732 D15: usability reads the ENABLED variants (UserHasEnabledAnthropicToken,
// CountEnabledCodexSecrets); the any-credential variants are read only to classify an explicit
// refusal as disabled-not-absent (errHarnessCredentialDisabled).
type harnessResolverStore interface {
	providerLaneSourceReader
	engineProviderSourceReader
	UserHasAnthropicToken(ctx context.Context, userID uuid.UUID) (bool, error)
	UserHasEnabledAnthropicToken(ctx context.Context, userID uuid.UUID) (bool, error)
	GetUserDefaultHarness(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	CountCodexSecrets(ctx context.Context, userID uuid.UUID) (int64, error)
	CountEnabledCodexSecrets(ctx context.Context, userID uuid.UUID) (int64, error)
	GetDefaultUserSecretMeta(ctx context.Context, arg store.GetDefaultUserSecretMetaParams) (store.GetDefaultUserSecretMetaRow, error)
	GetCodexCredentialState(ctx context.Context, arg store.GetCodexCredentialStateParams) (store.CodexCredentialState, error)
	// Issue #3719: the user's Codex account mode and pooled aliases decide usability in 'auto'.
	GetUserCodexAccountMode(ctx context.Context, id uuid.UUID) (string, error)
	ListCodexPoolAliases(ctx context.Context, userID uuid.UUID) ([]store.ListCodexPoolAliasesRow, error)
}

// errHarnessStoreUnavailable: the service's store does not expose the resolver's read
// surface (a test store, or a misconfiguration). Never happens with *store.Queries.
var errHarnessStoreUnavailable = errors.New("harness resolver query surface unavailable")

// harnessStore adapts the Service's Store to the narrow resolver read surface, exactly
// as codexStore() does for the Codex authority surface.
func (s *Service) harnessStore() (harnessResolverStore, bool) {
	q, ok := s.q.(harnessResolverStore)
	return q, ok
}

// resolveRunHarness gathers the D11 availability/preference facts from the C1-frozen read
// queries, runs the pure resolveHarness, and — when the result is Codex — carries the
// selected Codex credential (PRD #1332 D4). Production creation calls it through the
// atomic create seam, so the resolved harness is the one persisted and frozen.
//
// explicit is the caller's outright or inherited harness choice, nil for "let the resolver
// decide". Public request pins and derived-run inheritance both use the explicit path.
//
// Codex usability follows resolveUsableCodexCredential: in codex_account_mode 'default' (or
// 'auto' with no pooled alias) the user's single default codex credential decides both
// whether Codex is usable AND which credential a Codex run binds, so a failed or missing
// subscription default is simply "Codex not usable" and NEVER falls through to spend a
// non-default API key (D4). In 'auto' with a configured pool, Codex is usable iff a pooled
// alias is structurally eligible, and the alias is picked later, inside the creation
// transaction (issue #3719). It acquires no lock or lease (D6): it is reads only, so two
// runs bound to one canonical subscription resolve concurrently.
func (s *Service) resolveRunHarness(ctx context.Context, userID uuid.UUID, explicit *Harness) (resolvedHarness, error) {
	q, ok := s.harnessStore()
	if !ok {
		return resolvedHarness{}, errHarnessStoreUnavailable
	}
	return s.resolveRunHarnessQ(ctx, userID, explicit, q)
}

// resolveRunHarnessQ is resolveRunHarness against an EXPLICIT query surface rather than the
// ambient s.q (PRD #1429 M1, D1). The atomic create seam (createRunAtomic) passes a
// transaction-bound *store.Queries (WithTx) so the credential/default facts D11 decides over
// are re-read INSIDE the same transaction that commits the run and freezes a Codex binding —
// closing the window where a credential deletion between a pre-tx read and the commit could
// leave a Codex row bound to a vanished credential. *store.Queries satisfies harnessResolverStore,
// so a qtx derived via WithTx is a valid q here. resolveRunHarness is the ambient-q wrapper.
func (s *Service) resolveRunHarnessQ(ctx context.Context, userID uuid.UUID, explicit *Harness, q harnessResolverStore) (resolvedHarness, error) {
	return s.resolveRunHarnessMode(ctx, userID, explicit, q, false)
}

// claudeAvailability is the Claude half of the availability facts. Without a provider source (the
// null path, and every vendorOnly resolution) it is exactly today's rule: "does the user hold an
// ENABLED Anthropic token" (PRD #1732 D15: a slot whose tokens are all disabled has no default and
// counts as unavailable, so a new implicit request falls back as it would for a token-less user).
// A usable provider source makes Claude usable regardless of any Anthropic token; a set but
// unusable one makes it unusable (the caller then refuses rather than fall back). vendorOnly skips
// the source read entirely: an inherited vendor successor resolves vendor credentials as today and
// never consults the owner's current source (PRD #4320: successors use the source run's binding).
func (s *Service) claudeAvailability(ctx context.Context, q harnessResolverStore, userID uuid.UUID, vendorOnly bool) (bool, ProviderSourceResult, error) {
	var src ProviderSourceResult
	if !vendorOnly {
		var err error
		src, err = ResolveProviderSource(ctx, q, userID, string(HarnessClaude), s.providerAllow)
		if err != nil {
			return false, ProviderSourceResult{}, fmt.Errorf("harness resolve: %w", err)
		}
		switch src.State {
		case ProviderSourceUsable:
			return true, src, nil
		case ProviderSourceUnusable:
			return false, src, nil
		}
	}
	claudeUsable, err := q.UserHasEnabledAnthropicToken(ctx, userID)
	if err != nil {
		return false, src, fmt.Errorf("harness resolve: anthropic token check: %w", err)
	}
	return claudeUsable, src, nil
}

// resolveRunHarnessMode is resolveRunHarnessQ with the provider-source switch. vendorOnly resolves
// as if the owner had no provider source (see claudeAvailability).
//
// With a set source (PRD #4320 module 4):
//   - usable: Claude is usable, and a Claude result carries ProviderSource;
//   - unusable: Claude is NOT usable and the result is the typed ErrProviderSourceUnusable BEFORE any
//     same- or cross-engine fallback, unless the caller pinned Codex outright (explicit codex) or
//     the owner's stored default is a usable Codex. An unset default (automatic) never picks Codex
//     around an unusable source.
func (s *Service) resolveRunHarnessMode(ctx context.Context, userID uuid.UUID, explicit *Harness, q harnessResolverStore, vendorOnly bool) (resolvedHarness, error) {
	claudeUsable, src, err := s.claudeAvailability(ctx, q, userID, vendorOnly)
	if err != nil {
		return resolvedHarness{}, err
	}

	// Codex usability + the selected credential in one pass, so the fact that decided
	// availability is the exact credential a Codex run then binds (no re-read, no lock).

	var codexSrc ProviderSourceResult
	if !vendorOnly {
		codexSrc, err = ResolveProviderSource(ctx, q, userID, string(HarnessCodex), s.providerAllow)
		if err != nil {
			return resolvedHarness{}, err
		}
	}
	var codexChoice codexCredentialChoice
	codexUsable := codexSrc.State == ProviderSourceUsable
	if !codexSrc.Set() {
		codexChoice, codexUsable, err = s.resolveUsableCodexCredential(ctx, userID, q)
		if err != nil {
			return resolvedHarness{}, err
		}
	}

	userDefault, defaultUnset, err := s.userDefaultHarness(ctx, userID, q)
	if err != nil {
		return resolvedHarness{}, err
	}
	bothUsable := claudeUsable && codexUsable
	preferred := explicit
	if preferred == nil {
		preferred = userDefault
	}
	// A selected source refuses on its own preferred engine even when both sources are broken.
	if preferred != nil && *preferred == HarnessClaude && src.State == ProviderSourceUnusable {
		return resolvedHarness{}, src.Err()
	}
	if preferred != nil && *preferred == HarnessCodex && codexSrc.State == ProviderSourceUnusable {
		return resolvedHarness{}, codexSrc.Err()
	}
	if codexSrc.State == ProviderSourceUnusable {
		claudeWins := explicit != nil && *explicit == HarnessClaude
		if explicit == nil {
			claudeWins = userDefault != nil && *userDefault == HarnessClaude && claudeUsable
		}
		if !claudeWins {
			return resolvedHarness{}, codexSrc.Err()
		}
	}

	if src.State == ProviderSourceUnusable {
		// Claude would be the answer unless Codex is pinned outright or is the usable stored default.
		codexWins := false
		if explicit != nil {
			codexWins = *explicit == HarnessCodex
		} else {
			codexWins = userDefault != nil && *userDefault == HarnessCodex && codexUsable
		}
		if !codexWins {
			return resolvedHarness{}, src.Err()
		}
	}

	h, rule, err := resolveHarness(resolveHarnessInput{
		explicit:    explicit,
		userDefault: userDefault,
		avail:       harnessAvailability{claudeUsable: claudeUsable, codexUsable: codexUsable},
	})
	if err != nil {
		if explicit != nil && errors.Is(err, errNoCredentialForHarness) {
			return resolvedHarness{}, explicitHarnessRefusal(ctx, q, userID, *explicit)
		}
		return resolvedHarness{}, err
	}

	defaultSkipped := explicit == nil && userDefault != nil &&
		!(harnessAvailability{claudeUsable: claudeUsable, codexUsable: codexUsable}).usable(*userDefault)

	out := resolvedHarness{Harness: h, DefaultUnset: defaultUnset, BothUsable: bothUsable, Rule: rule, DefaultSkipped: defaultSkipped, ClaudeSourceSet: src.Set(), CodexSourceSet: codexSrc.Set()}
	selected := src
	if h == HarnessCodex {
		selected = codexSrc
		if !selected.Set() {
			out.Codex = &codexChoice
		}
	}
	if selected.State == ProviderSourceUsable {
		bound := selected.Source
		out.ProviderSource = &bound
	}

	return out, nil
}

// explicitHarnessRefusal classifies an explicit harness's no-usable-credential refusal (PRD
// #1732 D15): errHarnessCredentialDisabled when the harness's slot holds credentials but none is
// enabled, else the plain errNoCredentialForHarness. Both refuse the request; neither falls
// back. A store error is returned as-is.
func explicitHarnessRefusal(ctx context.Context, q harnessResolverStore, userID uuid.UUID, h Harness) error {
	switch h {
	case HarnessClaude:
		present, err := q.UserHasAnthropicToken(ctx, userID)
		if err != nil {
			return fmt.Errorf("harness resolve: anthropic token presence: %w", err)
		}
		if present {
			return errHarnessCredentialDisabled
		}
	case HarnessCodex:
		total, err := q.CountCodexSecrets(ctx, userID)
		if err != nil {
			return fmt.Errorf("harness resolve: count codex secrets: %w", err)
		}
		enabled, err := q.CountEnabledCodexSecrets(ctx, userID)
		if err != nil {
			return fmt.Errorf("harness resolve: count enabled codex secrets: %w", err)
		}
		if total > 0 && enabled == 0 {
			return errHarnessCredentialDisabled
		}
		// Issue #3719: an 'auto' user with a configured pool was refused because no pooled
		// alias is bindable, which is a different remedy from "add a credential".
		auto, rows, err := codexAutoPool(ctx, q, userID)
		if err != nil {
			return err
		}
		if auto && len(rows) > 0 {
			return errCodexPoolUnavailable
		}
	}
	return errNoCredentialForHarness
}

// BothHarnessesUsable applies the same credential rules as run creation when a user
// enables plan cross-check. In particular, Codex must be usable under the user's
// codex_account_mode (resolveUsableCodexCredential); no usage score is read.
func (s *Service) BothHarnessesUsable(ctx context.Context, userID uuid.UUID) (bool, error) {
	q, ok := s.harnessStore()
	if !ok {
		return false, errHarnessStoreUnavailable
	}
	if err := s.refuseProviderLane(ctx, q, userID, HarnessCodex); err != nil {
		if errors.Is(err, ErrProviderLaneUnsupported) {
			return false, nil
		}
		return false, err
	}
	claude, _, err := s.claudeAvailability(ctx, q, userID, true)
	if err != nil {
		return false, err
	}
	if !claude {
		return false, nil
	}
	_, codex, err := s.resolveUsableCodexCredential(ctx, userID, q)
	return codex, err
}

// ResolveSettingsHarness resolves the harness the settings surface projects the legacy
// default_model lane from (PRD #1551 M1 / D3): it runs the same D11 resolver production run
// creation uses, with no explicit selection, so a usable stored default_harness is honoured.
// Unlike run creation, a credential-unavailability refusal is NOT an error here — settings
// GET/PUT must never fail merely because no harness is usable, so errNoUsableCredential and
// errNoCredentialForHarness both map to (HarnessClaude, nil), matching the zero-credential
// projection D3 requires. Only a genuine store error propagates. It is reads only (no lock).
func (s *Service) ResolveSettingsHarness(ctx context.Context, userID uuid.UUID) (Harness, error) {
	q, ok := s.harnessStore()
	if !ok {
		return "", errHarnessStoreUnavailable
	}
	pref, _, err := s.userDefaultHarness(ctx, userID, q)
	if err != nil {
		return "", err
	}
	return s.ResolveSettingsHarnessWithDefault(ctx, userID, pref)
}

// ResolveSettingsHarnessWithDefault applies the D11 preference order to a proposed
// default_harness before it is saved. The settings handler uses this to keep the legacy
// default_model column aligned with the harness a run would actually select after the
// grouped write. An unusable pin falls through to the usable harness; no credentials
// projects to Claude without failing the settings request. It does not read the stored
// default_harness because the proposed value replaces it in the same write.
func (s *Service) ResolveSettingsHarnessWithDefault(ctx context.Context, userID uuid.UUID, proposed *Harness) (Harness, error) {
	q, ok := s.harnessStore()
	if !ok {
		return "", errHarnessStoreUnavailable
	}
	claudeUsable, src, err := s.claudeAvailability(ctx, q, userID, false)
	if err != nil {
		return "", err
	}
	codexSrc, err := ResolveProviderSource(ctx, q, userID, string(HarnessCodex), s.providerAllow)
	if err != nil {
		return "", err
	}
	codexUsable := codexSrc.State == ProviderSourceUsable
	if !codexSrc.Set() {
		_, codexUsable, err = s.resolveUsableCodexCredential(ctx, userID, q)
		if err != nil {
			return "", err
		}
	}
	if proposed != nil && *proposed == HarnessClaude && src.State == ProviderSourceUnusable {
		return HarnessClaude, nil
	}
	if proposed != nil && *proposed == HarnessCodex && codexSrc.State == ProviderSourceUnusable {
		return HarnessCodex, nil
	}
	if codexSrc.State == ProviderSourceUnusable && (proposed == nil || *proposed != HarnessClaude || !claudeUsable) {
		return HarnessCodex, nil
	}

	if src.State == ProviderSourceUnusable && (proposed == nil || *proposed != HarnessCodex || !codexUsable) {
		// A set Claude source never projects to Codex (creation refuses rather than fall back).
		return HarnessClaude, nil
	}
	h, _, err := resolveHarness(resolveHarnessInput{
		userDefault: proposed,
		avail:       harnessAvailability{claudeUsable: claudeUsable, codexUsable: codexUsable},
	})
	if errors.Is(err, errNoUsableCredential) {
		return HarnessClaude, nil
	}
	return h, err
}

// userDefaultHarness reads users.default_harness and maps it to a *Harness preference:
// NULL (or any value outside the CHECK vocabulary, which cannot occur) is nil for "no
// preference". unset reports a NULL column alone.
func (s *Service) userDefaultHarness(ctx context.Context, userID uuid.UUID, q harnessResolverStore) (pref *Harness, unset bool, err error) {
	raw, err := q.GetUserDefaultHarness(ctx, userID)
	if err != nil {
		return nil, false, fmt.Errorf("harness resolve: read default harness: %w", err)
	}
	if !raw.Valid {
		return nil, true, nil
	}
	switch Harness(raw.String) {
	case HarnessClaude:
		h := HarnessClaude
		return &h, false, nil
	case HarnessCodex:
		h := HarnessCodex
		return &h, false, nil
	default:
		// The default_harness CHECK admits only claude/codex, so this is unreachable;
		// treat an unexpected value as "no preference" rather than trusting it. It is
		// not "unset": only a NULL column is the automatic choice (#3845).
		return nil, false, nil
	}
}

// resolveUsableCodexCredential decides whether Codex is USABLE for the user. In
// codex_account_mode 'auto' with a configured pool it answers from the pooled aliases alone
// (see the comment in the body); otherwise it resolves the user's single default codex
// credential and returns the coordinates a Codex run would bind (PRD #1332 D4, M1's
// named-default rules), as described below.
//
// The codex kinds share ONE default slot (user_secrets_codex_one_default_key), so the
// default is at most one row across both kinds; it is resolved per-kind and whichever
// kind returns a row is the default. Usability is per auth mode:
//
//   - a subscription (codex_auth) default is usable only once LINKED to a provider
//     account (codex_credential_state.status='linked'); a staging/failed subscription is
//     NOT usable;
//   - an api_key (openai_api_key) default is usable by existence (a static key needs no
//     linking).
//
// Because usability keys on the DEFAULT credential, a failed or missing subscription
// default resolves to "Codex not usable" and NEVER falls through to a non-default API key
// (D4: "a failed or missing subscription never spends an API key"). It is reads only — no
// lock or lease (D6).
func (s *Service) resolveUsableCodexCredential(ctx context.Context, userID uuid.UUID, q harnessResolverStore) (codexCredentialChoice, bool, error) {
	// Issue #3719: users.codex_account_mode decides which rule applies. 'default' (and anything
	// unrecognised) is today's rule, untouched. 'auto' with no pooled alias is today's rule
	// too, marked pool_empty. 'auto' with a configured pool is usable iff at least one pooled
	// alias is structurally eligible; scores are never read here, and the alias itself is picked
	// inside the creation transaction (selectCodexPoolAlias).
	auto, rows, err := codexAutoPool(ctx, q, userID)
	if err != nil {
		return codexCredentialChoice{}, false, err
	}
	if auto && len(rows) > 0 {
		for _, r := range rows {
			if codexPoolRowStructural(r) {
				return codexCredentialChoice{AuthMode: codexAuthModeSubscription, Select: codexChoicePool}, true, nil
			}
		}
		return codexCredentialChoice{}, false, nil
	}
	choice, usable, err := s.resolveDefaultCodexCredential(ctx, userID, q)
	if err != nil || !usable {
		return codexCredentialChoice{}, false, err
	}
	choice.Select = codexChoiceDefault
	if auto {
		choice.Select = codexChoicePoolEmpty
	}
	return choice, true, nil
}

// codexAutoPool reads the user's Codex account mode and, only in 'auto', their pooled aliases
// (disabled ones included, so "configured but all disabled" stays distinct from "empty").
// Mode 'default' reads no pool, so its path costs one extra single-row read.
func codexAutoPool(ctx context.Context, q harnessResolverStore, userID uuid.UUID) (auto bool, rows []store.ListCodexPoolAliasesRow, err error) {
	mode, err := q.GetUserCodexAccountMode(ctx, userID)
	if err != nil {
		return false, nil, fmt.Errorf("harness resolve: read codex account mode: %w", err)
	}
	if mode != codexAccountModeAuto {
		return false, nil, nil
	}
	rows, err = q.ListCodexPoolAliases(ctx, userID)
	if err != nil {
		return true, nil, fmt.Errorf("harness resolve: list codex pool: %w", err)
	}
	return true, rows, nil
}

// codexPoolRowStructural reports whether a pooled alias may EVER be bound: enabled, linked to
// an account, the account not quarantined and not flagged for re-authentication. These are the
// facts the usability rule reads; capacity (limits, cooldown, reserve) is the selector's job.
func codexPoolRowStructural(r store.ListCodexPoolAliasesRow) bool {
	return r.Enabled &&
		r.LinkStatus.Valid && r.LinkStatus.String == codexStatusLinked &&
		r.ProviderAccountID.Valid && r.ProviderUserID.Valid && r.WorkspaceAccountID.Valid &&
		codexPoolAccountUsable(r.CoordState, r.ReauthRequired)
}

// resolveDefaultCodexCredential is the PRD #1332 D4 default-credential rule.
func (s *Service) resolveDefaultCodexCredential(ctx context.Context, userID uuid.UUID, q harnessResolverStore) (codexCredentialChoice, bool, error) {
	// Cheap "is Codex configured at all" guard. Zero ENABLED codex credentials ⇒ no default
	// to select (PRD #1732 D4/D15: disabling the slot's last credential clears its default) ⇒
	// Codex unusable, without the per-kind default lookups.
	n, err := q.CountEnabledCodexSecrets(ctx, userID)
	if err != nil {
		return codexCredentialChoice{}, false, fmt.Errorf("harness resolve: count codex secrets: %w", err)
	}
	if n == 0 {
		return codexCredentialChoice{}, false, nil
	}

	// Subscription (codex_auth) default first. It is usable only when linked.
	sub, ok, err := codexDefaultMeta(ctx, q, userID, store.KindCodexAuth)
	if err != nil {
		return codexCredentialChoice{}, false, err
	}
	if ok {
		// Defensive (PRD #1732 D4 keeps every default enabled): a disabled default is not
		// usable, and like a failed subscription it never falls through to another alias.
		if sub.Disabled {
			return codexCredentialChoice{}, false, nil
		}
		st, serr := q.GetCodexCredentialState(ctx, store.GetCodexCredentialStateParams{
			UserSecretID: sub.ID,
			UserID:       userID,
		})
		if serr != nil {
			if errors.Is(serr, pgx.ErrNoRows) {
				// A subscription default with no state row is not linked ⇒ not usable. It
				// does NOT fall through to a non-default api_key.
				return codexCredentialChoice{}, false, nil
			}
			return codexCredentialChoice{}, false, fmt.Errorf("harness resolve: read codex state: %w", serr)
		}
		if st.Status == codexStatusLinked && st.ProviderAccountID.Valid {
			return codexCredentialChoice{
				SecretID: sub.ID,
				Label:    sub.Label,
				AuthMode: codexAuthModeSubscription,
			}, true, nil
		}
		// A staging/failed subscription default is unusable — and per D4 the resolver must
		// NOT reach for a non-default api_key behind it.
		return codexCredentialChoice{}, false, nil
	}

	// api_key (openai_api_key) default. Usable by existence.
	api, ok, err := codexDefaultMeta(ctx, q, userID, store.KindOpenAIAPIKey)
	if err != nil {
		return codexCredentialChoice{}, false, err
	}
	if ok && !api.Disabled {
		return codexCredentialChoice{
			SecretID: api.ID,
			Label:    api.Label,
			AuthMode: codexAuthModeAPIKey,
		}, true, nil
	}

	// Codex credentials exist (n>0) but none is the default of either kind — an
	// inconsistent state the force-default invariant forbids. Treat it as not usable
	// (the safe direction: there is no default to select).
	return codexCredentialChoice{}, false, nil
}

// errCodexCreateRequiresTx is the FAIL-CLOSED refusal when a run resolves to Codex but no
// transaction beginner is wired (PRD #1429 M1, D1): a Codex run's binding freeze MUST commit
// atomically with the run INSERT, so with no transaction there is no safe path — creation
// refuses rather than inserting a Codex row non-atomically (which could leave an unbound Codex
// row on a subsequent freeze failure). A Claude resolution never reaches this: it keeps the
// existing cheap non-tx insert.
var errCodexCreateRequiresTx = errors.New("codex run creation requires an atomic transaction (no tx beginner wired)")

// createRunAtomic is the M5B atomic create seam (PRD #1429 M1, D1). It resolves the run's
// harness via D11 and runs a caller-supplied, INSERT-shape-agnostic run INSERT, and — when the
// resolved harness is Codex — freezes the run's Codex binding, ALL in ONE database transaction.
// So runtime / judge / task / mr_rework / prompt / self-improve / ci-fix INSERT shapes all use
// this one helper: each passes an insert closure that builds its own CreateXRunParams.
//
// The insert closure receives the transaction-bound *store.Queries AND the resolved harness, so
// it stamps runs.harness with the D11 result and (M2/M3) can validate a #1247 credential
// override against that EXACT harness inside the same tx via validateCredentialOverrideOn(ctx, q, ...,
// string(resolved.Harness), ...) — D5's "resolved-once-inside-the-tx", not a pre-transaction
// guess. (M1 builds and proves the seam; M2/M3 wire the production origins onto it.)
//
// Ordering (D1): begin tx → derive qtx via WithTx → resolve D11 through qtx (re-reading the
// credential/default facts inside the tx that commits) → insert(qtx, resolved) → when codex,
// freeze through the SAME qtx → commit. Any error rolls the whole transaction back, so a Codex
// freeze failure leaves NO runs row.
//
// A nil txBeginner is FAIL CLOSED for Codex (errCodexCreateRequiresTx): the binding freeze must
// commit atomically with the run, so with no transaction there is no safe insert — it never
// falls back to a non-atomic Codex insert. A Claude resolution with a nil txBeginner keeps the
// existing cheap non-tx path (no freeze is needed).
func (s *Service) createRunAtomic(ctx context.Context, userID uuid.UUID, explicit *Harness, insert func(q *store.Queries, resolved resolvedHarness) (store.Run, error), origins ...harnessCreationOrigin) (store.Run, resolvedHarness, error) {
	var origin harnessCreationOrigin
	if len(origins) > 0 {
		origin = origins[0]
	}
	inherit := origin.InheritFrom
	inheritedProvider := inherit != nil && providerBoundRun(*inherit)
	if s.txBeginner == nil {
		// No transaction available. Resolve via the ambient queries to learn the harness:
		// Codex fails closed (it needs the atomic freeze); Claude keeps the cheap path.
		if inheritedProvider {
			// A provider binding's re-check and insert need one transaction: fail closed.
			return store.Run{}, resolvedHarness{}, errProviderCreateRequiresTx
		}
		if (origin.RefuseProviderOwner || origin.JudgeLane) && (explicit == nil || (*explicit != HarnessClaude && *explicit != HarnessCodex)) {
			return store.Run{}, resolvedHarness{}, ErrProviderLaneUnsupported
		}
		rq, ok := s.harnessStore()
		if !ok {
			return store.Run{}, resolvedHarness{}, errHarnessStoreUnavailable
		}
		if origin.RefuseProviderOwner {
			if err := s.refuseProviderLane(ctx, rq, userID, *explicit); err != nil {
				return store.Run{}, resolvedHarness{}, err
			}
		}
		if origin.JudgeLane {
			if err := s.judgeLaneNonAtomic(ctx, userID, *explicit); err != nil {
				return store.Run{}, resolvedHarness{}, err
			}
		}
		res, err := s.resolveRunHarnessMode(ctx, userID, explicit, rq, inherit != nil || origin.RefuseProviderOwner || origin.JudgeLane)
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		if res.ProviderSource != nil {
			// A provider freeze takes the secret and provider locks in the create transaction;
			// there is no safe non-atomic insert (the resolver's Claude path is fail-closed here).
			return store.Run{}, resolvedHarness{}, errProviderCreateRequiresTx
		}
		// An overflow or automatic decision needs the shared lock and the fence, so it refuses
		// rather than decide without them.
		if q, ok := s.q.(*store.Queries); ok && origin.Kind == "issue" {
			policy, e := q.GetHarnessCreationPolicy(ctx, userID)
			if e != nil {
				return store.Run{}, resolvedHarness{}, e
			}
			if (policy.HarnessOverflowEnabled && origin.eligible(policy)) || automaticHarnessEligible(res, explicit, origin, policy) {
				return store.Run{}, resolvedHarness{}, errHarnessOverflowRequiresTx
			}
		}
		if res.Harness == HarnessCodex {
			return store.Run{}, resolvedHarness{}, errCodexCreateRequiresTx
		}
		q, ok := s.q.(*store.Queries)
		if !ok {
			return store.Run{}, resolvedHarness{}, errHarnessStoreUnavailable
		}
		if origin.ConsumerGateRepo.Valid {
			// Test-only path: a gated create still takes the gate, in a transaction of its own.
			run, err := s.insertGatedNoBeginner(ctx, q, origin.ConsumerGateRepo.Bytes, res, insert)
			if err != nil {
				return store.Run{}, resolvedHarness{}, err
			}
			return run, res, nil
		}
		run, err := insert(q, res)
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		return run, res, nil
	}

	tx, err := s.txBeginner.Begin(ctx)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	// A no-op after a successful Commit; on any early return it rolls back the run INSERT and
	// any Codex freeze, so a create that does not fully succeed commits nothing.
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := store.New(tx)

	// Issue #4488: the consumer gate is the FIRST thing the transaction does, before the provider
	// lane check, the inherited-provider freeze, the secret lock and every closure lock.
	if origin.ConsumerGateRepo.Valid {
		if err := s.takeConsumerGate(ctx, tx, origin.ConsumerGateRepo.Bytes); err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
	}

	if origin.RefuseProviderOwner {
		if explicit == nil {
			return store.Run{}, resolvedHarness{}, ErrProviderLaneUnsupported
		}
		if err := s.refuseProviderLane(ctx, qtx, userID, *explicit); err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
	}
	// A judge decides its lane under the shared secret-mutation lock (PRD #4322 M1a): the lock is
	// taken after the consumer gate and before the policy read, and held to commit, so every writer
	// of the pin policy and the sources (all exclusive) is ordered before this read or after commit.
	var judgeDec judgeLaneDecision
	if origin.JudgeLane {
		if explicit == nil || (*explicit != HarnessClaude && *explicit != HarnessCodex) {
			return store.Run{}, resolvedHarness{}, ErrProviderLaneUnsupported
		}
		if err := store.LockSecretMutationShared(ctx, tx, userID); err != nil {
			return store.Run{}, resolvedHarness{}, fmt.Errorf("judge lane: secret lock: %w", err)
		}
		dec, err := decideJudgeLaneFor(ctx, qtx, userID, *explicit)
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		judgeDec = dec
		if s.judgeDecidedHook != nil {
			s.judgeDecidedHook(ctx)
		}
		if dec.Vendor && *explicit == HarnessClaude {
			if err := requireJudgeAnthropicDefault(ctx, qtx, userID); err != nil {
				return store.Run{}, resolvedHarness{}, err
			}
		}
	}
	providerJudge := origin.JudgeLane && !judgeDec.Vendor
	if inheritedProvider {
		// A successor of a provider-bound run uses the SOURCE RUN's binding and never consults the
		// owner's current source (PRD #4320 inheritance).
		run, res, err := s.createInheritedProviderRun(ctx, tx, qtx, userID, explicit, insert, *inherit)
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		if err := s.beforeCommit(run); err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		return run, res, nil
	}
	// A successor of a vendor run resolves vendor credentials as today, WITHOUT the owner's
	// provider source (vendorOnly): the source run's lineage, not the owner's setting, decides.
	// A guarded vendor lane (job, cross-check) does the same: the guard above allowed its source
	// snapshot, and a vendor lane insert stamps no binding, so a second source read here could only
	// see a source selected in between and then fail the binding assertion as an internal mismatch.
	// A vendor judge needs vendorOnly for a different reason: its pin policy can choose vendor while
	// a Claude source IS selected (pinned with a credential), so the resolver must not consult the
	// source. A provider judge never reaches resolve(): it stamps all five binding columns from the
	// source frozen under the shared lock (freezeProviderJudge).
	vendorOnly := inherit != nil || origin.RefuseProviderOwner || origin.JudgeLane
	resolve := func() (resolvedHarness, error) {
		return s.resolveRunHarnessMode(ctx, userID, explicit, qtx, vendorOnly)
	}

	// Re-read the credential/default facts and resolve D11 INSIDE the transaction that commits.
	// A provider judge never reaches the vendor resolver (its token checks would refuse an owner
	// with no vendor credential): it freezes the Claude source directly, under the locks above.
	var res resolvedHarness
	if providerJudge {
		frozen, ferr := s.freezeProviderJudge(ctx, tx, qtx, userID, judgeDec)
		if ferr != nil {
			return store.Run{}, resolvedHarness{}, ferr
		}
		res = resolvedHarness{Harness: HarnessClaude, ClaudeSourceSet: true, ProviderSource: &frozen}
	} else {
		res, err = resolve()
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
	}

	policy, err := qtx.GetHarnessCreationPolicy(ctx, userID)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	// The shared lock also covers an automatic decision (an unset default), whatever the overflow toggle.
	lockEligible := origin.eligible(policy) && (policy.HarnessOverflowEnabled || res.DefaultUnset)
	if lockEligible {
		if s.codexSelectHook != nil {
			s.codexSelectHook(ctx)
		}
		if err = store.LockSecretMutationShared(ctx, tx, userID); err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		if s.codexSelectLockedHook != nil {
			s.codexSelectLockedHook(ctx)
		}
		// Re-resolve after the mutation lock, including default-account promotions.
		res, err = resolve()
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
	}
	res, decisionPick, err := s.evaluateCreationOverflow(ctx, qtx, userID, explicit, res, policy, origin)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}

	// Issue #3719: a pool choice picks its alias HERE, under the user's shared secret-mutation
	// lock and BEFORE the run INSERT, so no run row is held while the lock is taken (the lock
	// order migrate.go's LockSecretMutation documents). The shared lock fences exactly what
	// every credential writer takes the exclusive lock for: pool consent, enablement and the
	// account link. Quarantine and reauth_required are written by the refresher without that
	// lock, so the alias fence re-reads them (codexPoolPickHolds) and drops a pick that
	// turned unusable.
	var pick codexPoolPick
	pooled := res.Harness == HarnessCodex && res.Codex != nil && res.Codex.Select == codexChoicePool
	if pooled && decisionPick == nil {
		if s.codexSelectHook != nil {
			s.codexSelectHook(ctx)
		}
		if lerr := store.LockSecretMutationShared(ctx, tx, userID); lerr != nil {
			return store.Run{}, resolvedHarness{}, fmt.Errorf("codex pool select: lock: %w", lerr)
		}
		if s.codexSelectLockedHook != nil {
			s.codexSelectLockedHook(ctx)
		}
		pick, err = s.selectCodexPoolAlias(ctx, qtx, userID, nil)
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		choice := *res.Codex
		choice.SecretID = pick.AliasID
		res.Codex = &choice
	}

	if decisionPick != nil {
		pick = *decisionPick
	}

	if res.ProviderSource != nil && !providerJudge {
		// Freeze: secret-mutation lock (shared) THEN the provider row's share lock, then re-read and
		// re-validate, so a rotation, edit, retire or URL change commits either before this read or
		// after this transaction. Replaces the pre-lock source with what is committed now.
		frozen, ferr := s.freezeProviderSource(ctx, tx, qtx, userID, res.Harness)
		if ferr != nil {
			return store.Run{}, resolvedHarness{}, ferr
		}
		res.ProviderSource = &frozen
	}
	run, err := insert(qtx, res)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	if err := s.verifyRunBinding(ctx, qtx, run, res); err != nil {
		return store.Run{}, resolvedHarness{}, err
	}

	if decisionPick != nil {
		d := res.Decision
		if s.codexSelectBeforeFenceHook != nil {
			s.codexSelectBeforeFenceHook(ctx, pick.AliasID)
		}
		fenced, fencedOK, ferr := s.fenceCreationPick(ctx, qtx, userID, d, pick)
		if ferr != nil {
			return store.Run{}, resolvedHarness{}, ferr
		}
		if isAutomaticReason(d.Reason) {
			// A Codex state change at the fence maps the Codex side to unknown; a changed
			// harness is refused so the rollback undoes the INSERT.
			if err := s.recheckAutomaticDecision(d, res, fenced, fencedOK); err != nil {
				return store.Run{}, resolvedHarness{}, err
			}
		} else {
			ce := codexOverflowEvidence(fenced.Candidate, fenced.Reason, d.Preferred == HarnessClaude)
			if d.Preferred == HarnessCodex {
				d.PreferredEvidence = ce
			} else {
				d.OtherEvidence = ce
			}
			d.Reason = overflowReason(true, true, d.PreferredEvidence, d.OtherEvidence)
			if d.chosen() != res.Harness {
				return store.Run{}, resolvedHarness{}, errHarnessOverflowFence
			}
		}
		if fencedOK {
			pick = fenced
		}
		if res.Harness == HarnessCodex {
			choice := *res.Codex
			choice.SecretID = pick.AliasID
			res.Codex = &choice
		}
	}

	if res.Harness == HarnessCodex && res.ProviderSource == nil {
		// resolveRunHarness returns Codex only with a populated choice; guard defensively so a
		// disagreement never freezes a Codex run with no credential.
		if res.Codex == nil {
			return store.Run{}, resolvedHarness{}, errNoUsableCredential
		}
		if pooled && decisionPick == nil {
			if s.codexSelectBeforeFenceHook != nil {
				s.codexSelectBeforeFenceHook(ctx, pick.AliasID)
			}
			// The consent and link fence: re-validate the pick under a share lock on its alias
			// row, after the INSERT (users KEY SHARE, then the alias row, then the credential
			// state: every writer's order). The shared lock does not stop the refresher's
			// quarantine or reauth writes, so a failure here is an expected path for those; the
			// alias is dropped and the pool re-selected. The fence reads coord_state and reauth
			// unlocked, so a quarantine or reauth committed between the fence and this commit can
			// still bind. Claim-time admission (codex_operation_admission.go refuses a quarantined
			// account) backs up quarantine ONLY; a reauth flagged between the fence and the commit
			// surfaces at the run's first token use.
			pick, err = s.fenceCodexPoolAlias(ctx, qtx, userID, pick)
			if err != nil {
				return store.Run{}, resolvedHarness{}, err
			}
			choice := *res.Codex
			choice.SecretID = pick.AliasID
			res.Codex = &choice
		}
		freeze := s.freezeCodexBinding
		if s.codexFreezeFn != nil {
			freeze = s.codexFreezeFn
		}
		if ferr := freeze(ctx, qtx, userID, run.ID, res.Codex.SecretID, res.Codex.AuthMode); ferr != nil {
			return store.Run{}, resolvedHarness{}, ferr
		}
		reason := codexselect.Reason(res.Codex.Select)
		if pooled || decisionPick != nil {
			if cerr := s.checkFrozenCodexAccount(ctx, qtx, userID, run.ID, pick); cerr != nil {
				return store.Run{}, resolvedHarness{}, cerr
			}
			reason = pick.Reason
		}
		n, rerr := qtx.SetRunCodexSelectReason(ctx, store.SetRunCodexSelectReasonParams{Reason: string(reason), ID: run.ID, UserID: userID})
		if rerr != nil {
			return store.Run{}, resolvedHarness{}, fmt.Errorf("codex select reason: %w", rerr)
		}
		if n != 1 {
			return store.Run{}, resolvedHarness{}, fmt.Errorf("codex select reason: wrote %d rows, want 1", n)
		}
		run.CodexSelectReason = pgtype.Text{String: string(reason), Valid: true}
	}

	if err := s.persistHarnessDecision(ctx, qtx, &run, res.Decision); err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	if err := s.beforeCommit(run); err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	return run, res, nil
}

// codexDefaultMeta resolves the user's default secret of one codex kind to its (id,
// label), mapping pgx.ErrNoRows to ok=false ("no default of this kind") rather than an
// error.
func codexDefaultMeta(ctx context.Context, q harnessResolverStore, userID uuid.UUID, kind string) (store.GetDefaultUserSecretMetaRow, bool, error) {
	row, err := q.GetDefaultUserSecretMeta(ctx, store.GetDefaultUserSecretMetaParams{
		UserID: userID,
		Kind:   kind,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.GetDefaultUserSecretMetaRow{}, false, nil
		}
		return store.GetDefaultUserSecretMetaRow{}, false, fmt.Errorf("harness resolve: read default %s: %w", kind, err)
	}
	return row, true, nil
}

// verifyRunBinding is the fail-closed assertion every creation ends with: the inserted row carries
// EXACTLY the resolution's binding (all NULL for a vendor run). An insert closure that forgot the
// five binding params (a sqlc.narg trap: unstamped = NULL) would otherwise silently turn a
// provider-sourced owner's run into a vendor run. For a provider run it also checks the lead model
// and every enabled template's model against the frozen map (provider_model_unmapped).
func (s *Service) verifyRunBinding(ctx context.Context, q *store.Queries, run store.Run, res resolvedHarness) error {
	if !bindingOf(res).equalsRun(run) {
		return errProviderBindingMismatch
	}
	if res.ProviderSource == nil {
		return nil
	}
	if run.Harness != res.ProviderSource.Engine || run.AnthropicSecretID.Valid || run.CodexSecretID.Valid || run.CodexAuthMode.Valid || run.CredentialOverrideSecretID.Valid {
		return errProviderBindingMismatch
	}
	if run.Kind == runkind.Judge {
		// A judge runs one model, not a lead plus templates: validate it strictly (PRD #4322 M1a).
		return s.checkProviderJudgeModel(ctx, q, run, *res.ProviderSource)
	}
	return s.checkProviderModels(ctx, q, run, *res.ProviderSource)
}

// providerLaneSourceReader reads both source selections in one owner snapshot.
type providerLaneSourceReader interface {
	GetUserProviderLaneSources(context.Context, uuid.UUID) (store.GetUserProviderLaneSourcesRow, error)
}

// refuseProviderLane uses selection presence only, before any credential work.
// A Claude source blocks both lanes; a Codex source additionally blocks the Codex lane.
func (s *Service) refuseProviderLane(ctx context.Context, q providerLaneSourceReader, userID uuid.UUID, lane Harness) error {
	if lane != HarnessClaude && lane != HarnessCodex {
		return ErrProviderLaneUnsupported
	}
	sources, err := q.GetUserProviderLaneSources(ctx, userID)
	if err != nil {
		return err
	}
	if sources.ClaudeProviderID.Valid || (lane == HarnessCodex && sources.CodexProviderID.Valid) {
		return ErrProviderLaneUnsupported
	}
	return nil
}

// judgeLanePolicyReader reads the judge's creation policy in one owner snapshot.
type judgeLanePolicyReader interface {
	GetUserJudgeLanePolicy(context.Context, uuid.UUID) (store.GetUserJudgeLanePolicyRow, error)
}

// judgeLanePolicy is the one read of the judge's authoritative creation decision (PRD #4322 M1a):
// the judge pin policy and both lane sources in a single statement. It is meant to run in the
// create transaction under the shared secret-mutation lock, and every caller goes through it so the
// store's caller pin for GetUserJudgeLanePolicy names exactly this file.
func judgeLanePolicy(ctx context.Context, q judgeLanePolicyReader, userID uuid.UUID) (store.GetUserJudgeLanePolicyRow, error) {
	return q.GetUserJudgeLanePolicy(ctx, userID)
}

// ErrJudgePolicyChanged: a provider judge's policy (its Claude source or its pin policy) read under
// the shared secret-mutation lock differs from the one its decision used. Every writer of those
// inputs holds the exclusive lock, so this is defence in depth against a lock-bypassing commit.
// The create is refused with no vendor fallback; the automatic path logs and records no skip, the
// manual path answers 409 judge_policy_changed.
var ErrJudgePolicyChanged = errors.New("judge_policy_changed")

// judgeLaneDecision is the judge's creation decision over one policy read (PRD #4322 M1a).
type judgeLaneDecision struct {
	// Vendor is true when the judge resolves vendor credentials only; false is the provider branch
	// (a Claude judge bound to the owner's Claude source).
	Vendor bool
	// Policy is the read the decision was taken over, kept to compare against the locked re-read.
	Policy store.GetUserJudgeLanePolicyRow
}

// decideJudgeLane is the pure judge lane decision:
//   - Codex judge: a Codex source refuses (ErrProviderLaneUnsupported, until #4574); else vendor.
//   - Claude judge: pinned to a credential (mode 'pinned' with a non-NULL secret, as judgeChoice
//     reads it) is vendor despite any source; otherwise no Claude source is vendor, and a Claude
//     source is the provider branch.
//
// A harness other than Claude or Codex is ErrProviderLaneUnsupported.
func decideJudgeLane(h Harness, p store.GetUserJudgeLanePolicyRow) (judgeLaneDecision, error) {
	switch h {
	case HarnessCodex:
		if p.CodexProviderID.Valid {
			return judgeLaneDecision{}, ErrProviderLaneUnsupported
		}
		return judgeLaneDecision{Vendor: true, Policy: p}, nil
	case HarnessClaude:
		pinned := p.JudgeAnthropicBindMode == BindModePinned && p.JudgeAnthropicSecretID.Valid
		return judgeLaneDecision{Vendor: pinned || !p.ClaudeProviderID.Valid, Policy: p}, nil
	default:
		return judgeLaneDecision{}, ErrProviderLaneUnsupported
	}
}

// decideJudgeLaneFor reads the policy through judgeLanePolicy and decides. Callers in the create
// transaction hold the shared secret-mutation lock; the no-transaction and fake-store branches
// have none and refuse the provider branch (errProviderCreateRequiresTx) before it freezes anything.
func decideJudgeLaneFor(ctx context.Context, q judgeLanePolicyReader, userID uuid.UUID, h Harness) (judgeLaneDecision, error) {
	if h != HarnessClaude && h != HarnessCodex {
		return judgeLaneDecision{}, ErrProviderLaneUnsupported
	}
	p, err := judgeLanePolicy(ctx, q, userID)
	if err != nil {
		return judgeLaneDecision{}, err
	}
	return decideJudgeLane(h, p)
}

// judgeTokenReader is the default-credential presence read of the vendor Claude judge branch.
type judgeTokenReader interface {
	GetUserSecretCiphertext(context.Context, store.GetUserSecretCiphertextParams) (store.GetUserSecretCiphertextRow, error)
}

// requireJudgeAnthropicDefault is the vendor Claude judge's presence check: the owner has a default
// Anthropic token row (an enabled or disabled one; the resolver classifies that). No row is
// ErrNoAnthropicToken, which rolls the create back so the automatic path can record its
// no_usable_credential skip afterwards and the manual path can answer 422.
func requireJudgeAnthropicDefault(ctx context.Context, q judgeTokenReader, userID uuid.UUID) error {
	if _, err := q.GetUserSecretCiphertext(ctx, store.GetUserSecretCiphertextParams{UserID: userID, Kind: store.KindAnthropicToken}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoAnthropicToken
		}
		return err
	}
	return nil
}

// judgePolicyDrift compares a provider judge's frozen source and a locked re-read of its pin policy
// with its decision. Precedence: the frozen provider id first, then the pin policy.
func judgePolicyDrift(dec judgeLaneDecision, frozen uuid.UUID, now store.GetUserJudgeLanePolicyRow) error {
	if !dec.Policy.ClaudeProviderID.Valid || uuid.UUID(dec.Policy.ClaudeProviderID.Bytes) != frozen {
		return ErrJudgePolicyChanged
	}
	if now.JudgeAnthropicBindMode != dec.Policy.JudgeAnthropicBindMode || now.JudgeAnthropicSecretID != dec.Policy.JudgeAnthropicSecretID {
		return ErrJudgePolicyChanged
	}
	return nil
}

// judgeLaneNonAtomic is the judge decision for the creation paths that have no transaction (no
// beginner wired, or the in-memory fake store), so no secret-mutation lock: a vendor decision
// proceeds (with the vendor Claude judge's default-token presence check), and the provider branch
// fails closed with errProviderCreateRequiresTx since it cannot freeze under the lock.
func (s *Service) judgeLaneNonAtomic(ctx context.Context, userID uuid.UUID, h Harness) error {
	pr, ok := s.q.(judgeLanePolicyReader)
	if !ok {
		return errHarnessStoreUnavailable
	}
	dec, err := decideJudgeLaneFor(ctx, pr, userID, h)
	if err != nil {
		return err
	}
	if !dec.Vendor {
		return errProviderCreateRequiresTx
	}
	if h == HarnessClaude {
		tr, ok := s.q.(judgeTokenReader)
		if !ok {
			return errHarnessStoreUnavailable
		}
		return requireJudgeAnthropicDefault(ctx, tr, userID)
	}
	return nil
}

// freezeProviderJudge freezes a provider judge's source under the shared secret-mutation lock the
// caller already holds: freezeProviderSource takes the provider row's share lock and re-reads, so a
// cleared, unusable, invisible or retired source is ErrProviderSourceUnusable (typed where the freeze
// returns one). Only then is the frozen provider compared with the decision's, and the pin policy
// re-read and compared (ErrJudgePolicyChanged), in that precedence. It never touches the vendor
// credential resolver.
func (s *Service) freezeProviderJudge(ctx context.Context, tx store.DBTX, qtx *store.Queries, userID uuid.UUID, dec judgeLaneDecision) (providerSource, error) {
	frozen, err := s.freezeProviderSource(ctx, tx, qtx, userID, HarnessClaude)
	if err != nil {
		return providerSource{}, err
	}
	now, err := judgeLanePolicy(ctx, qtx, userID)
	if err != nil {
		return providerSource{}, err
	}
	if err := judgePolicyDrift(dec, frozen.ProviderID, now); err != nil {
		return providerSource{}, err
	}
	return frozen, nil
}

// createInheritedProviderRun creates a successor of a provider-bound run (mr_rework, task child,
// chat continuation): harness from the source run, the binding copied verbatim, the owner's
// CURRENT source never read. The copied binding is re-validated first: a rotated or disabled key is
// provider_binding_stale, a no-longer-allowlisted URL provider_destination_not_allowed.
func (s *Service) createInheritedProviderRun(ctx context.Context, tx store.DBTX, qtx *store.Queries, userID uuid.UUID, explicit *Harness, insert func(q *store.Queries, resolved resolvedHarness) (store.Run, error), src store.Run) (store.Run, resolvedHarness, error) {
	if src.UserID != userID || (src.Harness != string(HarnessClaude) && src.Harness != string(HarnessCodex)) || (explicit != nil && string(*explicit) != src.Harness) {
		return store.Run{}, resolvedHarness{}, errProviderBindingMismatch
	}
	bound, err := s.inheritProviderBinding(ctx, tx, qtx, src)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	res := resolvedHarness{Harness: Harness(src.Harness), ClaudeSourceSet: src.Harness == string(HarnessClaude), CodexSourceSet: src.Harness == string(HarnessCodex), ProviderSource: &bound}
	if bindingOf(res) != bindingOfRun(src) {
		return store.Run{}, resolvedHarness{}, errProviderBindingMismatch
	}
	run, err := insert(qtx, res)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	if err := s.verifyRunBinding(ctx, qtx, run, res); err != nil {
		return store.Run{}, resolvedHarness{}, err
	}
	return run, res, nil
}
