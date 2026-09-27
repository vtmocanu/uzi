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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1809 M5 (D6) data-volume-full park, against a REAL Postgres: the counted park stores the
// cause and bumps disk_park_count, the cap fails the run with fail_origin='data_volume_full', a
// PREVENTIVE park is never counted or capped, and the widened CHECKs accept the new values. These
// drive the real Service (real *store.Queries + the pool as the tx beginner).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (run via
// ./e2e/run-store-it.sh). A package that prints `ok` with PASS=0 is INVALID, not green.

// diskParkService builds a Service over the live store with the pool wired as the tx beginner
// (the disk park transaction needs it) and a caller-chosen disk park cap.
func (e interlockLiveDB) diskParkService(t *testing.T, maxParks int) *Service {
	t.Helper()
	p := testParams()
	p.RunDiskParkMax = maxParks
	svc := New(e.q, newBox(t), p)
	svc.SetTxBeginner(e.pool)
	svc.SetBackground(func(func()) {})
	return svc
}

// dpRun reads the disk-park-relevant run columns.
func (e interlockLiveDB) dpRun(t *testing.T, runID uuid.UUID) (status string, cause pgtype.Text, diskParkCount, forgeParkCount int32, failOrigin pgtype.Text) {
	t.Helper()
	if err := e.pool.QueryRow(e.ctx,
		`SELECT status, recovery_wait_cause, disk_park_count, forge_park_count, fail_origin FROM runs WHERE id = $1`, runID).
		Scan(&status, &cause, &diskParkCount, &forgeParkCount, &failOrigin); err != nil {
		t.Fatalf("read run: %v", err)
	}
	return status, cause, diskParkCount, forgeParkCount, failOrigin
}

// dpResume promotes a parked run to queued through the real promoter, then re-claims it back to
// running on the same worker at the same generation, the resume a real claim performs.
func (e interlockLiveDB) dpResume(t *testing.T, runID, workerID uuid.UUID) {
	t.Helper()
	e.exec(t, `UPDATE runs SET recovery_retry_not_before = now() - interval '1 minute' WHERE id = $1`, runID)
	if _, err := e.q.PromoteRecoveryWaitRuns(e.ctx, pgconv.Time(time.Now())); err != nil {
		t.Fatalf("PromoteRecoveryWaitRuns: %v", err)
	}
	if status, _, _, _, _ := e.dpRun(t, runID); status != "queued" {
		t.Fatalf("status after promote = %q, want queued", status)
	}
	e.exec(t, `UPDATE runs SET status = 'running', worker_id = $2 WHERE id = $1`, runID, workerID)
}

func dataVolumeFullReport(gen int64, preventive *bool) StateRequest {
	return StateRequest{
		State: "recovery_wait", RecoveryCause: strPtr(recoveryCauseDataVolumeFull),
		DiskParkPreventive: preventive, ClaimGeneration: i64Ptr(gen),
	}
}

// TestDiskParkCountedStoresCauseAndBumpsLiveDB: a counted park parks the run recovery_wait with
// cause data_volume_full, stamps the retry, bumps disk_park_count, leaves forge_park_count alone,
// and leaves the generation's custody hold OPEN (a disk park keeps custody). A redelivery is the
// idempotent 409 no-op, never counted twice.
func TestDiskParkCountedStoresCauseAndBumpsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	hold := mhOpenHold(t, e, runID, 1, w)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, nil))
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("counted disk park: applied=%v status=%q err=%v, want applied recovery_wait", applied, run.Status, err)
	}
	status, cause, disk, forge, _ := e.dpRun(t, runID)
	if status != "recovery_wait" || !cause.Valid || cause.String != "data_volume_full" {
		t.Fatalf("after the park: status=%q cause=%v, want recovery_wait/data_volume_full", status, cause)
	}
	if disk != 1 || forge != 0 {
		t.Fatalf("disk_park_count=%d forge_park_count=%d, want 1/0", disk, forge)
	}
	if _, _, _, retrySet, _ := e.fpRun(t, runID); !retrySet {
		t.Fatal("recovery_retry_not_before was not stamped")
	}
	if state, _ := e.fpHoldEvidence(t, hold); state != "open" {
		t.Fatalf("hold state = %q, want open (a disk park keeps custody)", state)
	}

	run, applied, err = svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, nil))
	if err != nil || applied || run.Status != "recovery_wait" {
		t.Fatalf("redelivered disk park: applied=%v status=%q err=%v, want a not-applied recovery_wait no-op", applied, run.Status, err)
	}
	if _, _, disk, _, _ := e.dpRun(t, runID); disk != 1 {
		t.Fatalf("disk_park_count = %d after a redelivery, want 1 (never counted twice)", disk)
	}
}

