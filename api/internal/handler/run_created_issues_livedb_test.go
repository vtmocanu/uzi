package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Only this projection is intercepted: authentication, run authorization and
// every unrelated query still execute against the real router/database.
type createdIssuesLiveDB struct {
	store.DBTX
	calls int
	fail  bool
}

func (db *createdIssuesLiveDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "-- name: ListCreatedIssuesForRun") {
		db.calls++
		if db.fail {
			return nil, errors.New("created issue metadata unavailable")
		}
	}
	return db.DBTX.Query(ctx, sql, args...)
}

func TestCreatedIssuesQueryAndRouterLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	ctx := context.Background()
	owner, foreign, admin := cliSeedUser(t, pool, false), cliSeedUser(t, pool, false), cliSeedUser(t, pool, true)
	repo, otherRepo, foreignRepo := cliSeedOwnedRepo(t, pool, owner), uuid.New(), cliSeedOwnedRepo(t, pool, foreign)
	cliMustExec(t, pool, `INSERT INTO repos(id,connection_id,forge_project_id,path_with_namespace,web_url,default_branch,enabled)
 SELECT $1,connection_id,2,'g/other','https://forge.e2e/g/other','main',true FROM repos WHERE id=$2`, otherRepo, repo)
	newRun := func(user, repoID uuid.UUID, kind, trigger string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		var repoArg any = repoID
		var branch any
		if kind == "chat" {
			repoArg = nil
		}
		if kind == "task" {
			branch = "task/test"
		}
		cliMustExec(t, pool, `INSERT INTO runs (id,user_id,repo_id,kind,trigger_source,status,issue_title,issue_description,branch)
   VALUES ($1,$2,$3,$4,$5,'completed','scheduled idea','body',$6)`, id, user, repoArg, kind, trigger, branch)
		return id
	}
	runID := newRun(owner, repo, "prompt", "schedule")
	otherRun := newRun(owner, repo, "prompt", "schedule")
	// Deliberately asymmetric ownership: neither side alone authorizes the join.
	wrongConnection := newRun(owner, foreignRepo, "prompt", "schedule")
	wrongRunOwner := newRun(foreign, repo, "prompt", "schedule")
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	proposal := func(id, runID, repoID uuid.UUID, title, status string, iid any, at time.Time) {
		t.Helper()
		cliMustExec(t, pool, `INSERT INTO issue_proposals (id,run_id,repo_id,title,description,status,created_issue_iid,created_at)
    VALUES ($1,$2,$3,$4,'body',$5,$6,$7)`, id, runID, repoID, title, status, iid, at)
	}
	firstID, secondID := uuid.New(), uuid.New()
	if firstID.String() > secondID.String() {
		firstID, secondID = secondID, firstID
	}
	// Insert reverse UUID order to prove the tie-break is SQL ordering, not insertion order.
	proposal(secondID, runID, repo, "second", "confirmed", int64(12), stamp)
	proposal(firstID, runID, repo, "first", "confirmed", int64(11), stamp)
	proposal(uuid.New(), runID, repo, "later duplicate", "confirmed", int64(11), stamp.Add(time.Second))
	proposal(uuid.New(), runID, repo, "latest", "confirmed", int64(13), stamp.Add(2*time.Second))
	for _, status := range []string{"pending", "confirming", "dismissed"} {
		proposal(uuid.New(), runID, repo, status, status, int64(98), stamp)
	}
	for _, iid := range []any{nil, int64(0), int64(-2)} {
		proposal(uuid.New(), runID, repo, "invalid coordinate", "confirmed", iid, stamp)
	}
	proposal(uuid.New(), runID, otherRepo, "wrong proposal repo", "confirmed", int64(99), stamp)
	proposal(uuid.New(), otherRun, repo, "wrong proposal run", "confirmed", int64(97), stamp)
	proposal(uuid.New(), wrongConnection, foreignRepo, "foreign connection", "confirmed", int64(96), stamp)
	proposal(uuid.New(), wrongRunOwner, repo, "foreign run owner", "confirmed", int64(95), stamp)
	q := store.New(pool)
	params := store.ListCreatedIssuesForRunParams{RunID: runID, RepoID: pgtype.UUID{Bytes: repo, Valid: true}, UserID: owner}
	rows, err := q.ListCreatedIssuesForRun(ctx, params)
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, row := range rows {
		titles = append(titles, row.Title)
	}
	if !reflect.DeepEqual(titles, []string{"first", "second", "later duplicate", "latest"}) {
		t.Fatalf("ordered confirmed rows=%v", titles)
	}
	for _, tc := range []struct {
		name            string
		run, repo, user uuid.UUID
	}{
		{"wrong requested run", uuid.New(), repo, owner},
		{"wrong requested repo", runID, otherRepo, owner},
		{"wrong requested owner", runID, repo, foreign},
		{"connection owner differs", wrongConnection, foreignRepo, owner},
		{"run owner differs", wrongRunOwner, repo, foreign},
		{"run owner cannot be bypassed by connection owner", wrongRunOwner, repo, owner},
		{"connection owner cannot bypass run owner", wrongConnection, foreignRepo, foreign},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := q.ListCreatedIssuesForRun(ctx, store.ListCreatedIssuesForRunParams{RunID: tc.run, RepoID: pgtype.UUID{Bytes: tc.repo, Valid: true}, UserID: tc.user})
			if err != nil || len(rows) != 0 {
				t.Fatalf("rows=%+v err=%v", rows, err)
			}
		})
	}
	db := &createdIssuesLiveDB{DBTX: pool}
	h.q = store.New(db)
	ownerJWT, adminJWT, foreignJWT := cliMintJWT(t, pool, owner), cliMintJWT(t, pool, admin), cliMintJWT(t, pool, foreign)
	path := "/api/runs/" + runID.String()
	for _, tc := range []struct {
		name, jwt     string
		status, calls int
	}{
		{"owner", ownerJWT, 200, 1}, {"admin", adminJWT, 200, 1}, {"foreign", foreignJWT, 404, 0}, {"unauthenticated", "", 401, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := db.calls
			rec := cookieReq(t, router, http.MethodGet, path, tc.jwt, "")
			if rec.Code != tc.status || db.calls-before != tc.calls {
				t.Fatalf("status=%d queries=%d body=%s", rec.Code, db.calls-before, rec.Body.String())
			}
			if tc.status != 200 {
				return
			}
			var body struct{ Run apitypes.RunDTO }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			want := []apitypes.CreatedIssueDTO{
				{IID: 11, Title: "first", WebURL: "https://forge.e2e/g/r/-/issues/11"},
				{IID: 12, Title: "second", WebURL: "https://forge.e2e/g/r/-/issues/12"},
				{IID: 13, Title: "latest", WebURL: "https://forge.e2e/g/r/-/issues/13"},
			}
			if !reflect.DeepEqual(body.Run.CreatedIssues, want) {
				t.Fatalf("issues=%+v", body.Run.CreatedIssues)
			}
		})
	}
	// A retained schedule FK qualifies even without trigger_source. Later edits do
	// not redefine the historical result, and deletion retains scheduled provenance.
	scheduleID := uuid.New()
	cliMustExec(t, pool, `INSERT INTO run_schedules(id,user_id,repo_id,target,prompt,timing,run_at,output_mode)
   VALUES ($1,$2,$3,'prompt','idea','once',now(),'issues')`, scheduleID, owner, repo)
	cliMustExec(t, pool, `UPDATE runs SET schedule_id=$2,trigger_source='manual' WHERE id=$1`, runID, scheduleID)
	for _, phase := range []string{"linked", "changed", "deleted"} {
		if phase == "changed" {
			cliMustExec(t, pool, `UPDATE run_schedules SET output_mode='mr',prompt='new prompt',enabled=false WHERE id=$1`, scheduleID)
		}
		if phase == "deleted" {
			cliMustExec(t, pool, `UPDATE runs SET trigger_source='schedule' WHERE id=$1`, runID)
			cliMustExec(t, pool, `DELETE FROM run_schedules WHERE id=$1`, scheduleID)
		}
		rec := cookieReq(t, router, http.MethodGet, path, ownerJWT, "")
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "\"created_issues\"") {
			t.Fatalf("%s: %d %s", phase, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct{ kind, trigger string }{{"prompt", "manual"}, {"chat", "schedule"}, {"task", "schedule"}} {
		id := newRun(owner, repo, tc.kind, tc.trigger)
		proposal(uuid.New(), id, repo, "ineligible", "confirmed", int64(42), stamp)
		p := store.ListCreatedIssuesForRunParams{RunID: id, RepoID: pgtype.UUID{Bytes: repo, Valid: true}, UserID: owner}
		rows, err := q.ListCreatedIssuesForRun(ctx, p)
		if err != nil || len(rows) != 0 {
			t.Fatalf("%+v query: rows=%v err=%v", tc, rows, err)
		}
		before := db.calls
		rec := cookieReq(t, router, http.MethodGet, "/api/runs/"+id.String(), ownerJWT, "")
		if rec.Code != 200 || db.calls != before || strings.Contains(rec.Body.String(), "\"created_issues\"") {
			t.Fatalf("%+v: %d %s queries=%d", tc, rec.Code, rec.Body.String(), db.calls-before)
		}
	}
	// Real list routes must not execute the detail projection.
	for _, listPath := range []string{"/api/runs", "/api/runs?view=summary", "/api/admin/runs", "/api/admin/runs?view=summary"} {
		before := db.calls
		jwt := ownerJWT
		if strings.HasPrefix(listPath, "/api/admin/") {
			jwt = adminJWT
		}
		rec := cookieReq(t, router, http.MethodGet, listPath, jwt, "")
		if rec.Code != 200 || db.calls != before || strings.Contains(rec.Body.String(), "\"created_issues\"") {
			t.Fatalf("list %s: %d %s", listPath, rec.Code, rec.Body.String())
		}
	}
	// Authorization of the run alone must not expose proposals through a foreign connection.
	for _, tc := range []struct {
		name string
		id   uuid.UUID
		jwt  string
	}{
		{"owner foreign connection", wrongConnection, ownerJWT},
		{"admin foreign connection", wrongConnection, adminJWT},
		{"foreign run owned repository", wrongRunOwner, foreignJWT},
		{"admin foreign run owned repository", wrongRunOwner, adminJWT},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := db.calls
			rec := cookieReq(t, router, http.MethodGet, "/api/runs/"+tc.id.String(), tc.jwt, "")
			if rec.Code != 200 || db.calls-before != 1 || strings.Contains(rec.Body.String(), "\"created_issues\"") {
				t.Fatalf("cross-owner decoration: status=%d queries=%d body=%s", rec.Code, db.calls-before, rec.Body.String())
			}
		})
	}
	db.fail = true
	before := db.calls
	rec := cookieReq(t, router, http.MethodGet, path, ownerJWT, "")
	if rec.Code != 200 || db.calls-before != 1 || strings.Contains(rec.Body.String(), "\"created_issues\"") {
		t.Fatalf("best effort: %d %s", rec.Code, rec.Body.String())
	}
}
