package workersvc

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/providers"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// These are helper/storage proofs only. They never assemble or deliver an actual
// Codex claim, and the worker capabilities exist only on a local value.
type codexBindingReads struct {
	*store.Queries
	cipherReads int
}

func (q *codexBindingReads) GetUserSecretCiphertextByID(ctx context.Context, arg store.GetUserSecretCiphertextByIDParams) (store.GetUserSecretCiphertextByIDRow, error) {
	q.cipherReads++
	return q.Queries.GetUserSecretCiphertextByID(ctx, arg)
}

type codexBindingFix struct {
	*provFix
	reads       *codexBindingReads
	run         store.Run
	plain       string
	keyRevision int64
}

func newCodexBindingFix(t *testing.T) *codexBindingFix {
	t.Helper()
	fx := newProvFix(t)
	seedCodexSource(t, fx)
	plain := "inert-" + "binding-canary-" + uuid.NewString()
	// Seal under the real owner/kind AAD policy. Replace only this fixture's
	// dummy material; enablement_rev is deliberately preserved.
	sealed, err := fx.env.vlt.Seal(fx.env.ctx, fx.userID, store.KindOpenAICompatibleKey, []byte(plain), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	fx.env.exec(`UPDATE user_secrets SET ciphertext=$3, sealed_with=$4 WHERE id=$1 AND user_id=$2`,
		fx.key, fx.userID, sealed.Ciphertext, sealed.SealedWith)
	keyRevision := fx.keyRev()
	run, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	wantCodexBinding(t, fx, run, 1, keyRevision)
	reads := &codexBindingReads{Queries: fx.env.q}
	fx.svc.q = reads
	return &codexBindingFix{provFix: fx, reads: reads, run: run, plain: plain, keyRevision: keyRevision}
}

func (f *codexBindingFix) wantOpen(t *testing.T, run store.Run, provider uuid.UUID, revision int32, model string) {
	t.Helper()
	before := f.reads.cipherReads
	worker := store.Worker{ProtocolCapabilities: []string{
		capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3,
	}}
	pb, err := f.svc.assembleProviderBinding(f.env.ctx, worker, run)
	if err != nil || pb == nil {
		t.Fatalf("helper open failed: %v", err)
	}
	if pb.Credential != f.plain {
		t.Fatal("helper did not open the fixture canary")
	}
	want := ClaimProviderBinding{
		ProviderID: provider.String(), Revision: revision, Engine: providers.EngineCodex,
		Protocol: "openai_responses", BaseURL: pvBase, AuthStyle: "bearer",
		Credential: f.plain, ModelMap: providers.ModelMap{"default": {Model: model}},
	}
	if !reflect.DeepEqual(*pb, want) {
		t.Fatal("helper binding differs from the exact frozen profile")
	}
	if f.reads.cipherReads != before+1 {
		t.Fatalf("ciphertext reads = %d, want %d", f.reads.cipherReads, before+1)
	}
}

func TestCodexProviderBindingReadLiveDB(t *testing.T) {
	t.Run("frozen_profile_after_source_and_revision_edit", func(t *testing.T) {
		f := newCodexBindingFix(t)
		a, keyA := f.provider, f.key
		f.wantOpen(t, f.run, a, 1, "gateway") // positive control before edits
		rev, err := f.env.q.GetModelProviderRevisionForOwner(f.env.ctx, store.GetModelProviderRevisionForOwnerParams{
			ProviderID: a, Revision: 1, OwnerUserID: f.userID,
		})
		if err != nil || rev.Engine != providers.EngineCodex {
			t.Fatalf("generated owner revision engine = %q, err = %v", rev.Engine, err)
		}
		next, err := f.env.q.BumpModelProviderRevision(f.env.ctx, store.BumpModelProviderRevisionParams{ID: a, OwnerUserID: f.userID})
		if err != nil || next != 2 {
			t.Fatalf("bump revision = %d, err = %v", next, err)
		}
		if err := f.env.q.InsertModelProviderRevision(f.env.ctx, store.InsertModelProviderRevisionParams{
			ProviderID: a, Revision: next, Protocol: "openai_responses", BaseUrl: pvBase,
			AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"edited-a"}}`),
		}); err != nil {
			t.Fatal(err)
		}
		freshA, _, err := f.svc.createRunAtomic(f.env.ctx, f.userID, nil, f.insertAt(nil))
		if err != nil {
			t.Fatal(err)
		}
		wantCodexBinding(t, f.provFix, freshA, 2, f.keyRevision)
		f.wantOpen(t, freshA, a, 2, "edited-a")
		// B is a distinct current source; the frozen A run must still open key A.
		seedCodexSource(t, f.provFix)
		b, keyB := f.provider, f.key
		// Distinct B material detects accidental current-source reads.
		plainA := f.plain
		plainB := "inert-" + "binding-b-" + uuid.NewString()
		sealed, err := f.env.vlt.Seal(f.env.ctx, f.userID, store.KindOpenAICompatibleKey, []byte(plainB), vault.SealForUse)
		if err != nil {
			t.Fatal(err)
		}
		f.env.exec(`UPDATE user_secrets SET ciphertext=$3, sealed_with=$4 WHERE id=$1 AND user_id=$2`, keyB, f.userID, sealed.Ciphertext, sealed.SealedWith)
		keyBRev := f.keyRev()
		f.wantOpen(t, f.run, a, 1, "gateway")
		fresh, _, err := f.svc.createRunAtomic(f.env.ctx, f.userID, nil, f.insertAt(nil))
		if err != nil {
			t.Fatal(err)
		}
		wantCodexBinding(t, f.provFix, fresh, 1, keyBRev)
		f.plain = plainB
		f.wantOpen(t, fresh, b, 1, "gateway")
		f.plain = plainA
		f.provider, f.key = a, keyA
		wantCodexBinding(t, f.provFix, mustRun(t, f.env, f.run.ID), 1, f.keyRevision)
		if f.keyRev() != f.keyRevision {
			t.Fatal("material replacement changed A enablement revision")
		}
	})

	for _, mutation := range []string{"disabled", "enablement_revision_changed"} {
		t.Run(mutation, func(t *testing.T) {
			f := newCodexBindingFix(t)
			f.wantOpen(t, f.run, f.provider, 1, "gateway")
			if mutation == "disabled" {
				f.env.exec(`UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, f.key)
			} else {
				f.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, f.key)
			}
			before := f.reads.cipherReads
			pb, err := f.svc.assembleProviderBinding(f.env.ctx, store.Worker{ProtocolCapabilities: []string{
				capability.CodexProviderV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3,
			}}, f.run)
			if pb != nil || !errors.Is(err, ErrProviderBindingStale) {
				t.Fatalf("stale helper returned binding=%t, err=%v", pb != nil, err)
			}
			if f.reads.cipherReads != before {
				t.Fatal("stale helper fetched ciphertext")
			}
		})
	}
}

