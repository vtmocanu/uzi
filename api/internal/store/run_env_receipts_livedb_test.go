package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/leadcfg"
)

// PRD #4416 M2: delivery receipts, the variable identity guard, the claim-time
// reads and the lead-lane predicate, against a real PostgreSQL. Skipped without
// UZI_TEST_DATABASE_URL (see codexJudgeMigrationDB).

func envRunDB(t *testing.T) (*pgxpool.Pool, *Queries) {
	t.Helper()
	pool, _ := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_receipts"))
	return pool, New(pool)
}

// seedEnvRun inserts a run of the user on the repo and returns its id.
func seedEnvRun(t *testing.T, pool *pgxpool.Pool, user, repo uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	cjExec(t, pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
 VALUES ($1,$2,$3,$4,'t','d','running','issue')`, id, user, repo, int(id[0])*1000+int(id[1]))
	return id
}

func wantCheckViolation(t *testing.T, err error) {
	t.Helper()
	wantConstraint(t, err, "23514", "")
}

func TestRunEnvVarIdentityRefusesReuseWhileIdentityRowExistsLiveDB(t *testing.T) {
	pool, _ := envRunDB(t)
	user, other := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	s2 := seedEnvRepo(t, pool, user)
	varID := uuid.New()
	if err := envInsert(pool, user, s.repo, "TOKEN", true, 1, varID, "live"); err != nil {
		t.Fatal(err)
	}
	// A second version of the same variable is the same identity.
	if err := envInsert(pool, user, s.repo, "TOKEN", true, 2, varID, "retired"); err != nil {
		t.Fatalf("another version of the same identity: %v", err)
	}
	// A reused var_id under another name, repo, scope or owner is refused.
	wantCheckViolation(t, envInsert(pool, user, s.repo, "OTHER_NAME", true, 3, varID, "live"))
	wantCheckViolation(t, envInsert(pool, user, s2.repo, "TOKEN", true, 3, varID, "live"))
	wantCheckViolation(t, envInsert(pool, user, uuid.Nil, "TOKEN", true, 3, varID, "live"))
	wantCheckViolation(t, envInsert(pool, other, s.repo, "TOKEN", true, 3, varID, "live"))

	// Deleting every row of the variable leaves its identity row, which still binds the id.
	cjExec(t, pool, `DELETE FROM run_env_vars WHERE var_id=$1`, varID)
	wantCheckViolation(t, envInsert(pool, user, s.repo, "RENAMED", true, 1, varID, "live"))
	if err := envInsert(pool, user, s.repo, "TOKEN", true, 1, varID, "live"); err != nil {
		t.Fatalf("re-creating the same identity: %v", err)
	}

	// The identity columns cannot be updated; other columns can.
	wantCheckViolation(t, execErr(pool, `UPDATE run_env_vars SET name='MOVED' WHERE var_id=$1`, varID))
	wantCheckViolation(t, execErr(pool, `UPDATE run_env_vars SET repo_id=$2 WHERE var_id=$1`, varID, s2.repo))
	wantCheckViolation(t, execErr(pool, `UPDATE run_env_vars SET repo_id=NULL WHERE var_id=$1`, varID))
	wantCheckViolation(t, execErr(pool, `UPDATE run_env_vars SET user_id=$2 WHERE var_id=$1`, varID, other))
	wantCheckViolation(t, execErr(pool, `UPDATE run_env_vars SET var_id=$2 WHERE var_id=$1`, varID, uuid.New()))
	if err := execErr(pool, `UPDATE run_env_vars SET description='fine', disabled_at=now() WHERE var_id=$1`, varID); err != nil {
		t.Fatalf("a metadata update: %v", err)
	}

	// A repo delete cascades the rows and leaves the identity behind.
	cjExec(t, pool, `DELETE FROM repos WHERE id=$1`, s.repo)
	var rows, ids int
	if err := pool.QueryRow(context.Background(), `SELECT (SELECT count(*) FROM run_env_vars WHERE var_id=$1), (SELECT count(*) FROM run_env_var_ids WHERE var_id=$1)`, varID).Scan(&rows, &ids); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || ids != 1 {
		t.Fatalf("after the repo delete: %d rows, %d identity rows; want 0 and 1", rows, ids)
	}
	wantCheckViolation(t, envInsert(pool, user, uuid.Nil, "TOKEN", true, 1, varID, "live"))
}

func execErr(pool *pgxpool.Pool, sql string, args ...any) error {
	_, err := pool.Exec(context.Background(), sql, args...)
	return err
}

func TestRunEnvVarIdentityBackfillLiveDB(t *testing.T) {
	// Migrate to M1, write rows, then apply M2: the identity table is backfilled.
	_, dsn := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_vars"))
	pool := cjOpenPool(t, dsn)
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	repoVar, userVar := uuid.New(), uuid.New()
	for _, c := range []struct {
		repo  uuid.UUID
		name  string
		rev   int
		id    uuid.UUID
		state string
	}{{s.repo, "REPO_VAR", 1, repoVar, "retired"}, {s.repo, "REPO_VAR", 2, repoVar, "live"}, {uuid.Nil, "USER_VAR", 1, userVar, "live"}} {
		if err := envInsert(pool, user, c.repo, c.name, true, c.rev, c.id, c.state); err != nil {
			t.Fatal(err)
		}
	}
	if err := MigrateTo(context.Background(), dsn, migrationVersionByName(t, "run_env_receipts")); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[uuid.UUID][3]string{repoVar: {"repo", "REPO_VAR", "1"}, userVar: {"user", "USER_VAR", "0"}} {
		var scope, name string
		var hasRepo bool
		if err := pool.QueryRow(context.Background(), `SELECT scope, name, repo_id IS NOT NULL FROM run_env_var_ids WHERE var_id=$1 AND user_id=$2`, id, user).Scan(&scope, &name, &hasRepo); err != nil {
			t.Fatalf("identity of %s: %v", want[1], err)
		}
		if scope != want[0] || name != want[1] || hasRepo != (want[2] == "1") {
			t.Errorf("identity of %s = %s %s repo=%v", want[1], scope, name, hasRepo)
		}
	}
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM run_env_var_ids WHERE user_id=$1`, user).Scan(&n); err != nil || n != 2 {
		t.Fatalf("identity rows = %d, %v; want one per variable", n, err)
	}
}

func TestRunEnvReceiptsLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q := envRunDB(t)
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	run, run2 := seedEnvRun(t, pool, user, s.repo), seedEnvRun(t, pool, user, s.repo)
	secretVar, plainVar := uuid.New(), uuid.New()

	hasReceipt := func(v uuid.UUID, rev int32) bool {
		t.Helper()
		got, err := q.RunEnvVersionHasReceipt(ctx, v, rev)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if hasReceipt(secretVar, 1) {
		t.Fatal("a version nobody received reports a receipt")
	}
	upsert := func(r uuid.UUID, v uuid.UUID, rev int32, name string, secret bool, gen int64) {
		t.Helper()
		if err := q.UpsertRunEnvReceipt(ctx, UpsertRunEnvReceiptParams{RunID: r, UserID: user, VarID: v, Rev: rev, Scope: "repo", Name: name, Secret: secret, Generation: gen}); err != nil {
			t.Fatal(err)
		}
	}
	upsert(run, secretVar, 1, "SECRET_ONE", true, 3)
	upsert(run, plainVar, 1, "PLAIN_ONE", false, 3)
	if !hasReceipt(secretVar, 1) {
		t.Fatal("a secret receipt is not reported")
	}
	if hasReceipt(plainVar, 1) {
		t.Fatal("a plain receipt retains a version")
	}
	if hasReceipt(secretVar, 2) {
		t.Fatal("a receipt for rev 1 answers for rev 2")
	}

	// Generations: first is kept, last only moves forward.
	gens := func() (first, last int64) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT first_claim_generation, last_claim_generation FROM run_env_receipts WHERE run_id=$1 AND var_id=$2 AND rev=1`, run, secretVar).Scan(&first, &last); err != nil {
			t.Fatal(err)
		}
		return
	}
	upsert(run, secretVar, 1, "SECRET_ONE", true, 5)
	if f, l := gens(); f != 3 || l != 5 {
		t.Fatalf("after a later claim: first=%d last=%d, want 3 and 5", f, l)
	}
	upsert(run, secretVar, 1, "SECRET_ONE", true, 4)
	if f, l := gens(); f != 3 || l != 5 {
		t.Fatalf("after an older claim replays: first=%d last=%d, want 3 and 5", f, l)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_env_receipts WHERE run_id=$1`, run).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("receipt rows = %d, %v; want 2", rows, err)
	}

	// Per-run secret receipts.
	if has, err := q.RunHasSecretRunEnvReceipts(ctx, run); err != nil || !has {
		t.Fatalf("RunHasSecretRunEnvReceipts(run) = %v, %v", has, err)
	}
	upsert(run2, plainVar, 1, "PLAIN_ONE", false, 1)
	if has, err := q.RunHasSecretRunEnvReceipts(ctx, run2); err != nil || has {
		t.Fatalf("RunHasSecretRunEnvReceipts(plain only) = %v, %v", has, err)
	}

	// The run page's view: only what the CURRENT claim generation delivered.
	cjExec(t, pool, `UPDATE runs SET claim_generation=5 WHERE id=$1`, run)
	list, err := q.ListRunEnvReceiptsForRunGeneration(ctx, run)
	if err != nil || len(list) != 1 || list[0].Name != "SECRET_ONE" || !list[0].Secret {
		t.Fatalf("receipts of generation 5 = %+v, %v; want only SECRET_ONE", list, err)
	}

	// A run delete cascades its receipts, and the version is free again.
	cjExec(t, pool, `DELETE FROM runs WHERE id=$1`, run)
	if hasReceipt(secretVar, 1) {
		t.Fatal("a deleted run still holds a receipt")
	}
}

func TestRunEnvClaimReadsLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q := envRunDB(t)
	user, foreign := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	other := seedEnvRepo(t, pool, user)
	run := seedEnvRun(t, pool, user, s.repo)

	live, disabled, changed, gone := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, c := range []struct {
		repo  uuid.UUID
		name  string
		id    uuid.UUID
		state string
	}{
		{s.repo, "LIVE_ONE", live, "live"}, {s.repo, "DISABLED_ONE", disabled, "live"},
		{s.repo, "CHANGED_ONE", changed, "retired"}, {other.repo, "OTHER_REPO", uuid.New(), "live"},
		{uuid.Nil, "USER_SCOPE", uuid.New(), "live"},
	} {
		if err := envInsert(pool, user, c.repo, c.name, true, 1, c.id, c.state); err != nil {
			t.Fatal(err)
		}
	}
	cjExec(t, pool, `UPDATE run_env_vars SET disabled_at=now() WHERE name='DISABLED_ONE'`)
	if err := envInsert(pool, foreign, seedEnvRepo(t, pool, foreign).repo, "FOREIGN", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}
	// A current version of CHANGED_ONE.
	if err := envInsert(pool, user, s.repo, "CHANGED_ONE", true, 2, changed, "live"); err != nil {
		t.Fatal(err)
	}

	eff, err := q.ListRunEnvEffectiveForRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int32{}
	for _, e := range eff {
		names[e.Name] = e.Rev
		if e.UserID != user || !e.RepoID.Valid || e.RepoID.Bytes != s.repo || len(e.Ciphertext) == 0 || e.AadVersion != 1 {
			t.Errorf("effective row %+v carries the wrong owner, repo, ciphertext or aad_version", e)
		}
	}
	if len(names) != 2 || names["LIVE_ONE"] != 1 || names["CHANGED_ONE"] != 2 {
		t.Fatalf("effective set = %v; want LIVE_ONE#1 and CHANGED_ONE#2 only (no disabled, retired, other repo, user scope, foreign)", names)
	}
	keys, err := q.ListRunEnvEffectiveKeysForRun(ctx, run)
	if err != nil || len(keys) != 2 || keys[0].Name != "CHANGED_ONE" || keys[1].Name != "LIVE_ONE" {
		t.Fatalf("effective keys = %+v, %v", keys, err)
	}

	// Receipts: LIVE_ONE#1 is still effective, CHANGED_ONE#1 was retired by a rotation,
	// DISABLED_ONE#1 is disabled, GONE#1 has no row at all, and a plain receipt is ignored.
	rec := func(v uuid.UUID, rev int32, name string, secret bool) {
		t.Helper()
		if err := q.UpsertRunEnvReceipt(ctx, UpsertRunEnvReceiptParams{RunID: run, UserID: user, VarID: v, Rev: rev, Scope: "repo", Name: name, Secret: secret, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	rec(live, 1, "LIVE_ONE", true)
	rec(changed, 1, "CHANGED_ONE", true)
	rec(disabled, 1, "DISABLED_ONE", true)
	rec(gone, 1, "GONE_ONE", true)
	rec(uuid.New(), 1, "PLAIN_ONE", false)

	redact, err := q.ListRunEnvRedactForRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ListRunEnvRedactForRunRow{}
	for _, r := range redact {
		got[r.Name] = r
	}
	if len(got) != 3 || len(redact) != 3 {
		t.Fatalf("redact set = %+v; want CHANGED_ONE, DISABLED_ONE, GONE_ONE", redact)
	}
	if r := got["CHANGED_ONE"]; !r.RowID.Valid || len(r.Ciphertext) == 0 || r.AadVersion.Int16 != 1 {
		t.Errorf("a retired version must come back with its row: %+v", r)
	}
	if r := got["DISABLED_ONE"]; !r.RowID.Valid {
		t.Errorf("a disabled version must come back with its row: %+v", r)
	}
	if r := got["GONE_ONE"]; r.RowID.Valid || r.Ciphertext != nil || r.AadVersion.Valid {
		t.Errorf("a missing version must come back with NULL row columns: %+v", r)
	}
	// Once the row is deleted the receipt is visible as missing.
	cjExec(t, pool, `DELETE FROM run_env_vars WHERE var_id=$1`, disabled)
	redact, err = q.ListRunEnvRedactForRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range redact {
		if r.Name == "DISABLED_ONE" && r.RowID.Valid {
			t.Errorf("a deleted version still has a row: %+v", r)
		}
	}
	rk, err := q.ListRunEnvRedactKeysForRun(ctx, run)
	if err != nil || len(rk) != 3 {
		t.Fatalf("redact keys = %+v, %v", rk, err)
	}
	present := map[string]bool{}
	for _, r := range rk {
		present[r.Name] = r.Present
	}
	if !present["CHANGED_ONE"] || present["DISABLED_ONE"] || present["GONE_ONE"] {
		t.Fatalf("redact key presence = %v", present)
	}
}

// TestRunEnvLaneFunctionLiveDB pins fn_run_env_lane to leadcfg.LeadLane (the Go predicate behind
// workersvc.RunEnvDelivered) for every run kind of the shared registry, with and without a
// review target and an egress profile, so a kind added to one side cannot silently miss the other.
func TestRunEnvLaneFunctionLiveDB(t *testing.T) {
	pool, _ := envRunDB(t)
	raw, err := os.ReadFile("../../../fixtures/run-kinds/registry.json")
	if err != nil {
		t.Fatal(err)
	}
	var reg struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal(raw, &reg); err != nil || len(reg.Kinds) == 0 {
		t.Fatalf("registry kinds: %v %v", reg.Kinds, err)
	}
	delivered := 0
	for _, kind := range reg.Kinds {
		for _, review := range []bool{false, true} {
			for _, egress := range []bool{false, true} {
				var reviewID, egressID any
				if review {
					reviewID = uuid.New()
				}
				if egress {
					egressID = uuid.New()
				}
				var got bool
				if err := pool.QueryRow(context.Background(), `SELECT fn_run_env_lane($1::text, $2::uuid, $3::uuid)`, kind, reviewID, egressID).Scan(&got); err != nil {
					t.Fatal(err)
				}
				want := leadcfg.LeadLane(kind, review, egress)
				if got != want {
					t.Errorf("fn_run_env_lane(%s, review=%v, egress=%v) = %v, leadcfg.LeadLane = %v", kind, review, egress, got, want)
				}
				if want {
					delivered++
				}
			}
		}
	}
	if delivered == 0 {
		t.Fatal("no kind is delivered: the pin compared nothing")
	}
}

func TestLockUserVaultStateForKeyShareNowaitLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q := envRunDB(t)
	user := cjSeedUser(t, pool, nil)
	st, err := q.LockUserVaultStateForKeyShareNowait(ctx, user)
	if err != nil || st.VaultMode == "" {
		t.Fatalf("lock vault state = %+v, %v", st, err)
	}
	// A concurrent vault change holds the row exclusively: the share lock fails at once.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id=$1 FOR UPDATE`, user); err != nil {
		t.Fatal(err)
	}
	_, err = q.LockUserVaultStateForKeyShareNowait(ctx, user)
	wantConstraint(t, err, "55P03", "")
}

