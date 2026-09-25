package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

// PRD #1590 layout and wiring pins for the Codex account hold surfaces: the detail hold line
// fits the terminal, both TUI layouts reserve the rows they draw for it, the run tables keep a
// bounded STATUS cell, the `run logs --follow` notice fires, and a relogin_required hold bands
// into NEEDS YOU.

// longCodexLabel is a 64-byte alias label: long enough that the relogin sentence overflows an
// 80-column terminal on its own.
const longCodexLabel = "team-shared-codex-subscription-for-the-nightly-sweep-runners-001"

func codexHeld(id, action, label string) apitypes.RunDTO {
	cause := codexAccountUnavailableCause
	r := apitypes.RunDTO{ID: id, Kind: "issue", Status: statusRecoveryWait, IssueTitle: "held " + id, Health: "ok",
		RecoveryWaitCause: &cause, CodexAccountAction: &action}
	if label != "" {
		r.CodexSecretLabel = &label
	}
	return r
}

// TestCodexHoldDetailFitsTerminal: the detail's Codex hold line is clamped to m.width, and the
// frame is exactly m.height rows, at 60 and 80 columns with a 64-byte label. Mutation-checked:
// drawing the line without clampVisual (the original m.renderer.Plain(line, 120)) reddened the
// width assertion at both widths; dropping the codex term from transcriptViewport reddened the
// height assertion (the frame came to m.height+1 rows).
func TestCodexHoldDetailFitsTerminal(t *testing.T) {
	if len(longCodexLabel) != 64 {
		t.Fatalf("fixture label is %d bytes, want 64", len(longCodexLabel))
	}
	now := time.Now()
	for _, width := range []int{60, 80} {
		run := codexHeld("77777777-codex", codexActionReloginRequired, longCodexLabel)
		m := tuiTestModel(t, &uzicli.FakeClient{}, run.ID)
		m = applyDetail(m, run, []apitypes.MessageDTO{msgDTO(1, "text", "lead", "", "", "one short line", now)})
		m = step(m, tea.WindowSizeMsg{Width: width, Height: 30})
		out := stripANSI(m.View().Content)
		if !strings.Contains(out, "re-log in Codex credential") {
			t.Fatalf("width %d: the hold line is missing:\n%s", width, out)
		}
		rows := strings.Split(out, "\n")
		for i, row := range rows {
			if w := visualWidth(row); w > width {
				t.Errorf("width %d: row %d is %d columns wide, overflowing the terminal: %q", width, i, w, row)
			}
		}
		if len(rows) != m.height {
			t.Errorf("width %d: detail frame is %d rows, want exactly m.height %d\n%s", width, len(rows), m.height, out)
		}
	}
}

// TestCodexHoldDetailViewportReservesRow: transcriptViewport charges the hold line one row,
// so a held run's viewport is exactly one row shorter than the same run unheld. Reddening
// mutation: drop `codexAccountActionLine(m.detail.run) != ""` from transcriptViewport.
func TestCodexHoldDetailViewportReservesRow(t *testing.T) {
	held := codexHeld("77777777-codex", codexActionReconciling, "")
	plain := held
	plain.Status = "running"
	vp := func(r apitypes.RunDTO) int {
		m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
		m.width, m.height = 100, 30
		return applyDetail(m, r, nil).transcriptViewport()
	}
	if h, p := vp(held), vp(plain); h != p-1 {
		t.Errorf("transcriptViewport(held) = %d, want %d (one row fewer than the unheld run's %d for the hold line)", h, p-1, p)
	}
}

// TestCodexHoldBoardReservesSecondLine: a selected held row's second line is reserved in
// boardCapacity, so on a board with more runs than fit the frame stays within m.height and
// the footer stays on screen. Reddening mutation: drop the codexAccountActionLine term from
// boardShowSecondLine; renderBoard still draws the second line, capacity no longer reserves
// it, and the frame overflows by one row.
func TestCodexHoldBoardReservesSecondLine(t *testing.T) {
	// A reconciling hold stays ON THE FLOOR; put it first so the cursor starts on it.
	runs := []apitypes.RunListItemDTO{{RunDTO: codexHeld("00000000-held", codexActionReconciling, "work")}}
	for i := 0; i < 40; i++ {
		id := "1" + strings.Repeat("0", 6) + string(rune('a'+i%26)) + "-run"
		runs = append(runs, apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: id, Kind: "issue", Status: "running", IssueTitle: "busy", Health: "ok"}})
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 100, 20
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs})
	if r, ok := m.board.selected(); !ok || r.ID != "00000000-held" {
		t.Fatalf("cursor is not on the held run: %+v", r.ID)
	}
	if !m.boardShowSecondLine(m.board.runs[0]) {
		t.Error("boardShowSecondLine(held) = false; the selected held row's second line is not reserved")
	}
	out := stripANSI(m.View().Content)
	if !strings.Contains(out, "▸ Codex account is reconciling") {
		t.Fatalf("board lacks the held row's second line:\n%s", out)
	}
	rows := strings.Split(out, "\n")
	if len(rows) > m.height {
		t.Errorf("board frame is %d rows, want at most m.height %d (the second line was not reserved)\n%s", len(rows), m.height, out)
	}
	// The same board with the cursor on a plain running row (no second line) has one more
	// capacity row: the reservation is exactly the one physical line the held row adds.
	held := m.boardCapacity()
	m.board.cursor = 1
	if plain := m.boardCapacity(); held != plain-1 {
		t.Errorf("boardCapacity(held selected) = %d, want %d (one fewer than %d)", held, plain-1, plain)
	}
}

