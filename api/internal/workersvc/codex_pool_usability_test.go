package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_pool_usability_test.go pins the issue #3719 usability rule for EVERY reader of Codex
// usability, against a fake store (no database): mode 'default' is today's rule untouched, and
// mode 'auto' is today's rule when no alias is pooled (pool_empty), and otherwise "usable iff at
// least one pooled alias is structurally eligible". Scores are never read here.

// poolFakeStore is the narrow harnessResolverStore surface over scripted facts. The embedded Store
// is nil: only the methods the readers under test call are implemented, so a reader that reached
// for anything else would panic and fail the test.
type poolFakeStore struct {
	Store
	mode          string
	pool          []store.ListCodexPoolAliasesRow
	hasAnthropic  bool
	totalCodex    int64
	enabledCodex  int64
	subDefault    *store.GetDefaultUserSecretMetaRow // codex_auth default
	subLinked     bool
	apiDefault    *store.GetDefaultUserSecretMetaRow // openai_api_key default
	poolReadCalls int
}

func (f *poolFakeStore) UserHasAnthropicToken(context.Context, uuid.UUID) (bool, error) {
	return f.hasAnthropic, nil
}
func (f *poolFakeStore) UserHasEnabledAnthropicToken(context.Context, uuid.UUID) (bool, error) {
	return f.hasAnthropic, nil
}
func (f *poolFakeStore) GetUserDefaultHarness(context.Context, uuid.UUID) (pgtype.Text, error) {
	return pgtype.Text{}, nil
}
func (f *poolFakeStore) CountCodexSecrets(context.Context, uuid.UUID) (int64, error) {
	return f.totalCodex, nil
}
func (f *poolFakeStore) CountEnabledCodexSecrets(context.Context, uuid.UUID) (int64, error) {
	return f.enabledCodex, nil
}
func (f *poolFakeStore) GetDefaultUserSecretMeta(_ context.Context, arg store.GetDefaultUserSecretMetaParams) (store.GetDefaultUserSecretMetaRow, error) {
	switch {
	case arg.Kind == store.KindCodexAuth && f.subDefault != nil:
		return *f.subDefault, nil
	case arg.Kind == store.KindOpenAIAPIKey && f.apiDefault != nil:
		return *f.apiDefault, nil
	}
	return store.GetDefaultUserSecretMetaRow{}, pgx.ErrNoRows
}
func (f *poolFakeStore) GetCodexCredentialState(_ context.Context, arg store.GetCodexCredentialStateParams) (store.CodexCredentialState, error) {
	st := store.CodexCredentialState{UserSecretID: arg.UserSecretID, UserID: arg.UserID, Status: "staging"}
	if f.subLinked {
		st.Status = codexStatusLinked
		st.ProviderAccountID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	}
	return st, nil
}
func (f *poolFakeStore) GetUserCodexAccountMode(context.Context, uuid.UUID) (string, error) {
	if f.mode == "" {
		return "default", nil
	}
	return f.mode, nil
}
func (f *poolFakeStore) ListCodexPoolAliases(context.Context, uuid.UUID) ([]store.ListCodexPoolAliasesRow, error) {
	f.poolReadCalls++
	return f.pool, nil
}

// GetPlanCrossCheck lets the cross-check gate validator reach its harness resolution.
func (f *poolFakeStore) GetPlanCrossCheck(context.Context, uuid.UUID) (store.CrossCheck, error) {
	return store.CrossCheck{}, pgx.ErrNoRows
}

func eligiblePoolRow() store.ListCodexPoolAliasesRow {
	return store.ListCodexPoolAliasesRow{
		AliasID:               uuid.New(),
		Enabled:               true,
		LinkStatus:            pgtype.Text{String: codexStatusLinked, Valid: true},
		ProviderAccountID:     pgtype.UUID{Bytes: uuid.New(), Valid: true},
		ProviderUserID:        pgtype.Text{String: "user-x", Valid: true},
		WorkspaceAccountID:    pgtype.Text{String: "acct-x", Valid: true},
		CoordState:            pgtype.Text{String: "idle", Valid: true},
		ReauthRequired:        pgtype.Bool{Bool: false, Valid: true},
		ThrottleSpreadWidthMs: pgtype.Int4{},
	}
}

