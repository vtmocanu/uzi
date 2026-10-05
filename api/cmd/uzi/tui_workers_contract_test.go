package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestWorkersApprovedLowercaseKind(t *testing.T) {
	m := workersScene(true, "workers-list-120")
	for _, tc := range []struct {
		size, kind        string
		ephemeral, docker bool
	}{{"l", "host·L", false, false}, {"m", "eph·M+dk", true, true}, {"S", "host·S", false, false}, {"future", "host·?", false, false}} {
		r := workerRow{w: apitypes.WorkerDTO{Kind: "hosted", HostedSize: sp(tc.size), Ephemeral: tc.ephemeral, Docker: &tc.docker}}
		if got := m.workerKind(r); got != tc.kind {
			t.Errorf("size %q: got %q want %q", tc.size, got, tc.kind)
		}
	}
	for _, w := range newDemoClient().Workers {
		if w.HostedSize != nil && *w.HostedSize != strings.ToLower(*w.HostedSize) {
			t.Errorf("demo uses a non-wire size %q", *w.HostedSize)
		}
	}
}

func TestWorkersApprovedFleetTitle(t *testing.T) {
	for _, split := range []bool{false, true} {
		for _, width := range []int{80, 120} {
			for _, view := range []tuiView{viewBoard, viewWorkers} {
				t.Run(fmt.Sprintf("split=%t/width=%d/view=%d", split, width, view), func(t *testing.T) {
					m := workersScene(true, "workers-list-120")
					m.splitMode = "off"
					m = resizeSplit(m, width, 60)
					if split {
						m.splitMode = "auto"
						m = resizeSplit(m, width, 60)
					}
					m.setListView(view)
					lines := strings.Split(stripANSI(m.View().Content), "\n")
					inline := split || width >= 120
					if strings.Contains(lines[0], "need attention") != inline {
						t.Fatalf("fleet placement inline=%t: %q", inline, lines[:min(3, len(lines))])
					}
					if !inline && !strings.Contains(lines[1], "need attention") {
						t.Fatalf("missing fleet fallback: %q", lines[:2])
					}
					if strings.Contains(strings.Join(lines, "\n"), "2 for detail") {
						t.Fatal("numeric navigation hint leaked")
					}
				})
			}
		}
	}
}

func TestWorkersApprovedMissingFloorWorker(t *testing.T) {
	m := workersScene(true, "floor-fleet")
	m.width = 120
	for _, status := range []string{"queued", "claimed", "running", "awaiting_approval", "completed", "failed", "cancelled"} {
		r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "run", Status: status}}
		got := stripANSI(m.boardRow(r, false, boardMarkerCols{}))
		want := "no worker yet"
		if status == "completed" || status == "failed" || status == "cancelled" {
			want = "—"
		}
		cell := []rune(got)[len([]rune(got))-16:]
		if strings.TrimSpace(string(cell)) != want || strings.Contains(string(cell), "?") {
			t.Errorf("%s worker cell %q want %q", status, string(cell), want)
		}
	}
}

func TestWorkersApprovedRunIDCells(t *testing.T) {
	for _, tc := range []struct{ name, id, want string }{
		{"multibyte", "界😀界😀界😀BAD", "界😀界…"},
		{"escape before cut", "\x1b[31mBAD界😀界😀", "[31mBAD…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := workerRenderModel(true)
			w := &m.workers.rows[0].w
			w.ReportedRuns = []apitypes.WorkerReportedRunDTO{{RunID: tc.id, Phase: "running"}}
			w.RunDisk = []apitypes.WorkerRunDiskDTO{{RunID: tc.id, SampledAt: time.Now()}}
			lines, _ := m.workerDetailLines(time.Now())
			out := strings.Join(lines, "\n")
			assertNoRawControls(t, "reported/run-disk IDs", out)
			clean := stripANSI(out)
			if !utf8.ValidString(clean) || strings.ContainsRune(clean, '\uFFFD') {
				t.Fatalf("split rune: %q", clean)
			}
			runLine, diskLine := "", ""
			for _, line := range strings.Split(clean, "\n") {
				if strings.Contains(line, "▌ ") {
					runLine = line
				}
				if strings.Contains(line, "largest HOME") {
					diskLine = line
				}
			}
			for _, line := range []string{runLine, diskLine} {
				if !strings.Contains(line, tc.want) {
					t.Errorf("sanitise before cell truncation: %q want %q", line, tc.want)
				}
			}
		})
	}
}

func TestWorkersApprovedArrows(t *testing.T) {
	m, _ := workerNavFixture(t)
	m.view = viewWorkers
	m.topTab = viewWorkers
	m = press(t, m, keyRight)
	if m.view != viewWorker {
		t.Fatal("right did not open worker")
	}
	next, cmd := m.handleKey(keyRight)
	m = next.(tuiModel)
	if cmd == nil {
		t.Fatal("right did not open reported run")
	}
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	if m.view != viewDetail || m.detail.runID != "B" {
		t.Fatal("reported-run right navigation")
	}
	m = press(t, m, keyEsc)
	if m.view != viewWorker {
		t.Fatal("run return")
	}
	m = press(t, m, keyLeft)
	if m.view != viewWorkers {
		t.Fatal("left did not restore workers list")
	}
}

