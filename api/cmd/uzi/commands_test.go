package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"gitlab.example.com/vtmocanu/uzi/api/internal/apitypes"
	"gitlab.example.com/vtmocanu/uzi/api/internal/uzicli"
)

// fakeEnv builds an Env backed by the given fake client, with no config store
// (so no file IO) and a non-TTY stdout (so no colour). It takes the Client
// interface (not *FakeClient) so tests can inject bespoke fakes too.
func fakeEnv(fc uzicli.Client) Env {
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
		"login", "logout", "auth", "whoami", "run", "review",
		"worker", "token", "repo", "admin", "skill", "version",
	}
	for _, name := range topWant {
		if findCmd(root, name) == nil {
			t.Errorf("missing top-level command %q", name)
		}
	}

	subWant := map[string][]string{
		"run": {"list", "get", "logs", "review", "create", "approve", "reject", "cancel", "follow-up", "inputs"},
		// backlog is the PRD #98 M7 read; there is deliberately no `file` verb —
		// filing stays browser-only (#68's stance, Decision 10).
		"review": {"show", "backlog", "resolve", "dismiss", "undo", "stats"},
		"worker": {"list", "rm", "set-token"},
		"token":  {"list"},
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

// PRD #108 M9b. An auto-stopped run must be distinguishable from a user cancel at
// the CLI, and the two are IDENTICAL on every other field: both end `failed`, and
// on the live-poller half the worker's own SetRunFailed overwrites failure_reason
// with "run cancelled". stop_kind is the only thing that tells them apart.
func TestRunGetSurfacesStopKind(t *testing.T) {
	autoStop, cancelled := "auto_stopped", "cancelled"
	// The SAME failure_reason on both, which is what the live half actually produces
	// — the point being that the reason cannot be the discriminator.
	reason := "run cancelled"
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{
		"auto":  {ID: "auto", Status: "failed", FailureReason: &reason, StopKind: &autoStop},
		"user":  {ID: "user", Status: "failed", FailureReason: &reason, StopKind: &cancelled},
		"plain": {ID: "plain", Status: "failed", FailureReason: &reason},
	}}

	autoOut, _, code := runCLI(t, fakeEnv(fc), "run", "get", "auto")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(autoOut, "STOP_KIND") || !strings.Contains(autoOut, "auto_stopped") {
		t.Fatalf("an auto-stopped run does not show its stop kind, so it is indistinguishable from a user cancel:\n%s", autoOut)
	}
	userOut, _, _ := runCLI(t, fakeEnv(fc), "run", "get", "user")
	if !strings.Contains(userOut, "cancelled") {
		t.Errorf("a user-cancelled run lost its stop kind:\n%s", userOut)
	}
	// The whole claim, stated as a comparison rather than as two substring checks:
	// the two outputs must actually DIFFER. Without this the assertions above would
	// pass on a build that printed the same constant for every run.
	if autoOut == userOut {
		t.Errorf("an auto-stop and a user cancel render identically; stop_kind is the only field that separates them:\n%s", autoOut)
	}
	// And a run that stopped for neither reason carries no row at all, rather than an
	// empty one — same shape as HEALTH_REASON and FAILURE_REASON above it.
	plainOut, _, _ := runCLI(t, fakeEnv(fc), "run", "get", "plain")
	if strings.Contains(plainOut, "STOP_KIND") {
		t.Errorf("a genuine failure (no stop_kind) printed an empty STOP_KIND row:\n%s", plainOut)
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

// ── PRD #99 M4: instance + label on the human line ───────────────────────────

// twoCoderLogs is the shape this milestone exists for: one lead frame with neither
// column, then two DISTINCT `coder` invocations. Before M4 the last two rows were
// byte-identical apart from their seq and payload, so `uzi run watch` could not tell
// which coder was doing what.
func twoCoderLogs() map[string][]apitypes.MessageDTO {
	lead, coder := "lead", "coder"
	instA, instB := "toolu_01AAAAAAAAAAAAAAAA3v6ptu", "toolu_01BBBBBBBBBBBBBBBB2k9xqf"
	labelA, labelB := "API wiring", "web gate UX"
	return map[string][]apitypes.MessageDTO{
		"r1": {
			{Seq: 1, Kind: "text", Agent: &lead, Payload: []byte(`{"text":"dispatching"}`)},
			{Seq: 2, Kind: "tool_use", Agent: &coder, AgentInstance: &instA, AgentLabel: &labelA, Payload: []byte(`{"name":"Edit"}`)},
			{Seq: 3, Kind: "tool_use", Agent: &coder, AgentInstance: &instB, AgentLabel: &labelB, Payload: []byte(`{"name":"Write"}`)},
		},
	}
}

// The headline requirement (Decision 10): two same-role instances must read
// DISTINCTLY in the text format. Asserted on the whole actor cell of each line, not
// on a substring floating anywhere in the output — a fragment test would pass even
// if both ids landed on one row.
func TestRunLogsDistinguishesSameRoleInstances(t *testing.T) {
	fc := &uzicli.FakeClient{LogsByID: twoCoderLogs()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 message lines, got %d:\n%s", len(lines), out)
	}
	// The lead frame carries neither column, so its cell is the bare role: no "/"
	// short id and no " · " label. This is the legacy/pre-migration rendering.
	if strings.Contains(lines[0], "/") || strings.Contains(lines[0], " · ") {
		t.Errorf("a frame with no instance must render the bare role:\n%s", lines[0])
	}
	if !strings.Contains(lines[0], "lead") {
		t.Errorf("lead row lost its role:\n%s", lines[0])
	}
	// Both coder rows carry the role, their own short id, and their own label.
	if !strings.Contains(lines[1], "coder/3v6ptu") || !strings.Contains(lines[1], "API wiring") {
		t.Errorf("coder A row missing its instance or label:\n%s", lines[1])
	}
	if !strings.Contains(lines[2], "coder/2k9xqf") || !strings.Contains(lines[2], "web gate UX") {
		t.Errorf("coder B row missing its instance or label:\n%s", lines[2])
	}
	// …and the two rows are not each other's twin, which is the actual bug.
	if strings.Contains(lines[1], "2k9xqf") || strings.Contains(lines[2], "3v6ptu") {
		t.Errorf("the two coder invocations bled into each other:\n%s\n%s", lines[1], lines[2])
	}
}

// The short id is a TAIL, not a prefix. An SDK tool-use id begins with a constant
// `toolu_01`, so a first-N rule would render both invocations above identically —
// this pins the choice so a future "make it match shortRecID" tidy has to fail a
// test rather than silently collapse the column.
func TestShortInstanceIDUsesTheTail(t *testing.T) {
	const a = "toolu_01AAAAAAAAAAAAAAAA3v6ptu"
	const b = "toolu_01BBBBBBBBBBBBBBBB2k9xqf"
	if got := shortInstanceID(a); got != "3v6ptu" {
		t.Errorf("shortInstanceID(%q) = %q, want the last 6 runes", a, got)
	}
	if shortInstanceID(a) == shortInstanceID(b) {
		t.Errorf("two ids sharing the constant toolu_01 prefix must not collapse: %q", shortInstanceID(a))
	}
	// Shorter than the window: returned whole, never padded or panicking.
	if got := shortInstanceID("toolu_A"); got != "oolu_A" {
		t.Errorf("shortInstanceID(%q) = %q, want its 6-rune tail", "toolu_A", got)
	}
	if got := shortInstanceID("ab"); got != "ab" {
		t.Errorf("shortInstanceID(%q) = %q, want it unchanged", "ab", got)
	}
}

// --json parity is FREE (renderMessage marshals the DTO whole), but "free" is a
// claim, and an unpinned one would break the moment someone hand-rolls the JSON
// object. Agents read the FULL instance id here, never the display tail.
func TestRunLogsJSONCarriesFullInstanceAndLabel(t *testing.T) {
	fc := &uzicli.FakeClient{LogsByID: twoCoderLogs()}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 NDJSON lines, got %d:\n%s", len(lines), out)
	}
	var lead, coderA apitypes.MessageDTO
	if err := json.Unmarshal([]byte(lines[0]), &lead); err != nil {
		t.Fatalf("lead line is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &coderA); err != nil {
		t.Fatalf("coder line is not JSON: %v", err)
	}
	if lead.AgentInstance != nil || lead.AgentLabel != nil {
		t.Errorf("absent columns must stay null, got %v / %v", lead.AgentInstance, lead.AgentLabel)
	}
	if coderA.AgentInstance == nil || *coderA.AgentInstance != "toolu_01AAAAAAAAAAAAAAAA3v6ptu" {
		t.Errorf("--json must carry the FULL instance id, not the display tail: %v", coderA.AgentInstance)
	}
	if coderA.AgentLabel == nil || *coderA.AgentLabel != "API wiring" {
		t.Errorf("--json lost the label: %v", coderA.AgentLabel)
	}
	// Both keys present even when null (MessageDTO's tags are not omitempty).
	if !strings.Contains(lines[0], `"agent_instance":null`) || !strings.Contains(lines[0], `"agent_label":null`) {
		t.Errorf("NULL columns must be emitted as explicit nulls:\n%s", lines[0])
	}
}

// agent_label is FREE model-authored prose reaching a TTY — a wider class of
// untrusted than `agent`, which is a role from a fixed roster. A CSI sequence in it
// must be stripped on the human path exactly as the judge's free text is (Risk 13),
// while the visible characters survive.
//
// All THREE fields are covered, not just the label. The sanitization was real in
// code from M4 but only the label was tested, so dropping cellText from `agent` and
// from `agent_instance` each left the suite green (MEASURED at the M4 audit) — the
// worker supplies all three and none of them is trustworthy just because a healthy
// one looks like a role name.
func TestRunLogsSanitizesActorFieldsForTTY(t *testing.T) {
	const esc = "\x1b[2Jwipe" // CSI screen-clear
	inst := "toolu_01AAAAAAAAAAAAAAAA3v6ptu"
	benignRole, benignLabel := "coder", "API wiring"

	cases := []struct {
		name    string
		msg     apitypes.MessageDTO
		visible string
	}{
		{
			name:    "agent",
			msg:     apitypes.MessageDTO{Agent: ptr(esc), AgentInstance: &inst, AgentLabel: &benignLabel},
			visible: "[2Jwipe",
		},
		{
			name: "agent_instance",
			// The ESC must sit inside the LAST 6 RUNES or shortInstanceID cuts it away
			// before sanitizing ever runs, and the case proves nothing. Tail here is
			// "\x1bwipe" plus one leading A, so "wipe" is what must survive.
			msg:     apitypes.MessageDTO{Agent: &benignRole, AgentInstance: ptr("toolu_01AAAA\x1bwipe"), AgentLabel: &benignLabel},
			visible: "wipe",
		},
		{
			name:    "agent_label",
			msg:     apitypes.MessageDTO{Agent: &benignRole, AgentInstance: &inst, AgentLabel: ptr(esc)},
			visible: "[2Jwipe",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.msg
			m.Seq, m.Kind, m.Payload = 1, "text", []byte(`{}`)
			fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{"r1": {m}}}
			out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
			if code != uzicli.ExitOK {
				t.Fatalf("exit = %d, want 0", code)
			}
			if strings.Contains(out, "\x1b") {
				t.Errorf("an ESC in %s reached the TTY:\n%q", tc.name, out)
			}
			if !strings.Contains(out, tc.visible) {
				t.Errorf("sanitizing %s must drop the control byte only, keeping visible text:\n%s", tc.name, out)
			}
		})
	}
}

