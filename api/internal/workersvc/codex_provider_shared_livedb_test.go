package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// codex_provider_shared_livedb_test.go is issue #4633 (admin-shared Codex provider definitions with
// per-user keys) against a real Postgres: a recipient's Codex run freezes the SHARED revision and the
// recipient's own key, survives an admin edit, retirement and prune, goes stale on a key change, and
// never falls back to the vendor ladder. Skipped unless UZI_TEST_DATABASE_URL points at a throwaway
// Postgres; run via ./e2e/run-store-it.sh. Every test name ends LiveDB so the store-IT sweep selects it.

// sharedScopeID is the nil uuid: the shared scope of codex_provider_scope_id.
const sharedScopeID = "00000000-0000-0000-0000-000000000000"

// selectSharedCodexBySQL selects a shared Codex definition for userID by direct SQL, scope included.
func selectSharedCodexBySQL(pf *pcFix, provider uuid.UUID) {
	pf.env.exec(`UPDATE users SET codex_provider_id=$2, codex_provider_scope_id='`+sharedScopeID+`' WHERE id=$1`, pf.userID, provider)
}

// createSharedCodexTask creates (without dispatching) another Codex task run through the real path.
func createSharedCodexTask(pf *pcFix, title string) (store.Run, error) {
	return pf.svc.CreateTaskRun(pf.env.ctx, pf.userID, pf.repoID, title, "main", false, false, false, false, nil)
}

