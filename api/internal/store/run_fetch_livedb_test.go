package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// run_fetch_livedb_test.go pins migration 00270 (PRD #1906 M3) against a real Postgres: the
// run binding's CHECKs and immutability trigger, the RESTRICT foreign key, and the revoke
// trigger across every status writer. Skipped unless UZI_TEST_DATABASE_URL points at a
// throwaway Postgres (./e2e/run-store-it.sh).

type rfFix struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	q       *store.Queries
	userID  uuid.UUID
	repoID  uuid.UUID
	worker  uuid.UUID
	profile uuid.UUID
	iid     int64
}

func newRFFix(t *testing.T) *rfFix {
	t.Helper()
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via e2e/run-store-it.sh for live-DB coverage")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := &rfFix{ctx: ctx, pool: pool, q: store.New(pool), userID: uuid.New(), repoID: uuid.New(), worker: uuid.New(), profile: uuid.New(), iid: 1}
	connID := uuid.New()
	f.exec(t, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, f.userID, fmt.Sprintf("rf-%s@e2e", f.userID))
	f.exec(t, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	           VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, f.userID, []byte{0x1})
	f.exec(t, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	           VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/rf', 'main', true)`, f.repoID, connID, "g/rf-"+f.repoID.String())
	f.exec(t, `INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, f.worker, f.userID, f.worker[:])
	f.exec(t, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`,
		f.profile, "rf-"+strings.ReplaceAll(f.profile.String(), "-", "")[:20])
	t.Cleanup(func() {
		// runs cascade with the user's repo; the profile is RESTRICTed until they are gone.
		_, _ = pool.Exec(ctx, `DELETE FROM runs WHERE user_id = $1`, f.userID)
		_, _ = pool.Exec(ctx, `DELETE FROM egress_profiles WHERE id = $1`, f.profile)
	})
	return f
}

func (f *rfFix) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// boundRun inserts a profile-bound issue run in status, owned by the fixture's worker at
// claim generation 1, with a credential row.
func (f *rfFix) boundRun(t *testing.T, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.iid++
	f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id,
	                             claim_generation, egress_profile_id)
	           VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, 1, $7)`,
		id, f.userID, f.repoID, f.iid, status, f.worker, f.profile)
	f.exec(t, `INSERT INTO run_fetch_credentials (run_id, token_hash, claim_generation) VALUES ($1, $2, 1)`, id, randHash())
	return id
}

func randHash() []byte {
	a, b := uuid.New(), uuid.New()
	return append(a[:], b[:]...)
}

func (f *rfFix) revoked(t *testing.T, runID uuid.UUID) bool {
	t.Helper()
	var r pgtype.Timestamptz
	if err := f.pool.QueryRow(f.ctx, `SELECT revoked_at FROM run_fetch_credentials WHERE run_id = $1`, runID).Scan(&r); err != nil {
		t.Fatalf("read revoked_at: %v", err)
	}
	return r.Valid
}

func rfPGErr(err error) (code, constraint string) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code, pgErr.ConstraintName
	}
	return "", ""
}

// TestRunEgressBindingSchemaLiveDB: the two CHECKs refuse a bound Codex run and a bound chat
// run; egress_profile_id cannot change after insert (to another profile or to NULL);
// egress_snapshot can be written once and never changed; a referenced profile cannot be
// deleted (the constraint name the admin handler maps to 409).
func TestRunEgressBindingSchemaLiveDB(t *testing.T) {
	f := newRFFix(t)

	_, err := f.pool.Exec(f.ctx, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness, egress_profile_id)
	                              VALUES ($1, $2, $3, 'issue', 9001, 't', 'd', 'queued', 'codex', $4)`, uuid.New(), f.userID, f.repoID, f.profile)
	if code, c := rfPGErr(err); code != "23514" || c != "runs_egress_profile_not_codex" {
		t.Errorf("bound codex run: err=%v (code %s constraint %s), want the runs_egress_profile_not_codex CHECK", err, code, c)
	}
	_, err = f.pool.Exec(f.ctx, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status, egress_profile_id)
	                             VALUES ($1, $2, 'chat', 't', 'd', 'queued', $3)`, uuid.New(), f.userID, f.profile)
	if code, c := rfPGErr(err); code != "23514" || c != "runs_egress_profile_not_chat" {
		t.Errorf("bound chat run: err=%v (code %s constraint %s), want the runs_egress_profile_not_chat CHECK", err, code, c)
	}
	// Positive controls: the same rows unbound insert fine.
	f.exec(t, `INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, harness)
	           VALUES ($1, $2, $3, 'issue', 9002, 't', 'd', 'queued', 'codex')`, uuid.New(), f.userID, f.repoID)
	f.exec(t, `INSERT INTO runs (id, user_id, kind, issue_title, issue_description, status)
	           VALUES ($1, $2, 'chat', 't', 'd', 'queued')`, uuid.New(), f.userID)

	run := f.boundRun(t, "queued")
	// A harness flip on a bound run is the CHECK again.
	_, err = f.pool.Exec(f.ctx, `UPDATE runs SET harness = 'codex' WHERE id = $1`, run)
	if code, c := rfPGErr(err); code != "23514" || c != "runs_egress_profile_not_codex" {
		t.Errorf("harness flip on a bound run: err=%v, want the codex CHECK", err)
	}

	other := uuid.New()
	f.exec(t, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{other.example.com}')`,
		other, "rf-"+strings.ReplaceAll(other.String(), "-", "")[:20])
	t.Cleanup(func() { _, _ = f.pool.Exec(f.ctx, `DELETE FROM egress_profiles WHERE id = $1`, other) })
	for name, sql := range map[string]string{
		"re-point": `UPDATE runs SET egress_profile_id = '` + other.String() + `' WHERE id = $1`,
		"unbind":   `UPDATE runs SET egress_profile_id = NULL WHERE id = $1`,
	} {
		_, err := f.pool.Exec(f.ctx, sql, run)
		if err == nil || !strings.Contains(err.Error(), "egress_profile_id is immutable") {
			t.Errorf("%s: err=%v, want the immutability trigger", name, err)
		}
	}
	// Same-value update (no change) is allowed.
	f.exec(t, `UPDATE runs SET egress_profile_id = egress_profile_id WHERE id = $1`, run)

	if err := f.q.SetRunEgressSnapshotOnce(f.ctx, store.SetRunEgressSnapshotOnceParams{ID: run, Snapshot: []byte(`{"profile":"a","entries":["docs.example.com"]}`)}); err != nil {
		t.Fatalf("first snapshot: %v", err)
	}
	// SetRunEgressSnapshotOnce matches no row once set: the snapshot is unchanged.
	if err := f.q.SetRunEgressSnapshotOnce(f.ctx, store.SetRunEgressSnapshotOnceParams{ID: run, Snapshot: []byte(`{"profile":"b","entries":["x.example.com"]}`)}); err != nil {
		t.Fatalf("second snapshot: %v", err)
	}
	snap, err := f.q.GetRunEgressSnapshot(f.ctx, run)
	if err != nil || !strings.Contains(string(snap), `"a"`) {
		t.Fatalf("snapshot = %s, %v; want the first one", snap, err)
	}
	for name, sql := range map[string]string{
		"change": `UPDATE runs SET egress_snapshot = '{"profile":"c","entries":[]}' WHERE id = $1`,
		"clear":  `UPDATE runs SET egress_snapshot = NULL WHERE id = $1`,
	} {
		_, err := f.pool.Exec(f.ctx, sql, run)
		if err == nil || !strings.Contains(err.Error(), "egress_snapshot is immutable") {
			t.Errorf("snapshot %s: err=%v, want the immutability trigger", name, err)
		}
	}

	_, err = f.pool.Exec(f.ctx, `DELETE FROM egress_profiles WHERE id = $1`, f.profile)
	if code, c := rfPGErr(err); code != "23503" || c != "runs_egress_profile_id_fkey" {
		t.Errorf("delete a referenced profile: err=%v (code %s constraint %s), want the RESTRICT fk runs_egress_profile_id_fkey", err, code, c)
	}
}

