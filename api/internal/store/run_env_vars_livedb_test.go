package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PRD #4416 M1: the run_env_vars table, its indexes and its queries, against a real
// PostgreSQL. Skipped without UZI_TEST_DATABASE_URL (see codexJudgeMigrationDB).

type envSeed struct {
	user, conn, repo uuid.UUID
}

func seedEnvRepo(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) envSeed {
	t.Helper()
	s := envSeed{user: user, conn: uuid.New(), repo: uuid.New()}
	cjExec(t, pool, `INSERT INTO forge_connections(id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES($1,$2,'github',$3,'bot',1,'x')`, s.conn, user, "https://"+s.conn.String()+".example.com")
	cjExec(t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch)
 VALUES($1,$2,1,$3,'https://example.com/r','main')`, s.repo, s.conn, "example/"+s.repo.String())
	return s
}

// envInsert inserts one version row; repo may be uuid.Nil for the user scope.
func envInsert(pool *pgxpool.Pool, user, repo uuid.UUID, name string, secret bool, rev int, varID uuid.UUID, state string) error {
	var repoArg any
	if repo != uuid.Nil {
		repoArg = repo
	}
	_, err := pool.Exec(context.Background(),
		`INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at,pending_delete_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'master_aad',10,$7,
		   CASE WHEN $8 = 'retired' THEN now() END, CASE WHEN $8 = 'pending' THEN now() END)`,
		varID, user, repoArg, name, secret, []byte("0123456789abcdef"), rev, state)
	return err
}

func wantConstraint(t *testing.T, err error, code, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code || (constraint != "" && pgErr.ConstraintName != constraint) {
		t.Fatalf("error = %v; want SQLSTATE %s on %q", err, code, constraint)
	}
}

func TestRunEnvVarsConstraintsAndIndexesLiveDB(t *testing.T) {
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_vars"))
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	otherRepo := seedEnvRepo(t, pool, user).repo

	// Live repo-scope index: a name is unique among LIVE rows of (user, repo).
	if err := envInsert(pool, user, s.repo, "API_KEY", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}
	wantConstraint(t, envInsert(pool, user, s.repo, "API_KEY", false, 1, uuid.New(), "live"), "23505", "run_env_vars_live_repo_name_key")
	// The same name is free in another repo, in the user scope, and for another user.
	for label, err := range map[string]error{
		"another repo":       envInsert(pool, user, otherRepo, "API_KEY", true, 1, uuid.New(), "live"),
		"user scope":         envInsert(pool, user, uuid.Nil, "API_KEY", true, 1, uuid.New(), "live"),
		"retired same scope": envInsert(pool, user, s.repo, "API_KEY", true, 1, uuid.New(), "retired"),
		"pending same scope": envInsert(pool, user, s.repo, "API_KEY", true, 1, uuid.New(), "pending"),
	} {
		if err != nil {
			t.Errorf("%s: %v", label, err)
		}
	}
	// Live user-scope index: (user, name) where repo IS NULL.
	wantConstraint(t, envInsert(pool, user, uuid.Nil, "API_KEY", true, 1, uuid.New(), "live"), "23505", "run_env_vars_live_user_name_key")
	if err := envInsert(pool, user, uuid.Nil, "API_KEY", true, 1, uuid.New(), "retired"); err != nil {
		t.Errorf("retired user-scope duplicate: %v", err)
	}

	// (var_id, rev) is unique, and a variable has one live version.
	v := uuid.New()
	if err := envInsert(pool, user, s.repo, "TWO_VERSIONS", true, 1, v, "retired"); err != nil {
		t.Fatal(err)
	}
	wantConstraint(t, envInsert(pool, user, s.repo, "TWO_VERSIONS", true, 1, v, "live"), "23505", "run_env_vars_var_rev_key")
	if err := envInsert(pool, user, s.repo, "TWO_VERSIONS", true, 2, v, "live"); err != nil {
		t.Fatal(err)
	}
	wantConstraint(t, envInsert(pool, user, s.repo, "OTHER_NAME", true, 3, v, "live"), "23505", "run_env_vars_live_var_key")

	// CHECKs: name shape, only secrets are retained, sealed_with discriminator, sizes.
	wantConstraint(t, envInsert(pool, user, s.repo, "lower", true, 1, uuid.New(), "live"), "23514", "run_env_vars_name_check")
	wantConstraint(t, envInsert(pool, user, s.repo, "PLAIN_RETIRED", false, 1, uuid.New(), "retired"), "23514", "run_env_vars_retained_secret_check")
	wantConstraint(t, envInsert(pool, user, s.repo, "PLAIN_PENDING", false, 1, uuid.New(), "pending"), "23514", "run_env_vars_retained_secret_check")
	_, err := pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev)
 VALUES ($1,$2,$3,'BAD_SEAL',true,'\x01','plaintext',1,1)`, uuid.New(), user, s.repo)
	wantConstraint(t, err, "23514", "run_env_vars_sealed_with_check")
	_, err = pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev)
 VALUES ($1,$2,$3,'BAD_SIZE',true,'\x01','dek',65537,1)`, uuid.New(), user, s.repo)
	wantConstraint(t, err, "23514", "run_env_vars_value_bytes_check")
	_, err = pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,description,ciphertext,sealed_with,value_bytes,rev)
 VALUES ($1,$2,$3,'BAD_DESC',true,repeat('a',1025),'\x01','dek',1,1)`, uuid.New(), user, s.repo)
	wantConstraint(t, err, "23514", "run_env_vars_description_check")
	_, err = pool.Exec(context.Background(), `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev)
 VALUES ($1,$2,$3,'BAD_REV',true,'\x01','dek',1,0)`, uuid.New(), user, s.repo)
	wantConstraint(t, err, "23514", "run_env_vars_rev_check")
}

