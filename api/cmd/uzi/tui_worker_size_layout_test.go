package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

// An admin-added size name is untrusted and unbounded; the KIND cell bounds it by terminal
// cells so a wide name cannot starve ATTENTION at 80 or 120 columns.
func TestWorkersAdminSizeNamesKeepAttentionVisible(t *testing.T) {
	docker := true
	names := []struct{ name, size string }{
		{"builtin", "l-highmem"},
		{"latin8", "gpu-a100"},
		{"cjk", "大型机器大型机器"},
		{"hostile", "a\x1b[31mb\x07c\x1b]0;x\x07def"},
	}
	for _, width := range []int{80, 120} {
		for _, admin := range []bool{false, true} {
			for _, tc := range names {
				t.Run(fmt.Sprintf("w%d/admin=%v/%s", width, admin, tc.name), func(t *testing.T) {
					m := workersScene(true, "workers-list-120")
					m.width, m.height = width, 30
					m.view = viewWorkers
					m.board.admin = admin
					m.workers.admin = admin
					r := workerRow{w: apitypes.WorkerDTO{ID: "w1", Name: "w1", Status: "online", Kind: "hosted", HostedSize: sp(tc.size), Ephemeral: true, Docker: &docker, Version: sp("0.85.1"), UpgradeStatus: "capacity_blocked"}}
					m.workers.rows = []workerRow{r}
					m.workers.selectedID = "w1"
					raw := m.View().Content
					text := stripANSI(raw)
					if strings.ContainsAny(text, "\x1b\x07") {
						t.Fatalf("control byte in plain text: %q", text)
					}
					sawHeader, sawLabel := false, false
					for _, l := range strings.Split(text, "\n") {
						if w := visualWidth(l); w > width {
							t.Errorf("line is %d cells, terminal is %d: %q", w, width, l)
						}
						if strings.Contains(l, "KIND") && strings.Contains(l, "ATTENTION") {
							sawHeader = true
						}
						if strings.Contains(l, "✕ capacity") {
							sawLabel = true
						}
					}
					if !sawHeader {
						t.Errorf("header lost ATTENTION:\n%s", text)
					}
					if !sawLabel {
						t.Errorf("attention label collapsed:\n%s", text)
					}
					if got := m.workerTableWidths(m.workers.rows, width).kind; got > workerKindMaxWidth {
						t.Errorf("kind column %d exceeds cap %d", got, workerKindMaxWidth)
					}
				})
			}
		}
	}
}

// A custom size on a docker worker, rendered beside the whole demo fleet: KIND yields
// before ATTENTION (the baseline "✕ data 92%" label survives at 80), the "+dk" marker
// survives the cap, builtin codes stay whole, and no line overflows.
func TestWorkersCustomSizeKeepsDockerMarkerAndFleetAttention(t *testing.T) {
	docker := true
	for _, width := range []int{80, 160} {
		for _, admin := range []bool{false, true} {
			t.Run(fmt.Sprintf("w%d/admin=%v", width, admin), func(t *testing.T) {
				m := workersScene(true, "workers-list-120")
				m.width, m.height = width, 60
				m.view = viewWorkers
				m.board.admin = admin
				m.workers.admin = admin
				m.workers.rows = append(m.workers.rows, workerRow{w: apitypes.WorkerDTO{ID: "wcustom", Name: "gpu-box", Status: "online", Kind: "hosted", HostedSize: sp("gpu-a100"), Docker: &docker, Version: sp("0.85.1")}})
				text := stripANSI(m.View().Content)
				for _, l := range strings.Split(text, "\n") {
					if w := visualWidth(l); w > width {
						t.Errorf("line is %d cells, terminal is %d: %q", w, width, l)
					}
				}
				if !strings.Contains(text, "✕ data 92%") {
					t.Errorf("baseline attention label lost:\n%s", text)
				}
				found := false
				for _, l := range strings.Split(text, "\n") {
					if strings.Contains(l, "gpu-box") {
						found = true
						if !strings.Contains(l, "host·") || !strings.Contains(l, "+dk") {
							t.Errorf("custom docker KIND lost its prefix or +dk: %q", l)
						}
					}
				}
				if !found {
					t.Errorf("custom worker row not rendered:\n%s", text)
				}
			})
		}
	}
}

func TestWorkerKindFitKeepsMarkersAndBuiltins(t *testing.T) {
	docker := true
	m := workersScene(true, "workers-list-120")
	row := func(size string, eph bool) workerRow {
		return workerRow{w: apitypes.WorkerDTO{Kind: "hosted", HostedSize: sp(size), Ephemeral: eph, Docker: &docker}}
	}
	if got := m.workerKindFit(row("l-highmem", false), workerKindAdminDenseWidth); got != "host·LHM+dk" {
		t.Errorf("builtin altered: %q", got)
	}
	got := m.workerKindFit(row("gpu-a100", false), workerKindAdminDenseWidth)
	if !strings.HasPrefix(got, "host·") || !strings.HasSuffix(got, "+dk") || visualWidth(got) > workerKindAdminDenseWidth {
		t.Errorf("capped kind = %q", got)
	}
}

func TestWorkersDetailSizeRowWrapsAt80(t *testing.T) {
	m := workerRenderModel(true)
	m.width = 80
	m.workers.rows[0].w.HostedSize = sp("extra-large-thing")
	m.workers.rows[0].w.HostedSizeSpec = &apitypes.WorkerSizeSpec{
		CPURequestMillicores: 250, CPULimitMillicores: 1000, MemoryRequestMiB: 1024, MemoryLimitMiB: 2048, DataGiB: 5, DinDDataGiB: 20,
	}
	lines, _ := m.workerDetailLines(time.Now())
	text := stripANSI(strings.Join(lines, "\n"))
	if !strings.Contains(text, "extra-large-thing") || !strings.Contains(text, "20 GiB dind") {
		t.Fatalf("size row lost the name or the dind quantity:\n%s", text)
	}
	for _, l := range strings.Split(text, "\n") {
		if w := visualWidth(l); w > 80 {
			t.Errorf("detail line is %d cells at 80: %q", w, l)
		}
	}
}

func TestSizeCellCapsAdminName(t *testing.T) {
	got := sizeCell(apitypes.WorkerDTO{Kind: "hosted", HostedSize: sp(strings.Repeat("x", 40))})
	if n := len([]rune(got)); n != 16 || !strings.HasSuffix(got, "…") {
		t.Errorf("SIZE cell = %q (%d runes), want 16 ending in an ellipsis", got, n)
	}
	if got := sizeCell(apitypes.WorkerDTO{Kind: "hosted", HostedSize: sp("l-highmem")}); got != "l-highmem" {
		t.Errorf("short name altered: %q", got)
	}
}
