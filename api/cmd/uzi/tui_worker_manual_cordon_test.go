package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
)

func TestTUIWorkerManualCordon(t *testing.T) {
	at := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	for _, width := range []int{80, 120, 160} {
		for _, admin := range []bool{false, true} {
			for _, kind := range []string{"offline", "holding", "draining", "idle", "busy", "combined", "effective", "legacy"} {
				t.Run(fmt.Sprintf("%d/%t/%s", width, admin, kind), func(t *testing.T) {
					m := workerRenderModel(true)
					m.board.admin, m.workers.admin, m.workerDetail.admin = admin, admin, admin
					m = step(m, tea.WindowSizeMsg{Width: width, Height: 34})
					w := &m.workers.rows[0].w
					w.ManualCordonedAt, w.EffectiveCordoned = &at, true
					w.ManualCordonedByLabel, w.ManualCordonedBy = sp("operator"), sp("actor-uuid")
					w.ManualCordonReason = sp("maintenance 維😀; held work continues")
					switch kind {
					case "offline":
						w.Status = "offline"
					case "holding":
						n := 1
						w.CustodyDecisionsNeeded = &n
					case "draining":
						w.DrainingSince = &at
						w.ActiveRuns = 1
					case "busy":
						w.Busy, w.ActiveRuns = true, 1
					case "combined":
						n := 1
						w.CustodyDecisionsNeeded, w.DrainingSince = &n, &at
						w.Busy, w.ActiveRuns = true, 1
					case "effective":
						w.ManualCordonedAt = nil
					case "legacy":
						w.ManualCordonedAt = nil
						w.EffectiveCordoned = false
					}
					expectedState := map[string]string{"offline": "offline", "holding": "holding", "draining": "draining", "busy": "busy", "combined": "holding", "idle": "idle", "legacy": "idle", "effective": "idle"}[kind]
					if got := workerState(m.workers.rows[0]); got != expectedState {
						t.Fatalf("state = %q, want %q", got, expectedState)
					}
					for _, view := range []tuiView{viewWorkers, viewWorker} {
						m.view = view
						m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
						var buf bytes.Buffer
						writer := colorprofile.NewWriter(&buf, []string{"NO_COLOR=1"})
						if _, err := writer.Write([]byte(m.View().Content)); err != nil {
							t.Fatal(err)
						}
						out := buf.String()
						if strings.Contains(out, "\u001b[") {
							t.Fatalf("SGR remained: %q", out)
						}
						switch kind {
						case "effective":
							requireWorkerText(t, out, "intake fenced")
						case "legacy":
							if strings.Contains(out, "cordoned (manual)") || strings.Contains(out, "intake fenced") {
								t.Fatal(out)
							}
						default:
							requireWorkerText(t, out, "cordoned (manual)", "operator", "2026-10-23T01:02:03Z", "maintenance 維😀")
						}
						requireWorkerText(t, out, expectedState)
						for _, line := range strings.Split(out, "\n") {
							if visualWidth(line) > width {
								t.Fatalf("overflow %q", line)
							}
						}
						if view == viewWorkers {
							requireWorkerText(t, out, "enter/→ worker", "render-worker")
						} else {
							requireWorkerText(t, out, "pgup/pgdn scroll")
						}
					}
				})
			}
		}
	}
}

