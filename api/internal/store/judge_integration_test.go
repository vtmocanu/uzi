package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// TestJudgeQueriesLiveDB exercises the PRD #46 judge schema + queries against a REAL
// Postgres — the parts the fake-store unit tests cannot cover: the one-active-judge
// -per-target unique index, the judge-run-scoped trace/review authz query, the
// command-not-found scan input, and the atomic UpsertRunReviewWithRecommendations CTE
// (jsonb_to_recordset + replace-on-re-judge semantics).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestJudgeQueriesLiveDB(t *testing.T) {
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
	targetID, workerID := uuid.New(), uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("judge-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	mustExec(ctx, t, pool,
		`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		 VALUES ($1, $2, 1, 'g/r', 'https://forge.e2e/g/r', 'main', true)`, repoID, connID)
	// The reviewed run: a completed issue run with a couple of tool_result messages.
	mustExec(ctx, t, pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		 VALUES ($1, $2, $3, 42, 'Do X', 'desc', 'completed', 'issue')`, targetID, userID, repoID)
	mustExec(ctx, t, pool,
		`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 1, 'tool_result', $2)`,
		targetID, []byte(`{"text":"bash: shellcheck: command not found"}`))
	mustExec(ctx, t, pool,
		`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 2, 'text', $2)`,
		targetID, []byte(`{"text":"not a tool_result"}`))
	// A tool_use sibling (PRD #121 M3): the invocation side the widened trace query
	// exists to reach. Inserted at a HIGHER seq than the tool_result so the ORDER BY
	// is assertable — under DESC the two swap places.
	mustExec(ctx, t, pool,
		`INSERT INTO run_messages (run_id, seq, kind, payload) VALUES ($1, 3, 'tool_use', $2)`,
		targetID, []byte(`{"id":"tu-1","name":"Bash","input":{"command":"npm run typecheck"}}`))
	mustExec(ctx, t, pool, `INSERT INTO workers (id, user_id, name, token_hash) VALUES ($1, $2, 'w', $3)`,
		workerID, userID, []byte{0x2})

	// ── CreateJudgeRun: repo/issue-less, kind='judge', points at the target ──
	judge, err := q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		UserID: userID, TargetRunID: pgtype.UUID{Bytes: targetID, Valid: true},
		IssueTitle: "Judge: Do X", IssueDescription: "",
	})
	if err != nil {
		t.Fatalf("CreateJudgeRun: %v", err)
	}
	if judge.Kind != "judge" || judge.RepoID.Valid || judge.IssueIid.Valid || !judge.TargetRunID.Valid {
		t.Fatalf("judge run shape wrong: %+v", judge)
	}

	// ── one-active-judge-per-target: a second non-terminal judge → 23505 ──
	if _, err := q.CreateJudgeRun(ctx, store.CreateJudgeRunParams{
		UserID: userID, TargetRunID: pgtype.UUID{Bytes: targetID, Valid: true}, IssueTitle: "dup",
	}); !isUniqueViolation(err) {
		t.Fatalf("a second active judge for the same target must 23505, got %v", err)
	}

	// ── ListToolTraceForRun (PRD #121 M3): BOTH tool kinds, the 'text' row excluded,
	// oldest first. Executing this is the only thing that verifies the query — sqlc
	// generating cleanly is not a measurement (CLAUDE.md), and the three properties
	// asserted here are the three a fold can break: the kind filter, the projection
	// (seq/kind, not just payload), and the ASC ordering suppression depends on.
	trace, err := q.ListToolTraceForRun(ctx, store.ListToolTraceForRunParams{RunID: targetID, Lim: 100})
	if err != nil || len(trace) != 2 {
		t.Fatalf("ListToolTraceForRun = %d rows, %v; want the tool_result + the tool_use, "+
			"with the 'text' row excluded", len(trace), err)
	}
	if trace[0].Seq != 1 || trace[0].Kind != "tool_result" {
		t.Fatalf("trace[0] = seq %d kind %q; want the OLDEST row first (seq 1, tool_result) — "+
			"ORDER BY seq ASC is what makes \"X later ran green\" mean anything", trace[0].Seq, trace[0].Kind)
	}
	if trace[1].Seq != 3 || trace[1].Kind != "tool_use" {
		t.Fatalf("trace[1] = seq %d kind %q; want seq 3, tool_use", trace[1].Seq, trace[1].Kind)
	}
	if !strings.Contains(string(trace[1].Payload), "npm run typecheck") {
		t.Fatalf("the tool_use payload must carry the command text (%q) — it is the ONLY place "+
			"the command exists; tool_result has no command at all", string(trace[1].Payload))
	}

	// ── trace/review authz: claim the judge run, then the scoped query finds it ──
	mustExec(ctx, t, pool, `UPDATE runs SET worker_id = $1, status = 'claimed' WHERE id = $2`, workerID, judge.ID)
	authz, err := q.GetActiveJudgeRunForWorkerTarget(ctx, store.GetActiveJudgeRunForWorkerTargetParams{
		WorkerID: pgtype.UUID{Bytes: workerID, Valid: true}, TargetRunID: pgtype.UUID{Bytes: targetID, Valid: true},
	})
	if err != nil || authz.ID != judge.ID {
		t.Fatalf("GetActiveJudgeRunForWorkerTarget = %v, %v; want the judge run %v", authz.ID, err, judge.ID)
	}

	// ── atomic upsert with 2 recs; then re-judge with 1 rec (replace semantics) ──
	recs2, _ := json.Marshal([]map[string]string{
		{"category": "install_worker_tool", "target": "shellcheck", "rationale_md": "missing", "confidence": "high"},
		{"category": "improve_uzi", "target": "", "rationale_md": "tidy", "confidence": "low"},
	})
	reviewID, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: targetID, JudgeRunID: pgtype.UUID{Bytes: judge.ID, Valid: true}, UserID: userID,
		Verdict: "issues", SummaryMd: "s", JudgeModel: "haiku", Status: "complete",
		ProducedByRunID: pgtype.UUID{Bytes: judge.ID, Valid: true}, ProducedByUserID: pgtype.UUID{Bytes: userID, Valid: true},
		Recommendations: recs2,
	})
	if err != nil {
		t.Fatalf("UpsertRunReviewWithRecommendations (first): %v", err)
	}
	if n := countRecs(ctx, t, pool, reviewID); n != 2 {
		t.Fatalf("after first upsert: %d recommendations, want 2", n)
	}

	recs1, _ := json.Marshal([]map[string]string{
		{"category": "improve_agent", "target": "coder", "rationale_md": "tweak", "confidence": ""},
	})
	reviewID2, err := q.UpsertRunReviewWithRecommendations(ctx, store.UpsertRunReviewWithRecommendationsParams{
		TargetRunID: targetID, JudgeRunID: pgtype.UUID{Bytes: judge.ID, Valid: true}, UserID: userID,
		Verdict: "ok", SummaryMd: "s2", JudgeModel: "haiku", Status: "complete",
		ProducedByRunID: pgtype.UUID{Bytes: judge.ID, Valid: true}, ProducedByUserID: pgtype.UUID{Bytes: userID, Valid: true},
		Recommendations: recs1,
	})
	if err != nil {
		t.Fatalf("UpsertRunReviewWithRecommendations (re-judge): %v", err)
	}
	if reviewID2 != reviewID {
		t.Fatalf("re-judge must UPSERT the same review row (UNIQUE target_run_id): %v vs %v", reviewID2, reviewID)
	}
	if n := countRecs(ctx, t, pool, reviewID); n != 1 {
		t.Fatalf("after re-judge: %d recommendations, want 1 (replace semantics)", n)
	}
	var verdict string
	if err := pool.QueryRow(ctx, `SELECT verdict FROM run_reviews WHERE id = $1`, reviewID).Scan(&verdict); err != nil || verdict != "ok" {
		t.Fatalf("re-judge did not update the verdict: %q, %v", verdict, err)
	}

	// ── read side (M4): GetRunReviewForTarget + ListRecommendationsForReview ──
	review, err := q.GetRunReviewForTarget(ctx, targetID)
	if err != nil {
		t.Fatalf("GetRunReviewForTarget: %v", err)
	}
	if review.ID != reviewID || review.Verdict != "ok" || review.TargetRunID != targetID {
		t.Fatalf("read-back review wrong: %+v", review)
	}
	readRecs, err := q.ListRecommendationsForReview(ctx, reviewID)
	if err != nil {
		t.Fatalf("ListRecommendationsForReview: %v", err)
	}
	if len(readRecs) != 1 || readRecs[0].Category != "improve_agent" || readRecs[0].Target != "coder" {
		t.Fatalf("read-back recommendations wrong (want the single post-re-judge rec): %+v", readRecs)
	}

	// ── trace/review authz: a TERMINAL judge run is no longer "active" ──
	// GetActiveJudgeRunForWorkerTarget filters status NOT IN (completed,failed,
	// cancelled), so once the judge run finishes the worker can no longer stream the
	// target's trace or post another review under it (M6 matrix: "terminal judge run").
	mustExec(ctx, t, pool, `UPDATE runs SET status = 'completed' WHERE id = $1`, judge.ID)
	if _, err := q.GetActiveJudgeRunForWorkerTarget(ctx, store.GetActiveJudgeRunForWorkerTargetParams{
		WorkerID: pgtype.UUID{Bytes: workerID, Valid: true}, TargetRunID: pgtype.UUID{Bytes: targetID, Valid: true},
	}); err != pgx.ErrNoRows {
		t.Fatalf("a terminal judge run must not resolve as active (want ErrNoRows), got %v", err)
	}
}

func countRecs(ctx context.Context, t *testing.T, pool *pgxpool.Pool, reviewID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM review_recommendations WHERE review_id = $1`, reviewID).Scan(&n); err != nil {
		t.Fatalf("count recs: %v", err)
	}
	return n
}
