package store_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/runkind"
)

// Issue #4776: top_runs entries carry issue_iid, issue_title and mr_iid in both Spend
// statements. The assertions read the RAW jsonb bytes (before DecodeAnalyticsSpendDoc) so a
// key that is missing, rather than null, fails.

// topEntry is one raw top_runs element.
type topEntry map[string]json.RawMessage

// rawTopRuns decodes the raw document's top_runs array keyed by run id.
func rawTopRuns(t *testing.T, label string, raw []byte) map[uuid.UUID]topEntry {
	t.Helper()
	var doc struct {
		TopRuns []topEntry `json:"top_runs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: raw document: %v", label, err)
	}
	out := map[uuid.UUID]topEntry{}
	for _, e := range doc.TopRuns {
		for _, k := range []string{"issue_iid", "issue_title", "mr_iid"} {
			if _, ok := e[k]; !ok {
				t.Fatalf("%s: top run %s is missing key %q", label, e["run_id"], k)
			}
		}
		var id uuid.UUID
		if err := json.Unmarshal(e["run_id"], &id); err != nil {
			t.Fatalf("%s: run_id: %v", label, err)
		}
		out[id] = e
	}
	return out
}

func (e topEntry) num(t *testing.T, key string) *int64 {
	t.Helper()
	var v *int64
	if err := json.Unmarshal(e[key], &v); err != nil {
		t.Fatalf("%s = %s: %v", key, e[key], err)
	}
	return v
}

func (e topEntry) str(t *testing.T, key string) *string {
	t.Helper()
	var v *string
	if err := json.Unmarshal(e[key], &v); err != nil {
		t.Fatalf("%s = %s: %v", key, e[key], err)
	}
	return v
}

func topWantNum(t *testing.T, what string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %d", what, got, want)
	}
}

func topWantNullNum(t *testing.T, what string, got *int64) {
	t.Helper()
	if got != nil {
		t.Errorf("%s = %d, want null", what, *got)
	}
}

func topWantStr(t *testing.T, what string, got *string, want string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s = null, want %q", what, want)
	} else if *got != want {
		t.Errorf("%s differs from the seeded title (got %d bytes, want %d bytes)", what, len(*got), len(want))
	}
}

const (
	topTitleShort = "a\x01b\tc\nd\x7fe <script>&\"'</script> é 中文 \U0001F600 \u202e rtl"
	topTitleMasks = "distinctive-inaccessible-title-4776"
	topTitleJob   = "distinctive-job-title-4776"
)

func TestAnalyticsSpendTopRunsIdentityLiveDB(t *testing.T) {
	f := newSpendFixture(t)
	// A year of its own keeps the all-scope cohort to this test's runs.
	f.claimYear(2052)
	now := utcAt(2052, 6, 15, 12, 0, 0)

	u := f.owner("topid-u")
	v := f.owner("topid-v")
	ru, _ := f.repo(u, "topid-u")
	rv, _ := f.repo(v, "topid-v") // owned by V; U's run references it

	setRow := func(id uuid.UUID, title string, ciFixMR, continued any) {
		t.Helper()
		mustExec(f.ctx, t, f.pool,
			`UPDATE runs SET issue_title = $2, ci_fix_mr_iid = $3, continued_issue_iid = $4 WHERE id = $1`,
			id, title, ciFixMR, continued)
	}
	seed := func(r spendRun, cost, title string, ciFixMR, continued any) uuid.UUID {
		t.Helper()
		r.Owner = u.User
		r.At = utcAt(2052, 6, 10, 9, 0, 0)
		id := f.run(r)
		f.metered(id, "model-a", cost)
		setRow(id, title, ciFixMR, continued)
		return id
	}

	long := strings.Repeat(topTitleShort+" ", 20000/len(topTitleShort)+1)
	if len(long) < 20000 {
		t.Fatalf("long title is only %d bytes", len(long))
	}

	issueRun := seed(spendRun{Repo: ru, MR: 77}, "10.000000", "revealed-issue-title", nil, nil)
	ciBoth := seed(spendRun{Repo: ru, Kind: runkind.CIFix, MR: 5}, "9.000000", "ci both", int64(9), nil)
	ciOnlyFix := seed(spendRun{Repo: ru, Kind: runkind.CIFix}, "8.000000", "ci only fix", int64(12), nil)
	ciOnlyMR := seed(spendRun{Repo: ru, Kind: runkind.CIFix, MR: 6}, "7.000000", "ci only mr", nil, nil)
	ciNeither := seed(spendRun{Repo: ru, Kind: runkind.CIFix}, "6.000000", "ci neither", nil, nil)
	taskCont := seed(spendRun{Repo: ru, Kind: runkind.Task}, "5.000000", "task continued", nil, int64(345))
	// A job's issue_iid is structurally NULL (runs_kind_shape), so only the title can show.
	// The seeded runs.mr_iid is a probe: no_repo must withhold it.
	noRepo := seed(spendRun{Kind: runkind.Job, MR: 88}, "4.000000", topTitleJob, nil, nil)
	inacc := seed(spendRun{Repo: rv, MR: 66}, "3.000000", topTitleMasks, nil, nil)
	titleShort := seed(spendRun{Repo: ru}, "2.000000", topTitleShort, nil, nil)
	titleLong := seed(spendRun{Repo: ru}, "1.000000", long, nil, nil)

	issueIID := func(id uuid.UUID) int64 {
		t.Helper()
		var n int64
		if err := f.pool.QueryRow(f.ctx, `SELECT issue_iid FROM runs WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("issue_iid of %s: %v", id, err)
		}
		return n
	}

	type call struct {
		name   string
		c      spendCall
		masked bool
	}
	calls := []call{
		{"owner reveal=false", spendCall{Owner: u.User, Reveal: false, Now: now}, true},
		{"owner reveal=true", spendCall{Owner: u.User, Reveal: true, Now: now}, false},
		{"all scope", spendCall{Now: now}, false},
	}
	results := map[string]map[uuid.UUID]topEntry{}
	for _, cl := range calls {
		res := f.call(cl.c)
		top := rawTopRuns(t, cl.name, res.Raw)
		results[cl.name] = top
		if len(top) != 10 {
			t.Fatalf("%s: %d top runs, want 10", cl.name, len(top))
		}
		get := func(id uuid.UUID) topEntry {
			t.Helper()
			e, ok := top[id]
			if !ok {
				t.Fatalf("%s: run %s not in the top runs", cl.name, id)
			}
			return e
		}
		p := func(s string) string { return cl.name + " " + s }

		// 1. A revealed issue run.
		e := get(issueRun)
		topWantNum(t, p("issue iid"), e.num(t, "issue_iid"), issueIID(issueRun))
		topWantStr(t, p("issue title"), e.str(t, "issue_title"), "revealed-issue-title")
		topWantNum(t, p("issue mr_iid"), e.num(t, "mr_iid"), 77)

		// 2. ci_fix: mr_iid wins over ci_fix_mr_iid; each alone is used; neither is null.
		topWantNum(t, p("ci both"), get(ciBoth).num(t, "mr_iid"), 5)
		topWantNum(t, p("ci only ci_fix_mr_iid"), get(ciOnlyFix).num(t, "mr_iid"), 12)
		topWantNum(t, p("ci only mr_iid"), get(ciOnlyMR).num(t, "mr_iid"), 6)
		topWantNullNum(t, p("ci neither"), get(ciNeither).num(t, "mr_iid"))

		// 3. A task run continuing an issue reports that issue's number.
		topWantNum(t, p("task continued issue iid"), get(taskCont).num(t, "issue_iid"), 345)

		// 4. no_repo: the title shows, the numbers are withheld.
		e = get(noRepo)
		topWantStr(t, p("no_repo title"), e.str(t, "issue_title"), topTitleJob)
		topWantNullNum(t, p("no_repo issue_iid"), e.num(t, "issue_iid"))
		topWantNullNum(t, p("no_repo mr_iid"), e.num(t, "mr_iid"))

		// 5. inaccessible_repo: masked all null, revealed all set.
		e = get(inacc)
		if cl.masked {
			if k := string(e["repo_key"]); k != `"inaccessible_repo"` {
				t.Errorf("%s: repo_key = %s", cl.name, k)
			}
			topWantNullNum(t, p("masked issue_iid"), e.num(t, "issue_iid"))
			if s := e.str(t, "issue_title"); s != nil {
				t.Errorf("%s: masked issue_title = %q, want null", cl.name, *s)
			}
			topWantNullNum(t, p("masked mr_iid"), e.num(t, "mr_iid"))
		} else {
			topWantNum(t, p("revealed issue_iid"), e.num(t, "issue_iid"), issueIID(inacc))
			topWantStr(t, p("revealed title"), e.str(t, "issue_title"), topTitleMasks)
			topWantNum(t, p("revealed mr_iid"), e.num(t, "mr_iid"), 66)
		}

		// 6. Titles round-trip byte for byte.
		topWantStr(t, p("short title"), get(titleShort).str(t, "issue_title"), topTitleShort)
		topWantStr(t, p("long title"), get(titleLong).str(t, "issue_title"), long)
	}

	// The masked entry differs from the revealed one only in the repo and identity keys.
	masked, revealed := results["owner reveal=false"][inacc], results["owner reveal=true"][inacc]
	skip := map[string]bool{"repo_key": true, "repo_id": true, "repo_path": true,
		"issue_iid": true, "issue_title": true, "mr_iid": true}
	if len(masked) != len(revealed) {
		t.Errorf("masked entry has %d keys, revealed %d", len(masked), len(revealed))
	}
	for k, mv := range masked {
		if skip[k] {
			continue
		}
		if rvv, ok := revealed[k]; !ok || string(rvv) != string(mv) {
			t.Errorf("key %q differs: masked %s, revealed %s", k, mv, rvv)
		}
	}
}
