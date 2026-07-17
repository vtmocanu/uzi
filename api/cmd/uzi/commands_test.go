package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// fakeEnv builds an Env backed by the given fake client, with no config store
// (so no file IO) and a non-TTY stdout (so no colour).
func fakeEnv(fc *uzicli.FakeClient) Env {
	return Env{
		Stdout:    &bytes.Buffer{},
		Stderr:    &bytes.Buffer{},
		Stdin:     strings.NewReader(""),
		StdoutTTY: false,
		NewClient: func(uzicli.Settings) uzicli.Client { return fc },
		Store:     nil,
	}
}

// runCLI runs the CLI with fresh output buffers and returns stdout, stderr and
// the process exit code.
func runCLI(t *testing.T, env Env, args ...string) (string, string, int) {
	t.Helper()
	// Keep resolveSettings deterministic regardless of the dev shell.
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	var out, errb bytes.Buffer
	env.Stdout = &out
	env.Stderr = &errb
	code := Main(env, args)
	return out.String(), errb.String(), code
}

func TestCommandTree(t *testing.T) {
	root := newRootCmd(fakeEnv(&uzicli.FakeClient{}))

	topWant := []string{
		"login", "logout", "auth", "whoami", "run",
		"worker", "repo", "admin", "skill", "version",
	}
	for _, name := range topWant {
		if findCmd(root, name) == nil {
			t.Errorf("missing top-level command %q", name)
		}
	}

	subWant := map[string][]string{
		"run":    {"list", "get", "logs", "review", "create", "approve", "reject", "cancel", "follow-up"},
		"worker": {"list", "rm"},
		"repo":   {"list"},
		"admin":  {"users", "runs", "workers", "usage", "rate-limits"},
		"skill":  {"status", "install"},
		"auth":   {"token", "status"},
	}
	for parent, kids := range subWant {
		pc := findCmd(root, parent)
		if pc == nil {
			t.Errorf("missing parent command %q", parent)
			continue
		}
		for _, kid := range kids {
			if findCmd(pc, kid) == nil {
				t.Errorf("missing %q subcommand %q", parent, kid)
			}
		}
	}

	// Global flags are the whole agent contract; assert they exist.
	for _, f := range []string{"json", "url", "quiet", "no-color"} {
		if root.PersistentFlags().Lookup(f) == nil {
			t.Errorf("missing global flag --%s", f)
		}
	}
}

func findCmd(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func TestHelpRendersTree(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "--help")
	if code != uzicli.ExitOK {
		t.Fatalf("--help exit = %d, want 0", code)
	}
	for _, want := range []string{"run", "worker", "repo", "admin", "skill", "version", "whoami", "--json"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q\n%s", want, out)
		}
	}
}

func TestWhoamiJSON(t *testing.T) {
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@example.com", IsAdmin: false}}
	out, _, code := runCLI(t, fakeEnv(fc), "whoami", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": "u1"`) || !strings.Contains(out, `"is_admin": false`) {
		t.Errorf("unexpected JSON:\n%s", out)
	}
}

func TestWhoamiTable(t *testing.T) {
	fc := &uzicli.FakeClient{User: apitypes.UserDTO{ID: "u1", Email: "a@example.com", IsAdmin: true}}
	out, _, code := runCLI(t, fakeEnv(fc), "whoami")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "EMAIL") || !strings.Contains(out, "a@example.com") {
		t.Errorf("unexpected table:\n%s", out)
	}
}

func TestRunListJSON(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "r1", Status: "running", Kind: "issue", IssueTitle: "fix"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "list", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": "r1"`) {
		t.Errorf("unexpected JSON:\n%s", out)
	}
}

func TestRunListTableTitle(t *testing.T) {
	fc := &uzicli.FakeClient{Runs: []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "r1", Status: "running", Kind: "issue", IssueTitle: "fix login"}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "fix login") || !strings.Contains(out, "KIND") {
		t.Errorf("unexpected table:\n%s", out)
	}
}

func TestRunGetPresent(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": {ID: "r1", Status: "queued"}}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
}

