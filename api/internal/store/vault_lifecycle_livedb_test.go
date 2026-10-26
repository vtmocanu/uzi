package store_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/vtmocanu/uzi/api/internal/auth"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// This file deliberately has no dependencies on other _test.go files: it can
// run as a named-file package while backend callers migrate to the new API.
const vlLoginPhrase = "vault-live-current-password"

// vlIsolatedPool follows standIsolatedDB's ownership convention, but also accepts
// pgx keyword DSNs. Errors involving connection configuration are kept opaque:
// neither a DSN nor driver diagnostics containing credentials enter test output.
func vlIsolatedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; root owns LiveDB execution")
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("parse LiveDB configuration failed")
	}
	admin, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("open LiveDB admin pool failed")
	}
	name := "vault_lifecycle_" + uuid.New().String()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatal("create owned LiveDB database failed")
	}
	t.Cleanup(func() {
		defer admin.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Error("drop owned LiveDB database failed")
		}
	})
	isolated := config.Copy()
	isolated.ConnConfig.Database = name
	// ConnString retains the original parsed DSN rather than serializing changed
	// fields. Register the changed configuration so migrations target this owned
	// database, exactly like the work pool below.
	migrationDSN := stdlib.RegisterConnConfig(isolated.ConnConfig)
	defer stdlib.UnregisterConnConfig(migrationDSN)
	if err := store.Migrate(ctx, migrationDSN); err != nil {
		t.Fatal("migrate owned LiveDB database failed")
	}
	pool, err := pgxpool.NewWithConfig(ctx, isolated)
	if err != nil {
		t.Fatal("open owned LiveDB pool failed")
	}
	t.Cleanup(pool.Close)
	return pool
}

func vlExec(t *testing.T, p *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := p.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture SQL: %v", err)
	}
}

type vlFixture struct {
	pool   *pgxpool.Pool
	owner  uuid.UUID
	master *secretbox.Box
}