// Exercise the writer downgrade independently of the explicit Ascii model above.
func TestTUIWorkerManualNoColorWriter(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, dark := range []bool{false, true} {
		for _, view := range []tuiView{viewWorkers, viewWorker} {
			m := workersScene(dark, "workers-manual-120")
			m.view = view
			m.workerDetail.workerID = m.workers.selectedID
			m = step(m, tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
			raw := m.View().Content
			if !strings.Contains(raw, "\u001b[") {
				t.Fatal("TrueColor input has no ANSI: downgrade control is vacuous")
			}
			var buf bytes.Buffer
			writer := colorprofile.NewWriter(&buf, []string{"NO_COLOR=1", "TERM=xterm-256color", "COLORTERM=truecolor"})
			if _, err := writer.Write([]byte(raw)); err != nil {
				t.Fatal(err)
			}
			out := buf.String()
			if strings.Contains(out, "\u001b") {
				t.Fatalf("NO_COLOR retained ANSI: %q", out)
			}
			requireWorkerText(t, out, "cordoned (manual)", "fleet operator", "Maintenance window", "holding")
		}
	}
}

func TestTUIWorkerManualSummary(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := workersScene(dark, "workers-manual-160")
		// Demo manual intent spans offline, holding, automatic drain and busy.
		// Pin the totals independently so counting manual intent cannot reclassify liveness.
		requireWorkerText(t, stripANSI(m.workersSummary(300, false)), "4 cordoned (manual)", "8 online", "5/12 run slots in use", "1 holding", "1 draining", "7 need attention")
		requireWorkerText(t, stripANSI(m.View().Content), "4 cordoned (manual)", "7 need attention")
		m = step(m, tea.WindowSizeMsg{Width: 240, Height: 34})
		requireWorkerText(t, stripANSI(m.View().Content), "4 cordoned (manual)", "8 online", "5/12 run slots in use", "1 holding", "1 draining", "7 need attention")
		for i := range m.workers.rows {
			m.workers.rows[i].w.ManualCordonedAt = nil
		}
		out := stripANSI(m.workersSummary(300, false))
		if strings.Contains(out, "cordoned (manual)") {
			t.Fatal("manual intake inferred from automatic state")
		}
		requireWorkerText(t, out, "8 online", "5/12 run slots in use", "1 holding", "1 draining")
	}
}

func TestTUIWorkerManualCompactSummary(t *testing.T) {
	for _, dark := range []bool{false, true} {
		m := workersScene(dark, "workers-manual-160")
		for _, tc := range []struct {
			width int
			hide  bool
			want  string
		}{
			{60, true, "workers · 5/12 runs · 4 cordoned (manual) · 7 need attention"},
			{49, true, "workers · 5/12 runs · 4 manual · 7 need attention"},
			{43, true, "workers · 4 manual · 7 need attention"},
			{62, false, "workers · 5/12 runs · 0/? checks · 4 manual · 7 need attention"},
		} {
			got := stripANSI(m.workersSummary(tc.width, tc.hide))
			if got != tc.want || visualWidth(got) > tc.width {
				t.Errorf("width=%d hide=%t: summary %q, want %q", tc.width, tc.hide, got, tc.want)
			}
		}
		for _, split := range []bool{false, true} {
			for _, view := range []tuiView{viewBoard, viewWorkers} {
				m := workersScene(dark, "workers-manual-80")
				m.splitMode = "off"
				if split {
					m.splitMode = "auto"
				}
				m = resizeSplit(m, 80, 60)
				m.setListView(view)
				lines := strings.Split(stripANSI(m.View().Content), "\n")
				at := 0
				if !split && view == viewWorkers {
					at = 1
				}
				requireWorkerText(t, lines[at], "4", "manual", "7 need attention")
				if split || at == 1 {
					requireWorkerText(t, lines[at], "5/12")
				}
				if at == 0 {
					requireWorkerText(t, lines[0], "4 manual")
				} else if strings.Contains(lines[0], "need attention") {
					t.Fatal("owner workers fallback moved inline")
				}
				t.Logf("dark=%t split=%t view=%d: title width=%d summary line=%q", dark, split, view, visualWidth(lines[0]), lines[at])
				for _, line := range lines {
					if visualWidth(line) > 80 {
						t.Fatalf("ship View overflow: %q", line)
					}
				}
			}
		}
	}
}

