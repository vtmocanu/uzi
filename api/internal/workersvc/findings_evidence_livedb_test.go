package workersvc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestFindingsBacklogEvidenceLiveDB exercises the PRD #1183 M3 evidence attach: FindingsBacklog
// batches ListFindingEvidenceForDispositions for the page and sets each row's evidence_preview
// (the NEWEST evidence row's description_md, capped with RationalePreviewMaxRunes) and occurrences
// (newest-first, capped at maxFindingOccurrences=20). Both are properties of the join + the Go
// merge that a fake store cannot reproduce.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestFindingsBacklogEvidenceLiveDB(t *testing.T) {
	dsn := os.Getenv("UZI_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("UZI_TEST_DATABASE_URL not set; run via ./e2e/run-store-it.sh for live-DB coverage")
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
	q := store.New(pool)

	userID, connID := uuid.New(), uuid.New()
	repoID := uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userID, fmt.Sprintf("m3ev-%s@e2e", userID))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userID, []byte{0x1})
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      VALUES ($1, $2, 1, 'g/ev', 'https://forge.e2e/g/ev', 'main', true)`, repoID, connID)

	const location = "internal/loop.go#run"
	if _, err := q.UpsertOpenDisposition(ctx, store.UpsertOpenDispositionParams{
		UserID: userID, RepoID: repoID, Location: location, ContentHash: "h", LastTitle: "leaky loop",
	}); err != nil {
		t.Fatalf("UpsertOpenDisposition: %v", err)
	}

	// 25 evidence rows across 25 runs at the SAME coordinate, with strictly increasing created_at
	// (run index i is older than i+1). Each run's issue_title is unique so the occurrence's
	// run_title is checkable. The NEWEST (i=24) carries a > cap description so the preview truncates.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const total = 25
	longDesc := strings.Repeat("é", RationalePreviewMaxRunes+50) // 50 runes past the cap
	newestRun := uuid.Nil
	for i := 0; i < total; i++ {
		runID := uuid.New()
		if i == total-1 {
			newestRun = runID
		}
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		      VALUES ($1, $2, $3, $4, $5, 'd', 'completed', 'issue')`,
			runID, userID, repoID, int64(100+i), fmt.Sprintf("run-%02d", i))
		desc := fmt.Sprintf("short evidence %02d", i)
		if i == total-1 {
			desc = longDesc
		}
		exec(`INSERT INTO findings (id, run_id, user_id, repo_id, location, title, description_md, labels, confidence, created_at)
		      VALUES ($1, $2, $3, $4, $5, $6, $7, '[]'::jsonb, $8, $9)`,
			uuid.New(), runID, userID, repoID, location,
			fmt.Sprintf("title %02d", i), desc, fmt.Sprintf("conf-%02d", i), base.Add(time.Duration(i)*time.Minute))
	}

	backlog, err := svc.FindingsBacklog(ctx, userID, BucketToFile, uuid.Nil, uuid.Nil)
	if err != nil {
		t.Fatalf("FindingsBacklog: %v", err)
	}
	var row *apitypes.IncidentalFindingDTO
	for i := range backlog.Findings {
		if backlog.Findings[i].Location == location {
			row = &backlog.Findings[i]
		}
	}
	if row == nil {
		t.Fatalf("coordinate not found in backlog: %+v", backlog.Findings)
	}

	// (a) evidence_preview is the NEWEST evidence's description, capped exactly like the judge's.
	wantPreview := rationalePreview(longDesc)
	if row.EvidencePreview != wantPreview {
		t.Fatalf("evidence_preview = %q, want the capped newest description %q", row.EvidencePreview, wantPreview)
	}
	if !strings.HasSuffix(row.EvidencePreview, "…") {
		t.Errorf("a > cap preview must end with the ellipsis, got %q", row.EvidencePreview)
	}
	if len([]rune(row.EvidencePreview)) != RationalePreviewMaxRunes+1 { // capped runes + the ellipsis
		t.Errorf("preview rune length = %d, want %d", len([]rune(row.EvidencePreview)), RationalePreviewMaxRunes+1)
	}

	// (b) occurrences are capped at 20 and newest-first.
	if len(row.Occurrences) != maxFindingOccurrences {
		t.Fatalf("occurrences len = %d, want the cap %d", len(row.Occurrences), maxFindingOccurrences)
	}
	if row.Occurrences[0].RunID != newestRun.String() {
		t.Errorf("occurrences[0].run_id = %s, want the newest run %s", row.Occurrences[0].RunID, newestRun)
	}
	if row.Occurrences[0].RunTitle != fmt.Sprintf("run-%02d", total-1) {
		t.Errorf("occurrences[0].run_title = %q, want %q", row.Occurrences[0].RunTitle, fmt.Sprintf("run-%02d", total-1))
	}
	if row.Occurrences[0].Confidence != fmt.Sprintf("conf-%02d", total-1) {
		t.Errorf("occurrences[0].confidence = %q, want the newest run's", row.Occurrences[0].Confidence)
	}
	for i := 1; i < len(row.Occurrences); i++ {
		if !row.Occurrences[i-1].ReportedAt.After(row.Occurrences[i].ReportedAt) {
			t.Fatalf("occurrences not strictly newest-first at %d: %v then %v",
				i, row.Occurrences[i-1].ReportedAt, row.Occurrences[i].ReportedAt)
		}
	}
	// The 20 kept are the 20 NEWEST (i=24..5); the oldest kept is run index 5.
	oldestKept := row.Occurrences[len(row.Occurrences)-1]
	if oldestKept.RunTitle != fmt.Sprintf("run-%02d", total-maxFindingOccurrences) {
		t.Errorf("oldest kept occurrence run_title = %q, want %q (only the 20 newest survive the cap)",
			oldestKept.RunTitle, fmt.Sprintf("run-%02d", total-maxFindingOccurrences))
	}
	// seen_in_runs counts every distinct run, uncapped.
	if row.SeenInRuns != total {
		t.Errorf("seen_in_runs = %d, want %d (uncapped, distinct runs)", row.SeenInRuns, total)
	}
}
