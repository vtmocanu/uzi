package runlifecycle

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/forge"
	"github.com/vtmocanu/uzi/api/internal/forge/forgetest"
	"github.com/vtmocanu/uzi/api/internal/forgesvc"
	"github.com/vtmocanu/uzi/api/internal/issuetracker"
	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	"github.com/vtmocanu/uzi/api/internal/issuetrackersvc"
	"github.com/vtmocanu/uzi/api/internal/secretbox"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// mrForge is the code host of the repo: it reports the run's MR as opened.
type mrForge struct{ forgetest.BaseFake }

func (*mrForge) GetMergeRequest(_ context.Context, _, iid int64) (forge.MergeRequest, error) {
	return forge.MergeRequest{IID: iid, State: forge.MRStateOpened}, nil
}

// PRD #4173 M4 acceptance, last clause (D10), the move BEFORE the write: a run on a
// repo linked to YouTrack whose issue was moved to another project completes. The
// lifecycle's issue write is skipped (no write request reaches YouTrack), the move
// marker is cleared instead of being retried to the give-up window, and the code-host
// side (the MR the run opened, recorded by the board-free watch) still completes.
// A move between the check and the write is not tested: YouTrack has no conditional
// write, so that window is accepted (PRD #4173).
//
// Skipped unless UZI_TEST_DATABASE_URL points at a throwaway Postgres.
func TestMovedLinkedIssueSkipsTheWriteAndTheCodeHostSideCompletesLiveDB(t *testing.T) {
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
	key := make([]byte, secretbox.KeySize)
	for i := range key {
		key[i] = byte(i + 1)
	}
	box, err := secretbox.New(key)
	if err != nil {
		t.Fatal(err)
	}

	srv := youtracktest.NewServer(t)
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	saved := def.TLSClientConfig
	def.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	t.Cleanup(func() { def.TLSClientConfig = saved })

	rec := forgetest.NewRecorder(&mrForge{})
	svc := forgesvc.NewWithForgeBuilder(q, box, 5*time.Second, nil, func(forge.Type, string, string, time.Duration) (forge.Forge, error) { return rec, nil })
	itSvc := issuetrackersvc.New(q, box, func(u string) bool { return u == srv.URL }, issuetracker.New)
	svc.SetIssueTrackerDrivers(itSvc)
	sealed, err := svc.EncryptToken("forge-token")
	if err != nil {
		t.Fatal(err)
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	user, conn, repo, worker, runID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	exec(`INSERT INTO users (id, email, password_hash) VALUES ($1, $2, 'x')`, user, fmt.Sprintf("moved-%s@e2e", user))
	exec(`INSERT INTO forge_connections (id, user_id, forge_type, base_url, bot_username, bot_forge_user_id, token_ciphertext)
	      VALUES ($1, $2, 'gitlab', $3, 'bot', 1, $4)`, conn, user, "https://"+uuid.NewString()+".forge.test", sealed)
	yt, err := itSvc.Connect(ctx, user, "youtrack", srv.URL, srv.Token)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled,
	        issue_tracker_connection_id, issue_tracker_project_id, issue_tracker_project_short, eligibility_tag_id, eligibility_tag_name, issue_tracker_linked_at)
	      VALUES ($1, $2, 77, $3, 'https://forge.test/g/l', 'main', true, $4, $5, $6, $7, $8, now())`,
		repo, conn, "g/moved-"+repo.String(), yt.ID, youtracktest.ProjectID, youtracktest.ProjectShortName, youtracktest.EligibilityTagID, youtracktest.EligibilityTagName)
	if err := forgesvc.EnsureBoardColumnRows(ctx, q, repo); err != nil {
		t.Fatal(err)
	}
	issueN := srv.SeedIssue("a youtrack card", "body", youtracktest.EligibilityTagID)
	exec(`INSERT INTO issues (repo_id, forge_issue_iid, title, state, labels, web_url, forge_updated_at, synced_at)
	      VALUES ($1, $2, 't', 'opened', '[]', 'https://yt.test/i', now(), now())`, repo, issueN)
	sum := sha256.Sum256([]byte("moved-worker-" + worker.String()))
	exec(`INSERT INTO workers(id,user_id,name,token_hash,status) VALUES($1,$2,'moved-worker',$3,'online')`, worker, user, sum[:])
	exec(`INSERT INTO runs (id, user_id, repo_id, kind, issue_iid, issue_title, issue_description, branch, status, auto_approve,
	        origin_column, worker_id, claim_generation)
	      VALUES ($1, $2, $3, 'issue', $4, 't', 'd', $5, 'running', false, '', $6, 1)`, runID, user, repo, issueN, "agent/issue-"+runID.String(), worker)

	// The issue moves to another project BEFORE the run completes.
	srv.MoveIssue(issueN, youtracktest.OtherProjectID)
	srv.ResetRequests()
	tr, err := svc.IssueTrackerForRepo(ctx, user, repo)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.GetIssue(ctx, 77, issueN); !forgesvc.IssueWriteSkippable(err) {
		t.Fatalf("a read of the moved issue = %v, want a skippable not-found", err)
	}
	srv.ResetRequests()

	// The run completes with its MR: the completion write stamps the move marker.
	rows, err := q.SetRunCompleted(ctx, store.SetRunCompletedParams{
		MrIid: pgtype.Int8{Int64: 13, Valid: true}, ID: runID, WorkerID: pgtype.UUID{Bytes: worker, Valid: true},
	})
	if err != nil || rows != 1 {
		t.Fatalf("SetRunCompleted rows=%d err=%v", rows, err)
	}
	marker := func() (pending bool, boardColumn pgtype.Text, mrState pgtype.Text) {
		t.Helper()
		var since pgtype.Timestamptz
		if err := pool.QueryRow(ctx, `SELECT move_pending_since, board_column, mr_state FROM runs WHERE id = $1`, runID).Scan(&since, &boardColumn, &mrState); err != nil {
			t.Fatal(err)
		}
		return since.Valid, boardColumn, mrState
	}
	if pending, _, _ := marker(); !pending {
		t.Fatal("the completion did not stamp the move marker")
	}

	l := New(q, svc, "https://uzi.test")
	l.SetIssueSources(svc)
	l.notifyOnce(ctx, runID, "completed")

	for _, r := range srv.Requests() {
		if r.Method != http.MethodGet {
			t.Fatalf("an issue write reached YouTrack for a moved issue: %+v", r)
		}
	}
	pending, boardColumn, _ := marker()
	if pending {
		t.Fatal("the move marker was left set: it would be retried to the give-up window")
	}
	if boardColumn.Valid {
		t.Fatalf("board_column recorded as %q for a skipped move", boardColumn.String)
	}

	// The code-host side is independent of the issue: the board-free lane records the MR.
	if err := svc.SyncBoardFreeMRStates(ctx, repo, 77, rec); err != nil {
		t.Fatal(err)
	}
	if _, _, mrState := marker(); !mrState.Valid || mrState.String != forge.MRStateOpened {
		t.Fatalf("mr_state = %+v, want the MR recorded as opened", mrState)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1`, runID).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("status = %q, %v", status, err)
	}
}
