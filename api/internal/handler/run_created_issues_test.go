package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCreatedIssueURL(t *testing.T) {
	for _, tc := range []struct {
		base, forge string
		iid         int64
		want        string
	}{
		{"https://github.com/team/repo", "github", 12, "https://github.com/team/repo/issues/12"},
		{"https://forge.example:8443/host/team/sub/repo///", "gitlab", 9, "https://forge.example:8443/host/team/sub/repo/-/issues/9"},
		{"https://forge.example/prefix/a%20b/r%C3%A9po", "forgejo", 1, "https://forge.example/prefix/a%20b/r%C3%A9po/issues/1"},
		{"https://[::1]/g/r", "forgejo", 2, "https://[::1]/g/r/issues/2"},
		{"https://forge.example/g/r%2Egit", "gitlab", 3, "https://forge.example/g/r%2Egit/-/issues/3"},
	} {
		if got := createdIssueURL(tc.base, tc.forge, tc.iid); got != tc.want {
			t.Errorf("%q: got %q want %q", tc.base, got, tc.want)
		}
	}
	for _, base := range []string{
		"", "http://forge.example/g/r", "//forge.example/g/r", "https:///g/r",
		"https://user:pass@forge.example/g/r", "https://forge.example/g/r?x=1", "https://forge.example/g/r?",
		"https://forge.example/g/r#x", "https://forge.example/g/r#",
		"https://forge.example/g/../r", "https://forge.example/g/./r", "https://forge.example/g//r",
		"https://forge.example/g/%2e%2e", "https://forge.example/g%2fr",
		"https://forge.example/g%5cr", "https://forge.example/g/%00r", "https://forge.example/g/%0ar",
		"https://forge.example/g/%250ar", "https://forge.example/g/%", "https://forge.example/g/\\r",
		"https://forge.example/g/r\n", "https://forge.example", "https://forge.example/",
		"https://[example.com]/g/r", "https://bad_host/g/r", "https://-host/g/r", "https://forge.example:/g/r", "https://forge.example:99999/g/r",
	} {
		if got := createdIssueURL(base, "gitlab", 1); got != "" {
			t.Errorf("unsafe %q => %q", base, got)
		}
	}
	for _, forge := range []string{"", "GitLab", "gitea", "gitlab "} {
		if got := createdIssueURL("https://forge.example/g/r", forge, 1); got != "" {
			t.Errorf("forge %q => %q", forge, got)
		}
	}
	for _, iid := range []int64{0, -1} {
		if got := createdIssueURL("https://forge.example/g/r", "github", iid); got != "" {
			t.Errorf("iid %d => %q", iid, got)
		}
	}
}

type createdIssuesDB struct {
	runRepoPathDB
	rows                       []store.ListCreatedIssuesForRunRow
	queryErr, scanErr, lateErr error
	calls                      int
	args                       []any
}

func (db *createdIssuesDB) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	if !strings.Contains(query, "-- name: ListCreatedIssuesForRun") {
		return db.runRepoPathDB.Query(ctx, query, args...)
	}
	db.calls++
	db.args = args
	if db.queryErr != nil {
		return nil, db.queryErr
	}
	return &createdIssuesRows{values: db.rows, scanErr: db.scanErr, lateErr: db.lateErr}, nil
}

type createdIssuesRows struct {
	pgx.Rows
	values           []store.ListCreatedIssuesForRunRow
	index            int
	scanErr, lateErr error
}

func (r *createdIssuesRows) Next() bool { r.index++; return r.index <= len(r.values) }
func (r *createdIssuesRows) Close()     {}
func (r *createdIssuesRows) Err() error { return r.lateErr }
func (r *createdIssuesRows) Scan(dest ...any) error {
	if r.scanErr != nil && r.index > 1 {
		return r.scanErr
	}
	v := r.values[r.index-1]
	*dest[0].(*uuid.UUID) = v.RepoID
	*dest[1].(*int64) = v.Iid
	*dest[2].(*string) = v.Title
	*dest[3].(*string) = v.WebUrl
	*dest[4].(*string) = v.ForgeType
	return nil
}

