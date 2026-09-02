package workersvc

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestFindingsBacklogLiveDB exercises Service.FindingsBacklog against a REAL Postgres — the
// M4 backlog read end to end through the M1 store (extended in M4 with repo_path +
// latest_finding_id): the disposition-driven, coordinate-deduped assembly the fake-store unit
// tests cannot cover. It proves the M4 success criteria (PRD line 121) and the M4 store
// columns:
//
//	(a) a finding reported in TWO runs dedupes into ONE row reading seen_in_runs=2;
//	(b) buckets by disposition status (to_file/filed/dismissed/all);
//	(c) ?repo= filters to one repo, and the same location under two repos stays two rows;
//	(d) the D8 meta open_count is returned and matches CountOpenFindingsForUser (also under ?repo=);
//	(e) a filed coordinate whose evidence rows were DELETED still appears (disposition-driven),
//	    showing last_title and finding_id=nil;
//	(f) a non-owner sees NONE of another user's rows;
//	(g) the deferred M1 assertion — a ?run= filter over an OPEN coordinate recurring in TWO runs
//	    still shows seen_in_runs=2 (the semi-join must NOT shrink it: EXISTS-not-WHERE);
//	(h) repo_path and finding_id are carried for a live coordinate.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres (e2e/run-store-it.sh).
func TestFindingsBacklogLiveDB(t *testing.T) {
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
	q := store.New(pool)

	userA, userB, connID := uuid.New(), uuid.New(), uuid.New()
	repoA, repoB := uuid.New(), uuid.New()
	runA1, runA2, runB1 := uuid.New(), uuid.New(), uuid.New()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userA, fmt.Sprintf("m4a-%s@e2e", userA))
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, userB, fmt.Sprintf("m4b-%s@e2e", userB))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`, connID, userA, []byte{0x1})
	mkRepo := func(id uuid.UUID, pid int64, path string) {
		exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
		      VALUES ($1, $2, $3, $4, 'https://forge.e2e/'||$4, 'main', true)`, id, connID, pid, path)
	}
	mkRun := func(id, repoID uuid.UUID, iid int64) {
		exec(`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind)
		      VALUES ($1, $2, $3, $4, 'Do X', 'desc', 'completed', 'issue')`, id, userA, repoID, iid)
	}
	mkRepo(repoA, 1, "g/a")
	mkRepo(repoB, 2, "g/b")
	mkRun(runA1, repoA, 11)
	mkRun(runA2, repoA, 12)
	mkRun(runB1, repoB, 21)

	insFinding := func(runID, repoID uuid.UUID, location, title string) store.IncidentalFinding {
		t.Helper()
		f, err := q.InsertFinding(ctx, store.InsertFindingParams{
			RunID: runID, UserID: userA, RepoID: repoID, Location: location,
			Title: title, DescriptionMd: "does a thing", Labels: []byte(`["bug"]`), Confidence: "high",
		})
		if err != nil {
			t.Fatalf("InsertFinding(%s): %v", location, err)
		}
		return f
	}
	openDisp := func(repoID uuid.UUID, location, hash, title string) {
		t.Helper()
		if _, err := q.UpsertOpenDisposition(ctx, store.UpsertOpenDispositionParams{
			UserID: userA, RepoID: repoID, Location: location, ContentHash: hash, LastTitle: title,
		}); err != nil {
			t.Fatalf("UpsertOpenDisposition(%s): %v", location, err)
		}
	}
	// find returns the coordinate matching (repo, location) in a backlog, or nil.
	find := func(b apitypes.IncidentalFindingBacklogDTO, repoID uuid.UUID, location string) *apitypes.IncidentalFindingDTO {
		for i := range b.Findings {
			if b.Findings[i].Location == location && b.Findings[i].RepoID == repoID.String() {
				return &b.Findings[i]
			}
		}
		return nil
	}

	const locOpen = "internal/sweep.go#sweeploop"
	const locFiled = "internal/http.go#serve"
	const locDismissed = "internal/db.go#open"

	// (a)+(c)+(g): the SAME open coordinate reported in runA1 AND runA2 in repoA (2 evidence
	// rows → one coordinate, seen_in_runs=2); the same location under repoB is a DISTINCT
	// coordinate (seen_in_runs=1).
	insFinding(runA1, repoA, locOpen, "leaked ticker")
	insFinding(runA2, repoA, locOpen, "leaked ticker (again)")
	insFinding(runB1, repoB, locOpen, "leaked ticker (repo B)")
	openDisp(repoA, locOpen, "h-open-a", "leaked ticker")
	openDisp(repoB, locOpen, "h-open-b", "leaked ticker (repo B)")

	// (e): a FILED coordinate in repoA whose evidence rows are then DELETED — it must still
	// appear, disposition-driven, with last_title shown and finding_id=nil.
	insFinding(runA1, repoA, locFiled, "n+1 in serve")
	openDisp(repoA, locFiled, "h-filed", "n+1 in serve")
	const filedURL = "https://forge.e2e/g/a/-/issues/4242"
	exec(`UPDATE finding_dispositions SET status='filed', filed_issue_iid=4242, filed_issue_url=$4, resolved_at=now()
	      WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userA, repoA, locFiled, filedURL)
	exec(`DELETE FROM findings WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userA, repoA, locFiled)

	// (b): a DISMISSED coordinate in repoA.
	insFinding(runA2, repoA, locDismissed, "unused open")
	openDisp(repoA, locDismissed, "h-dismissed", "unused open")
	exec(`UPDATE finding_dispositions SET status='dismissed', dismiss_reason='wont_do', resolved_at=now()
	      WHERE user_id=$1 AND repo_id=$2 AND location=$3`, userA, repoA, locDismissed)

	// ── (a)+(c)+(h): all-bucket backlog, dedupe + cross-repo + repo_path + finding_id ──
	all, err := svc.FindingsBacklog(ctx, userA, BucketAll, uuid.Nil, uuid.Nil)
	if err != nil {
		t.Fatalf("FindingsBacklog(all): %v", err)
	}
	if len(all.Findings) != 4 { // locOpen@repoA, locOpen@repoB, locFiled@repoA, locDismissed@repoA
		t.Fatalf("all bucket has %d coordinates, want 4: %+v", len(all.Findings), all.Findings)
	}
	openA := find(all, repoA, locOpen)
	openB := find(all, repoB, locOpen)
	filed := find(all, repoA, locFiled)
	if openA == nil || openB == nil || filed == nil {
		t.Fatalf("expected coordinates missing: openA=%v openB=%v filed=%v", openA, openB, filed)
	}
	if openA.SeenInRuns != 2 {
		t.Errorf("locOpen@repoA seen_in_runs = %d, want 2 (deduped across two runs)", openA.SeenInRuns)
	}
	if openB.SeenInRuns != 1 {
		t.Errorf("locOpen@repoB seen_in_runs = %d, want 1 (distinct coordinate)", openB.SeenInRuns)
	}
	if openA.RepoPath != "g/a" {
		t.Errorf("locOpen@repoA repo_path = %q, want g/a", openA.RepoPath)
	}
	if openA.FindingID == nil {
		t.Errorf("locOpen@repoA finding_id must be non-nil for a live coordinate")
	} else if _, perr := uuid.Parse(*openA.FindingID); perr != nil {
		t.Errorf("locOpen@repoA finding_id is not a uuid: %q", *openA.FindingID)
	}
	// (e) filed coordinate with deleted evidence: still present, last_title shown, finding_id nil.
	if filed.SeenInRuns != 0 {
		t.Errorf("filed coordinate with deleted evidence seen_in_runs = %d, want 0", filed.SeenInRuns)
	}
	if filed.FindingID != nil {
		t.Errorf("filed coordinate with deleted evidence must have finding_id=nil, got %v", *filed.FindingID)
	}
	if filed.LastTitle != "n+1 in serve" {
		t.Errorf("filed coordinate last_title = %q, want the snapshot 'n+1 in serve'", filed.LastTitle)
	}
	if filed.FiledIssueIID == nil || *filed.FiledIssueIID != 4242 {
		t.Errorf("filed coordinate filed_issue_iid = %v, want 4242", filed.FiledIssueIID)
	}
	// (h): the filed coordinate carries its stored forge URL end to end (FIX 1) — this is what
	// lets the web link "Filed #<iid>" for a backlog-loaded row, not just a session-filed one.
	if filed.FiledIssueURL != filedURL {
		t.Errorf("filed coordinate filed_issue_url = %q, want %q", filed.FiledIssueURL, filedURL)
	}

	// ── (b) buckets by disposition status ──
	toFile, _ := svc.FindingsBacklog(ctx, userA, BucketToFile, uuid.Nil, uuid.Nil)
	if len(toFile.Findings) != 2 { // locOpen@repoA + locOpen@repoB
		t.Fatalf("to_file bucket has %d, want 2 (the two open coordinates)", len(toFile.Findings))
	}
	for _, f := range toFile.Findings {
		if f.Status != "open" {
			t.Errorf("to_file bucket row status = %q, want open", f.Status)
		}
	}
	filedBucket, _ := svc.FindingsBacklog(ctx, userA, BucketFiled, uuid.Nil, uuid.Nil)
	if len(filedBucket.Findings) != 1 || filedBucket.Findings[0].Location != locFiled {
		t.Fatalf("filed bucket = %+v, want exactly the filed coordinate", filedBucket.Findings)
	}
	dismissedBucket, _ := svc.FindingsBacklog(ctx, userA, BucketDismissed, uuid.Nil, uuid.Nil)
	if len(dismissedBucket.Findings) != 1 || dismissedBucket.Findings[0].Location != locDismissed {
		t.Fatalf("dismissed bucket = %+v, want exactly the dismissed coordinate", dismissedBucket.Findings)
	}

	// ── (d) meta open_count matches CountOpenFindingsForUser, globally and under ?repo= ──
	wantOpen, err := q.CountOpenFindingsForUser(ctx, store.CountOpenFindingsForUserParams{UserID: userA})
	if err != nil {
		t.Fatalf("CountOpenFindingsForUser: %v", err)
	}
	if int64(all.OpenCount) != wantOpen || wantOpen != 2 {
		t.Errorf("all.open_count = %d, CountOpenFindingsForUser = %d, want both 2", all.OpenCount, wantOpen)
	}
	byRepoA, _ := svc.FindingsBacklog(ctx, userA, BucketAll, repoA, uuid.Nil)
	wantOpenA, _ := q.CountOpenFindingsForUser(ctx, store.CountOpenFindingsForUserParams{
		UserID: userA, RepoID: pgconv.UUID(repoA),
	})
	if int64(byRepoA.OpenCount) != wantOpenA || wantOpenA != 1 {
		t.Errorf("?repo=repoA open_count = %d, CountOpenFindingsForUser(repoA) = %d, want both 1", byRepoA.OpenCount, wantOpenA)
	}
	if byRepoA.Repo != repoA.String() {
		t.Errorf("?repo= echo = %q, want %q", byRepoA.Repo, repoA.String())
	}
	for _, f := range byRepoA.Findings {
		if f.RepoID != repoA.String() {
			t.Errorf("?repo=repoA leaked a repoB coordinate: %+v", f)
		}
	}

	// ── (g) the deferred M1 assertion: ?run=runA1 over the OPEN coordinate recurring in TWO
	// runs still shows seen_in_runs=2 (the EXISTS semi-join must not shrink it). ──
	byRun, _ := svc.FindingsBacklog(ctx, userA, BucketToFile, uuid.Nil, runA1)
	anchored := find(byRun, repoA, locOpen)
	if anchored == nil {
		t.Fatalf("?run=runA1 must keep the coordinate that recurs in it; got %+v", byRun.Findings)
	}
	if anchored.SeenInRuns != 2 {
		t.Errorf("?run= anchored coordinate seen_in_runs = %d, want 2 (semi-join must NOT shrink it)", anchored.SeenInRuns)
	}
	if byRun.Run != runA1.String() {
		t.Errorf("?run= echo = %q, want %q", byRun.Run, runA1.String())
	}
	if find(byRun, repoB, locOpen) != nil {
		t.Errorf("?run=runA1 must drop the repoB coordinate (no evidence there)")
	}

	// ── (f) a non-owner sees NONE of userA's coordinates ──
	foreign, err := svc.FindingsBacklog(ctx, userB, BucketAll, uuid.Nil, uuid.Nil)
	if err != nil {
		t.Fatalf("FindingsBacklog(non-owner): %v", err)
	}
	if len(foreign.Findings) != 0 || foreign.OpenCount != 0 {
		t.Errorf("non-owner backlog = %d rows / open_count %d, want 0/0", len(foreign.Findings), foreign.OpenCount)
	}
}
