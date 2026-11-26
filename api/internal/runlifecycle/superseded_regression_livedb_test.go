package runlifecycle

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/board"
	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// This file deliberately references nothing the #4704 change adds, so it compiles and
// runs against the base commit, where it fails (no fence: the old run moves the card).

type supRegMove struct {
	projectID, issueIID int64
	target              string
}

type supRegForge struct{ forgetest.BaseFake }

// supRegMover is a recording Mover: ForgeForConnection hands back a bare fake and
// AutoMove records the move instead of calling a forge.
type supRegMover struct {
	moves []supRegMove
}

func (m *supRegMover) ForgeForConnection(string, string, []byte) (forge.Forge, error) {
	return &supRegForge{}, nil
}

func (m *supRegMover) AutoMove(_ context.Context, _ forge.IssueTracker, forgeProjectID int64, issue store.Issue, _ []store.BoardColumn, target string) (store.Issue, error) {
	m.moves = append(m.moves, supRegMove{forgeProjectID, issue.ForgeIssueIid, target})
	return issue, nil
}

func supRegTS(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// #4704 regression: an older failed run whose move marker is still pending must not
// restore its origin column onto a card that a newer run on the same issue now owns.
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestSupersededRunRetryDoesNotMoveNewerRunsCardLiveDB(t *testing.T) {
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
	t.Cleanup(pool.Close)
	q := store.New(pool)

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	const issueN = 41
	projectID := 5_000_000_000 + int64(uuid.New().ID())
	user, conn, repo := uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("suprg-%s@e2e", user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, user, "https://"+uuid.NewString()+".forge.test", []byte("sealed"))
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, $3, $4, 'https://forge.test/g/s', 'main', true)`, repo, conn, projectID, "g/suprg-"+repo.String())
	exec(`INSERT INTO board_columns(repo_id,label_name,position) VALUES($1,$2,0),($1,'Upcoming',1),($1,$3,2)`,
		repo, board.ColumnInProgress, board.ColumnHumanReview)
	exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, $2, 't', 'opened', $3::jsonb, 'https://forge.test/i/41', now(), now())`,
		repo, issueN, fmt.Sprintf(`[%q]`, board.ColumnInProgress))

	pendingSince := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	now := time.Now().UTC()
	oldID, newID := uuid.New(), uuid.New()
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status,
	        origin_column, board_column, move_pending_since, created_at)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, 'failed', 'Upcoming', $6, $7, $8)`,
		oldID, user, repo, issueN, "agent/issue-"+oldID.String(), board.ColumnInProgress, pendingSince, now.Add(-2*time.Hour))
	// The newer run is running and also expects In Progress (its origin was the
	// old run's In Progress placement), so both baselines match the card.
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status,
	        origin_column, board_column, created_at)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, 'running', $6, $6, $7)`,
		newID, user, repo, issueN, "agent/issue-"+newID.String(), board.ColumnInProgress, now.Add(-time.Hour))

	pending, err := q.ListPendingColumnMoves(ctx, store.ListPendingColumnMovesParams{
		GraceCutoff:  supRegTS(pendingSince),
		GiveupCutoff: supRegTS(pendingSince.Add(-time.Microsecond)),
		MaxBatch:     1000,
	})
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	found := false
	for _, id := range pending {
		if id == oldID {
			found = true
		}
	}
	if !found {
		t.Fatalf("setup: old run not listed by ListPendingColumnMoves")
	}

	mover := &supRegMover{}
	l := New(q, mover, "")
	l.reconcileOne(ctx, oldID)

	var moves []supRegMove
	for _, m := range mover.moves {
		if m.projectID == projectID && m.issueIID == issueN {
			moves = append(moves, m)
		}
	}
	if len(moves) != 0 {
		t.Fatalf("old run moved the newer run's card: %v", moves)
	}
	var since pgtype.Timestamptz
	if err := pool.QueryRow(ctx, `SELECT move_pending_since FROM runs WHERE id = $1`, oldID).Scan(&since); err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if since.Valid {
		t.Fatalf("old run's move marker still set: %v", since.Time)
	}
}