// TestCodexHoldBoardBands: a relogin_required hold is the owner's turn, so it bands into
// NEEDS YOU with the amber "codex login" word and counts in the header cluster (⚿ 1); every
// other Codex hold action stays ON THE FLOOR as "codex wait". Reddening mutations: drop the
// codexReloginHold branch from runBandOf (the relogin row lands under ON THE FLOOR, NEEDS YOU
// counts 1), from runStateToken (the row reads "codex wait"), or from boardSummary (no ⚿ 1).
func TestCodexHoldBoardBands(t *testing.T) {
	runs := []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "aaaaaaaa-run", Kind: "issue", Status: "running", IssueTitle: "working", Health: "ok"}},
		{RunDTO: codexHeld("bbbbbbbb-relogin", codexActionReloginRequired, "work")},
		{RunDTO: codexHeld("cccccccc-reconcile", codexActionReconciling, "work")},
		{RunDTO: codexHeld("dddddddd-verify", codexActionVerifyingLogin, "work")},
		{RunDTO: apitypes.RunDTO{ID: "eeeeeeee-gate", Kind: "issue", Status: "awaiting_approval", IssueTitle: "gate", Health: "ok"}},
		{RunDTO: apitypes.RunDTO{ID: "ffffffff-done", Kind: "issue", Status: "completed", IssueTitle: "done", Health: "ok"}},
	}
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = 140, 40
	m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: runs})
	out := stripANSI(m.View().Content)
	rows := strings.Split(out, "\n")
	idx := func(sub string) int {
		t.Helper()
		for i, r := range rows {
			if strings.Contains(r, sub) {
				return i
			}
		}
		t.Fatalf("no row contains %q:\n%s", sub, out)
		return -1
	}
	needs, floor, done := idx("NEEDS YOU · 2"), idx("ON THE FLOOR · 3"), idx("DONE · 1")
	band := func(id string) string {
		switch i := idx(id); {
		case i > needs && i < floor:
			return "NEEDS YOU"
		case i > floor && i < done:
			return "ON THE FLOOR"
		case i > done:
			return "DONE"
		}
		return "none"
	}
	for id, want := range map[string]string{
		"bbbbbbbb": "NEEDS YOU", "eeeeeeee": "NEEDS YOU",
		"aaaaaaaa": "ON THE FLOOR", "cccccccc": "ON THE FLOOR", "dddddddd": "ON THE FLOOR",
		"ffffffff": "DONE",
	} {
		if got := band(id); got != want {
			t.Errorf("run %s landed in %s, want %s\n%s", id, got, want, out)
		}
	}
	if row := rows[idx("bbbbbbbb")]; !strings.Contains(row, "codex login") || !strings.Contains(row, codexReloginGlyph) {
		t.Errorf("relogin row lacks the ⚿ codex login token: %q", row)
	}
	for _, id := range []string{"cccccccc", "dddddddd"} {
		if row := rows[idx(id)]; !strings.Contains(row, "~ "+id) || !strings.Contains(row, "codex wait") {
			t.Errorf("self-resolving hold %s lacks the ~ codex wait token: %q", id, row)
		}
	}
	if !strings.Contains(out, "⚑ 1 · ⚿ 1 · ") {
		t.Errorf("header cluster lacks the needs-you counters ⚑ 1 · ⚿ 1:\n%s", rows[0])
	}
	// The detail header shares the token.
	d := tuiTestModel(t, &uzicli.FakeClient{}, "bbbbbbbb-relogin")
	d = applyDetail(d, runs[1].RunDTO, nil)
	if got := stripANSI(d.View().Content); !strings.Contains(got, codexReloginGlyph+" codex login") {
		t.Errorf("detail header lacks the codex login token:\n%s", got)
	}
}

