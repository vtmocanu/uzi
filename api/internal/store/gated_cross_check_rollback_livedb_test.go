package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #3708 R3: the migration pair 00387/00388 is fail-closed on the way down and recreatable on
// the way up. Every test here migrates its own UUID-named database, never the shared one.

type gatedSchemaFacts struct {
	version        int64
	column         bool
	trigger        bool
	function       bool
	modeCheck      bool
	modeValidated  bool
	reasonHasNew   bool
	reasonValidate bool
}

func gatedFacts(ctx context.Context, t *testing.T, pool *pgxpool.Pool) gatedSchemaFacts {
	t.Helper()
	var f gatedSchemaFacts
	q := func(sql string, dest ...any) {
		t.Helper()
		if err := pool.QueryRow(ctx, sql).Scan(dest...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	q(`SELECT max(version_id) FROM goose_db_version WHERE is_applied`, &f.version)
	q(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'runs' AND column_name = 'plan_cross_check_gated')`, &f.column)
	q(`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'runs'::regclass AND tgname = 'enforce_gated_plan_approval' AND NOT tgisinternal)`, &f.trigger)
	q(`SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'enforce_gated_plan_approval')`, &f.function)
	q(`SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'runs'::regclass AND conname = 'runs_plan_cross_check_mode_check')`, &f.modeCheck)
	if f.modeCheck {
		q(`SELECT convalidated FROM pg_constraint WHERE conrelid = 'runs'::regclass AND conname = 'runs_plan_cross_check_mode_check'`, &f.modeValidated)
	}
	q(`SELECT pg_get_constraintdef(oid) LIKE '%worker_unsupported%', convalidated FROM pg_constraint
		WHERE conrelid = 'runs'::regclass AND conname = 'runs_plan_cross_check_gate_reason_check'`, &f.reasonHasNew, &f.reasonValidate)
	return f
}

func requireGatedSchema(t *testing.T, stage string, got, want gatedSchemaFacts) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: schema = %+v, want %+v", stage, got, want)
	}
}

var (
	gatedHead = gatedSchemaFacts{version: 388, column: true, trigger: true, function: true,
		modeCheck: true, modeValidated: true, reasonHasNew: true, reasonValidate: true}
	// Between the pair: 00387 applied, 00388's VALIDATE not.
	gatedBetween = gatedSchemaFacts{version: 387, column: true, trigger: true, function: true,
		modeCheck: true, modeValidated: false, reasonHasNew: true, reasonValidate: false}
	// After 00387's Down: the 00312 reason list, restored NOT VALID.
	gatedBelow = gatedSchemaFacts{version: 386, reasonHasNew: false, reasonValidate: false}
)

// rollbackOwner creates the user and repo every kind=issue row needs (runs_kind_shape).
func rollbackOwner(ctx context.Context, t *testing.T, pool *pgxpool.Pool) rollbackIDs {
	t.Helper()
	ids := rollbackIDs{user: uuid.New(), repo: uuid.New()}
	conn := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, ids.user, ids.user.String()+"@e2e")
	mustExec(ctx, t, pool, `INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, conn, ids.user, []byte{0x1})
	mustExec(ctx, t, pool, `INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, ids.repo, conn)
	return ids
}

type rollbackIDs struct{ user, repo uuid.UUID }

var rollbackIssue atomic.Int64

func insertRollbackRun(ctx context.Context, t *testing.T, pool *pgxpool.Pool, owner rollbackIDs, status, reason string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	var reasonArg any
	if reason != "" {
		reasonArg = reason
	}
	mustExec(ctx, t, pool, `INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness,
		plan_cross_check_gated, plan_cross_check_gate_reason) VALUES ($1, $2, $3, $4, 't', 'd', $5, 'claude', true, $6)`,
		id, owner.user, owner.repo, rollbackIssue.Add(1), status, reasonArg)
	return id
}

func requireRefusedDown(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "non-terminal gated plan cross-check runs exist; abort the rollback") {
		t.Fatalf("Down was not refused by the guard: %v", err)
	}
}

func TestGatedPlanCrossCheckMigrationDownLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "gated_down_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	owner := rollbackOwner(ctx, t, pool)
	requireGatedSchema(t, "head", gatedFacts(ctx, t, pool), gatedHead)

	// 00388's Down refuses first, including an awaiting_approval run that already carries an
	// applied approval (approval does not make it terminal).
	approved := insertRollbackRun(ctx, t, pool, owner, "awaiting_approval", "")
	mustExec(ctx, t, pool, `UPDATE runs SET plan_md = 'p', gate_revision = 1, gate_presented_payload = '{"plan_md":"p"}' WHERE id = $1`, approved)
	mustExec(ctx, t, pool, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, applied_at, disposition, gate_binding, gate_revision)
		VALUES ($1, 'approve_plan', '{}', now(), now(), 'applied', 'bound', 1)`, approved)
	requireRefusedDown(t, store.MigrateDownTo(ctx, dsn, 386))
	requireGatedSchema(t, "after refused 00388 Down", gatedFacts(ctx, t, pool), gatedHead)
	for _, status := range []string{"queued", "running"} {
		mustExec(ctx, t, pool, `UPDATE runs SET status = $2 WHERE id = $1`, approved, status)
		requireRefusedDown(t, store.MigrateDownTo(ctx, dsn, 386))
		requireGatedSchema(t, "after refused Down with a "+status+" gated run", gatedFacts(ctx, t, pool), gatedHead)
	}

	// 00387's Down carries the same guard: stand at 00387, add a live gated run, refuse again.
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, approved)
	if err := store.MigrateDownTo(ctx, dsn, 387); err != nil {
		t.Fatalf("Down to 00387 with only terminal gated runs: %v", err)
	}
	requireGatedSchema(t, "at 00387", gatedFacts(ctx, t, pool), gatedBetween)
	live := insertRollbackRun(ctx, t, pool, owner, "running", "")
	requireRefusedDown(t, store.MigrateDownTo(ctx, dsn, 386))
	requireGatedSchema(t, "after refused 00387 Down", gatedFacts(ctx, t, pool), gatedBetween)

	// Terminal runs: worker_unsupported is remapped, everything else is removed.
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, live)
	unsupported := insertRollbackRun(ctx, t, pool, owner, "cancelled", "worker_unsupported")
	if err := store.MigrateDownTo(ctx, dsn, 386); err != nil {
		t.Fatalf("Down with only terminal gated runs: %v", err)
	}
	requireGatedSchema(t, "at 00386", gatedFacts(ctx, t, pool), gatedBelow)
	var remapped string
	if err := pool.QueryRow(ctx, `SELECT plan_cross_check_gate_reason FROM runs WHERE id = $1`, unsupported).Scan(&remapped); err != nil || remapped != "checker_unavailable" {
		t.Fatalf("worker_unsupported remapped to %q (err=%v), want checker_unavailable", remapped, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runs SET plan_cross_check_gate_reason = 'worker_unsupported' WHERE id = $1`, unsupported); err == nil {
		t.Fatal("the 00312 reason list admitted worker_unsupported after the Down")
	}
}

// Up -> Down -> Up recreates the column, constraints, trigger and function (twice, so a leftover
// object from a Down would fail the second Up), and the recreated trigger still guards the gate.
func TestGatedPlanCrossCheckMigrationCycleLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "gated_cycle_")
	var owner rollbackIDs
	for cycle := 1; cycle <= 2; cycle++ {
		if err := store.Migrate(ctx, dsn); err != nil {
			t.Fatalf("cycle %d Up: %v", cycle, err)
		}
		pool, err := store.OpenPool(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		if cycle == 1 {
			owner = rollbackOwner(ctx, t, pool)
		}
		requireGatedSchema(t, "cycle Up", gatedFacts(ctx, t, pool), gatedHead)

		// An insert-matrix row and the trigger behave again after the Up.
		run := insertRollbackRun(ctx, t, pool, owner, "awaiting_approval", "")
		mustExec(ctx, t, pool, `UPDATE runs SET plan_md = 'p', gate_revision = 1, gate_presented_payload = '{"plan_md":"p"}' WHERE id = $1`, run)
		_, err = pool.Exec(ctx, `UPDATE runs SET status = 'running' WHERE id = $1`, run)
		requirePgCheckViolation(t, err, "runs_gated_plan_approval")
		mustExec(ctx, t, pool, `INSERT INTO run_user_inputs (run_id, kind, body, consumed_at, applied_at, disposition, gate_binding, gate_revision)
			VALUES ($1, 'approve_plan', '{}', now(), now(), 'applied', 'bound', 1)`, run)
		mustExec(ctx, t, pool, `UPDATE runs SET status = 'running' WHERE id = $1`, run)
		_, err = pool.Exec(ctx, `INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, plan_cross_check_gated, plan_cross_check_required)
			VALUES ($1, $2, $3, 't', 'd', true, true)`, owner.user, owner.repo, rollbackIssue.Add(1))
		requirePgCheckViolation(t, err, "runs_plan_cross_check_mode_check")

		mustExec(ctx, t, pool, `UPDATE runs SET status = 'failed' WHERE plan_cross_check_gated`)
		pool.Close()
		if err := store.MigrateDownTo(ctx, dsn, 386); err != nil {
			t.Fatalf("cycle %d Down: %v", cycle, err)
		}
		pool, err = store.OpenPool(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		requireGatedSchema(t, "cycle Down", gatedFacts(ctx, t, pool), gatedBelow)
		pool.Close()
	}
	// The schema also ends where a never-downgraded database does.
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatalf("final Up: %v", err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	requireGatedSchema(t, "final Up", gatedFacts(ctx, t, pool), gatedHead)
}

// The operator's drain script, executed against a real database: it raises while any gated run is
// not terminal and passes (writing nothing) otherwise. The runner is psql with ON_ERROR_STOP and
// one transaction; a transaction here is the same unit.
func TestGatedCrossCheckDrainScriptLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "gated_drain_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	owner := rollbackOwner(ctx, t, pool)
	script := readFile(t, filepath.Join("..", "..", "..", "scripts", "gated-cross-check-drain-check.sql"))
	run := func() error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx, script)
		return err
	}
	if err := run(); err != nil {
		t.Fatalf("empty database: %v", err)
	}
	// A required (non-gated) live run and a terminal gated run are not admissions.
	mustExec(ctx, t, pool, `INSERT INTO runs (user_id, repo_id, issue_iid, issue_title, issue_description, status, plan_cross_check_required, auto_approve)
		VALUES ($1, $2, $3, 't', 'd', 'running', true, true)`, owner.user, owner.repo, rollbackIssue.Add(1))
	insertRollbackRun(ctx, t, pool, owner, "completed", "")
	if err := run(); err != nil {
		t.Fatalf("only terminal gated runs: %v", err)
	}
	for _, status := range []string{"queued", "awaiting_approval", "running", "paused"} {
		live := insertRollbackRun(ctx, t, pool, owner, status, "")
		err := run()
		if err == nil || !strings.Contains(err.Error(), "1 non-terminal gated plan cross-check runs exist; abort the rollback") {
			t.Fatalf("%s gated run: script error = %v", status, err)
		}
		var pgErr interface{ SQLState() string }
		if !errors.As(err, &pgErr) || pgErr.SQLState() != "P0001" {
			t.Fatalf("%s gated run: want a raised exception (P0001), got %v", status, err)
		}
		mustExec(ctx, t, pool, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, live)
	}
	if err := run(); err != nil {
		t.Fatalf("after draining: %v", err)
	}
}
