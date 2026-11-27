package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Issue #4776: the Spend response's top_runs entries carry issue_iid, issue_title and mr_iid
// on the owner route and on the admin routes. The body is decoded into plain maps so the test
// references no Go field of the response type.

type topProbe struct {
	kind      string
	repo      uuid.UUID // zero = no repo
	cost      string
	title     string
	issueIID  any
	mr        any
	ciFixMR   any
	continued any
}

// seed inserts one completed run with one metered usage row for user, at at.
func (f *anFixture) seedProbe(user uuid.UUID, at time.Time, p topProbe) uuid.UUID {
	f.t.Helper()
	id := uuid.New()
	n := anSeq.Add(1)
	var repo, branch, pipelineID, pipelineRef, jobType any
	if p.repo != uuid.Nil {
		repo = p.repo
	}
	switch p.kind {
	case "task":
		branch = fmt.Sprintf("uzi/task/%s", id)
	case "ci_fix":
		pipelineID, pipelineRef = n, fmt.Sprintf("refs/pipe/%d", n)
	case "job":
		jobType = "research"
	}
	cliMustExec(f.t, f.pool,
		`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, kind,
		                   created_at, status_since, harness, mr_iid, ci_fix_mr_iid, continued_issue_iid,
		                   branch, pipeline_id, pipeline_ref, job_type)
		 VALUES ($1, $2, $3, $4, $5, 'd', 'completed', $6, $7, $7, 'claude', $8, $9, $10, $11, $12, $13, $14)`,
		id, user, repo, p.issueIID, p.title, p.kind, at, p.mr, p.ciFixMR, p.continued, branch, pipelineID, pipelineRef, jobType)
	cliMustExec(f.t, f.pool,
		`INSERT INTO run_usage (run_id, session_id, model, lineage_epoch, lineage_index, usage_basis,
		     input_tokens, cache_read_tokens, cache_creation_tokens, output_tokens, cost_usd, cost_status)
		 VALUES ($1, $2, 'claude-sonnet', 0, 0, 'per_leg', 100, 50, 10, 20, $3::numeric, 'metered')`,
		id, fmt.Sprintf("an4776-%d", n), p.cost)
	return id
}

