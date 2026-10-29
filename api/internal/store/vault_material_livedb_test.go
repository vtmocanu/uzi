package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

func vaultMaterialSnapshot(t *testing.T, pool *pgxpool.Pool, query string, id uuid.UUID) []byte {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), query, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func vaultMaterialWantLocked(t *testing.T, pool *pgxpool.Pool, query string, id uuid.UUID) {
	t.Helper()
	var got uuid.UUID
	err := pool.QueryRow(context.Background(), query, id).Scan(&got)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("material row was not locked: %v", err)
	}
}

func TestVaultMaterialQueriesLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "vault_mode"))
	q := New(pool)
	owner, foreign := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	if totals, err := q.GetVaultMaterialTotals(ctx, owner); err != nil || totals != (GetVaultMaterialTotalsRow{}) {
		t.Fatalf("empty totals = %+v, %v", totals, err)
	}
	kinds := []string{KindAnthropicToken, KindOpenAIAPIKey, KindCodexAuth}
	for i := range 65 {
		id := uuid.New()
		if i == 0 {
			id = uuid.Nil // A NULL initial cursor must include even this valid UUID.
		}
		cjExec(t, pool, "INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,$3,$4,$5,'master')",
			id, owner, kinds[i%len(kinds)], fmt.Sprintf("material-%03d", i), []byte("old"))
	}
	cjExec(t, pool, "UPDATE user_secrets SET disabled_at=now(),auto_eligible=true,enablement_rev=5 WHERE id=$1", uuid.Nil)
	for i := range 33 {
		cjExec(t, pool, "INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with,recovery_sealed,recovery_sealed_with,generation,credential_revision,recovery_generation,coord_state) VALUES ($1,$2,'workspace',$3,'master',$4,'master',7,9,7,'quarantined')",
			owner, fmt.Sprintf("principal-%03d", i), []byte("live"), []byte("backup"))
	}
	cjExec(t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with) VALUES ($1,'anthropic_token','foreign',$2,'master')", foreign, []byte("foreign"))
	cjExec(t, pool, "INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with) VALUES ($1,'foreign','workspace',$2,'master')", foreign, []byte("else"))
	const personalMetadataSQL = "SELECT to_jsonb(s)-ARRAY['ciphertext','sealed_with'] FROM user_secrets s WHERE id=$1"
	const accountMetadata = "SELECT to_jsonb(a)-ARRAY['sealed_login','sealed_with','recovery_sealed','recovery_sealed_with'] FROM codex_provider_account a WHERE id=$1"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockSecretMutation(ctx, tx, owner); err != nil {
		t.Fatal(err)
	}
	tq := q.WithTx(tx)
	if _, err := tq.LockUserVaultState(ctx, owner); err != nil {
		t.Fatal(err)
	}
	totals, err := tq.GetVaultMaterialTotals(ctx, owner)
	if err != nil || totals.MaterialRows != 131 || totals.CiphertextBytes != 525 || totals.MaxRowBytes != 10 {
		t.Fatalf("owned totals (including disabled rows and orphan account slots) = %+v, %v", totals, err)
	}
	accounts, err := tq.LockVaultAccountMaterialBatch(ctx, LockVaultAccountMaterialBatchParams{UserID: owner})
	if err != nil || len(accounts) != 32 {
		t.Fatalf("64 account-slot page: rows=%d error=%v", len(accounts), err)
	}
	vaultMaterialWantLocked(t, pool, "SELECT id FROM codex_provider_account WHERE id=$1 FOR UPDATE NOWAIT", accounts[0].ID)
	nextAccounts, err := tq.LockVaultAccountMaterialBatch(ctx, LockVaultAccountMaterialBatchParams{UserID: owner, AfterID: pgtype.UUID{Bytes: accounts[len(accounts)-1].ID, Valid: true}})
	if err != nil || len(nextAccounts) != 1 {
		t.Fatalf("account tail: rows=%d error=%v", len(nextAccounts), err)
	}
	secrets, err := tq.LockVaultSecretMaterialBatch(ctx, LockVaultSecretMaterialBatchParams{UserID: owner})
	if err != nil || len(secrets) != 64 || secrets[0].ID != uuid.Nil {
		t.Fatalf("64 secret page including zero UUID: rows=%d error=%v", len(secrets), err)
	}
	vaultMaterialWantLocked(t, pool, "SELECT id FROM user_secrets WHERE id=$1 FOR UPDATE NOWAIT", secrets[0].ID)
	nextSecrets, err := tq.LockVaultSecretMaterialBatch(ctx, LockVaultSecretMaterialBatchParams{UserID: owner, AfterID: pgtype.UUID{Bytes: secrets[len(secrets)-1].ID, Valid: true}})
	if err != nil || len(nextSecrets) != 1 {
		t.Fatalf("secret tail: rows=%d error=%v", len(nextSecrets), err)
	}
	secretBefore := vaultMaterialSnapshot(t, pool, personalMetadataSQL, secrets[0].ID)
	accountBefore := vaultMaterialSnapshot(t, pool, accountMetadata, accounts[0].ID)
	secretWrite := ResealVaultSecretMaterialParams{ID: secrets[0].ID, UserID: foreign, Ciphertext: []byte("replacement"), SealedWith: SealedWithMasterAAD}
	if n, err := tq.ResealVaultSecretMaterial(ctx, secretWrite); err != nil || n != 0 {
		t.Fatalf("foreign secret reseal: rows=%d error=%v", n, err)
	}
	secretWrite.UserID = owner
	if n, err := tq.ResealVaultSecretMaterial(ctx, secretWrite); err != nil || n != 1 {
		t.Fatalf("owned secret reseal: rows=%d error=%v", n, err)
	}
	accountWrite := ResealVaultAccountMaterialParams{ID: accounts[0].ID, UserID: foreign,
		SealedLogin: []byte("replacement-live"), SealedWith: SealedWithMasterAAD,
		RecoverySealed: []byte("replacement-recovery"), RecoverySealedWith: pgtype.Text{String: SealedWithMasterAAD, Valid: true}}
	if n, err := tq.ResealVaultAccountMaterial(ctx, accountWrite); err != nil || n != 0 {
		t.Fatalf("foreign account reseal: rows=%d error=%v", n, err)
	}
	accountWrite.UserID = owner
	if n, err := tq.ResealVaultAccountMaterial(ctx, accountWrite); err != nil || n != 1 {
		t.Fatalf("owned account reseal: rows=%d error=%v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := vaultMaterialSnapshot(t, pool, personalMetadataSQL, secrets[0].ID); !bytes.Equal(got, secretBefore) {
		t.Fatal("secret reseal changed non-crypto metadata")
	}
	if got := vaultMaterialSnapshot(t, pool, accountMetadata, accounts[0].ID); !bytes.Equal(got, accountBefore) {
		t.Fatal("account reseal changed non-crypto metadata")
	}
	var secretMarker, liveMarker, recoveryMarker string
	var savedPersonal, savedLive, savedRecovery []byte
	if err := pool.QueryRow(ctx, "SELECT sealed_with,ciphertext FROM user_secrets WHERE id=$1", secrets[0].ID).Scan(&secretMarker, &savedPersonal); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, "SELECT sealed_with,recovery_sealed_with,sealed_login,recovery_sealed FROM codex_provider_account WHERE id=$1", accounts[0].ID).Scan(&liveMarker, &recoveryMarker, &savedLive, &savedRecovery); err != nil {
		t.Fatal(err)
	}
	if secretMarker != SealedWithMasterAAD || liveMarker != SealedWithMasterAAD || recoveryMarker != SealedWithMasterAAD {
		t.Fatal("crypto discriminators were not updated together")
	}
	if !bytes.Equal(savedPersonal, secretWrite.Ciphertext) {
		t.Fatal("secret ciphertext was not replaced")
	}
	if !bytes.Equal(savedLive, accountWrite.SealedLogin) {
		t.Fatal("live account ciphertext was not replaced")
	}
	if !bytes.Equal(savedRecovery, accountWrite.RecoverySealed) {
		t.Fatal("recovery account ciphertext was not replaced")
	}
	// The foreign positive control also covers an account with no recovery slot.
	ftx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ftx.Rollback(ctx) }()
	if err := LockSecretMutation(ctx, ftx, foreign); err != nil {
		t.Fatal(err)
	}
	fq := q.WithTx(ftx)
	if _, err := fq.LockUserVaultState(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	frows, err := fq.LockVaultAccountMaterialBatch(ctx, LockVaultAccountMaterialBatchParams{UserID: foreign})
	if err != nil || len(frows) != 1 {
		t.Fatalf("foreign positive control: rows=%d error=%v", len(frows), err)
	}
	if frows[0].RecoverySealed != nil || frows[0].RecoverySealedWith.Valid {
		t.Fatal("empty recovery slot acquired material")
	}
	if n, err := fq.ResealVaultAccountMaterial(ctx, ResealVaultAccountMaterialParams{ID: frows[0].ID, UserID: foreign, SealedLogin: []byte("new-live"), SealedWith: SealedWithMasterAAD}); err != nil || n != 1 {
		t.Fatalf("reseal empty-slot account: rows=%d error=%v", n, err)
	}
	if err := ftx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var recoveryStillEmpty bool
	if err := pool.QueryRow(ctx, "SELECT recovery_sealed IS NULL AND recovery_sealed_with IS NULL,sealed_login FROM codex_provider_account WHERE id=$1", frows[0].ID).Scan(&recoveryStillEmpty, &savedLive); err != nil || !recoveryStillEmpty {
		t.Fatalf("empty recovery pair changed: empty=%v error=%v", recoveryStillEmpty, err)
	}
	if string(savedLive) != "new-live" {
		t.Fatal("empty-slot account ciphertext was not replaced")
	}
}

func TestVaultMaterialBatchBytesLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "vault_mode"))
	q := New(pool)
	secretOwner, accountOwner, oversizedOwner := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	const halfMiB = 512 * 1024
	for i := range 3 {
		cjExec(t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with) VALUES ($1,'anthropic_token',$2,$3,'master')",
			secretOwner, fmt.Sprintf("bytes-%d", i), bytes.Repeat([]byte("s"), halfMiB))
		cjExec(t, pool, "INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with,recovery_sealed,recovery_sealed_with) VALUES ($1,$2,'workspace',$3,'master',$4,'master')",
			accountOwner, fmt.Sprintf("bytes-%d", i), bytes.Repeat([]byte("l"), halfMiB/2), bytes.Repeat([]byte("r"), halfMiB/2))
	}
	cjExec(t, pool, "INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with,recovery_sealed,recovery_sealed_with) VALUES ($1,'oversized','workspace',$2,'master',$3,'master')",
		oversizedOwner, bytes.Repeat([]byte("l"), halfMiB+1), bytes.Repeat([]byte("r"), halfMiB))
	// These byte-only query probes use a transaction per owner, in production
	// lock order. The owning row excludes all compliant ciphertext writers.
	for _, owner := range []uuid.UUID{secretOwner, accountOwner, oversizedOwner} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		tq := q.WithTx(tx)
		if err := LockSecretMutation(ctx, tx, owner); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if _, err := tq.LockUserVaultState(ctx, owner); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if owner == secretOwner {
			rows, err := tq.LockVaultSecretMaterialBatch(ctx, LockVaultSecretMaterialBatchParams{UserID: owner})
			if err != nil || len(rows) != 2 {
				_ = tx.Rollback(ctx)
				t.Fatalf("secret byte page: rows=%d error=%v", len(rows), err)
			}
			if got := len(rows[0].Ciphertext) + len(rows[1].Ciphertext); got != 2*halfMiB {
				t.Fatalf("secret byte boundary = %d", got)
			}
		} else {
			rows, err := tq.LockVaultAccountMaterialBatch(ctx, LockVaultAccountMaterialBatchParams{UserID: owner})
			want := 2
			if owner == oversizedOwner {
				want = 0
				totals, terr := tq.GetVaultMaterialTotals(ctx, owner)
				if terr != nil || totals.MaterialRows != 2 || totals.MaxRowBytes != 2*halfMiB+1 {
					_ = tx.Rollback(ctx)
					t.Fatalf("oversized pair preflight: %+v, %v", totals, terr)
				}
			}
			if err != nil || len(rows) != want {
				_ = tx.Rollback(ctx)
				t.Fatalf("account byte page: rows=%d want=%d error=%v", len(rows), want, err)
			}
			if len(rows) == 2 {
				got := len(rows[0].SealedLogin) + len(rows[0].RecoverySealed) + len(rows[1].SealedLogin) + len(rows[1].RecoverySealed)
				if got != 2*halfMiB {
					t.Fatalf("account byte boundary = %d", got)
				}
			}
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
	}
}