func vlSeed(t *testing.T, pool *pgxpool.Pool, explicit, mixed bool) vlFixture {
	t.Helper()
	ctx := context.Background()
	hash, err := auth.HashPassword(vlLoginPhrase)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	// Raw insertion covers both the schema default and an explicit protected mode.
	if explicit {
		vlExec(t, pool, "INSERT INTO users (id,email,password_hash,vault_mode) VALUES ($1,$2,$3,'protected')", id, id.String()+"@example.test", hash)
	} else {
		vlExec(t, pool, "INSERT INTO users (id,email,password_hash) VALUES ($1,$2,$3)", id, id.String()+"@example.test", hash)
	}
	var policy string
	if err := pool.QueryRow(ctx, "UPDATE app_settings SET value='optional' WHERE key='vault_policy' RETURNING value").Scan(&policy); err != nil || policy != "optional" {
		t.Fatalf("existing vault_policy setting: %v", err)
	}
	master, err := secretbox.New(bytes.Repeat([]byte{0x43}, secretbox.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	f := vlFixture{pool: pool, owner: id, master: master}
	seal := func(kind, text string, aad bool) ([]byte, string) {
		t.Helper()
		if aad {
			// Exact shared owner/kind binding, independently spelled for the input fixture.
			binding := append(append(append([]byte{}, id[:]...), 0), []byte(kind)...)
			ct, err := master.SealWithAAD([]byte(text), binding)
			if err != nil {
				t.Fatal(err)
			}
			return ct, store.SealedWithMasterAAD
		}
		ct, err := master.Seal([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return ct, store.SealedWithMaster
	}
	for i, kind := range []string{store.KindAnthropicToken, store.KindOpenAIAPIKey, store.KindCodexAuth} {
		ct, marker := seal(kind, "secret-"+kind, mixed && i == 1)
		vlExec(t, pool, "INSERT INTO user_secrets (user_id,kind,label,ciphertext,sealed_with,is_default,enablement_rev,auto_eligible) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)",
			id, kind, "label-"+kind, ct, marker, i != 2, 11+i, i == 0)
		if i == 2 {
			vlExec(t, pool, "UPDATE user_secrets SET disabled_at='2026-01-02T03:04:05Z' WHERE user_id=$1 AND kind=$2", id, kind)
		}
	}
	// Two orphan canonical accounts, with no alias links. The second account
	// ensures an account-stage failure can follow a completed account write.
	for i := range 2 {
		live, lm := seal(store.KindCodexAuth, fmt.Sprintf("live-%d", i), mixed && i == 0)
		recovery, rm := seal(store.KindCodexAuth, fmt.Sprintf("recovery-%d", i), mixed && i == 1)
		vlExec(t, pool, "INSERT INTO codex_provider_account (user_id,provider_user_id,workspace_account_id,sealed_login,sealed_with,recovery_sealed,recovery_sealed_with,generation,credential_revision,recovery_generation,coord_state,recovery_cause,coord_operation_id,committed_generation) VALUES ($1,$2,'workspace',$3,$4,$5,$6,7,9,6,'quarantined','vault_locked',$7,7)",
			id, fmt.Sprintf("principal-%d", i), live, lm, recovery, rm, uuid.New())
	}
	state, err := store.New(pool).GetUserVaultState(ctx, id)
	if err != nil || state.VaultMode != store.VaultModeProtected || state.VaultEpoch != 0 {
		t.Fatalf("raw protected initial state: %+v, %v", state, err)
	}
	totals, err := store.New(pool).GetVaultMaterialTotals(ctx, id)
	if err != nil || totals.MaterialRows != 7 {
		t.Fatalf("totals including disabled/orphan slots: %+v, %v", totals, err)
	}
	return f
}

// JSONB captures every column, including timestamps and future metadata. Full
// snapshots include user mode/epoch/hash and wrappers; metadata excludes only
// the four account crypto columns and the two personal-secret crypto columns.
func (f vlFixture) snapshot(t *testing.T, metadata bool) []byte {
	t.Helper()
	personalSQL, accountSQL := "to_jsonb(s)", "to_jsonb(a)"
	if metadata {
		personalSQL += "-ARRAY['ciphertext','sealed_with']"
		accountSQL += "-ARRAY['sealed_login','sealed_with','recovery_sealed','recovery_sealed_with']"
	}
	query := "SELECT jsonb_build_object('secrets',coalesce((SELECT jsonb_agg(" + personalSQL + " ORDER BY s.id) FROM user_secrets s WHERE user_id=$1),'[]'::jsonb),'accounts',coalesce((SELECT jsonb_agg(" + accountSQL + " ORDER BY a.id) FROM codex_provider_account a WHERE user_id=$1),'[]'::jsonb)"
	if metadata {
		query += ",'user_metadata',(SELECT to_jsonb(u)-ARRAY['vault_mode','vault_epoch'] FROM users u WHERE id=$1)"
	} else {
		query += ",'user',(SELECT to_jsonb(u) FROM users u WHERE id=$1),'wrapper',(SELECT to_jsonb(v) FROM user_vaults v WHERE user_id=$1)"
	}
	query += ")"
	var out []byte
	if err := f.pool.QueryRow(context.Background(), query, f.owner).Scan(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func vlEqual(t *testing.T, before, after []byte, what string) {
	t.Helper()
	if !bytes.Equal(before, after) {
		t.Fatalf("%s changed", what)
	}
}

func (f vlFixture) state(t *testing.T, v *vault.Vault, mode string, epoch int64, cached, wrapper bool) {
	t.Helper()
	ctx := context.Background()
	state, err := v.State(ctx, f.owner)
	if err != nil || state.VaultMode != mode || state.VaultEpoch != epoch {
		t.Fatalf("state = %+v, %v; want %s/%d", state, err, mode, epoch)
	}
	exists, err := v.Exists(ctx, f.owner)
	if err != nil || exists != wrapper || v.Unlocked(f.owner) != cached {
		t.Fatalf("wrapper/cache: exists=%v cached=%v error=%v", exists, v.Unlocked(f.owner), err)
	}
}

func (f vlFixture) readable(t *testing.T, v *vault.Vault, marker string) {
	t.Helper()
	ctx := context.Background()
	rows, err := f.pool.Query(ctx, "SELECT kind,label,ciphertext,sealed_with FROM user_secrets WHERE user_id=$1 ORDER BY id", f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	check := func(kind, gotMarker string, ct []byte, want string) {
		t.Helper()
		if gotMarker != marker {
			t.Fatalf("marker=%s want=%s", gotMarker, marker)
		}
		plain, err := v.Open(f.owner, kind, gotMarker, ct)
		if err != nil || string(plain) != want {
			t.Fatalf("open %s: %v; plaintext matches=%v", kind, err, string(plain) == want)
		}
		if _, err := v.Open(uuid.New(), kind, gotMarker, ct); err == nil {
			t.Fatal("cross-owner open succeeded")
		}
		other := store.KindAnthropicToken
		if kind == other {
			other = store.KindOpenAIAPIKey
		}
		if _, err := v.Open(f.owner, other, gotMarker, ct); err == nil {
			t.Fatal("cross-kind open succeeded")
		}
		if marker == store.SealedWithDEK {
			if _, err := vault.OpenMasterMaterial(f.master, f.owner, kind, store.SealedWithMasterAAD, ct); err == nil {
				t.Fatal("master opened private DEK material")
			}
		} else {
			aad := append(append(append([]byte{}, f.owner[:]...), 0), []byte(kind)...)
			plain, err := f.master.OpenWithAAD(ct, aad)
			if err != nil || string(plain) != want {
				t.Fatal("disabled material did not use exact owner/kind master AAD")
			}
		}
	}
	for rows.Next() {
		var kind, label, m string
		var ct []byte
		if err := rows.Scan(&kind, &label, &ct, &m); err != nil {
			t.Fatal(err)
		}
		if label != "label-"+kind {
			t.Fatal("secret identity changed")
		}
		check(kind, m, ct, "secret-"+kind)
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("personal secret rows=%d", n)
	}
	accounts, err := f.pool.Query(ctx, "SELECT provider_user_id,sealed_login,sealed_with,recovery_sealed,recovery_sealed_with FROM codex_provider_account WHERE user_id=$1 ORDER BY id", f.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer accounts.Close()
	n = 0
	for accounts.Next() {
		var principal, lm, rm string
		var live, recovery []byte
		if err := accounts.Scan(&principal, &live, &lm, &recovery, &rm); err != nil {
			t.Fatal(err)
		}
		var i int
		if _, err := fmt.Sscanf(principal, "principal-%d", &i); err != nil {
			t.Fatal(err)
		}
		check(store.KindCodexAuth, lm, live, fmt.Sprintf("live-%d", i))
		check(store.KindCodexAuth, rm, recovery, fmt.Sprintf("recovery-%d", i))
		n++
	}
	if err := accounts.Err(); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("orphan canonical accounts=%d", n)
	}
}

func TestVaultLifecycleRoundTripLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	for _, explicit := range []bool{false, true} {
		for _, mixed := range []bool{false, true} {
			t.Run(fmt.Sprintf("explicit=%v/mixed=%v", explicit, mixed), func(t *testing.T) {
				ctx := context.Background()
				f := vlSeed(t, pool, explicit, mixed)
				metadata := f.snapshot(t, true)
				v := vault.NewPostgres(f.master, pool)
				if !v.RequiresUnlock(ctx, f.owner) {
					t.Fatal("initial protected owner did not require unlock")
				}
				if err := v.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				f.state(t, v, store.VaultModeProtected, 1, true, true)
				f.readable(t, v, store.SealedWithDEK)
				vlEqual(t, metadata, f.snapshot(t, true), "unlock metadata")
				// Existing-wrapper unlock exercises both the no-work fast path and
				// locked, current-material rewrap of personal and BOTH account slots.
				if err := v.UnlockExisting(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				legacy := func(value string) []byte {
					t.Helper()
					ct, err := f.master.Seal([]byte(value))
					if err != nil {
						t.Fatal(err)
					}
					return ct
				}
				vlExec(t, pool, "UPDATE user_secrets SET ciphertext=$2,sealed_with='master' WHERE user_id=$1 AND kind='anthropic_token'", f.owner, legacy("secret-anthropic_token"))
				vlExec(t, pool, "UPDATE codex_provider_account SET sealed_login=$2,sealed_with='master',recovery_sealed=$3,recovery_sealed_with='master' WHERE user_id=$1 AND provider_user_id='principal-0'", f.owner, legacy("live-0"), legacy("recovery-0"))
				v.Lock(f.owner)
				if err := v.UnlockExisting(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				f.state(t, v, store.VaultModeProtected, 1, true, true)
				f.readable(t, v, store.SealedWithDEK)
				vlEqual(t, metadata, f.snapshot(t, true), "existing unlock metadata")
				if err := v.Disable(ctx, f.owner); err != nil {
					t.Fatal(err)
				}
				f.state(t, v, store.VaultModeUnprotected, 2, false, false)
				f.readable(t, v, store.SealedWithMasterAAD)
				vlEqual(t, metadata, f.snapshot(t, true), "disable metadata")

				restarted := vault.NewPostgres(f.master, pool)
				if restarted.RequiresUnlock(ctx, f.owner) {
					t.Fatal("off owner required unlock after restart")
				}
				f.readable(t, restarted, store.SealedWithMasterAAD)
				offSeal, err := restarted.Seal(ctx, f.owner, store.KindAnthropicToken, []byte("off snapshot"), vault.SealForUse)
				if err != nil || offSeal.Mode != store.VaultModeUnprotected || offSeal.Epoch != 2 || offSeal.SealedWith != store.SealedWithMasterAAD || offSeal.RecoveryMaster {
					t.Fatalf("off policy seal snapshot: %v", err)
				}
				before := f.snapshot(t, false)
				if err := restarted.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				vlEqual(t, before, f.snapshot(t, false), "off unlock durable snapshot")
				f.state(t, restarted, store.VaultModeUnprotected, 2, false, false)
				if err := restarted.Enable(ctx, f.owner, "wrong-current-password"); !errors.Is(err, vault.ErrWrongPassword) {
					t.Fatalf("wrong password: %v", err)
				}
				vlEqual(t, before, f.snapshot(t, false), "wrong-password enable snapshot")
				f.state(t, restarted, store.VaultModeUnprotected, 2, false, false)
				if err := restarted.Enable(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				f.state(t, restarted, store.VaultModeProtected, 3, true, true)
				f.readable(t, restarted, store.SealedWithDEK)
				vlEqual(t, metadata, f.snapshot(t, true), "enable metadata")
				cold := vault.NewPostgres(f.master, pool)
				if !cold.RequiresUnlock(ctx, f.owner) || cold.Unlocked(f.owner) {
					t.Fatal("protected restart retained private key")
				}
				if _, err := cold.Seal(ctx, f.owner, store.KindAnthropicToken, []byte("blocked"), vault.SealForUse); !errors.Is(err, vault.ErrLocked) {
					t.Fatalf("cold protected Seal: %v", err)
				}
			})
		}
	}
}

// vlWrite is an ordinary ciphertext writer: advisory exclusion, typed fence,
// then persistence. A stale snapshot must return before the UPDATE.
func vlWrite(ctx context.Context, pool *pgxpool.Pool, owner uuid.UUID, sealed vault.SealedMaterial) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := q.LockSecretMutation(ctx, owner); err != nil {
		return err
	}
	if _, err := q.FenceVaultWrite(ctx, owner, sealed.Mode, sealed.Epoch); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, "UPDATE user_secrets SET ciphertext=$2,sealed_with=$3 WHERE user_id=$1 AND kind='anthropic_token'", owner, sealed.Ciphertext, sealed.SealedWith)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("ordinary writer did not update exactly one row")
	}
	return tx.Commit(ctx)
}

func TestVaultLifecycleFenceLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	ctx := context.Background()
	f := vlSeed(t, pool, true, true)
	v := vault.NewPostgres(f.master, pool)
	if err := v.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
		t.Fatal(err)
	}
	old, err := v.Seal(ctx, f.owner, store.KindAnthropicToken, []byte("old-DEK-A"), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	if old.Mode != store.VaultModeProtected || old.Epoch != 1 || old.SealedWith != store.SealedWithDEK {
		t.Fatal("unexpected DEK A seal snapshot")
	}
	if err := v.Disable(ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	if err := v.Enable(ctx, f.owner, vlLoginPhrase); err != nil {
		t.Fatal(err)
	}
	current, err := v.Seal(ctx, f.owner, store.KindAnthropicToken, []byte("current-DEK-B"), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	if current.Mode != old.Mode || current.Epoch != 3 || current.SealedWith != store.SealedWithDEK {
		t.Fatal("unexpected DEK B seal snapshot")
	}
	if _, err := v.Open(f.owner, store.KindAnthropicToken, old.SealedWith, old.Ciphertext); err == nil {
		t.Fatal("DEK B opened DEK A snapshot")
	}
	before := f.snapshot(t, false)
	wrongMode := current
	wrongMode.Mode = store.VaultModeUnprotected
	if err := vlWrite(ctx, pool, f.owner, wrongMode); !errors.Is(err, store.ErrVaultSealStale) {
		t.Fatalf("same epoch with wrong mode: %v", err)
	}
	vlEqual(t, before, f.snapshot(t, false), "wrong-mode writer durable snapshot")
	if err := vlWrite(ctx, pool, f.owner, old); !errors.Is(err, store.ErrVaultSealStale) {
		t.Fatalf("stale ordinary writer: %v", err)
	}
	vlEqual(t, before, f.snapshot(t, false), "stale writer durable snapshot")
	if err := vlWrite(ctx, pool, f.owner, current); err != nil {
		t.Fatalf("current fence: %v", err)
	}
	var ct []byte
	var marker string
	if err := pool.QueryRow(ctx, "SELECT ciphertext,sealed_with FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", f.owner).Scan(&ct, &marker); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ct, current.Ciphertext) || marker != current.SealedWith {
		t.Fatal("current writer did not persist exact seal")
	}
	plain, err := v.Open(f.owner, store.KindAnthropicToken, marker, ct)
	if err != nil || string(plain) != "current-DEK-B" {
		t.Fatalf("current persisted seal unreadable: %v", err)
	}
	q := store.New(pool)
	if _, err := q.FenceVaultWrite(ctx, f.owner, current.Mode, current.Epoch); !errors.Is(err, store.ErrVaultTransactionRequired) {
		t.Fatalf("autocommit fence: %v", err)
	}
	if err := q.LockSettingsAdmission(ctx); !errors.Is(err, store.ErrVaultTransactionRequired) {
		t.Fatalf("autocommit admission: %v", err)
	}
	if err := q.LockSecretMutation(ctx, f.owner); !errors.Is(err, store.ErrVaultTransactionRequired) {
		t.Fatalf("autocommit mutation: %v", err)
	}
}

var errVLInjected = errors.New("vault LiveDB injected after real statement")

type vlFailStore struct {
	*store.Queries
	stage         string
	writes        *[]string
	hit           *bool
	accountWrites int
}

func (s *vlFailStore) after(stage string, err error) error {
	if err != nil {
		return err
	}
	*s.writes = append(*s.writes, stage)
	if stage == "account" {
		s.accountWrites++
	}
	// Fail after the second account write, so even disable has a prior write.
	if s.stage == stage && (stage != "account" || s.accountWrites >= 2) {
		*s.hit = true
		return errVLInjected
	}
	return nil
}
func (s *vlFailStore) ResealVaultAccountMaterial(ctx context.Context, p store.ResealVaultAccountMaterialParams) (int64, error) {
	n, err := s.Queries.ResealVaultAccountMaterial(ctx, p)
	if err == nil && n != 1 {
		return n, fmt.Errorf("account rows=%d", n)
	}
	return n, s.after("account", err)
}
func (s *vlFailStore) ResealVaultSecretMaterial(ctx context.Context, p store.ResealVaultSecretMaterialParams) (int64, error) {
	n, err := s.Queries.ResealVaultSecretMaterial(ctx, p)
	if err == nil && n != 1 {
		return n, fmt.Errorf("secret rows=%d", n)
	}
	return n, s.after("secret", err)
}
func (s *vlFailStore) AdvanceUserVaultMode(ctx context.Context, p store.AdvanceUserVaultModeParams) (store.AdvanceUserVaultModeRow, error) {
	row, err := s.Queries.AdvanceUserVaultMode(ctx, p)
	return row, s.after("advance", err)
}
func (s *vlFailStore) CreateUserVaultIfAbsent(ctx context.Context, p store.CreateUserVaultIfAbsentParams) (store.UserVault, error) {
	row, err := s.Queries.CreateUserVaultIfAbsent(ctx, p)
	return row, s.after("create", err)
}
func (s *vlFailStore) DeleteUserVault(ctx context.Context, id uuid.UUID) (int64, error) {
	n, err := s.Queries.DeleteUserVault(ctx, id)
	if err == nil && n != 1 {
		return n, fmt.Errorf("wrapper delete rows=%d", n)
	}
	return n, s.after("delete", err)
}

func TestVaultLifecycleRollbackLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	for _, direction := range []string{"enable", "disable"} {
		stages := []string{"account", "secret", "advance", "create"}
		if direction == "disable" {
			stages = []string{"account", "secret", "advance", "delete"}
		}
		for _, stage := range stages {
			t.Run(direction+"/"+stage, func(t *testing.T) {
				ctx := context.Background()
				f := vlSeed(t, pool, true, true)
				setup := vault.NewPostgres(f.master, pool)
				if err := setup.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
					t.Fatal(err)
				}
				if direction == "enable" {
					if err := setup.Disable(ctx, f.owner); err != nil {
						t.Fatal(err)
					}
				}
				var writes []string
				hit := false
				armed := false
				adapter := func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
					tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					if err != nil {
						return err
					}
					defer func() { _ = tx.Rollback(context.Background()) }()
					wrapped := &vlFailStore{Queries: store.New(tx), writes: &writes, hit: &hit}
					if armed {
						wrapped.stage = stage
					}
					if err := fn(vault.WithStateReader(ctx, wrapped), wrapped); err != nil {
						return err
					}
					return tx.Commit(ctx)
				}
				v := vault.New(f.master, store.New(pool), adapter)
				if direction == "disable" {
					if err := v.UnlockExisting(ctx, f.owner, vlLoginPhrase); err != nil {
						t.Fatal(err)
					}
				}
				writes = nil
				armed = true
				before := f.snapshot(t, false)
				cached := v.Unlocked(f.owner)
				var probe vault.SealedMaterial
				if cached {
					var err error
					probe, err = v.Seal(ctx, f.owner, store.KindAnthropicToken, []byte("cache survives rollback"), vault.SealForUse)
					if err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if direction == "enable" {
					err = v.Enable(ctx, f.owner, vlLoginPhrase)
				} else {
					err = v.Disable(ctx, f.owner)
				}
				if !errors.Is(err, errVLInjected) || !hit {
					t.Fatalf("did not reach post-statement injection: %v; writes=%v", err, writes)
				}
				if (stage == "account" || stage == "secret") && len(writes) < 2 {
					t.Fatalf("no preceding write before %s failure: %v", stage, writes)
				}
				vlEqual(t, before, f.snapshot(t, false), "real PG rollback full snapshot")
				if v.Unlocked(f.owner) != cached {
					t.Fatal("rollback changed cached key presence")
				}
				if cached {
					plain, err := v.Open(f.owner, store.KindAnthropicToken, probe.SealedWith, probe.Ciphertext)
					if err != nil || string(plain) != "cache survives rollback" {
						t.Fatalf("rollback replaced cached key: %v", err)
					}
					f.state(t, v, store.VaultModeProtected, 1, true, true)
					f.readable(t, v, store.SealedWithDEK)
				} else {
					f.state(t, v, store.VaultModeUnprotected, 2, false, false)
					// A separate instance sees the same durable state; no failed
					// enable wrapper or private publication may escape rollback.
					cold := vault.NewPostgres(f.master, pool)
					if cold.RequiresUnlock(ctx, f.owner) {
						t.Fatal("failed enable left protected durable state")
					}
					f.readable(t, v, store.SealedWithMasterAAD)
				}
				t.Logf("post-statement rollback observed; executed writes: %v", writes)
			})
		}
	}
}
