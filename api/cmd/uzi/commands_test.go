package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
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
		"worker", "repo", "admin", "skill", "version",
	}
	for _, name := range topWant {
		if findCmd(root, name) == nil {
			t.Errorf("missing top-level command %q", name)
		}
	}

	subWant := map[string][]string{
		"run":    {"list", "get", "logs", "review", "create", "approve", "reject", "cancel", "follow-up", "inputs"},
		"review": {"show", "resolve", "dismiss", "undo", "stats"},
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
	if got := shortInstanceID("toolu_A"); got != "olu_A" && got != "oolu_A" {
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
func TestRunLogsSanitizesAgentLabelForTTY(t *testing.T) {
	coder := "coder"
	inst := "toolu_01AAAAAAAAAAAAAAAA3v6ptu"
	label := "\x1b[2Jwipe" // CSI screen-clear
	fc := &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
		"r1": {{Seq: 1, Kind: "text", Agent: &coder, AgentInstance: &inst, AgentLabel: &label, Payload: []byte(`{}`)}},
	}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "logs", "r1")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("an ESC in agent_label reached the TTY:\n%q", out)
	}
	if !strings.Contains(out, "[2Jwipe") {
		t.Errorf("sanitizing must drop the control byte only, keeping visible text:\n%s", out)
	}
}

// The actor cell holds a `·` separator and a possibly non-ASCII label, and the
// payload column must start at the same RUNE offset on every line. Go's fmt pads
// `%-*s` by runes, so this passes — but pinning it guards against a future switch to
// a byte-based manual pad (an earlier draft of this code shipped exactly such a
// helper, on the false belief that fmt pads by bytes; the mutation test that should
// have caught it did not, because fmt was already doing the right thing). Rune
// alignment only: a CJK rune still takes two terminal columns, which this CLI does
// not model anywhere.
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
