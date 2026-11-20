package store

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
)

// PRD #4416 M2: run_env_receipts, run_env_var_ids and the run-env lane predicate, against a
// real PostgreSQL. Skipped without UZI_TEST_DATABASE_URL (see codexJudgeMigrationDB).

func envReceiptsDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_receipts"))
	return pool
}

func envSeedRun(t *testing.T, pool *pgxpool.Pool, user, repo uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cjExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
 VALUES ($1,$2,$3,$4,'t','d','running','issue')`, id, user, repo, int(id[0])+1000)
	return id
}

func wantIdentityRefusal(t *testing.T, label string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatalf("%s: accepted", label)
	}
	if !asPgErr(err, &pgErr) || pgErr.Code != "UE001" {
		t.Fatalf("%s: error = %v; want SQLSTATE UE001", label, err)
	}
}

func asPgErr(err error, target **pgconn.PgError) bool {
	for err != nil {
		if pe, ok := err.(*pgconn.PgError); ok { //nolint:errorlint // pgx returns the concrete error directly; Unwrap is walked below.
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error }) //nolint:errorlint // walking the chain by hand keeps this helper dependency-free.
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestRunEnvVarIdentityLiveDB(t *testing.T) {
	pool := envReceiptsDB(t)
	ctx := context.Background()
	user, other := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	otherRepo := seedEnvRepo(t, pool, user)
	v := uuid.New()
	if err := envInsert(pool, user, s.repo, "API_KEY", true, 1, v, "live"); err != nil {
		t.Fatal(err)
	}
	var scope string
	if err := pool.QueryRow(ctx, `SELECT scope FROM run_env_var_ids WHERE var_id=$1`, v).Scan(&scope); err != nil || scope != "repo" {
		t.Fatalf("identity row = %q, %v; want scope repo", scope, err)
	}
	// A later version of the same variable under the same identity is accepted.
	if err := envInsert(pool, user, s.repo, "API_KEY", true, 2, v, "retired"); err != nil {
		t.Fatalf("second version: %v", err)
	}
	wantIdentityRefusal(t, "another name", envInsert(pool, user, s.repo, "OTHER_NAME", true, 3, v, "retired"))
	wantIdentityRefusal(t, "another repo", envInsert(pool, user, otherRepo.repo, "API_KEY", true, 3, v, "retired"))
	wantIdentityRefusal(t, "user scope", envInsert(pool, user, uuid.Nil, "API_KEY", true, 3, v, "retired"))
	wantIdentityRefusal(t, "another user", envInsert(pool, other, uuid.Nil, "API_KEY", true, 3, v, "retired"))

	// An UPDATE may not move a row to another name or repo (or another variable or user).
	for label, stmt := range map[string]string{
		"name":    `UPDATE run_env_vars SET name='RENAMED' WHERE var_id=$1`,
		"repo_id": `UPDATE run_env_vars SET repo_id=NULL WHERE var_id=$1`,
		"var_id":  `UPDATE run_env_vars SET var_id=gen_random_uuid() WHERE var_id=$1`,
		"user_id": `UPDATE run_env_vars SET user_id='` + other.String() + `' WHERE var_id=$1`,
	} {
		_, err := pool.Exec(ctx, stmt, v)
		wantIdentityRefusal(t, "update "+label, err)
	}
	// A metadata update is untouched.
	if _, err := pool.Exec(ctx, `UPDATE run_env_vars SET description='ok', disabled_at=now() WHERE var_id=$1`, v); err != nil {
		t.Fatalf("metadata update refused: %v", err)
	}

	// Delete every row of the variable, then re-insert the same var_id under another name:
	// the identity row outlived the rows.
	cjExec(t, pool, `DELETE FROM run_env_vars WHERE var_id=$1`, v)
	wantIdentityRefusal(t, "re-insert after delete under another name", envInsert(pool, user, s.repo, "OTHER_NAME", true, 1, v, "live"))
	if err := envInsert(pool, user, s.repo, "API_KEY", true, 1, v, "live"); err != nil {
		t.Fatalf("re-insert under the original identity: %v", err)
	}

	// The identity survives its repo; the reuse is still refused.
	cjExec(t, pool, `DELETE FROM repos WHERE id=$1`, s.repo)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_env_vars WHERE var_id=$1`, v).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rows after repo delete = %d, %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_env_var_ids WHERE var_id=$1`, v).Scan(&n); err != nil || n != 1 {
		t.Fatalf("identity rows after repo delete = %d, %v; want 1", n, err)
	}
	wantIdentityRefusal(t, "reuse after repo delete", envInsert(pool, user, otherRepo.repo, "API_KEY", true, 1, v, "live"))

	// The identity goes with its user.
	cjExec(t, pool, `DELETE FROM users WHERE id=$1`, user)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_env_var_ids WHERE var_id=$1`, v).Scan(&n); err != nil || n != 0 {
		t.Fatalf("identity rows after user delete = %d, %v; want 0", n, err)
	}
}