// rawTop decodes a 200 response's top_runs into maps and checks every identity key is present.
func rawTop(t *testing.T, label string, rec *httptest.ResponseRecorder) map[string]map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d %s, want 200", label, rec.Code, rec.Body.String())
	}
	var body struct {
		TopRuns []map[string]any `json:"top_runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: decode: %v", label, err)
	}
	out := map[string]map[string]any{}
	for _, e := range body.TopRuns {
		for _, k := range []string{"issue_iid", "issue_title", "mr_iid"} {
			if _, ok := e[k]; !ok {
				t.Fatalf("%s: top run %v is missing key %q", label, e["run_id"], k)
			}
		}
		out[fmt.Sprint(e["run_id"])] = e
	}
	return out
}

func topNum(t *testing.T, label string, e map[string]any, key string, want any) {
	t.Helper()
	got := e[key]
	if want == nil {
		if got != nil {
			t.Errorf("%s: %s = %v, want null", label, key, got)
		}
		return
	}
	g, ok := got.(float64)
	if !ok || int64(g) != want.(int64) {
		t.Errorf("%s: %s = %v, want %v", label, key, got, want)
	}
}

func topTitle(t *testing.T, label string, e map[string]any, want any) {
	t.Helper()
	got := e["issue_title"]
	if want == nil {
		if got != nil {
			t.Errorf("%s: issue_title = %v, want null", label, got)
		}
		return
	}
	s, ok := got.(string)
	if !ok || s != want.(string) {
		t.Errorf("%s: issue_title mismatch (got %T len %d, want len %d)", label, got, len(fmt.Sprint(got)), len(want.(string)))
	}
}

func TestAnalyticsSpendTopRunsIdentityLiveDB(t *testing.T) {
	f := newAnFixture(t, 2056)
	admin, u, v := f.user(true), f.user(false), f.user(false)
	ru, _ := f.repoOwnedBy(u, "tr-u")
	rv, _ := f.repoOwnedBy(v, "tr-v")
	at := f.now.Add(-3 * time.Hour)

	short := "a\x01b\tc\nd\x7fe <script>&\"'</script> é 中文 \U0001F600 \u202e rtl"
	long := strings.Repeat(short+" ", 20000/len(short)+1)
	if len(long) < 20000 {
		t.Fatalf("long title is only %d bytes", len(long))
	}
	const issueA, issueInacc, issueCont = int64(41001), int64(41002), int64(345)
	const jobTitle, maskTitle = "distinctive-job-title-4776", "distinctive-inaccessible-title-4776"

	issueRun := f.seedProbe(u, at, topProbe{kind: "issue", repo: ru, cost: "10.000000", title: "revealed-issue-title", issueIID: issueA, mr: int64(77)})
	ciBoth := f.seedProbe(u, at, topProbe{kind: "ci_fix", repo: ru, cost: "9.000000", title: "ci both", mr: int64(5), ciFixMR: int64(9)})
	ciOnlyFix := f.seedProbe(u, at, topProbe{kind: "ci_fix", repo: ru, cost: "8.000000", title: "ci only fix", ciFixMR: int64(12)})
	ciOnlyMR := f.seedProbe(u, at, topProbe{kind: "ci_fix", repo: ru, cost: "7.000000", title: "ci only mr", mr: int64(6)})
	ciNeither := f.seedProbe(u, at, topProbe{kind: "ci_fix", repo: ru, cost: "6.000000", title: "ci neither"})
	taskCont := f.seedProbe(u, at, topProbe{kind: "task", repo: ru, cost: "5.000000", title: "task continued", continued: issueCont})
	// A job's issue_iid is structurally NULL (runs_kind_shape); runs.mr_iid is a probe that
	// no_repo must withhold.
	noRepo := f.seedProbe(u, at, topProbe{kind: "job", cost: "4.000000", title: jobTitle, mr: int64(88)})
	inacc := f.seedProbe(u, at, topProbe{kind: "issue", repo: rv, cost: "3.000000", title: maskTitle, issueIID: issueInacc, mr: int64(66)})
	titleShort := f.seedProbe(u, at, topProbe{kind: "issue", repo: ru, cost: "2.000000", title: short, issueIID: int64(41003)})
	titleLong := f.seedProbe(u, at, topProbe{kind: "issue", repo: ru, cost: "1.000000", title: long, issueIID: int64(41004)})

	routes := []struct {
		name   string
		path   string
		cookie uuid.UUID
		masked bool
	}{
		{"owner route", "/api/analytics/spend", u, true},
		{"admin user route", "/api/admin/analytics/spend?user=" + u.String(), admin, false},
		{"admin all-users route", "/api/admin/analytics/spend?user=all", admin, false},
	}
	for _, r := range routes {
		top := rawTop(t, r.name, f.cookie(r.cookie, r.path))
		if len(top) != 10 {
			t.Fatalf("%s: %d top runs, want 10", r.name, len(top))
		}
		get := func(id uuid.UUID) map[string]any {
			t.Helper()
			e, ok := top[id.String()]
			if !ok {
				t.Fatalf("%s: run %s not in the top runs", r.name, id)
			}
			return e
		}
		l := func(s string) string { return r.name + " " + s }

		e := get(issueRun)
		topNum(t, l("issue"), e, "issue_iid", issueA)
		topTitle(t, l("issue"), e, "revealed-issue-title")
		topNum(t, l("issue"), e, "mr_iid", int64(77))

		topNum(t, l("ci both"), get(ciBoth), "mr_iid", int64(5))
		topNum(t, l("ci only ci_fix_mr_iid"), get(ciOnlyFix), "mr_iid", int64(12))
		topNum(t, l("ci only mr_iid"), get(ciOnlyMR), "mr_iid", int64(6))
		topNum(t, l("ci neither"), get(ciNeither), "mr_iid", nil)

		topNum(t, l("task continued"), get(taskCont), "issue_iid", issueCont)

		e = get(noRepo)
		topTitle(t, l("no_repo"), e, jobTitle)
		topNum(t, l("no_repo"), e, "issue_iid", nil)
		topNum(t, l("no_repo"), e, "mr_iid", nil)

		e = get(inacc)
		if r.masked {
			if e["repo_key"] != "inaccessible_repo" {
				t.Errorf("%s: repo_key = %v", r.name, e["repo_key"])
			}
			topNum(t, l("inaccessible"), e, "issue_iid", nil)
			topTitle(t, l("inaccessible"), e, nil)
			topNum(t, l("inaccessible"), e, "mr_iid", nil)
		} else {
			topNum(t, l("revealed foreign"), e, "issue_iid", issueInacc)
			topTitle(t, l("revealed foreign"), e, maskTitle)
			topNum(t, l("revealed foreign"), e, "mr_iid", int64(66))
		}

		topTitle(t, l("short title"), get(titleShort), short)
		topTitle(t, l("long title"), get(titleLong), long)
	}
}
