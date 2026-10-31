package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestBoardDurText(t *testing.T) {
	tp := func(tm time.Time) *time.Time { return &tm }
	end := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		run  apitypes.RunDTO
		want string
	}{
		{"never started", apitypes.RunDTO{FinishedAt: tp(end)}, ""},
		{"no finish stamp", apitypes.RunDTO{FirstStartedAt: tp(end.Add(-time.Hour))}, ""},
		{"zero finish stamp", apitypes.RunDTO{FirstStartedAt: tp(end.Add(-time.Hour)), FinishedAt: &time.Time{}}, ""},
		{"first start wins over a resumed leg", apitypes.RunDTO{
			FirstStartedAt: tp(end.Add(-72 * time.Minute)), StartedAt: tp(end.Add(-10 * time.Minute)),
			ClaimedAt: tp(end.Add(-11 * time.Minute)), FinishedAt: tp(end)}, "1h12m"},
		{"started leg when no first start", apitypes.RunDTO{StartedAt: tp(end.Add(-10 * time.Minute)), FinishedAt: tp(end)}, "10m"},
		{"claimed fallback", apitypes.RunDTO{ClaimedAt: tp(end.Add(-45 * time.Second)), FinishedAt: tp(end)}, "45s"},
		{"widest regular value", apitypes.RunDTO{FirstStartedAt: tp(end.Add(-(99*24 + 23) * time.Hour)), FinishedAt: tp(end)}, "99d23h"},
		{"past the cell drops to days", apitypes.RunDTO{FirstStartedAt: tp(end.Add(-(100*24 + 23) * time.Hour)), FinishedAt: tp(end)}, "100d"},
		{"years from 1000 days", apitypes.RunDTO{FirstStartedAt: tp(end.Add(-1000 * 24 * time.Hour)), FinishedAt: tp(end)}, "2y"},
		{"longest representable span", apitypes.RunDTO{FirstStartedAt: tp(end.Add(-math.MaxInt64)), FinishedAt: tp(end)}, "292y"},
	}
	for _, tc := range cases {
		got := boardDurText(tc.run)
		if got != tc.want {
			t.Errorf("%s: boardDurText = %q, want %q", tc.name, got, tc.want)
		}
		if visualWidth(got) > boardDurWidth {
			t.Errorf("%s: %q overflows the %d-column cell", tc.name, got, boardDurWidth)
		}
	}
}

// durRuns is one DONE run whose length (1h12m from its FIRST start) differs from both its
// age (finished 20m ago) and its current budget leg (10m), plus one running floor run.
func durRuns() (done, running apitypes.RunListItemDTO) {
	now := time.Now()
	finished := now.Add(-20 * time.Minute)
	first := finished.Add(-72 * time.Minute)
	leg := finished.Add(-10 * time.Minute)
	done = progRun("dddddddd", "completed", nil)
	done.CreatedAt = first.Add(-5 * time.Minute)
	done.FirstStartedAt, done.StartedAt, done.FinishedAt = &first, &leg, &finished
	running = progRun("rrrrrrrr", "running", nil)
	running.CreatedAt = now.Add(-3 * time.Hour)
	running.FirstStartedAt, running.StartedAt = &first, &first
	return done, running
}

func TestBoardDoneRunLengthColumn(t *testing.T) {
	done, running := durRuns()
	m := progBoard(t, 160, done, running)
	if !m.boardShowDur() {
		t.Fatal("run-length column hidden on a 160-column own board")
	}
	doneLine, runLine := progLine(m, done.ID), progLine(m, running.ID)
	if doneLine == "" || runLine == "" {
		t.Fatalf("rows missing:\n%s", strings.Join(viewLines(m), "\n"))
	}
	// Right-aligned in its own cell straight after the age cell.
	if !strings.Contains(doneLine, "20m    1h12m  ") {
		t.Errorf("DONE row lacks age then right-aligned run length: %q", doneLine)
	}
	// Wall time from the first start, not the current leg.
	if strings.Contains(doneLine, " 10m ") {
		t.Errorf("DONE row shows the current budget leg instead of the whole run: %q", doneLine)
	}
	// Blank outside DONE: the running row's run-length cell (same columns as the DONE row's)
	// shows no live elapsed clock.
	durEnd := runeCol(doneLine, "1h12m") + len("1h12m")
	if cell := string([]rune(runLine)[durEnd-boardDurWidth : durEnd]); strings.TrimSpace(cell) != "" {
		t.Errorf("floor row filled the run-length cell: %q in %q", cell, runLine)
	}
	// Later columns keep one position across bands.
	if a, b := runeCol(doneLine, "▰"), runeCol(runLine, "▰"); a < 0 || a != b {
		t.Errorf("milestone column misaligned across bands (%d vs %d):\n%q\n%q", a, b, doneLine, runLine)
	}
	if a, b := runeCol(doneLine, "title "), runeCol(runLine, "title "); a != b {
		t.Errorf("title column misaligned across bands (%d vs %d)", a, b)
	}
}

