package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Exercise the production entry point, not assembleProviderBinding. With no store,
// box or vault, any context/credential access panics; no payload can authorize spend.
func TestCodexProviderUnsupportedActualClaimBeforeReads(t *testing.T) {
	// The Codex judge is a supported lane since PRD #4574 M1; this worker lacks
	// codex_provider_judge_v1, so it is refused by the capability backstop, not by its kind.
	for _, kind := range []string{runkind.Judge, runkind.Job, runkind.CrossCheck, runkind.Chat} {
		t.Run(kind, func(t *testing.T) {
			run := providerBoundRow(kind)
			run.Harness = harnessCodex
			worker := store.Worker{ProtocolCapabilities: []string{capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}}
			payload, err := (&Service{}).assembleClaim(context.Background(), worker, run)
			if payload != nil || !errors.Is(err, errProviderLaneClaimRefused) {
				t.Fatalf("payload present=%v err=%v", payload != nil, err)
			}
		})
	}
	for _, engine := range []string{harnessCodex, "unknown"} {
		run := providerBoundRow(runkind.Issue)
		run.Harness = engine
		payload, err := (&Service{}).assembleClaim(context.Background(), store.Worker{}, run)
		want := ErrProviderLaneUnsupported
		if engine == "unknown" {
			want = errProviderLaneClaimRefused
		}
		if payload != nil || !errors.Is(err, want) {
			t.Fatalf("engine=%s payload present=%v err=%v", engine, payload != nil, err)
		}
	}
	run := providerBoundRow(runkind.Issue)
	run.Harness = harnessCodex
	run.EgressProfileID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
	if payload, err := (&Service{}).assembleClaim(context.Background(), store.Worker{}, run); payload != nil || !errors.Is(err, errIsolatedClaimRefused) {
		t.Fatalf("isolated payload present=%v err=%v", payload != nil, err)
	}
}

func TestCodexProviderCapabilityBeforeRead(t *testing.T) {
	s := &Service{q: bindingReadSpy{fakeStore: &fakeStore{}, t: t}}
	run := providerBoundRow(runkind.Issue)
	run.Harness = harnessCodex
	for _, caps := range [][]string{nil, {capability.ClaudeProviderV1}, {capability.CodexHarnessV1, capability.CodexRuntimeV2}, {capability.CodexProviderV1}, {capability.CodexProviderV1, capability.CodexHarnessV1}, {capability.CodexProviderV1, capability.CodexRuntimeV2}} {
		if _, err := s.assembleProviderBinding(context.Background(), store.Worker{ProtocolCapabilities: caps}, run); !errors.Is(err, ErrProviderLaneUnsupported) {
			t.Fatalf("caps %v: %v", caps, err)
		}
	}
}

type providerClaimMatrixStore struct {
	*fakeStore
	rev    store.GetModelProviderRevisionForOwnerRow
	key    store.GetProviderBindingKeyByIDRow
	cipher store.GetUserSecretCiphertextByIDRow
	opens  int
}

func (f *providerClaimMatrixStore) GetModelProviderRevisionForOwner(context.Context, store.GetModelProviderRevisionForOwnerParams) (store.GetModelProviderRevisionForOwnerRow, error) {
	return f.rev, nil
}
func (f *providerClaimMatrixStore) GetProviderBindingKeyByID(context.Context, store.GetProviderBindingKeyByIDParams) (store.GetProviderBindingKeyByIDRow, error) {
	return f.key, nil
}
func (f *providerClaimMatrixStore) GetUserSecretCiphertextByID(context.Context, store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	f.opens++
	return f.cipher, nil
}
func (f *providerClaimMatrixStore) GetUserVaultState(context.Context, uuid.UUID) (store.GetUserVaultStateRow, error) {
	return store.GetUserVaultStateRow{VaultMode: store.VaultModeUnprotected}, nil
}
func (f *providerClaimMatrixStore) LockProviderBindingKeyForShareNowait(context.Context, store.LockProviderBindingKeyForShareNowaitParams) (store.LockProviderBindingKeyForShareNowaitRow, error) {
	return store.LockProviderBindingKeyForShareNowaitRow(f.key), nil
}
func (f *providerClaimMatrixStore) LockProviderBindingRevisionForKeyShareNowait(context.Context, store.LockProviderBindingRevisionForKeyShareNowaitParams) (store.LockProviderBindingRevisionForKeyShareNowaitRow, error) {
	return store.LockProviderBindingRevisionForKeyShareNowaitRow(f.rev), nil
}

