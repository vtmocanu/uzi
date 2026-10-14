package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/uzicli"
)

func TestWorkerRollASCIIAndNoTTY(t *testing.T) {
	for _, profile := range []colorprofile.Profile{colorprofile.Ascii, colorprofile.NoTTY} {
		for _, scene := range []string{"worker-capacity-blocked", "worker-provisioning", "worker-provisioning-failed"} {
			m := workersScene(true, scene)
			m = step(m, tea.ColorProfileMsg{Profile: profile})
			out := stripANSI(m.View().Content)
			want := strings.ReplaceAll(strings.TrimPrefix(scene, "worker-"), "-", " ")
			if !strings.Contains(out, want) || strings.Contains(out, "upgrade failed") {
				t.Fatalf("%s lost plain status: %s", scene, out)
			}
			if scene == "worker-capacity-blocked" && (!strings.Contains(out, "insufficient memory") || !strings.Contains(out, "smaller worker")) {
				t.Fatalf("capacity remedy lost: %s", out)
			}
		}
	}
}

func TestWorkerRollAttentionRankingFilterAndSummary(t *testing.T) {
	m := tuiTestModel(t, &uzicli.FakeClient{}, "")
	m.splitMode = "off"
	m = press(t, m, "2")
	rows := []workerRow{
		{w: apitypes.WorkerDTO{ID: "healthy", Name: "forge-a", Status: "online"}},
		{w: apitypes.WorkerDTO{ID: "provision", Name: "forge-b", Status: "online", UpgradeStatus: "provisioning"}},
		{w: apitypes.WorkerDTO{ID: "capacity", Name: "forge-z", Status: "online", UpgradeStatus: "capacity_blocked"}},
		{w: apitypes.WorkerDTO{ID: "failed", Name: "forge-y", Status: "online", UpgradeStatus: "provisioning_failed"}},
		{w: apitypes.WorkerDTO{ID: "storage", Name: "laptop", Status: "online", UpgradeStatus: "upgrade_failed"}},
		{w: apitypes.WorkerDTO{ID: "offline", Name: "forge-c", Status: "offline"}},
	}
	m = step(m, workersMsg{reqID: m.workers.waitID, rows: rows})
	ids := func(m tuiModel) []string {
		var got []string
		for _, row := range m.workers.visible(time.Now()) {
			got = append(got, row.w.ID)
		}
		return got
	}
	if got := ids(m); !reflect.DeepEqual(got, []string{"failed", "capacity", "storage", "offline", "provision", "healthy"}) {
		t.Fatal("mixed severity order", got)
	}
	m = press(t, m, "/")
	for _, key := range []string{"F", "O", "R", "G", "E", "enter"} {
		m = press(t, m, key)
	}
	if got := ids(m); !reflect.DeepEqual(got, []string{"failed", "capacity", "offline", "provision", "healthy"}) {
		t.Fatal("filtered severity order", got)
	}
	out := stripANSI(m.View().Content)
	for _, want := range []string{"forge-y", "forge-z", "forge-b", "forge-a", "4 need attention"} {
		if !strings.Contains(out, want) {
			t.Fatalf("filtered fleet lost %q: %s", want, out)
		}
	}
	if strings.Contains(out, "laptop") {
		t.Fatal("filter leaked excluded worker", out)
	}
	// The summary is fleet-wide even while the list is filtered.
	if got := stripANSI(m.workersSummary(200, false)); !strings.Contains(got, "6") || !strings.Contains(got, "4 need attention") {
		t.Fatal("summary counted provisioning or excluded a filtered-out failure", got)
	}
}

func TestWorkerRollDetailCapacityRemediesAndStorageFailure(t *testing.T) {
	for _, resource := range []string{"cpu", "memory", "cpu and memory"} {
		t.Run(resource, func(t *testing.T) {
			m := workersScene(true, "workers-list-120")
			w := apitypes.WorkerDTO{ID: "capacity", Name: "capacity-worker", Status: "online", UpgradeStatus: "capacity_blocked",
				UpgradeBlockingReason: strPtrT("scheduler capacity: insufficient " + resource)}
			m.workers.rows = []workerRow{{w: w}}
			next, _ := m.openWorker(w.ID)
			m = next.(tuiModel)
			m.width, m.height = 200, 60
			out := stripANSI(m.View().Content)
			for _, want := range []string{"✕ capacity blocked", "insufficient " + resource, "wait for available capacity", "choose a smaller worker", "ask an operator to add capacity"} {
				if !strings.Contains(out, want) {
					t.Fatalf("detail lost %q: %s", want, out)
				}
			}
			if strings.Contains(out, "upgrade failed") || strings.Contains(out, "rollback") {
				t.Fatal("capacity described as an upgrade/storage failure", out)
			}
		})
	}
	m := workersScene(true, "workers-list-120")
	m.workers.rows = []workerRow{{w: apitypes.WorkerDTO{ID: "storage", Name: "storage-worker", Status: "online",
		UpgradeStatus: "upgrade_failed", UpgradeBlockingContainer: strPtrT("storage-init"),
		UpgradeBlockingReason: strPtrT("persistent storage mount failed"), UpgradeDetail: strPtrT("volume unavailable")}}}
	next, _ := m.openWorker("storage")
	m = next.(tuiModel)
	m.width, m.height = 200, 60
	out := stripANSI(m.View().Content)
	for _, want := range []string{"✕ upgrade failed", "storage-init", "persistent storage mount failed", "volume unavailable"} {
		if !strings.Contains(out, want) {
			t.Fatalf("real storage failure lost %q: %s", want, out)
		}
	}
	if strings.Contains(out, "smaller worker") || strings.Contains(out, "capacity blocked") {
		t.Fatal("storage failure replaced by capacity advice", out)
	}
}