// A TAB in the actor cell keeps every invariant the code checks — `%-*s` pads in
// RUNES and a tab is one rune — while the terminal expands it to the next 8-column
// stop and walks the payload column right. That is the one way to defeat
// actorCellWidth without tripping the rune-based alignment test, so it is asserted
// on the RENDERED width, not the rune count. DEL rides along: it is outside
// sanitizeTTY's C0/C1 ranges and some terminals draw a glyph for it.
func TestRunLogsFoldsTabsAndDELInTheActorCell(t *testing.T) {
	// renderWidth expands tabs at 8-column stops, the way a terminal does.
	renderWidth := func(s string) int {
		col := 0
		for _, r := range s {
			if r == '\t' {
				col += 8 - (col % 8)
			} else {
				col++
			}
		}
		return col
	}
	role := "coder"
	inst := "toolu_01AAAAAAAAAAAAAAAA3v6ptu"
	benign, tabs, del := "API wiring", "x\t\t\t\t\t\t\t\ty", "a\x7fb"
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {
			{Seq: 1, Kind: "text", Agent: &role, AgentInstance: &inst, AgentLabel: &benign, Payload: []byte(`{"t":1}`)},
			{Seq: 2, Kind: "text", Agent: &role, AgentInstance: &inst, AgentLabel: &tabs, Payload: []byte(`{"t":2}`)},
			{Seq: 3, Kind: "text", Agent: &role, AgentInstance: &inst, AgentLabel: &del, Payload: []byte(`{"t":3}`)},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 lines, got %d:\n%s", len(lines), out)
	}
	if strings.ContainsRune(out, '\t') {
		t.Errorf("a tab survived into the actor column:\n%q", out)
	}
	if strings.ContainsRune(out, 0x7f) {
		t.Errorf("a DEL survived into the actor column:\n%q", out)
	}
	// The payload starts at the same RENDERED column on every line. Before the fix
	// the stacked-tab line measured 107 here against the benign line's 58, while the
	// rune offset was a constant 58 on both.
	want := renderWidth(lines[0][:strings.Index(lines[0], `{"t":1}`)])
	for i, marker := range []string{`{"t":2}`, `{"t":3}`} {
		got := renderWidth(lines[i+1][:strings.Index(lines[i+1], marker)])
		if got != want {
			t.Errorf("line %d payload starts at rendered column %d, want %d\n%q", i+2, got, want, lines[i+1])
		}
	}
	// The fold is a space, not a deletion, and it is 1:1 — eight tabs become eight
	// spaces, so the label's word break survives while its rendered width no longer
	// depends on where the cell happens to start.
	if !strings.Contains(lines[1], "x"+strings.Repeat(" ", 8)+"y") {
		t.Errorf("each tab should fold to exactly one space:\n%q", lines[1])
	}
}

