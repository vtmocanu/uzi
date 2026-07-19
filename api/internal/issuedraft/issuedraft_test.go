package issuedraft

import (
	"strings"
	"testing"
	"time"
)

func baseInput() Input {
	return Input{
		Category:            "install_worker_tool",
		Target:              "shellcheck",
		RationaleMd:         "The reviewer invoked shellcheck; command not found.",
		Confidence:          "high",
		Verdict:             "issues",
		SummaryMd:           "The run shipped an MR but lost lint coverage.",
		JudgeModel:          "sonnet",
		ReviewDate:          time.Date(2026, 7, 17, 9, 0, 0, 0, time.UTC),
		RunKind:             "issue",
		RunStatus:           "completed",
		RepoPath:            "vtmocanu/uzi",
		IssueIID:            58,
		RunURL:              "https://uzi.example/runs/8f2c1d04",
		RequestingUser:      "vmocanu",
		ProducingUser:       "vmocanu",
		ProducingRunShortID: "8f2c1d04",
	}
}

func TestRenderBasicShape(t *testing.T) {
	d := Render(baseInput())
	if d.Title != "Install a worker tool: shellcheck" {
		t.Fatalf("title = %q", d.Title)
	}
	for _, want := range []string{
		"## What the judge found",
		"## Context",
		"- Recommendation: **Install a worker tool** — ` shellcheck ` (high confidence)",
		"- Verdict on the judged run: **Issues found**",
		"- Judged run: [issue run on vtmocanu/uzi#58](https://uzi.example/runs/8f2c1d04) (completed)",
		"- Retrospective by ` sonnet `, 2026-07-17",
		"## Judge's summary of the run",
		"Opened by uzi on behalf of @` vmocanu `, from a retrospective of ` vmocanu `'s run 8f2c1d04",
		"quick-actions render inert.",
	} {
		if !strings.Contains(d.Description, want) {
			t.Fatalf("body missing %q\n---\n%s", want, d.Description)
		}
	}
	if d.Provenance != "from vmocanu's worker, run 8f2c1d04" {
		t.Fatalf("provenance = %q", d.Provenance)
	}
}

func TestRenderEmptyTargetDropsInline(t *testing.T) {
	in := baseInput()
	in.Category = "improve_uzi"
	in.Target = ""
	d := Render(in)
	if d.Title != "Improve uzi" {
		t.Fatalf("title = %q, want %q", d.Title, "Improve uzi")
	}
	if strings.Contains(d.Description, " — ") {
		t.Fatalf("empty target must not render an inline-code dash:\n%s", d.Description)
	}
	if !strings.Contains(d.Description, "- Recommendation: **Improve uzi** (high confidence)") {
		t.Fatalf("recommendation line wrong:\n%s", d.Description)
	}
}

// The load-bearing security test: hostile rationale carrying its OWN triple-backtick
// run plus a live image beacon and a quick-action line must not break out. The fence
// delimiter must be longer than the injected run, the injected lines stay INSIDE the
// fence (inert, not executed, not stripped), and no unfenced "/"-line survives.
func TestRenderFenceBreakoutProof(t *testing.T) {
	in := baseInput()
	in.RationaleMd = "look here\n```\n> ![beacon](https://evil.example/p.png)\n/label ~autopilot\n```\ntrailing"
	d := Render(in)

	if !strings.Contains(d.Description, "````") {
		t.Fatalf("fence must be >= 4 backticks to contain a 3-backtick run:\n%s", d.Description)
	}
	// The beacon and the quick-action line are contained as fenced text (present, inert),
	// NOT re-exposed as live markdown and NOT stripped (fenced content is left verbatim).
	if !strings.Contains(d.Description, "![beacon](https://evil.example/p.png)") {
		t.Fatalf("beacon text should be present but fenced:\n%s", d.Description)
	}
	if !strings.Contains(d.Description, "/label ~autopilot") {
		t.Fatalf("fenced quick-action line must be kept verbatim inside the fence:\n%s", d.Description)
	}
	// Prove the injected 3-backtick run did NOT terminate the block early: the "trailing"
	// text and the "/label" line sit between the opening ```` and the matching closing ````.
	open := strings.Index(d.Description, "````")
	closeIdx := strings.Index(d.Description[open+4:], "````")
	if closeIdx < 0 {
		t.Fatalf("no closing 4-backtick fence found:\n%s", d.Description)
	}
	fenced := d.Description[open : open+4+closeIdx]
	if !strings.Contains(fenced, "/label ~autopilot") || !strings.Contains(fenced, "trailing") {
		t.Fatalf("hostile lines escaped the fence:\n%s", fenced)
	}
}