func TestWorkersApprovedPlainTabsAndHelp(t *testing.T) {
	m := workersScene(true, "workers-list-120")
	for _, view := range []tuiView{viewBoard, viewWorkers, viewPulls, viewCI} {
		strip := stripANSI(m.tabStrip(false, view, false))
		for _, digit := range "1234" {
			if strings.ContainsRune(strip, digit) {
				t.Fatalf("numbered tab strip: %q", strip)
			}
		}
		m.view = view
		if strings.Contains(m.splitFooterLine(), "1-4") {
			t.Fatal("numbered split footer")
		}
		help := strings.Join(helpLines(view), "\n")
		if !strings.Contains(help, "1 / 2 / 3 / 4") {
			t.Fatalf("help omitted numeric bindings: %s", help)
		}
	}
}

func TestWorkersApprovedVersionWidth(t *testing.T) {
	m := workersScene(true, "workers-list-120")
	m.width = 160
	r := m.workers.rows[0]
	r.w.Version = sp("0.85.1+gba846d7")
	r.w.UpgradeStatus = "outdated"
	m.workers.rows = []workerRow{r}
	row := stripANSI(m.workerRowLine(r, true, m.width, m.workerTableWidths(m.workers.rows, m.width), time.Now()))
	if !strings.Contains(row, "0.85.1+gba846d7↑") {
		t.Fatalf("version truncated with spare width: %s", row)
	}
	r.w.Version = sp(strings.Repeat("v", 30))
	m.workers.rows = []workerRow{r}
	row = stripANSI(m.workerRowLine(r, true, m.width, m.workerTableWidths(m.workers.rows, m.width), time.Now()))
	if !strings.Contains(row, strings.Repeat("v", 16)+"…↑") || strings.Contains(row, strings.Repeat("v", 18)) {
		t.Fatalf("version must cap at 18 cells including marker: %s", row)
	}
}

func TestWorkersApprovedSplitMeters(t *testing.T) {
	m := workersScene(true, "split-workers-top")
	m.width = 160
	meter := "accounts-fixture"
	m.rateLimits = []apitypes.TokenRateLimitDTO{{SecretID: "fixture", Label: meter, IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "ok"}}}
	// Claude meter aliases are supplied by the seeded client and visible only with floor.
	m.board.runs = []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "run", Status: "running"}}}
	m.setListView(viewWorkers)
	top := strings.Split(stripANSI(m.View().Content), "\n")[0]
	if !strings.Contains(top, "floor · [workers]") {
		t.Fatalf("top tabs missing: %q", top)
	}
	// Use the shared meter compositor to prove the right edge and fallback, independent of provider vocabulary.
	summary := "$1733+ 7d · 200 runs · 1–20"
	joined := m.boardMeterSummaryLines([]string{meter}, summary)
	if len(joined) != 1 || !strings.HasPrefix(joined[0], meter) || !strings.HasSuffix(joined[0], summary) || visualWidth(joined[0]) != 160 {
		t.Fatalf("meter summary alignment: %q", joined)
	}
	workersHeader := stripANSI(strings.Join(m.splitHeader(time.Now()), "\n"))
	if strings.Contains(workersHeader, "1 runs") || strings.Contains(workersHeader, meter) {
		t.Fatalf("workers exposed floor summary: %s", workersHeader)
	}
	m.setListView(viewBoard)
	floorHeader := stripANSI(strings.Join(m.splitHeader(time.Now()), "\n"))
	if !strings.Contains(floorHeader, "[floor] · workers") || !strings.Contains(floorHeader, "1 runs") {
		t.Fatalf("floor header omitted summary: %s", floorHeader)
	}
	frame := stripANSI(m.View().Content)
	found := false
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, meter) {
			found = true
			if !strings.Contains(line, "1 runs · 1–1") || visualWidth(line) != m.width {
				t.Fatalf("floor summary must share accounts at right edge: %q", line)
			}
		}
	}
	if !found {
		t.Fatal("positive control: floor account meter missing")
	}
	m.splitMode = "off"
	frame = stripANSI(m.View().Content)
	found = false
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, meter) {
			found = true
			if !strings.Contains(line, "1 runs · 1–1") {
				t.Fatalf("full-floor summary separate from account line: %q", line)
			}
		}
	}
	if !found {
		t.Fatal("positive control: full-floor account meter missing")
	}
	m.width = 40
	fallback := m.boardMeterSummaryLines([]string{strings.Repeat("m", 38)}, summary)
	if len(fallback) != 2 || !strings.HasSuffix(fallback[1], summary) {
		t.Fatalf("summary fallback: %q", fallback)
	}
}

