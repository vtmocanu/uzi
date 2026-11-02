package store_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestListCreatedIssuesForRunLiveDB exercises the SQL ownership boundaries,
// confirmed positive coordinates and deterministic proposal order.
func TestListCreatedIssuesForRunLiveDB(t *testing.T) {
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

	owner, foreign := uuid.New(), uuid.New()
	conn, foreignConn := uuid.New(), uuid.New()
	repo, otherRepo, foreignRepo := uuid.New(), uuid.New(), uuid.New()
	for _, user := range []uuid.UUID{owner, foreign} {
		mustExec(ctx, t, pool, `INSERT INTO users(id,email,password_hash) VALUES ($1,$2,'x')`,
			user, fmt.Sprintf("created-issues-%s@e2e", user))
	}
	for _, pair := range [][2]uuid.UUID{{conn, owner}, {foreignConn, foreign}} {
		mustExec(ctx, t, pool, `INSERT INTO forge_connections
 (id,user_id,forge_type,base_url,bot_username,bot_forge_user_id,token_ciphertext)
 VALUES ($1,$2,'gitlab','https://forge.e2e','bot',1,$3)`, pair[0], pair[1], []byte{0x1})
	}
	for i, pair := range [][2]uuid.UUID{{repo, conn}, {otherRepo, conn}, {foreignRepo, foreignConn}} {
		mustExec(ctx, t, pool, `INSERT INTO repos
 (id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 VALUES ($1,$2,$3,'g/r','https://forge.e2e/g/r','main',true)`, pair[0], pair[1], int64(i+1))
	}
	run, otherRun, wrongConnection, wrongRunOwner := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	for _, fixture := range []struct{ id, user, repo uuid.UUID }{
		{run, owner, repo}, {otherRun, owner, repo},
		{wrongConnection, owner, foreignRepo}, {wrongRunOwner, foreign, repo},
	} {
		mustExec(ctx, t, pool, `INSERT INTO runs
 (id,user_id,repo_id,kind,trigger_source,status,issue_title,issue_description)
 VALUES ($1,$2,$3,'prompt','schedule','completed','idea','body')`, fixture.id, fixture.user, fixture.repo)
	}

	first, second := uuid.New(), uuid.New()
	if first.String() > second.String() {
		first, second = second, first
	}
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, fixture := range []struct {
		id, run, repo uuid.UUID
		title, status string
		iid           any
		at            time.Time
	}{
		// Reverse insertion order exercises both created_at and the UUID tie-break.
		{uuid.New(), run, repo, "latest", "confirmed", int64(13), stamp.Add(time.Second)},
		{second, run, repo, "second", "confirmed", int64(12), stamp},
		{first, run, repo, "first", "confirmed", int64(11), stamp},
		{uuid.New(), run, repo, "pending", "pending", int64(90), stamp},
		{uuid.New(), run, repo, "confirming", "confirming", int64(91), stamp},
		{uuid.New(), run, repo, "dismissed", "dismissed", int64(92), stamp},
		{uuid.New(), run, repo, "unstamped", "confirmed", nil, stamp},
		{uuid.New(), run, repo, "zero", "confirmed", int64(0), stamp},
		{uuid.New(), run, repo, "negative", "confirmed", int64(-1), stamp},
		{uuid.New(), run, otherRepo, "wrong proposal repo", "confirmed", int64(93), stamp},
		{uuid.New(), otherRun, repo, "wrong proposal run", "confirmed", int64(94), stamp},
		{uuid.New(), wrongConnection, foreignRepo, "foreign connection", "confirmed", int64(95), stamp},
		{uuid.New(), wrongRunOwner, repo, "foreign run owner", "confirmed", int64(96), stamp},
	} {
		mustExec(ctx, t, pool, `INSERT INTO issue_proposals
 (id,run_id,repo_id,title,description,status,created_issue_iid,created_at)
 VALUES ($1,$2,$3,$4,'body',$5,$6,$7)`,
			fixture.id, fixture.run, fixture.repo, fixture.title, fixture.status, fixture.iid, fixture.at)
	}
	// A failed best-effort stamp cannot manufacture a confirmed output on read.
	pendingID := uuid.New()
	mustExec(ctx, t, pool, `INSERT INTO issue_proposals (id,run_id,repo_id,title,description,status)
 VALUES ($1,$2,$3,'failed stamp','body','pending')`, pendingID, run, repo)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := q.StampFiledProposal(cancelled, store.StampFiledProposalParams{ID: pendingID, Iid: pgtype.Int8{Int64: 97, Valid: true}}); err == nil {
		t.Fatal("cancelled stamp unexpectedly succeeded")
	}
	var pendingStatus string
	var pendingIID pgtype.Int8
	if err := pool.QueryRow(ctx, `SELECT status,created_issue_iid FROM issue_proposals WHERE id=$1`, pendingID).Scan(&pendingStatus, &pendingIID); err != nil {
		t.Fatal(err)
	}
	if pendingStatus != "pending" || pendingIID.Valid {
		t.Fatalf("failed stamp changed pending row: status=%s iid=%v", pendingStatus, pendingIID)
	}
	rows, err := q.ListCreatedIssuesForRun(ctx, store.ListCreatedIssuesForRunParams{
		RunID: run, RepoID: pgtype.UUID{Bytes: repo, Valid: true}, UserID: owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []store.ListCreatedIssuesForRunRow{
		{RepoID: repo, Iid: 11, Title: "first", WebUrl: "https://forge.e2e/g/r", ForgeType: "gitlab"},
		{RepoID: repo, Iid: 12, Title: "second", WebUrl: "https://forge.e2e/g/r", ForgeType: "gitlab"},
		{RepoID: repo, Iid: 13, Title: "latest", WebUrl: "https://forge.e2e/g/r", ForgeType: "gitlab"},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("ordered confirmed outputs = %+v, want %+v", rows, want)
	}
	for _, tc := range []struct {
		name            string
		run, repo, user uuid.UUID
	}{
		{"wrong requested run", uuid.New(), repo, owner},
		{"wrong requested repo", run, otherRepo, owner},
		{"wrong requested owner", run, repo, foreign},
		{"connection ownership", wrongConnection, foreignRepo, owner},
		{"run ownership", wrongRunOwner, repo, owner},
		{"foreign run cannot authorize connection", wrongRunOwner, repo, foreign},
		{"foreign connection cannot authorize run", wrongConnection, foreignRepo, foreign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := q.ListCreatedIssuesForRun(ctx, store.ListCreatedIssuesForRunParams{
				RunID: tc.run, RepoID: pgtype.UUID{Bytes: tc.repo, Valid: true}, UserID: tc.user,
			})
			if err != nil || len(rows) != 0 {
				t.Fatalf("rows = %+v, err = %v; want no outputs", rows, err)
			}
		})
	}
}
