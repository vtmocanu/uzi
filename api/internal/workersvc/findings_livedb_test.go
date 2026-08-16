package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCreateFindingLiveDB exercises Service.CreateFinding against a REAL Postgres — the
// M2 capture path end to end through the M1 store: derive (user_id, repo_id) from the
// claimed run (never a client id), canonicalise the location, and the D6 anti-nag /
// D3 re-open ordering the fake-store unit tests assert only behaviourally.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (named OUTSIDE the
// uzi- namespace, per the store live-DB harness).
func TestCreateFindingLiveDB(t *testing.T) {
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
	svc := New(store.New(pool), nil, Params{})

	userID, connID, repoID := uuid.New(), uuid.New(), uuid.New()
	run1, run2, run3 := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("m2findings-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/m2', 'https://forge.e2e/g/m2', 'main', true)`, repoID, connID)
	mkRun := func(id uuid.UUID, iid int64) {
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		      VALUES ($1, $2, $3, $4, 'Do X', 'desc', 'running', 'issue')`, id, userID, repoID, iid)
	}
	mkRun(run1, 11)
	mkRun(run2, 12)
	mkRun(run3, 13)

	wkr := store.Worker{ID: uuid.New(), UserID: userID}

	evidenceCount := func() int64 {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM findings WHERE user_id=$1 AND repo_id=$2 AND location='api/internal/sweep.go#sweeploop'`, userID, repoID).Scan(&n); err != nil {
			t.Fatalf("count evidence: %v", err)
		}
		return n
	}
	dispStatus := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM finding_dispositions WHERE user_id=$1 AND repo_id=$2 AND location='api/internal/sweep.go#sweeploop'`, userID, repoID).Scan(&s); err != nil {
			t.Fatalf("read disposition status: %v", err)
		}
		return s
	}

	// (1) First report — a drifted location collapses to the canonical coordinate and
	//     opens the disposition.
	if _, err := svc.CreateFinding(ctx, wkr, run1, CreateFindingRequest{
		Title: "Leaked ticker", Description: "sweepLoop never Stops it", Location: "./api/internal/Sweep.go#sweepLoop",
	}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	if evidenceCount() != 1 {
		t.Fatalf("after first report, evidence=%d want 1", evidenceCount())
	}
	if dispStatus() != "open" {
		t.Fatalf("first report status=%q want open", dispStatus())
	}

	// (2) The user dismisses the coordinate.
	exec(`UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do', resolved_at=now()
	      WHERE user_id=$1 AND repo_id=$2 AND location='api/internal/sweep.go#sweeploop'`, userID, repoID)

	// (3) A later run RE-REPORTS the SAME finding (identical content) at the same
	//     coordinate: a new evidence row exists, but the dismissed coordinate does NOT
	//     resurrect (anti-nag, R2).
	if _, err := svc.CreateFinding(ctx, wkr, run2, CreateFindingRequest{
		Title: "Leaked ticker", Description: "sweepLoop never Stops it", Location: "api/internal/sweep.go#sweepLoop",
	}); err != nil {
		t.Fatalf("re-report: %v", err)
	}
	if evidenceCount() != 2 {
		t.Errorf("after identical re-report, evidence=%d want 2 (evidence still recorded)", evidenceCount())
	}
	if dispStatus() != "dismissed" {
		t.Errorf("an identical-hash re-report must NOT resurrect a dismissed coordinate; status=%q", dispStatus())
	}

	// (4) A materially-different finding at the SAME coordinate re-opens it (D3).
	if _, err := svc.CreateFinding(ctx, wkr, run3, CreateFindingRequest{
		Title: "Leaked ticker", Description: "actually it double-Stops and panics", Location: "api/internal/sweep.go#sweepLoop",
	}); err != nil {
		t.Fatalf("materially-different re-report: %v", err)
	}
	if evidenceCount() != 3 {
		t.Errorf("after re-open report, evidence=%d want 3", evidenceCount())
	}
	if dispStatus() != "open" {
		t.Errorf("a materially-different report must re-open the coordinate; status=%q", dispStatus())
	}
}