func TestTUIWorkerManualHostileMetadata(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := workerRenderModel(false)
		m = step(m, tea.WindowSizeMsg{Width: width, Height: 34})
		w := &m.workers.rows[0].w
		at := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
		w.ManualCordonedAt = &at
		w.ManualCordonedByLabel = sp("\u001b]0;hidden-title\a\u001b[31m\u202e\u200b\u0085\n")
		w.ManualCordonedBy = sp("\u001b]0;hidden-actor\a\u001b[2Jactor-uuid\u0001\u0085\u200b\u202e\n")
		w.ManualCordonReason = sp("\u001b]8;;https://hidden.test\u001b\\\u001b[31m維😀 safe\u001b]8;;\u001b\\\n\t\u0001\u009f\u202e" + strings.Repeat("界", 200) + "TAIL")
		for _, actualAscii := range []bool{false, true} {
			if actualAscii {
				m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			}
			for _, view := range []tuiView{viewWorkers, viewWorker} {
				m.view = view
				raw := m.View().Content
				assertNoRawControls(t, "manual metadata", raw)
				// Metadata lines are plain text even when surrounding UI chrome is styled.
				for _, line := range m.manualCordonLines(m.workers.rows[0], width) {
					assertNoRawControls(t, "plain manual metadata", line)
					if strings.ContainsAny(line, "\u001b\t\r\n") {
						t.Fatalf("terminal instruction or extra line in metadata: %q", line)
					}
				}
				for _, bad := range []string{"\u001b]", "\u001b[2J", "\a", "\t", "\u0001", "\u0085", "\u009f", "\u200b", "\u202e", "hidden-title", "hidden-actor", "hidden.test"} {
					if strings.Contains(raw, bad) {
						t.Errorf("unsafe %q in raw frame: %q", bad, raw)
					}
				}
				if actualAscii {
					var buf bytes.Buffer
					writer := colorprofile.NewWriter(&buf, nil)
					writer.Profile = colorprofile.Ascii
					if _, err := writer.Write([]byte(raw)); err != nil {
						t.Fatal(err)
					}
					raw = buf.String()
					assertNoRawControls(t, "Ascii manual metadata", raw)
					// Ascii drops colours, retaining legitimate bold/reset UI styling.
					// Only these exact chrome sequences may remain; metadata above allows none.
					plain := strings.NewReplacer("\u001b[1m", "", "\u001b[m", "", "\u001b[0m", "", "\u001b[22m", "").Replace(raw)
					if strings.Contains(plain, "\u001b") {
						t.Errorf("Ascii frame retained a colour or terminal instruction: %q", raw)
					}
				}
				out := stripANSI(raw)
				requireWorkerText(t, out, "cordoned (manual)", "actor-uuid", "維😀 safe", "reason:")
				for _, bad := range []string{"hidden-title", "hidden.test", "[31m", "TAIL", "\u202e", "\u200b", "\u009f"} {
					if strings.Contains(out, bad) {
						t.Fatalf("unsafe %q in %q", bad, out)
					}
				}
				for _, line := range strings.Split(out, "\n") {
					if visualWidth(line) > width {
						t.Fatalf("overflow %q", line)
					}
				}
			}
		}
	}
}

func TestTUIWorkerManualIntakeColumns(t *testing.T) {
	at := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
	for _, width := range []int{80, 120, 160, 200} {
		for _, admin := range []bool{false, true} {
			for _, intake := range []string{"manual", "effective", "none"} {
				t.Run(fmt.Sprintf("%d/%t/%s", width, admin, intake), func(t *testing.T) {
					m := workerRenderModel(true)
					m.width, m.height, m.view = width, 34, viewWorkers
					m.board.admin, m.workers.admin = admin, admin
					r := m.workers.rows[0]
					r.w.ID, r.w.Name, r.workerOwner = "warning", "warning-row", "owner@example.test"
					if intake != "none" || width >= 160 {
						r.w.Version = sp("0.85.1+gba846d7")
					}
					r.w.OutboxBlocked = sp("automatic warning")
					r.w.LastHeartbeatAt = &at
					switch intake {
					case "manual":
						r.w.ManualCordonedAt = &at
					case "effective":
						r.w.EffectiveCordoned = true
					}
					plain := m.workers.rows[0]
					plain.w.ID, plain.w.Name = "plain", "plain-row"
					m.workers.rows = []workerRow{r, plain}
					m.workers.selectedID = plain.w.ID
					out := stripANSI(m.renderWorkersBody(34, false))
					var header, warning, ordinary string
					for _, line := range strings.Split(out, "\n") {
						if visualWidth(line) > width {
							t.Fatalf("overflow: %q", line)
						}
						switch {
						case strings.Contains(line, "NAME"):
							header = line
						case strings.Contains(line, "warning-row"):
							warning = line
						case strings.Contains(line, "plain-row") && !strings.Contains(line, "selected"):
							ordinary = line
						}
					}
					requireWorkerText(t, header, "STATE", "KIND", "RUNS", "CHECKS", "ATTENTION")
					requireWorkerText(t, warning, "idle", "host·?", "✕ outbox")
					if strings.Contains(warning, "▌") {
						t.Fatal("warning regression must exercise an unselected row")
					}
					if ordinary == "" {
						t.Fatal("missing ordinary row")
					}
					column := func(line, label string) int {
						t.Helper()
						i := strings.Index(line, label)
						if i < 0 {
							t.Fatalf("missing %q in %q", label, line)
						}
						return visualWidth(line[:i])
					}
					if column(header, "ATTENTION") != column(warning, "✕ outbox") ||
						column(header, "ATTENTION") != column(ordinary, "—") {
						t.Fatalf("attention columns differ:\n%s\n%s\n%s", header, warning, ordinary)
					}
					dense := width >= 120 && (intake == "none" || width >= 160)
					for _, label := range []string{"CPU", "MEM", "DISK (worst)", "VERSION", "HB"} {
						if strings.Contains(header, label) != dense {
							t.Errorf("dense column %s at width %d: %q", label, width, header)
						}
					}
					if admin && width >= 120 {
						requireWorkerText(t, header, "OWNER")
						if column(header, "OWNER") != column(warning, "owner") {
							t.Fatal("owner column moved")
						}
					}
					if intake == "none" {
						if strings.Contains(header, "INTAKE") || strings.Contains(warning, "cordoned") {
							t.Fatal("optional intake changed the legacy layout")
						}
					} else {
						label := "cordoned (manual)"
						if intake == "effective" {
							label = "intake fenced"
						}
						if column(header, "INTAKE") != column(warning, label) {
							t.Fatalf("intake columns differ:\n%s\n%s", header, warning)
						}
						if width >= 120 {
							requireWorkerText(t, warning, "✕ outbox blocked")
						}
						m.workers.filter = "plain-row"
						filtered := stripANSI(m.renderWorkersBody(34, false))
						if strings.Contains(filtered, "INTAKE") {
							t.Fatal("filtered-out intake worker changed the table")
						}
						if width >= 120 {
							requireWorkerText(t, filtered, "CPU", "HB")
						}
					}
				})
			}
		}
	}
}