func TestRunEnvVarsCascadeLiveDB(t *testing.T) {
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_vars"))
	user, keep := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	other := seedEnvRepo(t, pool, user)
	kept := seedEnvRepo(t, pool, keep)
	for _, c := range []struct {
		user, repo uuid.UUID
		name       string
	}{{user, s.repo, "A_ONE"}, {user, s.repo, "A_TWO"}, {user, other.repo, "B_ONE"}, {user, uuid.Nil, "USER_SCOPE"}, {keep, kept.repo, "KEPT"}} {
		if err := envInsert(pool, c.user, c.repo, c.name, true, 1, uuid.New(), "live"); err != nil {
			t.Fatal(err)
		}
	}
	count := func(where string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM run_env_vars WHERE `+where, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Deleting a repo removes exactly that repo's rows.
	cjExec(t, pool, `DELETE FROM repos WHERE id=$1`, s.repo)
	if got := count(`repo_id=$1`, s.repo); got != 0 {
		t.Fatalf("%d rows survived their repo", got)
	}
	if got := count(`user_id=$1`, user); got != 2 {
		t.Fatalf("user rows after repo delete = %d, want 2 (another repo and the user scope)", got)
	}
	// Deleting a user removes all of that user's rows, in both scopes, and no one else's.
	cjExec(t, pool, `DELETE FROM users WHERE id=$1`, user)
	if got := count(`user_id=$1`, user); got != 0 {
		t.Fatalf("%d rows survived their user", got)
	}
	if got := count(`user_id=$1`, keep); got != 1 {
		t.Fatalf("another user's rows = %d, want 1", got)
	}
}

func TestRunEnvVarQueriesLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_receipts"))
	q := New(pool)
	user, foreign := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	fs := seedEnvRepo(t, pool, foreign)
	repo := pgtype.UUID{Bytes: s.repo, Valid: true}

	// A live secret, a disabled plain, a pending-deletion secret and two retired secrets.
	rows := []struct {
		name   string
		secret bool
		state  string
	}{{"LIVE_SECRET", true, "live"}, {"DISABLED_PLAIN", false, "live"}, {"PENDING_SECRET", true, "pending"}, {"OLD_ONE", true, "retired"}, {"OLD_TWO", true, "retired"}}
	for _, r := range rows {
		if err := envInsert(pool, user, s.repo, r.name, r.secret, 1, uuid.New(), r.state); err != nil {
			t.Fatal(err)
		}
	}
	cjExec(t, pool, `UPDATE run_env_vars SET disabled_at=now() WHERE name='DISABLED_PLAIN'`)
	if err := envInsert(pool, foreign, fs.repo, "FOREIGN", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}
	if err := envInsert(pool, user, uuid.Nil, "USER_SCOPE_ONE", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}

	list, err := q.ListRunEnvVarsForRepo(ctx, ListRunEnvVarsForRepoParams{UserID: user, RepoID: repo})
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %d rows, %v; want live, disabled and pending only (no retired history, no other scope)", len(list), err)
	}
	if list[0].Name != "DISABLED_PLAIN" || list[1].Name != "LIVE_SECRET" || list[2].Name != "PENDING_SECRET" {
		t.Fatalf("list order = %s, %s, %s; want name order", list[0].Name, list[1].Name, list[2].Name)
	}

	usage, err := q.GetRunEnvScopeUsage(ctx, GetRunEnvScopeUsageParams{UserID: user, RepoID: repo})
	if err != nil || usage != (GetRunEnvScopeUsageRow{LiveCount: 2, DisabledCount: 1, PendingCount: 1, ValueBytes: 30}) {
		t.Fatalf("scope usage = %+v, %v; want 2 live, 1 disabled, 1 pending and 30 value bytes (retired excluded)", usage, err)
	}
	retired, err := q.GetRunEnvRetiredUsage(ctx, user)
	if err != nil || retired != (GetRunEnvRetiredUsageRow{RetiredCount: 2, ValueBytes: 20}) {
		t.Fatalf("retired usage = %+v, %v", retired, err)
	}

	counts, err := q.ListRunEnvVarCountsForUser(ctx, user)
	if err != nil || len(counts) != 1 || counts[0].RepoID != s.repo || counts[0].VarCount != 2 {
		t.Fatalf("repo counts = %+v, %v; want 2 live (enabled+disabled) on one repo, user scope and pending excluded", counts, err)
	}
	if n, err := q.CountRunEnvVarsForRepo(ctx, CountRunEnvVarsForRepoParams{UserID: user, RepoID: repo}); err != nil || n != 2 {
		t.Fatalf("repo count = %d, %v", n, err)
	}

	// Vault enumeration: every version row counts as material, whatever its state.
	totals, err := q.GetVaultMaterialTotals(ctx, user)
	if err != nil || totals.MaterialRows != 6 || totals.CiphertextBytes != 6*16 || totals.MaxRowBytes != 16 {
		t.Fatalf("material totals = %+v, %v; want the 6 env rows (live, disabled, pending, retired, user scope)", totals, err)
	}
	if has, err := q.HasMasterVaultMaterial(ctx, user); err != nil || !has {
		t.Fatalf("HasMasterVaultMaterial with only master_aad env rows = %v, %v", has, err)
	}
	if has, err := q.HasMasterVaultMaterial(ctx, cjSeedUser(t, pool, nil)); err != nil || has {
		t.Fatalf("HasMasterVaultMaterial for an empty owner = %v, %v", has, err)
	}
	cjExec(t, pool, `UPDATE run_env_vars SET sealed_with='dek' WHERE user_id=$1`, user)
	if has, err := q.HasMasterVaultMaterial(ctx, user); err != nil || has {
		t.Fatalf("HasMasterVaultMaterial with only DEK env rows = %v, %v", has, err)
	}
	cjExec(t, pool, `UPDATE run_env_vars SET sealed_with='master_aad' WHERE user_id=$1`, user)

	// The lock batch is owner scoped, ordered by id, keyset paged, and row-locking; the
	// reseal changes crypto columns only.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	tq := q.WithTx(tx)
	cands, err := tq.ListVaultEnvMaterialCandidates(ctx, ListVaultEnvMaterialCandidatesParams{UserID: user})
	if err != nil || len(cands) != 6 {
		t.Fatalf("candidates = %d ids, %v; want all 6 of the owner", len(cands), err)
	}
	for i := 1; i < len(cands); i++ {
		if bytes.Compare(cands[i-1][:], cands[i][:]) >= 0 {
			t.Fatal("candidates are not in ascending id order")
		}
	}
	// Listing candidates takes no row lock.
	vaultMaterialWantUnlocked(t, pool, "SELECT id FROM run_env_vars WHERE id=$1 FOR UPDATE NOWAIT", cands[0])
	tail, err := tq.ListVaultEnvMaterialCandidates(ctx, ListVaultEnvMaterialCandidatesParams{UserID: user, AfterID: pgtype.UUID{Bytes: cands[2], Valid: true}})
	if err != nil || len(tail) != 3 || tail[0] != cands[3] {
		t.Fatalf("keyset tail = %d ids, %v", len(tail), err)
	}
	if foreignCands, err := tq.ListVaultEnvMaterialCandidates(ctx, ListVaultEnvMaterialCandidatesParams{UserID: foreign}); err != nil || len(foreignCands) != 1 {
		t.Fatalf("foreign candidates = %d, %v; want only the foreign owner's row", len(foreignCands), err)
	}
	// A partial id list locks only those rows: run it first, in its own transaction, before
	// any full batch has locked everything.
	partTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = partTx.Rollback(ctx) }()
	part, err := q.WithTx(partTx).LockVaultEnvMaterialBatch(ctx, LockVaultEnvMaterialBatchParams{UserID: user, Ids: cands[3:]})
	if err != nil || len(part) != 3 {
		t.Fatalf("partial lock = %d rows, %v; want the 3 listed rows", len(part), err)
	}
	for i := range part {
		if part[i].ID != cands[3+i] {
			t.Fatalf("partial lock row %d is not cands[%d] (ascending order)", i, 3+i)
		}
	}
	// Probe every id while partTx is open: the requested ones locked, every omitted one free.
	for _, id := range cands[3:] {
		vaultMaterialWantLocked(t, pool, "SELECT id FROM run_env_vars WHERE id=$1 FOR UPDATE NOWAIT", id)
	}
	for _, id := range cands[:3] {
		vaultMaterialWantUnlocked(t, pool, "SELECT id FROM run_env_vars WHERE id=$1 FOR UPDATE NOWAIT", id)
	}
	if err := partTx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// The lock batch is owner scoped by id: foreign ids lock nothing, vanished ids are absent.
	if got, err := tq.LockVaultEnvMaterialBatch(ctx, LockVaultEnvMaterialBatchParams{UserID: foreign, Ids: cands}); err != nil || len(got) != 0 {
		t.Fatalf("lock under a foreign owner = %d rows, %v; want none", len(got), err)
	}
	batch, err := tq.LockVaultEnvMaterialBatch(ctx, LockVaultEnvMaterialBatchParams{UserID: user, Ids: append([]uuid.UUID{uuid.New()}, cands...)})
	if err != nil || len(batch) != 6 {
		t.Fatalf("batch = %d rows, %v; want all 6 of the owner (the unknown id absent)", len(batch), err)
	}
	for i := range batch {
		if batch[i].ID != cands[i] {
			t.Fatal("batch is not in ascending id order")
		}
	}
	vaultMaterialWantLocked(t, pool, "SELECT id FROM run_env_vars WHERE id=$1 FOR UPDATE NOWAIT", batch[0].ID)
	// The connection and repo fence queries return the owner's env-bearing parents only.
	if conns, err := tq.LockVaultEnvConnectionsKeyShare(ctx, user); err != nil || len(conns) != 1 || conns[0] != s.conn {
		t.Fatalf("connection fence = %v, %v; want the owner's one env-bearing connection", conns, err)
	}
	if repos, err := tq.LockVaultEnvReposKeyShare(ctx, user); err != nil || len(repos) != 1 || repos[0] != s.repo {
		t.Fatalf("repo fence = %v, %v; want the owner's one env-bearing repo", repos, err)
	}
	const snapshot = `SELECT to_jsonb(e)-ARRAY['ciphertext','sealed_with'] FROM run_env_vars e WHERE id=$1`
	var before, after []byte
	target := batch[0]
	if err := pool.QueryRow(ctx, snapshot, target.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if n, err := tq.ResealVaultEnvMaterial(ctx, ResealVaultEnvMaterialParams{ID: target.ID, UserID: foreign, Ciphertext: []byte("zz"), SealedWith: "dek"}); err != nil || n != 0 {
		t.Fatalf("reseal under a foreign owner = %d, %v; want 0 rows", n, err)
	}
	if n, err := tq.ResealVaultEnvMaterial(ctx, ResealVaultEnvMaterialParams{ID: target.ID, UserID: user, Ciphertext: []byte("zz"), SealedWith: "dek"}); err != nil || n != 1 {
		t.Fatalf("reseal = %d, %v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, snapshot, target.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("reseal changed a non-crypto column:\n%s\n%s", before, after)
	}
	var ct []byte
	var marker string
	if err := pool.QueryRow(ctx, `SELECT ciphertext, sealed_with FROM run_env_vars WHERE id=$1`, target.ID).Scan(&ct, &marker); err != nil || string(ct) != "zz" || marker != "dek" {
		t.Fatalf("reseal did not store the new crypto columns: %q %q %v", ct, marker, err)
	}

	// The account-reset delete takes this owner's rows, all states, and nobody else's.
	if n, err := q.DeleteAllRunEnvVarsForOwner(ctx, user); err != nil || n != 6 {
		t.Fatalf("DeleteAllRunEnvVarsForOwner = %d, %v; want 6", n, err)
	}
	if n, err := q.CountRunEnvVarsForRepo(ctx, CountRunEnvVarsForRepoParams{UserID: foreign, RepoID: pgtype.UUID{Bytes: fs.repo, Valid: true}}); err != nil || n != 1 {
		t.Fatalf("a foreign owner's row after the delete = %d, %v", n, err)
	}
}

func TestRunEnvVarWriteQueriesLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_receipts"))
	q := New(pool)
	user, foreign := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	repo := pgtype.UUID{Bytes: s.repo, Valid: true}
	ins, err := q.InsertRunEnvVar(ctx, InsertRunEnvVarParams{VarID: uuid.New(), UserID: user, RepoID: repo, Name: "TOKEN_X", Secret: true,
		Description: "first", Ciphertext: []byte("c"), SealedWith: "master_aad", ValueBytes: 9, Rev: 1, AadVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Foreign owners change nothing.
	if _, err := q.UpdateRunEnvVarDescription(ctx, UpdateRunEnvVarDescriptionParams{ID: ins.ID, UserID: foreign, Description: "x"}); err == nil {
		t.Fatal("a foreign owner updated the description")
	}
	upd, err := q.UpdateRunEnvVarDescription(ctx, UpdateRunEnvVarDescriptionParams{ID: ins.ID, UserID: user, Description: "second"})
	if err != nil || upd.Description != "second" || upd.Rev != 1 || upd.ID != ins.ID {
		t.Fatalf("description update = %+v, %v; want in place at the same rev", upd, err)
	}
	off, err := q.SetRunEnvVarDisabled(ctx, SetRunEnvVarDisabledParams{ID: ins.ID, UserID: user, Disabled: true})
	if err != nil || !off.DisabledAt.Valid || off.Rev != 1 {
		t.Fatalf("disable = %+v, %v", off, err)
	}
	again, err := q.SetRunEnvVarDisabled(ctx, SetRunEnvVarDisabledParams{ID: ins.ID, UserID: user, Disabled: true})
	if err != nil || !again.DisabledAt.Time.Equal(off.DisabledAt.Time) {
		t.Fatalf("a repeated disable moved disabled_at: %+v, %v", again, err)
	}
	on, err := q.SetRunEnvVarDisabled(ctx, SetRunEnvVarDisabledParams{ID: ins.ID, UserID: user, Disabled: false})
	if err != nil || on.DisabledAt.Valid {
		t.Fatalf("enable = %+v, %v", on, err)
	}
	if n, err := q.RetireRunEnvVarVersion(ctx, RetireRunEnvVarVersionParams{ID: ins.ID, UserID: foreign}); err != nil || n != 0 {
		t.Fatalf("foreign retire = %d, %v", n, err)
	}
	if n, err := q.RetireRunEnvVarVersion(ctx, RetireRunEnvVarVersionParams{ID: ins.ID, UserID: user}); err != nil || n != 1 {
		t.Fatalf("retire = %d, %v", n, err)
	}
	if n, err := q.MarkRunEnvVarPendingDeletion(ctx, MarkRunEnvVarPendingDeletionParams{ID: ins.ID, UserID: user}); err != nil || n != 0 {
		t.Fatalf("pending deletion of a retired version = %d, %v; want 0", n, err)
	}
	if n, err := q.DeleteRunEnvVarVersion(ctx, DeleteRunEnvVarVersionParams{ID: ins.ID, UserID: foreign}); err != nil || n != 0 {
		t.Fatalf("foreign delete = %d, %v", n, err)
	}
	if n, err := q.DeleteRunEnvVarVersion(ctx, DeleteRunEnvVarVersionParams{ID: ins.ID, UserID: user}); err != nil || n != 1 {
		t.Fatalf("delete = %d, %v", n, err)
	}
}
