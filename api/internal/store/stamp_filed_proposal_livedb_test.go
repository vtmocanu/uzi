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

// TestStampFiledProposalLiveDB pins the PRD #929 M2 server-filing store path against a
// real Postgres: CreateIssueProposal inserts a PENDING row, then StampFiledProposal
// settles it to 'confirmed' with the created iid WITHOUT the status='confirming' guard
// MarkProposalConfirmed carries (the auto-file path never claims). It asserts the row
// ends confirmed + iid + resolved_at set, and that a second stamp is idempotent.
func TestStampFiledProposalLiveDB(t *testing.T) {
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
		userID, fmt.Sprintf("stamp-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)

	// A prompt run is what files a proposal; issue_proposals.run_id FKs runs(id).
	runID := uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 'scheduled idea', 'body', 'completed', 'prompt')`, runID, userID, repoID)

	prop, err := q.CreateIssueProposal(ctx, store.CreateIssueProposalParams{
		RunID: runID, RepoID: repoID, Title: "Add a retry backoff", Description: "detail", Labels: []byte(`["proposal::feature-bingo"]`),
	})
	if err != nil {
		t.Fatalf("CreateIssueProposal: %v", err)
	}
	if prop.Status != "pending" {
		t.Fatalf("a new proposal must be pending, got %q", prop.Status)
	}

	rows, err := q.StampFiledProposal(ctx, store.StampFiledProposalParams{
		ID: prop.ID, Iid: pgtype.Int8{Int64: 123, Valid: true},
	})
	if err != nil {
		t.Fatalf("StampFiledProposal: %v", err)
	}
	if rows != 1 {
		t.Fatalf("StampFiledProposal touched %d rows, want 1 (no status guard on a pending row)", rows)
	}

	var status string
	var iid pgtype.Int8
	var resolvedAt pgtype.Timestamptz
	if err := pool.QueryRow(ctx,
		`SELECT status, created_issue_iid, resolved_at FROM issue_proposals WHERE id = $1`, prop.ID,
	).Scan(&status, &iid, &resolvedAt); err != nil {
		t.Fatalf("read back proposal: %v", err)
	}
	if status != "confirmed" {
		t.Errorf("status = %q, want confirmed", status)
	}
	if !iid.Valid || iid.Int64 != 123 {
		t.Errorf("created_issue_iid = %+v, want 123", iid)
	}
	if !resolvedAt.Valid {
		t.Error("resolved_at must be stamped")
	}

	// Idempotent: a second stamp still matches the row by id (1 row), never errors.
	if rows, err := q.StampFiledProposal(ctx, store.StampFiledProposalParams{
		ID: prop.ID, Iid: pgtype.Int8{Int64: 123, Valid: true},
	}); err != nil || rows != 1 {
		t.Fatalf("second StampFiledProposal: rows=%d err=%v, want 1, nil", rows, err)
	}
}