func TestParkRunEnvHistoryUnavailableRunLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q := envRunDB(t)
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	run := seedEnvRun(t, pool, user, s.repo)
	worker := uuid.New()
	cjExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`, worker, user, worker.String(), worker[:])
	cjExec(t, pool, `UPDATE runs SET status='claimed', worker_id=$2, claim_generation=2, claim_released_at=NULL WHERE id=$1`, run, worker)

	params := ParkRunEnvHistoryUnavailableRunParams{ID: run, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, ClaimGeneration: 1}
	if n, err := q.ParkRunEnvHistoryUnavailableRun(ctx, params); err != nil || n != 0 {
		t.Fatalf("a stale generation parked %d rows, %v", n, err)
	}
	params.ClaimGeneration = 2
	if n, err := q.ParkRunEnvHistoryUnavailableRun(ctx, params); err != nil || n != 1 {
		t.Fatalf("the exact claim parked %d rows, %v", n, err)
	}
	var status, hold string
	var released bool
	if err := pool.QueryRow(ctx, `SELECT status, hold_reason, claim_released_at IS NOT NULL FROM runs WHERE id=$1`, run).Scan(&status, &hold, &released); err != nil {
		t.Fatal(err)
	}
	if status != "paused" || hold != "run_env_history_unavailable" || !released {
		t.Fatalf("parked run = %s / %s / released=%v", status, hold, released)
	}
	if n, err := q.ParkRunEnvHistoryUnavailableRun(ctx, params); err != nil || n != 0 {
		t.Fatalf("a second park of the released claim changed %d rows, %v", n, err)
	}
}

// A park that restores a consumed Fresh grant leaves policy_pending_fresh true; the owner's
// Resume of the run_env_history_unavailable hold must still requeue it (ResumePausedRun admits
// the hold the way it admits run_override_unavailable). Without the exception the resume
// matches no row and the run is stuck.
func TestResumeRunEnvHistoryUnavailableWithConsumedFreshGrantLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q := envRunDB(t)
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	run := seedEnvRun(t, pool, user, s.repo)
	worker := uuid.New()
	cjExec(t, pool, `INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,$3,$4,'online')`, worker, user, worker.String(), worker[:])
	cjExec(t, pool, `UPDATE runs SET status='claimed', worker_id=$2, claim_generation=2, claim_released_at=NULL,
 started_at=now(), policy_outcome_id=$3, policy_continuation_mode='same_conversation', policy_applying_worker_id=$2,
 policy_applying_generation=1, policy_source_generation=1, policy_resolved_at=now(), policy_preserve_required=true,
 policy_fresh_consumed_generation=2 WHERE id=$1`, run, worker, uuid.New())

	params := ParkRunEnvHistoryUnavailableRunParams{ID: run, WorkerID: pgtype.UUID{Bytes: worker, Valid: true}, ClaimGeneration: 2}
	if n, err := q.ParkRunEnvHistoryUnavailableRun(ctx, params); err != nil || n != 1 {
		t.Fatalf("park = %d, %v", n, err)
	}
	var pending bool
	if err := pool.QueryRow(ctx, `SELECT policy_pending_fresh FROM runs WHERE id=$1`, run).Scan(&pending); err != nil || !pending {
		t.Fatalf("the park must restore the consumed Fresh grant: pending=%v err=%v", pending, err)
	}
	row, err := q.ResumePausedRun(ctx, ResumePausedRunParams{ID: run, UserID: user, GlobalTimeoutSeconds: 3600})
	if err != nil {
		t.Fatalf("owner resume of a run_env_history_unavailable hold with a pending Fresh grant: %v", err)
	}
	if row.Status != "queued" {
		t.Fatalf("resumed status = %s; want queued", row.Status)
	}
	var stillPending bool
	if err := pool.QueryRow(ctx, `SELECT policy_pending_fresh FROM runs WHERE id=$1`, run).Scan(&stillPending); err != nil || !stillPending {
		t.Fatalf("the resume must leave the grant pending for the next claim: %v %v", stillPending, err)
	}
	// An ordinary hold with a pending grant stays refused.
	cjExec(t, pool, `UPDATE runs SET status='paused', hold_reason='manual' WHERE id=$1`, run)
	if _, err := q.ResumePausedRun(ctx, ResumePausedRunParams{ID: run, UserID: user, GlobalTimeoutSeconds: 3600}); err == nil {
		t.Fatal("a pending Fresh grant on another hold must still block the resume")
	}
}

// A run_env_vars row of another owner on the run's own repo is neither delivered nor
// redacted: the reads join on the run's owner.
func TestRunEnvForeignOwnerRowOnOwnRepoIsExcludedLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q := envRunDB(t)
	user, foreign := cjSeedUser(t, pool, nil), cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	run := seedEnvRun(t, pool, user, s.repo)
	liveForeign, retiredForeign := uuid.New(), uuid.New()
	if err := envInsert(pool, foreign, s.repo, "FOREIGN_LIVE", true, 1, liveForeign, "live"); err != nil {
		t.Fatal(err)
	}
	if err := envInsert(pool, foreign, s.repo, "FOREIGN_RETIRED", true, 1, retiredForeign, "retired"); err != nil {
		t.Fatal(err)
	}
	if err := envInsert(pool, user, s.repo, "MINE", true, 1, uuid.New(), "live"); err != nil {
		t.Fatal(err)
	}
	eff, err := q.ListRunEnvEffectiveForRun(ctx, run)
	if err != nil || len(eff) != 1 || eff[0].Name != "MINE" {
		t.Fatalf("effective = %+v, %v; want only MINE", eff, err)
	}
	keys, err := q.ListRunEnvEffectiveKeysForRun(ctx, run)
	if err != nil || len(keys) != 1 || keys[0].Name != "MINE" {
		t.Fatalf("effective keys = %+v, %v; want only MINE", keys, err)
	}
	// A receipt of the run naming the foreign retired version: the owner's rows do not hold it.
	if err := q.UpsertRunEnvReceipt(ctx, UpsertRunEnvReceiptParams{RunID: run, UserID: user, VarID: retiredForeign, Rev: 1, Scope: "repo", Name: "FOREIGN_RETIRED", Secret: true, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	red, err := q.ListRunEnvRedactForRun(ctx, run)
	if err != nil || len(red) != 1 || red[0].RowID.Valid || red[0].Ciphertext != nil {
		t.Fatalf("redact = %+v, %v; want one receipt row with NULL row columns (missing)", red, err)
	}
	rk, err := q.ListRunEnvRedactKeysForRun(ctx, run)
	if err != nil || len(rk) != 1 || rk[0].Present {
		t.Fatalf("redact keys = %+v, %v; want present=false", rk, err)
	}
}

// FOR KEY SHARE does not conflict with a plain update of a non-key users column, so a
// concurrent settings write never fails the claim finish with 55P03.
func TestLockUserVaultStateKeyShareIgnoresPlainUserUpdateLiveDB(t *testing.T) {
	ctx := context.Background()
	pool, q := envRunDB(t)
	user := cjSeedUser(t, pool, nil)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE users SET default_effort='low' WHERE id=$1`, user); err != nil {
		t.Fatal(err)
	}
	if _, err := q.LockUserVaultStateForKeyShareNowait(ctx, user); err != nil {
		t.Fatalf("a plain non-key UPDATE of the users row must not block the key-share lock: %v", err)
	}
}