func TestCodexProviderBindingLockedFenceLiveDB(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "intact"
		if changed {
			name = "committed_revision_change_after_open"
		}
		t.Run(name, func(t *testing.T) {
			f := newCodexBindingFix(t)
			f.wantOpen(t, f.run, f.provider, 1, "gateway")
			if changed {
				f.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, f.key)
			}
			before := f.reads.cipherReads
			tx, err := f.env.pool.Begin(f.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			q := store.New(tx)
			// The run fence precedes the helper's key then revision locks.
			locked, err := q.GetRunByIDForUpdate(f.env.ctx, f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			reason, err := f.svc.lockedProviderRefusal(f.env.ctx, q, locked)
			wantReason := ""
			if changed {
				wantReason = holdProviderBindingStale
			}
			if err != nil || reason != wantReason {
				t.Fatalf("locked refusal = %q, %v; want %q", reason, err, wantReason)
			}
			rev, err := q.LockProviderBindingRevisionForKeyShareNowait(f.env.ctx, store.LockProviderBindingRevisionForKeyShareNowaitParams{
				ProviderID: f.provider, Revision: 1, OwnerUserID: f.userID,
			})
			if err != nil || rev.Engine != providers.EngineCodex {
				t.Fatalf("generated locked revision engine = %q, err = %v", rev.Engine, err)
			}
			if f.reads.cipherReads != before {
				t.Fatal("locked recheck fetched ciphertext")
			}
		})
	}
}