// TestRunFetchRevokeTriggerLiveDB: the credential is revoked by every writer that moves a
// bound run out of claimed/running (terminal, requeue, park), through the real Go writers
// where one exists and a raw status UPDATE for the rest; claimed -> running does not
// revoke, and an unbound run's status change touches no credential.
func TestRunFetchRevokeTriggerLiveDB(t *testing.T) {
	f := newRFFix(t)
	wk := pgtype.UUID{Bytes: f.worker, Valid: true}

	writers := map[string]func(runID uuid.UUID) (int64, error){
		"SetRunCompleted": func(id uuid.UUID) (int64, error) {
			return f.q.SetRunCompleted(f.ctx, store.SetRunCompletedParams{ID: id, WorkerID: wk})
		},
		"MarkRunFailedByID": func(id uuid.UUID) (int64, error) {
			return f.q.MarkRunFailedByID(f.ctx, store.MarkRunFailedByIDParams{ID: id, FailureReason: pgtype.Text{String: "x", Valid: true}})
		},
		"CancelRunByWorker": func(id uuid.UUID) (int64, error) {
			return f.q.CancelRunByWorker(f.ctx, store.CancelRunByWorkerParams{ID: id, WorkerID: wk})
		},
	}
	for name, write := range writers {
		run := f.boundRun(t, "running")
		n, err := write(run)
		if err != nil || n != 1 {
			t.Fatalf("%s: rows=%d err=%v, want 1 row", name, n, err)
		}
		if !f.revoked(t, run) {
			t.Errorf("%s: the credential is not revoked", name)
		}
	}
	// The claim-assembly outcomes, fenced to status claimed.
	claimWriters := map[string]func(runID uuid.UUID) (int64, error){
		"RequeueClaimAssemblyExact(queued)": func(id uuid.UUID) (int64, error) {
			return f.q.RequeueClaimAssemblyExact(f.ctx, store.RequeueClaimAssemblyExactParams{ID: id, WorkerID: wk, ClaimGeneration: 1})
		},
		"RequeueClaimAssemblyExact(pool_wait)": func(id uuid.UUID) (int64, error) {
			return f.q.RequeueClaimAssemblyExact(f.ctx, store.RequeueClaimAssemblyExactParams{ID: id, WorkerID: wk, ClaimGeneration: 1, PoolWait: true})
		},
		"FailClaimAssemblyExact": func(id uuid.UUID) (int64, error) {
			return f.q.FailClaimAssemblyExact(f.ctx, store.FailClaimAssemblyExactParams{ID: id, WorkerID: wk, ClaimGeneration: 1,
				FailureReason: pgtype.Text{String: "x", Valid: true}, FailOrigin: pgtype.Text{String: "credential_unavailable", Valid: true}})
		},
	}
	for name, write := range claimWriters {
		run := f.boundRun(t, "claimed")
		n, err := write(run)
		if err != nil || n != 1 {
			t.Fatalf("%s: rows=%d err=%v, want 1 row", name, n, err)
		}
		if !f.revoked(t, run) {
			t.Errorf("%s: the credential is not revoked", name)
		}
	}
	// Every other status a run can leave running for.
	for _, status := range []string{"completed", "failed", "cancelled", "queued", "awaiting_approval", "awaiting_input",
		"limit_wait", "awaiting_followup", "pool_wait", "paused", "recovery_wait"} {
		run := f.boundRun(t, "running")
		f.exec(t, `UPDATE runs SET status = $2 WHERE id = $1`, run, status)
		if !f.revoked(t, run) {
			t.Errorf("running -> %s: the credential is not revoked", status)
		}
	}
	// Negative controls: claimed -> running keeps it; a same-status write keeps it.
	run := f.boundRun(t, "claimed")
	f.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, run)
	f.exec(t, `UPDATE runs SET status = 'running' WHERE id = $1`, run)
	if f.revoked(t, run) {
		t.Error("claimed -> running revoked the credential")
	}
}