// TestDiskParkStaleGenerationRefusedLiveDB: a report from a superseded generation is the generic
// stale_claim refusal with nothing mutated.
func TestDiskParkStaleGenerationRefusedLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 2)

	_, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, nil))
	if !errors.Is(err, ErrStaleClaim) || applied {
		t.Fatalf("stale disk park: applied=%v err=%v, want ErrStaleClaim", applied, err)
	}
	if status, cause, disk, _, _ := e.dpRun(t, runID); status != "running" || cause.Valid || disk != 0 {
		t.Fatalf("stale report mutated the run: status=%q cause=%v disk_park_count=%d", status, cause, disk)
	}
}

// TestDiskParkCapFailsRunLiveDB: with the cap at 3, the fourth counted park fails the run with
// the server-derived fail_origin data_volume_full and the fixed count-naming reason, and enqueues
// no judge (enableJudge makes the judge path reachable, so judges==0 is a real assertion).
func TestDiskParkCapFailsRunLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 3)
	e.enableJudge(t, svc)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	for i := 1; i <= 3; i++ {
		if _, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, nil)); err != nil || !applied {
			t.Fatalf("counted park %d: applied=%v err=%v", i, applied, err)
		}
		if _, _, disk, _, _ := e.dpRun(t, runID); int(disk) != i {
			t.Fatalf("disk_park_count after park %d = %d", i, disk)
		}
		e.dpResume(t, runID, w)
	}

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, nil))
	if err != nil || !applied {
		t.Fatalf("cap-exceeding park: applied=%v err=%v", applied, err)
	}
	if run.Status != "failed" {
		t.Fatalf("run status = %q, want failed (past the cap)", run.Status)
	}
	status, _, disk, _, origin := e.dpRun(t, runID)
	if status != "failed" || !origin.Valid || origin.String != "data_volume_full" {
		t.Fatalf("status=%q fail_origin=%v, want failed/data_volume_full", status, origin)
	}
	if disk != 3 {
		t.Fatalf("disk_park_count = %d after the cap-fail, want 3 (the failing park is not a park)", disk)
	}
	var reason pgtype.Text
	if err := e.pool.QueryRow(e.ctx, `SELECT failure_reason FROM runs WHERE id = $1`, runID).Scan(&reason); err != nil {
		t.Fatalf("read failure_reason: %v", err)
	}
	if !reason.Valid || reason.String != "the worker's data volume stayed full across 4 parks" {
		t.Fatalf("failure_reason = %q, want the count-naming reason", reason.String)
	}
	var judges int
	if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM runs WHERE kind = 'judge' AND target_run_id = $1`, runID).Scan(&judges); err != nil {
		t.Fatalf("count judge runs: %v", err)
	}
	if judges != 0 {
		t.Fatalf("judge runs targeting the disk-failed run = %d, want 0", judges)
	}
}

// TestDiskParkPreventiveNeverFailsLiveDB is the preventive-park guarantee: park and resume the
// same run UZI_RUN_DISK_PARK_MAX+2 times with disk_park_preventive=true — disk_park_count stays 0
// and the run never fails — and then ONE counted park bumps the counter to 1 (still a park, not a
// fail, since 1 is within the cap).
func TestDiskParkPreventiveNeverFailsLiveDB(t *testing.T) {
	const maxParks = 3
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, maxParks)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	for i := 1; i <= maxParks+2; i++ {
		run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, boolPtr(true)))
		if err != nil || !applied || run.Status != "recovery_wait" {
			t.Fatalf("preventive park %d: applied=%v status=%q err=%v, want applied recovery_wait", i, applied, run.Status, err)
		}
		status, cause, disk, _, origin := e.dpRun(t, runID)
		if status != "recovery_wait" || !cause.Valid || cause.String != "data_volume_full" || disk != 0 || origin.Valid {
			t.Fatalf("after preventive park %d: status=%q cause=%v disk_park_count=%d fail_origin=%v, want recovery_wait/data_volume_full/0/NULL",
				i, status, cause, disk, origin)
		}
		e.dpResume(t, runID, w)
	}

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, boolPtr(false)))
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("counted park after preventive ones: applied=%v status=%q err=%v, want applied recovery_wait", applied, run.Status, err)
	}
	if status, _, disk, _, _ := e.dpRun(t, runID); status != "recovery_wait" || disk != 1 {
		t.Fatalf("after the counted park: status=%q disk_park_count=%d, want recovery_wait/1", status, disk)
	}
}

// TestDiskParkCapZeroDisablesLiveDB: UZI_RUN_DISK_PARK_MAX=0 is unlimited — a run that has
// already taken many counted disk parks parks again rather than failing.
func TestDiskParkCapZeroDisablesLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	svc := e.diskParkService(t, 0)

	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)
	e.exec(t, `UPDATE runs SET disk_park_count = 99 WHERE id = $1`, runID)

	run, applied, err := svc.SetState(e.ctx, store.Worker{ID: w}, runID, dataVolumeFullReport(1, nil))
	if err != nil || !applied || run.Status != "recovery_wait" {
		t.Fatalf("cap-0 park: applied=%v status=%q err=%v, want applied recovery_wait", applied, run.Status, err)
	}
	if _, _, disk, _, _ := e.dpRun(t, runID); disk != 100 {
		t.Fatalf("disk_park_count = %d, want 100 (incremented, not capped)", disk)
	}
}

// TestDataVolumeFullConstraintsLiveDB: the widened CHECKs accept the new cause and fail_origin,
// still reject an off-vocabulary value, and the new columns carry their documented defaults
// (disk_park_count 0, checkpoint_contains_latest NULL).
func TestDataVolumeFullConstraintsLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	w := e.seedWorker(t, nil)
	runID := e.fpSeedRunning(t, w, 1)

	var disk int32
	var latest pgtype.Bool
	if err := e.pool.QueryRow(e.ctx, `SELECT disk_park_count, checkpoint_contains_latest FROM runs WHERE id = $1`, runID).
		Scan(&disk, &latest); err != nil {
		t.Fatalf("read new columns: %v", err)
	}
	if disk != 0 || latest.Valid {
		t.Fatalf("defaults: disk_park_count=%d checkpoint_contains_latest=%v, want 0/NULL", disk, latest)
	}

	e.exec(t, `UPDATE runs SET recovery_wait_cause = 'data_volume_full' WHERE id = $1`, runID)
	e.exec(t, `UPDATE runs SET fail_origin = 'data_volume_full' WHERE id = $1`, runID)
	e.exec(t, `UPDATE runs SET checkpoint_contains_latest = false WHERE id = $1`, runID)

	for _, stmt := range []string{
		`UPDATE runs SET recovery_wait_cause = 'disk_full' WHERE id = $1`,
		`UPDATE runs SET fail_origin = 'disk_full' WHERE id = $1`,
	} {
		_, err := e.pool.Exec(e.ctx, stmt, runID)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Fatalf("%s: err = %v, want a 23514 check violation", stmt, err)
		}
	}
}

// TestDataVolumeFullMigrationRoundTripLiveDB drives 00259 Down on an ISOLATED database: rows
// carrying the new cause and fail_origin are NULLed (not deleted), other values are untouched,
// the restored CHECKs reject the new values again, and the new columns are gone. Mirrors
// TestRecoveryWaitVaultLockedMigrationRoundTripLiveDB (00257); the version numbers are drafts
// renumbered with the migration at landing.
func TestDataVolumeFullMigrationRoundTripLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
	}
	e := setupInterlockLiveDB(t) // migrates the shared DB; only its ctx is used below
	ctx := e.ctx

	name := "disk_full_mig_" + strings.ReplaceAll(uuid.NewString(), "-", "")
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

	if err := store.MigrateTo(ctx, isoDSN, 259); err != nil {
		t.Fatalf("MigrateTo(259): %v", err)
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
		{`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, []any{userID, fmt.Sprintf("dvf-%s@e2e", userID)}},
		{`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		  VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, 'x')`, []any{connID, userID}},
		{`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		  VALUES ($1, $2, 1, 'g/dvf', 'https://forge.e2e/g/dvf', 'main', true)`, []any{repoID, connID}},
	} {
		if err := exec(stmt.sql, stmt.args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seedRun := func(iid int, status, cause, origin string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if err := exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status,
		                                  recovery_wait_cause, fail_origin, disk_park_count, checkpoint_contains_latest)
		                VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, NULLIF($6, ''), NULLIF($7, ''), 2, true)`,
			id, userID, repoID, iid, status, cause, origin); err != nil {
			t.Fatalf("seed run (cause %q, origin %q) at 00259: %v", cause, origin, err)
		}
		return id
	}
	parked := seedRun(1, "recovery_wait", "data_volume_full", "")
	failed := seedRun(2, "failed", "", "data_volume_full")
	forge := seedRun(3, "recovery_wait", "forge_unreachable", "")

	if err := store.MigrateDownTo(ctx, isoDSN, 258); err != nil {
		t.Fatalf("MigrateDownTo(258): %v", err)
	}
	read := func(id uuid.UUID) (cause, origin pgtype.Text) {
		t.Helper()
		if err := pool.QueryRow(ctx, `SELECT recovery_wait_cause, fail_origin FROM runs WHERE id = $1`, id).Scan(&cause, &origin); err != nil {
			t.Fatalf("read run after Down: %v", err)
		}
		return cause, origin
	}
	if c, _ := read(parked); c.Valid {
		t.Fatalf("after Down: data_volume_full park cause = %q, want NULL", c.String)
	}
	if _, o := read(failed); o.Valid {
		t.Fatalf("after Down: data_volume_full fail_origin = %q, want NULL", o.String)
	}
	if c, _ := read(forge); !c.Valid || c.String != "forge_unreachable" {
		t.Fatalf("after Down: forge run cause = %v, want forge_unreachable untouched", c)
	}
	if err := exec(`UPDATE runs SET recovery_wait_cause = 'data_volume_full' WHERE id = $1`, parked); err == nil {
		t.Fatal("after Down the restored recovery_wait_cause CHECK still admits data_volume_full")
	}
	if err := exec(`UPDATE runs SET fail_origin = 'data_volume_full' WHERE id = $1`, failed); err == nil {
		t.Fatal("after Down the restored fail_origin CHECK still admits data_volume_full")
	}
	var cols int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns
	    WHERE table_name = 'runs' AND column_name IN ('disk_park_count', 'checkpoint_contains_latest')`).Scan(&cols); err != nil {
		t.Fatalf("read columns: %v", err)
	}
	if cols != 0 {
		t.Fatalf("after Down %d of the new columns remain, want 0", cols)
	}
}