func TestWorkerRollColorProfileFallbacks(t *testing.T) {
	env := []string{"TERM=xterm-256color", "COLORTERM=truecolor", "CLICOLOR_FORCE=1", "TTY_FORCE=1"}
	var control bytes.Buffer
	colored := colorprofile.NewWriter(&control, env)
	if colored.Profile != colorprofile.TrueColor {
		t.Fatalf("positive color detection control = %v", colored.Profile)
	}
	if _, err := colored.WriteString("\x1b[38;2;255;0;0mprobe\x1b[0m"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(control.String(), "\x1b[38;2;") {
		t.Fatal("positive control did not emit color")
	}
	for _, mode := range []string{"NO_COLOR", "Ascii", "NoTTY"} {
		t.Run(mode, func(t *testing.T) {
			var buf bytes.Buffer
			writer := colorprofile.NewWriter(&buf, append(append([]string{}, env...), "NO_COLOR=1"))
			if writer.Profile != colorprofile.Ascii {
				t.Fatalf("NO_COLOR detection = %v", writer.Profile)
			}
			if mode == "Ascii" {
				writer.Profile = colorprofile.Ascii
			}
			if mode == "NoTTY" {
				writer.Profile = colorprofile.NoTTY
			}
			for _, status := range []string{"capacity_blocked", "provisioning", "provisioning_failed"} {
				m := workersScene(true, "workers-list-120")
				m.workers.rows = []workerRow{{w: apitypes.WorkerDTO{ID: "w", Name: "worker", Status: "online", UpgradeStatus: status}}}
				m = step(m, tea.ColorProfileMsg{Profile: writer.Profile})
				for _, detail := range []bool{false, true} {
					if detail {
						next, _ := m.openWorker("w")
						m = next.(tuiModel)
					}
					m.width, m.height = 200, 60
					buf.Reset()
					if _, err := writer.WriteString(m.View().Content); err != nil {
						t.Fatal(err)
					}
					out := buf.String()
					want := "✕ " + strings.ReplaceAll(status, "_", " ")
					if status == "provisioning" {
						want = "◌ provisioning"
					}
					if !strings.Contains(stripANSI(out), want) {
						t.Fatalf("%s detail=%t lost glyph/text %q: %s", status, detail, want, out)
					}
					if strings.Contains(out, "38;") || strings.Contains(out, "48;") {
						t.Fatal("downgrade retained color", out)
					}
					assertNoRawControls(t, mode, out)
				}
			}
		})
	}
}

func TestWorkerRollHostileShippedRenders(t *testing.T) {
	hostile := func(marker string) string {
		return marker + "\x1b[2J\x1b]0;injected\x07\x01\u202e\u2066\r\nFORGED_ROW\t" + strings.Repeat("界", 250)
	}
	for _, status := range []string{"capacity_blocked", "provisioning", "provisioning_failed"} {
		t.Run(status, func(t *testing.T) {
			// Isolate each field so preceding long diagnostics cannot hide the marker.
			for _, field := range []string{"name", "container", "reason", "detail", "target"} {
				t.Run(field, func(t *testing.T) {
					w := apitypes.WorkerDTO{ID: "w", Name: "worker", Status: "online", UpgradeStatus: status}
					marker := field + "-safe"
					value := hostile(marker)
					switch field {
					case "name":
						w.Name = value
					case "container":
						w.UpgradeBlockingContainer = &value
					case "reason":
						w.UpgradeBlockingReason = &value
					case "detail":
						w.UpgradeDetail = &value
					case "target":
						w.UpgradeTarget = value
					}
					m := workersScene(true, "workers-list-120")
					m.workers.rows = []workerRow{{w: w}}
					m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
					for _, width := range []int{80, 120} {
						m.width, m.height = width, 60
						frame := m.View().Content
						assertWorkerRollFrame(t, frame, width, m.height)
						if !strings.Contains(stripANSI(frame), strings.ReplaceAll(status, "_", " ")) || (field == "name" && !strings.Contains(frame, marker)) {
							t.Fatal("hostile list lost status/name positive control", frame)
						}
					}
					next, _ := m.openWorker(w.ID)
					m = next.(tuiModel)
					m.width, m.height = 240, 80
					out := m.View().Content
					assertWorkerRollFrame(t, out, m.width, m.height)
					clean := m
					clean.workers.rows = []workerRow{{w: apitypes.WorkerDTO{ID: w.ID, Name: "worker", Status: "online", UpgradeStatus: status}}}
					if strings.Count(out, "\n") != strings.Count(clean.View().Content, "\n") {
						t.Fatal("hostile detail changed the number of rendered rows", out)
					}
					drawn := field == "name" || (field == "target" && status != "provisioning") ||
						(field == "reason" && status != "provisioning") ||
						((field == "container" || field == "detail") && status == "provisioning_failed")
					if strings.Contains(stripANSI(out), marker) != drawn {
						t.Fatalf("detail marker %q drawn=%t: %s", marker, drawn, stripANSI(out))
					}
					if !strings.Contains(stripANSI(out), strings.ReplaceAll(status, "_", " ")) || strings.Contains(out, "upgrade failed") {
						t.Fatal("new status mislabeled", out)
					}
					fake := &uzicli.FakeClient{Workers: []apitypes.WorkerDTO{w},
						AdminWorkers: []apitypes.AdminWorkerDTO{{WorkerDTO: w, OwnerEmail: "owner@example.test"}}}
					for _, args := range [][]string{{"worker", "list"}, {"admin", "workers"}} {
						stdout, stderr, code := runCLI(t, fakeEnv(fake), args...)
						if code != 0 {
							t.Fatalf("%v: exit=%d stderr=%s", args, code, stderr)
						}
						assertNoRawControls(t, strings.Join(args, " "), stdout)
						if strings.ContainsRune(stdout, '\x1b') || len(strings.Split(strings.TrimSpace(stdout), "\n")) != 2 {
							t.Fatal("CLI escape or forged row", stdout)
						}
						cliDrawn := field == "name" || (args[0] == "admin" && (field == "reason" || (field == "container" && status != "capacity_blocked")))
						if strings.Contains(stdout, marker) != cliDrawn {
							t.Fatalf("%v marker %q drawn=%t: %s", args, marker, cliDrawn, stdout)
						}
						if !strings.Contains(stdout, strings.ReplaceAll(status, "_", " ")) || strings.Contains(stdout, "upgrade failed") {
							t.Fatal("CLI mislabeled new status", stdout)
						}
					}
				})
			}
		})
	}
}

func assertWorkerRollFrame(t *testing.T, out string, width, height int) {
	t.Helper()
	assertNoRawControls(t, "worker roll", out)
	if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b]0;") || strings.Contains(out, "\t") || strings.Contains(out, "\nFORGED_ROW") {
		t.Fatal("hostile terminal sequence reached frame", out)
	}
	if lines := strings.Split(out, "\n"); len(lines) > height {
		t.Fatalf("frame height=%d exceeds %d", len(lines), height)
	} else {
		for _, line := range lines {
			if visualWidth(line) > width {
				t.Fatalf("frame line exceeds %d columns: %q", width, line)
			}
		}
	}
}

