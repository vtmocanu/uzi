package store_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserJudgeRunsOnCheckLiveDB pins users_judge_runs_on_check (PRD #3257): NULL, claude
// and codex are accepted and any other value is rejected with SQLSTATE 23514.
func TestUserJudgeRunsOnCheckLiveDB(t *testing.T) {
	ctx, dsn := standIsolatedDB(t, "user_judge_runs_on_")
	if err := store.Migrate(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	pool, err := store.OpenPool(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	owner := uuid.New()
	mustExec(ctx, t, pool, "INSERT INTO users(id,email,password_hash) VALUES($1,$2,'x')", owner, owner.String()+"@example.com")

	var stored *string
	if err := pool.QueryRow(ctx, `SELECT judge_runs_on FROM users WHERE id=$1`, owner).Scan(&stored); err != nil || stored != nil {
		t.Fatalf("a new user's judge_runs_on = %v (err %v), want NULL", stored, err)
	}
	for _, ok := range []string{"claude", "codex"} {
		mustExec(ctx, t, pool, `UPDATE users SET judge_runs_on=$2 WHERE id=$1`, owner, ok)
	}
	mustExec(ctx, t, pool, `UPDATE users SET judge_runs_on=NULL WHERE id=$1`, owner)

	_, err = pool.Exec(ctx, `UPDATE users SET judge_runs_on=$2 WHERE id=$1`, owner, "other")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "users_judge_runs_on_check" {
		t.Fatalf("judge_runs_on='other' error = %v, want SQLSTATE 23514 on users_judge_runs_on_check", err)
	}
}