func rowWith(mut func(*store.ListCodexPoolAliasesRow)) store.ListCodexPoolAliasesRow {
	r := eligiblePoolRow()
	mut(&r)
	return r
}

// scenario is one user's credential facts and the expected Codex usability and, when unusable, the
// explicit-Codex refusal.
type poolScenario struct {
	name        string
	fake        func() *poolFakeStore
	codexUsable bool
	wantSelect  codexChoiceKind // for a usable scenario
	refusal     error           // the explicit-Codex refusal when unusable
}

func subDefault() *store.GetDefaultUserSecretMetaRow {
	return &store.GetDefaultUserSecretMetaRow{ID: uuid.New(), Label: "sub"}
}

func poolScenarios() []poolScenario {
	usableSub := func(mode string, pool ...store.ListCodexPoolAliasesRow) *poolFakeStore {
		return &poolFakeStore{mode: mode, pool: pool, totalCodex: 1, enabledCodex: 1, subDefault: subDefault(), subLinked: true}
	}
	usableAPI := func(mode string) *poolFakeStore {
		return &poolFakeStore{mode: mode, totalCodex: 1, enabledCodex: 1, apiDefault: subDefault()}
	}
	unusableDefault := func(mode string, pool ...store.ListCodexPoolAliasesRow) *poolFakeStore {
		// A staging (unlinked) subscription default: enabled, but not usable.
		return &poolFakeStore{mode: mode, pool: pool, totalCodex: 1, enabledCodex: 1, subDefault: subDefault()}
	}
	return []poolScenario{
		// Mode default: today's rule, regardless of any pooled alias.
		{"default/usable subscription default", func() *poolFakeStore { return usableSub("default") }, true, codexChoiceDefault, nil},
		{"default/usable api-key default", func() *poolFakeStore { return usableAPI("default") }, true, codexChoiceDefault, nil},
		{"default/unusable default", func() *poolFakeStore { return unusableDefault("default") }, false, "", errNoCredentialForHarness},
		{"default/unusable default, eligible pooled alias is ignored", func() *poolFakeStore {
			return unusableDefault("default", eligiblePoolRow())
		}, false, "", errNoCredentialForHarness},
		{"default/usable default, pool is never read", func() *poolFakeStore { return usableSub("default", eligiblePoolRow()) }, true, codexChoiceDefault, nil},
		{"default/no codex", func() *poolFakeStore { return &poolFakeStore{mode: "default"} }, false, "", errNoCredentialForHarness},
		{"default/all disabled", func() *poolFakeStore {
			return &poolFakeStore{mode: "default", totalCodex: 2, enabledCodex: 0}
		}, false, "", errHarnessCredentialDisabled},

		// Mode auto, nothing pooled: today's usability, marked pool_empty.
		{"auto/empty pool + usable subscription default", func() *poolFakeStore { return usableSub("auto") }, true, codexChoicePoolEmpty, nil},
		{"auto/empty pool + usable api-key default", func() *poolFakeStore { return usableAPI("auto") }, true, codexChoicePoolEmpty, nil},
		{"auto/empty pool + unusable default", func() *poolFakeStore { return unusableDefault("auto") }, false, "", errNoCredentialForHarness},

		// Mode auto, pool configured.
		{"auto/one eligible pooled alias", func() *poolFakeStore {
			f := unusableDefault("auto", eligiblePoolRow())
			return f
		}, true, codexChoicePool, nil},
		{"auto/eligible pooled alias beside an unusable default", func() *poolFakeStore {
			return unusableDefault("auto", rowWith(func(r *store.ListCodexPoolAliasesRow) { r.Enabled = false }), eligiblePoolRow())
		}, true, codexChoicePool, nil},
		{"auto/pool all disabled, other codex credential enabled", func() *poolFakeStore {
			f := usableSub("auto", rowWith(func(r *store.ListCodexPoolAliasesRow) { r.Enabled = false }))
			f.totalCodex, f.enabledCodex = 2, 1
			return f
		}, false, "", ErrCodexPoolUnavailable},
		{"auto/pool alias unlinked", func() *poolFakeStore {
			return usableSub("auto", rowWith(func(r *store.ListCodexPoolAliasesRow) {
				r.LinkStatus = pgtype.Text{String: "staging", Valid: true}
				r.ProviderAccountID = pgtype.UUID{}
			}))
		}, false, "", ErrCodexPoolUnavailable},
		{"auto/pool account quarantined", func() *poolFakeStore {
			return usableSub("auto", rowWith(func(r *store.ListCodexPoolAliasesRow) {
				r.CoordState = pgtype.Text{String: codexCoordQuarantined, Valid: true}
			}))
		}, false, "", ErrCodexPoolUnavailable},
		{"auto/pool account needs reauth", func() *poolFakeStore {
			return usableSub("auto", rowWith(func(r *store.ListCodexPoolAliasesRow) {
				r.ReauthRequired = pgtype.Bool{Bool: true, Valid: true}
			}))
		}, false, "", ErrCodexPoolUnavailable},
		{"auto/every codex credential disabled keeps the disabled refusal", func() *poolFakeStore {
			return &poolFakeStore{mode: "auto", totalCodex: 1, enabledCodex: 0,
				pool: []store.ListCodexPoolAliasesRow{rowWith(func(r *store.ListCodexPoolAliasesRow) { r.Enabled = false })}}
		}, false, "", errHarnessCredentialDisabled},
	}
}

