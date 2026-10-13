package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func humanStopRows() []apitypes.RunListItemDTO {
	return []apitypes.RunListItemDTO{
		{RunDTO: apitypes.RunDTO{ID: "cancel", Kind: "issue", Status: "cancelled", StopKind: sp("auto_stopped"), Health: "stalled", LandingState: "needs_landing", IssueTitle: "cancel title"}},
		{RunDTO: apitypes.RunDTO{ID: "reject", Kind: "issue", Status: "failed", StopKind: sp("plan_rejected"), Health: "stalled", LandingState: "needs_landing", IssueTitle: "reject title"}},
		{RunDTO: apitypes.RunDTO{ID: "stop", Kind: "issue", Status: "failed", StopKind: sp("cancelled"), LandingState: "needs_landing", IssueTitle: "stop title"}},
		{RunDTO: apitypes.RunDTO{ID: "failure", Kind: "issue", Status: "failed", StopKind: sp("auto_stopped"), IssueTitle: "failure title"}},
		{RunDTO: apitypes.RunDTO{ID: "active", Kind: "issue", Status: "running", StopKind: sp("cancelled"), IssueTitle: "active title"}},
	}
}

func humanStopClient() *uzicli.FakeClient {
	rows := humanStopRows()
	fc := &uzicli.FakeClient{Runs: rows, AdminRuns: rows, RunByID: map[string]apitypes.RunDTO{}}
	for _, r := range rows {
		fc.RunByID[r.ID] = r.RunDTO
	}
	return fc
}

func TestHumanStopCommands(t *testing.T) {
	// Admin's endpoint is active-only; these stopped fake rows exercise its renderer.
	for _, args := range [][]string{{"run", "list"}, {"admin", "runs"}} {
		out, err, code := runCLI(t, fakeEnv(humanStopClient()), args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, err)
		}
		for _, tc := range []struct{ id, want string }{{"cancel", "stopped (cancelled)"}, {"reject", "stopped (plan rejected)"}, {"stop", "stopped (cancelled)"}, {"failure", "failed"}, {"active", "running"}} {
			if row := lineWith(t, out, tc.id+" "); !strings.Contains(row, tc.want) {
				t.Errorf("%v %s: %q want %q", args, tc.id, row, tc.want)
			}
		}
	}
	for _, tc := range []struct{ id, want string }{{"cancel", "stopped (cancelled)"}, {"reject", "stopped (plan rejected)"}, {"stop", "stopped (cancelled)"}} {
		out, err, code := runCLI(t, fakeEnv(humanStopClient()), "run", "get", tc.id)
		if code != 0 {
			t.Fatalf("%d %s", code, err)
		}
		if row := lineWith(t, out, "STATUS"); !strings.Contains(row, tc.want) {
			t.Errorf("%s: %q want %q", tc.id, row, tc.want)
		}
	}
	out, err, code := runCLI(t, fakeEnv(humanStopClient()), "run", "get", "reject", "--field", "status", "--field", "stop_kind")
	if code != 0 || out != "failed\nplan_rejected\n" {
		t.Fatalf("raw fields: %q %s exit %d", out, err, code)
	}
}

// Capture the same command fixtures before renderer changes, then compare exact bytes.
// Captures live exclusively in the caller-provided scratch directory.
func TestHumanStopJSONBaseline(t *testing.T) {
	dir := os.Getenv("UZI_HUMAN_STOP_JSON_DIR")
	if dir == "" {
		t.Skip("scratch baseline directory required")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := root.Close(); err != nil {
			t.Error(err)
		}
	})
	for i, args := range [][]string{{"run", "get", "reject", "--json"}, {"run", "list", "--json"}, {"admin", "runs", "--json"}} {
		out, err, code := runCLI(t, fakeEnv(humanStopClient()), args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", args, code, err)
		}
		name := fmt.Sprintf("command-%d.json", i)
		if os.Getenv("UZI_HUMAN_STOP_CAPTURE") == "1" {
			if err := root.WriteFile(name, []byte(out), 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			want, err := root.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if out != string(want) {
				t.Errorf("%v JSON bytes differ from baseline", args)
			}
		}
	}
}

