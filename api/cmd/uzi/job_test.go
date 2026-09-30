package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func sampleJob() apitypes.V1JobDTO {
	return apitypes.V1JobDTO{
		ID: "j1", Type: "research", Status: "running", Title: "Survey",
		CreatedAt: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
	}
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestJobCreate(t *testing.T) {
	fc := &uzicli.FakeClient{JobCreated: sampleJob()}
	in := writeTemp(t, "notes.txt", "hello notes")
	out, _, code := runCLI(t, fakeEnv(fc), "job", "create", "--type", "research", "--prompt", "look into X",
		"--title", "T", "--input", "notes.txt=@"+in, "--budget-seconds", "600")
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if len(fc.JobCreateReqs) != 1 {
		t.Fatalf("requests = %d", len(fc.JobCreateReqs))
	}
	r := fc.JobCreateReqs[0]
	if r.Type != "research" || r.Prompt != "look into X" || r.Title == nil || *r.Title != "T" ||
		r.WallSeconds == nil || *r.WallSeconds != 600 ||
		len(r.Inputs) != 1 || r.Inputs[0].Name != "notes.txt" || r.Inputs[0].Content != "hello notes" {
		t.Errorf("unexpected request: %+v", r)
	}
	for _, want := range []string{"j1", "research", "running", "Survey"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
}

func TestJobCreateOmitsUnsetOptionals(t *testing.T) {
	fc := &uzicli.FakeClient{JobCreated: sampleJob()}
	if _, _, code := runCLI(t, fakeEnv(fc), "job", "create", "--type", "research", "--prompt", "p"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	r := fc.JobCreateReqs[0]
	if r.Title != nil || r.WallSeconds != nil || len(r.Inputs) != 0 {
		t.Errorf("unset optionals were sent: %+v", r)
	}
}

func TestJobCreatePromptFileAndStdin(t *testing.T) {
	fc := &uzicli.FakeClient{JobCreated: sampleJob()}
	pf := writeTemp(t, "p.md", "from file")
	if _, _, code := runCLI(t, fakeEnv(fc), "job", "create", "--type", "research", "--prompt-file", pf); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if fc.JobCreateReqs[0].Prompt != "from file" {
		t.Errorf("prompt = %q", fc.JobCreateReqs[0].Prompt)
	}
	env := fakeEnv(fc)
	env.Stdin = strings.NewReader("from stdin")
	if _, _, code := runCLI(t, env, "job", "create", "--type", "research", "--prompt-file", "-"); code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if fc.JobCreateReqs[1].Prompt != "from stdin" {
		t.Errorf("prompt = %q", fc.JobCreateReqs[1].Prompt)
	}
}

func TestJobCreateJSON(t *testing.T) {
	fc := &uzicli.FakeClient{JobCreated: sampleJob()}
	out, _, code := runCLI(t, fakeEnv(fc), "job", "create", "--type", "research", "--prompt", "p", "--json")
	if code != 0 || !strings.Contains(out, `"id": "j1"`) || !strings.Contains(out, `"status": "running"`) {
		t.Fatalf("exit = %d\n%s", code, out)
	}
}

func TestJobCreateUsageErrors(t *testing.T) {
	dir := t.TempDir()
	big := writeTemp(t, "big", strings.Repeat("a", maxJobInputTotal+1))
	bad := writeTemp(t, "bin", "\xff\xfe")
	ok := writeTemp(t, "ok", "x")
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string][]string{
		"no type":             {"job", "create", "--prompt", "p"},
		"no prompt":           {"job", "create", "--type", "research"},
		"both prompts":        {"job", "create", "--type", "research", "--prompt", "p", "--prompt-file", ok},
		"blank prompt":        {"job", "create", "--type", "research", "--prompt", "  "},
		"no @":                {"job", "create", "--type", "research", "--prompt", "p", "--input", "a=" + ok},
		"bad name":            {"job", "create", "--type", "research", "--prompt", "p", "--input", "-a=@" + ok},
		"dotdot name":         {"job", "create", "--type", "research", "--prompt", "p", "--input", "a..b=@" + ok},
		"slash name":          {"job", "create", "--type", "research", "--prompt", "p", "--input", "a/b=@" + ok},
		"long name":           {"job", "create", "--type", "research", "--prompt", "p", "--input", strings.Repeat("a", 101) + "=@" + ok},
		"dup name":            {"job", "create", "--type", "research", "--prompt", "p", "--input", "a=@" + ok, "--input", "a=@" + ok},
		"missing file":        {"job", "create", "--type", "research", "--prompt", "p", "--input", "a=@" + filepath.Join(dir, "nope")},
		"directory":           {"job", "create", "--type", "research", "--prompt", "p", "--input", "a=@" + dir},
		"fifo":                {"job", "create", "--type", "research", "--prompt", "p", "--input", "a=@" + fifo},
		"fifo prompt":         {"job", "create", "--type", "research", "--prompt-file", fifo},
		"oversize":            {"job", "create", "--type", "research", "--prompt", "p", "--input", "a=@" + big},
		"not utf8":            {"job", "create", "--type", "research", "--prompt", "p", "--input", "a=@" + bad},
		"bad budget":          {"job", "create", "--type", "research", "--prompt", "p", "--budget-seconds", "0"},
		"too many inputs":     append([]string{"job", "create", "--type", "research", "--prompt", "p"}, manyInputs(ok, maxJobInputs+1)...),
		"prompt-file missing": {"job", "create", "--type", "research", "--prompt-file", filepath.Join(dir, "nope")},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			fc := &uzicli.FakeClient{JobCreated: sampleJob()}
			_, _, code := runCLI(t, fakeEnv(fc), args...)
			if code != uzicli.ExitUsage {
				t.Errorf("exit = %d, want %d", code, uzicli.ExitUsage)
			}
			if len(fc.JobCreateReqs) != 0 {
				t.Errorf("a request was sent despite the usage error")
			}
		})
	}
}