// TestRunTablesCodexStatusCell drives the real `uzi run list` and `uzi admin runs` commands:
// a held run's STATUS cell carries the short, label-free action, so a long alias label never
// widens the column for every row. Reddening mutations: revert either call site to
// displayRunStatus (the "(re-log in Codex credential)" suffix disappears), or append the full
// codexAccountActionLine (the label reaches the table).
func TestRunTablesCodexStatusCell(t *testing.T) {
	rows := []apitypes.RunListItemDTO{
		{RunDTO: codexHeld("held-1", codexActionReloginRequired, longCodexLabel)},
		{RunDTO: apitypes.RunDTO{ID: "plain-2", Kind: "issue", Status: "running", IssueTitle: "plain"}},
	}
	for _, args := range [][]string{{"run", "list"}, {"admin", "runs"}} {
		fc := &uzicli.FakeClient{Runs: rows, AdminRuns: rows}
		out, stderr, code := runCLI(t, fakeEnv(fc), args...)
		if code != uzicli.ExitOK {
			t.Fatalf("%v: exit = %d, stderr:\n%s", args, code, stderr)
		}
		if line := lineWith(t, out, "held-1"); !strings.Contains(line, "recovery_wait (re-log in Codex credential)") {
			t.Errorf("%v: held row lacks the short Codex action: %q", args, line)
		}
		if strings.Contains(out, longCodexLabel) {
			t.Errorf("%v: the alias label reached the table, widening STATUS for every row:\n%s", args, out)
		}
		if line := lineWith(t, out, "plain-2"); strings.Contains(line, "Codex") {
			t.Errorf("%v: an unheld row gained a Codex suffix: %q", args, line)
		}
	}
	// Every action's short form is bounded and label-free; `run get` keeps the full label.
	for _, a := range []string{codexActionReconciling, codexActionReloginRequired, codexActionVerifyingLogin, codexActionResuming, "future"} {
		if s := codexAccountActionShort(codexHeld("x", a, longCodexLabel)); len([]rune(s)) > 26 || s == "" {
			t.Errorf("codexAccountActionShort(%s) = %q, want 1..26 runes", a, s)
		}
	}
	var buf bytes.Buffer
	if err := renderRunDetail(uzicli.NewPrinter(&buf, false, false, true, false), codexHeld("held-1", codexActionReloginRequired, longCodexLabel)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "re-log in Codex credential "+longCodexLabel+" to continue") {
		t.Errorf("run get lost the full labelled CODEX_ACCOUNT row:\n%s", buf.String())
	}
}

// codexParkFake serves a scripted GetRun sequence for `run logs --follow`.
type codexParkFake struct {
	*uzicli.FakeClient
	seq   []apitypes.RunDTO
	calls int
}

func (f *codexParkFake) GetRun(_ context.Context, _ string) (apitypes.RunDTO, error) {
	i := f.calls
	f.calls++
	if i >= len(f.seq) {
		i = len(f.seq) - 1
	}
	return f.seq[i], nil
}

// TestRunLogsFollowCodexHoldNotice: `run logs --follow` rides out a Codex account hold and
// prints its one-shot notice from codexAccountActionLine, not the transient-recovery or
// forge wording and no resume promise. Reddening mutation: drop the codexAccountActionLine
// branch in run_get.go (the generic "recovering" notice prints instead).
func TestRunLogsFollowCodexHoldNotice(t *testing.T) {
	t.Setenv("UZI_URL", "")
	t.Setenv("UZI_TOKEN", "")
	old := logsPollInterval
	logsPollInterval = time.Millisecond
	defer func() { logsPollInterval = old }()

	held := codexHeld("r1", codexActionReloginRequired, "work")
	pf := &codexParkFake{
		FakeClient: &uzicli.FakeClient{LogsByID: map[string][]apitypes.MessageDTO{
			"r1": {{Seq: 1, Kind: "assistant", Payload: []byte(`{"text":"hi"}`)}},
		}},
		seq: []apitypes.RunDTO{
			{ID: "r1", Status: "running"},
			held, held, held,
			{ID: "r1", Status: "completed"},
		},
	}
	var out, errBuf bytes.Buffer
	env := fakeEnv(pf)
	env.Stdout, env.Stderr = &out, &errBuf
	done := make(chan int, 1)
	go func() { done <- Main(env, []string{"run", "logs", "r1", "--follow"}) }()
	select {
	case code := <-done:
		if code != uzicli.ExitOK {
			t.Fatalf("--follow exit = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run logs --follow hung on a Codex account hold")
	}
	stderr := errBuf.String()
	if n := strings.Count(stderr, "run r1 held — re-log in Codex credential work to continue; still following"); n != 1 {
		t.Errorf("Codex hold notice appeared %d times, want exactly 1:\n%s", n, stderr)
	}
	if strings.Contains(stderr, "recovering") || strings.Contains(stderr, "resumes on its own") {
		t.Errorf("a Codex account hold got the transient-recovery wording:\n%s", stderr)
	}
	if strings.Contains(out.String(), "Codex") {
		t.Errorf("the hold notice reached STDOUT:\n%s", out.String())
	}
}