// The actor cell holds a `·` separator and a possibly non-ASCII label, and the
// payload column must start at the same RUNE offset on every line. Go's fmt pads
// `%-*s` by runes, so this passes — but pinning it guards against a future switch to
// a byte-based manual pad, written on the false belief that fmt pads by bytes.
//
// This comment used to claim such a helper had SHIPPED and that "the mutation test
// that should have caught it did not". Both halves are false, corrected at the M4
// audit: `git log --all -S padCell -- api/` finds nothing, so no commit ever carried
// it (the only hits anywhere are this PRD's prose and a commit message describing an
// uncommitted draft), and the byte-pad mutation IS killed by this very test —
// re-measured, it fails with "rune offset 51 vs 58". The guard works; the story
// attached to it did not.
//
// Rune alignment only: a CJK rune still takes two terminal columns, which this CLI
// does not model anywhere.
func TestRunLogsActorColumnAlignsAcrossMultibyteLabels(t *testing.T) {
	ascii, wide := "lead", "coder"
	inst := "toolu_01AAAAAAAAAAAAAAAA3v6ptu"
	label := "日本語" // 3 runes, 9 bytes
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {
			{Seq: 1, Kind: "text", Agent: &ascii, Payload: []byte(`{"t":1}`)},
			{Seq: 2, Kind: "text", Agent: &wide, AgentInstance: &inst, AgentLabel: &label, Payload: []byte(`{"t":2}`)},
		},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
	}
	// The payload starts at the same RUNE offset on both lines.
	at := func(line, want string) int { return utf8.RuneCountInString(line[:strings.Index(line, want)]) }
	if got, want := at(lines[1], `{"t":2}`), at(lines[0], `{"t":1}`); got != want {
		t.Errorf("multibyte label skewed the payload column: rune offset %d vs %d\n%s\n%s", got, want, lines[0], lines[1])
	}
	if !strings.Contains(lines[1], "· 日本語") {
		t.Errorf("the multibyte label did not survive:\n%s", lines[1])
	}
}

