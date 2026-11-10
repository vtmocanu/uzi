package store_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #3876: AdminUsagePerUser splits each user's run counts by harness, in both
// windows, over the same population as run_count / last7_run_count. The transaction
// pins now() and rolls back the isolated dataset.
func TestAdminUsageHarnessRunsLiveDB(t *testing.T) {
	e := setupHarnessEnv(t)
	ctx := e.ctx
	tx, err := e.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(context.Background()); err != nil && err != pgx.ErrTxClosed {
			t.Error(err)
		}
	}()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Prior live-DB tests leave runs in the shared database; the reset is undone by rollback.
	exec("TRUNCATE runs CASCADE")
	mixed, claudeOnly, codexOnly := e.userID, uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{claudeOnly, codexOnly} {
		exec("INSERT INTO users (id,email,password_hash) VALUES ($1,$2,'x')", id, fmt.Sprintf("harness-%s@example.com", id))
	}
	type seed struct {
		user      uuid.UUID
		harness   string
		kind      string
		age       string
		withUsage bool
	}
	seeds := []seed{
		// Mixed user: Claude and Codex, inside and outside the seven-day window.
		{mixed, "claude", "issue", "1 day", true},
		{mixed, "claude", "issue", "10 days", true},
		{mixed, "codex", "issue", "2 days", true},
		{mixed, "codex", "issue", "20 days", true},
		{mixed, "codex", "issue", "3 days", true},
		// Excluded: a Codex run without a usage row, and a chat run with usage.
		{mixed, "codex", "issue", "1 day", false},
		{mixed, "claude", "chat", "1 day", true},
		// Claude-only user.
		{claudeOnly, "claude", "issue", "1 day", true},
		{claudeOnly, "claude", "issue", "2 days", true},
		{claudeOnly, "claude", "issue", "30 days", true},
		// Codex-only user, with the cutoff boundary: exactly 7 days is inside (inclusive),
		// a hair older is outside.
		{codexOnly, "codex", "issue", "7 days", true},
		{codexOnly, "codex", "issue", "7 days 0.000001 seconds", true},
	}
	for i, s := range seeds {
		id := uuid.New()
		var repo, iid any = e.repoID, int64(387600 + i)
		if s.kind == "chat" {
			repo, iid = nil, nil
		}
		exec(`INSERT INTO runs (id,user_id,repo_id,issue_iid,issue_title,issue_description,kind,status,created_at,finished_at,harness)
   VALUES ($1,$2,$3,$4,'t','d',$5,'completed',now()-$6::interval,now(),$7)`,
			id, s.user, repo, iid, s.kind, s.age, s.harness)
		if s.withUsage {
			exec(`INSERT INTO run_usage (run_id,session_id,model,input_tokens,cache_read_tokens,cache_creation_tokens,output_tokens,cost_usd,cost_status)
    VALUES ($1,'s','m',1,1,1,1,0,'metered')`, id)
		}
	}
	rows, err := store.New(tx).AdminUsagePerUser(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]store.AdminUsagePerUserRow{}
	for _, r := range rows {
		got[r.UserID] = r
	}
	if len(got) != 3 {
		t.Fatalf("population: %+v", rows)
	}
	// {claude, codex, last7 claude, last7 codex}
	want := map[uuid.UUID][4]int64{
		mixed:      {2, 3, 1, 2},
		claudeOnly: {3, 0, 2, 0},
		codexOnly:  {0, 2, 0, 1},
	}
	for id, w := range want {
		r := got[id]
		g := [4]int64{r.ClaudeRunCount, r.CodexRunCount, r.Last7ClaudeRunCount, r.Last7CodexRunCount}
		if g != w {
			t.Errorf("user %s: harness counts %v, want %v", id, g, w)
		}
		if r.ClaudeRunCount+r.CodexRunCount != r.RunCount {
			t.Errorf("user %s: claude+codex=%d != run_count=%d", id, r.ClaudeRunCount+r.CodexRunCount, r.RunCount)
		}
		if r.Last7ClaudeRunCount+r.Last7CodexRunCount != r.Last7RunCount {
			t.Errorf("user %s: last7 claude+codex=%d != last7_run_count=%d", id, r.Last7ClaudeRunCount+r.Last7CodexRunCount, r.Last7RunCount)
		}
	}
}