// The migration reverses cleanly and applies again.
func TestRunEnvReceiptsMigrationRoundTripLiveDB(t *testing.T) {
	ctx := context.Background()
	version := migrationVersionByName(t, "run_env_receipts")
	_, dsn := codexJudgeMigrationDB(t, version)
	if err := MigrateDownTo(ctx, dsn, version-1); err != nil {
		t.Fatalf("down: %v", err)
	}
	pool := cjOpenPool(t, dsn)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('run_env_receipts','run_env_var_ids')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("tables left after down: %d, %v", n, err)
	}
	if err := MigrateTo(ctx, dsn, version); err != nil {
		t.Fatalf("up again: %v", err)
	}
}

// A var_id that already names more than one variable cannot be backfilled into a single
// identity: the migration refuses and names the problem.
func TestRunEnvReceiptsMigrationRefusesAmbiguousVarIDLiveDB(t *testing.T) {
	_, dsn := codexJudgeMigrationDB(t, migrationVersionByName(t, "run_env_vars"))
	pool := cjOpenPool(t, dsn)
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	varID := uuid.New()
	if err := envInsert(pool, user, s.repo, "FIRST_NAME", true, 1, varID, "retired"); err != nil {
		t.Fatal(err)
	}
	if err := envInsert(pool, user, s.repo, "SECOND_NAME", true, 2, varID, "live"); err != nil {
		t.Fatal(err)
	}
	err := MigrateTo(context.Background(), dsn, migrationVersionByName(t, "run_env_receipts"))
	if err == nil || !strings.Contains(err.Error(), "more than one (user_id, repo_id, name)") {
		t.Fatalf("migrate with an ambiguous var_id: %v, want the refusal", err)
	}
	// Rows of one variable that keep their name migrate.
	cjExec(t, pool, `DELETE FROM run_env_vars WHERE var_id=$1`, varID)
	if err := envInsert(pool, user, s.repo, "ONE_NAME", true, 1, varID, "retired"); err != nil {
		t.Fatal(err)
	}
	if err := envInsert(pool, user, s.repo, "ONE_NAME", true, 2, varID, "live"); err != nil {
		t.Fatal(err)
	}
	if err := MigrateTo(context.Background(), dsn, migrationVersionByName(t, "run_env_receipts")); err != nil {
		t.Fatalf("migrate with a consistent var_id: %v", err)
	}
}

