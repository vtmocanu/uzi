package workersvc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

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
//  4. otherwise (both usable, no usable default) Claude is chosen;
//  5. otherwise (neither usable) errNoUsableCredential.
func resolveHarness(in resolveHarnessInput) (Harness, error) {
	// (1) Explicit selection is authoritative and terminal — used if usable, else a
	// typed refusal with NO fallback to the other harness.
	if in.explicit != nil {
		h := *in.explicit
		if in.avail.usable(h) {
			return h, nil
		}
		return "", errNoCredentialForHarness
	}

	// (2) A usable user default wins next. A default set to a harness with no usable
	// credential is skipped so the resolver falls through to the availability rules,
	// rather than refusing.
	if in.userDefault != nil && in.avail.usable(*in.userDefault) {
		return *in.userDefault, nil
	}

	// (3)-(5) Availability rules.
	switch {
	case in.avail.claudeUsable && !in.avail.codexUsable:
		return HarnessClaude, nil // (3) sole Claude
	case in.avail.codexUsable && !in.avail.claudeUsable:
		return HarnessCodex, nil // (3) sole Codex
	case in.avail.claudeUsable && in.avail.codexUsable:
		return HarnessClaude, nil // (4) both usable, no usable default → Claude
	default:
		return "", errNoUsableCredential // (5) neither usable
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
}

// resolvedHarness is the full result of resolveRunHarness: the chosen harness and, when
// that harness is Codex, the selected Codex credential. Codex is nil for a Claude run.
type resolvedHarness struct {
	Harness Harness
	Codex   *codexCredentialChoice
}

// harnessResolverStore is the narrow C1-frozen read surface the store-backed resolver
// consumes. *store.Queries satisfies it; the production Service's s.q is a *store.Queries
// underneath, reached via harnessStore() below. Keeping it a local narrow interface
// (the same idiom codexauthz.go's codexAuthzStore uses) means this dark layer reuses
// ONLY existing C1 queries without widening the broad Store interface — GetUserDefaultHarness
// and CountCodexSecrets are C1 additions not on that interface, and this file must not edit it.
type harnessResolverStore interface {
	UserHasAnthropicToken(ctx context.Context, userID uuid.UUID) (bool, error)
	GetUserDefaultHarness(ctx context.Context, id uuid.UUID) (pgtype.Text, error)
	CountCodexSecrets(ctx context.Context, userID uuid.UUID) (int64, error)
	GetDefaultUserSecretMeta(ctx context.Context, arg store.GetDefaultUserSecretMetaParams) (store.GetDefaultUserSecretMetaRow, error)
	GetCodexCredentialState(ctx context.Context, arg store.GetCodexCredentialStateParams) (store.CodexCredentialState, error)
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
// Codex credential selection follows M1's named-default and auth-mode rules
// (resolveUsableCodexCredential): the user's single default codex credential decides both
// whether Codex is usable AND which credential a Codex run binds — so a failed or missing
// subscription default is simply "Codex not usable" and NEVER falls through to spend a
// non-default API key (D4). It acquires no lock or lease (D6): it is reads only, so two
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
	// Claude usability is exactly "does the user hold an Anthropic token", the same
	// door-check GET /api/me/rate-limits derives no_token from.
	claudeUsable, err := q.UserHasAnthropicToken(ctx, userID)
	if err != nil {
		return resolvedHarness{}, fmt.Errorf("harness resolve: anthropic token check: %w", err)
	}

	// Codex usability + the selected credential in one pass, so the fact that decided
	// availability is the exact credential a Codex run then binds (no re-read, no lock).
	codexChoice, codexUsable, err := s.resolveUsableCodexCredential(ctx, userID, q)
	if err != nil {
		return resolvedHarness{}, err
	}

	userDefault, err := s.userDefaultHarness(ctx, userID, q)
	if err != nil {
		return resolvedHarness{}, err
	}

	h, err := resolveHarness(resolveHarnessInput{
		explicit:    explicit,
		userDefault: userDefault,
		avail:       harnessAvailability{claudeUsable: claudeUsable, codexUsable: codexUsable},
	})
	if err != nil {
		return resolvedHarness{}, err
	}

	if h == HarnessCodex {
		// resolveHarness returns Codex only when codexUsable was true, so the choice is
		// populated. Guard defensively: an unpopulated choice would mean the two facts
		// disagreed, which must never ship a Codex run with no credential.
		if !codexUsable {
			return resolvedHarness{}, errNoUsableCredential
		}
		choice := codexChoice
		return resolvedHarness{Harness: HarnessCodex, Codex: &choice}, nil
	}
	return resolvedHarness{Harness: h}, nil
}

// userDefaultHarness reads users.default_harness and maps it to a *Harness preference:
// NULL (or any value outside the CHECK vocabulary, which cannot occur) is nil for "no
// preference".
func (s *Service) userDefaultHarness(ctx context.Context, userID uuid.UUID, q harnessResolverStore) (*Harness, error) {
	raw, err := q.GetUserDefaultHarness(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("harness resolve: read default harness: %w", err)
	}
	if !raw.Valid {
		return nil, nil
	}
	switch Harness(raw.String) {
	case HarnessClaude:
		h := HarnessClaude
		return &h, nil
	case HarnessCodex:
		h := HarnessCodex
		return &h, nil
	default:
		// The default_harness CHECK admits only claude/codex, so this is unreachable;
		// treat an unexpected value as "no preference" rather than trusting it.
		return nil, nil
	}
}

// resolveUsableCodexCredential resolves the user's single default codex credential and
// decides whether it is USABLE, returning the credential coordinates a Codex run would
// bind (PRD #1332 D4, M1's named-default rules).
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
	// Cheap "is Codex configured at all" guard. Zero codex credentials ⇒ no default to
	// select ⇒ Codex unusable, without the per-kind default lookups.
	n, err := q.CountCodexSecrets(ctx, userID)
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
	if ok {
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
// override against that EXACT harness inside the same tx via ResolveCredentialOverride(...,
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
func (s *Service) createRunAtomic(ctx context.Context, userID uuid.UUID, explicit *Harness, insert func(q *store.Queries, resolved resolvedHarness) (store.Run, error)) (store.Run, resolvedHarness, error) {
	if s.txBeginner == nil {
		// No transaction available. Resolve via the ambient queries to learn the harness:
		// Codex fails closed (it needs the atomic freeze); Claude keeps the cheap path.
		res, err := s.resolveRunHarness(ctx, userID, explicit)
		if err != nil {
			return store.Run{}, resolvedHarness{}, err
		}
		if res.Harness == HarnessCodex {
			return store.Run{}, resolvedHarness{}, errCodexCreateRequiresTx
		}
		q, ok := s.q.(*store.Queries)
		if !ok {
			return store.Run{}, resolvedHarness{}, errHarnessStoreUnavailable
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

	// Re-read the credential/default facts and resolve D11 INSIDE the transaction that commits.
	res, err := s.resolveRunHarnessQ(ctx, userID, explicit, qtx)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}

	run, err := insert(qtx, res)
	if err != nil {
		return store.Run{}, resolvedHarness{}, err
	}

	if res.Harness == HarnessCodex {
		// resolveRunHarness returns Codex only with a populated choice; guard defensively so a
		// disagreement never freezes a Codex run with no credential.
		if res.Codex == nil {
			return store.Run{}, resolvedHarness{}, errNoUsableCredential
		}
		freeze := s.freezeCodexBinding
		if s.codexFreezeFn != nil {
			freeze = s.codexFreezeFn
		}
		if ferr := freeze(ctx, qtx, userID, run.ID, res.Codex.SecretID, res.Codex.AuthMode); ferr != nil {
			return store.Run{}, resolvedHarness{}, ferr
		}
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
