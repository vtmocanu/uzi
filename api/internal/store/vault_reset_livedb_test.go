package store_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// PRD #3989: vault.Reset against a real PostgreSQL. These tests reuse the
// vault-lifecycle fixtures (vlIsolatedPool, vlSeed, vlExec, vlWrite) from
// vault_lifecycle_livedb_test.go and, like it, skip without UZI_TEST_DATABASE_URL.

func vrNoop(context.Context, vault.BrowserStore) error { return nil }

// vrIDs are the rows vrBind creates for one owner.
type vrIDs struct {
	anthropicSecret, codexSecret, overrideSecret, deadSecret uuid.UUID
	worker, runClaude, runCodex                              uuid.UUID
	accounts                                                 []uuid.UUID
}

// vrBind adds, on top of vlSeed's secrets and accounts, everything a reset must
// null or cascade: a master_aad-sealed row (a reset must delete rows of every
// sealing marker), a linked Codex credential state, a worker and two runs bound
// to the secrets, a browser slot and a sidebar account id.
func vrBind(t *testing.T, f vlFixture, v *vault.Vault) vrIDs {
	t.Helper()
	ctx := context.Background()
	var ids vrIDs
	scan := func(dst *uuid.UUID, sql string, args ...any) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
			t.Fatalf("fixture lookup: %v", err)
		}
	}
	scan(&ids.anthropicSecret, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='anthropic_token'", f.owner)
	scan(&ids.codexSecret, "SELECT id FROM user_secrets WHERE user_id=$1 AND kind='codex_auth'", f.owner)
	// An extra legacy master_aad row: Reset deletes it like any other.
	binding := append(append(append([]byte{}, f.owner[:]...), 0), []byte(store.KindAnthropicToken)...)
	ct, err := f.master.SealWithAAD([]byte("legacy master aad row"), binding)
	if err != nil {
		t.Fatal(err)
	}
	ids.overrideSecret = uuid.New()
	vlExec(t, f.pool, "INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,$3,'legacy-master-aad',$4,$5)",
		ids.overrideSecret, f.owner, store.KindAnthropicToken, ct, store.SealedWithMasterAAD)
	ids.deadSecret = uuid.New()
	vlExec(t, f.pool, "INSERT INTO user_secrets (id,user_id,kind,label,ciphertext,sealed_with) VALUES ($1,$2,$3,'legacy-master-aad-2',$4,$5)",
		ids.deadSecret, f.owner, store.KindAnthropicToken, ct, store.SealedWithMasterAAD)

	rows, err := f.pool.Query(ctx, "SELECT id FROM codex_provider_account WHERE user_id=$1 ORDER BY id", f.owner)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids.accounts = append(ids.accounts, id)
	}
	rows.Close()
	if len(ids.accounts) != 2 {
		t.Fatalf("seeded accounts = %d, want 2", len(ids.accounts))
	}
	vlExec(t, f.pool, "INSERT INTO codex_credential_state (user_secret_id,user_id,status,provider_account_id) VALUES ($1,$2,'linked',$3)",
		ids.codexSecret, f.owner, ids.accounts[0])
	vlExec(t, f.pool, "UPDATE users SET sidebar_codex_account_ids=$2 WHERE id=$1", f.owner, ids.accounts)

	ids.worker = uuid.New()
	vlExec(t, f.pool, "INSERT INTO workers (id,user_id,name,token_hash,status) VALUES ($1,$2,'reset-fixture',$3,'online')", ids.worker, f.owner, ids.worker[:])
	vlExec(t, f.pool, "UPDATE workers SET anthropic_secret_id=$2 WHERE id=$1", ids.worker, ids.anthropicSecret)
	ids.runClaude, ids.runCodex = uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{ids.runClaude, ids.runCodex} {
		vlExec(t, f.pool, "INSERT INTO runs (id,user_id,kind,status,issue_title,issue_description) VALUES ($1,$2,'chat','completed','t','d')", id, f.owner)
	}
	vlExec(t, f.pool, "UPDATE runs SET anthropic_secret_id=$2, limit_dead_secret_id=$3 WHERE id=$1", ids.runClaude, ids.anthropicSecret, ids.deadSecret)
	vlExec(t, f.pool, "UPDATE runs SET harness='codex', codex_secret_id=$2, credential_override_secret_id=$3 WHERE id=$1", ids.runCodex, ids.codexSecret, ids.overrideSecret)

	state, err := v.State(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	vlExec(t, f.pool, "INSERT INTO vault_browser_slots (id,user_id,browser_id,wrapped_dek,created_at,expires_at,token_version,vault_epoch,generation) VALUES ($1,$2,$3,'\\x01',now(),now()+interval '1 hour',0,$4,0)",
		uuid.New(), f.owner, uuid.New(), state.VaultEpoch)
	return ids
}

