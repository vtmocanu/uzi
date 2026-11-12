package store_test

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4162: migration 00427 redefines fn_custody_admission_count / _members to count admission
// UNITS. These tests pin its Down, the operator rollback script (scripts/custody-admission-units-
// rollback.sql, for running the previous api image against an upgraded database), the script's
// version coupling to the migration filename, and the hazard the script exists for: the previous
// image's archived ClaimRun admits a run whose own open holds reach the limit while the grouped
// functions are installed, and refuses it again once the script has run. Each test stands its own
// isolated database so goose state can be manipulated.

//go:embed testdata/custody-units-legacy/*.sql testdata/custody-units-legacy/*.json
var custodyUnitsLegacyFixtures embed.FS

const custodyUnitsRollbackScript = "../../../scripts/custody-admission-units-rollback.sql"

// custodyUnitsMigrationVersion parses the version prefix of the migration whose file name ends
// _custody_admission_units.sql, so a merge-time renumber is followed automatically.
func custodyUnitsMigrationVersion(t *testing.T) int64 {
	t.Helper()
	matches, err := filepath.Glob("migrations/*_custody_admission_units.sql")
	if err != nil || len(matches) != 1 {
		t.Fatalf("custody admission units migration files=%v err=%v", matches, err)
	}
	prefix, _, ok := strings.Cut(filepath.Base(matches[0]), "_")
	v, err := strconv.ParseInt(prefix, 10, 64)
	if !ok || err != nil {
		t.Fatalf("migration prefix %q: %v", prefix, err)
	}
	return v
}

type custodyUnitsDefs struct{ count, members string }

func custodyUnitsFunctionDefs(ctx context.Context, t *testing.T, pool *pgxpool.Pool) custodyUnitsDefs {
	t.Helper()
	var d custodyUnitsDefs
	if err := pool.QueryRow(ctx, `SELECT
 pg_get_functiondef('fn_custody_admission_count(uuid,timestamptz)'::regprocedure),
 pg_get_functiondef('fn_custody_admission_members(uuid,timestamptz)'::regprocedure)`).Scan(&d.count, &d.members); err != nil {
		t.Fatal(err)
	}
	return d
}

func custodyUnitsGooseRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(v) ORDER BY v.id),'[]'::jsonb)::text
 FROM goose_db_version v`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// runCustodyUnitsRollback executes the script verbatim over one dedicated connection with the
// simple protocol (no parameters), the way psql sends it.
func runCustodyUnitsRollback(ctx context.Context, t *testing.T, dsn string) error {
	t.Helper()
	script, err := os.ReadFile(custodyUnitsRollbackScript)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// A refused script leaves its explicit transaction aborted; closing the connection discards it.
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, string(script))
	return err
}

func custodyUnitsPool(ctx context.Context, t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func TestCustodyAdmissionUnitsMigrationDownLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "cau_down_")
	ver := custodyUnitsMigrationVersion(t)
	if err := store.MigrateTo(ctx, dsn, ver-1); err != nil {
		t.Fatal(err)
	}
	pool := custodyUnitsPool(ctx, t, dsn)
	old := custodyUnitsFunctionDefs(ctx, t, pool)
	if err := store.MigrateTo(ctx, dsn, ver); err != nil {
		t.Fatal(err)
	}
	up := custodyUnitsFunctionDefs(ctx, t, pool)
	if up.count == old.count || up.members == old.members {
		t.Fatal("migration Up did not change both function bodies")
	}
	if !strings.Contains(up.members, "same_run_group") || strings.Contains(old.members, "same_run_group") {
		t.Fatal("same_run_group reason is not unique to the Up body")
	}
	if err := store.MigrateDownTo(ctx, dsn, ver-1); err != nil {
		t.Fatal(err)
	}
	if got := custodyUnitsFunctionDefs(ctx, t, pool); got != old {
		t.Fatal("goose Down did not restore the pre-migration function bodies byte for byte")
	}
}

func TestCustodyAdmissionUnitsRollbackScriptLiveDB(t *testing.T) {
	ver := custodyUnitsMigrationVersion(t)

	t.Run("restores_old_bodies_and_reapplies", func(t *testing.T) {
		ctx, dsn := standIsolatedDB(t, "cau_rb_")
		if err := store.MigrateTo(ctx, dsn, ver-1); err != nil {
			t.Fatal(err)
		}
		pool := custodyUnitsPool(ctx, t, dsn)
		old := custodyUnitsFunctionDefs(ctx, t, pool)
		if err := store.MigrateTo(ctx, dsn, ver); err != nil {
			t.Fatal(err)
		}
		up := custodyUnitsFunctionDefs(ctx, t, pool)
		if up == old {
			t.Fatal("fixture: Up bodies equal the old bodies")
		}
		if err := runCustodyUnitsRollback(ctx, t, dsn); err != nil {
			t.Fatalf("rollback script: %v", err)
		}
		if got := custodyUnitsFunctionDefs(ctx, t, pool); got != old {
			t.Fatal("rollback script did not restore the pre-migration function bodies byte for byte")
		}
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id=$1`, ver).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("goose rows for version %d after rollback: %d err=%v", ver, rows, err)
		}
		var head int64
		if err := pool.QueryRow(ctx, `SELECT max(version_id) FROM goose_db_version WHERE is_applied`).Scan(&head); err != nil || head != ver-1 {
			t.Fatalf("head after rollback=%d err=%v want %d", head, err, ver-1)
		}
		// The upgraded image booting again re-applies Up.
		if err := store.Migrate(ctx, dsn); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
		if got := custodyUnitsFunctionDefs(ctx, t, pool); got != up {
			t.Fatal("re-applied migration did not reinstall the Up bodies")
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id=$1 AND is_applied`, ver).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("goose rows for version %d after re-migrate: %d err=%v", ver, rows, err)
		}
	})

	t.Run("refuses_when_not_head", func(t *testing.T) {
		ctx, dsn := standIsolatedDB(t, "cau_nh_")
		if err := store.MigrateTo(ctx, dsn, ver); err != nil {
			t.Fatal(err)
		}
		pool := custodyUnitsPool(ctx, t, dsn)
		if _, err := pool.Exec(ctx, `INSERT INTO goose_db_version(version_id,is_applied) VALUES($1,true)`, ver+1); err != nil {
			t.Fatal(err)
		}
		defsBefore, rowsBefore := custodyUnitsFunctionDefs(ctx, t, pool), custodyUnitsGooseRows(ctx, t, pool)
		err := runCustodyUnitsRollback(ctx, t, dsn)
		if err == nil || !strings.Contains(err.Error(), "not the head") {
			t.Fatalf("rollback with a later version applied: err=%v, want refusal naming the head", err)
		}
		if custodyUnitsFunctionDefs(ctx, t, pool) != defsBefore || custodyUnitsGooseRows(ctx, t, pool) != rowsBefore {
			t.Fatal("refused rollback changed function bodies or goose rows")
		}
	})

	t.Run("refuses_when_not_applied", func(t *testing.T) {
		ctx, dsn := standIsolatedDB(t, "cau_na_")
		if err := store.MigrateTo(ctx, dsn, ver-1); err != nil {
			t.Fatal(err)
		}
		pool := custodyUnitsPool(ctx, t, dsn)
		defsBefore, rowsBefore := custodyUnitsFunctionDefs(ctx, t, pool), custodyUnitsGooseRows(ctx, t, pool)
		err := runCustodyUnitsRollback(ctx, t, dsn)
		if err == nil || !strings.Contains(err.Error(), "not applied") {
			t.Fatalf("rollback before the migration: err=%v, want refusal naming not applied", err)
		}
		if custodyUnitsFunctionDefs(ctx, t, pool) != defsBefore || custodyUnitsGooseRows(ctx, t, pool) != rowsBefore {
			t.Fatal("refused rollback changed function bodies or goose rows")
		}
	})
}

// A merge-time renumber of the migration must renumber the script too.
func TestCustodyAdmissionUnitsRollbackScriptVersionLiveDB(t *testing.T) {
	ver := custodyUnitsMigrationVersion(t)
	script, err := os.ReadFile(custodyUnitsRollbackScript)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?:version_id = |IS DISTINCT FROM |goose version )(\d+)|(\d+) is not the head`)
	found := re.FindAllStringSubmatch(string(script), -1)
	if len(found) < 5 {
		t.Fatalf("script version references=%d, want at least 5 (guard, two messages, IS DISTINCT FROM, DELETE)", len(found))
	}
	for _, m := range found {
		got := m[1] + m[2]
		if got != strconv.FormatInt(ver, 10) {
			t.Fatalf("script references version %s, migration is %d (renumber the script)", got, ver)
		}
	}
}

