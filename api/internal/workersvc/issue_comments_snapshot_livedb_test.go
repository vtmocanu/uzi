package workersvc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// fakeCommentForge overrides only ListIssueComments (embedding forge.Forge for the
// rest of the interface), returning a per-issue scripted comment list. The other 20
// methods are never called on this path — createRun only reads comments.
type fakeCommentForge struct {
	forge.Forge
	byIID map[int64][]forge.IssueComment
}

func (f *fakeCommentForge) ListIssueComments(_ context.Context, _ int64, issueIID int64) ([]forge.IssueComment, error) {
	return f.byIID[issueIID], nil
}

// fakeCommentForges is the ForgeBuilder seam: every connection resolves to the one
// scripted fake forge (the snapshot filter keys on the connection's stored bot id,
// not on which driver is returned here).
type fakeCommentForges struct{ f forge.Forge }

func (b fakeCommentForges) ForgeForConnection(_ string, _ string, _ []byte) (forge.Forge, error) {
	return b.f, nil
}

// TestIssueCommentsSnapshotLiveDB exercises the M2a fetch+store path against a REAL
// Postgres: createRun builds a forge from the run's connection, fetches the issue's
// comments, applies the D1 self-filter / D9 fail-safe, and stores the structured
// JSONB in runs.issue_comments. It asserts the stored column by reading it back raw.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE
// the uzi- namespace, per the store live-DB harness).
func TestIssueCommentsSnapshotLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via the store live-DB harness for coverage")
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

	const botID = int64(100)
	const humanID = int64(42)
	scripted := map[int64][]forge.IssueComment{
		// (1) human + bot: the human survives the D1 filter, the bot is dropped.
		11: {
			{AuthorForgeUserID: humanID, AuthorUsername: "human", Body: "please guard the budget", CreatedAt: commentTS(1)},
			{AuthorForgeUserID: botID, AuthorUsername: "uzi-bot", Body: "run started", CreatedAt: commentTS(2)},
		},
		// (2) only bot: nothing survives the D1 filter ⇒ NULL.
		12: {
			{AuthorForgeUserID: botID, AuthorUsername: "uzi-bot", Body: "run started", CreatedAt: commentTS(1)},
		},
		// (3) human, but the connection's bot id is unknown (0) ⇒ D9 omits ⇒ NULL.
		13: {
			{AuthorForgeUserID: humanID, AuthorUsername: "human", Body: "context that must not leak the feature", CreatedAt: commentTS(1)},
		},
	}
	svc := New(store.New(pool), nil, Params{})
	svc.SetForges(fakeCommentForges{f: &fakeCommentForge{byIID: scripted}})

	userID := uuid.New()
	connKnown, connZero := uuid.New(), uuid.New()
	repoKnown, repoZero := uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("m2a-comments-%s@e2e", userID))
	// Connection with a KNOWN bot id (100) and one with an UNKNOWN bot id (0, D9).
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'uzi-bot', $3, $4)`, connKnown, userID, botID, []byte{0x1})
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge2.e2e', 'uzi-bot', 0, $3)`, connZero, userID, []byte{0x2})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/known', 'https://forge.e2e/g/known', 'main', true)`, repoKnown, connKnown)
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 2, 'g/zero', 'https://forge.e2e/g/zero', 'main', true)`, repoZero, connZero)
	mkIssue := func(repoID uuid.UUID, iid int64) {
		// Labelled uzi so the single run-eligibility gate (PRD #764 M1) passes; the
		// snapshot behaviour under test is orthogonal to eligibility.
		exec(`INSERT INTO issues (id, repo_id, forge_issue_iid, title, state, labels, web_url, has_prd_link, forge_updated_at, synced_at)
		      VALUES ($1, $2, $3, 'Do X', 'opened', '["PRD","uzi"]', 'https://forge.e2e/i', true, now(), now())`,
			uuid.New(), repoID, iid)
	}
	mkIssue(repoKnown, 11)
	mkIssue(repoKnown, 12)
	mkIssue(repoZero, 13)

	waitFalse := false
	rawColumn := func(runID uuid.UUID) []byte {
		var raw []byte
		if err := pool.QueryRow(ctx, `SELECT issue_comments FROM runs WHERE id = $1`, runID).Scan(&raw); err != nil {
			t.Fatalf("read issue_comments for %s: %v", runID, err)
		}
		return raw
	}

	// (1) Human + bot ⇒ stored JSONB carries the human comment and NOT the bot one.
	run1, err := svc.CreateRun(ctx, userID, repoKnown, 11, "desc", &waitFalse, nil, nil)
	if err != nil {
		t.Fatalf("create run for issue 11: %v", err)
	}
	raw := rawColumn(run1.ID)
	if len(raw) == 0 {
		t.Fatal("issue 11: want a non-NULL issue_comments snapshot, got NULL")
	}
	var snap IssueCommentsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("issue 11: decode stored snapshot: %v", err)
	}
	if len(snap.Comments) != 1 {
		t.Fatalf("issue 11: want exactly the human comment, got %d comments", len(snap.Comments))
	}
	if snap.Comments[0].AuthorForgeUserID != humanID {
		t.Fatalf("issue 11: want the human comment (author %d), got author %d", humanID, snap.Comments[0].AuthorForgeUserID)
	}
	for _, c := range snap.Comments {
		if c.AuthorForgeUserID == botID {
			t.Fatal("issue 11: the bot comment must be filtered out of the stored snapshot (D1)")
		}
	}

	// (2) Only bot comments ⇒ stored NULL.
	run2, err := svc.CreateRun(ctx, userID, repoKnown, 12, "desc", &waitFalse, nil, nil)
	if err != nil {
		t.Fatalf("create run for issue 12: %v", err)
	}
	if raw := rawColumn(run2.ID); len(raw) != 0 {
		t.Fatalf("issue 12: an all-bot thread must store NULL, got %s", raw)
	}

	// (3) Human comment but the connection's bot id is 0 ⇒ D9 stores NULL.
	run3, err := svc.CreateRun(ctx, userID, repoZero, 13, "desc", &waitFalse, nil, nil)
	if err != nil {
		t.Fatalf("create run for issue 13: %v", err)
	}
	if raw := rawColumn(run3.ID); len(raw) != 0 {
		t.Fatalf("issue 13: a zero bot id must store NULL (D9), got %s", raw)
	}
}