func vrCount(t *testing.T, p *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := p.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func TestVaultResetLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	ctx := context.Background()
	f := vlSeed(t, pool, true, true)
	other := vlSeed(t, pool, true, true)
	v := vault.NewPostgres(f.master, pool)
	for _, owner := range []uuid.UUID{f.owner, other.owner} {
		if err := v.Unlock(ctx, owner, vlLoginPhrase); err != nil {
			t.Fatal(err)
		}
	}
	ids := vrBind(t, f, v)
	otherBefore := other.snapshot(t, false)
	// A seal taken before the reset: its epoch must be fenced out afterwards.
	old, err := v.Seal(ctx, f.owner, store.KindAnthropicToken, []byte("sealed before reset"), vault.SealForUse)
	if err != nil {
		t.Fatal(err)
	}
	before, err := v.State(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM vault_browser_slots WHERE user_id=$1", f.owner); n != 1 {
		t.Fatalf("fixture browser slots = %d", n)
	}
	// Further rows the secret and account deletes must null or cascade.
	vlExec(t, pool, "UPDATE users SET judge_anthropic_secret_id=$2 WHERE id=$1", f.owner, ids.anthropicSecret)
	vlExec(t, pool, "INSERT INTO anthropic_rate_limits (user_secret_id,user_id,synced_at) VALUES ($1,$2,now())", ids.anthropicSecret, f.owner)
	conn, repo, schedule, epochRun := uuid.New(), uuid.New(), uuid.New(), ids.runCodex
	vlExec(t, pool, "INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext) VALUES ($1,$2,'gitlab','https://forge.test','bot',1,$3)", conn, f.owner, []byte{1})
	vlExec(t, pool, "INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch) VALUES ($1,$2,1,'g/r','https://forge.test/g/r','main')", repo, conn)
	vlExec(t, pool, `INSERT INTO run_schedules (id,user_id,repo_id,target,issue_iid,timing,run_at,next_fire_at,timezone,auto_approve,wait_on_limit,enabled,status,origin,harness,credential_override_secret_id)
		VALUES ($1,$2,$3,'issue',1,'once',now(),now(),'UTC',true,false,false,'active','user','claude',$4)`, schedule, f.owner, repo, ids.overrideSecret)
	vlExec(t, pool, "INSERT INTO run_credential_epochs (run_id,claim_generation,secret_id) VALUES ($1,1,$2)", epochRun, ids.anthropicSecret)
	vlExec(t, pool, "INSERT INTO codex_account_rate_limits (user_id,provider_account_id) VALUES ($1,$2)", f.owner, ids.accounts[0])
	vlExec(t, pool, "INSERT INTO codex_refresh_intent (operation_id,user_id,provider_account_id,from_generation,state) VALUES ($1,$2,$3,0,'rotating')", uuid.New(), f.owner, ids.accounts[0])

	if err := v.Reset(ctx, f.owner, vrNoop); err != nil {
		t.Fatalf("Reset: %v", err)
	}

	for table, col := range map[string]string{
		"user_secrets": "user_id", "codex_provider_account": "user_id", "user_vaults": "user_id",
		"vault_browser_slots": "user_id", "codex_credential_state": "user_id",
	} {
		if n := vrCount(t, pool, "SELECT count(*) FROM "+table+" WHERE "+col+"=$1", f.owner); n != 0 {
			t.Errorf("%s rows after reset = %d, want 0", table, n)
		}
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM workers WHERE id=$1 AND anthropic_secret_id IS NULL", ids.worker); n != 1 {
		t.Error("worker binding not nulled")
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM runs WHERE user_id=$1 AND anthropic_secret_id IS NULL AND codex_secret_id IS NULL AND limit_dead_secret_id IS NULL AND credential_override_secret_id IS NULL", f.owner); n != 2 {
		t.Errorf("runs with all four bindings nulled = %d, want 2 (rows kept)", n)
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM users WHERE id=$1 AND sidebar_codex_account_ids IS NULL", f.owner); n != 1 {
		t.Error("sidebar codex account ids not cleared")
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM users WHERE id=$1 AND judge_anthropic_secret_id IS NULL", f.owner); n != 1 {
		t.Error("judge anthropic secret binding not nulled")
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM anthropic_rate_limits WHERE user_id=$1", f.owner); n != 0 {
		t.Errorf("anthropic_rate_limits rows after reset = %d, want 0", n)
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM run_schedules WHERE id=$1 AND credential_override_secret_id IS NULL", schedule); n != 1 {
		t.Error("schedule credential override not nulled (row must be kept)")
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM run_credential_epochs WHERE run_id=$1 AND secret_id IS NULL", epochRun); n != 1 {
		t.Error("run credential epoch secret_id not nulled (row must be kept)")
	}
	for _, table := range []string{"codex_account_rate_limits", "codex_refresh_intent"} {
		if n := vrCount(t, pool, "SELECT count(*) FROM "+table+" WHERE user_id=$1", f.owner); n != 0 {
			t.Errorf("%s rows after reset = %d, want 0", table, n)
		}
	}
	after, err := v.State(ctx, f.owner)
	if err != nil || after.VaultMode != store.VaultModeProtected || after.VaultEpoch != before.VaultEpoch+1 {
		t.Fatalf("state after reset = %+v, %v; want protected epoch %d", after, err, before.VaultEpoch+1)
	}
	if v.Unlocked(f.owner) {
		t.Error("cached DEK survived reset")
	}
	vlEqual(t, otherBefore, other.snapshot(t, false), "unrelated user's rows")

	// A seal started before the reset is refused after it.
	if err := vlWrite(ctx, pool, f.owner, old); !errors.Is(err, store.ErrVaultSealStale) {
		t.Fatalf("pre-reset seal write = %v, want ErrVaultSealStale", err)
	}
	// A Codex recovery-slot write for a deleted account touches nothing and
	// recreates nothing.
	q := store.New(pool)
	n, err := q.SetCodexRecoverySlot(ctx, store.SetCodexRecoverySlotParams{
		ID: ids.accounts[0], UserID: f.owner, Op: uuid.New(), Sealed: []byte("late recovery"),
		RecoverySealedWith: pgconv.Text(store.SealedWithMaster),
	})
	if err != nil || n != 0 {
		t.Fatalf("SetCodexRecoverySlot after reset = (%d,%v), want (0,nil)", n, err)
	}
	if c := vrCount(t, pool, "SELECT count(*) FROM codex_provider_account WHERE user_id=$1", f.owner); c != 0 {
		t.Fatalf("%d account rows remain after the refused recovery write", c)
	}

	// Idempotent on an owner with nothing left to delete; the owner can then
	// enroll a fresh vault through the ordinary create path.
	if err := v.Reset(ctx, f.owner, vrNoop); err != nil {
		t.Fatalf("second Reset: %v", err)
	}
	if err := v.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
		t.Fatalf("fresh enrollment after reset: %v", err)
	}
	if exists, err := v.Exists(ctx, f.owner); err != nil || !exists {
		t.Fatalf("new wrapper after reset: %v, %v", exists, err)
	}
	vlEqual(t, otherBefore, other.snapshot(t, false), "unrelated user's rows after second reset")
}