type custodyUnitsLegacyFixture struct {
	t                        *testing.T
	ctx                      context.Context
	pool                     *pgxpool.Pool
	seed                     *custodyEpisodeSeeder
	owner, repo, worker, run uuid.UUID
	limit                    int32
	cutoff                   pgtype.Timestamptz
	legacySQL                string
	legacyBindings           []string
}

func newCustodyUnitsLegacyFixture(ctx context.Context, t *testing.T, pool *pgxpool.Pool) *custodyUnitsLegacyFixture {
	t.Helper()
	sql, err := custodyUnitsLegacyFixtures.ReadFile("testdata/custody-units-legacy/legacy-ClaimRun.sql")
	if err != nil {
		t.Fatal(err)
	}
	data, err := custodyUnitsLegacyFixtures.ReadFile("testdata/custody-units-legacy/legacy-ClaimRun-bindings.json")
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		SourceSHA string   `json:"source_sha"`
		Bindings  []string `json:"bindings"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.SourceSHA != "e7c7d897b4d0477260fc35fdfc0e2959d6f34e31" || len(meta.Bindings) == 0 {
		t.Fatalf("unexpected legacy fixture metadata %+v", meta)
	}
	s := &custodyEpisodeSeeder{ctx: ctx, t: t, pool: pool}
	f := &custodyUnitsLegacyFixture{t: t, ctx: ctx, pool: pool, seed: s, owner: s.user(),
		repo: uuid.New(), worker: uuid.New(), run: uuid.New(), limit: 2,
		cutoff:    pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
		legacySQL: string(sql), legacyBindings: meta.Bindings}
	conn := uuid.New()
	s.exec(`INSERT INTO forge_connections (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'github','https://forge.e2e','bot',1,$3)`, conn, f.owner, []byte{1})
	s.exec(`INSERT INTO repos (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,1,'g/custody','https://forge.e2e/g/custody','main',true)`, f.repo, conn)
	s.exec(`INSERT INTO workers(id,user_id,name,token_hash,status,last_heartbeat_at)
 VALUES($1,$2,$3,$4,'offline',now())`, f.worker, f.owner, f.worker.String(), f.worker[:])
	// One queued code-publishing run that was claimed twice: its OWN open holds (one per
	// generation, same original worker and repo) equal the limit, so the owner's admission UNIT
	// count (1) stays below it while the per-run bound is reached.
	s.exec(`INSERT INTO runs(id,user_id,repo_id,kind,issue_iid,issue_title,issue_description,status,claim_generation)
 VALUES($1,$2,$3,'issue',1,'custody','test','queued',2)`, f.run, f.owner, f.repo)
	for gen := int64(1); gen <= int64(f.limit); gen++ {
		s.exec(`INSERT INTO recovery_custody_holds(id,user_id,run_id,generation,state,original_worker_id,
 original_worker_identity,live_worker_id,live_run_id,repo_id) VALUES($1,$2,$3,$4,'open',$5,'original',$5,$3,$6)`,
			uuid.New(), f.owner, f.run, gen, f.worker, f.repo)
	}
	t.Cleanup(func() {
		s.exec(`DELETE FROM recovery_captures WHERE user_id=$1`, f.owner)
		s.exec(`DELETE FROM recovery_custody_holds WHERE user_id=$1`, f.owner)
		s.exec(`DELETE FROM users WHERE id=$1`, f.owner)
	})
	return f
}

func (f *custodyUnitsLegacyFixture) params() store.ClaimRunParams {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return store.ClaimRunParams{
		UserID: f.owner, WorkerID: pgtype.UUID{Bytes: f.worker, Valid: true}, HeartbeatCutoff: f.cutoff,
		AffinityCutoff: now, SpreadCutoff: now, BackgroundGraceCutoff: now, CrossCheckEvaluatedAt: now,
		CapabilityAware: true, WorkerCaps: []string{}, WorkerProtocolCaps: []string{"recovery_archive_v1"},
		CustodyHoldLimit: f.limit, RecoveryCapable: true, WorkerIdentity: f.worker.String(),
	}
}

// snapshot is the full persisted state of the run and every custody hold of the owner.
func (f *custodyUnitsLegacyFixture) snapshot() string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT jsonb_build_object(
 'run',(SELECT to_jsonb(r) FROM runs r WHERE r.id=$1),
 'holds',COALESCE((SELECT jsonb_agg(to_jsonb(h) ORDER BY h.generation,h.id) FROM recovery_custody_holds h
                   WHERE h.user_id=$2),'[]'::jsonb))::text`, f.run, f.owner).Scan(&s); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// legacyClaim runs the archived (previous image) ClaimRun SQL with its archived binding order in
