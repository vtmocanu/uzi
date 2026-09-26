package workersvc

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// Issue #1766 M2: a worker whose codex refresh/release was answered 409 vault_locked reports
// {status:"recovery_wait", recovery_cause:"vault_locked"}; the run parks as recovery_wait with
// recovery_wait_cause='vault_locked', the timer promoter re-queues it, and the claim vault gate
// holds it queued until the owner unlocks. Against a REAL Postgres; skipped unless
// UZI_TEST_DATABASE_URL is set (run via ./e2e/run-store-it.sh). Every name ends LiveDB.

// vaultLockedCause reads a run's (status, recovery_wait_cause).
func vaultLockedCause(t *testing.T, env codexTestEnv, runID uuid.UUID) (string, pgtype.Text) {
	t.Helper()
	var status string
	var cause pgtype.Text
	if err := env.pool.QueryRow(env.ctx, `SELECT status, recovery_wait_cause FROM runs WHERE id = $1`, runID).
		Scan(&status, &cause); err != nil {
		t.Fatalf("read run %s: %v", runID, err)
	}
	return status, cause
}

// TestRecoveryWaitVaultLockedParkPromoteClaimLiveDB walks the whole M2 lifecycle: a stale
// claim_generation report is refused with nothing written; the current generation parks the
// run recovery_wait/vault_locked; the promoter re-queues it once recovery_retry_not_before
// passes; with the owner's vault locked Service.Claim answers idle and the run stays queued;
// after the unlock the same worker claims it.
func TestRecoveryWaitVaultLockedParkPromoteClaimLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	enableClaimAssembly(t, env, userID)
	wk := seedSnapshotWorker(t, env, userID, "nonce-vault")
	svc := snapshotSvc(env, testParams())
	runID := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 3, 0)
	vaultLocked := "vault_locked"

	// A report from a superseded flight (generation 2) is refused and parks nothing.
	_, applied, err := svc.SetState(env.ctx, wkrRow(t, env, wk), runID,
		StateRequest{State: "recovery_wait", RecoveryCause: &vaultLocked, ClaimGeneration: i64Ptr(2)})
	if applied || (err != nil && !errors.Is(err, ErrStaleClaim)) {
		t.Fatalf("stale-generation report: applied=%v err=%v, want refused (not applied, nil or ErrStaleClaim)", applied, err)
	}
	if status, cause := vaultLockedCause(t, env, runID); status != "running" || cause.Valid {
		t.Fatalf("after the stale report: status=%q cause=%v, want running/NULL", status, cause)
	}

	// The current flight's report parks the run and stores the cause.
	run, applied, err := svc.SetState(env.ctx, wkrRow(t, env, wk), runID,
		StateRequest{State: "recovery_wait", RecoveryCause: &vaultLocked, ClaimGeneration: i64Ptr(3)})
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("vault_locked park: applied=%v status=%q err=%v, want applied recovery_wait", applied, run.Status, err)
	}
	status, cause := vaultLockedCause(t, env, runID)
	if status != "recovery_wait" || !cause.Valid || cause.String != "vault_locked" {
		t.Fatalf("after the park: status=%q cause=%v, want recovery_wait/vault_locked", status, cause)
	}
	// A redelivery of the same report is the idempotent 0-row no-op (positive source guard).
	if _, applied, err := svc.SetState(env.ctx, wkrRow(t, env, wk), runID,
		StateRequest{State: "recovery_wait", RecoveryCause: &vaultLocked, ClaimGeneration: i64Ptr(3)}); applied || err != nil {
		t.Fatalf("redelivered park: applied=%v err=%v, want a not-applied no-op", applied, err)
	}

	// The timer promoter does NOT fence out vault_locked (only codex_account_unavailable).
	env.exec(`UPDATE runs SET recovery_retry_not_before = now() - interval '1 minute' WHERE id = $1`, runID)
	promoted, err := env.q.PromoteRecoveryWaitRuns(env.ctx, pgconv.Time(time.Now()))
	if err != nil {
		t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
	}
	found := false
	for _, p := range promoted {
		found = found || p.ID == runID
	}
	if !found || statusOf(t, env, runID) != "queued" {
		t.Fatalf("promoter did not re-queue the vault_locked run (promoted=%v status=%q)", found, statusOf(t, env, runID))
	}
	// Keep the affinity/spread windows out of the claim decision below.
	env.exec(`UPDATE runs SET updated_at = now() - interval '3 hours' WHERE id = $1`, runID)

	// The owner's vault is locked (a real vault that has never been unlocked in this process):
	// Claim answers idle and the run waits queued.
	vlt := vault.New(env.box, env.q)
	svc.SetVault(vlt)
	if vlt.Unlocked(userID) {
		t.Fatal("fixture: vault unexpectedly unlocked")
	}
	payload, err := svc.Claim(env.ctx, wkrRow(t, env, wk), nil)
	if err != nil || payload != nil {
		t.Fatalf("Claim with the vault locked: payload=%v err=%v, want idle", payload != nil, err)
	}
	if got := statusOf(t, env, runID); got != "queued" {
		t.Fatalf("status with the vault locked = %q, want queued", got)
	}

	// After the unlock the run is claimable again.
	if err := vlt.Unlock(env.ctx, userID, codexVaultLockedTestPassword); err != nil {
		t.Fatalf("unlock vault: %v", err)
	}
	payload, err = svc.Claim(env.ctx, wkrRow(t, env, wk), nil)
	if err != nil {
		t.Fatalf("Claim after unlock: %v", err)
	}
	if payload == nil || payload.RunID != runID.String() {
		t.Fatalf("Claim after unlock = %+v, want run %s claimed", payload, runID)
	}
}