// TestVaultResetAtomicLiveDB: the caller's writes and every delete commit or roll
// back together, including a failure injected after the last statement.
func TestVaultResetAtomicLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	ctx := context.Background()
	f := vlSeed(t, pool, true, true)
	v := vault.NewPostgres(f.master, pool)
	if err := v.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
		t.Fatal(err)
	}
	vrBind(t, f, v)
	before, err := v.State(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := f.snapshot(t, false)
	var tokenVersion int32
	if err := pool.QueryRow(ctx, "SELECT token_version FROM users WHERE id=$1", f.owner).Scan(&tokenVersion); err != nil {
		t.Fatal(err)
	}
	intact := func(what string) {
		t.Helper()
		vlEqual(t, snapshot, f.snapshot(t, false), what)
		if n := vrCount(t, pool, "SELECT count(*) FROM runs WHERE user_id=$1 AND (anthropic_secret_id IS NOT NULL OR codex_secret_id IS NOT NULL)", f.owner); n != 2 {
			t.Fatalf("%s: run bindings changed (%d bound)", what, n)
		}
		if n := vrCount(t, pool, "SELECT count(*) FROM vault_browser_slots WHERE user_id=$1", f.owner); n != 1 {
			t.Fatalf("%s: browser slot deleted", what)
		}
	}

	boom := errors.New("callback refused")
	err = v.Reset(ctx, f.owner, func(ctx context.Context, q vault.BrowserStore) error {
		if _, err := q.BumpTokenVersion(ctx, f.owner); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Reset = %v, want the callback error", err)
	}
	intact("failed callback")

	// Fail after every delete and the epoch advance ran: Postgres must roll them all back.
	failing := vault.New(f.master, store.New(pool), func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		tq := store.New(tx)
		if err := fn(vault.WithStateReader(ctx, tq), tq); err != nil {
			return err
		}
		return errors.New("injected failure after the last statement")
	})
	if err := failing.Reset(ctx, f.owner, vrNoop); err == nil {
		t.Fatal("injected failure was swallowed")
	}
	intact("failure after all statements")
	if state, err := v.State(ctx, f.owner); err != nil || state != before {
		t.Fatalf("state changed by rolled-back reset: %+v, %v", state, err)
	}

	// Success: the caller's write commits with the reset.
	err = v.Reset(ctx, f.owner, func(ctx context.Context, q vault.BrowserStore) error {
		_, err := q.BumpTokenVersion(ctx, f.owner)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var got int32
	if err := pool.QueryRow(ctx, "SELECT token_version FROM users WHERE id=$1", f.owner).Scan(&got); err != nil || got != tokenVersion+1 {
		t.Fatalf("token_version = %d, %v; want %d (callback write committed once)", got, err, tokenVersion+1)
	}
}

// vrCountingVault returns a vault over pool whose transactions are counted, so a
// test can tell a deadlock-victim retry (attempts > 1) from a clean first attempt.
func vrCountingVault(f vlFixture, pool *pgxpool.Pool) (*vault.Vault, *atomic.Int32) {
	var attempts atomic.Int32
	v := vault.New(f.master, store.New(pool), func(ctx context.Context, fn func(context.Context, vault.Store) error) error {
		attempts.Add(1)
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(context.Background()) }()
		tq := store.New(tx)
		if err := fn(vault.WithStateReader(ctx, tq), tq); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return &vault.CommitError{Err: err}
		}
		return nil
	})
	return v, &attempts
}