func TestWorkersApprovedDetailHeader(t *testing.T) {
	now := time.Now()
	m := workerRenderModel(true)
	w := &m.workers.rows[0].w
	w.Name = "forge"
	w.HostedSize = sp("l")
	w.OnlineSince = tp(now.Add(-48 * time.Hour))
	w.LastHeartbeatAt = tp(now.Add(-3 * time.Second))
	lines, _ := m.workerDetailLines(now)
	header := stripANSI(lines[1])
	if !strings.Contains(header, "worker › forge  ● idle  host·L  up 2d0h · heartbeat 3s ago") {
		t.Fatalf("detail header: %q", header)
	}
	if lines[2] != "" {
		t.Fatalf("heartbeat unnecessarily took a row: %q", lines[2])
	}
	m.width = 40
	lines, _ = m.workerDetailLines(now)
	if !strings.Contains(stripANSI(lines[2]), "up 2d0h · heartbeat 3s ago") {
		t.Fatalf("narrow fallback: %q", lines[:3])
	}
	w.OnlineSince = nil
	m.width = 120
	lines, _ = m.workerDetailLines(now)
	if strings.Contains(stripANSI(lines[1]), "up ?") || !strings.Contains(stripANSI(lines[1]), "heartbeat 3s ago") {
		t.Fatal("unknown uptime invented")
	}
	w.Status = "offline"
	lines, _ = m.workerDetailLines(now)
	if !strings.Contains(stripANSI(lines[1]), "last heartbeat 3s ago") {
		t.Fatal("offline heartbeat wording")
	}
}

func TestWorkersStaleResourceCells(t *testing.T) {
	for _, tc := range []struct {
		status string
		cpu    float64
		used   int64
		memory string
	}{
		{"online", 12, 12 << 30, "12.0/16G"},
		{"offline", 12, 12 << 30, "~12.0/16G"},
		{"offline", 100, 79 * (1 << 30) / 10, "~7.9/16G"},
		{"offline", 100, 100 << 30, "~100.0/16G"},
	} {
		t.Run(fmt.Sprintf("%s/cpu=%.0f/memory=%s", tc.status, tc.cpu, tc.memory), func(t *testing.T) {
			m := workersScene(true, "workers-list-120")
			r := workerRow{w: apitypes.WorkerDTO{ID: "sample", Name: "sample", Status: tc.status, Kind: "hosted", Version: sp("0.85.1"), StatsCPUPct: &tc.cpu, StatsMemBytes: &tc.used, StatsMemLimitBytes: i64(16 << 30), StatsDiskDataBytes: i64(100), StatsDiskDataTotalBytes: i64(100)}}
			m.workers.rows = []workerRow{r}
			row := stripANSI(m.workerRowLine(r, true, 120, m.workerTableWidths(m.workers.rows, 120), time.Now()))
			cpu := fmt.Sprintf("%.0f%%", tc.cpu)
			disk := "data     ▮▮▮▮ 100%"
			if tc.status == "offline" {
				cpu = "~" + cpu
				disk = "~" + disk
			}
			for _, want := range []string{cpu, tc.memory, disk} {
				if !strings.Contains(row, want) {
					t.Errorf("lost resource reading %q: %s", want, row)
				}
			}
			if visualWidth(row) > 120 {
				t.Fatalf("resource row exceeds viewport: %s", row)
			}
		})
	}
}

func TestFloorTinyMeterSummaryHeight(t *testing.T) {
	for width := 40; width <= 58; width++ {
		t.Run(itoa(width), func(t *testing.T) {
			m := workersScene(true, "floor-fleet")
			m.splitMode = "off"
			m.width, m.height = width, 6
			m.rateLimits = []apitypes.TokenRateLimitDTO{{Label: "account", IsDefault: true, Limits: apitypes.RateLimitDTO{Status: "ok"}}}
			frame := stripANSI(m.View().Content)
			lines := strings.Split(frame, "\n")
			if len(lines) > m.height {
				t.Fatalf("floor at width %d overflows height %d with %d lines: %s", width, m.height, len(lines), frame)
			}
			if !strings.Contains(lines[len(lines)-1], "enter/→") {
				t.Fatalf("footer lost: %s", frame)
			}
		})
	}
}

func TestWorkerUnknownHeaderAndEmptyCapabilities(t *testing.T) {
	m := workerRenderModel(true)
	w := &m.workers.rows[0].w
	w.LastHeartbeatAt = nil
	w.OnlineSince = nil
	w.Capabilities = nil
	lines, _ := m.workerDetailLines(time.Now())
	out := stripANSI(strings.Join(lines, "\n"))
	if strings.Contains(stripANSI(lines[1]), "heartbeat") || strings.Contains(stripANSI(lines[1]), "up ?") {
		t.Fatalf("unknown header timestamp displayed: %s", lines[1])
	}
	if !strings.Contains(out, "capabilities  none") {
		t.Fatalf("empty capabilities unclear: %s", out)
	}
}