// An over-long label must not blow the column open; the short id — the part that
// actually disambiguates — must survive the truncation.
func TestRunLogsCapsTheActorCell(t *testing.T) {
	coder := "coder"
	inst := "toolu_01AAAAAAAAAAAAAAAA3v6ptu"
	label := strings.Repeat("x", 200)
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {{Seq: 1, Kind: "text", Agent: &coder, AgentInstance: &inst, AgentLabel: &label, Payload: []byte(`{"t":1}`)}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	line := strings.TrimRight(out, "\n")
	if !strings.Contains(line, "coder/3v6ptu") {
		t.Errorf("truncation ate the short id, the one part that disambiguates:\n%s", line)
	}
	if !strings.Contains(line, "…") {
		t.Errorf("an over-long cell must be visibly truncated:\n%s", line)
	}
	// The whole actor cell fits its column: the payload starts right after it.
	if idx := strings.Index(line, `{"t":1}`); utf8.RuneCountInString(line[:idx]) != 6+17+actorCellWidth+1 {
		t.Errorf("actor cell overflowed its column, payload at rune %d:\n%s", utf8.RuneCountInString(line[:idx]), line)
	}
}

// Untrusted judge free text carrying ANSI escape/CSI sequences must render to the
// human TTY with the control bytes stripped (Risk 13 — no screen-clear, recolour,
// hide, or spoof). Only the C0/C1 control bytes go; visible text — including a
// multibyte codepoint whose UTF-8 bytes overlap the C1 byte range — survives,
// proving the strip is rune-wise, not byte-wise.
func TestRunReviewSanitizesHumanRender(t *testing.T) {
	const summary = "\x1b[2J\x1b[31mFAKE ERROR" // CSI screen-clear + red
	const target = "docs–end"                  // C1 NEL (U+0085) + en dash (U+2013, 0xE2 0x80 0x93)
	const rationale = "before\x1b[1mafter"      // SGR bold
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"r1": {Verdict: "needs_work", Status: "complete", SummaryMd: summary,
			Recommendations: []apitypes.RecommendationDTO{
				{Category: "improve_uzi", Target: target, Confidence: "high", RationaleMd: rationale},
			}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "review", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("human render leaked ESC (0x1b):\n%q", out)
	}
	if strings.ContainsRune(out, 0x85) {
		t.Errorf("human render leaked C1 control (U+0085):\n%q", out)
	}
	// The en dash's UTF-8 encoding contains a 0x80 byte; a byte-wise strip would
	// corrupt it, a rune-wise strip keeps "docs–end" intact.
	for _, want := range []string{"FAKE ERROR", "docs–end", "before", "after"} {
		if !strings.Contains(out, want) {
			t.Errorf("human render dropped visible text %q:\n%q", want, out)
		}
	}
}

// --json is the agent contract: it MUST NOT sanitize. The raw bytes survive the
// round-trip (encoding/json escapes the ESC byte as a \u sequence that decodes
// back to it), so a --json consumer sees exactly what the server sent.
func TestRunReviewJSONPreservesControlChars(t *testing.T) {
	const summary = "\x1b[2J\x1b[31mFAKE"
	fc := &uzicli.FakeClient{Reviews: map[string]*apitypes.ReviewDTO{
		"r1": {Verdict: "needs_work", Status: "complete", SummaryMd: summary},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "review", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	// Human render would strip the ESC; --json must instead carry it as a \u escape.
	if strings.ContainsRune(out, 0x1b) {
		t.Error("--json emitted a literal ESC byte; want it JSON-escaped, not raw")
	}
	var env struct {
		Review *apitypes.ReviewDTO `json:"review"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("decode --json envelope: %v\n%s", err, out)
	}
	got := "<nil review>"
	if env.Review != nil {
		got = env.Review.SummaryMd
	}
	if got != summary {
		t.Fatalf("--json must preserve raw bytes; summary_md = %q, want %q", got, summary)
	}
}

// followFake is a Client whose run flips to a terminal status after terminalAfter
// GetRun polls, so `run logs --follow` can be driven to completion
// deterministically. Every other method comes from the embedded FakeClient.
type followFake struct {
	*uzicli.FakeClient
	terminalAfter int
	getRunCalls   int
}

func (f *followFake) GetRun(_ context.Context, id string) (apitypes.RunDTO, error) {
	f.getRunCalls++
	st := "running"
	if f.getRunCalls >= f.terminalAfter {
		st = "completed"
	}
	return apitypes.RunDTO{ID: id, Status: st}, nil
}

// `run logs --follow` must terminate (exit 0) once the run reaches a terminal
// state, after draining remaining messages — not poll forever (the stated
// agent-audience footgun). A fake whose run goes terminal after 3 polls proves it
// returns rather than hanging.
func TestRunLogsFollowStopsOnTerminal(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	defer func() { logsPollInterval = old }()

	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"hi"}`)}},
	}}
	ff := &followFake{FakeClient: fc, terminalAfter: 3}

	var out bytes.Buffer
	env := fakeEnv(ff)
	env.Stdout = &out
	env.Stderr = &bytes.Buffer{}

	done := make(chan int, 1)
	go func() { done <- Main(env, []string{"run", "logs", "r1", "--follow"}) }()

	select {
	case code := <-done:
		if code != uzicli.ExitOK {
			t.Fatalf("--follow exit = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run logs --follow did not terminate on a terminal run (hung)")
	}
	if !strings.Contains(out.String(), "hi") {
		t.Errorf("--follow dropped the message before terminating:\n%s", out.String())
	}
	if ff.getRunCalls < 3 {
		t.Errorf("GetRun polled %d times, want >= 3 (should poll until terminal)", ff.getRunCalls)
	}
}