// TestVaultResetPinnedRunCreationLiveDB: run creation with a pinned credential
// override share-locks the user_secrets row and then its runs INSERT takes KEY
// SHARE on users. Reset must hold the secret rows before it locks users, or its
// secret DELETE waits on the creator while the creator waits on users (40P01).
func TestVaultResetPinnedRunCreationLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := vlSeed(t, pool, true, true)
	base := vault.NewPostgres(f.master, pool)
	if err := base.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
		t.Fatal(err)
	}
	ids := vrBind(t, f, base)
	counting, attempts := vrCountingVault(f, pool)

	creator, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = creator.Rollback(context.Background()) }()
	var locked uuid.UUID
	if err := creator.QueryRow(ctx, "SELECT id FROM user_secrets WHERE id=$1 FOR SHARE", ids.anthropicSecret).Scan(&locked); err != nil {
		t.Fatal(err)
	}

	resetErr := make(chan error, 1)
	go func() { resetErr <- counting.Reset(ctx, f.owner, vrNoop) }()
	deadline := time.Now().Add(15 * time.Second)
	for vrCount(t, pool, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Reset never blocked on the creator's secret share lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The creator now inserts its run (KEY SHARE on users via runs_user_id_fkey).
	_, insErr := creator.Exec(ctx, "INSERT INTO runs (id,user_id,kind,status,issue_title,issue_description,credential_override_secret_id) VALUES ($1,$2,'chat','completed','t','d',$3)",
		uuid.New(), f.owner, ids.anthropicSecret)
	if insErr == nil {
		insErr = creator.Commit(ctx)
	}
	if insErr != nil {
		t.Fatalf("run creation was aborted by Reset (deadlock?): %v", insErr)
	}
	if err := <-resetErr; err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("Reset transaction attempts = %d, want 1 (a retry means it was a deadlock victim)", got)
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM runs WHERE user_id=$1 AND credential_override_secret_id IS NOT NULL", f.owner); n != 0 {
		t.Fatalf("%d runs still pin a deleted secret", n)
	}
}

