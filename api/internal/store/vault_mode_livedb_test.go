package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Each test owns a database through the migration harness; sharedDB is never
// downgraded or used for fixture data.
func TestVaultModeMigrationBackfillLiveDB(t *testing.T) {
	ctx := context.Background()
	v := migrationVersionByName(t, "vault_mode")
	for _, kind := range []string{"empty", "vaultless", "oidc", "inactive"} {
		t.Run(kind, func(t *testing.T) {
			pool, dsn := codexJudgeMigrationDB(t, v-1)
			var user uuid.UUID
			if kind != "empty" {
				user = cjSeedUser(t, pool, nil)
				if kind == "oidc" {
					cjExec(t, pool, "UPDATE users SET password_hash=NULL, oidc_issuer='https://id.example', oidc_subject='subject' WHERE id=$1", user)
				}
				if kind == "inactive" {
					cjExec(t, pool, "UPDATE users SET is_active=false WHERE id=$1", user)
				}
				cjExec(t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext) VALUES ($1,'anthropic_token','old',$2)", user, []byte("old-secret"))
				cjExec(t, pool, "INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with,recovery_sealed,recovery_sealed_with) VALUES ($1,'principal','workspace',$2,'master',$3,'dek')", user, []byte("old-login"), []byte("old-recovery"))
			}
			if err := MigrateTo(ctx, dsn, v); err != nil {
				t.Fatal(err)
			}
			q := New(pool)
			want := "required"
			if kind == "empty" {
				want = "optional"
			}
			if got, err := q.GetVaultPolicy(ctx); err != nil || got != want {
				t.Fatalf("policy = %q, %v; want %q", got, err, want)
			}
			if kind != "empty" {
				state, err := q.GetUserVaultState(ctx, user)
				if err != nil || state.VaultMode != VaultModeProtected || state.VaultEpoch != 0 {
					t.Fatalf("backfill = %+v, %v", state, err)
				}
				var secret, login, recovery []byte
				var secretMode, loginMode, recoveryMode string
				err = pool.QueryRow(ctx, "SELECT s.ciphertext,s.sealed_with,a.sealed_login,a.sealed_with,a.recovery_sealed,a.recovery_sealed_with FROM user_secrets s JOIN codex_provider_account a USING(user_id) WHERE s.user_id=$1", user).Scan(&secret, &secretMode, &login, &loginMode, &recovery, &recoveryMode)
				if err != nil || !bytes.Equal(secret, []byte("old-secret")) || !bytes.Equal(login, []byte("old-login")) || !bytes.Equal(recovery, []byte("old-recovery")) || secretMode != "master" || loginMode != "master" || recoveryMode != "dek" {
					t.Fatalf("migration changed sealed material: modes %q/%q/%q, error %v", secretMode, loginMode, recoveryMode, err)
				}
			}
			if err := MigrateDownTo(ctx, dsn, v-1); err != nil {
				t.Fatalf("safe downgrade: %v", err)
			}
		})
	}
}

func TestVaultModeQueriesLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "vault_mode"))
	q := New(pool)
	protected := cjSeedUser(t, pool, nil)
	unprotected := cjSeedUser(t, pool, nil)
	for _, user := range []uuid.UUID{protected, unprotected} {
		for _, mode := range []string{SealedWithMaster, SealedWithMasterAAD, SealedWithDEK} {
			cjExec(t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with) VALUES ($1,'anthropic_token',$2,$3,$2)", user, mode, []byte("cipher"))
		}
	}
	for i, mode := range []string{VaultModeUnprotected, VaultModeProtected, VaultModeUnprotected} {
		state, err := q.AdvanceUserVaultMode(ctx, AdvanceUserVaultModeParams{ID: unprotected, VaultMode: mode})
		if err != nil || state.VaultMode != mode || state.VaultEpoch != int64(i+1) {
			t.Fatalf("advance = %+v, %v", state, err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := LockSettingsAdmission(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := LockSecretMutation(ctx, tx, unprotected); err != nil {
		t.Fatal(err)
	}
	state, err := q.WithTx(tx).LockUserVaultState(ctx, unprotected)
	if err != nil || state.VaultMode != VaultModeUnprotected || state.VaultEpoch != 3 {
		t.Fatalf("locked state = %+v, %v", state, err)
	}
	// A competing transaction cannot acquire the state lock. Probe with a read
	// rather than an autocommit UPDATE: a timed-out client's server statement
	// can still finish when the blocker releases, which would mutate this fixture.
	contender, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contender.Rollback(ctx) }()
	waitCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := q.WithTx(contender).LockUserVaultState(waitCtx, unprotected); err == nil {
		t.Fatal("locked user state allowed a competing lock")
	} else if waitCtx.Err() == nil {
		t.Fatalf("competing lock failed before timeout: %v", err)
	}
	_ = contender.Rollback(ctx)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := q.CountUnprotectedUsers(ctx); err != nil || n != 1 {
		t.Fatalf("unprotected count = %d, %v", n, err)
	}
	if n, err := q.CountMasterSealedSecrets(ctx); err != nil || n != 1 {
		t.Fatalf("protected legacy count = %d, %v", n, err)
	}
	for _, sql := range []string{
		"UPDATE users SET vault_mode='invalid' WHERE id=$1",
		"UPDATE users SET vault_epoch=-1 WHERE id=$1",
	} {
		_, err := pool.Exec(ctx, sql, protected)
		vaultModeWantCheckViolation(t, err)
	}
	for _, value := range []string{"optional", "required", "", "OPTIONAL", " optional", "optional ", "invalid"} {
		cjExec(t, pool, "UPDATE app_settings SET value=$1 WHERE key='vault_policy'", value)
		want := "required"
		if value == "optional" {
			want = "optional"
		}
		if got, err := q.GetVaultPolicy(ctx); err != nil || got != want {
			t.Fatalf("stored %q => %q, %v", value, got, err)
		}
	}
	cjExec(t, pool, "DELETE FROM app_settings WHERE key='vault_policy'")
	if got, err := q.GetVaultPolicy(ctx); err != nil || got != "required" {
		t.Fatalf("absent policy = %q, %v", got, err)
	}
}

// Lock probes have a 150ms deadline and no retries; no background work survives
// the test. The owned database keeps these policy writes isolated.
func TestVaultModeSettingsAdmissionLockLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "vault_mode"))
	cjExec(t, pool, "UPDATE app_settings SET value='required' WHERE key='vault_policy'")
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", SettingsMutationLockKey); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, "UPDATE app_settings SET value='optional' WHERE key='vault_policy'"); err != nil {
		t.Fatal(err)
	}
	reader, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	waitCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	err = LockSettingsAdmission(waitCtx, reader)
	waitErr := waitCtx.Err()
	cancel()
	if err == nil || !errors.Is(waitErr, context.DeadlineExceeded) {
		t.Fatalf("admission did not wait for exclusive settings writer: %v", err)
	}
	// A canceled pgx operation may close its connection; either way the
	// aborted transaction must release its resources before the next admission.
	_ = reader.Rollback(ctx)
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	reader, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Rollback(ctx) }()
	if err := LockSettingsAdmission(ctx, reader); err != nil {
		t.Fatal(err)
	}
	// This separate statement observes the writer's newly committed policy.
	if got, err := New(reader).GetVaultPolicy(ctx); err != nil || got != "optional" {
		t.Fatalf("policy after admission = %q, %v", got, err)
	}
	sibling, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sibling.Rollback(ctx) }()
	waitCtx, cancel = context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := LockSettingsAdmission(waitCtx, sibling); err != nil {
		t.Fatalf("shared admissions conflict: %v", err)
	}
	contender, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = contender.Rollback(ctx) }()
	var acquired bool
	if err := contender.QueryRow(ctx, "SELECT pg_try_advisory_xact_lock($1)", SettingsMutationLockKey).Scan(&acquired); err != nil || acquired {
		t.Fatalf("exclusive writer bypassed shared admission: acquired=%v error=%v", acquired, err)
	}
}

func vaultModeWantCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("want check violation, got %v", err)
	}
}

func TestVaultModeDiscriminatorsAndDownGuardLiveDB(t *testing.T) {
	ctx := context.Background()
	v := migrationVersionByName(t, "vault_mode")
	pool, dsn := codexJudgeMigrationDB(t, v)
	user := cjSeedUser(t, pool, nil)
	cjExec(t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with) VALUES ($1,'anthropic_token','secret',$2,'master')", user, []byte("secret"))
	cjExec(t, pool, "INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with) VALUES ($1,'principal','workspace',$2,'master')", user, []byte("login"))
	// Independent states ensure each guard is necessary. Each iteration resets
	// only this owned fixture; a failed downgrade must leave version v usable.
	for _, tc := range []struct {
		name, introduce, clear string
	}{
		{"unprotected", "UPDATE users SET vault_mode='unprotected'", "UPDATE users SET vault_mode='protected'"},
		{"secret", "UPDATE user_secrets SET sealed_with='master_aad'", "UPDATE user_secrets SET sealed_with='master'"},
		{"canonical", "UPDATE codex_provider_account SET sealed_with='master_aad'", "UPDATE codex_provider_account SET sealed_with='master'"},
		{"recovery", "UPDATE codex_provider_account SET recovery_sealed=decode('01','hex'),recovery_sealed_with='master_aad'", "UPDATE codex_provider_account SET recovery_sealed=NULL,recovery_sealed_with=NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cjExec(t, pool, tc.introduce)
			err := MigrateDownTo(ctx, dsn, v-1)
			if err == nil || !strings.Contains(err.Error(), "vault mode rollback refused") {
				t.Fatalf("downgrade guard = %v", err)
			}
			if _, err := New(pool).GetUserVaultState(ctx, user); err != nil {
				t.Fatalf("failed downgrade changed schema: %v", err)
			}
			cjExec(t, pool, tc.clear)
		})
	}
	for _, sql := range []string{
		"UPDATE codex_provider_account SET recovery_sealed=decode('01','hex'),recovery_sealed_with=NULL",
		"UPDATE codex_provider_account SET recovery_sealed=NULL,recovery_sealed_with='master_aad'",
		"UPDATE codex_provider_account SET recovery_sealed=decode('01','hex'),recovery_sealed_with='invalid'",
		"UPDATE codex_provider_account SET sealed_with='invalid'",
		"UPDATE user_secrets SET sealed_with='invalid'",
	} {
		_, err := pool.Exec(ctx, sql)
		vaultModeWantCheckViolation(t, err)
	}
	if err := MigrateDownTo(ctx, dsn, v-1); err != nil {
		t.Fatalf("safe downgrade after clearing all new states: %v", err)
	}
	if _, ok := cjAppValue(t, pool, "vault_policy"); ok {
		t.Fatal("vault_policy survived downgrade")
	}
	if err := MigrateTo(ctx, dsn, v); err != nil {
		t.Fatalf("upgrade after downgrade: %v", err)
	}
}