// TestRunFetchMintGuardLiveDB: the mint matches only a bound run still in the claim that
// asked (status claimed/running, same generation), rotates the token and clears revoked_at
// on a re-claim, and never resets the run's counters.
func TestRunFetchMintGuardLiveDB(t *testing.T) {
	f := newRFFix(t)
	run := f.boundRun(t, "claimed")
	f.exec(t, `UPDATE run_fetch_credentials SET used_bytes = 7, files = 2, attempts = 3, revoked_at = now() WHERE run_id = $1`, run)

	mint := func(gen int64, hash []byte) int64 {
		n, err := f.q.MintRunFetchCredential(f.ctx, store.MintRunFetchCredentialParams{RunID: run, ClaimGeneration: gen, TokenHash: hash})
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		return n
	}
	if n := mint(2, randHash()); n != 0 {
		t.Errorf("a mint for another generation matched %d rows", n)
	}
	h := randHash()
	if n := mint(1, h); n != 1 {
		t.Fatalf("mint = %d rows, want 1", n)
	}
	var (
		gotHash                    []byte
		revoked                    pgtype.Timestamptz
		used, files, attempts, gen int64
	)
	if err := f.pool.QueryRow(f.ctx, `SELECT token_hash, revoked_at, used_bytes, files, attempts, claim_generation
	                                  FROM run_fetch_credentials WHERE run_id = $1`, run).Scan(&gotHash, &revoked, &used, &files, &attempts, &gen); err != nil {
		t.Fatal(err)
	}
	if string(gotHash) != string(h) || revoked.Valid || used != 7 || files != 2 || attempts != 3 || gen != 1 {
		t.Fatalf("after mint: hash rotated=%v revoked=%v used=%d files=%d attempts=%d gen=%d; want rotated, unrevoked, counters kept",
			string(gotHash) == string(h), revoked.Valid, used, files, attempts, gen)
	}
	f.exec(t, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, run)
	if n := mint(1, randHash()); n != 0 {
		t.Errorf("a mint for a cancelled run matched %d rows", n)
	}
	if !f.revoked(t, run) {
		t.Error("a refused mint un-revoked the credential")
	}
}
