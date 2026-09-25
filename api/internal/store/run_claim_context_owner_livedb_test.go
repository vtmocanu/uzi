package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestRunClaimContextOwnerScopedLiveDB pins issue #1688: GetRunClaimContext and its two
// run-keyed siblings (GetRunMoveContext, GetRunForgeConnForWorker) project the forge
// connection's sealed bot PAT, so each must only resolve when the run's repo belongs to the
// run's own user. Every run-insert path checks repo ownership in Go (GetRepoForUser), but
// the schema does not tie runs.user_id to the repo owner, so the query is the last line: a
// run row whose repo_id points at another user's repo must read as no rows (the claim path
// then fails closed as a vanished run) rather than hand that user's token to the claimant.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres; e2e/run-store-it.sh
// provides one. A package that prints `ok` with PASS=0 is INVALID, not green.
func TestRunClaimContextOwnerScopedLiveDB(t *testing.T) {
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

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	// Two users, each with their own connection and repo. uuid-derived literals keep the
	// UNIQUE columns clear of fixtures other packages insert into the shared database.
	seedOwner := func(tag string) (userID, repoID uuid.UUID) {
		userID, connID := uuid.New(), uuid.New()
		repoID = uuid.New()
		exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("rcco-%s-%s@e2e", tag, userID))
		exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		      VALUES ($1, $2, 'github', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte("sealed-"+tag))
		exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		      VALUES ($1, $2, 1, $3, 'https://forge.e2e/g/rcco', 'main', true)`, repoID, connID, "g/rcco-"+tag+"-"+userID.String())
		return userID, repoID
	}
	ownerA, repoA := seedOwner("a")
	_, repoB := seedOwner("b")

	// User A's worker holds both runs, so GetRunForgeConnForWorker's worker_id gate passes and
	// only the owner predicate can refuse the cross-owner row.
	wkr := uuid.New()
	exec(`INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`, wkr, ownerA, wkr[:])
	insertRun := func(userID, repoID uuid.UUID) uuid.UUID {
		runID := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, status, worker_id)
		      VALUES ($1, $2, $3, 'issue', 1, 'do x', 'ctx', 'claimed', $4)`, runID, userID, repoID, wkr)
		return runID
	}
	own := insertRun(ownerA, repoA)
	cross := insertRun(ownerA, repoB)

	// All three run-keyed queries that project the connection's sealed bot PAT. Positive
	// control: the run on its owner's own repo resolves with that owner's token. The defect:
	// user A's run pointing at user B's repo must NOT resolve B's token.
	tokenQueries := []struct {
		name  string
		token func(runID uuid.UUID) ([]byte, error)
	}{
		{"GetRunClaimContext", func(id uuid.UUID) ([]byte, error) {
			r, err := q.GetRunClaimContext(ctx, id)
			return r.TokenCiphertext, err
		}},
		{"GetRunMoveContext", func(id uuid.UUID) ([]byte, error) {
			r, err := q.GetRunMoveContext(ctx, id)
			return r.TokenCiphertext, err
		}},
		{"GetRunForgeConnForWorker", func(id uuid.UUID) ([]byte, error) {
			r, err := q.GetRunForgeConnForWorker(ctx, store.GetRunForgeConnForWorkerParams{RunID: id, WorkerID: pgtype.UUID{Bytes: wkr, Valid: true}})
			return r.TokenCiphertext, err
		}},
	}
	for _, tq := range tokenQueries {
		t.Run(tq.name, func(t *testing.T) {
			tok, err := tq.token(own)
			if err != nil {
				t.Fatalf("own repo: %v", err)
			}
			if string(tok) != "sealed-a" {
				t.Fatalf("own-repo token = %q, want %q", string(tok), "sealed-a")
			}
			tok, err = tq.token(cross)
			if !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("cross-owner repo = (token %q, err %v), want pgx.ErrNoRows", string(tok), err)
			}
		})
	}
}
