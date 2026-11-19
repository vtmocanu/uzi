package handler

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runenv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// A repo delete cascades its env rows without the secret-mutation lock. A real vault
// enable that meets the delete mid-flight must still reseal every surviving env row.
// With the lock-order fence the conversion blocks at the repo KEY SHARE
// (LockVaultEnvReposKeyShare) until the delete commits, so the doomed rows are never
// candidates and the surviving rows are all resealed. The unit test in internal/vault
// covers the cursor path for a candidate page that vanishes between listing and locking.
func TestVaultEnableSurvivesRepoDeleteOfWholeEnvPageLiveDB(t *testing.T) {
	ctx := context.Background()
	h, _, pool := cliVaultLiveDB(t)
	vaultFixturePolicy(t, pool, "optional")
	owner := cliSeedUser(t, pool, false)
	const password = "a-repo-delete-race-password" //nolint:gosec // G101: test-only vault password.
	setVaultFixturePassword(t, pool, owner, password)
	doomed := cliSeedOwnedRepo(t, pool, owner)
	kept := envSecondRepo(t, pool, doomed)
	cliMustExec(t, pool, `UPDATE repos SET enabled=false WHERE id=$1`, doomed)

	const doomedRows, keptRows = 64, 6
	insert := func(id uuid.UUID, repo uuid.UUID, name string) {
		t.Helper()
		kind := runenv.AADKind(runenv.ScopeRepo, &repo, true, name)
		sealed, err := h.vault.Seal(ctx, owner, kind, []byte("dummy-"+name), vault.SealForUse)
		if err != nil {
			t.Fatal(err)
		}
		mustExecT(ctx, t, pool, `INSERT INTO run_env_vars (id,var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev)
 VALUES ($1,gen_random_uuid(),$2,$3,$4,true,$5,$6,10,1)`, id, owner, repo, name, sealed.Ciphertext, sealed.SealedWith)
	}
	// The doomed repo's rows sort first, so page one of the conversion is all of them.
	// Ids are random below a fixed first byte, so reruns against a shared database never collide.
	idWithPrefix := func(first byte) uuid.UUID {
		id := uuid.New()
		id[0] = first
		return id
	}
	for i := range doomedRows {
		insert(idWithPrefix(0x00), doomed, fmt.Sprintf("DOOMED_%03d", i))
	}
	for i := range keptRows {
		insert(idWithPrefix(0xff), kept, fmt.Sprintf("KEPT_%03d", i))
	}

	// Connection B deletes the repo and holds the delete uncommitted.
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	if n, err := store.New(txB).DeleteRepoForUser(ctx, store.DeleteRepoForUserParams{ID: doomed, UserID: owner}); err != nil || n != 1 {
		t.Fatalf("DeleteRepoForUser = %d, %v", n, err)
	}

	convCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.vault.Enable(convCtx, owner, password) }()

	// Wait until the conversion is blocked at the repo KEY SHARE fence (the
	// LockVaultEnvReposKeyShare statement) on B's uncommitted delete. A wait of any other
	// statement (for example the env row lock, which is where an unfenced conversion would
	// block) does not satisfy this, so dropping the fence fails the test here.
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND pid<>pg_backend_pid() AND wait_event_type='Lock'
   AND wait_event IN ('transactionid','tuple')
   AND query LIKE '%FROM repos r%' AND query LIKE '%FOR KEY SHARE OF r%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Enable finished without waiting for the repo delete: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the conversion never blocked on the uncommitted repo delete")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Enable after the concurrent repo delete committed: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("Enable did not finish after the repo delete committed")
	}

	var gone, resealed, other int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE repo_id=$2),
 count(*) FILTER (WHERE repo_id=$3 AND sealed_with='dek'),
 count(*) FILTER (WHERE repo_id=$3 AND sealed_with<>'dek') FROM run_env_vars WHERE user_id=$1`, owner, doomed, kept).Scan(&gone, &resealed, &other); err != nil {
		t.Fatal(err)
	}
	if gone != 0 || resealed != keptRows || other != 0 {
		t.Fatalf("after enable: deleted-repo rows=%d, kept rows resealed=%d, kept rows left unsealed=%d; want 0, %d, 0", gone, resealed, other, keptRows)
	}
	// The surviving rows must open under the DEK with their own name/repo binding.
	keptRowsSet, err := pool.Query(ctx, `SELECT name, ciphertext, sealed_with FROM run_env_vars WHERE user_id=$1 AND repo_id=$2`, owner, kept)
	if err != nil {
		t.Fatal(err)
	}
	defer keptRowsSet.Close()
	opened := 0
	for keptRowsSet.Next() {
		var name, sealedWith string
		var ciphertext []byte
		if err := keptRowsSet.Scan(&name, &ciphertext, &sealedWith); err != nil {
			t.Fatal(err)
		}
		plain, err := h.vault.Open(owner, runenv.AADKind(runenv.ScopeRepo, &kept, true, name), sealedWith, ciphertext)
		if err != nil || string(plain) != "dummy-"+name {
			t.Fatalf("kept row %s opened to %q, %v; want %q", name, plain, err, "dummy-"+name)
		}
		opened++
	}
	if err := keptRowsSet.Err(); err != nil {
		t.Fatal(err)
	}
	keptRowsSet.Close()
	if opened != keptRows {
		t.Fatalf("opened %d kept rows; want %d", opened, keptRows)
	}
	var mode string
	if err := pool.QueryRow(ctx, `SELECT vault_mode FROM users WHERE id=$1`, owner).Scan(&mode); err != nil || mode != store.VaultModeProtected {
		t.Fatalf("vault mode = %q, %v; want protected", mode, err)
	}
}