// TestRecoveryWaitUntypedCausesStayNullLiveDB pins PRD #1392 D9 alongside the new cause:
// empty_turn and an absent cause park with recovery_wait_cause NULL, and an untyped park
// REPLACES an earlier vault_locked cause rather than coalescing it.
func TestRecoveryWaitUntypedCausesStayNullLiveDB(t *testing.T) {
	env := setupCodexLiveDB(t)
	userID, _, repoID := env.seedCodexInfra(t)
	wk := seedSnapshotWorker(t, env, userID, "nonce-untyped")
	svc := snapshotSvc(env, testParams())
	emptyTurn := "empty_turn"

	for name, cause := range map[string]*string{"empty_turn": &emptyTurn, "absent": nil} {
		runID := seedOutageRun(t, env, userID, repoID, wk, "running", "issue", 1, 0)
		// A leftover typed cause from an earlier park must be replaced, not kept.
		env.exec(`UPDATE runs SET recovery_wait_cause = 'vault_locked' WHERE id = $1`, runID)
		_, applied, err := svc.SetState(env.ctx, wkrRow(t, env, wk), runID,
			StateRequest{State: "recovery_wait", RecoveryCause: cause, ClaimGeneration: i64Ptr(1)})
		if err != nil || !applied {
			t.Fatalf("%s: park applied=%v err=%v", name, applied, err)
		}
		if status, got := vaultLockedCause(t, env, runID); status != "recovery_wait" || got.Valid {
			t.Fatalf("%s: status=%q cause=%v, want recovery_wait/NULL", name, status, got)
		}
	}
}

// TestRecoveryWaitVaultLockedMigrationRoundTripLiveDB proves migration 00255 in an ISOLATED
// database: Up admits 'vault_locked' (and still refuses an unknown cause); Down clears every
// vault_locked cause to NULL and restores the four-value CHECK, which then refuses it.
func TestRecoveryWaitVaultLockedMigrationRoundTripLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	env := setupCodexLiveDB(t) // migrates the shared DB; only its ctx is used below
	ctx := env.ctx

	name := "vault_locked_mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	admin, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	admin.Close()
	t.Cleanup(func() {
		c, err := store.OpenPool(ctx, dsn)
		if err != nil {
			t.Logf("cleanup open: %v", err)
			return
		}
		defer c.Close()
		if _, err := c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("cleanup drop: %v", err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	u.Path = "/" + name
	isoDSN := u.String()

	if err := store.MigrateTo(ctx, isoDSN, 255); err != nil {
		t.Fatalf("MigrateTo(255): %v", err)
	}
	pool, err := store.OpenPool(ctx, isoDSN)
	if err != nil {
		t.Fatalf("open isolated pool: %v", err)
	}
	defer pool.Close()
	exec := func(sql string, args ...any) error {
		_, err := pool.Exec(ctx, sql, args...)
		return err
	}

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, []any{userID, fmt.Sprintf("vl-%s@e2e", userID)}},
		{`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		  VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, 'x')`, []any{connID, userID}},
		{`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		  VALUES ($1, $2, 1, 'g/vl', 'https://forge.e2e/g/vl', 'main', true)`, []any{repoID, connID}},
	} {
		if err := exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seedRun := func(iid int, cause string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, recovery_wait_cause)
		                VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'recovery_wait', $5)`, id, userID, repoID, iid, cause); err != nil {
			t.Fatalf("seed run with cause %q at 00255: %v", cause, err)
		}
		return id
	}
	locked := seedRun(1, "vault_locked")
	forge := seedRun(2, "forge_unreachable")
	if err := exec(`UPDATE runs SET recovery_wait_cause = 'not_a_cause' WHERE id = $1`, forge); err == nil {
		t.Fatal("00255's CHECK admitted an unknown cause")
	}

	if err := store.MigrateDownTo(ctx, isoDSN, 254); err != nil {
		t.Fatalf("MigrateDownTo(254): %v", err)
	}
	causeOf := func(id uuid.UUID) pgtype.Text {
		t.Helper()
		var c pgtype.Text
		if err := pool.QueryRow(ctx, `SELECT recovery_wait_cause FROM runs WHERE id = $1`, id).Scan(&c); err != nil {
			t.Fatalf("read cause: %v", err)
		}
		return c
	}
	if c := causeOf(locked); c.Valid {
		t.Fatalf("after Down: vault_locked run cause = %q, want NULL", c.String)
	}
	if c := causeOf(forge); !c.Valid || c.String != "forge_unreachable" {
		t.Fatalf("after Down: forge run cause = %v, want forge_unreachable untouched", c)
	}
	if err := exec(`UPDATE runs SET recovery_wait_cause = 'vault_locked' WHERE id = $1`, locked); err == nil {
		t.Fatal("after Down the restored CHECK still admits vault_locked")
	}
}