func TestRunGetHealthReason(t *testing.T) {
	reason := "waiting for vault unlock"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"r1": {ID: "r1", Status: "queued", Health: "blocked", HealthReason: &reason},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "HEALTH_REASON") || !strings.Contains(out, reason) {
		t.Errorf("run get did not surface the health reason (Risk 4):\n%s", out)
	}
}

func TestRunGetMissingExit4(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "get", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

// A visible-but-unjudged run (200 {"review":null}) is exit 0 "not judged", NOT
// exit 4 (D21). The fake models it as a present-but-nil map entry.
func TestRunReviewNotJudgedExit0(t *testing.T) {
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r1": nil}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "review", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0 (not judged)", code)
	}
	if !strings.Contains(out, "not judged") {
		t.Errorf("want a 'not judged' line, got:\n%s", out)
	}
}

// A real 404 (absent id) is exit 4 — reserved, distinct from the null case above.
func TestRunReviewMissingExit4(t *testing.T) {
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{}}
	_, _, code := runCLI(t, fakeEnv(fc), "run", "review", "nope")
	if code != uzicli.ExitNotFound {
		t.Fatalf("exit = %d, want %d", code, uzicli.ExitNotFound)
	}
}

// status:"failed" renders the "judge incomplete" caveat (wire value is "failed",
// not "incomplete").
func TestRunReviewFailedCaveat(t *testing.T) {
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"r1": {Verdict: "needs_work", Status: "failed", SummaryMd: "partial"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "review", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "judge incomplete") {
		t.Errorf("failed review missing the incomplete caveat:\n%s", out)
	}
}

// --json passes the {"review": ...} envelope through, including recommendations.
func TestRunReviewJSONEnvelope(t *testing.T) {
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"r1": {ID: "rv1", Verdict: "good", Status: "complete", Recommendations: []apitypes.RecommendationDTO{
			{Category: "improve_uzi", Target: "docs", Confidence: "high", RationaleMd: "add examples"},
		}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "review", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"review"`) || !strings.Contains(out, `"verdict": "good"`) {
		t.Errorf("--json did not pass the envelope through:\n%s", out)
	}
}

func TestRunReviewNullJSONEnvelope(t *testing.T) {
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{"r1": nil}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "review", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"review": null`) {
		t.Errorf("null review --json should emit {\"review\": null}:\n%s", out)
	}
}

func TestRunLogs(t *testing.T) {
	agent := "coder"
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {
			{Seq: 1, Kind: "assistant", Agent: &agent, Payload: []byte(`{"text":"hello"}`)},
			{Seq: 2, Kind: "tool", Payload: []byte(`{"name":"bash"}`)},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "#1") || !strings.Contains(out, "assistant") || !strings.Contains(out, "hello") {
		t.Errorf("unexpected logs:\n%s", out)
	}
}

func TestRunLogsAfterFilter(t *testing.T) {
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {
			{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"one"}`)},
			{Seq: 2, Kind: "assistant", Payload: []byte(`{"text":"two"}`)},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1", "--after", "1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Errorf("--after=1 should skip seq 1:\n%s", out)
	}
}

func TestAdminAuthErrorExit3(t *testing.T) {
	fc := &uzicli.FakeClient{Err: uzicli.Exitf(uzicli.ExitAuth, "admin scope required")}
	_, _, code := runCLI(t, fakeEnv(fc), "admin", "users")
	if code != uzicli.ExitAuth {
		t.Fatalf("exit = %d, want %d (auth)", code, uzicli.ExitAuth)
	}
}

func TestUnknownCommandExit2(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "bogus")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

func TestUnknownFlagExit2(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "run", "list", "--nope")
	if code != uzicli.ExitUsage {
		t.Fatalf("exit = %d, want %d (usage)", code, uzicli.ExitUsage)
	}
}

func TestStubExit1(t *testing.T) {
	_, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "login")
	if code != uzicli.ExitGeneric {
		t.Fatalf("exit = %d, want %d (generic/not-implemented)", code, uzicli.ExitGeneric)
	}
}

func TestVersion(t *testing.T) {
	out, _, code := runCLI(t, fakeEnv(&uzicli.FakeClient{}), "version")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, version) {
		t.Errorf("version output %q missing %q", out, version)
	}
}
