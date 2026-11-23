package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestUserTaskReviewModelLiveDB executes the queries migration 00509 adds columns for
// (GetUserTaskReviewSettings, SetUserTaskReviewModel, SetUserCodexTaskReviewModel, PRD #1570)
// against real Postgres: a pristine NULL, a set value on each lane independently, the same values
// through the wide GetUserSettings read, and clearing back to NULL. A green `sqlc generate` is not
// evidence a query runs.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (./e2e/run-store-it.sh
// provides one and sweeps this package for the LiveDB suffix).
func TestUserTaskReviewModelLiveDB(t *testing.T) {
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

	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user, fmt.Sprintf("task-review-model-%s@e2e", user))

	got, err := q.GetUserTaskReviewSettings(ctx, user)
	if err != nil {
		t.Fatalf("get (pristine row): %v", err)
	}
	if got.TaskReviewModel.Valid || got.CodexTaskReviewModel.Valid {
		t.Fatalf("pristine = %+v, want both NULL (the built-in)", got)
	}

	ret, err := q.SetUserTaskReviewModel(ctx, store.SetUserTaskReviewModelParams{ID: user, TaskReviewModel: pgtype.Text{String: "opus", Valid: true}})
	if err != nil || !ret.Valid || ret.String != "opus" {
		t.Fatalf("set claude = %+v, %v", ret, err)
	}
	got, err = q.GetUserTaskReviewSettings(ctx, user)
	if err != nil || got.TaskReviewModel.String != "opus" || got.CodexTaskReviewModel.Valid {
		t.Fatalf("after claude set = %+v, %v; want claude=opus, codex NULL", got, err)
	}

	codexRet, err := q.SetUserCodexTaskReviewModel(ctx, store.SetUserCodexTaskReviewModelParams{ID: user, CodexTaskReviewModel: pgtype.Text{String: "gpt-6.1-sol", Valid: true}})
	if err != nil || !codexRet.Valid || codexRet.String != "gpt-6.1-sol" {
		t.Fatalf("set codex = %+v, %v", codexRet, err)
	}
	got, err = q.GetUserTaskReviewSettings(ctx, user)
	if err != nil || got.TaskReviewModel.String != "opus" || got.CodexTaskReviewModel.String != "gpt-6.1-sol" {
		t.Fatalf("after both set = %+v, %v", got, err)
	}
	wide, err := q.GetUserSettings(ctx, user)
	if err != nil || wide.TaskReviewModel.String != "opus" || wide.CodexTaskReviewModel.String != "gpt-6.1-sol" {
		t.Fatalf("GetUserSettings = %+v, %v; want the same two columns", wide, err)
	}

	if ret, err = q.SetUserTaskReviewModel(ctx, store.SetUserTaskReviewModelParams{ID: user}); err != nil || ret.Valid {
		t.Fatalf("clear claude = %+v, %v", ret, err)
	}
	if codexRet, err = q.SetUserCodexTaskReviewModel(ctx, store.SetUserCodexTaskReviewModelParams{ID: user}); err != nil || codexRet.Valid {
		t.Fatalf("clear codex = %+v, %v", codexRet, err)
	}
	got, err = q.GetUserTaskReviewSettings(ctx, user)
	if err != nil || got.TaskReviewModel.Valid || got.CodexTaskReviewModel.Valid {
		t.Fatalf("after clear = %+v, %v; want both NULL", got, err)
	}
}

// TestUserTaskReviewEffortLiveDB executes the queries migration 00510 adds columns for
// (SetUserTaskReviewEffort, SetUserCodexTaskReviewEffort, and the effort and worker-effort columns
// of GetUserTaskReviewSettings and GetUserSettings, PRD #1570) against real Postgres.
func TestUserTaskReviewEffortLiveDB(t *testing.T) {
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

	user := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash, default_effort, default_codex_effort) VALUES ($1, $2, 'x', 'high', 'runtime')`,
		user, fmt.Sprintf("task-review-effort-%s@e2e", user))

	got, err := q.GetUserTaskReviewSettings(ctx, user)
	if err != nil || got.TaskReviewEffort.Valid || got.CodexTaskReviewEffort.Valid {
		t.Fatalf("pristine = %+v, %v; want both review efforts NULL (inherit)", got, err)
	}
	if got.DefaultEffort.String != "high" || got.DefaultCodexEffort.String != "runtime" {
		t.Fatalf("worker efforts = %+v / %+v, want high and runtime", got.DefaultEffort, got.DefaultCodexEffort)
	}

	ret, err := q.SetUserTaskReviewEffort(ctx, store.SetUserTaskReviewEffortParams{ID: user, TaskReviewEffort: pgtype.Text{String: "xhigh", Valid: true}})
	if err != nil || ret.String != "xhigh" {
		t.Fatalf("set claude = %+v, %v", ret, err)
	}
	codexRet, err := q.SetUserCodexTaskReviewEffort(ctx, store.SetUserCodexTaskReviewEffortParams{ID: user, CodexTaskReviewEffort: pgtype.Text{String: "runtime", Valid: true}})
	if err != nil || codexRet.String != "runtime" {
		t.Fatalf("set codex = %+v, %v", codexRet, err)
	}
	got, err = q.GetUserTaskReviewSettings(ctx, user)
	if err != nil || got.TaskReviewEffort.String != "xhigh" || got.CodexTaskReviewEffort.String != "runtime" {
		t.Fatalf("after set = %+v, %v", got, err)
	}
	wide, err := q.GetUserSettings(ctx, user)
	if err != nil || wide.TaskReviewEffort.String != "xhigh" || wide.CodexTaskReviewEffort.String != "runtime" {
		t.Fatalf("GetUserSettings = %+v, %v; want the same two columns", wide, err)
	}
	if ret, err = q.SetUserTaskReviewEffort(ctx, store.SetUserTaskReviewEffortParams{ID: user}); err != nil || ret.Valid {
		t.Fatalf("clear claude = %+v, %v", ret, err)
	}
	if codexRet, err = q.SetUserCodexTaskReviewEffort(ctx, store.SetUserCodexTaskReviewEffortParams{ID: user}); err != nil || codexRet.Valid {
		t.Fatalf("clear codex = %+v, %v", codexRet, err)
	}
	got, err = q.GetUserTaskReviewSettings(ctx, user)
	if err != nil || got.TaskReviewEffort.Valid || got.CodexTaskReviewEffort.Valid {
		t.Fatalf("after clear = %+v, %v; want both NULL", got, err)
	}
}
