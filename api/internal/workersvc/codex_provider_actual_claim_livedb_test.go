package workersvc

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Keep pcFix.run's historical Claude tuple intact. Create a separate, valid
// Codex revision/key and freeze it on a committed run before recreating Service.
func newCodexActualClaimFix(t *testing.T) (*pcFix, uuid.UUID) {
	t.Helper()
	pf := newPCFix(t)
	p, err := pf.env.q.InsertModelProvider(pf.env.ctx, store.InsertModelProviderParams{
		OwnerUserID: pf.userID, Name: "actual-codex-" + uuid.NewString(), Engine: harnessCodex,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pf.env.q.InsertModelProviderRevision(pf.env.ctx, store.InsertModelProviderRevisionParams{
		ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase,
		AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`),
	}); err != nil {
		t.Fatal(err)
	}
	sealed, err := pf.env.box.Seal([]byte(pf.plain))
	if err != nil {
		t.Fatal(err)
	}
	key, err := pf.env.q.InsertProviderKeySecret(pf.env.ctx, store.InsertProviderKeySecretParams{
		UserID: pf.userID, Label: p.ID.String(), Ciphertext: sealed, SealedWith: "master",
		ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	pf.provider, pf.key = p.ID, key.ID
	pf.env.exec(`UPDATE users SET codex_provider_id=$2,default_harness='codex' WHERE id=$1`, pf.userID, pf.provider)
	pf.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, pf.workerID,
		[]string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3, capability.CodexProviderV1, capability.RepoAgentSourceV1})
	// A usable native credential remains available throughout every provider refusal.
	vendor := pf.env.seedStaticAPIKey(t, pf.userID, "available-native-"+uuid.NewString(), "inert-available-native-key")
	pf.env.exec(`UPDATE user_secrets SET is_default=true WHERE id=$1`, vendor)
	created, err := pf.svc.CreateTaskRun(pf.env.ctx, pf.userID, pf.repoID,
		"Actual provider claim", "main", false, false, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	if created.DispatchedAt.Valid {
		t.Fatal("new task must remain undispatched until explicit dispatch")
	}
	dispatched, err := pf.svc.DispatchTaskRun(pf.env.ctx, pf.userID, id)
	if err != nil || !dispatched.DispatchedAt.Valid || bindingOfRun(dispatched) != bindingOfRun(created) {
		t.Fatalf("dispatch did not preserve the created binding: err=%v", err)
	}
	frozen := mustRun(t, pf.env, id)
	if !frozen.RepoAgentSourceRequired {
		t.Fatal("real creation must retain the repository-source protocol fence")
	}
	if frozen.Harness != harnessCodex || !frozen.ModelProviderID.Valid ||
		uuid.UUID(frozen.ModelProviderID.Bytes) != pf.provider {
		t.Fatal("creation did not commit provider A")
	}
	t.Cleanup(func() {
		_, _ = pf.env.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id=NULL WHERE id=$1`, pf.userID)
	})
	return pf, id
}

func restartCodexActualClaimService(pf *pcFix) {
	svc := New(pf.env.q, pf.env.box, pf.svc.p)
	svc.SetVault(pf.env.vlt)
	svc.SetTxBeginner(pf.env.pool)
	pf.svc = svc
}

func wantCodexActualProviderClaim(t *testing.T, pf *pcFix, id uuid.UUID) store.Run {
	t.Helper()
	payload, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || payload == nil {
		t.Fatalf("Claim payload present=%t err=%v", payload != nil, err)
	}
	p := payload.Secrets.Provider
	if payload.RunID != id.String() || p == nil || payload.Secrets.Codex != nil || payload.Secrets.AnthropicOAuthToken != "" {
		t.Fatal("Claim did not deliver the requested exclusive provider run")
	}
	if p.ProviderID != pf.provider.String() || p.Engine != harnessCodex || p.Protocol != "openai_responses" ||
		p.Revision != 1 || p.BaseURL != pvBase || p.AuthStyle != "bearer" || p.ModelMap["default"].Model != "gateway" || p.Credential != pf.plain {
		t.Fatal("Claim did not preserve frozen provider A")
	}
	r := mustRun(t, pf.env, id)
	wantCodexActualNoVendorWrites(t, pf, r)
	return r
}

func wantCodexActualNoVendorWrites(t *testing.T, pf *pcFix, r store.Run) {
	t.Helper()
	pf.wantNoVendorWrites(t, r)
	// ClaimRun advances this revocation counter for every claim, including providers.
	// The counter alone is no authority: no secret, material, hash or journal may exist.
	if r.CodexSecretID.Valid || r.CodexAuthMode.Valid || r.CodexMaterialRevision.Valid || len(r.CodexCapHash) != 0 {
		t.Fatalf("vendor authority present: secret=%t auth=%t material=%t capability_hash=%t",
			r.CodexSecretID.Valid, r.CodexAuthMode.Valid, r.CodexMaterialRevision.Valid, len(r.CodexCapHash) != 0)
	}
	if r.CodexClaimEpoch < r.ClaimGeneration {
		t.Fatal("shared claim revocation fence regressed")
	}
}

func TestCodexProviderActualClaimAndReclaimLiveDB(t *testing.T) {
	for _, mutation := range []string{"intact", "revoked", "rotated", "missing-key", "destination"} {
		t.Run(mutation, func(t *testing.T) {
			pf, id := newCodexActualClaimFix(t)
			// B is a separate valid profile/key. The intact case leaves B selected;
			// the other cases clear it before first claim. Neither is authority for A.
			b, err := pf.env.q.InsertModelProvider(pf.env.ctx, store.InsertModelProviderParams{OwnerUserID: pf.userID, Name: "replacement-" + uuid.NewString(), Engine: harnessCodex})
			if err != nil {
				t.Fatal(err)
			}
			if err := pf.env.q.InsertModelProviderRevision(pf.env.ctx, store.InsertModelProviderRevisionParams{ProviderID: b.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"replacement"}}`)}); err != nil {
				t.Fatal(err)
			}
			sealed, err := pf.env.box.Seal([]byte("inert-replacement-key"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pf.env.q.InsertProviderKeySecret(pf.env.ctx, store.InsertProviderKeySecretParams{UserID: pf.userID, Label: b.ID.String(), Ciphertext: sealed, SealedWith: "master", ProviderID: pgtype.UUID{Bytes: b.ID, Valid: true}}); err != nil {
				t.Fatal(err)
			}
			pf.env.exec(`UPDATE users SET codex_provider_id=$2 WHERE id=$1`, pf.userID, b.ID)
			if mutation != "intact" {
				pf.env.exec(`UPDATE users SET codex_provider_id=NULL WHERE id=$1`, pf.userID)
			}
			restartCodexActualClaimService(pf)
			first := wantCodexActualProviderClaim(t, pf, id)
			// No worker process runs in this fixture. Settle that exact worker/generation
			// and requeue it, preserving custody and the frozen binding.
			pf.env.exec(`UPDATE runs SET status='queued',claim_released_at=now()
    WHERE id=$1 AND worker_id=$2 AND claim_generation=$3 AND status='claimed'`, id, pf.workerID, first.ClaimGeneration)
			switch mutation {
			case "revoked":
				pf.disable()
			case "rotated":
				pf.rotate()
			case "missing-key":
				pf.deleteKey()
			case "destination":
				pf.allow()
			}
			restartCodexActualClaimService(pf)
			if mutation == "intact" {
				again := wantCodexActualProviderClaim(t, pf, id)
				if again.ClaimGeneration != first.ClaimGeneration+1 {
					t.Fatal("reclaim did not advance generation")
				}
			} else {
				p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
				if err != nil || p != nil {
					t.Fatalf("refused reclaim payload present=%t err=%v", p != nil, err)
				}
				reason := holdProviderBindingStale
				if mutation == "destination" {
					reason = holdProviderDestinationNotAllowed
				}
				r := pf.wantParked(t, id, reason)
				wantCodexActualNoVendorWrites(t, pf, r)
			}
			if bindingOfRun(mustRun(t, pf.env, id)) != bindingOfRun(first) {
				t.Fatal("claim or refusal rewrote the frozen tuple")
			}
		})
	}
}

func TestCodexProviderActualClaimRotationBetweenAssemblyAndFinishLiveDB(t *testing.T) {
	pf, id := newCodexActualClaimFix(t)
	restartCodexActualClaimService(pf)
	pf.svc.claimHooks = &claimTestHooks{afterAssembly: func(_ context.Context, _ store.Run, p *ClaimPayload, err error) {
		if err != nil || p == nil || p.Secrets.Provider == nil || p.Secrets.Codex != nil {
			t.Fatal("rotation hook requires successful provider assembly")
		}
		pf.rotate()
	}}
	p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || p != nil {
		t.Fatalf("Claim payload present=%t err=%v", p != nil, err)
	}
	wantCodexActualNoVendorWrites(t, pf, pf.wantParked(t, id, holdProviderBindingStale))
}

func TestCodexProviderActualClaimNativeVendorControlLiveDB(t *testing.T) {
	pf, _ := newCodexActualClaimFix(t)
	// Remove the queued provider candidate from admission without claiming it.
	pf.env.exec(`UPDATE runs SET status='completed' WHERE user_id=$1`, pf.userID)
	pf.env.exec(`UPDATE users SET codex_provider_id=NULL WHERE id=$1`, pf.userID)
	alias := pf.env.seedStaticAPIKey(t, pf.userID, "native-"+uuid.NewString(), "inert-native-api-key")
	id := pf.queuedRun(t, "issue")
	if err := pf.svc.FreezeCodexBinding(pf.env.ctx, pf.userID, id, alias, codexAuthModeAPIKey); err != nil {
		t.Fatal(err)
	}
	restartCodexActualClaimService(pf)
	p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
	if err != nil || p == nil {
		t.Fatalf("vendor Claim payload present=%t err=%v", p != nil, err)
	}
	if p.RunID != id.String() || p.Secrets.Codex == nil || p.Secrets.Codex.Capability == "" || p.Secrets.Provider != nil {
		t.Fatal("native vendor claim lost its exclusive capability")
	}
}
