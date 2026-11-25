package workersvc

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// These helper-only tests prove frozen persistence and key opening. Actual
// Service.Claim/reclaim is covered by codex_provider_actual_claim_livedb_test.go.
func restartCodexBindingService(f *codexBindingFix) {
	reads := &codexBindingReads{Queries: f.env.q}
	svc := New(reads, f.env.box, f.svc.p)
	svc.SetVault(f.env.vlt)
	svc.txBeginner = f.env.pool
	f.svc, f.reads = svc, reads
}

func selectCodexReclaimReplacement(t *testing.T, f *codexBindingFix) {
	t.Helper()
	seedCodexSource(t, f.provFix)
	sealed, err := f.env.box.Seal([]byte("inert-replacement-provider-key"))
	if err != nil {
		t.Fatal(err)
	}
	f.env.exec(`UPDATE user_secrets SET ciphertext=$2, sealed_with='master' WHERE id=$1`, f.key, sealed)
}

func codexReclaimWorker(f *codexBindingFix) store.Worker {
	return store.Worker{ID: f.workerID, UserID: f.userID, ProtocolCapabilities: []string{
		capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3,
		capability.ClaudeProviderV1,
	}}
}

func TestCodexProviderCreationCrashBeforeClaimPersistenceAssemblyLiveDB(t *testing.T) {
	f := newCodexBindingFix(t) // createRunAtomic has committed A; no claim was made.
	f.anthropic()
	f.codexDefault()
	a, keyA := f.provider, f.key
	selectCodexReclaimReplacement(t, f)
	b, keyB := f.provider, f.key
	if a == b || keyA == keyB {
		t.Fatal("replacement source must have distinct provider and key")
	}
	restartCodexBindingService(f)

	// Reload instead of retaining the creator's resolvedHarness or returned run.
	persisted := mustRun(t, f.env, f.run.ID)
	f.provider, f.key = a, keyA
	wantCodexBinding(t, f.provFix, persisted, 1, f.keyRevision)
	if persisted.Status != "queued" {
		t.Fatalf("unclaimed committed run status = %s", persisted.Status)
	}
	f.wantOpen(t, persisted, a, 1, "gateway")

	// Both vendor sources are available to creation on the restarted service.
	// Clearing the selection affects this new run only, not the committed A run.
	f.env.exec(`UPDATE users SET codex_provider_id=NULL WHERE id=$1`, f.userID)
	for _, harness := range []Harness{HarnessClaude, HarnessCodex} {
		_, resolved, err := f.svc.createRunAtomic(f.env.ctx, f.userID, &harness, f.insertAt(nil))
		if err != nil || resolved.ProviderSource != nil {
			t.Fatalf("vendor control %s: err=%v", harness, err)
		}
		if harness == HarnessCodex && resolved.Codex == nil {
			t.Fatal("Codex vendor control did not resolve an account")
		}
	}
	f.wantOpen(t, mustRun(t, f.env, f.run.ID), a, 1, "gateway")
}

func TestCodexProviderReclaimFrozenBindingPersistenceAssemblyLiveDB(t *testing.T) {
	for _, mutation := range []string{"intact", "revoked", "rotated"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCodexBindingFix(t)
			f.anthropic()
			f.codexDefault()
			a, keyA := f.provider, f.key
			// Open the binding only, discard service state, then reopen the DB row.
			f.wantOpen(t, mustRun(t, f.env, f.run.ID), a, 1, "gateway")
			selectCodexReclaimReplacement(t, f)
			f.provider, f.key = a, keyA
			switch mutation {
			case "revoked":
				f.env.exec(`UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, keyA)
			case "rotated":
				f.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, keyA)
			}
			restartCodexBindingService(f)
			persisted := mustRun(t, f.env, f.run.ID)
			wantCodexBinding(t, f.provFix, persisted, 1, f.keyRevision)
			if mutation == "intact" {
				f.wantOpen(t, persisted, a, 1, "gateway")
			} else {
				pb, err := f.svc.assembleProviderBinding(f.env.ctx, codexReclaimWorker(f), persisted)
				if pb != nil || !errors.Is(err, ErrProviderBindingStale) {
					t.Fatalf("stale binding present=%t err=%v", pb != nil, err)
				}
				if f.reads.cipherReads != 0 {
					t.Fatal("stale binding read ciphertext on restarted service")
				}
			}
			wantCodexBinding(t, f.provFix, mustRun(t, f.env, f.run.ID), 1, f.keyRevision)
		})
	}
}