func TestWorkerRollDisplays(t *testing.T) {
	for _, status := range []string{"capacity_blocked", "provisioning", "provisioning_failed"} {
		w := apitypes.WorkerDTO{Status: "online", UpgradeStatus: status}
		if got := upgradeCell(w); got != workerRollLabel(status) || strings.Contains(got, "upgrade") {
			t.Fatalf("%s label %q", status, got)
		}
		items := workerAttention(workerRow{w: w}, time.Now())
		if len(items) == 0 || (items[0].severity < 2) != (status != "provisioning") {
			t.Fatalf("%s lost failure attention: %+v", status, items)
		}
		if len(items) > 0 && strings.Contains(items[0].attnDetail, "upgrade failed") {
			t.Fatalf("incorrect first-provision/capacity wording: %+v", items)
		}
	}
	for _, resource := range []string{"cpu", "memory", "cpu and memory"} {
		w := apitypes.WorkerDTO{Status: "online", UpgradeStatus: "capacity_blocked",
			UpgradeBlockingReason: strPtrT("scheduler capacity: insufficient " + resource)}
		if got := blockingCell(w); !strings.Contains(got, resource) || !strings.Contains(got, "smaller worker") || strings.Contains(got, "rollback") {
			t.Fatal(got)
		}
	}
	if got := upgradeCell(apitypes.WorkerDTO{UpgradeStatus: "future_\x1b[31mstate"}); strings.ContainsRune(got, '\x1b') {
		t.Fatal("unsafe unknown status", got)
	}
}