// runeCol is sub's column in a board line: every board glyph is one column wide, so the rune
// index is the visual column (byte offsets differ across rows by the multi-byte ▸ cursor).
func runeCol(line, sub string) int {
	i := strings.Index(line, sub)
	if i < 0 {
		return -1
	}
	return len([]rune(line[:i]))
}

func TestBoardDoneRunLengthMissingFinish(t *testing.T) {
	done, _ := durRuns()
	done.FinishedAt = nil
	done.UpdatedAt = time.Now().Add(-20 * time.Minute)
	m := progBoard(t, 160, done)
	line := progLine(m, done.ID)
	ageEnd := runeCol(line, "20m") + len("20m")
	if ageEnd < len("20m") {
		t.Fatalf("age cell missing: %q", line)
	}
	if cell := string([]rune(line)[ageEnd : ageEnd+1+2+boardDurWidth]); strings.TrimSpace(cell) != "" {
		t.Errorf("DONE row without a finish stamp shows a length: %q in %q", cell, line)
	}
}

// Run length is the lowest-priority column: it appears only once the micro-bar, percentage,
// COST and the title floor all fit, never flaps across the 120 worker breakpoint, never
// costs the title, and never shows on the admin board (non-terminal runs only).
func TestBoardRunLengthWidthShedding(t *testing.T) {
	done, running := durRuns()
	pct := 70
	running.Progress = &apitypes.RunProgress{State: "percent", Pct: &pct}
	for _, cfg := range []struct {
		name        string
		admin, cred bool
		first       int
	}{{"own", false, false, 111}, {"own+cred", false, true, 123}, {"admin", true, false, 0}} {
		prev := false
		for w := 60; w <= 220; w++ {
			m := progBoard(t, w, done, running)
			m.board.admin = cfg.admin
			if cfg.cred {
				m.tokenCount = 2
			}
			dur := m.boardShowDur()
			if cfg.admin && dur {
				t.Fatalf("%s/%d: run length on the admin board", cfg.name, w)
			}
			if prev && !dur {
				t.Fatalf("%s/%d: run-length visibility flapped", cfg.name, w)
			}
			if !cfg.admin && dur != (w >= cfg.first) {
				t.Errorf("%s/%d: run length visible = %t, want from %d", cfg.name, w, dur, cfg.first)
			}
			if dur && (!m.boardShowMile() || !m.boardShowProg() || !m.boardShowCost()) {
				t.Errorf("%s/%d: run length shown while a live column is shed", cfg.name, w)
			}
			if dur {
				line := progLine(m, done.ID)
				if !strings.Contains(line, "1h12m") || !strings.Contains(line, "title "+done.ID) || visualWidth(line) > w {
					t.Errorf("%s/%d: run length or title lost: %q", cfg.name, w, line)
				}
			}
			prev = dur
		}
	}
}

// With judge markers on screen the gate also reserves the marker's width, so the column never
// appears at the cost of the title floor (tui-ux review: 123 → 16 visible title chars).
func TestBoardRunLengthKeepsTitleWithJudgeMarkers(t *testing.T) {
	done, running := durRuns()
	const title = "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz"
	done.IssueTitle, running.IssueTitle = title, title
	done.JudgeVerdict, done.JudgeTodoCount = sp("issues"), 3
	running.JudgeVerdict = sp("ok")
	pct := 70
	running.Progress = &apitypes.RunProgress{State: "percent", Pct: &pct}
	// Full-width account and cost cells, so measured columns leave no slack below the gate's
	// fixed reservations.
	for _, r := range []*apitypes.RunListItemDTO{&done, &running} {
		r.Harness = "claude"
		r.AnthropicSecretID, r.AnthropicSecretLabel = sp("fixture-account"), sp("credential")
		r.Usage = &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 1234}
	}
	for _, cred := range []bool{false, true} {
		sawDur := false
		for w := 60; w <= 220; w++ {
			m := progBoard(t, w, done, running)
			if cred {
				m.tokenCount = 2
			}
			if !m.boardShowDur() {
				continue
			}
			sawDur = true
			for _, line := range viewLines(m) {
				line = stripANSI(line)
				i := strings.Index(line, "abcdefghij")
				if i < 0 {
					continue
				}
				tw := 0
				for _, r := range line[i:] {
					if r < '0' || r > 'z' {
						break
					}
					tw++
				}
				if tw < 20 {
					t.Errorf("cred %t width %d: run length shown with only %d title cols: %q", cred, w, tw, line)
				}
			}
		}
		if !sawDur {
			t.Errorf("cred %t: run length never shown up to 220 columns", cred)
		}
	}
}

// The bare run-length cell is explained in the board's ? legend (user call: keep the cell
// unlabelled).
func TestBoardHelpExplainsRunLength(t *testing.T) {
	if got := strings.Join(helpLines(viewBoard), "\n"); !strings.Contains(got, "took = first start to finish") {
		t.Errorf("board help lacks the run-length legend:\n%s", got)
	}
}
