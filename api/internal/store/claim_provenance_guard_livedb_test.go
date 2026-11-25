package store_test

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4476: the claim provenance columns, the guard trigger and the mirrors, against a REAL
// Postgres (skipped unless UZI_TEST_DATABASE_URL points at a throwaway one). Candidate-only: this
// file names the new columns, the guard trigger and the projection fields. The entry-point
// regressions that must be red before the change live in claim_provenance_entry_livedb_test.go.
//
// Serial: the stale sweep is global and the fixture helpers are shared; never add t.Parallel here.

// The pre-#4476 ClaimRun (commit 908b978ea): raw SQL and argument order, never regenerated.
//
//go:embed testdata/claim-provenance-legacy/*.sql testdata/claim-provenance-legacy/*.json
var claimProvenanceLegacyFixtures embed.FS

var guardCodexCaps = []string{capability.CodexHarnessV1, capability.CodexRuntimeV2, capability.CodexRuntimeV3, capability.CodexCustomModelV1}

// guardClaim runs ClaimRun as w with the given protocol capabilities, grace disabled and a 30m
// ceiling (the same modeled cutoffs as the stale-requeue tests).
func guardClaim(fx *rollAffinityFixture, w uuid.UUID, caps []string) (store.Run, error) {
	return fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:           pgtype.UUID{Bytes: w, Valid: true},
		UserID:             fx.userID,
		AffinityCutoff:     pgtype.Timestamptz{Time: time.Now().Add(-staleCeiling), Valid: true},
		SpreadCutoff:       pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:    pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		WorkerProtocolCaps: caps,
	})
}

func guardProv(fx *rollAffinityFixture, run uuid.UUID) (*uuid.UUID, *int64) {
	fx.t.Helper()
	var w *uuid.UUID
	var g *int64
	if err := fx.pool.QueryRow(fx.ctx,
		`SELECT claim_provenance_worker_id, claim_provenance_generation FROM runs WHERE id = $1`, run).Scan(&w, &g); err != nil {
		fx.t.Fatalf("read provenance: %v", err)
	}
	return w, g
}

func guardWantProv(fx *rollAffinityFixture, run, worker uuid.UUID, gen int64, why string) {
	fx.t.Helper()
	w, g := guardProv(fx, run)
	if w == nil || *w != worker || g == nil || *g != gen {
		fx.t.Fatalf("%s: provenance=(%v,%v), want (%s,%d)", why, w, g, worker, gen)
	}
}

func guardWantNoProv(fx *rollAffinityFixture, run uuid.UUID, why string) {
	fx.t.Helper()
	if w, g := guardProv(fx, run); w != nil || g != nil {
		fx.t.Fatalf("%s: provenance=(%v,%v), want NULL/NULL", why, w, g)
	}
}

func guardRowJSON(fx *rollAffinityFixture, run uuid.UUID) string {
	fx.t.Helper()
	var s string
	if err := fx.pool.QueryRow(fx.ctx, `SELECT to_jsonb(r)::text FROM runs r WHERE id = $1`, run).Scan(&s); err != nil {
		fx.t.Fatalf("snapshot run: %v", err)
	}
	return s
}

// guardLegacyRun inserts a Codex run that already carries a claim generation but NO provenance
// (what every row claimed before the migration looks like). Provenance is unchanged on INSERT, so
// the guard does not fire.
func guardLegacyRun(fx *rollAffinityFixture, status string, worker *uuid.UUID, gen int64) uuid.UUID {
	fx.t.Helper()
	id := uuid.New()
	mustExec(fx.ctx, fx.t, fx.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness, worker_id, claim_generation)
		 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'codex', $6, $7)`,
		id, fx.userID, fx.repoID, fx.nextIID(), status, worker, gen)
	return id
}

func guardCodexWorkers(fx *rollAffinityFixture, ws ...uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = ANY($1)`, ws, guardCodexCaps)
}

// guardHealth reads the run's health projection through the real ListActiveRunsForHealth.
func guardHealth(fx *rollAffinityFixture, run uuid.UUID) store.ListActiveRunsForHealthRow {
	fx.t.Helper()
	rows, err := fx.q.ListActiveRunsForHealth(fx.ctx, []string{})
	if err != nil {
		fx.t.Fatalf("ListActiveRunsForHealth: %v", err)
	}
	for _, r := range rows {
		if r.ID == run {
			return r
		}
	}
	fx.t.Fatalf("run %s is not in the active health projection", run)
	return store.ListActiveRunsForHealthRow{}
}

func guardWantHealth(fx *rollAffinityFixture, run uuid.UUID, held bool, name, why string) {
	fx.t.Helper()
	h := guardHealth(fx, run)
	if h.CodexProvenanceHeld != held || h.CodexProvenanceWorkerName != name {
		fx.t.Fatalf("%s: health held=%v worker=%q, want held=%v worker=%q", why, h.CodexProvenanceHeld, h.CodexProvenanceWorkerName, held, name)
	}
}

