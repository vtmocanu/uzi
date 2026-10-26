package main

import (
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func TestWorkerNameTailRows(t *testing.T) {
	for _, dark := range []bool{true, false} {
		for _, name := range []string{"base.l-highmem-a32f", "base.l-highmem-b917", "界界界界界界界界-a32f", "\u202e\x07\x01base.l-highmem-a32f", "short-a32f"} {
			m := workerRenderModel(dark)
			m.width = 160
			r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "run", Status: "running", IssueTitle: "title"}, WorkerName: sp(name)}
			row := workerRow{w: apitypes.WorkerDTO{ID: "render", Name: name, Status: "online", Kind: "hosted"}}
			m.workers.rows = []workerRow{row}
			for site, out := range map[string]string{
				"board16": m.boardRow(r, false, boardMarkerCols{}, m.boardAccountCols([]apitypes.RunListItemDTO{r})),
				"table13": m.workerRowLine(row, false, 160, m.workerTableWidths([]workerRow{row}, 160), time.Now()),
			} {
				t.Run(site+"/"+name, func(t *testing.T) {
					assertNoRawControls(t, site, out)
					clean := stripANSI(out)
					suffix := name[strings.LastIndex(name, "-"):]
					if !strings.Contains(clean, suffix) {
						t.Fatalf("lost worker suffix %q: %q", suffix, clean)
					}
					if name == "short-a32f" && !strings.Contains(clean, name) {
						t.Fatalf("short name changed: %q", clean)
					}
					if visualWidth(clean) > 160 {
						t.Fatalf("row overflow: %q", clean)
					}
				})
			}
		}
	}
}

func TestWorkerNameTailOtherViews(t *testing.T) {
	name := strings.Repeat("界base", 15) + "-a32f"
	for _, dark := range []bool{true, false} {
		m := workerRenderModel(dark)
		m.width = 80
		m.workers.rows[0].w.Name = name
		m.detail.run.WorkerName = sp(name)
		for site, out := range map[string]string{
			"rail":    m.railWorkerLine(),
			"readout": strings.Join(m.workerReadout(m.workers.rows[0], 30), "\n"),
			"detail":  m.View().Content,
		} {
			t.Run(site, func(t *testing.T) {
				if !strings.Contains(stripANSI(out), "-a32f") {
					t.Fatalf("lost worker suffix: %q", stripANSI(out))
				}
			})
		}
	}
}

func TestWorkerNameMiddleCell(t *testing.T) {
	var renderer *tuiRenderer
	for _, tt := range []struct {
		name  string
		width int
		want  string
	}{
		{"short", 13, "short"},
		{"base.l-highmem-a32f", 16, "base.l-h…em-a32f"},
		{"base.l-highmem-a32f", 13, "base.l…m-a32f"},
		{"longname-suffix", 8, "lon…ffix"},
		{"abcdefghij", 5, "ab…ij"},
		{"abc-longsegment", 5, "ab…nt"},
		{"界界界界-a32f", 13, "界界界界-a32f"},
		{"base.l-highmem-a32f", 1, "…"},
		{"base.l-highmem-a32f", 0, ""},
		{"base.l-highmem-a32f", -1, ""},
		{"\u202e\x07\x01short", 13, "short"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := renderer.WorkerName(tt.name, tt.width)
			if got != tt.want {
				t.Fatalf("WorkerName(%q, %d) = %q, want %q", tt.name, tt.width, got, tt.want)
			}
			if visualWidth(got) > max(0, tt.width) {
				t.Fatalf("width overflow: %q", got)
			}
		})
	}
}

func TestWorkerNameDistinctHeads(t *testing.T) {
	var renderer *tuiRenderer
	for _, width := range []int{13, 16} {
		a := renderer.WorkerName("laptop-buildserver123", width)
		b := renderer.WorkerName("desktop-buildserver123", width)
		if a == b || !strings.HasPrefix(a, "lapt") || !strings.HasPrefix(b, "desk") {
			t.Errorf("width %d lost distinguishing heads: %q and %q", width, a, b)
		}
	}
	m := workerRenderModel(true)
	m.width = 160
	for _, name := range []string{"laptop-buildserver123", "desktop-buildserver123"} {
		r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "run", Status: "running", IssueTitle: "title"}, WorkerName: sp(name)}
		row := workerRow{w: apitypes.WorkerDTO{ID: "render", Name: name, Status: "online", Kind: "hosted"}}
		for site, out := range map[string]string{
			"board16": m.boardRow(r, false, boardMarkerCols{}, m.boardAccountCols([]apitypes.RunListItemDTO{r})),
			"table13": m.workerRowLine(row, false, 160, m.workerTableWidths([]workerRow{row}, 160), time.Now()),
		} {
			if !strings.Contains(stripANSI(out), name[:4]) {
				t.Errorf("%s lost distinguishing head of %q: %q", site, name, stripANSI(out))
			}
		}
	}
}

