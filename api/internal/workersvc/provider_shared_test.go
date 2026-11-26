package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #4324 corrupt or inaccessible provider references, through fakes where the database constraints
// forbid planting them. Every case must read as missing, stale or invalid and never reach a vault open
// or a dial. The nil-embedding fakes panic on a method they do not implement; the ciphertext lookup
// is implemented by fakeStore, so the claim test asserts its byIDLookups stays empty instead.

// A selection whose definition the caller cannot see comes back from GetUserProviderSource with the
// setting present and every provider column NULL (the visibility join found nothing): provider_missing
// for both engines, never a vendor fallback.
func TestSelectionOfAnInvisibleDefinitionIsMissing(t *testing.T) {
	pid := uuid.New()
	row := store.GetUserProviderSourceRow{ProviderID: pgtype.UUID{Bytes: pid, Valid: true}}
	for _, engine := range []string{providers.EngineClaude, providers.EngineCodex} {
		got := EvaluateProviderSource(engine, row, allowOnly("https://gw.example.com/a"))
		if got.State != ProviderSourceUnusable || got.Reason != SourceUnusableProviderMissing || got.ProviderID != pid || got.Err() == nil {
			t.Errorf("%s: result = %+v, want unusable provider_missing", engine, got)
		}
	}
}

// A row whose provider engine is not the requested engine is
// invalid (a shared Claude definition read as a Codex source, or a Codex definition read as Claude).
func TestSharedDefinitionOfTheWrongEngineIsInvalid(t *testing.T) {
	pid := uuid.New()
	allow := allowOnly("https://gw.example.com/a")
	claudeRow := usableRow(pid)
	if got := EvaluateProviderSource(providers.EngineCodex, claudeRow, allow); got.State != ProviderSourceUnusable || got.Reason != SourceUnusableInvalid {
		t.Errorf("claude definition as a codex source = %+v, want provider_invalid", got)
	}
	codexRow := usableRow(pid)
	codexRow.Engine = pgtype.Text{String: providers.EngineCodex, Valid: true}
	if got := EvaluateProviderSource(providers.EngineClaude, codexRow, allow); got.State != ProviderSourceUnusable || got.Reason != SourceUnusableInvalid {
		t.Errorf("codex definition as a claude source = %+v, want provider_invalid", got)
	}
}

func boundRevisionRow(run store.Run, engine string) *store.GetModelProviderRevisionForOwnerRow {
	return &store.GetModelProviderRevisionForOwnerRow{
		ProviderID: uuid.UUID(run.ModelProviderID.Bytes), Revision: run.ModelProviderRevision.Int32,
		Protocol: "anthropic_messages", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(pvDefaultMap), Engine: engine,
	}
}

func boundKeyFacts(run store.Run, kind string) *providerKeyFacts {
	return &providerKeyFacts{Kind: kind, ProviderID: run.ModelProviderID, EnablementRev: run.ModelProviderKeyRev.Int64}
}