// guardStaleSweep makes w heartbeat-stale and runs the real global requeue sweep, asserting it
// requeued run.
func guardStaleSweep(fx *rollAffinityFixture, w, run uuid.UUID) {
	fx.t.Helper()
	mustExec(fx.ctx, fx.t, fx.pool, `UPDATE workers SET last_heartbeat_at = now() - interval '2 minutes' WHERE id = $1`, w)
	rows, err := fx.q.RequeueRunsOfStaleWorkers(fx.ctx, store.RequeueRunsOfStaleWorkersParams{
		MaxRequeues: 3, Cutoff: pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
	})
	if err != nil {
		fx.t.Fatalf("RequeueRunsOfStaleWorkers: %v", err)
	}
	for _, r := range rows {
		if r.ID == run {
			return
		}
	}
	fx.t.Fatalf("the stale sweep did not requeue run %s", run)
}

// guardRejects runs a write that the guard must refuse: it errors with the trigger's own
// check_violation and leaves the row byte-identical.
func guardRejects(fx *rollAffinityFixture, run uuid.UUID, why, sql string, args ...any) {
	fx.t.Helper()
	before := guardRowJSON(fx, run)
	if _, err := fx.pool.Exec(fx.ctx, sql, args...); !isProvenanceGuardErr(err) {
		fx.t.Fatalf("%s: got %v, want the guard trigger's check_violation (23514)", why, err)
	}
	if after := guardRowJSON(fx, run); after != before {
		fx.t.Fatalf("%s: the refused write changed the row", why)
	}
}

func guardAccepts(fx *rollAffinityFixture, why, sql string, args ...any) {
	fx.t.Helper()
	if _, err := fx.pool.Exec(fx.ctx, sql, args...); err != nil {
		fx.t.Fatalf("%s: %v", why, err)
	}
}

// Legacy NULL provenance: a Codex run with claim_generation >= 1 and no recorded claimant is held
// for EVERY worker, the previous owner included, wherever it re-enters the queue; the health
// projection reports it held with no worker name.
func TestCodexProvenanceLegacyNullHeldLiveDB(t *testing.T) {
	setup := func(t *testing.T) (*rollAffinityFixture, uuid.UUID, uuid.UUID) {
		fx := newRollAffinityFixture(t)
		a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
		guardCodexWorkers(fx, a, b)
		return fx, a, b
	}
	heldForAll := func(fx *rollAffinityFixture, a, b, run uuid.UUID, why string) {
		fx.t.Helper()
		fx.mustBlock(a, 0, staleCeiling, why+": previous owner A")
		fx.mustBlock(b, 0, staleCeiling, why+": peer B")
		if n := fx.strictEligibleCount(run, 0, staleCeiling); n != 0 {
			fx.t.Fatalf("%s: strict_eligible = %d, want 0 (the health count mirrors the claim)", why, n)
		}
		guardWantHealth(fx, run, true, "", why)
		guardWantNoProv(fx, run, why)
	}

	t.Run("queued", func(t *testing.T) {
		fx, a, b := setup(t)
		run := guardLegacyRun(fx, "queued", &a, 2)
		heldForAll(fx, a, b, run, "queued legacy row")
	})
	t.Run("running_requeued_by_the_stale_sweep", func(t *testing.T) {
		fx, a, b := setup(t)
		run := guardLegacyRun(fx, "running", &a, 1)
		guardWantHealth(fx, run, true, "", "running legacy row")
		guardStaleSweep(fx, a, run)
		heldForAll(fx, a, b, run, "requeued legacy row")
	})
	t.Run("paused_then_resumed", func(t *testing.T) {
		fx, a, b := setup(t)
		run := guardLegacyRun(fx, "paused", &a, 1)
		if _, err := fx.q.ResumePausedRun(fx.ctx, store.ResumePausedRunParams{ID: run, UserID: fx.userID, GlobalTimeoutSeconds: 3600}); err != nil {
			t.Fatalf("ResumePausedRun: %v", err)
		}
		heldForAll(fx, a, b, run, "resumed legacy row")
	})
}