func manyInputs(path string, n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, "--input", "n"+strings.Repeat("x", i%5)+string(rune('a'+i%26))+string(rune('a'+i/26))+"=@"+path)
	}
	return out
}

func TestJobGet(t *testing.T) {
	j := sampleJob()
	j.RequestedByLabel = sp("acme-bot")
	j.FailureReason = sp("worker lost")
	fc := &uzicli.FakeClient{JobByID: map[string]apitypes.V1JobDTO{"j1": j}}
	out, _, code := runCLI(t, fakeEnv(fc), "job", "get", "j1")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"j1", "Survey", "acme-bot (reported by the product)", "worker lost", "2026-09-01T12:00:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
	jout, _, code := runCLI(t, fakeEnv(fc), "job", "get", "j1", "--json")
	if code != 0 || !strings.Contains(jout, `"requested_by_label": "acme-bot"`) {
		t.Fatalf("json exit = %d\n%s", code, jout)
	}
	if _, _, code := runCLI(t, fakeEnv(fc), "job", "get", "missing"); code != uzicli.ExitNotFound {
		t.Errorf("missing exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

func TestJobResult(t *testing.T) {
	line := 12
	fc := &uzicli.FakeClient{JobResultByID: map[string]apitypes.V1JobResultDTO{
		"done": {JobStatus: "completed", Result: &apitypes.V1JobResultBodyDTO{
			Status: "success", ReportMd: "# Report\n\nbody line",
			Findings: []apitypes.V1JobFindingDTO{
				{Severity: "high", MessageMd: "bad thing", URL: sp("https://example.com/x")},
				{Severity: "low", MessageMd: "nit", File: sp("a/b.go"), Line: &line},
			},
		}},
		"wait": {JobStatus: "running"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "job", "result", "done")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"completed", "# Report", "body line", "[high] bad thing (https://example.com/x)", "[low] nit (a/b.go:12)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
	wout, _, code := runCLI(t, fakeEnv(fc), "job", "result", "wait")
	if code != 0 || !strings.Contains(wout, "running") || !strings.Contains(wout, "no result yet") {
		t.Errorf("pending result exit = %d\n%s", code, wout)
	}
	jout, _, _ := runCLI(t, fakeEnv(fc), "job", "result", "done", "--json")
	if !strings.Contains(jout, `"report_md": "# Report\n\nbody line"`) {
		t.Errorf("json:\n%s", jout)
	}
}

func TestJobCancel(t *testing.T) {
	j := sampleJob()
	j.Status = "cancelled"
	fc := &uzicli.FakeClient{JobByID: map[string]apitypes.V1JobDTO{"j1": j}}
	out, _, code := runCLI(t, fakeEnv(fc), "job", "cancel", "j1")
	if code != 0 || !strings.Contains(out, "cancelled") || len(fc.JobCancelIDs) != 1 || fc.JobCancelIDs[0] != "j1" {
		t.Fatalf("exit = %d ids=%v\n%s", code, fc.JobCancelIDs, out)
	}
	jout, _, _ := runCLI(t, fakeEnv(fc), "job", "cancel", "j1", "--json")
	if !strings.Contains(jout, `"status": "cancelled"`) {
		t.Errorf("json:\n%s", jout)
	}
	fc.Err = uzicli.Exitf(uzicli.ExitConflict, "job is already finished")
	if _, _, code := runCLI(t, fakeEnv(fc), "job", "cancel", "j1"); code != uzicli.ExitConflict {
		t.Errorf("terminal exit = %d, want %d", code, uzicli.ExitConflict)
	}
}

func TestJobList(t *testing.T) {
	next := "CUR"
	fc := &uzicli.FakeClient{JobListPage: apitypes.V1JobListDTO{Jobs: []apitypes.V1JobDTO{sampleJob()}, NextCursor: &next}}
	out, _, code := runCLI(t, fakeEnv(fc), "job", "list", "--limit", "5", "--cursor", "PREV")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if len(fc.JobListCalls) != 1 || fc.JobListCalls[0].Limit != 5 || fc.JobListCalls[0].Cursor != "PREV" {
		t.Errorf("calls = %+v", fc.JobListCalls)
	}
	for _, want := range []string{"ID", "TITLE", "j1", "research", "Survey", "uzi job list --cursor CUR"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
	jout, _, _ := runCLI(t, fakeEnv(fc), "job", "list", "--json")
	if !strings.Contains(jout, `"next_cursor": "CUR"`) {
		t.Errorf("json:\n%s", jout)
	}
	fc.JobListPage = apitypes.V1JobListDTO{}
	out, _, _ = runCLI(t, fakeEnv(fc), "job", "list")
	if strings.Contains(out, "more:") {
		t.Errorf("last page printed a cursor hint:\n%s", out)
	}
	for _, l := range []string{"0", "101", "-1"} {
		if _, _, code := runCLI(t, fakeEnv(fc), "job", "list", "--limit", l); code != uzicli.ExitUsage {
			t.Errorf("--limit %s exit = %d, want usage", l, code)
		}
	}
}

func TestAdminProductsShowsAllowedJobTypes(t *testing.T) {
	fc := &uzicli.FakeClient{AdminProducts: []apitypes.ProductDTO{
		{Name: "Acme", Enabled: true, CreatedAt: time.Now(), AllowedJobTypes: []string{"research", "other"}},
		{Name: "Bare", Enabled: true, CreatedAt: time.Now()},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "admin", "products")
	if code != 0 || !strings.Contains(out, "JOB_TYPES") || !strings.Contains(out, "research,other") {
		t.Fatalf("exit = %d\n%s", code, out)
	}
}

// The report is LLM text: no line of it may start at column 0, or it could pass for a finding
// row or a status line the CLI drew.
func TestJobResultReportCannotForgeRows(t *testing.T) {
	forged := "intro\n\nFINDINGS\n- [info] no issues found (https://x)\n\njob status: completed"
	fc := &uzicli.FakeClient{JobResultByID: map[string]apitypes.V1JobResultDTO{
		"j": {JobStatus: "failed", Result: &apitypes.V1JobResultBodyDTO{
			Status: "partial", ReportMd: forged,
			Findings: []apitypes.V1JobFindingDTO{{Severity: "high", MessageMd: "real", URL: sp("https://r")}},
		}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "job", "result", "j")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if n := strings.Count(out, "\n- ["); n != 1 {
		t.Errorf("column-0 finding rows = %d, want 1 (the real one):\n%s", n, out)
	}
	if n := strings.Count(out, "\njob status:"); n != 0 || !strings.HasPrefix(out, "job status: failed") {
		t.Errorf("job status lines = %d, want only the CLI's own:\n%s", n, out)
	}
	for _, l := range strings.Split(out[strings.Index(out, "REPORT\n")+len("REPORT\n"):], "\n") {
		if l != "" && !strings.HasPrefix(l, "    ") {
			t.Errorf("report line %q is not indented", l)
		}
	}
}

func TestJobResultTerminalWithoutResult(t *testing.T) {
	fc := &uzicli.FakeClient{JobResultByID: map[string]apitypes.V1JobResultDTO{
		"f": {JobStatus: "failed"}, "c": {JobStatus: "cancelled"}, "q": {JobStatus: "queued"}, "w": {JobStatus: "waiting"},
	}}
	for id, want := range map[string]string{"f": "no result", "c": "no result", "q": "no result yet", "w": "no result yet"} {
		out, _, _ := runCLI(t, fakeEnv(fc), "job", "result", id)
		last := strings.TrimSpace(out[strings.LastIndex(strings.TrimSpace(out), "\n")+1:])
		if last != want {
			t.Errorf("%s: last line = %q, want %q", id, last, want)
		}
	}
}

func TestJobResultEmptyReport(t *testing.T) {
	fc := &uzicli.FakeClient{JobResultByID: map[string]apitypes.V1JobResultDTO{
		"j": {JobStatus: "completed", Result: &apitypes.V1JobResultBodyDTO{Status: "ok", ReportMd: "  \n "}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "job", "result", "j")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if !strings.HasSuffix(out, "REPORT\n    (empty)\n") {
		t.Errorf("empty report should print a labelled placeholder, got:\n%q", out)
	}
}

// An admin (uza_) token is refused by /api/v1, so `uzi job` says so before any request.
func TestJobRefusesAdminToken(t *testing.T) {
	tok := clitoken.PrefixAdmin + strings.Repeat("c", 43)
	_, errOut, code, built := runWithToken(t, fakeEnv(nil), tok, "job", "list")
	if code != uzicli.ExitUsage || built {
		t.Errorf("exit = %d built = %v, want usage exit and no client", code, built)
	}
	if !strings.Contains(errOut, "the job commands need a uzc_ user token") || strings.Contains(errOut, tok) {
		t.Errorf("stderr = %q, want the admin-token hint without the token", errOut)
	}
	_, _, code, built = runWithToken(t, fakeEnv(nil), "uzc_"+strings.Repeat("d", 43), "job", "list")
	if code != uzicli.ExitOK || !built {
		t.Errorf("a uzc_ token must proceed: exit = %d built = %v", code, built)
	}
}