func TestTUIWorkerManualResizeAndSmallRows(t *testing.T) {
	m := workersScene(true, "workers-manual-120")
	selected := m.workers.selectedID
	if selected == "" {
		t.Fatal("missing selected worker")
	}
	for _, size := range []struct{ width, height int }{{80, 12}, {120, 34}, {80, 34}, {160, 60}} {
		m = step(m, tea.WindowSizeMsg{Width: size.width, Height: size.height})
		if m.workers.selectedID != selected {
			t.Fatal("resize lost worker selection")
		}
		out := stripANSI(m.View().Content)
		requireWorkerText(t, out, "recovery", "cordoned (manual)")
		if size.height >= 60 {
			requireWorkerText(t, out, "enter/→ open", "ctrl+w focus")
		} else {
			requireWorkerText(t, out, "enter/→ worker")
		}
		if len(strings.Split(out, "\n")) > size.height {
			t.Fatal("resize hid footer beyond terminal height")
		}
		for _, line := range strings.Split(out, "\n") {
			if visualWidth(line) > size.width {
				t.Fatalf("resized view overflow: %q", line)
			}
		}
	}
	row, ok := m.scopedWorker(selected)
	if !ok {
		t.Fatal("selected worker missing after resize")
	}
	for _, width := range []int{0, 8, 16, 17, 24, 80, 120} {
		line := m.workerRowLine(row, true, width, m.workerTableWidths([]workerRow{row}, width), time.Now())
		if visualWidth(line) > width {
			t.Fatalf("manual suffix overflow at width %d: %q", width, line)
		}
	}
}

func TestTUIManualScenes(t *testing.T) {
	for _, name := range []string{"workers-manual-80", "workers-manual-120", "workers-manual-160", "worker-manual-80", "worker-manual-combined-80", "worker-manual", "worker-manual-combined"} {
		for _, dark := range []bool{true, false} {
			m := workersScene(dark, name)
			out := stripANSI(m.View().Content)
			requireWorkerText(t, out, "cordoned (manual)", "fleet operator", "Maintenance window")
			for _, line := range strings.Split(out, "\n") {
				if visualWidth(line) > m.width {
					t.Fatalf("%s overflow: %q", name, line)
				}
			}
			if name == "workers-manual-160" {
				requireWorkerText(t, out, "CPU", "MEM", "DISK (worst)", "VERSION", "HB", "INTAKE")
			}
			if strings.HasPrefix(name, "worker-manual-combined") {
				requireWorkerText(t, out, "offline", "unpublished work needs an owner decision", "only pinned work may claim")
			}
		}
	}
}