// The guard trigger refuses every write that is not an admitted +1 claim stamping exactly
// (NEW.worker_id, NEW.claim_generation), with SQLSTATE 23514 and its own message, and the row
// stays byte-identical.
func TestCodexProvenanceGuardRejectsLiveDB(t *testing.T) {

	t.Run("generation_rewind_cannot_re_stamp_an_existing_pair", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
		run := entrySeedCodex(fx, a)
		// The provenance trigger deliberately does not inspect unrelated writes.
		// Rewinding the generation must not turn an existing pair into a first claim.
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET claim_generation = 0 WHERE id = $1`, run)
		guardRejects(fx, run, "generation rewind followed by a foreign first-claim stamp",
			`UPDATE runs SET claim_generation = 1, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`, run, b)
	})

	t.Run("foreign_claimant_over_a_recorded_one", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
		run := entrySeedCodex(fx, a) // (A,1)
		guardRejects(fx, run, "foreign (B,2) over (A,1)",
			`UPDATE runs SET claim_generation = 2, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 2 WHERE id = $1`, run, b)
	})
	t.Run("legacy_backfill_by_a_plus_one_claim", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := guardLegacyRun(fx, "queued", &a, 1)
		guardRejects(fx, run, "+1 backfill of a legacy row",
			`UPDATE runs SET claim_generation = 2, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 2 WHERE id = $1`, run, a)
	})
	t.Run("legacy_backfill_by_a_provenance_only_write", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := guardLegacyRun(fx, "queued", &a, 1)
		guardRejects(fx, run, "provenance-only backfill of a legacy row",
			`UPDATE runs SET claim_provenance_worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`, run, a)
	})
	t.Run("plus_two_jump", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := entrySeedCodex(fx, a)
		guardRejects(fx, run, "+2 jump with an otherwise valid pair",
			`UPDATE runs SET claim_generation = 3, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 3 WHERE id = $1`, run, a)
	})
	t.Run("valid_claim_shape_but_provenance_generation_is_not_the_new_generation", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := entrySeedCodex(fx, a)
		guardRejects(fx, run, "recorded worker's +1 claim stamping generation 99",
			`UPDATE runs SET claim_generation = 2, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 99 WHERE id = $1`, run, a)
	})
	t.Run("valid_claim_shape_but_provenance_worker_is_not_the_claimant", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
		run := entrySeedCodex(fx, a)
		guardRejects(fx, run, "worker_id A with provenance worker B at g+1",
			`UPDATE runs SET claim_generation = 2, worker_id = $2, claim_provenance_worker_id = $3, claim_provenance_generation = 2 WHERE id = $1`, run, a, b)
	})
	t.Run("half_pair_worker_without_generation", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness, claim_generation)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'codex', 0)`, run, fx.userID, fx.repoID, fx.nextIID())
		guardRejects(fx, run, "worker set, generation NULL",
			`UPDATE runs SET claim_generation = 1, worker_id = $2, claim_provenance_worker_id = $2 WHERE id = $1`, run, a)
	})
	t.Run("half_pair_generation_without_worker", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness, claim_generation)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'codex', 0)`, run, fx.userID, fx.repoID, fx.nextIID())
		guardRejects(fx, run, "generation set, worker NULL",
			`UPDATE runs SET claim_generation = 1, worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`, run, a)
	})
	t.Run("provenance_change_with_unchanged_generation", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
		run := entrySeedCodex(fx, a)
		guardRejects(fx, run, "worker rewritten at the same generation",
			`UPDATE runs SET claim_provenance_worker_id = $2 WHERE id = $1`, run, b)
		guardRejects(fx, run, "provenance cleared at the same generation",
			`UPDATE runs SET claim_provenance_worker_id = NULL, claim_provenance_generation = NULL WHERE id = $1`, run)
	})
	t.Run("write_over_a_half_pair_old_row", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		// A half-pair OLD row cannot arise through the guard; build it with the trigger disabled
		// inside a transaction that is always rolled back (DDL is transactional), re-enabled
		// before the write under test.
		run := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness, claim_generation)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'codex', 0)`, run, fx.userID, fx.repoID, fx.nextIID())
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		for _, s := range []string{
			`SET LOCAL lock_timeout = '5s'`,
			`ALTER TABLE runs DISABLE TRIGGER trg_runs_claim_provenance_guard`,
		} {
			if _, err := tx.Exec(fx.ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		if _, err := tx.Exec(fx.ctx, `UPDATE runs SET claim_provenance_worker_id = $2 WHERE id = $1`, run, a); err != nil {
			t.Fatalf("seed the half pair: %v", err)
		}
		if _, err := tx.Exec(fx.ctx, `ALTER TABLE runs ENABLE TRIGGER trg_runs_claim_provenance_guard`); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		// An UPDATE that leaves provenance untouched is not inspected, even over the half pair.
		if _, err := tx.Exec(fx.ctx, `UPDATE runs SET issue_title = 'renamed' WHERE id = $1`, run); err != nil {
			t.Fatalf("an unrelated write over a half-pair row must pass: %v", err)
		}
		// A claim-shaped 0 -> 1 stamp is refused because the OLD pair is half.
		_, err = tx.Exec(fx.ctx,
			`UPDATE runs SET claim_generation = 1, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`, run, a)
		if !isProvenanceGuardErr(err) {
			t.Fatalf("stamp over a half-pair OLD row: got %v, want the guard trigger's check_violation", err)
		}
	})
	t.Run("trigger_state_survives_the_rolled_back_transaction", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		var enabled string
		if err := fx.pool.QueryRow(fx.ctx,
			`SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'trg_runs_claim_provenance_guard' AND tgrelid = 'runs'::regclass`).Scan(&enabled); err != nil || enabled != "O" {
			t.Fatalf("guard trigger tgenabled=%q (err %v), want O", enabled, err)
		}
	})
	t.Run("insert_carrying_provenance", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		for _, tc := range []struct{ name, cols, vals string }{
			{"worker only", `claim_provenance_worker_id`, `$5`},
			{"generation only", `claim_provenance_generation`, `1`},
			{"both", `claim_provenance_worker_id, claim_provenance_generation`, `$5, 1`},
		} {
			id := uuid.New()
			args := []any{id, fx.userID, fx.repoID, fx.nextIID()}
			if strings.Contains(tc.vals, "$5") {
				args = append(args, a)
			}
			_, err := fx.pool.Exec(fx.ctx,
				`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness, `+tc.cols+`)
				 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'codex', `+tc.vals+`)`, args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" || !strings.HasPrefix(pgErr.Message, "claim provenance cannot be set on insert") {
				t.Fatalf("INSERT with %s: got %v, want check_violation 'claim provenance cannot be set on insert'", tc.name, err)
			}
			var n int
			if err := fx.pool.QueryRow(fx.ctx, `SELECT count(*) FROM runs WHERE id = $1`, id).Scan(&n); err != nil || n != 0 {
				t.Fatalf("INSERT with %s left %d rows (err %v), want none", tc.name, n, err)
			}
		}
	})
}

