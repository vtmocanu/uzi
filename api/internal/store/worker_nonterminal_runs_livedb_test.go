package store_test

import (
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestListWorkerNonTerminalRunsLiveDB pins the #3390 delete-refusal listing against a REAL
// Postgres: it matches CountWorkerNonTerminalRuns's predicate (this worker, this user,
// non-terminal), is capped at 10 oldest-first, prefers runs.title over issue_title, and
// leaves issue_iid NULL for chats. Skipped unless UZI_TEST_DATABASE_URL is set.
func TestListWorkerNonTerminalRunsLiveDB(t *testing.T) {
	fx := newFleetFixture(t)
	w := fx.worker("w-list", nil, false)
	other := fx.worker("w-other", nil, false)

	// insert seeds a run with an explicit created_at (minutes before now), so the order is
	// deterministic and independent of insert timing.
	insert := func(userID uuid.UUID, workerID uuid.UUID, kind, status string, title any, issueTitle string, minutesAgo int) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if kind == "chat" {
			mustExec(fx.ctx, t, fx.pool,
				`INSERT INTO runs (id, user_id, kind, title, issue_title, issue_description, status, worker_id, created_at)
				 VALUES ($1, $2, 'chat', $3, $4, 'd', $5, $6, now() - make_interval(mins => $7))`,
				id, userID, title, issueTitle, status, workerID, minutesAgo)
			return id
		}
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, title, issue_title, issue_description, status, worker_id, created_at)
			 VALUES ($1, $2, $3, 'issue', $4, $5, $6, 'd', $7, $8, now() - make_interval(mins => $9))`,
			id, userID, fx.repoID, fx.nextIID(), title, issueTitle, status, workerID, minutesAgo)
		return id
	}

	// Terminal rows are the OLDEST, so an unfiltered listing would surface them first.
	for i, st := range []string{"completed", "failed", "cancelled"} {
		insert(fx.userID, w, "issue", st, nil, fmt.Sprintf("done-%d", i), 500+i)
	}

	statuses := []string{"queued", "running", "awaiting_approval", "awaiting_input"}
	var seeded []uuid.UUID
	for i := 0; i < 12; i++ {
		minutesAgo := 100 - i // strictly increasing created_at
		st := statuses[i%len(statuses)]
		var id uuid.UUID
		switch i {
		case 0: // issue run whose title overrides issue_title
			id = insert(fx.userID, w, "issue", st, "Override title", "forge title", minutesAgo)
		case 1: // chat with no title: falls back to issue_title
			id = insert(fx.userID, w, "chat", st, nil, "fallback chat title", minutesAgo)
		case 3: // chat with a title
			id = insert(fx.userID, w, "chat", st, "chat title", "ignored", minutesAgo)
		default:
			id = insert(fx.userID, w, "issue", st, nil, fmt.Sprintf("issue-%d", i), minutesAgo)
		}
		seeded = append(seeded, id)
	}

	// Rows that must not appear: another worker of the same user, and a second user's run
	// pointed at this worker (the cross-tenant case the user_id scope exists for). They are
	// non-terminal and OLDER than every qualifying row, so a weakened predicate would pull
	// them into the oldest-first first 10.
	insert(fx.userID, other, "issue", "running", nil, "other worker", 300)
	user2 := uuid.New()
	mustExec(fx.ctx, t, fx.pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		user2, fmt.Sprintf("list-%s@e2e", user2))
	insert(user2, w, "chat", "running", nil, "foreign user", 301)

	rows, err := fx.q.ListWorkerNonTerminalRuns(fx.ctx, store.ListWorkerNonTerminalRunsParams{
		WorkerID: pgtype.UUID{Bytes: w, Valid: true}, UserID: fx.userID,
	})
	if err != nil {
		t.Fatalf("ListWorkerNonTerminalRuns: %v", err)
	}
	if len(rows) != 10 {
		t.Fatalf("rows = %d, want the cap of 10", len(rows))
	}
	for i, r := range rows {
		if r.ID != seeded[i] {
			t.Fatalf("row %d = %s (%q), want seeded[%d] = %s (oldest-first, own worker and user, non-terminal only)",
				i, r.ID, r.DisplayTitle, i, seeded[i])
		}
	}
	if rows[0].DisplayTitle != "Override title" || rows[0].Kind != "issue" || !rows[0].IssueIid.Valid {
		t.Errorf("row 0 = %+v, want the issue run with title winning over issue_title", rows[0])
	}
	if rows[1].DisplayTitle != "fallback chat title" || rows[1].Kind != "chat" || rows[1].IssueIid.Valid {
		t.Errorf("row 1 = %+v, want a chat falling back to issue_title with a NULL issue_iid", rows[1])
	}
	if rows[3].DisplayTitle != "chat title" || rows[3].IssueIid.Valid {
		t.Errorf("row 3 = %+v, want the chat's own title and a NULL issue_iid", rows[3])
	}

	total, err := fx.q.CountWorkerNonTerminalRuns(fx.ctx, store.CountWorkerNonTerminalRunsParams{
		WorkerID: pgtype.UUID{Bytes: w, Valid: true}, UserID: fx.userID,
	})
	if err != nil {
		t.Fatalf("CountWorkerNonTerminalRuns: %v", err)
	}
	if total != 12 {
		t.Errorf("count = %d, want 12 (the listing is capped, the count is the total)", total)
	}
}
