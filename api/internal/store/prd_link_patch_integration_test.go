package store_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// The SQL half of PRD #72 M5. The forgesvc tests drive the watcher against a fake
// store and so cannot see what the candidate query MATCHES.
//
// THE FIXTURE CARRIES TWO REPOS SHARING AN ISSUE IID, and that is the point rather
// than thoroughness. On a single-repo fixture `r.repo_id = @repo_id` is UNPINNED —
// every row passes with or without it, so deleting the predicate leaves the suite
// green. The blast radius here is not a wrong badge: the watcher holds ONE repo's
// PAT and performs a description WRITE, so a mis-scoped candidate is a cross-tenant
// forge write.
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestListPRDLinkPatchCandidatesLiveDB(t *testing.T) {
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

	userID, connID := uuid.New(), uuid.New()
	repoA, repoB := uuid.New(), uuid.New()
	mustExec(ctx, t, pool,
		`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`,
		userID, fmt.Sprintf("prdpatch-%s@e2e", userID))
	mustExec(ctx, t, pool,
		`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
		 VALUES ($1, $2, 'gitlab', 'https://forge.e2e', 'bot', 1, $3)`,
		connID, userID, []byte{0x1})
	for i, r := range []uuid.UUID{repoA, repoB} {
		mustExec(ctx, t, pool,
			`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, enabled)
			 VALUES ($1, $2, $3, $4, 'https://forge.e2e/g/r', true)`,
			r, connID, 1000+i, fmt.Sprintf("g/r%d", i))
	}

	base := time.Now().Add(-time.Hour)
	// THE SAME ISSUE IID IN BOTH REPOS. issues.forge_issue_iid is unique only per
	// repo, so this is legal — and it is what makes the scope predicate observable.
	const sharedIID = int64(7)
	mkRun := func(repo uuid.UUID, kind, prdPath string, mrIID *int64, settled bool, offset time.Duration) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var settledAt any
		if settled {
			settledAt = time.Now()
		}
		var path any
		if prdPath != "" {
			path = prdPath
		}
		mustExec(ctx, t, pool,
			`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description,
			                   status, mr_iid, prd_done_path, prd_patch_settled_at, created_at)
			 VALUES ($1, $2, $3, $4, $5, 't', 'links prds/72-x.md', 'completed', $6, $7, $8, $9)`,
			id, userID, repo, kind, sharedIID, mrIID, path, settledAt, base.Add(offset))
		return id
	}
	mr := int64(99)

	// wantA is the NEWEST repo-A row on purpose: `superseded` is EXISTS(a newer run
	// on the same repo+issue), and the exclusion rows below are themselves runs on
	// this issue — so ordering wantA first would make it superseded by its own
	// fixture and the flag assertion below would be measuring the fixture, not the
	// query. (Caught by the assertion failing; the first draft had it at offset 0.)
	wantA := mkRun(repoA, "issue", "prds/done/72-x.md", &mr, false, 7*time.Minute)
	// Repo B's run is identical in every way except its repo. If the scope predicate
	// were dropped, a scan for A would return this too.
	crossTenant := mkRun(repoB, "issue", "prds/done/72-x.md", &mr, false, time.Minute)
	// Excluded rows, one per predicate, all in repo A.
	settled := mkRun(repoA, "issue", "prds/done/72-x.md", &mr, true, 2*time.Minute)
	selfImprove := mkRun(repoA, "self_improve", "prds/done/72-x.md", &mr, false, 3*time.Minute)
	// NO `judge` ROW HERE, and the reason is a finding rather than an omission:
	// `runs_kind_shape` (00058) requires kind='judge' to have repo_id IS NULL AND
	// issue_iid IS NULL, so a judge run cannot physically satisfy this query's
	// repo/issue predicates — the fixture cannot even insert one. The `= 'issue'`
	// form is still the right predicate (closed by construction against a future
	// kind, and self_improve above IS reachable and IS the dangerous one), but the
	// specific "an exclusion list would admit judge" hazard is already shut by the
	// CHECK constraint. Recorded so nobody re-adds an uninsertable row.
	//
	// Note also the DB knows a FIFTH kind, `chat`, which protocol.ts's RunKind does
	// not list; it too has repo_id IS NULL and so is unreachable here.
	noPath := mkRun(repoA, "issue", "", &mr, false, 5*time.Minute)
	noMR := mkRun(repoA, "issue", "prds/done/72-x.md", nil, false, 6*time.Minute)

	rows, err := q.ListPRDLinkPatchCandidates(ctx, store.ListPRDLinkPatchCandidatesParams{
		RepoID: repoA,
		Lim:    50,
	})
	if err != nil {
		t.Fatalf("ListPRDLinkPatchCandidates: %v", err)
	}

	got := map[uuid.UUID]store.ListPRDLinkPatchCandidatesRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if _, ok := got[wantA]; !ok {
		t.Errorf("repo A's pending issue run is missing from its own scan")
	}
	// BY IDENTITY, per exclusion, so a failure says which predicate went.
	for _, c := range []struct {
		id  uuid.UUID
		why string
	}{
		{crossTenant, "another repo's run (the r.repo_id scope predicate)"},
		{settled, "an already-settled edge (prd_patch_settled_at IS NULL)"},
		{selfImprove, "a self_improve run (the kind = 'issue' predicate); its issue is a reused backlog container and a LIVE control document"},
		{noPath, "a run that declared no path (prd_done_path IS NOT NULL)"},
		{noMR, "a run with no MR (mr_iid IS NOT NULL)"},
	} {
		if _, present := got[c.id]; present {
			t.Errorf("candidate scan returned %s", c.why)
		}
	}
	if len(rows) != 1 {
		t.Errorf("expected exactly repo A's one candidate, got %d", len(rows))
	}

	// The snapshot rides the candidate row, so the watcher needs no second read.
	if r := got[wantA]; r.IssueDescription != "links prds/72-x.md" {
		t.Errorf("issue_description = %q, want the queue-time snapshot", r.IssueDescription)
	}

	// --- superseded ----------------------------------------------------------
	// A newer run on the SAME issue in the SAME repo flips the flag. Asserted as a
	// transition on one row rather than as a second fixture, so nothing else varies.
	if r := got[wantA]; r.Superseded {
		t.Errorf("no newer run exists yet; superseded must be false")
	}
	mkRun(repoA, "issue", "", &mr, true, 10*time.Minute) // newer, settled so it is not itself a candidate
	rows2, err := q.ListPRDLinkPatchCandidates(ctx, store.ListPRDLinkPatchCandidatesParams{RepoID: repoA, Lim: 50})
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if len(rows2) != 1 || !rows2[0].Superseded {
		t.Errorf("a newer run on the same issue must set superseded; got %+v", rows2)
	}

	// --- settle is edge-guarded ----------------------------------------------
	n, err := q.SettlePRDLinkPatch(ctx, wantA)
	if err != nil {
		t.Fatalf("SettlePRDLinkPatch: %v", err)
	}
	if n != 1 {
		t.Errorf("first settle affected %d rows, want 1", n)
	}
	// Guarded on IS NULL so two concurrent pollers cannot both consume one edge.
	if n, err = q.SettlePRDLinkPatch(ctx, wantA); err != nil || n != 0 {
		t.Errorf("second settle affected %d rows (err %v), want 0", n, err)
	}
	rows3, err := q.ListPRDLinkPatchCandidates(ctx, store.ListPRDLinkPatchCandidatesParams{RepoID: repoA, Lim: 50})
	if err != nil {
		t.Fatalf("rescan after settle: %v", err)
	}
	if len(rows3) != 0 {
		t.Errorf("a settled edge must not re-enumerate; got %d rows", len(rows3))
	}
}
