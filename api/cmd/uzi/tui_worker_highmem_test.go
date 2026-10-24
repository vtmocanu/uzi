package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestWorkersHighmemKindCode(t *testing.T) {
	m := workersScene(true, "workers-list-120")
	for _, tc := range []struct {
		size, kind        string
		ephemeral, docker bool
	}{
		{"l-highmem", "host·LHM", false, false},
		{"L-HIGHMEM", "host·LHM", false, false},
		{"l-highmem", "eph·LHM+dk", true, true},
		{"l-highmem", "host·LHM+dk", false, true},
	} {
		r := workerRow{w: apitypes.WorkerDTO{Kind: "hosted", HostedSize: sp(tc.size), Ephemeral: tc.ephemeral, Docker: &tc.docker}}
		if got := m.workerKind(r); got != tc.kind {
			t.Errorf("size %q: got %q want %q", tc.size, got, tc.kind)
		}
	}
}

func TestWorkersHighmemKindRowUnclamped(t *testing.T) {
	for _, width := range []int{80, 120} {
		t.Run(fmt.Sprintf("width=%d", width), func(t *testing.T) {
			m := workersScene(true, "workers-list-120")
			docker := true
			r := workerRow{w: apitypes.WorkerDTO{ID: "hm", Name: "hm", Status: "online", Kind: "hosted", HostedSize: sp("l-highmem"), Docker: &docker, Version: sp("0.85.1")}}
			m.workers.rows = []workerRow{r}
			row := stripANSI(m.workerRowLine(r, true, width, m.workerTableWidths(m.workers.rows, width), time.Now()))
			if !strings.Contains(row, "host·LHM+dk") {
				t.Fatalf("kind clamped at width %d: %q", width, row)
			}
		})
	}
}

// The version column absorbs the leftover width after the fixed columns, so a
// wider KIND column (11 for a docker l-highmem worker) must shrink it by 2.
func TestWorkersKindWidthFeedsVersionBudget(t *testing.T) {
	docker := true
	mk := func(size string) workerRow {
		return workerRow{w: apitypes.WorkerDTO{ID: size, Name: size, Status: "online", Kind: "hosted", HostedSize: sp(size), Docker: &docker, Version: sp("0.85.1-rc.1+build.12345")}}
	}
	m := workersScene(true, "workers-list-120")
	m.board.admin = true
	const width = 120
	// Fixed budget with kind 11 and admin scope is 102 cells: 120-102-4 = 14.
	if got := m.workerTableWidths([]workerRow{mk("l-highmem")}, width); got.kind != 11 || got.version != 14 {
		t.Errorf("l-highmem: kind=%d version=%d want 11 and 14", got.kind, got.version)
	}
	// Kind 9 frees two cells: 120-100-4 = 16.
	if got := m.workerTableWidths([]workerRow{mk("l")}, width); got.kind != 9 || got.version != 16 {
		t.Errorf("l: kind=%d version=%d want 9 and 16", got.kind, got.version)
	}
}

func TestWorkersHighmemDetailShowsFullSize(t *testing.T) {
	m := workerRenderModel(true)
	m.workers.rows[0].w.HostedSize = sp("l-highmem")
	lines, _ := m.workerDetailLines(time.Now())
	if !strings.Contains(stripANSI(strings.Join(lines, "\n")), "size l-highmem") {
		t.Fatalf("detail lost the full size:\n%s", stripANSI(strings.Join(lines, "\n")))
	}
}

func TestWorkersKindColumnWidthIsMeasuredPerFleet(t *testing.T) {
	docker := true
	mk := func(size string, dk *bool) workerRow {
		return workerRow{w: apitypes.WorkerDTO{ID: size, Name: size, Status: "online", Kind: "hosted", HostedSize: sp(size), Docker: dk, Version: sp("0.85.1")}}
	}
	nextCol := func(m tuiModel, rows []workerRow, width int) int {
		m.workers.rows = rows
		hdr := ""
		for _, l := range strings.Split(stripANSI(m.renderWorkersBody(20, true)), "\n") {
			if strings.Contains(l, "KIND") && strings.Contains(l, "RUNS") {
				hdr = l
			}
		}
		if hdr == "" {
			t.Fatalf("no header at width %d", width)
		}
		return visualWidth(hdr[:strings.Index(hdr, "RUNS")]) - visualWidth(hdr[:strings.Index(hdr, "KIND")])
	}
	for _, width := range []int{80, 120} {
		m := workersScene(true, "workers-list-120")
		m.width = width
		plain := []workerRow{mk("s", nil), mk("m", nil), mk("l", nil), mk("l-highmem", nil)}
		if got := m.workerTableWidths(plain, width).kind; got != 9 {
			t.Errorf("width %d: kind=%d want 9 for a fleet without a docker l-highmem worker", width, got)
		}
		wide := append([]workerRow{mk("l-highmem", &docker)}, plain...)
		if got := m.workerTableWidths(wide, width).kind; got != 11 {
			t.Errorf("width %d: kind=%d want 11", width, got)
		}
		if a, b := nextCol(m, plain, width), nextCol(m, wide, width); a != 9+1 || b != 11+1 {
			t.Errorf("width %d: KIND header->RUNS offsets %d,%d want 10,12", width, a, b)
		}
	}
}
