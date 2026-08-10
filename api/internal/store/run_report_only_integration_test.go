package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestRunReportOnlyLiveDB pins issue #279 M2's report_only/report_md columns against a
// REAL Postgres: SetRunCompleted plain-assigns both, the RETURNING/SELECT * flow reads
// them back on the store.Run row, and a normal (no-declaration) completion leaves
// report_only=false / report_md NULL — the additive-absent, byte-identical-to-before
// contract. None of this round-trip lives in Go, so a fake store cannot exercise it.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (the store IT
// runner provides one).
func TestRunReportOnlyLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store integration runner for live-DB coverage")
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
		userID, fmt.Sprintf("report-only-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	wkr, err := q.CreateWorker(ctx, store.CreateWorkerParams{
		UserID: userID, Name: "laptop", TokenHash: append([]byte("report-only-"), userID[:]...),
		AnthropicBindMode: "default",
	})
	if err != nil {
		t.Fatalf("CreateWorker: %v", err)
	}
	workerID := pgtype.UUID{Bytes: wkr.ID, Valid: true}

	nextIID := int64(1)
	newRun := func() uuid.UUID {
		id := uuid.New()
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, worker_id, kind)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'running', $5, 'issue')`, id, userID, repoID, nextIID, wkr.ID)
		nextIID++
		return id
	}

	readRun := func(id uuid.UUID) store.Run {
		t.Helper()
		run, err := q.GetRunByID(ctx, id)
		if err != nil {
			t.Fatalf("GetRunByID(%s): %v", id, err)
		}
		return run
	}

	t.Run("report-only completion persists report_only=true and report_md", func(t *testing.T) {
		run := newRun()
		const findings = "All CI checks green; no code change required (issue #279)."
		if _, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch:     pgtype.Text{String: "agent/issue-1", Valid: true},
			ReportOnly: true,
			ReportMd:   pgtype.Text{String: findings, Valid: true},
			ID:         run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunCompleted: %v", err)
		}
		got := readRun(run)
		if !got.ReportOnly {
			t.Errorf("report_only = false, want true")
		}
		if !got.ReportMd.Valid || got.ReportMd.String != findings {
			t.Errorf("report_md = valid=%v %q, want valid=true %q", got.ReportMd.Valid, got.ReportMd.String, findings)
		}
	})

	t.Run("normal completion leaves report_only=false and report_md NULL", func(t *testing.T) {
		run := newRun()
		if _, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
			Branch:   pgtype.Text{String: "agent/issue-2", Valid: true},
			MrIid:    pgtype.Int8{Int64: 42, Valid: true},
			MrWebUrl: pgtype.Text{String: "https://forge.e2e/g/r/-/merge_requests/42", Valid: true},
			ID:       run, WorkerID: workerID,
		}); err != nil {
			t.Fatalf("SetRunCompleted: %v", err)
		}
		got := readRun(run)
		if got.ReportOnly {
			t.Errorf("report_only = true, want false for a normal MR completion")
		}
		if got.ReportMd.Valid {
			t.Errorf("report_md = %q, want NULL for a normal MR completion", got.ReportMd.String)
		}
	})
}