func humanStopBoard(t *testing.T, dark, admin bool, width int) tuiModel {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.width, m.height = width, 34
	m.pal = newPalette(dark)
	m.board.admin = admin
	m = step(m, boardRunsMsg{reqID: m.board.waitID, admin: admin, runs: humanStopRows()})
	return m
}

func TestHumanStopViewPrecedence(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, admin := range []bool{false, true} {
			m := humanStopBoard(t, dark, admin, 100)
			out := stripANSI(m.View().Content)
			for _, id := range []string{"cancel title", "reject title", "stop title"} {
				row := lineWith(t, out, id)
				if !strings.Contains(row, "stopped") || strings.Contains(row, "stalled") || strings.Contains(row, "needs landing") {
					t.Errorf("board row: %s", row)
				}
			}
			if strings.Contains(out, "stopped (") || strings.Contains(out, "▲ 2") {
				t.Errorf("board human stop alarm/full label:\n%s", out)
			}
			// Genuine unhealthy work must still warn in the real header.
			rows := humanStopRows()
			rows[4].Health = "stalled"
			m.board.runs = rows
			out = stripANSI(m.View().Content)
			if !strings.Contains(out, "▲ 1") {
				t.Errorf("genuine warning missing:\n%s", out)
			}
		}
		for _, r := range humanStopRows()[:3] {
			m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
			m.pal = newPalette(dark)
			m = applyDetail(m, r.RunDTO, nil)
			want := "stopped (cancelled)"
			if r.ID == "reject" {
				want = "stopped (plan rejected)"
			}
			if out := stripANSI(m.View().Content); !strings.Contains(out, want) {
				t.Errorf("detail missing %q:\n%s", want, out)
			}
		}
		m := workerRenderModel(dark)
		m.board.runs = humanStopRows()
		m.board.runsAdmin = m.board.admin
		m.workers.rows[0].w.ReportedRuns = []apitypes.WorkerReportedRunDTO{{RunID: "reject", Phase: "running"}}
		if out := stripANSI(m.View().Content); !strings.Contains(out, "run stage stopped") || strings.Contains(out, "stopped (") {
			t.Errorf("worker compact stage:\n%s", out)
		}
	}
	b := newBoardState()
	b.runs = humanStopRows()
	b.filter = "stopped"
	if got := b.visible(); len(got) != 3 {
		t.Errorf("search stopped got %d rows", len(got))
	}
}

func TestHumanStopViewNeutralSGR(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, admin := range []bool{false, true} {
			m := humanStopBoard(t, dark, admin, 100)
			out := m.View().Content
			segment := paintSeg(m.pal.faintC, nil, false, "stopped       ")
			for _, title := range []string{"cancel title", "reject title", "stop title"} {
				if row := lineWith(t, out, title); !strings.Contains(row, segment) {
					t.Errorf("board neutral SGR segment absent %q:\n%q", segment, row)
				}
			}
		}
		for _, r := range humanStopRows()[:3] {
			m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
			m.pal = newPalette(dark)
			m = applyDetail(m, r.RunDTO, nil)
			want := "stopped (cancelled)"
			if r.ID == "reject" {
				want = "stopped (plan rejected)"
			}
			segment := paintSeg(m.pal.faintC, nil, false, "· "+want)
			if out := m.View().Content; !strings.Contains(out, segment) {
				t.Errorf("detail neutral SGR segment absent %q:\n%q", segment, out)
			}
		}
	}
}

func TestHumanStopDetailFailureControls(t *testing.T) {
	for _, dark := range []bool{false, true} {
		r := humanStopRows()[3].RunDTO
		for _, landing := range []string{"", "needs_landing"} {
			r.LandingState = landing
			m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
			m.pal = newPalette(dark)
			m = applyDetail(m, r, nil)
			word := "failed"
			if landing != "" {
				word = "needs landing"
			}
			segment := paintSeg(m.pal.alarm, nil, false, "✗ "+word)
			if !strings.Contains(m.View().Content, segment) {
				t.Errorf("genuine failure alarm segment missing: %q", segment)
			}
		}
	}
}

