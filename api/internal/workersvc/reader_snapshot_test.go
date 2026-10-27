package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// readerClaimStore exercises API-key claim assembly without account integration.
type readerClaimStore struct {
	*fakeStore
	codexAuthzStore
	auth      store.GetRunCodexAuthContextRow
	lookupErr error
}

func (s *readerClaimStore) GetRunCodexAuthContext(context.Context, uuid.UUID) (store.GetRunCodexAuthContextRow, error) {
	return s.auth, nil
}
func (s *readerClaimStore) SetRunCodexClaimCapability(_ context.Context, p store.SetRunCodexClaimCapabilityParams) (int64, error) {
	s.auth.CodexClaimEpoch++
	s.auth.CodexCapHash = p.Hash
	return s.auth.CodexClaimEpoch, nil
}
func (s *readerClaimStore) GetUserSecretCiphertextByID(ctx context.Context, p store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	if s.lookupErr != nil {
		return store.GetUserSecretCiphertextByIDRow{}, s.lookupErr
	}
	return s.fakeStore.GetUserSecretCiphertextByID(ctx, p)
}

func TestReaderInfrastructureThroughClaimAssembly(t *testing.T) {
	for _, lookup := range []bool{false, true} {
		t.Run(map[bool]string{false: "vault configuration", true: "lookup"}[lookup], func(t *testing.T) {
			f := newAutoFixture(t)
			run := f.fs.claimRun
			run.Harness = harnessCodex
			run.CodexAuthMode = pgconv.TextOrNull(codexAuthModeAPIKey)
			run.CodexSecretID = pgconv.UUID(f.emptyID)
			run.CodexMaterialRevision = pgtype.Int8{Int64: 3, Valid: true}
			q := &readerClaimStore{fakeStore: f.fs, auth: store.GetRunCodexAuthContextRow{
				Status: "claimed", WorkerID: pgconv.UUID(f.worker.ID), ClaimGeneration: run.ClaimGeneration,
				CodexAuthMode: run.CodexAuthMode, BoundKind: store.KindOpenAIAPIKey,
				CodexMaterialRevision: run.CodexMaterialRevision, CurrentMaterialRevision: 3,
			}}
			row := f.fs.byIDSecrets[f.emptyID]
			row.ID, row.Kind = f.emptyID, store.KindOpenAIAPIKey
			f.fs.byIDSecrets[f.emptyID] = row
			expected := vault.ErrNotConfigured
			if lookup {
				expected = errors.New("snapshot lookup unavailable")
				q.lookupErr = expected
			}
			svc := New(q, f.svc.box, autoParams()) // nil vault intentionally exercises configuration failure
			payload, err := svc.assembleClaim(context.Background(), f.worker, run)
			if payload != nil || !errors.Is(err, expected) || errors.Is(err, errCredentialUnavailable) {
				t.Fatalf("reader infrastructure misclassified: payload-present=%v err=%v", payload != nil, err)
			}
			var minted *codexMintedClaimError
			if !errors.As(err, &minted) || minted.epoch != 1 || len(minted.hash) == 0 {
				t.Fatal("lost minted claim settlement authority")
			}
			if len(f.fs.recordedCreds) != 0 || len(f.fs.autoCandidateLookups) != 0 {
				t.Fatal("reader failure spent another credential")
			}
		})
	}
}

func TestReaderStaleThroughClaimAssemblyRequeuesExactClaim(t *testing.T) {
	f := newAutoFixture(t)
	// Stage a coherent selected credential, but a newer durable crypto epoch.
	// Refetching the same old snapshot twice must exhaust the bounded retry.
	row := f.fs.byIDSecrets[f.emptyID]
	row.ID, row.VaultEpoch = f.emptyID, 7
	f.fs.byIDSecrets[f.emptyID] = row
	m := newMemVaultStore()
	m.states[f.owner] = store.LockVaultAuthStateRow{VaultMode: store.VaultModeProtected, VaultEpoch: 9}
	f.svc.SetVault(newMemoryVault(f.svc.box, m))
	f.fs.claimRun.ClaimGeneration = 3
	payload, err := f.svc.assembleClaim(context.Background(), f.worker, f.fs.claimRun)
	if payload != nil || err != store.ErrVaultSealStale {
		t.Fatalf("assembly: payload-present=%v error=%v", payload != nil, err)
	}
	if errors.Is(err, errCredentialUnavailable) || errors.Is(err, errVaultLocked) {
		t.Fatal("stale changed classification")
	}
	if len(f.fs.byIDLookups) != 2 {
		t.Fatalf("lookups=%d", len(f.fs.byIDLookups))
	}
	for _, p := range f.fs.byIDLookups {
		if p.ID != f.emptyID || p.UserID != f.owner {
			t.Fatal("crypto retry changed pooled selection")
		}
	}
	payload, err = f.svc.finishRunClaim(context.Background(), f.fs.claimRun, payload, err, claimRecoveryIdentity{workerID: f.worker.ID})
	if payload != nil || err != nil {
		t.Fatalf("finish: payload-present=%v error=%v", payload != nil, err)
	}
	p := f.fs.claimRequeued
	if p == nil || p.ID != f.fs.claimRun.ID || p.WorkerID != pgconv.UUID(f.worker.ID) || p.ClaimGeneration != 3 || p.PoolWait {
		t.Fatal("stale did not settle exact claim as queued")
	}
	if f.fs.claimFailed != nil || f.fs.claimParked != nil || f.fs.claimCredParked != nil || f.fs.markedFailed != nil || f.fs.requeuedRun != nil {
		t.Fatal("stale used a terminal/park/unfenced settlement")
	}
}

func TestReaderStaleFinishRetainsSettlementFences(t *testing.T) {
	for _, skew := range []string{"generation", "worker", "cap epoch", "cap hash", "status"} {
		t.Run(skew, func(t *testing.T) {
			fs, svc, run, id := finishFixture(t)
			locked := run
			locked.Status = "claimed"
			locked.WorkerID = pgconv.UUID(id.workerID)
			switch skew {
			case "generation":
				locked.ClaimGeneration++
			case "worker":
				locked.WorkerID = pgtype.UUID{}
			case "cap epoch":
				locked.CodexClaimEpoch++
			case "cap hash":
				locked.CodexCapHash = []byte("new capability hash")
			case "status":
				locked.Status = "running"
			}
			fs.claimFinishLocked = &locked
			got, err := svc.finishRunClaim(context.Background(), run, nil, store.ErrVaultSealStale, id)
			if got != nil || err != nil {
				t.Fatalf("finish: payload-present=%v error=%v", got != nil, err)
			}
			if fs.claimRequeued != nil || fs.claimFailed != nil || fs.claimReleased != 0 {
				t.Fatal("stale settled superseded claim")
			}
		})
	}
}
