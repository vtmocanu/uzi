package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/issuetracker/youtracktest"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// issueKeyFixture is the linked repo of newLinkedHandlerFixture (project short name
// youtracktest.ProjectShortName) plus a second repo on the same forge connection that is not
// linked to an issue tracker, with the same run, schedule and finding shapes on each. Every
// payload that PRD #4173 M6 gives an issue key must carry it for the linked repo and omit it
// for the plain one.
type issueKeyFixture struct {
	*linkedHandlerFixture
	tb                    *testing.T
	plain                 uuid.UUID
	linkedRun, plainRun   uuid.UUID
	linkedSched, plainSch uuid.UUID
	linkedPromptRun       uuid.UUID
	plainPromptRun        uuid.UUID
}

const keyIssueIID = 7

func (f *issueKeyFixture) key(iid int64) string {
	return youtracktest.ProjectShortName + "-" + strconv.FormatInt(iid, 10)
}

func newIssueKeyFixture(t *testing.T) *issueKeyFixture {
	t.Helper()
	lf := newLinkedHandlerFixture(t, false)
	f := &issueKeyFixture{linkedHandlerFixture: lf, tb: t, plain: uuid.New()}
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := lf.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	exec(`INSERT INTO repos (id, connection_id, forge_project_id, path_with_namespace, web_url, default_branch, enabled)
	      SELECT $1, connection_id, 78, $2, 'https://forge.test/g/plain', 'main', true FROM repos WHERE id = $3`,
		f.plain, "g/plain-"+f.plain.String(), f.repo)
	newRun := func(repo uuid.UUID, kind, trigger string, iid any) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO runs (id, user_id, repo_id, kind, trigger_source, issue_iid, status, issue_title, issue_description)
		      VALUES ($1, $2, $3, $4, $5, $6, 'queued', 'a run', 'body')`, id, f.user.ID, repo, kind, trigger, iid)
		return id
	}
	f.linkedRun = newRun(f.repo, "issue", "manual", int64(keyIssueIID))
	f.plainRun = newRun(f.plain, "issue", "manual", int64(keyIssueIID))
	f.linkedPromptRun = newRun(f.repo, "prompt", "schedule", nil)
	f.plainPromptRun = newRun(f.plain, "prompt", "schedule", nil)
	for _, p := range []struct{ run, repo uuid.UUID }{{f.linkedPromptRun, f.repo}, {f.plainPromptRun, f.plain}} {
		exec(`INSERT INTO issue_proposals (run_id, repo_id, title, description, status, created_issue_iid)
		      VALUES ($1, $2, 'idea', 'body', 'confirmed', 12)`, p.run, p.repo)
	}
	for _, s := range []struct {
		id   *uuid.UUID
		repo uuid.UUID
	}{{&f.linkedSched, f.repo}, {&f.plainSch, f.plain}} {
		*s.id = uuid.New()
		exec(`INSERT INTO run_schedules (id, user_id, repo_id, target, issue_iid, timing, cron_expr, timezone, next_fire_at, auto_approve, status, enabled)
		      VALUES ($1, $2, $3, 'issue', $4, 'recurring', '0 3 * * *', 'UTC', now() + interval '1 day', true, 'active', true)`,
			*s.id, f.user.ID, s.repo, int64(keyIssueIID))
	}
	return f
}

func (f *issueKeyFixture) get(handler http.HandlerFunc, target string, params map[string]string) map[string]any {
	f.tb.Helper()
	return f.getAs(f.user, handler, target, params)
}

func (f *issueKeyFixture) getAs(user store.User, handler http.HandlerFunc, target string, params map[string]string) map[string]any {
	f.tb.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), user), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		f.tb.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		f.tb.Fatalf("decode %s: %v", target, err)
	}
	return out
}

// rowByID finds the object with the given id in a JSON array of objects.
func issueKeyRowByID(t *testing.T, rows any, id uuid.UUID) map[string]any {
	t.Helper()
	list, _ := rows.([]any)
	for _, r := range list {
		if m, ok := r.(map[string]any); ok && m["id"] == id.String() {
			return m
		}
	}
	t.Fatalf("no row %s in %v", id, rows)
	return nil
}

// wantKey asserts payload[field] is want, or absent when want is "".
func wantIssueKey(t *testing.T, what string, payload map[string]any, field, want string) {
	t.Helper()
	got, present := payload[field]
	if want == "" {
		if present {
			t.Errorf("%s: %s = %v, want it omitted", what, field, got)
		}
		return
	}
	if got != want {
		t.Errorf("%s: %s = %v, want %q", what, field, got, want)
	}
}

func TestRunPayloadsCarryTheLinkedIssueKeyLiveDB(t *testing.T) {
	f := newIssueKeyFixture(t)
	want := f.key(keyIssueIID)

	for _, view := range []string{"", "?view=summary"} {
		body := f.get(f.h.ListRuns, "/api/runs"+view, nil)
		wantIssueKey(t, "list"+view+" linked", issueKeyRowByID(t, body["runs"], f.linkedRun), "issue_key", want)
		wantIssueKey(t, "list"+view+" plain", issueKeyRowByID(t, body["runs"], f.plainRun), "issue_key", "")
		wantIssueKey(t, "list"+view+" prompt", issueKeyRowByID(t, body["runs"], f.linkedPromptRun), "issue_key", "")
	}

	run := f.get(f.h.GetRun, "/api/runs/x", map[string]string{"id": f.linkedRun.String()})["run"].(map[string]any)
	wantIssueKey(t, "GET run linked", run, "issue_key", want)
	run = f.get(f.h.GetRun, "/api/runs/x", map[string]string{"id": f.plainRun.String()})["run"].(map[string]any)
	wantIssueKey(t, "GET run plain", run, "issue_key", "")

	// The create-style responses build the run through the same single-run helper.
	for id, wantFor := range map[uuid.UUID]string{f.linkedRun: want, f.plainRun: ""} {
		stored, err := f.h.wsvc.GetRunForViewer(context.Background(), f.user.ID, false, id)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(f.h.createdRunBody(context.Background(), f.h.runDTO(context.Background(), stored)))
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Run map[string]any `json:"run"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
		wantIssueKey(t, "created run body", out.Run, "issue_key", wantFor)
	}

	// The admin lists read the same short name from their own SQL.
	admin := f.user
	admin.IsAdmin = true
	for _, view := range []string{"", "?view=summary"} {
		body := f.getAs(admin, f.h.AdminListRuns, "/api/admin/runs"+view, nil)
		wantIssueKey(t, "admin list"+view+" linked", issueKeyRowByID(t, body["runs"], f.linkedRun), "issue_key", want)
		wantIssueKey(t, "admin list"+view+" plain", issueKeyRowByID(t, body["runs"], f.plainRun), "issue_key", "")
	}
	svc := f.h.wsvc
	rows, err := svc.ListRunsAdminDebug(context.Background(), &f.user.ID, nil, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	shorts := map[uuid.UUID]string{}
	for _, r := range rows {
		shorts[r.Run.ID] = r.IssueTrackerProjectShort
	}
	if shorts[f.linkedRun] != youtracktest.ProjectShortName || shorts[f.plainRun] != "" {
		t.Errorf("admin debug list project short names = %q / %q, want %q / empty", shorts[f.linkedRun], shorts[f.plainRun], youtracktest.ProjectShortName)
	}
	active, err := svc.ListActiveRunsAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range active {
		if r.Run.ID == f.linkedRun {
			found = true
			if r.IssueTrackerProjectShort != youtracktest.ProjectShortName {
				t.Errorf("admin active list project short name = %q", r.IssueTrackerProjectShort)
			}
		}
	}
	if !found {
		t.Error("the linked run is missing from the admin active list")
	}

	// Created issues ride the run detail of a scheduled prompt run. A linked repo's created
	// issue lives in the issue tracker, so it carries no code forge URL: the forge URL with the
	// tracker's number would open an unrelated forge issue (PR #4757 review). The plain repo's
	// created issue keeps its forge URL.
	for _, tc := range []struct {
		run     uuid.UUID
		key     string
		wantURL string
	}{
		{f.linkedPromptRun, f.key(12), ""},
		{f.plainPromptRun, "", "https://forge.test/g/plain/-/issues/12"},
	} {
		run := f.get(f.h.GetRun, "/api/runs/x", map[string]string{"id": tc.run.String()})["run"].(map[string]any)
		issues, _ := run["created_issues"].([]any)
		if len(issues) != 1 {
			t.Fatalf("created_issues = %v, want one", run["created_issues"])
		}
		issue := issues[0].(map[string]any)
		wantIssueKey(t, "created issue", issue, "issue_key", tc.key)
		if got, _ := issue["web_url"].(string); got != tc.wantURL {
			t.Errorf("created issue web_url = %q, want %q", got, tc.wantURL)
		}
	}
}

