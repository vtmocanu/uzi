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
		{"longname-suffix", 8, "…-suffix"},
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