// seedSharedCodexSelection seeds a shared Codex definition, the recipient's own sealed key on it (unique
// label) and the direct-SQL selection, and creates no run. Tests that must reach a reader without any
// run-creation path in front of it (the freezeProviderSource pin) use this directly.
func seedSharedCodexSelection(t *testing.T) *pcFix {
	t.Helper()
	pf := newPCFix(t)
	provider, _ := seedSharedProvider(t, pf.env, "codex", `{"default":{"model":"gateway"}}`)
	key := seedSealedKey(t, pf.env, pf.userID, provider, pf.plain)
	pf.env.exec(`UPDATE user_secrets SET label=$2 WHERE id=$1`, key, "shared-codex-key-"+uuid.NewString())
	pf.provider, pf.key = provider, key
	selectSharedCodexBySQL(pf, provider)
	t.Cleanup(func() {
		_, _ = pf.env.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id=NULL,codex_provider_scope_id=NULL WHERE id=$1`, pf.userID)
	})
	return pf
}

// newSharedCodexPCFix is newCodexActualClaimFix over a SHARED Codex definition: default_harness is codex,
// a usable static openai_api_key default exists, the recipient holds their own sealed key on the shared
// definition (unique label), and the run is created, dispatched and frozen through CreateTaskRun.
func newSharedCodexPCFix(t *testing.T) (*pcFix, uuid.UUID) {
	t.Helper()
	pf := seedSharedCodexSelection(t)
	pf.env.exec(`UPDATE users SET default_harness='codex' WHERE id=$1`, pf.userID)
	pf.env.exec(`UPDATE workers SET protocol_capabilities=$2 WHERE id=$1`, pf.workerID,
		[]string{capability.RecoveryArchiveV1, capability.CredentialSwitchV1, capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3, capability.CodexProviderV1, capability.RepoAgentSourceV1})
	// A usable native credential stays available throughout every provider refusal.
	vendor := pf.env.seedStaticAPIKey(t, pf.userID, "available-native-"+uuid.NewString(), "inert-available-native-key")
	pf.env.exec(`UPDATE user_secrets SET is_default=true WHERE id=$1`, vendor)

	created, err := createSharedCodexTask(pf, "Shared Codex provider claim")
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := pf.svc.DispatchTaskRun(pf.env.ctx, pf.userID, created.ID)
	if err != nil || !dispatched.DispatchedAt.Valid || bindingOfRun(dispatched) != bindingOfRun(created) {
		t.Fatalf("dispatch did not preserve the created binding: err=%v", err)
	}
	return pf, created.ID
}

func wantSharedCodexBinding(t *testing.T, pf *pcFix, r store.Run, revision int32, keyRev int64) {
	t.Helper()
	if !r.ModelProviderID.Valid || uuid.UUID(r.ModelProviderID.Bytes) != pf.provider ||
		!r.ModelProviderRevision.Valid || r.ModelProviderRevision.Int32 != revision ||
		r.ModelProviderKeyMode.String != providerKeyModeUser ||
		!r.ModelProviderKeySecretID.Valid || uuid.UUID(r.ModelProviderKeySecretID.Bytes) != pf.key ||
		!r.ModelProviderKeyRev.Valid || r.ModelProviderKeyRev.Int64 != keyRev {
		t.Fatalf("binding = %+v, want shared provider %s rev %d key %s keyRev %d", bindingOfRun(r), pf.provider, revision, pf.key, keyRev)
	}
	if r.Harness != harnessCodex || r.CodexSecretID.Valid || r.CodexAuthMode.Valid || r.AnthropicSecretID.Valid || r.CredentialOverrideSecretID.Valid {
		t.Fatal("shared provider run retained vendor, auth or override fields")
	}
}

func TestCodexProviderActualClaimSharedLiveDB(t *testing.T) {
	t.Run("freeze and claim", func(t *testing.T) {
		pf, id := newSharedCodexPCFix(t)
		frozen := mustRun(t, pf.env, id)
		wantSharedCodexBinding(t, pf, frozen, 1, pf.keyRev(t))
		restartCodexActualClaimService(pf)
		wantCodexActualProviderClaim(t, pf, id)
	})

	// The pinned site for the owner-only reader mutation: freezeProviderSource itself, on the shared
	// selection, must bind revision N and the recipient's key.
	t.Run("freezeProviderSource binds the shared selection", func(t *testing.T) {
		pf := seedSharedCodexSelection(t) // no run is created: the reader is the first thing exercised
		tx, err := pf.env.pool.Begin(pf.env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(pf.env.ctx) }()
		src, err := pf.svc.freezeProviderSource(pf.env.ctx, tx, store.New(tx), pf.userID, HarnessCodex)
		if err != nil {
			t.Fatalf("freezeProviderSource on a shared Codex selection = %v, want a binding", err)
		}
		if src.Engine != harnessCodex || src.ProviderID != pf.provider || src.Revision != 1 ||
			src.KeySecretID != pf.key || src.KeyRev != pf.keyRev(t) {
			t.Fatalf("frozen source = %+v, want shared %s rev 1 key %s", src, pf.provider, pf.key)
		}
	})

	t.Run("admin edit and prune", func(t *testing.T) {
		pf, id := newSharedCodexPCFix(t)
		ctx := pf.env.ctx
		keyRev := pf.keyRev(t)

		// Replay providers_admin.go's edit: lock for edit, copy forward, bump, insert, then prune.
		tx, err := pf.env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		q := store.New(tx)
		cur, err := q.LockSharedModelProviderForEdit(ctx, pf.provider)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		prev, err := q.GetModelProviderRevision(ctx, store.GetModelProviderRevisionParams{ProviderID: pf.provider, Revision: cur.CurrentRevision})
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		next, err := q.BumpSharedModelProviderRevision(ctx, pf.provider)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := q.InsertModelProviderRevision(ctx, store.InsertModelProviderRevisionParams{
			ProviderID: pf.provider, Revision: next, Protocol: prev.Protocol, BaseUrl: prev.BaseUrl, AuthStyle: prev.AuthStyle,
			ModelMap: []byte(`{"default":{"model":"edited-gateway"}}`),
		}); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if next != 2 {
			t.Fatalf("edit produced revision %d, want 2", next)
		}
		if _, err := pf.env.q.PruneSharedModelProviderRevisions(ctx, pf.provider); err != nil {
			t.Fatal(err)
		}

		// A fresh run freezes N+1 with the same key; the frozen run is untouched.
		fresh, err := createSharedCodexTask(pf, "Shared Codex fresh after edit")
		if err != nil {
			t.Fatal(err)
		}
		wantSharedCodexBinding(t, pf, mustRun(t, pf.env, fresh.ID), 2, keyRev)
		wantSharedCodexBinding(t, pf, mustRun(t, pf.env, id), 1, keyRev)

		// Revision N survives the prune because the frozen run references it.
		var n int
		if err := pf.env.pool.QueryRow(ctx, `SELECT count(*) FROM model_provider_revisions WHERE provider_id=$1 AND revision=1`, pf.provider).Scan(&n); err != nil || n != 1 {
			t.Fatalf("revision 1 after prune: %d rows (%v), want it kept", n, err)
		}

		// The frozen run still claims revision N with the recipient's key (fresh is undispatched).
		restartCodexActualClaimService(pf)
		wantCodexActualProviderClaim(t, pf, id)
	})

	t.Run("retirement", func(t *testing.T) {
		pf, id := newSharedCodexPCFix(t)
		ctx := pf.env.ctx
		// First claim happens BEFORE retirement so the later claim is a genuine reclaim.
		restartCodexActualClaimService(pf)
		first := wantCodexActualProviderClaim(t, pf, id)
		src := mustRun(t, pf.env, id)
		if n, err := pf.env.q.RetireSharedModelProvider(ctx, pf.provider); err != nil || n != 1 {
			t.Fatalf("retire shared = (%d, %v)", n, err)
		}

		before := snapshotProviderRuns(t, pf.env, pf.userID)
		if _, err := createSharedCodexTask(pf, "Shared Codex after retire"); !isUnusable(err, SourceUnusableRetired) {
			t.Fatalf("fresh create err = %v, want provider_source_unusable (retired)", err)
		}
		if snapshotProviderRuns(t, pf.env, pf.userID) != before {
			t.Fatal("a refused creation changed persisted runs")
		}

		// An inherited successor keeps the binding.
		tx, err := pf.env.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		got, err := pf.svc.inheritProviderBinding(ctx, tx, store.New(tx), src)
		if err != nil {
			t.Fatalf("inherit after retire: %v", err)
		}
		if got.ProviderID != pf.provider || got.Revision != 1 || got.KeySecretID != pf.key || got.Engine != harnessCodex {
			t.Fatalf("inherited source = %+v", got)
		}

		// A real reclaim keeps it too: requeue the claimed run (no worker process runs in this fixture),
		// then claim again after retirement.
		_ = tx.Rollback(ctx)
		pf.env.exec(`UPDATE runs SET status='queued',claim_released_at=now()
    WHERE id=$1 AND worker_id=$2 AND claim_generation=$3 AND status='claimed'`, id, pf.workerID, first.ClaimGeneration)
		restartCodexActualClaimService(pf)
		again := wantCodexActualProviderClaim(t, pf, id)
		if again.ClaimGeneration <= first.ClaimGeneration {
			t.Fatalf("claim generation %d did not advance past %d", again.ClaimGeneration, first.ClaimGeneration)
		}
		wantSharedCodexBinding(t, pf, again, 1, pf.keyRev(t))
	})

	for _, mutation := range []string{"rotated", "disabled", "deleted"} {
		t.Run("key "+mutation, func(t *testing.T) {
			pf, id := newSharedCodexPCFix(t)
			switch mutation {
			case "rotated":
				pf.rotate()
			case "disabled":
				pf.disable()
			case "deleted":
				pf.deleteKey()
			}
			restartCodexActualClaimService(pf)
			p, err := pf.svc.Claim(pf.env.ctx, pf.worker(t), nil)
			if err != nil || p != nil {
				t.Fatalf("refused claim payload present=%t err=%v", p != nil, err)
			}
			wantCodexActualNoVendorWrites(t, pf, pf.wantParked(t, id, holdProviderBindingStale))
		})
	}
}

// TestCodexProviderInheritRefusesWrongKindKeyLiveDB: the inherit check compares the key's kind with the
// engine's key kind. Flip only K's kind in place, keeping its id, provider, disabled state and
// enablement_rev, so the kind alone is what makes the binding stale.
func TestCodexProviderInheritRefusesWrongKindKeyLiveDB(t *testing.T) {
	pf, id := newSharedCodexPCFix(t)
	ctx := pf.env.ctx
	src := mustRun(t, pf.env, id)
	pf.env.exec(`UPDATE user_secrets SET kind='anthropic_compatible_key' WHERE id=$1`, pf.key)

	tx, err := pf.env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := pf.svc.inheritProviderBinding(ctx, tx, store.New(tx), src); !errors.Is(err, ErrProviderBindingStale) {
		t.Fatalf("inherit over a wrong-kind key err = %v, want provider_binding_stale", err)
	}
	_ = tx.Rollback(ctx)

	restartCodexActualClaimService(pf)
	p, err := pf.svc.Claim(ctx, pf.worker(t), nil)
	if err != nil || p != nil {
		t.Fatalf("claim over a wrong-kind key: payload present=%t err=%v", p != nil, err)
	}
	wantCodexActualNoVendorWrites(t, pf, pf.wantParked(t, id, holdProviderBindingStale))
}

func TestCodexProviderSharedNoVendorFallbackLiveDB(t *testing.T) {
	t.Run("vendor control", func(t *testing.T) {
		pf, first := newSharedCodexPCFix(t)
		ctx := pf.env.ctx
		var vendor uuid.UUID
		if err := pf.env.pool.QueryRow(ctx, `SELECT id FROM user_secrets WHERE user_id=$1 AND kind='openai_api_key'`, pf.userID).Scan(&vendor); err != nil {
			t.Fatal(err)
		}
		// Retire the provider-bound run from admission and clear both columns.
		pf.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, first)
		pf.env.exec(`UPDATE users SET codex_provider_id=NULL, codex_provider_scope_id=NULL WHERE id=$1`, pf.userID)

		created, err := createSharedCodexTask(pf, "Vendor control")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pf.svc.DispatchTaskRun(ctx, pf.userID, created.ID); err != nil {
			t.Fatal(err)
		}
		restartCodexActualClaimService(pf)
		p, err := pf.svc.Claim(ctx, pf.worker(t), nil)
		if err != nil || p == nil || p.RunID != created.ID.String() || p.Secrets.Provider != nil || p.Secrets.Codex == nil {
			t.Fatalf("vendor Claim payload=%+v err=%v", p, err)
		}
		r := mustRun(t, pf.env, created.ID)
		if r.Harness != harnessCodex {
			t.Fatalf("harness = %q, want codex", r.Harness)
		}
		if b := bindingOfRun(r); b != (bindingParams{}) {
			t.Fatalf("a vendor run carries a binding: %+v", b)
		}
		if !r.CodexSecretID.Valid || uuid.UUID(r.CodexSecretID.Bytes) != vendor {
			t.Fatalf("codex secret = %v, want the static vendor credential %s", r.CodexSecretID, vendor)
		}
	})

	t.Run("deleted key refuses without fallback", func(t *testing.T) {
		pf, first := newSharedCodexPCFix(t)
		ctx := pf.env.ctx
		pf.env.exec(`UPDATE runs SET status='completed' WHERE id=$1`, first)
		pf.deleteKey()
		// Direct SQL selection (scope included) is still in place from the fixture.
		selectSharedCodexBySQL(pf, pf.provider)

		before := snapshotProviderRuns(t, pf.env, pf.userID)
		created, err := createSharedCodexTask(pf, "Shared Codex without a key")
		if err == nil {
			// A fallback mutant creates the run. Record the failure, then run it exactly as the vendor
			// control does so the red output shows which credential the fallback reached.
			t.Errorf("create succeeded, want provider_source_unusable (key_missing)")
			var vendor uuid.UUID
			if err := pf.env.pool.QueryRow(ctx, `SELECT id FROM user_secrets WHERE user_id=$1 AND kind='openai_api_key'`, pf.userID).Scan(&vendor); err != nil {
				t.Fatal(err)
			}
			if _, err := pf.svc.DispatchTaskRun(ctx, pf.userID, created.ID); err != nil {
				t.Fatal(err)
			}
			restartCodexActualClaimService(pf)
			p, err := pf.svc.Claim(ctx, pf.worker(t), nil)
			if err != nil || p == nil || p.RunID != created.ID.String() {
				t.Fatalf("fallback Claim payload=%+v err=%v", p, err)
			}
			r := mustRun(t, pf.env, created.ID)
			if r.CodexSecretID.Valid && uuid.UUID(r.CodexSecretID.Bytes) == vendor {
				t.Errorf("the run fell back to the vendor credential %s", vendor)
			}
			if b := bindingOfRun(r); b == (bindingParams{}) {
				t.Errorf("the fallback run carries no provider binding, so it ran on the vendor path")
			}
			return
		}
		if !errors.Is(err, ErrProviderSourceUnusable) || !isUnusable(err, SourceUnusableKeyMissing) {
			t.Fatalf("create err = %v, want provider_source_unusable (key_missing)", err)
		}
		var u *ProviderSourceUnusableError
		if !errors.As(err, &u) || u.Reason != SourceUnusableKeyMissing || u.ProviderID != pf.provider {
			t.Fatalf("unusable error = %+v", u)
		}
		if snapshotProviderRuns(t, pf.env, pf.userID) != before {
			t.Fatal("a refused creation wrote a run")
		}
		var vendorBound, bound int
		if err := pf.env.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE codex_secret_id IS NOT NULL), count(*) FILTER (WHERE model_provider_id IS NOT NULL)
			FROM runs WHERE user_id=$1 AND id<>$2`, pf.userID, first).Scan(&vendorBound, &bound); err != nil {
			t.Fatal(err)
		}
		if vendorBound != 0 || bound != 0 {
			t.Fatalf("refusal left %d vendor-bound and %d provider-bound run(s)", vendorBound, bound)
		}
		restartCodexActualClaimService(pf)
		if p, err := pf.svc.Claim(ctx, pf.worker(t), nil); err != nil || p != nil {
			t.Fatalf("a vendor claim happened after the refusal: payload present=%t err=%v", p != nil, err)
		}
		if snapshotProviderRuns(t, pf.env, pf.userID) != before {
			t.Fatal("the idle claim changed persisted runs")
		}
	})
}