func TestSchedulePayloadCarriesTheLinkedIssueKeyLiveDB(t *testing.T) {
	f := newIssueKeyFixture(t)
	for id, wantFor := range map[uuid.UUID]string{f.linkedSched: f.key(keyIssueIID), f.plainSch: ""} {
		sched := f.get(f.h.GetSchedule, "/api/schedules/x", map[string]string{"id": id.String()})
		wantIssueKey(t, "schedule", sched, "issue_key", wantFor)
	}
}

func TestFindingPayloadsCarryTheFiledIssueKeyLiveDB(t *testing.T) {
	f := newIssueKeyFixture(t)
	ctx := context.Background()
	disp := map[uuid.UUID]uuid.UUID{} // repo -> disposition
	for _, repo := range []uuid.UUID{f.repo, f.plain} {
		var id uuid.UUID
		if err := f.pool.QueryRow(ctx, `INSERT INTO finding_dispositions (user_id, repo_id, location, content_hash, last_title, status, filed_issue_iid, filed_issue_url)
			VALUES ($1, $2, 'a.go#x', 'h', 't', 'filed', 9, 'https://x/9') RETURNING id`, f.user.ID, repo).Scan(&id); err != nil {
			t.Fatal(err)
		}
		disp[repo] = id
	}
	wantFor := map[uuid.UUID]string{disp[f.repo]: f.key(9), disp[f.plain]: ""}

	backlog, err := f.h.wsvc.FindingsBacklog(ctx, f.user.ID, "all", uuid.Nil, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlog.Findings) != 2 {
		t.Fatalf("backlog findings = %d, want 2", len(backlog.Findings))
	}
	for _, fd := range backlog.Findings {
		id, _ := uuid.Parse(fd.DispositionID)
		if fd.FiledIssueKey != wantFor[id] {
			t.Errorf("backlog finding %s filed_issue_key = %q, want %q", id, fd.FiledIssueKey, wantFor[id])
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/api/findings/done", strings.NewReader(`{"ids":["`+disp[f.repo].String()+`","`+disp[f.plain].String()+`"]}`))
	req = req.WithContext(mw.ContextWithUser(req.Context(), f.user))
	rec := httptest.NewRecorder()
	f.h.BulkMarkFindingsDone(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("bulk done = %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Findings []map[string]any `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Findings) != 2 {
		t.Fatalf("bulk done findings = %d, want 2", len(out.Findings))
	}
	for _, fd := range out.Findings {
		id, _ := uuid.Parse(fd["disposition_id"].(string))
		wantIssueKey(t, "bulk done finding", fd, "filed_issue_key", wantFor[id])
	}
}

// Filing a finding on the linked repo answers with the key of the issue it created (PRD #4173
// M6). The issue tracker is the YouTrack fake. The plain repo leg is not driven here: the
// fixture's forge fake stubs no CreateIssue, and an unlinked repo's omission is
// the empty-key branch pinned by TestLinkedIssueKeyIsEmptyOffALinkedRepo.
func TestFileFindingResultCarriesTheIssueKeyLiveDB(t *testing.T) {
	f := newIssueKeyFixture(t)
	ctx := context.Background()
	file := func(repo, run uuid.UUID, location string) map[string]any {
		t.Helper()
		finding, err := f.h.q.InsertFinding(ctx, store.InsertFindingParams{
			RunID: run, UserID: f.user.ID, RepoID: repo,
			Location: location, Title: "t", DescriptionMd: "d", Labels: []byte(`[]`), Confidence: "high",
		})
		if err != nil {
			t.Fatalf("InsertFinding: %v", err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO finding_dispositions (user_id, repo_id, location, content_hash, last_title)
			VALUES ($1, $2, $3, 'h', 't')`, f.user.ID, repo, location); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/findings/x/issue", strings.NewReader("{}"))
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", finding.ID.String())
		req = req.WithContext(context.WithValue(mw.ContextWithUser(req.Context(), f.user), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		f.h.FileFinding(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("file finding = %d: %s", rec.Code, rec.Body.String())
		}
		var out struct {
			Issue map[string]any `json:"issue"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Issue
	}
	linked := file(f.repo, f.linkedRun, "a.go#linked")
	iid, _ := linked["iid"].(float64)
	wantIssueKey(t, "linked file result", linked, "issue_key", f.key(int64(iid)))
}