func TestWorkerNameNarrowDetailHeader(t *testing.T) {
	for _, width := range []int{20, 21, 22, 26, 30} {
		m := workerRenderModel(true)
		m.width = width
		m.workers.rows[0].w.Name = "base.l-highmem-a32f"
		m.workers.rows[0].w.OnlineSince = tp(time.Now().Add(-time.Minute))
		out := stripANSI(m.View().Content)
		found := false
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, "worker › ") {
				found = true
				want := "worker › " + m.renderer.WorkerName("base.l-highmem-a32f", min(13, width-9))
				if !strings.HasPrefix(line, want) {
					t.Errorf("width %d lost minimum name: %q", width, line)
				}
				if strings.HasSuffix(line, "-a32f…") || strings.HasSuffix(line, "-a32f …") {
					t.Errorf("width %d has ambiguous tail ellipsis: %q", width, line)
				}
			}
		}
		if !found {
			t.Fatalf("width %d missing worker header: %q", width, out)
		}
		if !strings.Contains(out, "\n  up 1m\n") {
			t.Errorf("width %d lost separate heartbeat: %q", width, out)
		}
	}
	m := workerRenderModel(true)
	m.width = 80
	m.workers.rows[0].w.Name = "short"
	m.workers.rows[0].w.OnlineSince = tp(time.Now().Add(-time.Minute))
	lines, _ := m.workerDetailLines(time.Now())
	if !strings.Contains(stripANSI(lines[1]), "short") || !strings.Contains(stripANSI(lines[1]), "up 1m") {
		t.Errorf("short name lost inline heartbeat: %q", stripANSI(lines[1]))
	}
}

func TestWorkerNameHostileAllSites(t *testing.T) {
	const payload = "\x1b[2J\x1b]8;;evil\x07x\x1b]8;;\x07"
	for _, dark := range []bool{true, false} {
		m := workerRenderModel(dark)
		m.width = 160
		name := payload + "base.l-highmem-a32f"
		m.workers.rows[0].w.Name = name
		m.detail.run.WorkerName = sp(name)
		row := m.workers.rows[0]
		r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "run", Status: "running", IssueTitle: "title"}, WorkerName: sp(name)}
		for site, out := range map[string]string{
			"board":   m.boardRow(r, false, boardMarkerCols{}, m.boardAccountCols([]apitypes.RunListItemDTO{r})),
			"table":   m.workerRowLine(row, false, 160, m.workerTableWidths([]workerRow{row}, 160), time.Now()),
			"rail":    m.railWorkerLine(),
			"readout": strings.Join(m.workerReadout(row, 30), "\n"),
			"detail":  m.View().Content,
		} {
			t.Run(site, func(t *testing.T) {
				assertNoRawControls(t, site, out)
				if strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\x1b]8;;evil") {
					t.Fatalf("hostile escape survived: %q", out)
				}
			})
		}
	}
}

func TestWorkerNameAdminReadoutKeepsFittedName(t *testing.T) {
	for _, width := range []int{30, 120} {
		for _, spare := range []int{0, 1, 2} {
			m := workerRenderModel(true)
			m.board.admin = true
			row := m.workers.rows[0]
			row.w.Name = strings.Repeat("manual", 30) + "-a32f"
			if spare > 0 {
				row.w.Name = strings.Repeat("m", width-9-spare-5) + "-a32f"
			}
			row.workerOwner = "owner@example.test"
			want := m.renderer.WorkerName(row.w.Name, width-9)
			out := stripANSI(m.workerReadout(row, width)[0])
			if !strings.Contains(out, want) || !strings.HasSuffix(out, "-a32f") {
				t.Errorf("width %d lost fitted name %q: %q", width, want, out)
			}
			if visualWidth(out) > width || strings.Contains(out, "owner") {
				t.Errorf("width %d should have no metadata space: %q", width, out)
			}
		}
	}
}

func TestWorkerNameReadoutDropsTinyAttentionMetadata(t *testing.T) {
	m := workerRenderModel(true)
	m.board.admin = false
	row := m.workers.rows[0]
	row.w.Status = "offline"
	for _, spare := range []int{1, 2, 3} {
		row.w.Name = strings.Repeat("m", 120-9-spare-5) + "-a32f"
		out := stripANSI(m.workerReadout(row, 120)[0])
		if out != "selected "+row.w.Name {
			t.Errorf("spare %d should drop attention metadata: %q", spare, out)
		}
	}
}