// The rollback refuses while version-2 rows or receipts exist, and runs once they are gone.
func TestRunEnvReceiptsMigrationDownRefusesWithDataLiveDB(t *testing.T) {
	version := migrationVersionByName(t, "run_env_receipts")
	_, dsn := codexJudgeMigrationDB(t, version)
	pool := cjOpenPool(t, dsn)
	user := cjSeedUser(t, pool, nil)
	s := seedEnvRepo(t, pool, user)
	varID := uuid.New()
	if err := envInsert(pool, user, s.repo, "TOKEN", true, 1, varID, "live"); err != nil {
		t.Fatal(err)
	}
	cjExec(t, pool, `UPDATE run_env_vars SET aad_version=2 WHERE var_id=$1`, varID)
	ctx := context.Background()
	if err := MigrateDownTo(ctx, dsn, version-1); err == nil || !strings.Contains(err.Error(), "cannot roll back run_env_receipts") {
		t.Fatalf("down with a version-2 row: %v, want the refusal", err)
	}
	cjExec(t, pool, `UPDATE run_env_vars SET aad_version=1 WHERE var_id=$1`, varID)
	run := seedEnvRun(t, pool, user, s.repo)
	cjExec(t, pool, `INSERT INTO run_env_receipts (run_id,user_id,var_id,rev,scope,name,secret,first_claim_generation,last_claim_generation)
	    VALUES ($1,$2,$3,1,'repo','TOKEN',true,1,1)`, run, user, varID)
	if err := MigrateDownTo(ctx, dsn, version-1); err == nil || !strings.Contains(err.Error(), "cannot roll back run_env_receipts") {
		t.Fatalf("down with a receipt: %v, want the refusal", err)
	}
	cjExec(t, pool, `DELETE FROM run_env_receipts WHERE run_id=$1`, run)
	if err := MigrateDownTo(ctx, dsn, version-1); err != nil {
		t.Fatalf("down with nothing to lose: %v", err)
	}
}

// TestRunEnvReceiptsVarIndexServesIdentityScanLiveDB pins the plain var_id index the sweeper's
// orphan-identity checks need: the primary key leads with run_id and the other var_id index is
// partial (WHERE secret), so without it every sweep pass scans the whole receipts table. With
// sequential scans disabled the planner must still find an index path for the plain lookup;
// this proves the path exists, not that the planner picks it at default settings.
func TestRunEnvReceiptsVarIndexServesIdentityScanLiveDB(t *testing.T) {
	pool, _ := envRunDB(t)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	var plan string
	rows, err := tx.Query(ctx, `EXPLAIN SELECT 1 FROM run_env_receipts WHERE var_id = '00000000-0000-0000-0000-000000000001'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan += line + "\n"
	}
	rows.Close()
	if !strings.Contains(plan, "run_env_receipts_var_idx") {
		t.Fatalf("plain var_id lookup does not use run_env_receipts_var_idx:\n%s", plan)
	}
}