// a transaction that is always rolled back, and reports the rows it updated and the run status
// it left inside that transaction.
func (f *custodyUnitsLegacyFixture) legacyClaim() (int64, string) {
	f.t.Helper()
	value := reflect.ValueOf(f.params())
	args := make([]any, len(f.legacyBindings))
	for i, field := range f.legacyBindings {
		v := value.FieldByName(field)
		if !v.IsValid() || !v.CanInterface() {
			f.t.Fatalf("legacy binding %d %q missing from ClaimRunParams", i+1, field)
		}
		args[i] = v.Interface()
	}
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(f.ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			f.t.Error(err)
		}
	}()
	tag, err := tx.Exec(f.ctx, f.legacySQL, args...)
	if err != nil {
		f.t.Fatalf("legacy ClaimRun: %v", err)
	}
	var status string
	if err := tx.QueryRow(f.ctx, `SELECT status FROM runs WHERE id=$1`, f.run).Scan(&status); err != nil {
		f.t.Fatal(err)
	}
	return tag.RowsAffected(), status
}

func (f *custodyUnitsLegacyFixture) counted() (open, counted int64) {
	f.t.Helper()
	row, err := store.New(f.pool).GetCustodyAdmissionForRun(f.ctx, store.GetCustodyAdmissionForRunParams{
		UserID: f.owner, RunID: f.run, HeartbeatCutoff: f.cutoff, CustodyHoldLimit: f.limit})
	if err != nil {
		f.t.Fatal(err)
	}
	return row.OpenHolds, row.AdmissionCountedHolds
}

