package store_test

// PRD #108 M5 — the two things about auto-stop that only a real Postgres can
// answer: whether migration 00082 actually widened the CHECK (it names the
// auto-generated constraint, and a wrong name would make store.Migrate fail at
// every boot), and whether FailRunAutoStop's SQL does what its comment claims.
//
// Everything else about auto-stop — the whole guard conjunction, the escalation, the
// two halves — is a pure function of in-process state plus one row read, so it lives
// in workersvc's fake-store suite and needs no container. (This line used to say
// "all six guards". Six was the PRE-G5 count, and it survived here for three commits
// after the class gate landed — the very stale tally whose disagreement with two
// other counts is what made the missing gate visible. Name the mechanism, never the
// number: the conjunction lives in workersvc/autostop.go and every member is named
// there.)
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; run via
// e2e/run-store-it.sh. A package that prints `ok` with PASS=0 is INVALID, not green.

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestRunStopKindAutoStoppedLiveDB(t *testing.T) {
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
	defer pool.Close()
	q := store.New(pool)

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("autostop-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	seedRun := func(iid int64, status string) uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, health, health_reason, health_since)
			 VALUES ($1, $2, $3, $4, 't', 'd', $5, 'looping', 'the agent''s updates can''t be saved, so it keeps resending them', now())`,
			id, userID, repoID, iid, status)
		return id
	}

	// ── The migration. The CHECK must now ACCEPT 'auto_stopped'. ──
	rAccept := seedRun(101, "running")
	if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = 'auto_stopped' WHERE id = $1`, rAccept); err != nil {
		t.Fatalf("stop_kind = 'auto_stopped' was REJECTED: %v.\nEither 00082 did not run, or it named a constraint that does not exist — the auto-generated name was measured as runs_stop_kind_check against postgres:17.", err)
	}
	// The positive control that keeps the assertion above from being satisfiable by a
	// constraint that was simply DROPPED and never re-added. Without this, "widened"
	// and "removed" are indistinguishable.
	if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = 'bogus' WHERE id = $1`, rAccept); err == nil {
		t.Error("an out-of-domain stop_kind was ACCEPTED: 00082 dropped the CHECK without re-adding it, so the column now holds anything")
	}
	// And the two human kinds must still pass, or the widening broke the old domain.
	for _, kind := range []string{"cancelled", "plan_rejected"} {
		if _, err := pool.Exec(ctx, `UPDATE runs SET stop_kind = $2 WHERE id = $1`, rAccept, kind); err != nil {
			t.Errorf("stop_kind = %q no longer passes the CHECK after 00082: %v", kind, err)
		}
	}

	// ── FailRunAutoStop on a live run. ──
	rFail := seedRun(102, "running")
	rows, err := q.FailRunAutoStop(ctx, store.FailRunAutoStopParams{
		ID: rFail, FailureReason: pgtype.Text{String: "uzi stopped this run: its updates could not be saved", Valid: true},
	})
	if err != nil {
		t.Fatalf("FailRunAutoStop: %v", err)
	}
	if rows != 1 {
		t.Fatalf("FailRunAutoStop touched %d rows, want 1", rows)
	}
	run, err := q.GetRunByID(ctx, rFail)
	if err != nil {
		t.Fatalf("GetRunByID: %v", err)
	}
	if run.Status != "failed" {
		t.Errorf("status = %q, want failed", run.Status)
	}
	if !run.StopKind.Valid || run.StopKind.String != "auto_stopped" {
		t.Errorf("stop_kind = %v, want auto_stopped — it is the ONLY field that distinguishes an auto-stop on both halves of this stop", run.StopKind)
	}
	if !run.MovePendingSince.Valid {
		t.Error("move_pending_since was not stamped: `failed` restores the origin column, and without the marker the reconcile loop never moves the card back — the run rots in In Progress")
	}
	if !run.FinishedAt.Valid {
		t.Error("finished_at was not stamped")
	}
	// PRD #47 Decision 3, the exit contract: a terminal run carries no health flag.
	// The seed above deliberately staged a `looping` flag with a reason and a since,
	// so all three assertions have something real to clear.
	if run.Health != "ok" || run.HealthReason.Valid || run.HealthSince.Valid {
		t.Errorf("health=%q reason=%v since=%v, want ok/NULL/NULL: a terminal run must not keep a stale ⚠",
			run.Health, run.HealthReason, run.HealthSince)
	}

	// ── The status scope. This is the race between the evaluator's read and its
	//    write, and it is what makes the escalation safe to fire unconditionally. ──
	for _, status := range []string{"completed", "failed", "cancelled"} {
		rTerm := seedRun(200+int64(len(status)), "running")
		mustExec(ctx, t, pool, `UPDATE runs SET status = $2 WHERE id = $1`, rTerm, status)
		rows, err := q.FailRunAutoStop(ctx, store.FailRunAutoStopParams{
			ID: rTerm, FailureReason: pgtype.Text{String: "should not land", Valid: true},
		})
		if err != nil {
			t.Fatalf("FailRunAutoStop on a %s run: %v", status, err)
		}
		if rows != 0 {
			t.Errorf("FailRunAutoStop touched %d rows on an already-%s run, want 0: without the status scope an auto-stop would overwrite a run that had ALREADY finished — including one that completed successfully", rows, status)
		}
		after, _ := q.GetRunByID(ctx, rTerm)
		if after.Status != status {
			t.Errorf("an already-%s run became %q", status, after.Status)
		}
		if after.StopKind.Valid {
			t.Errorf("an already-%s run was stamped stop_kind=%v by a no-op write", status, after.StopKind)
		}
	}
}
