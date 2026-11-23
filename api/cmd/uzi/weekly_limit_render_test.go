package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/shellquote"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func weeklyF(v float64) *float64 { return &v }

func weeklyBlock() *apitypes.WeeklyLimitDTO {
	reset := time.Date(2026, 11, 27, 9, 30, 0, 0, time.UTC)
	// Held in plain locals: gosec G101 reads a literal under a Credential-named field as a secret.
	owner, key := "team-a", "anthropic:s1"
	return &apitypes.WeeklyLimitDTO{
		CredentialLabel: owner, CredentialKey: key, Harness: "claude",
		Pct: weeklyF(96), EffectiveLimit: 90, ResetsAt: &reset,
		Tier: apitypes.WeeklyLimitTierWarning, Reason: "can_switch", AlternativeLabel: "team-b",
		Fix: []apitypes.WeeklyLimitFixDTO{
			{Kind: "set_token", TargetID: "s2", TargetLabel: "team-b", Command: "uzi run set-token r1 -- team-b"},
			{Kind: "edit_reserve"},
		},
		FutureRuns: []apitypes.WeeklyLimitFixDTO{
			{Kind: "pool_on", TargetID: "s3", TargetLabel: "odd", CommandUnavailable: apitypes.WeeklyFixCommandLabel},
		},
		Wait: "waits",
	}
}

func weeklyRun() apitypes.RunDTO {
	return apitypes.RunDTO{ID: "r1", Kind: "issue", Status: "limit_wait", IssueTitle: "t", Health: "ok", WeeklyLimit: weeklyBlock()}
}

func weeklyDetailString(t *testing.T, r apitypes.RunDTO) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderRunDetail(uzicli.NewPrinter(&buf, false, false, true, false), r); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// TestRenderRunDetailWeeklyLimitRows: the block renders as a WEEKLY_LIMIT row plus one FIX row per
// fix with a command, future-run advice marked as such, a label-unsafe fix saying how to get a
// command, and no row at all for a run without the block. Reddening mutation: drop the
// weeklyLimitCell block in renderRunDetailAt.
func TestRenderRunDetailWeeklyLimitRows(t *testing.T) {
	out := weeklyDetailString(t, weeklyRun())
	row := lineWith(t, out, "WEEKLY_LIMIT")
	for _, want := range []string{"team-a", "96%", "90%", "2026-11-27T09:30:00Z", "team-b", "waits for the reset"} {
		if !strings.Contains(row, want) {
			t.Errorf("WEEKLY_LIMIT row lacks %q: %q", want, row)
		}
	}
	if fix := lineWith(t, out, "FIX "); !strings.Contains(fix, "uzi run set-token r1 -- team-b") {
		t.Errorf("FIX row = %q", fix)
	}
	if fut := lineWith(t, out, "FIX_FUTURE_RUNS"); !strings.Contains(fut, "rename this token in Settings to get a copyable command") || !strings.Contains(fut, "odd") {
		t.Errorf("future-run row = %q", fut)
	}
	fixRows := 0
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == "FIX" {
			fixRows++
			if strings.Contains(l, "edit_reserve") {
				t.Errorf("the raw fix kind leaked into a row: %q", l)
			}
		}
	}
	// set_token and edit_reserve: the Settings-only fix has no command but still gets a row.
	if fixRows != 2 {
		t.Errorf("FIX rows = %d, want 2:\n%s", fixRows, out)
	}
	if !strings.Contains(out, "raise or turn off the weekly reserve in Settings → Run defaults (or `uzi settings reserve --threshold <1-100|off>`)") {
		t.Errorf("edit_reserve row text missing:\n%s", out)
	}
	plain := weeklyRun()
	plain.WeeklyLimit = nil
	if out := weeklyDetailString(t, plain); strings.Contains(out, "WEEKLY_LIMIT") || strings.Contains(out, "FIX") {
		t.Errorf("a run without the block gained weekly rows:\n%s", out)
	}
}

