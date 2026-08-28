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

// TestMRReworkLiveDB exercises the PRD #700 M3 SQL against a REAL Postgres — the parts
// only a live DB can answer: the candidate query's opted-in + opened-MR + green-vs-red
// gates, the create-time CROSS-KIND branch guard keyed on the pipeline_ref column
// (SC4), the runs_kind_shape CHECK for the new mr_rework kind, and the ledger's
// advance-only GREATEST semantics.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (per the store
// live-DB harness). A package that prints `ok` with PASS=0 is INVALID, not green.
func TestMRReworkLiveDB(t *testing.T) {
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

	// A repo owned by an opted-in user (mr_rework_enabled NULL = default-ON) and a repo
	// owned by an opted-OUT user (mr_rework_enabled=false), each with a distinct branch,
	// so the candidate query's opt-in gate can be asserted both ways in one pass.
	inUser, outUser := uuid.New(), uuid.New()
	connID, repoID := uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, inUser, fmt.Sprintf("in-%s@e2e", inUser))
	exec(`INSERT INTO users (id, email, password_hash, mr_rework_enabled) VALUES ($1, $2, 'x', false)`, outUser, fmt.Sprintf("out-%s@e2e", outUser))
	// The candidate query gates on the owner having an Anthropic token on file (an
	// mr_rework run executes on the owner's token), mirroring ListCIAutofixCandidateRefs.
	// Give the opted-in owner one so it surfaces as a candidate.
	exec(`INSERT INTO user_secrets (user_id, kind, label, is_default, ciphertext, sealed_with)
	      VALUES ($1, 'anthropic_token', 'default', true, $2, 'master')`, inUser, []byte{0x2})
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 777, $3)`, connID, inUser, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/m3', 'https://forge.e2e/g/m3', 'main', true)`, repoID, connID)

	// seedIssueRun inserts a completed issue run with an open MR on agent/issue-<iid>.
	seedIssueRun := func(owner uuid.UUID, iid int64, mrState string, sha string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		branch := fmt.Sprintf("agent/issue-%d", iid)
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
		      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, $6, $7, 'completed')`,
			id, owner, repoID, iid, branch, iid*10, mrState)
		exec(`INSERT INTO pipeline_statuses (repo_id, ref, pipeline_id, sha, status, web_url, synced_at)
		      VALUES ($1, $2, $3, $4, 'success', 'https://forge.e2e/p', now())`, repoID, branch, iid*100, sha)
		return id
	}

	// Opted-in, opened MR, green pipeline → a candidate.
	src7 := seedIssueRun(inUser, 7, "opened", "sha7")
	// Opted-in, but the MR is CLOSED → excluded (stop-on-close, Decision 10).
	seedIssueRun(inUser, 8, "closed", "sha8")
	// Opted-OUT owner → excluded even with an opened MR.
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, mr_iid, mr_state, status)
	      VALUES ($1, $2, $3, 'issue', 9, 't', 'd', 'agent/issue-9', 90, 'opened', 'completed')`,
		uuid.New(), outUser, repoID)

	cands, err := q.ListMRReworkCandidates(ctx, repoID)
	if err != nil {
		t.Fatalf("ListMRReworkCandidates: %v", err)
	}
	if len(cands) != 1 {
		t.Fatalf("expected exactly one candidate (opted-in + opened MR), got %d: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.Ref.String != "agent/issue-7" || c.MrIid.Int64 != 70 || c.SourceRunID != src7 {
		t.Fatalf("candidate row wrong: %+v", c)
	}
	if c.BotForgeUserID != 777 {
		t.Fatalf("bot_forge_user_id not projected: %d", c.BotForgeUserID)
	}
	if !c.PipelineStatus.Valid || c.PipelineStatus.String != "success" || c.PipelineSha.String != "sha7" {
		t.Fatalf("green head pipeline not joined: %+v", c)
	}

	// SC4 — the create-time CROSS-KIND branch guard keys on the pipeline_ref COLUMN. An
	// active ci_fix run created with pipeline_ref=agent/issue-7 (populated AT INSERT,
	// NOT back-filled onto runs.branch) must be seen by CountActiveBranchRunsForRef.
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_id, pipeline_ref, status)
	      VALUES ($1, $2, $3, 'ci_fix', 't', 'd', 4242, 'agent/issue-7', 'running')`,
		uuid.New(), inUser, repoID)
	n, err := q.CountActiveBranchRunsForRef(ctx, store.CountActiveBranchRunsForRefParams{
		RepoID:      repoID,
		PipelineRef: pgtype.Text{String: "agent/issue-7", Valid: true},
	})
	if err != nil {
		t.Fatalf("CountActiveBranchRunsForRef: %v", err)
	}
	if n != 1 {
		t.Fatalf("cross-kind guard must see the active ci_fix on pipeline_ref, got count %d", n)
	}

	// The runs_kind_shape CHECK accepts a valid mr_rework row (repo_id, pipeline_ref,
	// mr_iid, target_run_id all present). Insert on a DIFFERENT ref so the cross-kind
	// unique index does not collide with the ci_fix above.
	run, err := q.CreateAutoMRReworkRun(ctx, store.CreateAutoMRReworkRunParams{
		UserID:           inUser,
		RepoID:           repoID,
		IssueTitle:       "Rework MR review",
		IssueDescription: "d",
		PipelineRef:      pgtype.Text{String: "agent/issue-7b", Valid: true},
		MrIid:            pgtype.Int8{Int64: 71, Valid: true},
		TargetRunID:      pgtype.UUID{Bytes: src7, Valid: true},
		ReviewComments:   []byte(`{"comments":[{"id":120}],"truncated":false}`),
		WaitOnLimit:      false,
	})
	if err != nil {
		t.Fatalf("CreateAutoMRReworkRun (valid shape): %v", err)
	}
	if run.Kind != "mr_rework" || !run.AutoApprove {
		t.Fatalf("mr_rework run shape wrong: kind=%q auto_approve=%v", run.Kind, run.AutoApprove)
	}

	// The shape CHECK REJECTS an mr_rework missing target_run_id.
	if _, err := pool.Exec(ctx,
		`INSERT INTO runs (id, user_id, repo_id, kind, issue_title, issue_description, pipeline_ref, mr_iid, status)
		 VALUES ($1, $2, $3, 'mr_rework', 't', 'd', 'agent/issue-99', 99, 'queued')`,
		uuid.New(), inUser, repoID); err == nil {
		t.Fatal("runs_kind_shape must reject an mr_rework with no target_run_id")
	}

	// Ledger: first upsert INSERTs attempt_count=1 / high_water=120; a second upsert
	// with a SMALLER high_water leaves the mark unchanged (advance-only GREATEST) while
	// still incrementing the counter.
	up := func(hw int64) {
		t.Helper()
		if err := q.UpsertMRReworkLedger(ctx, store.UpsertMRReworkLedgerParams{RepoID: repoID, Ref: "agent/issue-7", HighWater: hw}); err != nil {
			t.Fatalf("UpsertMRReworkLedger(%d): %v", hw, err)
		}
	}
	up(120)
	up(90) // smaller — must NOT lower the mark
	led, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: "agent/issue-7"})
	if err != nil {
		t.Fatalf("GetMRReworkLedger: %v", err)
	}
	if led.HighWater != 120 {
		t.Fatalf("high_water must be advance-only, got %d want 120", led.HighWater)
	}
	if led.AttemptCount != 2 {
		t.Fatalf("attempt_count = %d, want 2", led.AttemptCount)
	}

	// Reconcile eviction with an empty keep-set clears the ledger (stop-on-merge cleanup).
	if _, err := q.DeleteMRReworkLedgerNotIn(ctx, store.DeleteMRReworkLedgerNotInParams{RepoID: repoID, KeepRefs: []string{}}); err != nil {
		t.Fatalf("DeleteMRReworkLedgerNotIn: %v", err)
	}
	if _, err := q.GetMRReworkLedger(ctx, store.GetMRReworkLedgerParams{RepoID: repoID, Ref: "agent/issue-7"}); err == nil {
		t.Fatal("expected the ledger row to be evicted")
	}
}
