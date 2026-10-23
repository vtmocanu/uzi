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
	for _, width := range []int{80, 120} {
		for _, admin := range []bool{false, true} {
			for _, kind := range []string{"offline", "holding", "draining", "idle", "effective", "legacy"} {
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
					case "effective":
						w.ManualCordonedAt = nil
					case "legacy":
						w.ManualCordonedAt = nil
						w.EffectiveCordoned = false
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
						if kind == "offline" || kind == "holding" || kind == "draining" {
							requireWorkerText(t, out, kind)
						}
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

func TestTUIWorkerManualHostileMetadata(t *testing.T) {
	for _, width := range []int{80, 120} {
		m := workerRenderModel(false)
		m = step(m, tea.WindowSizeMsg{Width: width, Height: 34})
		w := &m.workers.rows[0].w
		at := time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)
		w.ManualCordonedAt = &at
		w.ManualCordonedByLabel = sp("\u001b]0;hidden-title\a\u001b[31m\u202e\u200b\u0085\n")
		w.ManualCordonedBy = sp("actor-uuid")
		w.ManualCordonReason = sp("\u001b]8;;https://hidden.test\u001b\\\u001b[31m維😀 safe\u001b]8;;\u001b\\\n\t\u0001\u009f\u202e" + strings.Repeat("界", 200) + "TAIL")
		for _, view := range []tuiView{viewWorkers, viewWorker} {
			m.view = view
			out := stripANSI(m.View().Content)
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

func TestTUIWorkerManualResizeAndSmallRows(t *testing.T) {
	m := workersScene(true, "workers-manual-120")
	selected := m.workers.selectedID
	if selected == "" {
		t.Fatal("missing selected worker")
	}
	for _, size := range []struct{ width, height int }{{80, 12}, {120, 34}, {80, 34}} {
		m = step(m, tea.WindowSizeMsg{Width: size.width, Height: size.height})
		if m.workers.selectedID != selected {
			t.Fatal("resize lost worker selection")
		}
		out := stripANSI(m.View().Content)
		requireWorkerText(t, out, "recovery", "cordoned (manual)", "enter/→ worker")
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
	for _, name := range []string{"workers-manual-80", "workers-manual-120", "worker-manual", "worker-manual-combined"} {
		m := workersScene(true, name)
		out := stripANSI(m.View().Content)
		requireWorkerText(t, out, "cordoned (manual)", "fleet operator", "Maintenance window")
		if name == "worker-manual-combined" {
			requireWorkerText(t, out, "offline", "unpublished work needs an owner decision", "only pinned work may claim")
		}
	}
}