func TestStripUnfencedSlashLines(t *testing.T) {
	in := strings.Join([]string{
		"intro",
		"/label ~autopilot", // unfenced → dropped
		"  /assign @me",     // leading spaces then / → dropped
		"```",
		"/close",   // fenced → kept
		"code line",
		"```",
		"/relabel", // unfenced again → dropped
		"tail",
	}, "\n")
	got := StripUnfencedSlashLines(in)
	if strings.Contains(got, "~autopilot") || strings.Contains(got, "/assign") || strings.Contains(got, "/relabel") {
		t.Fatalf("unfenced quick-action lines survived:\n%s", got)
	}
	if !strings.Contains(got, "/close") {
		t.Fatalf("a fenced /-line must be kept:\n%s", got)
	}
	for _, keep := range []string{"intro", "code line", "tail"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("dropped a legitimate line %q:\n%s", keep, got)
		}
	}
}

func TestStripUnfencedSlashLinesCRLF(t *testing.T) {
	// GitLab CRLF-normalizes before quick-action processing, so a fence closed with a
	// trailing \r (or a bare-\r body) must be treated as CLOSED here and the following
	// column-0 /label line stripped (audit Medium-1 — the CRLF-blind bypass).
	cases := map[string]string{
		"crlf":   "```\r\ncode\r\n```\r\n/label ~autopilot\r\n/close\r\n",
		"bareCR": "```\rcode\r```\r/label ~autopilot\r/close\r",
	}
	for name, body := range cases {
		got := StripUnfencedSlashLines(body)
		if strings.Contains(got, "/label") || strings.Contains(got, "/close") {
			t.Fatalf("%s: CRLF-blind strip left a quick-action line: %q", name, got)
		}
		if !strings.Contains(got, "code") {
			t.Fatalf("%s: fenced code was lost: %q", name, got)
		}
	}
}

func TestSanitizeTitleCRLF(t *testing.T) {
	// A CRLF/­bare-CR title with a leading quick-action must collapse to one line and lose
	// its leading "/" (never open with a quick-action character).
	for _, in := range []string{"/label ~x\r\nmake the fix", "/label ~x\rmake the fix"} {
		got := SanitizeTitle(in)
		if strings.HasPrefix(got, "/") || strings.ContainsAny(got, "\r\n") {
			t.Fatalf("SanitizeTitle(%q) = %q; want single line, no leading slash", in, got)
		}
	}
}

func TestSanitizeFiledBodyCRLFAttack(t *testing.T) {
	body := "intro\r\n```\r\nx\r\n```\r\n/label ~autopilot\r\n"
	got := SanitizeFiledBody(body)
	if strings.Contains(got, "/label") {
		t.Fatalf("SanitizeFiledBody did not strip a CRLF-hidden quick-action:\n%q", got)
	}
}

func TestFooterNeutralizesHostileUsername(t *testing.T) {
	in := baseInput()
	in.RequestingUser = "[x](https://evil.example)"
	in.ProducingUser = "@everyone"
	d := Render(in)
	// A hostile display name is wrapped in inline code, so the "](url)" is inert and the
	// "@everyone" cannot fire a real GitLab mention.
	if !strings.Contains(d.Description, "` [x](https://evil.example) `") {
		t.Fatalf("hostile requesting-user name was not fenced in inline code:\n%s", d.Description)
	}
	if !strings.Contains(d.Description, "` @everyone `'s run") {
		t.Fatalf("producing-user name was not fenced (would render as a live mention):\n%s", d.Description)
	}
}

func TestRunReferenceRepoPathBreakout(t *testing.T) {
	in := baseInput()
	in.RepoPath = "evil](https://evil.example) ["
	d := Render(in)
	if strings.Contains(d.Description, "](https://evil.example)") {
		t.Fatalf("repo path broke out of the markdown link label:\n%s", d.Description)
	}
}

func TestVariableFenceLengthStrip(t *testing.T) {
	// A 4-backtick fence (as Render emits around content with a 3-backtick run) must be
	// tracked as a fence so its inner /-lines are not stripped and a 3-backtick line
	// inside does not prematurely close it.
	in := "````\n```\n/label ~x\n````\n/relabel"
	got := StripUnfencedSlashLines(in)
	if !strings.Contains(got, "/label ~x") {
		t.Fatalf("content inside a 4-backtick fence was stripped:\n%s", got)
	}
	if strings.Contains(got, "/relabel") {
		t.Fatalf("the unfenced trailing /-line should be dropped:\n%s", got)
	}
}