// TestRenderRunDetailWeeklyLimitHostileAndLongCommand: owner-authored labels are folded and bounded,
// while the FIX command cell is sanitized but never truncated.
func TestRenderRunDetailWeeklyLimitHostileAndLongCommand(t *testing.T) {
	r := weeklyRun()
	w := weeklyBlock()
	w.CredentialLabel = "evil\x1b[2J\u202ex\n" + strings.Repeat("L", 300)
	longCmd := "uzi run set-token r1 -- " + strings.Repeat("c", 400)
	w.Fix = []apitypes.WeeklyLimitFixDTO{{Kind: "set_token", Command: longCmd + "\x1b]0;x\x07"}}
	r.WeeklyLimit = w
	out := weeklyDetailString(t, r)
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x07) || strings.ContainsRune(out, '\u202e') {
		t.Errorf("control bytes reached the output: %q", out)
	}
	if !strings.Contains(lineWith(t, out, "FIX "), longCmd) {
		t.Errorf("the FIX command was truncated or altered")
	}
	if strings.Contains(lineWith(t, out, "WEEKLY_LIMIT"), strings.Repeat("L", 100)) {
		t.Errorf("the credential label was not bounded")
	}
}

// TestRunGetJSONCarriesWeeklyLimit: `uzi run get --json` already carries the block.
func TestRunGetJSONCarriesWeeklyLimit(t *testing.T) {
	fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{"r1": weeklyRun()}}
	out, _, code := runCLI(t, fakeEnv(fc), "run", "get", "r1", "--json")
	if code != uzicli.ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var got struct {
		WeeklyLimit *apitypes.WeeklyLimitDTO `json:"weekly_limit"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if got.WeeklyLimit == nil || got.WeeklyLimit.CredentialLabel != "team-a" || len(got.WeeklyLimit.Fix) != 2 ||
		got.WeeklyLimit.Fix[0].Command != "uzi run set-token r1 -- team-b" {
		t.Errorf("weekly_limit not carried: %+v", got.WeeklyLimit)
	}
}

// TestWeeklyLimitDetailFitsAndReservesRow: the TUI line is one physical row at every width and the
// transcript viewport is charged exactly that row (the #379 invariant). Reddening mutation: drop
// the weeklyLimitTUILine term in transcriptBudgetWithoutDiskGuidance.
func TestWeeklyLimitDetailFitsAndReservesRow(t *testing.T) {
	now := time.Now()
	for _, width := range []int{60, 80, 200} {
		run := weeklyRun()
		run.Status = "running"
		run.WeeklyLimit.CredentialLabel = "team\x1b[31m-a"
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m = applyDetail(m, run, []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "one short line", now)})
		m = step(m, tea.WindowSizeMsg{Width: width, Height: 30})
		out := stripANSI(m.View().Content)
		if !strings.Contains(out, "weekly limit: team") {
			t.Fatalf("width %d: weekly-limit line missing:\n%s", width, out)
		}
		rows := strings.Split(out, "\n")
		for i, row := range rows {
			if w := visualWidth(row); w > width {
				t.Errorf("width %d: row %d is %d wide: %q", width, i, w, row)
			}
		}
		if len(rows) != m.height {
			t.Errorf("width %d: frame is %d rows, want %d\n%s", width, len(rows), m.height, out)
		}
		if strings.Contains(m.View().Content, "team\x1b[31m") {
			t.Errorf("width %d: hostile label escape survived", width)
		}
	}
	vp := func(r apitypes.RunDTO) int {
		m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
		m.width, m.height = 100, 30
		return applyDetail(m, r, nil).transcriptViewport()
	}
	with := weeklyRun()
	with.Status = "running"
	without := with
	without.WeeklyLimit = nil
	if a, b := vp(with), vp(without); a != b-1 {
		t.Errorf("transcriptViewport with block = %d, want %d (one row fewer than %d)", a, b-1, b)
	}
}

// posixSplit splits a command line the way a POSIX shell does for the forms shellquote.Quote emits:
// unquoted words, single-quoted spans and backslash-escaped characters. Any expansion syntax is left
// literal, which is the point: a quoted word must reach the program unchanged.
func posixSplit(t *testing.T, s string) []string {
	t.Helper()
	var args []string
	var cur strings.Builder
	inWord, inSingle := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(c)
			}
		case c == '\'':
			inSingle, inWord = true, true
		case c == '\\':
			if i+1 >= len(s) {
				t.Fatalf("dangling backslash in %q", s)
			}
			i++
			cur.WriteByte(s[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inSingle {
		t.Fatalf("unterminated quote in %q", s)
	}
	if inWord {
		args = append(args, cur.String())
	}
	return args
}

// TestWeeklyFixCommandsRoundTripThroughRealCLI: commands in the grammar weeklylimit emits (built
// with shellquote.Quote; the weeklylimit tests pin the exact server-side strings) shell-split and
// run through the real command tree, and each request targets the advertised credential and kind. Option-like labels
// (--default, --auto, -x), a hostile label and a label shared by an Anthropic token and a Codex
// login are the cases a naive command would send somewhere else.
func TestWeeklyFixCommandsRoundTripThroughRealCLI(t *testing.T) {
	hostile := "o'brien \"q\" $(touch x) $HOME " + "`" + "id" + "`" + " ; rm -rf * \\n"
	labels := []string{"--default", "--auto", "-x", hostile, "shared"}

	secrets := func() []apitypes.SecretDTO {
		var s []apitypes.SecretDTO
		for i, l := range labels {
			s = append(s, apitypes.SecretDTO{ID: "a" + string(rune('0'+i)), Kind: "anthropic_token", Label: l})
		}
		s = append(s, apitypes.SecretDTO{ID: "cx1", Kind: "codex_auth", Label: "shared"})
		return s
	}

	wantAnthropicID := func(label string) string {
		for _, s := range secrets() {
			if s.Kind == "anthropic_token" && s.Label == label {
				return s.ID
			}
		}
		t.Fatalf("no secret for %q", label)
		return ""
	}

	for _, label := range labels {
		q := shellquote.Quote(label)

		t.Run("set-token anthropic "+label[:min(len(label), 6)], func(t *testing.T) {
			fc := &uzicli.FakeClient{
				Secrets: secrets(),
				RunByID: map[string]apitypes.RunDTO{"r1": {ID: "r1", Harness: "claude"}},
			}
			args := posixSplit(t, "uzi run set-token r1 -- "+q)
			if args[0] != "uzi" || args[len(args)-1] != label {
				t.Fatalf("split = %q, want the label %q as the last word", args, label)
			}
			if _, errs, code := runCLI(t, fakeEnv(fc), args[1:]...); code != uzicli.ExitOK {
				t.Fatalf("exit %d: %s", code, errs)
			}
			if fc.LastSetTokenRunID != "r1" || fc.LastSetTokenOverride == nil ||
				fc.LastSetTokenOverride.Mode != "pinned" || fc.LastSetTokenOverride.SecretID != wantAnthropicID(label) {
				t.Errorf("request = run %q override %+v, want pinned %s", fc.LastSetTokenRunID, fc.LastSetTokenOverride, wantAnthropicID(label))
			}
		})

		t.Run("pool anthropic "+label[:min(len(label), 6)], func(t *testing.T) {
			fc := &uzicli.FakeClient{Secrets: secrets(), PoolSecret: apitypes.SecretDTO{Label: "x"}}
			args := posixSplit(t, "uzi token pool --on --kind anthropic -- "+q)
			if _, errs, code := runCLI(t, fakeEnv(fc), args[1:]...); code != uzicli.ExitOK {
				t.Fatalf("exit %d: %s", code, errs)
			}
			if fc.LastPoolSecretID != wantAnthropicID(label) || !fc.LastPoolValue || fc.CodexPoolCalls != 0 {
				t.Errorf("pool request id %q value %v codex calls %d, want %s on the Anthropic path", fc.LastPoolSecretID, fc.LastPoolValue, fc.CodexPoolCalls, wantAnthropicID(label))
			}
		})
	}

	t.Run("same label, Codex run pins the Codex login", func(t *testing.T) {
		fc := &uzicli.FakeClient{Secrets: secrets(), RunByID: map[string]apitypes.RunDTO{"r2": {ID: "r2", Harness: "codex"}}}
		args := posixSplit(t, "uzi run set-token r2 -- "+shellquote.Quote("shared"))
		if _, errs, code := runCLI(t, fakeEnv(fc), args[1:]...); code != uzicli.ExitOK {
			t.Fatalf("exit %d: %s", code, errs)
		}
		if fc.LastSetTokenOverride == nil || fc.LastSetTokenOverride.SecretID != "cx1" {
			t.Errorf("override = %+v, want pinned cx1", fc.LastSetTokenOverride)
		}
	})

	t.Run("same label, pool --kind codex targets the Codex login", func(t *testing.T) {
		fc := &uzicli.FakeClient{Secrets: secrets(), CodexPoolSecret: apitypes.SecretDTO{Label: "shared"}}
		args := posixSplit(t, "uzi token pool --on --kind codex -- "+shellquote.Quote("shared"))
		if _, errs, code := runCLI(t, fakeEnv(fc), args[1:]...); code != uzicli.ExitOK {
			t.Fatalf("exit %d: %s", code, errs)
		}
		if fc.LastCodexPoolSecretID != "cx1" || fc.CodexPoolCalls != 1 || fc.LastPoolSecretID != "" {
			t.Errorf("codex pool id %q calls %d anthropic id %q, want cx1 once and no Anthropic write", fc.LastCodexPoolSecretID, fc.CodexPoolCalls, fc.LastPoolSecretID)
		}
	})
}

// TestWeeklyWaitTextParkedVersusNearLimit: a run that is not parked is never described as paused or
// waiting, only conditionally; a parked run keeps the unconditional wording.
func TestWeeklyWaitTextParkedVersusNearLimit(t *testing.T) {
	seven, reserve := "seven_day", "weekly_reserve"
	cases := []struct {
		name    string
		status  string
		rl, hr  *string
		wait    string
		want    string
		notWant string
	}{
		{"running reserve_pause", "running", nil, nil, "reserve_pause", "if usage reaches the reserve, the run pauses until usage drops below it or you resume it", "stays paused"},
		{"running waits", "running", nil, nil, "waits", "if it reaches the limit, it waits for the reset", "the run waits for the reset"},
		{"running fails", "running", nil, nil, "fails", "if it reaches the limit, it fails", "instead of waiting"},
		{"parked limit_wait waits", "limit_wait", &seven, nil, "waits", "the run waits for the reset", "if it reaches"},
		{"parked weekly reserve pause", "paused", nil, &reserve, "reserve_pause", "the run stays paused until usage drops below the reserve or you resume it", "if usage"},
		{"five_hour limit_wait is not parked", "limit_wait", ptrS("five_hour"), nil, "waits", "if it reaches the limit, it waits for the reset", "the run waits for the reset"},
	}
	for _, c := range cases {
		r := weeklyRun()
		r.Status, r.RateLimitType, r.HoldReason = c.status, c.rl, c.hr
		w := weeklyBlock()
		w.Wait = c.wait
		r.WeeklyLimit = w
		cell, tui := weeklyLimitCell(r), weeklyLimitTUILine(r)
		for _, got := range []string{cell, tui} {
			if !strings.Contains(got, c.want) || (c.notWant != "" && strings.Contains(got, c.notWant)) {
				t.Errorf("%s: %q want %q, not %q", c.name, got, c.want, c.notWant)
			}
		}
	}
}

func ptrS(s string) *string { return &s }
