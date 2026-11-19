package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The vault conversion's lock-order fence: KEY SHARE on the owner's env-bearing
// connections and then repos (LockVaultEnvConnectionsKeyShare, LockVaultEnvReposKeyShare)
// before any run_env_vars row lock. Repo delete (DeleteRepoForUser) and connection
// delete (DeleteForgeConnectionForUser) cascade env rows without the secret-mutation
// lock, so they must wait on, not interleave with, a converting transaction.

func envLockFullDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	_, dsn := cjCreateDatabase(t, "")
	if err := Migrate(context.Background(), dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return cjOpenPool(t, dsn)
}

// vaultMaterialWantUnlocked asserts the NOWAIT row lock succeeds, i.e. no other
// transaction holds the row.
func vaultMaterialWantUnlocked(t *testing.T, pool *pgxpool.Pool, query string, id uuid.UUID) {
	t.Helper()
	var got uuid.UUID
	if err := pool.QueryRow(context.Background(), query, id).Scan(&got); err != nil {
		t.Fatalf("row was unexpectedly locked: %v", err)
	}
}

func envLockWantTimeout(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("%s: error = %v; want lock_not_available 55P03 (blocked by the fence)", what, err)
	}
}

// envLockFence is what convertMaterial runs: both fences, then the env batch lock.
func envLockFence(t *testing.T, tx pgx.Tx, user uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tq := New(tx)
	if _, err := tq.LockVaultEnvConnectionsKeyShare(ctx, user); err != nil {
		t.Fatal(err)
	}
	if _, err := tq.LockVaultEnvReposKeyShare(ctx, user); err != nil {
		t.Fatal(err)
	}
	ids, err := tq.ListVaultEnvMaterialCandidates(ctx, ListVaultEnvMaterialCandidatesParams{UserID: user})
	if err != nil || len(ids) == 0 {
		t.Fatalf("candidates = %d, %v", len(ids), err)
	}
	if rows, err := tq.LockVaultEnvMaterialBatch(ctx, LockVaultEnvMaterialBatchParams{UserID: user, Ids: ids}); err != nil || len(rows) != len(ids) {
		t.Fatalf("env batch lock = %d rows, %v", len(rows), err)
	}
}