// providerBindingRefusal, which the claim precheck, the finisher and the hold promoter share, judges a
// binding whose revision or key does not belong to the run's engine stale, including a shared Claude
// definition under a Codex run and a Codex-engine revision under a Claude run.
func TestBindingRefusalOnCorruptEngineReferences(t *testing.T) {
	svc := New(&fakeStore{}, newBox(t), testParams())
	svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	claude := providerBoundRow(runkind.Issue)
	if got := svc.providerBindingRefusal(claude, boundRevisionRow(claude, providers.EngineClaude), boundKeyFacts(claude, store.KindAnthropicCompatibleKey)); got != "" {
		t.Fatalf("intact binding refused: %q", got)
	}
	codex := providerBoundRow(runkind.Issue)
	codex.Harness = "codex"
	cases := map[string]struct {
		run store.Run
		rev *store.GetModelProviderRevisionForOwnerRow
		key *providerKeyFacts
	}{
		"codex-engine revision under a claude run": {claude, boundRevisionRow(claude, providers.EngineCodex), boundKeyFacts(claude, store.KindAnthropicCompatibleKey)},
		"shared claude revision under a codex run": {codex, boundRevisionRow(codex, providers.EngineClaude), boundKeyFacts(codex, store.KindOpenAICompatibleKey)},
		"claude key under a codex run":             {codex, boundRevisionRow(codex, providers.EngineCodex), boundKeyFacts(codex, store.KindAnthropicCompatibleKey)},
		"key of another definition":                {claude, boundRevisionRow(claude, providers.EngineClaude), &providerKeyFacts{Kind: store.KindAnthropicCompatibleKey, ProviderID: pgtype.UUID{Bytes: uuid.New(), Valid: true}, EnablementRev: 1}},
		"revision that is not the frozen revision": {claude, func() *store.GetModelProviderRevisionForOwnerRow {
			r := boundRevisionRow(claude, providers.EngineClaude)
			r.Revision++
			return r
		}(), boundKeyFacts(claude, store.KindAnthropicCompatibleKey)},
		"missing revision (not visible to the caller)": {claude, nil, boundKeyFacts(claude, store.KindAnthropicCompatibleKey)},
		"missing key (not the caller's)":               {claude, boundRevisionRow(claude, providers.EngineClaude), nil},
	}
	for name, c := range cases {
		if got := svc.providerBindingRefusal(c.run, c.rev, c.key); got != holdProviderBindingStale {
			t.Errorf("%s: refusal = %q, want %q", name, got, holdProviderBindingStale)
		}
	}
}

// noRowsBindingStore answers every binding read as an invisible row, as the recipient-visibility SQL
// does for another user's private definition or key. The nil-embedding fake panics on a vault open.
type noRowsBindingStore struct{ *fakeStore }

func (noRowsBindingStore) GetModelProviderRevisionForOwner(context.Context, store.GetModelProviderRevisionForOwnerParams) (store.GetModelProviderRevisionForOwnerRow, error) {
	return store.GetModelProviderRevisionForOwnerRow{}, pgx.ErrNoRows
}

func (noRowsBindingStore) GetProviderBindingKeyByID(context.Context, store.GetProviderBindingKeyByIDParams) (store.GetProviderBindingKeyByIDRow, error) {
	return store.GetProviderBindingKeyByIDRow{}, pgx.ErrNoRows
}

// The claim of a run bound to a reference the run's user cannot see is a stale refusal, for a Claude
// run and for a Codex run bound to a shared Claude definition, with nothing opened.
func TestClaimOfAnInaccessibleBindingIsStaleWithoutOpening(t *testing.T) {
	fs := &fakeStore{}
	svc := New(noRowsBindingStore{fs}, newBox(t), testParams())
	svc.p.ProviderAllowedBaseURLs = []string{pvBase}
	for _, harness := range []string{"claude", "codex"} {
		run := providerBoundRow(runkind.Issue)
		run.Harness = harness
		wkr := store.Worker{ProtocolCapabilities: []string{capability.ClaudeProviderV1, capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}}
		_, err := svc.assembleProviderBinding(context.Background(), wkr, run)
		var refusal *providerClaimRefusal
		if !errors.As(err, &refusal) || refusal.Reason != holdProviderBindingStale || !errors.Is(err, ErrProviderBindingStale) {
			t.Errorf("%s: err = %v, want provider_binding_stale", harness, err)
		}
	}
	if len(fs.byIDLookups) != 0 {
		t.Fatalf("ciphertext lookups = %+v, want none for an inaccessible binding", fs.byIDLookups)
	}
}

// A run-name read that returns nothing for a run the user cannot see (the SQL join is the filter) is an
// empty map, not an error and not a guess.
type emptyNameStore struct{ *fakeStore }

func (emptyNameStore) ListRunProviderNames(context.Context, []uuid.UUID) ([]store.ListRunProviderNamesRow, error) {
	return nil, nil
}

func TestProviderNamesOmitRunsTheStoreDoesNotResolve(t *testing.T) {
	svc := New(emptyNameStore{&fakeStore{}}, newBox(t), testParams())
	got, err := svc.ProviderNamesForRuns(context.Background(), []uuid.UUID{uuid.New()})
	if err != nil || len(got) != 0 {
		t.Fatalf("names = (%v, %v), want an empty map", got, err)
	}
}
