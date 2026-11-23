package workersvc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// seedCodexSource uses the existing unique-user fixture and the real engine-aware key query.
func seedCodexSource(t *testing.T, fx *provFix) {
	t.Helper()
	p, err := fx.env.q.InsertModelProvider(fx.env.ctx, store.InsertModelProviderParams{OwnerUserID: fx.userID, Name: "codex-" + uuid.NewString(), Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.env.q.InsertModelProviderRevision(fx.env.ctx, store.InsertModelProviderRevisionParams{ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`)}); err != nil {
		t.Fatal(err)
	}
	key, err := fx.env.q.InsertProviderKeySecret(fx.env.ctx, store.InsertProviderKeySecretParams{UserID: fx.userID, Label: p.ID.String(), Ciphertext: []byte("ct"), SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	fx.provider, fx.key = p.ID, key.ID
	t.Cleanup(func() {
		_, _ = fx.env.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id=NULL WHERE id=$1`, fx.userID)
	})
	fx.env.exec(`UPDATE users SET codex_provider_id=$2, default_harness='codex' WHERE id=$1`, fx.userID, p.ID)
}

func wantCodexBinding(t *testing.T, fx *provFix, run store.Run, revision int32, keyRev int64) {
	t.Helper()
	want := bindingParams{
		ProviderID: pgtype.UUID{Bytes: fx.provider, Valid: true},
		Revision:   pgtype.Int4{Int32: revision, Valid: true},
		KeyMode:    pgtype.Text{String: "user_key", Valid: true},
		KeySecret:  pgtype.UUID{Bytes: fx.key, Valid: true},
		KeyRev:     pgtype.Int8{Int64: keyRev, Valid: true},
	}
	if bindingOfRun(run) != want {
		t.Fatalf("binding = %+v, want %+v", bindingOfRun(run), want)
	}
	if run.Harness != "codex" || run.CodexSecretID.Valid || run.CodexAuthMode.Valid || run.AnthropicSecretID.Valid || run.CredentialOverrideSecretID.Valid {
		t.Fatal("provider run retained vendor/auth/override fields")
	}
}

func TestCodexProviderSourceFreezeLiveDB(t *testing.T) {
	fx := newProvFix(t)
	seedCodexSource(t, fx) // No vendor credentials exist.
	fx.env.exec(`UPDATE user_secrets SET enablement_rev=7 WHERE id=$1`, fx.key)
	first, res, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Codex != nil {
		t.Fatal("provider resolution returned vendor account")
	}
	wantCodexBinding(t, fx, mustRun(t, fx.env, first.ID), 1, 7)
	rev, err := fx.env.q.BumpModelProviderRevision(fx.env.ctx, store.BumpModelProviderRevisionParams{ID: fx.provider, OwnerUserID: fx.userID})
	if err != nil || rev != 2 {
		t.Fatalf("bump: %d %v", rev, err)
	}
	if err := fx.env.q.InsertModelProviderRevision(fx.env.ctx, store.InsertModelProviderRevisionParams{ProviderID: fx.provider, Revision: rev, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"new-gateway"}}`)}); err != nil {
		t.Fatal(err)
	}
	n, err := fx.env.q.PruneModelProviderRevisions(fx.env.ctx, store.PruneModelProviderRevisionsParams{ProviderID: fx.provider, OwnerUserID: fx.userID})
	if err != nil || n != 0 {
		t.Fatalf("prune referenced revision: %d %v", n, err)
	}
	var modelMap []byte
	if err := fx.env.pool.QueryRow(fx.env.ctx, `SELECT model_map FROM model_provider_revisions WHERE provider_id=$1 AND revision=1`, fx.provider).Scan(&modelMap); err != nil {
		t.Fatalf("old revision removed: %v", err)
	}
	wantCodexBinding(t, fx, mustRun(t, fx.env, first.ID), 1, 7)
	fresh, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	wantCodexBinding(t, fx, mustRun(t, fx.env, fresh.ID), 2, 7)
}

func TestCodexProviderUnusableRefusesLiveDB(t *testing.T) {
	for _, broken := range []string{"retired", "disabled", "deleted"} {
		t.Run(broken, func(t *testing.T) {
			fx := newProvFix(t)
			fx.anthropic()
			fx.codexDefault()
			seedCodexSource(t, fx)
			switch broken {
			case "retired":
				fx.env.exec(`UPDATE model_providers SET retired_at=now() WHERE id=$1`, fx.provider)
			case "disabled":
				fx.env.exec(`UPDATE user_secrets SET disabled_at=now() WHERE id=$1`, fx.key)
			case "deleted":
				fx.env.exec(`DELETE FROM user_secrets WHERE id=$1`, fx.key)
			}
			for _, explicit := range []*Harness{nil, h(HarnessCodex)} {
				_, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, explicit, fx.insertAt(nil))
				if !errors.Is(err, ErrProviderSourceUnusable) {
					t.Fatalf("selected unusable source fell back: %v", err)
				}
			}
			if fx.runCount() != 0 {
				t.Fatal("refusal inserted a run")
			}
		})
	}
}

func TestCodexProviderFreezeLocksLiveDB(t *testing.T) {
	for _, probe := range []string{"provider edit", "secret mutation"} {
		t.Run(probe, func(t *testing.T) {
			fx := newProvFix(t)
			seedCodexSource(t, fx)
			inner := fx.insertAt(nil)
			run, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, func(q *store.Queries, r resolvedHarness) (store.Run, error) {
				ctx, cancel := context.WithTimeout(fx.env.ctx, 3*time.Second)
				defer cancel()
				tx, err := fx.aux.Begin(ctx)
				if err != nil {
					return store.Run{}, err
				}
				defer func() { _ = tx.Rollback(context.Background()) }()
				if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout='100ms'"); err != nil {
					return store.Run{}, err
				}
				if probe == "provider edit" {
					_, err = tx.Exec(ctx, `UPDATE model_providers SET current_revision=current_revision+1 WHERE id=$1`, fx.provider)
				} else {
					err = store.LockSecretMutation(ctx, tx, fx.userID)
				}
				var pgerr *pgconn.PgError
				if !errors.As(err, &pgerr) || pgerr.Code != "55P03" {
					t.Fatalf("%s: want lock_timeout 55P03, got %v", probe, err)
				}
				return inner(q, r)
			})
			if err != nil {
				t.Fatal(err)
			}
			wantCodexBinding(t, fx, mustRun(t, fx.env, run.ID), 1, fx.keyRev())
			// The exact same operations are possible once creation releases its locks.
			tx, err := fx.aux.Begin(fx.env.ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(fx.env.ctx, "SET LOCAL lock_timeout='100ms'"); err != nil {
				t.Fatal(err)
			}
			if probe == "provider edit" {
				_, err = tx.Exec(fx.env.ctx, `UPDATE model_providers SET current_revision=current_revision+1 WHERE id=$1`, fx.provider)
			} else {
				err = store.LockSecretMutation(fx.env.ctx, tx, fx.userID)
			}
			if err != nil {
				t.Fatalf("lock still held after commit: %v", err)
			}
		})
	}
}

func TestCodexProviderSuccessorLifecycleLiveDB(t *testing.T) {
	for _, change := range []string{"cleared", "changed", "retired", "rotated", "disable-enable"} {
		t.Run(change, func(t *testing.T) {
			fx := newProvFix(t)
			fx.codexDefault()
			seedCodexSource(t, fx)
			src, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
			if err != nil {
				t.Fatal(err)
			}
			oldProvider, oldKey := fx.provider, fx.key
			switch change {
			case "cleared":
				fx.env.exec(`UPDATE users SET codex_provider_id=NULL WHERE id=$1`, fx.userID)
			case "changed":
				seedCodexSource(t, fx)
			case "retired":
				fx.env.exec(`UPDATE model_providers SET retired_at=now() WHERE id=$1`, fx.provider)
			case "rotated":
				fx.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, fx.key)
			case "disable-enable":
				fx.env.exec(`UPDATE user_secrets SET disabled_at=now(), enablement_rev=enablement_rev+1 WHERE id=$1`, fx.key)
				fx.env.exec(`UPDATE user_secrets SET disabled_at=NULL, enablement_rev=enablement_rev+1 WHERE id=$1`, fx.key)
			}
			before := fx.runCount()
			child, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil), harnessCreationOrigin{InheritFrom: &src})
			if change == "rotated" || change == "disable-enable" {
				if !errors.Is(err, ErrProviderBindingStale) {
					t.Fatalf("successor: %v, want typed stale", err)
				}
				if fx.runCount() != before {
					t.Fatal("stale successor inserted a row")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if bindingOfRun(mustRun(t, fx.env, child.ID)) != bindingOfRun(src) {
				t.Fatal("successor moved binding")
			}
			if change == "retired" {
				return
			}
			fresh, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
			if err != nil {
				t.Fatal(err)
			}
			fresh = mustRun(t, fx.env, fresh.ID)
			if change == "cleared" {
				fx.wantVendor(fresh)
				if !fresh.CodexSecretID.Valid {
					t.Fatal("fresh cleared-source run lacks vendor credential")
				}
			} else if uuid.UUID(fresh.ModelProviderID.Bytes) != fx.provider || uuid.UUID(fresh.ModelProviderKeySecretID.Bytes) != fx.key || fx.provider == oldProvider || fx.key == oldKey {
				t.Fatal("ordinary fresh creation inherited old source")
			}
		})
	}
}

func TestCodexProviderFrozenSuccessorLiveDB(t *testing.T) {
	fx := newProvFix(t)
	fx.anthropic()
	fx.codexDefault()
	p, err := fx.env.q.InsertModelProvider(fx.env.ctx, store.InsertModelProviderParams{OwnerUserID: fx.userID, Name: "codex-" + uuid.NewString(), Engine: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	err = fx.env.q.InsertModelProviderRevision(fx.env.ctx, store.InsertModelProviderRevisionParams{ProviderID: p.ID, Revision: 1, Protocol: "openai_responses", BaseUrl: pvBase, AuthStyle: "bearer", ModelMap: []byte(`{"default":{"model":"gateway"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	key, err := fx.env.q.InsertProviderKeySecret(fx.env.ctx, store.InsertProviderKeySecretParams{UserID: fx.userID, Label: p.ID.String(), Ciphertext: []byte("ct"), SealedWith: "master", ProviderID: pgtype.UUID{Bytes: p.ID, Valid: true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fx.env.pool.Exec(context.Background(), `UPDATE users SET codex_provider_id=NULL WHERE id=$1`, fx.userID)
	})
	fx.env.exec(`UPDATE users SET codex_provider_id=$2,default_harness='codex' WHERE id=$1`, fx.userID, p.ID)
	run, res, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Codex != nil || run.Harness != "codex" || !run.ModelProviderKeyRev.Valid || uuid.UUID(run.ModelProviderKeySecretID.Bytes) != key.ID || run.CodexSecretID.Valid || run.CodexAuthMode.Valid {
		t.Fatalf("wrong binding: %+v", run)
	}
	fx.env.exec(`UPDATE users SET codex_provider_id=NULL WHERE id=$1`, fx.userID)
	child, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil), harnessCreationOrigin{InheritFrom: &run})
	if err != nil {
		t.Fatal(err)
	}
	if bindingOfRun(child) != bindingOfRun(run) {
		t.Fatal("successor changed binding")
	}
	fresh, _, err := fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil))
	if err != nil {
		t.Fatal(err)
	}
	fresh = mustRun(t, fx.env, fresh.ID)
	if fresh.ModelProviderID.Valid || !fresh.CodexSecretID.Valid {
		t.Fatal("fresh creation inherited provider")
	}
	fx.env.exec(`UPDATE user_secrets SET enablement_rev=enablement_rev+1 WHERE id=$1`, key.ID)
	_, _, err = fx.svc.createRunAtomic(fx.env.ctx, fx.userID, nil, fx.insertAt(nil), harnessCreationOrigin{InheritFrom: &run})
	if !errors.Is(err, ErrProviderBindingStale) {
		t.Fatalf("rotated successor: %v", err)
	}
}