func TestScrubSecretShapes(t *testing.T) {
	// Fixtures are ASSEMBLED from parts so a contiguous credential exists only at
	// runtime — the source never carries one, which keeps the repo secret-scanner quiet
	// while still exercising the scrubber against realistic secret SHAPES.
	redacted := []string{
		"AKIA" + strings.Repeat("A", 16),                                                       // AWS access key id
		"ghp_" + strings.Repeat("a", 36),                                                       // GitHub token
		"glpat-" + strings.Repeat("x", 20),                                                     // GitLab PAT
		"sk-ant-" + "api03" + strings.Repeat("Z", 16),                                          // Anthropic
		"xoxb-" + strings.Repeat("9", 10) + "-abcXYZ",                                          // Slack
		"eyJ" + strings.Repeat("A", 24) + "." + strings.Repeat("B", 16) + "." + strings.Repeat("C", 16), // JWT
		"postgres://app:" + "s3cr3tpw" + "@db.internal:5432/uzi",                               // DB URL creds
		"Authorization: Bearer " + strings.Repeat("t", 24),                                     // bearer header
	}
	for _, sec := range redacted {
		out := ScrubSecretShapes("prefix " + sec + " suffix")
		if strings.Contains(out, sec) {
			t.Fatalf("secret not redacted: %q -> %q", sec, out)
		}
		if !strings.Contains(out, "[redacted]") {
			t.Fatalf("expected a [redacted] marker for %q -> %q", sec, out)
		}
	}
	// A PEM block spanning lines (marker split so the source has no contiguous header).
	pem := "-----BEGIN RSA PRIVATE" + " KEY-----\nMII" + strings.Repeat("Z", 20) + "\n-----END RSA PRIVATE" + " KEY-----"
	if strings.Contains(ScrubSecretShapes(pem), "ZZZ") {
		t.Fatalf("PEM private key not redacted")
	}
	// False-positive guard: a git SHA and a UUID are NOT credentials and must survive.
	keep := "commit 9fceb02d0 ver 550e8400-e29b-41d4-a716-446655440000"
	if ScrubSecretShapes(keep) != keep {
		t.Fatalf("a SHA/UUID was wrongly redacted: %q", ScrubSecretShapes(keep))
	}
}

func TestSecretInRationaleIsScrubbedInBody(t *testing.T) {
	awsKey := "AKIA" + strings.Repeat("A", 16) // assembled — see TestScrubSecretShapes
	in := baseInput()
	in.RationaleMd = "the trace leaked " + awsKey + " into the log"
	d := Render(in)
	if strings.Contains(d.Description, awsKey) {
		t.Fatalf("a secret quoted in rationale must be scrubbed in the filed body:\n%s", d.Description)
	}
}

func TestSafeInlineCodeBreakoutProof(t *testing.T) {
	// content with a 2-backtick run needs a 3-backtick delimiter.
	got := SafeInlineCode("a`b``c")
	if !strings.HasPrefix(got, "```") || !strings.HasSuffix(got, "```") {
		t.Fatalf("delimiter must exceed the longest inner run: %q", got)
	}
	// newlines collapse so the span stays inline.
	if strings.Contains(SafeInlineCode("a\nb"), "\n") {
		t.Fatalf("inline code must not contain a newline")
	}
}

func TestSanitizeTitle(t *testing.T) {
	if got := SanitizeTitle("/label ~autopilot make the fix"); strings.HasPrefix(got, "/") {
		t.Fatalf("title must not open with a slash: %q", got)
	}
	if got := SanitizeTitle("multi\nline\ttitle"); got != "multi line title" {
		t.Fatalf("title should collapse whitespace: %q", got)
	}
	long := strings.Repeat("x", 400)
	if got := SanitizeTitle(long); len(got) > titleMax {
		t.Fatalf("title not capped: len=%d", len(got))
	}
}

func TestSanitizeFiledBodyIdempotent(t *testing.T) {
	body := "intro\n/label ~x\n" + "AKIA" + strings.Repeat("A", 16) + "\n```\n/close\n```"
	once := SanitizeFiledBody(body)
	if twice := SanitizeFiledBody(once); twice != once {
		t.Fatalf("SanitizeFiledBody not idempotent:\n%q\n%q", once, twice)
	}
}