// TestVaultResetLockOrderLiveDB: a session that locks worker, run, then users
// (the setRecoveryWaitLocked order) runs concurrently with Reset. Reset locks in
// the same order, so neither is a deadlock victim and Reset needs no retry.
func TestVaultResetLockOrderLiveDB(t *testing.T) {
	pool := vlIsolatedPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := vlSeed(t, pool, true, true)
	base := vault.NewPostgres(f.master, pool)
	if err := base.Unlock(ctx, f.owner, vlLoginPhrase); err != nil {
		t.Fatal(err)
	}
	vrBind(t, f, base)

	counting, attempts := vrCountingVault(f, pool)

	parkTx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = parkTx.Rollback(context.Background()) }()
	for _, sql := range []string{
		"SELECT id FROM workers WHERE user_id=$1 AND anthropic_secret_id IS NOT NULL ORDER BY id FOR UPDATE",
		"SELECT id FROM runs WHERE user_id=$1 AND anthropic_secret_id IS NOT NULL ORDER BY id FOR UPDATE",
	} {
		rows, err := parkTx.Query(ctx, sql, f.owner)
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
		if rows.Err() != nil {
			t.Fatal(rows.Err())
		}
	}

	var wg sync.WaitGroup
	resetErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resetErr <- counting.Reset(ctx, f.owner, vrNoop)
	}()
	// Wait until Reset is blocked on a row lock held by the parking session.
	deadline := time.Now().Add(15 * time.Second)
	for vrCount(t, pool, "SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock'") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Reset never blocked on the parking session's locks")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The parking session now takes users, as setRecoveryWaitLocked does.
	var mode string
	parkErr := parkTx.QueryRow(ctx, "SELECT vault_mode FROM users WHERE id=$1 FOR UPDATE", f.owner).Scan(&mode)
	if parkErr == nil {
		parkErr = parkTx.Commit(ctx)
	}
	wg.Wait()
	if parkErr != nil {
		t.Fatalf("parking session: %v", parkErr)
	}
	if err := <-resetErr; err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("Reset transaction attempts = %d, want 1 (a retry means it was a deadlock victim)", got)
	}
	if n := vrCount(t, pool, "SELECT count(*) FROM user_secrets WHERE user_id=$1", f.owner); n != 0 {
		t.Fatalf("secrets after reset = %d", n)
	}
}