// TestCodexUsabilityMatrix drives every reader of Codex usability over every scenario.
//
// MUTATION: make resolveUsableCodexCredential ignore the mode (always take the default path):
// every auto/configured scenario flips and this test fails. Skip the structural check
// (codexPoolRowStructural -> true): the unlinked, quarantined and reauth scenarios flip.
func TestCodexUsabilityMatrix(t *testing.T) {
	ctx := context.Background()
	userID := uuid.New()
	for _, sc := range poolScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			build := func(claude bool) (*Service, *poolFakeStore) {
				f := sc.fake()
				f.hasAnthropic = claude
				return &Service{q: f}, f
			}

			// resolveUsableCodexCredential itself.
			svc, f := build(false)
			choice, usable, err := svc.resolveUsableCodexCredential(ctx, userID, f)
			if err != nil || usable != sc.codexUsable {
				t.Fatalf("resolveUsableCodexCredential = (%+v, %t, %v), want usable=%t", choice, usable, err, sc.codexUsable)
			}
			if usable && choice.Select != sc.wantSelect {
				t.Fatalf("choice.Select = %q, want %q", choice.Select, sc.wantSelect)
			}
			if sc.wantSelect == codexChoiceDefault && f.mode == "default" && f.poolReadCalls != 0 {
				t.Fatalf("mode default read the pool %d times, want 0", f.poolReadCalls)
			}

			// resolveRunHarnessQ, implicit, with and without a Claude token.
			svc, f = build(false)
			res, err := svc.resolveRunHarnessQ(ctx, userID, nil, f)
			if sc.codexUsable {
				if err != nil || res.Harness != HarnessCodex || res.Codex == nil || res.Codex.Select != sc.wantSelect {
					t.Fatalf("implicit codex-only = (%+v, %v), want codex with select %q", res, err, sc.wantSelect)
				}
			} else if !errors.Is(err, errNoUsableCredential) {
				t.Fatalf("implicit codex-only unusable = (%+v, %v), want errNoUsableCredential", res, err)
			}
			svc, f = build(true)
			res, err = svc.resolveRunHarnessQ(ctx, userID, nil, f)
			if err != nil || res.Harness != HarnessClaude {
				t.Fatalf("implicit with a Claude token = (%+v, %v), want claude (the pool never outranks it)", res, err)
			}

			// Explicit Codex.
			svc, f = build(true)
			res, err = svc.resolveRunHarnessQ(ctx, userID, h(HarnessCodex), f)
			if sc.codexUsable {
				if err != nil || res.Harness != HarnessCodex {
					t.Fatalf("explicit codex = (%+v, %v), want codex", res, err)
				}
			} else {
				if !errors.Is(err, errNoCredentialForHarness) {
					t.Fatalf("explicit codex unusable err = %v, want a no-credential refusal", err)
				}
				if sc.refusal == ErrCodexPoolUnavailable && !errors.Is(err, errCodexPoolUnavailable) {
					t.Fatalf("explicit codex err = %v, want errCodexPoolUnavailable", err)
				}
				if sc.refusal != ErrCodexPoolUnavailable && errors.Is(err, errCodexPoolUnavailable) {
					t.Fatalf("explicit codex err = %v, must NOT be the pool refusal here", err)
				}
				if errors.Is(sc.refusal, errHarnessCredentialDisabled) != errors.Is(err, errHarnessCredentialDisabled) {
					t.Fatalf("explicit codex err = %v, disabled-refusal precedence differs from want %v", err, sc.refusal)
				}
			}

			// BothHarnessesUsable.
			svc, _ = build(true)
			both, err := svc.BothHarnessesUsable(ctx, userID)
			if err != nil || both != sc.codexUsable {
				t.Fatalf("BothHarnessesUsable = (%t, %v), want %t", both, err, sc.codexUsable)
			}

			// ResolveSettingsHarness (no Claude token): Codex iff usable, else the Claude projection.
			svc, _ = build(false)
			hz, err := svc.ResolveSettingsHarness(ctx, userID)
			wantH := HarnessClaude
			if sc.codexUsable {
				wantH = HarnessCodex
			}
			if err != nil || hz != wantH {
				t.Fatalf("ResolveSettingsHarness = (%q, %v), want %q", hz, err, wantH)
			}

			// ResolveSettingsHarnessWithDefault proposing codex, with Claude usable too.
			svc, _ = build(true)
			hz, err = svc.ResolveSettingsHarnessWithDefault(ctx, userID, h(HarnessCodex))
			if err != nil || hz != wantH {
				t.Fatalf("ResolveSettingsHarnessWithDefault = (%q, %v), want %q", hz, err, wantH)
			}

			// ResolveHarnessForUser, explicit Codex.
			svc, _ = build(true)
			hz, err = svc.ResolveHarnessForUser(ctx, userID, h(HarnessCodex))
			if sc.codexUsable {
				if err != nil || hz != HarnessCodex {
					t.Fatalf("ResolveHarnessForUser = (%q, %v), want codex", hz, err)
				}
			} else if !errors.Is(err, ErrNoCredentialForHarness) {
				t.Fatalf("ResolveHarnessForUser err = %v, want a no-credential refusal", err)
			}

			// ScheduleCredentialDisabled: only the two "waits for the owner" refusals surface.
			svc, _ = build(true)
			err = svc.ScheduleCredentialDisabled(ctx, userID, nil, h(HarnessCodex))
			switch {
			case sc.codexUsable || sc.refusal == errNoCredentialForHarness:
				if err != nil {
					t.Fatalf("ScheduleCredentialDisabled = %v, want nil", err)
				}
			case sc.refusal == ErrCodexPoolUnavailable:
				if !errors.Is(err, ErrCodexPoolUnavailable) {
					t.Fatalf("ScheduleCredentialDisabled = %v, want ErrCodexPoolUnavailable", err)
				}
			default:
				if !errors.Is(err, ErrHarnessCredentialDisabled) {
					t.Fatalf("ScheduleCredentialDisabled = %v, want ErrHarnessCredentialDisabled", err)
				}
			}

			// Cross-check gate: a Claude lead's checker_unavailable attestation is accepted iff the
			// opposite (Codex) family is unusable for this user.
			svc, f = build(true)
			reason := "checker_unavailable"
			gerr := svc.validatePlanCrossCheckGateReason(ctx, f, store.Worker{}, store.Run{Harness: string(HarnessClaude), UserID: userID},
				StateRequest{PlanCrossCheckGateReason: &reason}, false)
			if sc.codexUsable != errors.Is(gerr, ErrInvalidState) || (!sc.codexUsable && gerr != nil) {
				t.Fatalf("cross-check checker_unavailable gate err = %v with codexUsable=%t (attested iff unusable)", gerr, sc.codexUsable)
			}
		})
	}
}