// A code cross-check submission reads the repo inputs (GetCodeCrossCheckRepoInputsForUpdate) and
// then inserts the child run, whose runs_user_id_fkey needs KEY SHARE on the users row. The
// conversion holds that users row FOR UPDATE and then asks for KEY SHARE on the owner's
// env-bearing repos. With FOR UPDATE on the repo the two cross (40P01); FOR NO KEY UPDATE does
// not conflict with KEY SHARE, so both complete.
func TestCodeCrossCheckRepoInputsDoNotDeadlockWithVaultFenceLiveDB(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := envLockFullDB(t)
	user := cjSeedUser(t, pool, nil)
	seed := seedEnvRepo(t, pool, user)
	if err := envInsert(pool, user, seed.repo, "DEADLOCK_PROBE", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}

	// T1: the submission, holding the repo row.
	t1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = t1.Rollback(ctx) }()
	if _, err := New(t1).GetCodeCrossCheckRepoInputsForUpdate(ctx, GetCodeCrossCheckRepoInputsForUpdateParams{RepoID: seed.repo, UserID: user}); err != nil {
		t.Fatal(err)
	}

	// T2: the conversion's lock sequence, signalling once it holds the users row.
	usersLocked := make(chan struct{})
	t2Done := make(chan error, 1)
	go func() {
		t2Done <- func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				close(usersLocked)
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			tq := New(tx)
			if err := LockSecretMutation(ctx, tx, user); err != nil {
				close(usersLocked)
				return err
			}
			if _, err := tq.LockVaultAuthState(ctx, user); err != nil {
				close(usersLocked)
				return err
			}
			close(usersLocked)
			if _, err := tq.LockVaultEnvConnectionsKeyShare(ctx, user); err != nil {
				return err
			}
			if _, err := tq.LockVaultEnvReposKeyShare(ctx, user); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
	}()
	<-usersLocked

	// T1: the child-run insert, which needs KEY SHARE on the users row.
	_, insErr := t1.Exec(ctx, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 1, 't', 'd', 'queued', 'issue')`, uuid.New(), user, seed.repo)
	var pgErr *pgconn.PgError
	if errors.As(insErr, &pgErr) && pgErr.Code == "40P01" {
		t.Fatalf("submission insert deadlocked with the vault fence: %v", insErr)
	}
	if insErr != nil {
		t.Fatalf("submission insert: %v", insErr)
	}
	if err := t1.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-t2Done; err != nil {
		t.Fatalf("conversion lock sequence: %v", err)
	}
}

func TestVaultEnvFenceBlocksRepoAndConnectionDeleteLiveDB(t *testing.T) {
	ctx := context.Background()
	pool := envLockFullDB(t)
	q := New(pool)
	user := cjSeedUser(t, pool, nil)

	// (b) A repo delete waits on the fence.
	solo := seedEnvRepo(t, pool, user)
	if err := envInsert(pool, user, solo.repo, "SOLO", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}
	// (c) One connection owning two env-bearing repos.
	conn := uuid.New()
	cjExec(t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github',$3,'bot',1,'x')`, conn, user, "https://"+conn.String()+".example.com")
	var repos [2]uuid.UUID
	for i := range repos {
		repos[i] = uuid.New()
		cjExec(t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,$3,$4,'https://example.com/r','main')`, repos[i], conn, 10+i, "example/"+repos[i].String())
		if err := envInsert(pool, user, repos[i], "TWO_REPOS", true, 1, uuid.New(), "live"); err != nil {
			t.Fatal(err)
		}
	}

	txA, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	envLockFence(t, txA, user)

	// The fence holds KEY SHARE on every fenced repo itself, not only on the env rows:
	// dropping LockVaultEnvReposKeyShare leaves these row locks free.
	for _, repo := range []uuid.UUID{solo.repo, repos[0], repos[1]} {
		vaultMaterialWantLocked(t, pool, "SELECT id FROM repos WHERE id=$1 FOR UPDATE NOWAIT", repo)
	}

	// B: a repo delete under a short lock_timeout is blocked.
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	if _, err := txB.Exec(ctx, `SET LOCAL lock_timeout='300ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = New(txB).DeleteRepoForUser(ctx, DeleteRepoForUserParams{ID: solo.repo, UserID: user})
	envLockWantTimeout(t, err, "DeleteRepoForUser under the fence")
	_ = txB.Rollback(ctx)

	// B: a connection row lock is blocked, and so is the connection delete.
	vaultMaterialWantLocked(t, pool, "SELECT id FROM forge_connections WHERE id=$1 FOR UPDATE NOWAIT", conn)
	txC, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txC.Rollback(ctx) }()
	if _, err := txC.Exec(ctx, `SET LOCAL lock_timeout='300ms'`); err != nil {
		t.Fatal(err)
	}
	_, err = New(txC).DeleteForgeConnectionForUser(ctx, DeleteForgeConnectionForUserParams{ID: conn, UserID: user})
	envLockWantTimeout(t, err, "DeleteForgeConnectionForUser under the fence")
	_ = txC.Rollback(ctx)

	// Control: once A commits, both deletes go through.
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := q.DeleteRepoForUser(ctx, DeleteRepoForUserParams{ID: solo.repo, UserID: user}); err != nil || n != 1 {
		t.Fatalf("DeleteRepoForUser after the fence released = %d, %v", n, err)
	}
	if n, err := q.DeleteForgeConnectionForUser(ctx, DeleteForgeConnectionForUserParams{ID: conn, UserID: user}); err != nil || n != 1 {
		t.Fatalf("DeleteForgeConnectionForUser after the fence released = %d, %v", n, err)
	}
}