func TestGetRunCreatedIssues(t *testing.T) {
	owner := store.User{ID: uuid.New()}
	repo := pgtype.UUID{Bytes: uuid.New(), Valid: true}
	for _, tc := range []struct {
		name                       string
		viewer                     store.User
		kind, trigger              string
		schedule, repoless, unauth bool
		failure                    string
		status, calls, count       int
	}{
		{name: "owner", viewer: owner, kind: "prompt", trigger: "schedule", status: 200, calls: 1, count: 2},
		{name: "admin", viewer: store.User{ID: uuid.New(), IsAdmin: true}, kind: "prompt", trigger: "schedule", status: 200, calls: 1, count: 2},
		{name: "foreign", viewer: store.User{ID: uuid.New()}, kind: "prompt", trigger: "schedule", status: 404},
		{name: "unauthenticated", unauth: true, kind: "prompt", trigger: "schedule", status: 401},
		{name: "schedule FK", viewer: owner, kind: "prompt", schedule: true, status: 200, calls: 1, count: 2},
		{name: "manual", viewer: owner, kind: "prompt", trigger: "manual", status: 200},
		{name: "chat", viewer: owner, kind: "chat", trigger: "schedule", status: 200},
		{name: "nonprompt", viewer: owner, kind: "task", trigger: "schedule", status: 200},
		{name: "repoless", viewer: owner, kind: "prompt", trigger: "schedule", repoless: true, status: 200},
		{name: "empty", viewer: owner, kind: "prompt", trigger: "schedule", failure: "empty", status: 200, calls: 1},
		{name: "query error", viewer: owner, kind: "prompt", trigger: "schedule", failure: "query", status: 200, calls: 1},
		{name: "scan error", viewer: owner, kind: "prompt", trigger: "schedule", failure: "scan", status: 200, calls: 1},
		{name: "late error", viewer: owner, kind: "prompt", trigger: "schedule", failure: "late", status: 200, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := store.Run{ID: uuid.New(), UserID: owner.ID, RepoID: repo, Status: "completed", Kind: tc.kind, TriggerSource: tc.trigger, IssueIid: pgtype.Int8{Int64: 55, Valid: true}, MrIid: pgtype.Int8{Int64: 66, Valid: true}}
			if tc.schedule {
				run.ScheduleID = pgtype.UUID{Bytes: uuid.New(), Valid: true}
			}
			if tc.repoless {
				run.RepoID = pgtype.UUID{}
			}
			db := &createdIssuesDB{rows: []store.ListCreatedIssuesForRunRow{
				{RepoID: uuid.UUID(repo.Bytes), Iid: 7, Title: "first", WebUrl: "https://forge.example/group/repo", ForgeType: "gitlab"},
				{RepoID: uuid.UUID(repo.Bytes), Iid: 7, Title: "duplicate"},
				{RepoID: uuid.UUID(repo.Bytes), Iid: 8, Title: "plain", WebUrl: "http://unsafe.example/g/r", ForgeType: "github"},
			}}
			switch tc.failure {
			case "empty":
				db.rows = nil
			case "query":
				db.queryErr = errors.New(strings.Repeat("x", 511) + "😀" + strings.Repeat("x", 2048))
			case "scan":
				db.scanErr = errors.New("scan failed")
			case "late":
				db.lateErr = errors.New("stream failed")
			}
			h := newRunsHandler(t, &runsStore{ownerID: owner.ID, run: run})
			h.q = store.New(db)
			req := runReq(tc.viewer, run.ID)
			if tc.unauth {
				req = req.WithContext(context.Background())
			}
			var logs bytes.Buffer
			previousLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previousLogger) })
			rec := httptest.NewRecorder()
			h.GetRun(rec, req)
			if tc.failure == "query" || tc.failure == "scan" || tc.failure == "late" {
				var entry struct {
					Message string `json:"msg"`
					RunID   string `json:"run_id"`
					Error   string `json:"error"`
				}
				found := false
				for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
					if err := json.Unmarshal(line, &entry); err != nil {
						t.Fatalf("invalid structured log: %s (%v)", line, err)
					}
					if entry.Message != "resolve run created issues" {
						continue
					}
					found = true
					if entry.RunID != run.ID.String() || entry.Error == "" || len(entry.Error) > 512 {
						t.Fatalf("projection error log lacks bounded error/run identity: %+v", entry)
					}
				}
				if !found {
					t.Fatal("missing projection error log")
				}
			}
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if db.calls != tc.calls {
				t.Fatalf("queries=%d want %d", db.calls, tc.calls)
			}
			if tc.calls > 0 && !reflect.DeepEqual(db.args, []any{run.ID, repo, owner.ID}) {
				t.Fatalf("wrong bindings: %v", db.args)
			}
			if tc.status != 200 {
				return
			}
			var body struct{ Run apitypes.RunDTO }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Run.IssueIID == nil || *body.Run.IssueIID != 55 || body.Run.MrIID == nil || *body.Run.MrIID != 66 {
				t.Fatalf("source issue/MR changed: issue=%v mr=%v", body.Run.IssueIID, body.Run.MrIID)
			}
			if len(body.Run.CreatedIssues) != tc.count {
				t.Fatalf("issues=%+v", body.Run.CreatedIssues)
			}
			if tc.count == 0 {
				if strings.Contains(rec.Body.String(), "\"created_issues\"") {
					t.Fatal("empty/failed decoration must be omitted")
				}
			} else if !reflect.DeepEqual(body.Run.CreatedIssues, []apitypes.CreatedIssueDTO{
				{IID: 7, Title: "first", WebURL: "https://forge.example/group/repo/-/issues/7"},
				{IID: 8, Title: "plain", WebURL: ""},
			}) {
				t.Fatalf("stable dedup/plain coordinate: %+v", body.Run.CreatedIssues)
			}
		})
	}
}

func TestListRunsCreatedIssuesOmitted(t *testing.T) {
	owner := store.User{ID: uuid.New(), IsAdmin: true}
	for _, admin := range []bool{false, true} {
		for _, suffix := range []string{"", "?view=summary"} {
			run := store.Run{ID: uuid.New(), UserID: owner.ID, Kind: "prompt", TriggerSource: "schedule", Status: "completed"}
			st := &runsStore{userRuns: []store.ListRunsForUserRow{{Run: run}}, activeRuns: []store.ListActiveRunsAllRow{{Run: run}}}
			h := newRunsHandler(t, st)
			db := &createdIssuesDB{}
			h.q = store.New(db)
			req := httptest.NewRequest(http.MethodGet, "/api/runs"+suffix, nil)
			req = req.WithContext(mw.ContextWithUser(req.Context(), owner))
			rec := httptest.NewRecorder()
			if admin {
				h.AdminListRuns(rec, req)
			} else {
				h.ListRuns(rec, req)
			}
			if rec.Code != 200 || !strings.Contains(rec.Body.String(), run.ID.String()) {
				t.Fatalf("list missing run: %d %s", rec.Code, rec.Body.String())
			}
			if db.calls != 0 || strings.Contains(rec.Body.String(), "\"created_issues\"") {
				t.Fatalf("list decorated: calls=%d body=%s", db.calls, rec.Body.String())
			}
		}
	}
}