// What the guard admits: the 0 -> 1 stamp, the recorded worker's g -> g+1 stamp, and any write that
// leaves provenance untouched.
func TestCodexProvenanceGuardAcceptsLiveDB(t *testing.T) {

	t.Run("stamp_zero_to_one", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, harness, claim_generation)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'codex', 0)`, run, fx.userID, fx.repoID, fx.nextIID())
		guardAccepts(fx, "0 -> 1 stamp",
			`UPDATE runs SET claim_generation = 1, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`, run, a)
		guardWantProv(fx, run, a, 1, "after the 0 -> 1 stamp")
	})
	t.Run("recorded_worker_stamps_the_next_generation", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := entrySeedCodex(fx, a) // (A,1)
		guardAccepts(fx, "g -> g+1 stamp by the recorded worker",
			`UPDATE runs SET claim_generation = 2, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 2 WHERE id = $1`, run, a)
		guardWantProv(fx, run, a, 2, "after the recorded worker's g+1 stamp")
	})
	t.Run("untouched_provenance_is_not_inspected", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		legacy := guardLegacyRun(fx, "queued", &a, 3)
		guardAccepts(fx, "status change on a legacy row", `UPDATE runs SET status = 'paused', status_since = now() WHERE id = $1`, legacy)
		guardAccepts(fx, "unrelated column on a legacy row", `UPDATE runs SET issue_title = 'renamed', claim_generation = 7 WHERE id = $1`, legacy)
		guardWantNoProv(fx, legacy, "legacy row after unrelated writes")
	})
}

// Rollback re-upgrade: after an old-api claim bumps the generation without writing provenance, the
// recorded pair is stale. The guard does not fire for the old claim; afterwards no worker is admitted,
// the guard refuses to re-stamp, and health reports the run held with no worker name.
//
// The old claim is the real pre-change ClaimRun: the generated claimRun SQL of commit 908b978ea,
// frozen under testdata/claim-provenance-legacy and executed with that commit's argument order. It
// is a real UPDATE of worker_id and claim_generation + 1 that knows nothing of provenance.
func TestCodexProvenanceRollbackReUpgradeLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
	guardCodexWorkers(fx, a, b)
	run := entrySeedCodex(fx, a) // the new ClaimRun records (A,1)
	guardWantProv(fx, run, a, 1, "new ClaimRun")
	// Requeued past the ceiling so the old claim's pin lets B take it.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, run)
	fx.age(run, 40*time.Minute, 40*time.Minute)

	sqlText, err := claimProvenanceLegacyFixtures.ReadFile("testdata/claim-provenance-legacy/legacy-ClaimRun.sql")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := claimProvenanceLegacyFixtures.ReadFile("testdata/claim-provenance-legacy/legacy-ClaimRun-bindings.json")
	if err != nil {
		t.Fatal(err)
	}
	var bindings struct {
		SourceSHA string   `json:"source_sha"`
		Bindings  []string `json:"bindings"`
	}
	if err := json.Unmarshal(meta, &bindings); err != nil || bindings.SourceSHA != "908b978ea" || len(bindings.Bindings) == 0 {
		t.Fatalf("legacy fixture metadata %+v: %v", bindings, err)
	}
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	params := reflect.ValueOf(store.ClaimRunParams{
		WorkerID:                 pgtype.UUID{Bytes: b, Valid: true},
		UserID:                   fx.userID,
		AffinityCutoff:           pgtype.Timestamptz{Time: time.Now().Add(-staleCeiling), Valid: true},
		SpreadCutoff:             pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:          pgtype.Timestamptz{Time: time.Now().Add(-45 * time.Second), Valid: true},
		BackgroundGraceCutoff:    now,
		CrossCheckEvaluatedAt:    now,
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: time.Now().Add(-2 * time.Minute), Valid: true},
		WorkerCaps:               []string{},
		WorkerProtocolCaps:       guardCodexCaps,
		DockerRepoAllowlist:      []uuid.UUID{},
		CodexCuratedModels:       []string{},
	})
	args := make([]any, len(bindings.Bindings))
	for i, f := range bindings.Bindings {
		fv := params.FieldByName(f)
		if !fv.IsValid() {
			t.Fatalf("legacy binding %d %q missing from ClaimRunParams", i+1, f)
		}
		args[i] = fv.Interface()
	}
	tag, err := fx.pool.Exec(fx.ctx, string(sqlText), args...)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("old-api ClaimRun as B: rows=%d err=%v (the guard must not fire: provenance is unchanged)", tag.RowsAffected(), err)
	}
	var gen int64
	var worker uuid.UUID
	if err := fx.pool.QueryRow(fx.ctx, `SELECT claim_generation, worker_id FROM runs WHERE id = $1`, run).Scan(&gen, &worker); err != nil || gen != 2 || worker != b {
		t.Fatalf("after the old-api claim: generation=%d worker=%s (err %v), want 2 / B", gen, worker, err)
	}
	guardWantProv(fx, run, a, 1, "the old claim left provenance alone")

	// Back on the new api: the run is requeued, and neither the recorded worker (stale generation)
	// nor the old claimant is admitted.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, run)
	fx.age(run, 40*time.Minute, 40*time.Minute)
	fx.mustBlock(a, 0, staleCeiling, "recorded worker A with a stale generation")
	fx.mustBlock(b, 0, staleCeiling, "the old-api claimant B")
	guardRejects(fx, run, "re-stamp for A over a stale pair",
		`UPDATE runs SET claim_generation = 3, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 3 WHERE id = $1`, run, a)
	guardRejects(fx, run, "re-stamp for B over a stale pair",
		`UPDATE runs SET claim_generation = 3, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 3 WHERE id = $1`, run, b)
	guardWantHealth(fx, run, true, "", "stale provenance generation")
}

// Positive controls: provenance behaves where it must not interfere.
func TestCodexProvenancePositiveControlsLiveDB(t *testing.T) {
	t.Run("generation_zero_claim_records_the_claimant", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := entrySeedCodex(fx, a)
		guardWantProv(fx, run, a, 1, "first Codex claim")
	})

	t.Run("readoption_leaves_provenance_unchanged", func(t *testing.T) {
		for _, frozen := range []bool{false, true} {
			name := "direct"
			if frozen {
				name = "frozen"
			}
			t.Run(name, func(t *testing.T) {
				fx := newRollAffinityFixture(t)
				a := fx.worker("A", 0, false)
				run := entrySeedCodex(fx, a)
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, run)
				mustExec(fx.ctx, t, fx.pool,
					`INSERT INTO worker_active_runs (worker_id, run_id, claim_generation, phase, terminal_pending, snapshot_epoch, reported_at)
					 VALUES ($1, $2, 1, 'running', false, 1, now())`, a, run)
				tx, err := fx.pool.Begin(fx.ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackWorkerRecoveryTx(t, tx)
				var ids []uuid.UUID
				if frozen {
					targets, parents := freezeWorkerSnapshot(fx.ctx, t, tx, a, []uuid.UUID{run})
					rows, err := store.New(tx).FrozenReadoptRunsFromSnapshot(fx.ctx, store.FrozenReadoptRunsFromSnapshotParams{
						WorkerID: a, FrozenTargets: targets, LockedParentIds: parents,
					})
					if err != nil {
						t.Fatalf("FrozenReadoptRunsFromSnapshot: %v", err)
					}
					for _, r := range rows {
						ids = append(ids, r.ID)
					}
				} else {
					rows, err := store.New(tx).ReadoptRunsFromSnapshot(fx.ctx, a)
					if err != nil {
						t.Fatalf("ReadoptRunsFromSnapshot: %v", err)
					}
					for _, r := range rows {
						ids = append(ids, r.ID)
					}
				}
				if len(ids) != 1 || ids[0] != run {
					t.Fatalf("readopted %v, want exactly the run", ids)
				}
				var status string
				var gen, pgen int64
				var pw uuid.UUID
				if err := tx.QueryRow(fx.ctx,
					`SELECT status, claim_generation, claim_provenance_generation, claim_provenance_worker_id FROM runs WHERE id = $1`, run).Scan(&status, &gen, &pgen, &pw); err != nil {
					t.Fatal(err)
				}
				if status != "running" || gen != 1 || pgen != 1 || pw != a {
					t.Fatalf("after readoption: status=%s generation=%d provenance=(%s,%d), want running / 1 / (A,1)", status, gen, pw, pgen)
				}
			})
		}
	})

	t.Run("recorded_claimant_reclaim_under_a_new_nonce_records_the_next_generation", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := entrySeedCodex(fx, a)
		entryReleased(fx, run, a)
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued', worker_id = NULL WHERE id = $1`, run)
		// A re-registers: its snapshot nonce rotates away from the released incarnation's.
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET snapshot_register_nonce = 'new-incarnation' WHERE id = $1`, a)
		fx.mustClaim(a, run, 0, staleCeiling, "A under a new nonce")
		guardWantProv(fx, run, a, 2, "after A's fresh claim")
	})

	t.Run("deleting_a_worker_whose_runs_are_all_terminal_is_not_blocked", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a := fx.worker("A", 0, false)
		run := entrySeedCodex(fx, a)
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'completed', finished_at = now() WHERE id = $1`, run)
		n, err := fx.q.DeleteWorkerForUser(fx.ctx, store.DeleteWorkerForUserParams{ID: a, UserID: fx.userID})
		if err != nil || n != 1 {
			t.Fatalf("DeleteWorkerForUser = (%d, %v), want (1, nil)", n, err)
		}
		guardWantProv(fx, run, a, 1, "after the recorded worker row is deleted")
	})

	t.Run("claude_stale_reclaim_falls_open_with_columns_untouched", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
		run := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued')`, run, fx.userID, fx.repoID, fx.nextIID())
		fx.mustClaim(a, run, 0, staleCeiling, "Claude first claim")
		guardStaleSweep(fx, a, run)
		fx.mustClaim(b, run, 0, staleCeiling, "Claude run falls open to B after the stale sweep")
		guardWantNoProv(fx, run, "Claude run after two claims")
		var gen int64
		if err := fx.pool.QueryRow(fx.ctx, `SELECT claim_generation FROM runs WHERE id = $1`, run).Scan(&gen); err != nil || gen != 2 {
			t.Fatalf("claim_generation = %d (err %v), want 2", gen, err)
		}
	})

	t.Run("publication_resume_custody_without_handoff_still_blocks_the_recorded_claimant", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a, c := fx.worker("A", 0, false), fx.worker("C", 0, false)
		run := entrySeedCodex(fx, a)
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued' WHERE id = $1`, run)
		tx, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(fx.ctx) }()
		for _, stmt := range []struct {
			sql  string
			args []any
		}{
			{`SELECT set_config('uzi.publication_resume_api','v1',true)`, nil},
			{`INSERT INTO run_publication_resume (run_id, enrollment_seq, claim_generation, worker_id, intent, state)
			  VALUES ($1, 1, 1, $2, json_build_object('run_id', $3::text), 'active')`, []any{run, c, run.String()}},
		} {
			if _, err := tx.Exec(fx.ctx, stmt.sql, stmt.args...); err != nil {
				t.Fatalf("enroll: %v", err)
			}
		}
		if err := tx.Commit(fx.ctx); err != nil {
			t.Fatal(err)
		}
		caps := append([]string{capability.PublicationResumeV1}, guardCodexCaps...)
		if _, err := guardClaim(fx, a, caps); err != pgx.ErrNoRows {
			t.Fatalf("recorded claimant A against a foreign custodian: got %v, want pgx.ErrNoRows", err)
		}
		// Control: with the enrollment gone the same claim succeeds, so custody was the only blocker.
		del, err := fx.pool.Begin(fx.ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = del.Rollback(fx.ctx) }()
		if _, err := del.Exec(fx.ctx, `SELECT set_config('uzi.publication_resume_api','v1',true)`); err != nil {
			t.Fatal(err)
		}
		if _, err := del.Exec(fx.ctx, `DELETE FROM run_publication_resume WHERE run_id = $1`, run); err != nil {
			t.Fatalf("remove the enrollment: %v", err)
		}
		if err := del.Commit(fx.ctx); err != nil {
			t.Fatal(err)
		}
		got, err := guardClaim(fx, a, caps)
		if err != nil || got.ID != run {
			t.Fatalf("A without the enrollment: run=%s err=%v, want the run", got.ID, err)
		}
	})

	t.Run("health_counts_only_the_recorded_worker", func(t *testing.T) {
		fx := newRollAffinityFixture(t)
		a, b := fx.worker("A", 0, false), fx.worker("B", 0, false)
		guardCodexWorkers(fx, a, b)
		run := entrySeedCodex(fx, a)
		// A server-parked resume leaves the run unpinned (worker_id NULL): any live worker could
		// claim it but for provenance.
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued', worker_id = NULL WHERE id = $1`, run)
		if n := fx.strictEligibleCount(run, 0, staleCeiling); n != 1 {
			t.Fatalf("strict_eligible = %d, want 1 (only the recorded claimant)", n)
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET status = 'offline' WHERE id = $1`, b)
		if n := fx.strictEligibleCount(run, 0, staleCeiling); n != 1 {
			t.Fatalf("only A online: strict_eligible = %d, want 1", n)
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET status = CASE WHEN id = $1 THEN 'offline' ELSE 'online' END WHERE id IN ($1, $2)`, a, b)
		if n := fx.strictEligibleCount(run, 0, staleCeiling); n != 0 {
			t.Fatalf("only B online: strict_eligible = %d, want 0", n)
		}
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET status = 'online' WHERE id IN ($1, $2)`, a, b)
		guardWantHealth(fx, run, true, "A", "recorded pair at the current generation: held for a fresh claimant, named A")
	})
}

// The health projection names the worker from provenance alone.
func TestCodexProvenanceHealthProjectionLiveDB(t *testing.T) {
	fx := newRollAffinityFixture(t)
	a := fx.worker("recorded-A", 0, false)
	c := fx.worker("custody-C", 0, false)
	run := entrySeedCodex(fx, a)
	// A held run is held for the NULL claimant (a worker that does not exist yet) whatever the pair.
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued', worker_id = $2 WHERE id = $1`, run, c)
	guardWantHealth(fx, run, true, "recorded-A", "worker_id rewritten to C still names A")

	mustExec(fx.ctx, t, fx.pool, `DELETE FROM workers WHERE id = $1`, a)
	guardWantHealth(fx, run, true, a.String(), "recorded worker row deleted names its id")

	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET claim_generation = 2 WHERE id = $1`, run)
	guardWantHealth(fx, run, true, "", "mismatched provenance generation names nobody")
}

// Ephemeral provisioning mirrors: a run held by provenance is never demand for a fresh (NULL
// claimant) ephemeral worker. A Claude control with the same shape is listed, so the exclusion is
// not vacuous.
func TestCodexProvenanceEphemeralMirrorsLiveDB(t *testing.T) {
	iv := leaseInterval(0)

	t.Run("unplaceable_and_saturation_exclude_a_held_run", func(t *testing.T) {
		fx := newFleetFixture(t)
		// These opted-in controls are valid global provisioning demand. Remove
		// this owner before the shared LiveDB sweep reaches later packages.
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			mustExec(ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID)
		})
		optInEphemeral(fx)
		busy := fx.worker("persistent", capOf(1), true)
		fx.holdActive(busy, 1) // saturated
		// Both persistent workers carry the Codex protocol capabilities, so a Codex run is dropped
		// from the lists by provenance and never by capability.
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, busy, guardCodexCaps)
		mk := func(codexHeld bool, caps []string) uuid.UUID {
			id := fx.queuedRun()
			if codexHeld {
				mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET harness = 'codex', claim_generation = 1 WHERE id = $1`, id)
			}
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status_since = now() - interval '1 hour', required_capabilities = $2 WHERE id = $1`, id, caps)
			return id
		}
		gapHeld, gapControl := mk(true, []string{"gh"}), mk(false, []string{"gh"}) // no worker has "gh"
		satHeld, satControl := mk(true, []string{}), mk(false, []string{})         // capable but full
		// Valid recorded provenance (A,1) with worker_id NULL (what a server-parked resume leaves).
		a := fx.worker("recorded", capOf(1), false)
		fx.holdActive(a, 1) // full too, so the saturation trigger still applies
		mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET protocol_capabilities = $2 WHERE id = $1`, a, guardCodexCaps)
		// Non-held Codex controls (generation 0): listed by both triggers.
		gapCodex0, satCodex0 := mk(false, []string{"gh"}), mk(false, []string{})
		mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET harness = 'codex' WHERE id = ANY($1)`, []uuid.UUID{gapCodex0, satCodex0})
		mkRecorded := func(caps []string) uuid.UUID {
			id := mk(false, caps)
			mustExec(fx.ctx, t, fx.pool,
				`UPDATE runs SET harness = 'codex', claim_generation = 1, worker_id = $2, claim_provenance_worker_id = $2, claim_provenance_generation = 1 WHERE id = $1`, id, a)
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET worker_id = NULL WHERE id = $1`, id)
			return id
		}
		gapRec, satRec := mkRecorded([]string{"gh"}), mkRecorded([]string{})
		gap := listedUnplaceable(fx, 1000, iv)
		if !gap[gapCodex0] {
			t.Fatalf("unplaceable did not list the generation-0 Codex control %s: %v", gapCodex0, gap)
		}
		if gap[gapRec] {
			t.Fatalf("unplaceable listed the recorded-provenance run %s with worker_id NULL: %v", gapRec, gap)
		}
		if gap[gapHeld] || !gap[gapControl] {
			t.Fatalf("unplaceable listed %v; want the Claude control %s, not the held run %s", gap, gapControl, gapHeld)
		}
		sat := listedSaturation(fx, 1000, iv)
		if !sat[satCodex0] {
			t.Fatalf("saturation did not list the generation-0 Codex control %s: %v", satCodex0, sat)
		}
		if sat[satRec] {
			t.Fatalf("saturation listed the recorded-provenance run %s with worker_id NULL: %v", satRec, sat)
		}
		if sat[satHeld] || !sat[satControl] {
			t.Fatalf("saturation listed %v; want the Claude control %s, not the held run %s", sat, satControl, satHeld)
		}
	})

	t.Run("isolated_lane_codex_indicating_row_is_refused_by_the_schema", func(t *testing.T) {
		fx := newFleetFixture(t)
		profile := uuid.New()
		mustExec(fx.ctx, t, fx.pool, `INSERT INTO egress_profiles (id, name, hosts) VALUES ($1, $2, '{docs.example.com}')`,
			profile, "prov-"+profile.String()[:20])
		plain, codex := uuid.New(), uuid.New()
		for _, id := range []uuid.UUID{plain, codex} {
			mustExec(fx.ctx, t, fx.pool,
				`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, egress_profile_id)
				 VALUES ($1, $2, $3, 'issue', $4, 't', 'd', 'queued', $5)`, id, fx.userID, fx.repoID, fx.nextIID(), profile)
		}
		t.Cleanup(func() {
			_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id = ANY($1)`, []uuid.UUID{plain, codex})
			_, _ = fx.pool.Exec(fx.ctx, `DELETE FROM egress_profiles WHERE id = $1`, profile)
		})
		// The schema itself refuses a Codex-indicating profile-bound row, so the query's own Codex
		// exclusion is a belt: assert that refusal instead of seeding the unreachable row.
		for _, set := range []string{`harness = 'codex'`, `codex_material_revision = 1`} {
			_, err := fx.pool.Exec(fx.ctx, `UPDATE runs SET `+set+` WHERE id = $1`, codex)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
				t.Fatalf("making a profile-bound run Codex-indicating (%s): got %v, want a check_violation", set, err)
			}
		}
		got := listedIsolated(fx, 1000)
		if !got[plain] || !got[codex] {
			t.Fatalf("isolated listed %v; want both profile-bound Claude runs %s and %s", got, plain, codex)
		}
	})
}

// Hibernation wake demand names only the recorded claimant: with two hibernated workers and the
// OLDER one not recorded, the demand still points at the recorded one; a legacy row is never demand.
func TestCodexProvenanceHibernationWakeDemandLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	t.Cleanup(func() { mustExec(fx.ctx, t, fx.pool, `DELETE FROM users WHERE id = $1`, fx.userID) })
	a := hostedWorker(fx, wakeWorkerOpts{protocol: guardCodexCaps})
	b := hostedWorker(fx, wakeWorkerOpts{protocol: guardCodexCaps})

	run := fx.queuedRun()
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET harness = 'codex' WHERE id = $1`, run)
	now := time.Now()
	claimed, err := fx.q.ClaimRun(fx.ctx, store.ClaimRunParams{
		WorkerID:                 pgtype.UUID{Bytes: a, Valid: true},
		UserID:                   fx.userID,
		AffinityCutoff:           pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true},
		SpreadCutoff:             pgtype.Timestamptz{Time: now.Add(-9 * time.Second), Valid: true},
		HeartbeatCutoff:          pgtype.Timestamptz{Time: now.Add(-45 * time.Second), Valid: true},
		CrossCheckEvaluatedAt:    pgtype.Timestamptz{Time: now, Valid: true},
		CrossCheckAffinityCutoff: pgtype.Timestamptz{Time: now.Add(-time.Hour), Valid: true},
		WorkerProtocolCaps:       guardCodexCaps,
	})
	if err != nil || claimed.ID != run {
		t.Fatalf("A's first Codex claim: run=%s err=%v", claimed.ID, err)
	}
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET status = 'queued', worker_id = NULL WHERE id = $1`, run)
	setHibernated(fx, a, "hibernated")
	setHibernated(fx, b, "hibernated")
	mustExec(fx.ctx, t, fx.pool, `UPDATE workers SET hibernated_at = now() - interval '3 hours' WHERE id = $1`, b) // B is older: it would win without provenance
	if got := wakeDemandFor(fx, nil)[run]; got != a {
		t.Fatalf("wake demand names %s, want the recorded claimant A %s (B %s is older but not admitted)", got, a, b)
	}

	// A legacy row (generation >= 1, NULL provenance) is not wake demand at all.
	legacy := fx.queuedRun()
	mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET harness = 'codex', claim_generation = 1 WHERE id = $1`, legacy)
	if got, ok := wakeDemandFor(fx, nil)[legacy]; ok {
		t.Fatalf("legacy NULL-provenance run is wake demand for %s, want none", got)
	}
}