// The steer-queue table renders each follow-up's body and its delivery state.
// A non-consumed input on a live run is "queued"; a consumed one is "delivered".
func TestRunInputsTableStates(t *testing.T) {
	consumed := time.Now().Add(-time.Minute)
	queued := "please add a test"
	delivered := "and fix the lint"
	fc := &uzicli.FakeClient{
		RunByID: map[string]apitypes.RunDTO{"r1": {ID: "r1", Status: "running", Kind: "issue"}},
		InputsByID: map[string][]apitypes.SteerInputDTO{"r1": {
			{ID: 2, Body: &delivered, CreatedAt: time.Now().Add(-30 * time.Second), ConsumedAt: &consumed},
			{ID: 1, Body: &queued, CreatedAt: time.Now().Add(-2 * time.Minute)},
		}},
	}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "inputs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, want := range []string{"BODY", "STATE", "AGE", queued, "queued", delivered, "delivered"} {
		if !strings.Contains(out, want) {
			t.Errorf("inputs table missing %q:\n%s", want, out)
		}
	}
}

// --json emits the raw DTO list (id/body/created_at/consumed_at); state is
// derived by the agent from these fields, so the CLI does NOT fetch the run in
// --json mode — proven here by the absence of any RunByID entry.
func TestRunInputsJSON(t *testing.T) {
	body := "queued msg"
	fc := &uzicli.FakeClient{InputsByID: map[string][]apitypes.SteerInputDTO{
		"r1": {{ID: 7, Body: &body, CreatedAt: time.Now()}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "inputs", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, `"id": 7`) || !strings.Contains(out, `"consumed_at": null`) {
		t.Errorf("--json did not emit the raw DTO list:\n%s", out)
	}
}

// GET /inputs is owner-only (Decision 8): the caller reads its OWN run's queue,
// but another owner's run (absent from the fake, as the server 404s a non-owner)
// is exit 4 — never another user's steer text.
func TestRunInputsOwn200Foreign404(t *testing.T) {
	body := "mine"
	fc := &uzicli.FakeClient{InputsByID: map[string][]apitypes.SteerInputDTO{
		"mine": {{ID: 1, Body: &body, CreatedAt: time.Now()}},
	}}
	if _, _, code := runCLI(t, fakeEnv(fc), "run", "inputs", "mine", "--json"); code != uzicli.ExitOK {
		t.Fatalf("own run: exit = %d, want 0", code)
	}
	if _, _, code := runCLI(t, fakeEnv(fc), "run", "inputs", "someone-else"); code != uzicli.ExitNotFound {
		t.Fatalf("foreign run: exit = %d, want %d (not found)", code, uzicli.ExitNotFound)
	}
}

// The gate and terminal nuances (Decision 7) render when the run's status is
// known: a follow-up consumed at a plan gate reads "delivered (applies after
// approval)"; an unconsumed one on a terminal run reads "not delivered".
func TestRunInputsGateAndTerminalStates(t *testing.T) {
	consumed := time.Now().Add(-time.Minute)
	atGate := "approve me"
	stranded := "too late"
	gate := &uzicli.FakeClient{
		RunByID:    map[string]apitypes.RunDTO{"g": {ID: "g", Status: "awaiting_approval", Kind: "issue"}},
		InputsByID: map[string][]apitypes.SteerInputDTO{"g": {{ID: 1, Body: &atGate, CreatedAt: time.Now(), ConsumedAt: &consumed}}},
	}
	out, _, code := runCLI(t, fakeEnv(gate), "run", "inputs", "g")
	if code != uzicli.ExitOK {
		t.Fatalf("gate: exit = %d, want 0", code)
	}
	if !strings.Contains(out, "applies after approval") {
		t.Errorf("gate state missing the approval nuance:\n%s", out)
	}
	term := &uzicli.FakeClient{
		RunByID:    map[string]apitypes.RunDTO{"t": {ID: "t", Status: "completed", Kind: "issue"}},
		InputsByID: map[string][]apitypes.SteerInputDTO{"t": {{ID: 1, Body: &stranded, CreatedAt: time.Now()}}},
	}
	out, _, code = runCLI(t, fakeEnv(term), "run", "inputs", "t")
	if code != uzicli.ExitOK {
		t.Fatalf("terminal: exit = %d, want 0", code)
	}
	if !strings.Contains(out, "not delivered (run finished)") {
		t.Errorf("terminal state missing 'not delivered':\n%s", out)
	}
}

// The chat caveat (N3) prints to stderr ONLY for a chat run (whose queue lists
// every chat turn); an issue run's output stays clean.
func TestRunInputsChatCaveatNote(t *testing.T) {
	body := "first turn"
	chat := &uzicli.FakeClient{
		RunByID:    map[string]apitypes.RunDTO{"c": {ID: "c", Status: "running", Kind: "chat"}},
		InputsByID: map[string][]apitypes.SteerInputDTO{"c": {{ID: 1, Body: &body, CreatedAt: time.Now()}}},
	}
	if _, errOut, code := runCLI(t, fakeEnv(chat), "run", "inputs", "c"); code != uzicli.ExitOK || !strings.Contains(errOut, "chat run") {
		t.Fatalf("chat run: exit=%d, stderr=%q — want exit 0 with the chat caveat", code, errOut)
	}
	issue := &uzicli.FakeClient{
		RunByID:    map[string]apitypes.RunDTO{"i": {ID: "i", Status: "running", Kind: "issue"}},
		InputsByID: map[string][]apitypes.SteerInputDTO{"i": {{ID: 1, Body: &body, CreatedAt: time.Now()}}},
	}
	if _, errOut, _ := runCLI(t, fakeEnv(issue), "run", "inputs", "i"); strings.Contains(errOut, "chat run") {
		t.Errorf("issue run must NOT print the chat caveat:\n%s", errOut)
	}
}

// An empty queue renders the header row and exits 0 without fetching the run
// (no RunByID entry present), proving the empty-queue fast path.
func TestRunInputsEmpty(t *testing.T) {
	fc := &uzicli.FakeClient{InputsByID: map[string][]apitypes.SteerInputDTO{"r1": {}}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "inputs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "BODY") {
		t.Errorf("empty queue should still render the header row:\n%s", out)
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

// Every verb is now wired (M9 landed the last stubs: worker rm, skill
// status/install). `skill status` runs against a temp home and succeeds.
func TestSkillStatusRuns(t *testing.T) {
	env := fakeEnv(&uzicli.FakeClient{})
	env.SkillHome = t.TempDir()
	out, _, code := runCLI(t, env, "skill", "status")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "INSTALLED") {
		t.Errorf("skill status output missing INSTALLED:\n%s", out)
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

// ptr is a local test helper: MessageDTO's string fields are pointers, and the
// table cases above need per-case literals rather than shared addressable vars.
func ptr(s string) *string { return &s }

// --json must be BYTE-EXACT: the human path's sanitize-and-fold is a TTY concern,
// and an agent decoding NDJSON needs the label the worker actually stored. JSON's
// own encoder escapes control bytes structurally, so nothing is unsafe about
// emitting them — sanitizing here would silently corrupt the payload instead.
//
// This was untested until the M4 audit: the existing --json test used a benign
// label, so moving compactText into the FormatJSON branch left the suite green
// (MEASURED). The fixture therefore carries a control byte, a tab and a DEL —
// exactly the three the human cell now folds — so the two paths cannot drift into
// agreement by accident.
func TestRunLogsJSONKeepsAgentLabelByteExact(t *testing.T) {
	raw := "wipe\x1b[2J\ttabbed\x7fdel"
	role := "coder"
	inst := "toolu_01AAAAAAAAAAAAAAAA3v6ptu"
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {{Seq: 1, Kind: "text", Agent: &role, AgentInstance: &inst, AgentLabel: &raw, Payload: []byte(`{}`)}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var got apitypes.MessageDTO
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &got); err != nil {
		t.Fatalf("unmarshal NDJSON line: %v\n%s", err, out)
	}
	if got.AgentLabel == nil || *got.AgentLabel != raw {
		t.Errorf("--json altered agent_label:\n got %q\nwant %q", derefOr(got.AgentLabel), raw)
	}
	// The full instance id rides the JSON, never the display tail.
	if got.AgentInstance == nil || *got.AgentInstance != inst {
		t.Errorf("--json must carry the FULL instance id, got %q", derefOr(got.AgentInstance))
	}
}

// capCell slices RUNES, not bytes: a byte slice through a multibyte codepoint emits
// invalid UTF-8, the house trap this repo has hit before (cli_auth_flow.go:148
// spells it out, and PRD #99's server-side cap was written against the same one).
// Untested until the M4 audit — making capCell byte-slice left the suite green.
func TestActorCellCapsOnRuneBoundaries(t *testing.T) {
	role := "coder"
	long := strings.Repeat("あ", 200) // 200 runes, 600 bytes
	cell := actorCell(apitypes.MessageDTO{Agent: &role, AgentLabel: &long})
	if !utf8.ValidString(cell) {
		t.Errorf("capping split a multibyte rune, emitting invalid UTF-8: %q", cell)
	}
	if n := utf8.RuneCountInString(cell); n != actorCellWidth {
		t.Errorf("capped cell is %d runes, want exactly %d: %q", n, actorCellWidth, cell)
	}
	if !strings.HasSuffix(cell, "…") {
		t.Errorf("a capped cell should end with the ellipsis: %q", cell)
	}
}

// derefOr renders a *string for a %q error message without panicking on nil.
func derefOr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// PRD #108 M9b. docs/run-auto-stopped.md's first remedy for an auto-stopped run is
// "check the worker's version" — v0.10.1+ isolates a poisoned message itself, so an
// upgrade is the real fix. The web has shown it since PRD #42; the CLI did not, so
// the page shipped a remedy one of its two audiences could not follow.
func TestWorkerListShowsVersion(t *testing.T) {
	v := "v0.10.1"
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
		{ID: "w1", Name: "alpha", Status: "online", Version: &v},
		{ID: "w2", Name: "beta", Status: "offline"}, // never registered a version
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "VERSION") || !strings.Contains(out, v) {
		t.Fatalf("worker list does not surface the version, so the doc's first remedy is unfollowable from the CLI:\n%s", out)
	}
	if !strings.Contains(out, "-") {
		t.Errorf("a worker that never registered a version should render \"-\", not an empty cell:\n%s", out)
	}
}

// Issue #124: `version` is worker self-reported and Printer.Table Fprintln's cells through
// a tabwriter with no scrub of its own, so this was the ONE free-text sink in the CLI not
// routed through sanitizeTTY while every other one is. A bidi override here reorders a
// fleet-list row in an operator's terminal.
func TestWorkerListSanitizesVersion(t *testing.T) {
	spoof := "0.11\u202e.0\u200b"
	onlyFormat := "\u202e\u200b"
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
		{ID: "w1", Name: "alpha", Status: "online", Version: &spoof},
		{ID: "w2", Name: "beta", Status: "online", Version: &onlyFormat},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	for _, r := range out {
		if unicode.In(r, unicode.Cf) {
			t.Errorf("%U reached the terminal in a worker-list row:\n%s", r, out)
		}
	}
	if !strings.Contains(out, "0.11.0") {
		t.Errorf("the version must still be readable once stripped:\n%s", out)
	}
	// A version that is NOTHING but format characters compacts to "" and must still read
	// as the placeholder, not as a blank cell — which is why the sanitize runs before the
	// strOr fallback rather than after it.
	//
	// Asserted on beta's VERSION CELL, not on the whole output. `strings.Contains(out, "-")`
	// looks like it guards this and cannot fail: upgradeCell returns "-" for an empty
	// UpgradeStatus (worker.go), which BOTH fixture workers have, so the UPGRADE column
	// satisfies it whatever VERSION renders. Measured — with the placeholder removed
	// entirely, so an all-Cf version renders a blank cell, that assertion stayed green.
	// Columns are ID NAME STATUS VERSION UPGRADE, so a blank VERSION collapses the row to
	// four fields and the length check is what catches it.
	var betaRow []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == "beta" {
			betaRow = f
			break
		}
	}
	if betaRow == nil {
		t.Fatalf("no row for worker beta:\n%s", out)
	}
	if len(betaRow) != 5 || betaRow[3] != "-" {
		t.Errorf("an all-format-character version must render \"-\" in the VERSION cell, got %q:\n%s", betaRow, out)
	}
}

// PRD #113 M5: the UPGRADE column. `uzi worker list` is a first-class second consumer of
// the same DTO the web badge reads, so a worker that is failing an upgrade must be visible
// here too — the CLI is where an operator already is when a run stalls.
func TestWorkerListShowsUpgradeStatus(t *testing.T) {
	v := "0.11.0"
	cur := "0.11.7"
	fc := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{
		{ID: "w1", Name: "behind", Status: "online", Version: &v, UpgradeStatus: "outdated", UpgradeTarget: "0.11.7"},
		{ID: "w2", Name: "broken", Status: "offline", Version: &v, UpgradeStatus: "upgrade_failed", UpgradeTarget: "0.11.7"},
		{ID: "w3", Name: "fine", Status: "online", Version: &cur, UpgradeStatus: "up_to_date", UpgradeTarget: "0.11.7"},
		{ID: "w4", Name: "local", Status: "online", UpgradeStatus: "unknown"},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "worker", "list")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out, "UPGRADE") {
		t.Fatalf("no UPGRADE column:\n%s", out)
	}
	for _, want := range []string{"outdated", "FAILED", "up to date"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// `unknown` renders as "-", not as the word. An unstamped local build, an
	// unparseable report and a `dev` control plane all classify unknown, and none is a
	// finding — a column of "unknown" in front of every local developer trains them to
	// ignore this column entirely.
	if strings.Contains(out, "unknown") {
		t.Errorf("the unknown state rendered as the word rather than \"-\":\n%s", out)
	}
}

// parkFake walks a run through a scripted status sequence, one step per GetRun poll,
// repeating the last entry forever. It exists because the park behaviour under test is
// about TRANSITIONS — the notice fires on the edge into a park and again on the edge
// out — and a fake with a single status cannot express an edge at all.
type parkFake struct {
	*uzicli.FakeClient
	statuses []string
	calls    int
}

func (f *parkFake) GetRun(_ context.Context, id string) (apitypes.RunDTO, error) {
	i := f.calls
	f.calls++
	if i >= len(f.statuses) {
		i = len(f.statuses) - 1
	}
	retry := time.Now().Add(90 * time.Minute)
	rlt := "five_hour"
	return apitypes.RunDTO{
		ID: id, Status: f.statuses[i],
		RetryNotBefore: &retry, RateLimitType: &rlt, LimitWaitCount: 1,
	}, nil
}

// `uzi run logs --follow` must RIDE OUT a usage-limit park rather than exiting, and must
// say why it has gone quiet (PRD #35).
//
// Three properties, and the reason each is here:
//
//   - It does not exit. limit_wait is absent from terminalRunStatuses, so the loop keeps
//     polling. Exiting would truncate the capture mid-run on a run that completes
//     normally hours later — the failure this milestone exists to prevent.
//   - The notice fires ONCE per park, on the edge. The loop re-polls every 2s and a park
//     lasts hours, so a notice keyed on the status rather than the transition would emit
//     thousands of identical lines into an agent's stderr.
//   - The notices go to STDERR. Stdout is the Printer's, and `--json` streams NDJSON
//     there for an agent to parse line by line; a human-readable line on that stream
//     would corrupt the contract.
func TestRunLogsFollowRidesOutALimitWaitPark(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	defer func() { logsPollInterval = old }()

	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"hi"}`)}},
	}}
	// 🔴 THE RESUMED STATUS CARRIES A NEWLINE, and the fake is the only place it can
	// come from — which is the point. runs_status_check makes this unreachable through
	// the API today, and the renderer's safety must not depend on a guarantee made in
	// another package (the same argument the ANTHROPIC_TOKEN test makes at length).
	// sanitizeTTY spares "\n"; only cellText folds it. With the weaker helper the
	// injected text lands on its own stderr line.
	pf := &parkFake{FakeClient: fc, statuses: []string{
		"running",
		"limit_wait", "limit_wait", "limit_wait", // one park, three polls
		"running\nINJECTED",
		"completed",
	}}

	var out, errBuf bytes.Buffer
	env := fakeEnv(pf)
	env.Stdout = &out
	env.Stderr = &errBuf

	done := make(chan int, 1)
	go func() { done <- Main(env, []string{"run", "logs", "r1", "--follow"}) }()

	select {
	case code := <-done:
		if code != uzicli.ExitOK {
			t.Fatalf("--follow exit = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run logs --follow hung; it must ride out a park and then terminate when the run completes")
	}

	// It got PAST the park rather than stopping in it: the completed status is only
	// reachable at poll 6.
	if pf.calls < 6 {
		t.Errorf("GetRun polled %d times, want >= 6 — --follow exited during the park instead of riding it out", pf.calls)
	}

	stderr := errBuf.String()
	if n := strings.Count(stderr, "paused"); n != 1 {
		t.Errorf("park notice appeared %d times, want exactly 1 — it must fire on the EDGE into the park, not on every poll of a park that lasts hours:\n%s", n, stderr)
	}
	if !strings.Contains(stderr, "five_hour") || !strings.Contains(stderr, "resumes in") {
		t.Errorf("the park notice must say which window and when it resumes, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "resumed") {
		t.Errorf("no notice on the way OUT of the park; a user told the run paused must be told it restarted, got:\n%s", stderr)
	}

	// The discriminator for cellText over sanitizeTTY on the resumed branch.
	if strings.Contains(stderr, "\nINJECTED") {
		t.Errorf("a newline in the run status injected a line onto stderr — the resumed notice must fold it (cellText), not spare it (sanitizeTTY), got:\n%q", stderr)
	}
	if !strings.Contains(stderr, "running INJECTED") {
		t.Errorf("the folded status lost its text entirely, got:\n%q", stderr)
	}

	// Stdout stays the message stream and nothing else.
	if !strings.Contains(out.String(), "hi") {
		t.Errorf("--follow dropped the message:\n%s", out.String())
	}
	if strings.Contains(out.String(), "paused") || strings.Contains(out.String(), "resumed") {
		t.Errorf("a park notice reached STDOUT, which is the --json NDJSON stream an agent parses line by line:\n%s", out.String())
	}
}

// TestRunCreateWaitOnLimitIsTriState is the whole point of the flag, and the case that
// makes it more than a switch: OMITTING it must send NOTHING, so the server stamps the
// run from the owner's Settings default (PRD #35 Decision 7, and the requirement
// specs/human.md states as "wait_on_limit on run creation for CLI/API callers").
//
// The fake keeps the POINTER, so all three states are distinguishable here. A bool
// capture would collapse the first two rows into one and the test would pass against
// exactly the implementation it exists to reject.
func TestRunCreateWaitOnLimitIsTriState(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")

	run := func(t *testing.T, extra ...string) *uzicli.FakeClient {
		t.Helper()
		fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "queued"}}
		env := fakeEnv(fc)
		env.Stdout = &bytes.Buffer{}
		env.Stderr = &bytes.Buffer{}
		args := append([]string{"run", "create", "--repo", "p1", "--issue", "42"}, extra...)
		if code := Main(env, args); code != uzicli.ExitOK {
			t.Fatalf("run create %v exit = %d, want 0", extra, code)
		}
		return fc
	}

	// 🔴 THE ROW THAT MATTERS. The flag is DEFINED with a default of false, so GetBool
	// returns false here — identical to an explicit --wait-on-limit=false. Passing that
	// through would send "wait_on_limit": false on every CLI-created run and silently
	// override the user's own Settings default. Only Changed() separates them.
	if got := run(t).LastCreateWaitOnLimit; got != nil {
		t.Errorf("omitting --wait-on-limit sent %v, want nil — an absent flag must send NO key so the server inherits the user's Settings default; sending false here silently opts every CLI-created run out", *got)
	}

	if got := run(t, "--wait-on-limit").LastCreateWaitOnLimit; got == nil || !*got {
		t.Errorf("--wait-on-limit sent %v, want an explicit true", got)
	}

	// Explicit false is a DIFFERENT statement from absent: "this run, specifically, must
	// not park", overriding a default that is on.
	if got := run(t, "--wait-on-limit=false").LastCreateWaitOnLimit; got == nil || *got {
		t.Errorf("--wait-on-limit=false sent %v, want an explicit false — it must override a user default of true, not fall back to it", got)
	}

	// The repo/issue arguments still ride through unchanged.
	fc := run(t, "--wait-on-limit")
	if fc.LastCreateRepoID != "p1" || fc.LastCreateIssueIID != 42 {
		t.Errorf("create sent repo=%q issue=%d, want p1/42", fc.LastCreateRepoID, fc.LastCreateIssueIID)
	}
}

// `--wait-on-limit false` (a SPACE instead of `=`) must FAIL LOUDLY rather than
// silently meaning true. pflag reads a bare bool flag as true and leaves "false" as a
// positional argument; `create` is cobra.NoArgs, so the stray argument is a usage
// error. This pins that it stays loud — if `create` ever gains positional arguments,
// this inversion becomes silent and needs a guard of its own.
func TestRunCreateWaitOnLimitSpaceFormIsAUsageError(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	fc := &uzicli.FakeClient{CreatedRun: apitypes.RunDTO{ID: "r1"}}
	env := fakeEnv(fc)
	env.Stdout = &bytes.Buffer{}
	env.Stderr = &bytes.Buffer{}

	code := Main(env, []string{"run", "create", "--repo", "p1", "--issue", "42", "--wait-on-limit", "false"})
	if code != uzicli.ExitUsage {
		t.Errorf("`--wait-on-limit false` exit = %d, want %d (usage) — the space form must not silently mean true", code, uzicli.ExitUsage)
	}
	if fc.LastCreateRepoID != "" {
		t.Error("a usage error still created a run; the command must refuse before calling the API")
	}
}