// The migration backfills one identity per variable from the M1 rows it finds.
func TestRunEnvVarIdentityBackfillLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, dsn := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_vars"))
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	v1, v2 := uuid.New(), uuid.New()
	for _, r := range []struct {
		v    uuid.UUID
		repo any
		rev  int
		name string
		keep string
	}{{v1, s.repo, 1, "API_KEY", "retired"}, {v1, s.repo, 2, "API_KEY", "live"}, {v2, nil, 1, "USER_ONE", "live"}} {
		cjExec(t, pool, `INSERT INTO run_env_vars (var_id,user_id,repo_id,name,secret,ciphertext,sealed_with,value_bytes,rev,retired_at)
 VALUES ($1,$2,$3,$4,true,'\x01','dek',1,$5, CASE WHEN $6='retired' THEN now() END)`, r.v, user, r.repo, r.name, r.rev, r.keep)
	}
	if err := Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `SELECT var_id, scope, name, repo_id FROM run_env_var_ids ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type got struct {
		v           uuid.UUID
		scope, name string
		repo        pgtype.UUID
	}
	var all []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.v, &g.scope, &g.name, &g.repo); err != nil {
			t.Fatal(err)
		}
		all = append(all, g)
	}
	if len(all) != 2 || all[0].v != v1 || all[0].scope != "repo" || all[0].repo.Bytes != s.repo || all[1].v != v2 || all[1].scope != "user" || all[1].repo.Valid {
		t.Fatalf("backfilled identities = %+v", all)
	}
	var version int16
	if err := pool.QueryRow(ctx, `SELECT min(aad_version) FROM run_env_vars`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("existing rows' aad_version = %d, %v; want 1", version, err)
	}
}

// fn_run_env_lane equals leadcfg.LeadLane for every kind of the registry fixture, with and
// without a review target and an egress profile.
func TestRunEnvLaneMatchesLeadLaneLiveDB(t *testing.T) {
	pool := envReceiptsDB(t)
	ctx := context.Background()
	raw, err := os.ReadFile("../../../fixtures/run-kinds/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil || len(reg.Kinds) == 0 {
		t.Fatalf("registry: %v (%d kinds)", err, len(reg.Kinds))
	}
	kinds := append(slices.Clone(reg.Kinds), "unknown_kind", "")
	checked, lanes := 0, 0
	for _, kind := range kinds {
		for _, target := range []bool{false, true} {
			for _, egress := range []bool{false, true} {
				var tgt, egr any
				if target {
					tgt = uuid.New()
				}
				if egress {
					egr = uuid.New()
				}
				var got bool
				if err := pool.QueryRow(ctx, `SELECT fn_run_env_lane($1::text, $2::uuid, $3::uuid)`, kind, tgt, egr).Scan(&got); err != nil {
					t.Fatal(err)
				}
				want := leadcfg.LeadLane(kind, target, egress)
				if got != want {
					t.Fatalf("fn_run_env_lane(%q, review_target=%v, egress=%v) = %v; leadcfg.LeadLane = %v", kind, target, egress, got, want)
				}
				checked++
				if want {
					lanes++
				}
			}
		}
	}
	if lanes == 0 || lanes == checked {
		t.Fatalf("degenerate matrix: %d of %d combinations are in the lane", lanes, checked)
	}
	// A NULL kind is not in the lane (LeadLane has no NULL).
	var got bool
	if err := pool.QueryRow(ctx, `SELECT fn_run_env_lane(NULL, NULL, NULL)`).Scan(&got); err != nil || got {
		t.Fatalf("fn_run_env_lane(NULL,NULL,NULL) = %v, %v; want false", got, err)
	}
}

func TestRunEnvReceiptQueriesLiveDB(t *testing.T) {
	pool := envReceiptsDB(t)
	ctx := context.Background()
	q := New(pool)
	user, foreign := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	fs := seedEnvRepo(t, pool, foreign)
	run := envSeedRun(t, pool, user, s.repo)
	run2 := envSeedRun(t, pool, user, s.repo)

	// Four variables of the run's repo: an enabled secret, an enabled plain, a disabled
	// secret, and a secret that was rotated (rev 1 retired, rev 2 live). A foreign repo's
	// variable and a user-scope variable are never in the repo set.
	live, plain, disabled, rotated := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, r := range []struct {
		v      uuid.UUID
		name   string
		secret bool
		rev    int
		state  string
	}{{live, "LIVE_SECRET", true, 1, "live"}, {plain, "PLAIN_VALUE", false, 1, "live"}, {disabled, "DISABLED_SECRET", true, 1, "live"},
		{rotated, "ROTATED", true, 1, "retired"}, {rotated, "ROTATED", true, 2, "live"}} {
		if err := envInsert(pool, user, s.repo, r.name, r.secret, r.rev, r.v, r.state); err != nil {
			t.Fatal(err)
		}
	}
	cjExec(t, pool, `UPDATE run_env_vars SET disabled_at=now() WHERE var_id=$1`, disabled)
	if err := envInsert(pool, foreign, fs.repo, "FOREIGN", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}
	if err := envInsert(pool, user, uuid.Nil, "USER_SCOPE", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}

	eff, err := q.ListRunEnvEffectiveForRun(ctx, run)
	if err != nil || len(eff) != 3 {
		t.Fatalf("effective = %d rows, %v; want the 3 enabled live repo rows", len(eff), err)
	}
	names := []string{eff[0].Name, eff[1].Name, eff[2].Name}
	if !slices.Equal(names, []string{"LIVE_SECRET", "PLAIN_VALUE", "ROTATED"}) {
		t.Fatalf("effective names = %v", names)
	}
	if eff[0].Scope != "repo" || eff[0].AadVersion != 1 || len(eff[0].Ciphertext) == 0 || eff[2].Rev != 2 {
		t.Fatalf("effective row = %+v", eff[0])
	}
	keys, err := q.ListRunEnvEffectiveKeysForRun(ctx, run)
	if err != nil || len(keys) != 3 {
		t.Fatalf("effective keys = %d, %v", len(keys), err)
	}

	// No receipts yet: nothing to redact, no holder.
	if red, err := q.ListRunEnvRedactForRun(ctx, run); err != nil || len(red) != 0 {
		t.Fatalf("redact before any receipt = %d, %v", len(red), err)
	}
	if held, err := q.RunEnvVersionHasReceipt(ctx, rotated, 1); err != nil || held {
		t.Fatalf("has receipt before any delivery = %v, %v", held, err)
	}

	// Deliver: every effective version, secret and plain, at generation 1; later the rotated
	// one's rev 1 at generation 1 too (it was effective then).
	deliver := func(r uuid.UUID, v uuid.UUID, rev int32, scope, name string, secret bool, gen int64) {
		t.Helper()
		if err := q.UpsertRunEnvReceipt(ctx, UpsertRunEnvReceiptParams{RunID: r, UserID: user, VarID: v, Rev: rev, Scope: scope, Name: name, Secret: secret, ClaimGeneration: gen}); err != nil {
			t.Fatal(err)
		}
	}
	deliver(run, live, 1, "repo", "LIVE_SECRET", true, 1)
	deliver(run, plain, 1, "repo", "PLAIN_VALUE", false, 1)
	deliver(run, rotated, 1, "repo", "ROTATED", true, 1)
	deliver(run, disabled, 1, "repo", "DISABLED_SECRET", true, 1)

	// Secret receipts answer the retention question; the plain one does not.
	for _, c := range []struct {
		v    uuid.UUID
		rev  int32
		want bool
	}{{live, 1, true}, {plain, 1, false}, {rotated, 1, true}, {rotated, 2, false}, {uuid.New(), 1, false}} {
		if held, err := q.RunEnvVersionHasReceipt(ctx, c.v, c.rev); err != nil || held != c.want {
			t.Fatalf("RunEnvVersionHasReceipt(%s, %d) = %v, %v; want %v", c.v, c.rev, held, err, c.want)
		}
	}
	if held, err := q.RunHasSecretRunEnvReceipts(ctx, run); err != nil || !held {
		t.Fatalf("RunHasSecretRunEnvReceipts = %v, %v", held, err)
	}
	if held, err := q.RunHasSecretRunEnvReceipts(ctx, run2); err != nil || held {
		t.Fatalf("RunHasSecretRunEnvReceipts for a run without receipts = %v, %v", held, err)
	}

	// Redact: the retired rev 1 and the disabled secret; not the effective secrets, not the plain.
	red, err := q.ListRunEnvRedactForRun(ctx, run)
	if err != nil || len(red) != 2 {
		t.Fatalf("redact = %d rows, %v; want the retired rotation and the disabled secret", len(red), err)
	}
	got := map[string]int32{}
	for _, r := range red {
		got[r.Name] = r.Rev
		if len(r.Ciphertext) == 0 || !r.AadVersion.Valid || !r.SealedWith.Valid {
			t.Fatalf("redact row %s lacks its sealed value: %+v", r.Name, r)
		}
	}
	if got["ROTATED"] != 1 || got["DISABLED_SECRET"] != 1 {
		t.Fatalf("redact = %v", got)
	}
	if rk, err := q.ListRunEnvRedactKeysForRun(ctx, run); err != nil || len(rk) != 2 {
		t.Fatalf("redact keys = %d, %v", len(rk), err)
	}

	// A receipted version whose row is gone comes back with a NULL ciphertext, so the claim
	// can park the run instead of delivering less.
	cjExec(t, pool, `DELETE FROM run_env_vars WHERE var_id=$1 AND rev=1`, rotated)
	red, err = q.ListRunEnvRedactForRun(ctx, run)
	if err != nil || len(red) != 2 {
		t.Fatalf("redact after the row is gone = %d, %v", len(red), err)
	}
	for _, r := range red {
		if r.Name == "ROTATED" && (r.Ciphertext != nil || r.SealedWith.Valid) {
			t.Fatalf("a missing row came back with a value: %+v", r)
		}
	}
	// A pending-deletion version stays in the redact set with its value.
	cjExec(t, pool, `UPDATE run_env_vars SET pending_delete_at=now() WHERE var_id=$1`, live)
	red, err = q.ListRunEnvRedactForRun(ctx, run)
	if err != nil || len(red) != 3 {
		t.Fatalf("redact with a pending deletion = %d, %v", len(red), err)
	}

	// Another run of another owner never sees this run's receipts.
	foreignRun := envSeedRun(t, pool, foreign, fs.repo)
	if red, err := q.ListRunEnvRedactForRun(ctx, foreignRun); err != nil || len(red) != 0 {
		t.Fatalf("a foreign run's redact set = %d, %v", len(red), err)
	}

	// Injected names for one claim generation: no value, only the generations that match.
	deliver(run, live, 1, "repo", "LIVE_SECRET", true, 3)
	inj, err := q.ListRunInjectedEnv(ctx, ListRunInjectedEnvParams{RunID: run, ClaimGeneration: 3})
	if err != nil || len(inj) != 1 || inj[0].Name != "LIVE_SECRET" || inj[0].Scope != "repo" || !inj[0].Secret {
		t.Fatalf("injected at generation 3 = %+v, %v", inj, err)
	}
	if inj, err := q.ListRunInjectedEnv(ctx, ListRunInjectedEnvParams{RunID: run, ClaimGeneration: 1}); err != nil || len(inj) != 3 {
		t.Fatalf("injected at generation 1 = %d, %v; want the 3 not re-delivered since", len(inj), err)
	}

	// Holders: both runs after the second delivers; capped by the limit.
	deliver(run2, rotated, 2, "repo", "ROTATED", true, 1)
	deliver(run2, live, 1, "repo", "LIVE_SECRET", true, 1)
	holders, err := q.ListRunsHoldingRunEnvVersion(ctx, ListRunsHoldingRunEnvVersionParams{VarID: live, Rev: 1, MaxRuns: 20})
	if err != nil || len(holders) != 2 {
		t.Fatalf("holders = %v, %v; want both runs", holders, err)
	}
	if one, err := q.ListRunsHoldingRunEnvVersion(ctx, ListRunsHoldingRunEnvVersionParams{VarID: live, Rev: 1, MaxRuns: 1}); err != nil || len(one) != 1 {
		t.Fatalf("holders with limit 1 = %v, %v", one, err)
	}
	if none, err := q.ListRunsHoldingRunEnvVersion(ctx, ListRunsHoldingRunEnvVersionParams{VarID: plain, Rev: 1, MaxRuns: 20}); err != nil || len(none) != 0 {
		t.Fatalf("holders of a plain version = %v, %v", none, err)
	}
}

func TestRunEnvReceiptUpsertKeepsFirstAdvancesLastLiveDB(t *testing.T) {
	pool := envReceiptsDB(t)
	ctx := context.Background()
	q := New(pool)
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	run := envSeedRun(t, pool, user, s.repo)
	v := uuid.New()
	up := func(gen int64) {
		t.Helper()
		if err := q.UpsertRunEnvReceipt(ctx, UpsertRunEnvReceiptParams{RunID: run, UserID: user, VarID: v, Rev: 1, Scope: "repo", Name: "TOKEN", Secret: true, ClaimGeneration: gen}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() (first, last int64, n int) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT first_claim_generation, last_claim_generation FROM run_env_receipts WHERE run_id=$1 AND var_id=$2 AND rev=1`, run, v).Scan(&first, &last); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_env_receipts WHERE run_id=$1`, run).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}
	up(2)
	if f, l, n := read(); f != 2 || l != 2 || n != 1 {
		t.Fatalf("after the first delivery: first=%d last=%d rows=%d", f, l, n)
	}
	up(5)
	if f, l, n := read(); f != 2 || l != 5 || n != 1 {
		t.Fatalf("after a re-delivery: first=%d last=%d rows=%d; want first kept, last advanced, one row", f, l, n)
	}
	up(3) // an older generation arriving late never moves last backwards
	if f, l, _ := read(); f != 2 || l != 5 {
		t.Fatalf("after a late older delivery: first=%d last=%d", f, l)
	}
	// Deleting the run removes its receipts; the variable's rows play no part.
	cjExec(t, pool, `DELETE FROM runs WHERE id=$1`, run)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_env_receipts WHERE run_id=$1`, run).Scan(&n); err != nil || n != 0 {
		t.Fatalf("receipts after the run is deleted = %d, %v", n, err)
	}
}

// The share lock the claim takes on the owner row conflicts with a variable write's FOR UPDATE
// and fails at once (55P03) instead of waiting.
func TestLockUserVaultStateForShareNowaitLiveDB(t *testing.T) {
	pool := envReceiptsDB(t)
	ctx := context.Background()
	user := cjSeedUser(t, pool, nil)
	if row, err := New(pool).LockUserVaultStateForShareNowait(ctx, user); err != nil || row.VaultMode == "" {
		t.Fatalf("uncontended share lock = %+v, %v", row, err)
	}
	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := New(writer).LockUserVaultState(ctx, user); err != nil {
		t.Fatal(err)
	}
	claim, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = claim.Rollback(ctx) }()
	_, err = New(claim).LockUserVaultStateForShareNowait(ctx, user)
	var pgErr *pgconn.PgError
	if !asPgErr(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("share lock against a writer = %v; want 55P03", err)
	}
}