func TestHumanStopDetailDuration(t *testing.T) {
	for _, dark := range []bool{false, true} {
		r := humanStopRows()[1].RunDTO
		start := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
		finish := start.Add(90 * time.Second)
		r.StartedAt, r.FinishedAt = &start, &finish
		m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
		m.pal = newPalette(dark)
		m = applyDetail(m, r, nil)
		header := strings.Split(stripANSI(m.View().Content), "\n")[0]
		if !strings.Contains(header, "· stopped (plan rejected) · 1m") {
			t.Errorf("duration/label header: %q", header)
		}
	}
}

func TestHumanStopProfileDowngrade(t *testing.T) {
	for _, dark := range []bool{false, true} {
		r := humanStopRows()[1].RunDTO
		m := tuiTestModel(t, &uzicli.FakeClient{}, r.ID)
		m.pal = newPalette(dark)
		m = applyDetail(m, r, nil)
		for _, profile := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.NoTTY} {
			var b bytes.Buffer
			w := colorprofile.NewWriter(&b, nil)
			w.Profile = profile
			if _, err := w.Write([]byte(m.View().Content)); err != nil {
				t.Fatal(err)
			}
			out := b.String()
			if !strings.Contains(out, "· stopped (plan rejected)") || strings.Contains(out, "38;") || strings.Contains(out, "48;") {
				t.Errorf("profile %v: %q", profile, out)
			}
		}
	}
}

func TestHumanStopBoardProfileDowngrade(t *testing.T) {
	for _, dark := range []bool{false, true} {
		for _, admin := range []bool{false, true} {
			m := humanStopBoard(t, dark, admin, 100)
			for _, profile := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.NoTTY} {
				var b bytes.Buffer
				w := colorprofile.NewWriter(&b, nil)
				w.Profile = profile
				if _, err := w.Write([]byte(m.View().Content)); err != nil {
					t.Fatal(err)
				}
				out := b.String()
				row := lineWith(t, stripANSI(out), "reject title")
				if !strings.Contains(row, "· reject") || !strings.Contains(row, "stopped") || strings.Contains(out, "stopped (") || strings.Contains(out, "38;") || strings.Contains(out, "48;") {
					t.Errorf("compact downgraded row: %q", row)
				}
			}
		}
	}
}