func TestProviderClaimEngineProfileAndLockedRecheck(t *testing.T) {
	for _, engine := range []string{harnessClaude, harnessCodex} {
		for _, defect := range []string{"", "engine", "protocol", "style", "models", "kind", "revision", "disabled", "rotated", "destination", "stale-before-destination"} {
			t.Run(engine+"/"+defect, func(t *testing.T) {
				run := providerBoundRow(runkind.Issue)
				run.Harness = engine
				protocol, models := providers.ProtocolAnth, pvDefaultMap
				caps := []string{capability.ClaudeProviderV1}
				if engine == harnessCodex {
					protocol = providers.ProtocolResponses
					models = `{"default":{"model":"gateway"}}`
					caps = []string{capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3}
				}
				f := &providerClaimMatrixStore{fakeStore: &fakeStore{}, rev: store.GetModelProviderRevisionForOwnerRow{ProviderID: uuid.UUID(run.ModelProviderID.Bytes), Revision: 1, Engine: engine, Protocol: protocol, AuthStyle: "bearer", BaseUrl: pvBase, ModelMap: []byte(models)}, key: store.GetProviderBindingKeyByIDRow{Kind: providerKeyKind(engine), ProviderID: run.ModelProviderID, EnablementRev: 1}}
				want := holdProviderBindingStale
				switch defect {
				case "":
					want = ""
				case "engine":
					f.rev.Engine = "other"
				case "protocol":
					f.rev.Protocol = "other"
				case "style":
					f.rev.AuthStyle = "other"
				case "models":
					f.rev.ModelMap = []byte(`{}`)
				case "kind":
					f.key.Kind = store.KindOpenAIAPIKey
				case "revision":
					f.rev.Revision++
				case "disabled":
					f.key.DisabledAt.Valid = true
				case "rotated":
					f.key.EnablementRev++
				case "destination":
					f.rev.BaseUrl = "https://denied.example/v1"
					want = holdProviderDestinationNotAllowed
				case "stale-before-destination":
					f.rev.BaseUrl = "https://denied.example/v1"
					f.key.EnablementRev++
				}
				box, err := secretbox.New(make([]byte, 32))
				if err != nil {
					t.Fatal(err)
				}
				plain := "provider-" + "fixture-key"
				sealed, err := box.Seal([]byte(plain))
				if err != nil {
					t.Fatal(err)
				}
				f.cipher = store.GetUserSecretCiphertextByIDRow{ID: uuid.UUID(run.ModelProviderKeySecretID.Bytes), UserID: run.UserID, Kind: providerKeyKind(engine), SealedWith: store.SealedWithMaster, Ciphertext: sealed}
				s := &Service{q: f, box: box, vlt: vault.New(box, nil, func(context.Context, func(context.Context, vault.Store) error) error {
					return errors.New("unexpected vault transaction")
				}), p: Params{ProviderAllowedBaseURLs: []string{pvBase}}}
				ctx := vault.WithStateReader(context.Background(), f)
				pb, err := s.assembleProviderBinding(ctx, store.Worker{ProtocolCapabilities: caps}, run)
				if want == "" {
					if err != nil || pb == nil || pb.Engine != engine || pb.Credential != plain {
						t.Fatalf("binding=%v err=%v", pb, err)
					}
					if f.opens != 1 {
						t.Fatalf("opens=%d", f.opens)
					}
				} else {
					var refusal *providerClaimRefusal
					if !errors.As(err, &refusal) || refusal.Reason != want {
						t.Fatalf("err=%v want=%s", err, want)
					}
					if f.opens != 0 {
						t.Fatalf("opened refused key %d times", f.opens)
					}
				}
				reason, err := s.lockedProviderRefusal(ctx, f, run)
				if err != nil || reason != want {
					t.Fatalf("locked reason=%s err=%v want=%s", reason, err, want)
				}
			})
		}
	}
}