func TestCustodyAdmissionUnitsLegacyBinaryCompatLiveDB(t *testing.T) {
	ver := custodyUnitsMigrationVersion(t)
	ctx, dsn := standIsolatedDB(t, "cau_legacy_")
	if err := store.MigrateTo(ctx, dsn, ver); err != nil {
		t.Fatal(err)
	}
	pool := custodyUnitsPool(ctx, t, dsn)
	f := newCustodyUnitsLegacyFixture(ctx, t, pool)
	before := f.snapshot()

	// Grouped accounting: two holds of one run on one worker are one admission unit.
	if open, counted := f.counted(); open != int64(f.limit) || counted != 1 {
		t.Fatalf("grouped accounting open=%d counted=%d, want open=%d counted=1", open, counted, f.limit)
	}
	// The current ClaimRun applies the hard per-run bound and refuses.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.New(tx).ClaimRun(ctx, f.params())
	_ = tx.Rollback(ctx)
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("current ClaimRun err=%v, want no rows (hard per-run bound)", err)
	}

	// Hazard: the previous image's ClaimRun has no per-run bound, so grouped accounting admits.
	if rows, status := f.legacyClaim(); rows != 1 || status == "queued" {
		t.Fatalf("legacy ClaimRun on grouped functions rows=%d status=%q, want it admitted", rows, status)
	}
	if f.snapshot() != before {
		t.Fatal("rolled-back legacy claim changed persisted state")
	}

	// The documented rollback restores unit-free accounting, so the previous image refuses again.
	if err := runCustodyUnitsRollback(ctx, t, dsn); err != nil {
		t.Fatalf("rollback script: %v", err)
	}
	if open, counted := f.counted(); open != int64(f.limit) || counted != int64(f.limit) {
		t.Fatalf("restored accounting open=%d counted=%d, want both %d", open, counted, f.limit)
	}
	if f.snapshot() != before {
		t.Fatal("rollback script changed run or custody state")
	}
	if rows, status := f.legacyClaim(); rows != 0 || status != "queued" {
		t.Fatalf("legacy ClaimRun after rollback rows=%d status=%q, want refused and still queued", rows, status)
	}
	if f.snapshot() != before {
		t.Fatal("refused legacy claim changed persisted state")
	}
}

// The grouped-accounting wire samples decode strictly into the production DTOs, their counted
// flags sum to the aggregate, and the run sample carries the shared hold-bound reason verbatim.
func TestCustodyAdmissionUnitsGroupedFixturesLiveDB(t *testing.T) {
	decode := func(name string, into any) {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("../../../fixtures/api-contract", name)) //nolint:gosec // G304: test reads a fixed repo-relative fixture path
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(into); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	var grouped apitypes.RecoveryCustodyHoldsDTO
	decode("recovery_custody_holds.grouped.json", &grouped)
	counted, sameRun := 0, 0
	for _, h := range grouped.Holds {
		if h.CountsTowardAdmission == nil {
			t.Fatalf("hold %s has no counts_toward_admission", h.ID)
		}
		if *h.CountsTowardAdmission {
			counted++
		}
		if h.AdmissionReason == "same_run_group" {
			sameRun++
			if *h.CountsTowardAdmission {
				t.Fatalf("same_run_group hold %s counts toward admission", h.ID)
			}
		}
	}
	if counted != grouped.Aggregate.AdmissionCountedHolds || sameRun == 0 || len(grouped.Holds) != grouped.Aggregate.OpenHolds {
		t.Fatalf("counted=%d same_run_group=%d holds=%d aggregate=%+v", counted, sameRun, len(grouped.Holds), grouped.Aggregate)
	}
	var zero apitypes.RecoveryCustodyHoldsDTO
	decode("recovery_custody_holds.grouped-zero.json", &zero)
	if zero.Holds == nil || len(zero.Holds) != 0 || zero.Aggregate.OpenHolds != 0 || zero.Aggregate.AdmissionCountedHolds != 0 {
		t.Fatalf("grouped-zero sample %+v", zero)
	}
	var run apitypes.RunDTO
	decode("run.custody_run_hold_bound.json", &run)
	if run.Status != "queued" || run.HealthReason == nil || *run.HealthReason != apitypes.ReasonCustodyRunHoldBound {
		t.Fatalf("run sample status=%q health_reason=%v", run.Status, run.HealthReason)
	}
}