func TestHumanStopNarrowBoards(t *testing.T) {
	for _, width := range []int{80, 100, 120} {
		for _, dark := range []bool{false, true} {
			for _, admin := range []bool{false, true} {
				m := humanStopBoard(t, dark, admin, width)
				m.tokenCount = 2
				pct := 70
				for i := range m.board.runs {
					r := &m.board.runs[i]
					r.IssueTitle = "TITLE " + strings.Repeat("long title ", 30)
					r.Harness = "claude"
					r.AnthropicSecretID, r.AnthropicSecretLabel = sp("fixture-account"), sp("credential")
					r.OwnerEmail = sp("owner@example.test")
					r.Usage = &apitypes.UsageDTO{CostStatus: "metered", CostUSD: 9.4}
					r.Milestones = []apitypes.Milestone{{ID: "m1"}, {ID: "m2"}, {ID: "m3"}}
					r.MilestonesCompleted = []string{"m1"}
					r.Progress = &apitypes.RunProgress{State: "percent", Pct: &pct}
					r.JudgeVerdict, r.JudgeTodoCount = sp("issues"), 3
				}
				out := stripANSI(m.View().Content)
				if !strings.Contains(out, "stopped") || strings.Contains(out, "stopped (") {
					t.Errorf("compact label missing:\n%s", out)
				}
				lines := strings.Split(out, "\n")
				for _, line := range lines {
					if visualWidth(line) > width {
						t.Errorf("width %d overflow %q", width, line)
					}
				}
				// Board band headings share the frame's left edge; rows keep fixed status and
				// credential columns despite the shorter stopped word.
				for _, heading := range []string{" ON THE FLOOR", " DONE"} {
					if !strings.HasPrefix(lineWith(t, out, heading), heading) {
						t.Errorf("band header alignment: %q", heading)
					}
				}
				statusCol, credCol := -1, -1
				for _, id := range []string{"cancel", "reject", "stop"} {
					row := lineWith(t, out, " "+id+" ")
					at := strings.Index(row, "stopped")
					if at < 0 {
						t.Errorf("width %d dark %t admin %t: stopped label missing for %s: %q", width, dark, admin, id, row)
					} else {
						col := visualWidth(row[:at])
						if statusCol >= 0 && col != statusCol {
							t.Errorf("status columns %d/%d: %q", statusCol, col, row)
						}
						statusCol = col
					}
					at = strings.Index(row, "credential")
					if at < 0 {
						t.Fatalf("credential fixture not rendered: %q", row)
					}
					col := visualWidth(row[:at])
					if credCol >= 0 && col != credCol {
						t.Errorf("credential columns %d/%d", credCol, col)
					}
					credCol = col
				}
				footer := lines[len(lines)-1]
				if !strings.Contains(footer, "open") || !strings.Contains(footer, "quit") || strings.Count(out, "enter/→") != 1 {
					t.Errorf("single footer missing: %q", footer)
				}
				if !admin && !strings.Contains(out, "issues · 3") {
					t.Errorf("judge marker missing:\n%s", out)
				}
				if !admin && width == 120 && !strings.Contains(out, "$9") {
					t.Errorf("cost control missing:\n%s", out)
				}
				if !admin && width == 120 && !strings.Contains(out, "70%") {
					t.Errorf("progress control missing:\n%s", out)
				}
			}
		}
	}
}

// These cases are regression controls, not baseline-red evidence.
func TestHumanStopRendererControls(t *testing.T) {
	for _, tc := range []struct{ status, kind, landing, cli, compact string }{
		{"cancelled", "", "needs_landing", "stopped (cancelled)", "stopped"},
		{"failed", "auto_stopped", "", "failed", "failed"},
		{"failed", "auto_stopped", "needs_landing", "needs landing", "needs landing"},
		{"failed", "", "", "failed", "failed"},
		{"failed", "future-kind", "", "failed", "failed"},
		{"running", "cancelled", "", "running", "running"},
		{"running", "plan_rejected", "", "running", "running"},
		{"completed", "cancelled", "", "completed", "done"},
		{"completed", "plan_rejected", "", "completed", "done"},
	} {
		t.Run(tc.status+"/"+tc.kind+"/"+tc.landing, func(t *testing.T) {
			r := apitypes.RunDTO{ID: "control", Kind: "issue", Status: tc.status, LandingState: tc.landing, IssueTitle: "control title"}
			if tc.kind != "" {
				r.StopKind = sp(tc.kind)
			}
			rows := []apitypes.RunListItemDTO{{RunDTO: r}}
			fc := &uzicli.FakeClient{RunByID: map[string]apitypes.RunDTO{r.ID: r}, Runs: rows, AdminRuns: rows}
			for _, args := range [][]string{{"run", "get", r.ID}, {"run", "list"}, {"admin", "runs"}} {
				out, err, code := runCLI(t, fakeEnv(fc), args...)
				if code != 0 {
					t.Fatalf("%v: exit %d %s", args, code, err)
				}
				rowKey := "control"
				if args[1] == "get" {
					rowKey = "STATUS"
				}
				row := lineWith(t, out, rowKey)
				if !strings.Contains(row, tc.cli) {
					t.Errorf("%v: %q want %q", args, row, tc.cli)
				}
			}
			m := tuiTestModel(t, fc, "")
			m = step(m, boardRunsMsg{reqID: m.board.waitID, runs: rows})
			row := lineWith(t, stripANSI(m.View().Content), "control title")
			if !strings.Contains(row, tc.compact) {
				t.Errorf("board: %q want %q", row, tc.compact)
			}
		})
	}
}
