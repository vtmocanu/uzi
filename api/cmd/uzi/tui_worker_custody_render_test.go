package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func custodyCount(n int) *int { return &n }

func TestWorkerCustodyDecisionSurfaces(t *testing.T) {
	for _, tc := range []struct {
		name      string
		count     *int
		retaining bool
		state     string
		cue       bool
	}{
		{"healthy live custody", custodyCount(0), true, "busy", false},
		{"absent with retained source", nil, true, "busy", false},
		{"absent without retained source", nil, false, "busy", false},
		{"one decision", custodyCount(1), true, "holding", true},
		{"multiple decisions", custodyCount(3), true, "holding", true},
		{"decision independent of old boolean", custodyCount(1), false, "holding", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, dark := range []bool{true, false} {
				for _, width := range []int{80, 180} {
					m := workerRenderModel(dark)
					m.board.admin, m.workers.admin = false, false
					m.width = width
					r := workerRow{w: apitypes.WorkerDTO{
						ID: "custody", Name: "custody", Status: "online", Kind: "hosted",
						Busy: true, ActiveRuns: 1, MaxConcurrentRuns: custodyCount(2),
						RetainingUnpublishedWork: tc.retaining, CustodyDecisionsNeeded: tc.count,
					}}
					m.workers.rows = []workerRow{r}
					m.workers.selectedID = r.w.ID
					m.view = viewWorkers
					row := stripANSI(m.workerRowLine(r, true, width, m.workerTableWidths(m.workers.rows, width), time.Now()))
					requireWorkerText(t, row, workerStateGlyph(tc.state)+" "+tc.state)
					if tc.cue {
						requireWorkerText(t, row, "⚑ unpublished work")
						if strings.Contains(row, " +") {
							t.Fatalf("multiple holds became multiple row warnings: %q", row)
						}
					} else if !strings.HasSuffix(strings.TrimSpace(row), "—") {
						t.Fatalf("healthy row ATTENTION is not empty: %q", row)
					}
					readout := stripANSI(strings.Join(m.workerReadout(r, width), "\n"))
					requireWorkerText(t, readout, "selected custody")
					wantFooter := "selected custody"
					if tc.cue {
						if width >= 120 {
							wantFooter += " · ⚑ unpublished work"
						} else {
							wantFooter += "\n  ⚑ unpublished work"
						}
					}
					if readout != wantFooter {
						t.Fatalf("width %d selected footer: got %q, want %q", width, readout, wantFooter)
					}
					list := stripANSI(m.View().Content)
					requireWorkerText(t, list, "STATE", "ATTENTION", workerStateGlyph(tc.state)+" "+tc.state, wantFooter)
					m.view = viewWorker
					m.workerDetail = workerDetailState{workerID: r.w.ID}
					detail := stripANSI(m.View().Content)
					requireWorkerText(t, detail, "worker › custody  "+workerStateGlyph(tc.state)+" "+tc.state)
					if tc.cue {
						requireWorkerText(t, detail, "unpublished work needs an owner decision")
						if strings.Count(detail, "unpublished work needs an owner decision") != 1 {
							t.Fatalf("duplicate detail cue: %s", detail)
						}
					} else {
						requireWorkerText(t, detail, "no reported warnings")
						for surface, out := range map[string]string{"row": row, "readout": readout, "list": list, "detail": detail} {
							if strings.Contains(out, "unpublished work") || strings.Contains(out, "holding") || strings.Contains(out, "owner decision") {
								t.Fatalf("%s shows custody attention without a decision: %s", surface, out)
							}
						}
					}
					for n := 1; n <= 2; n++ {
						second := r
						second.w.ID, second.w.Name = "second", "second"
						m.workers.rows = []workerRow{r, second}[:n]
						want := fmt.Sprintf("workers · %d · %d online · %d/%d run slots in use · 0/? cross-checks", n, n, n, n*2)
						if tc.cue {
							want += fmt.Sprintf(" · %d holding · %d need attention", n, n)
						}
						if got := stripANSI(m.workersSummary(180)); got != want {
							t.Fatalf("summary got %q, want %q", got, want)
						}
					}
				}
			}
		})
	}
}

func TestWorkersHealthyCustodySummary(t *testing.T) {
	for _, n := range []int{1, 2} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			m := workerRenderModel(true)
			m.board.admin, m.workers.admin = false, false
			m.workers.rows = nil
			for i := 0; i < n; i++ {
				m.workers.rows = append(m.workers.rows, workerRow{w: apitypes.WorkerDTO{
					ID: fmt.Sprint(i), Status: "online", Busy: true, ActiveRuns: 1,
					MaxConcurrentRuns: custodyCount(2), RetainingUnpublishedWork: true,
					CustodyDecisionsNeeded: custodyCount(0),
				}})
			}
			want := fmt.Sprintf("workers · %d · %d online · %d/%d run slots in use · 0/? cross-checks", n, n, n, n*2)
			if got := stripANSI(m.workersSummary(180)); got != want {
				t.Fatalf("healthy custody summary: got %q, want %q", got, want)
			}
		})
	}
}

func TestWorkerCustodyDecisionSortAndIndependentWarnings(t *testing.T) {
	now := time.Now()
	m := workerRenderModel(true)
	m.board.admin, m.workers.admin = false, false
	mk := func(id, name string, count *int) workerRow {
		return workerRow{w: apitypes.WorkerDTO{
			ID: id, Name: name, Status: "online", Busy: true, ActiveRuns: 1,
			RetainingUnpublishedWork: true, CustodyDecisionsNeeded: count,
		}}
	}
	healthy := mk("healthy", "z-healthy", custodyCount(0))
	absent := mk("absent", "y-absent", nil)
	plain := mk("plain", "a-plain", custodyCount(0))
	plain.w.RetainingUnpublishedWork = false
	heldOne := mk("held-one", "x-held", custodyCount(1))
	heldMany := mk("held-many", "x-held", custodyCount(4))
	offline := mk("offline", "b-offline", custodyCount(0))
	offline.w.Status = "offline"
	blocked := mk("blocked", "z-blocked", custodyCount(0))
	blocked.w.OutboxBlocked = sp("delivery refused")
	m.workers.rows = []workerRow{healthy, absent, heldOne, plain, blocked, heldMany, offline}
	var ids []string
	for _, r := range m.workers.visible(now) {
		ids = append(ids, r.w.ID)
	}
	want := []string{"blocked", "offline", "held-many", "held-one", "plain", "absent", "healthy"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("severity/name/ID order: got %v, want %v", ids, want)
	}
	for _, r := range []workerRow{healthy, absent, plain} {
		if got := workerSeverity(r, now); got != 3 {
			t.Fatalf("%s promoted to severity %d", r.w.ID, got)
		}
	}
	for _, r := range []workerRow{heldOne, heldMany} {
		if got := workerSeverity(r, now); got != 1 {
			t.Fatalf("%s decision severity %d", r.w.ID, got)
		}
	}
	requireWorkerText(t, stripANSI(m.workersSummary(200)), "4 need attention", "2 holding")
	for _, count := range []*int{nil, custodyCount(0), custodyCount(2)} {
		blocked.w.CustodyDecisionsNeeded = count
		m.workers.rows = []workerRow{blocked}
		row := stripANSI(m.workerRowLine(blocked, false, 200, m.workerTableWidths(m.workers.rows, 200), now))
		requireWorkerText(t, row, "✕ outbox blocked")
		if count != nil && *count > 0 {
			requireWorkerText(t, row, "+1", "⚑ holding")
		} else if strings.Contains(row, "+1") || strings.Contains(row, "holding") {
			t.Fatalf("old boolean added an independent warning: %q", row)
		}
		requireWorkerText(t, stripANSI(m.workersSummary(200)), "1 need attention")
		if got := workerSeverity(blocked, now); got != 0 {
			t.Fatalf("outbox severity changed: %d", got)
		}
	}
}

func TestWorkerCustodyDecisionPriorityRendering(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, status string
		count        *int
		draining     bool
		active       int
		state        string
	}{
		{"offline positive", "offline", custodyCount(2), true, 1, "offline"},
		{"holding before draining", "online", custodyCount(1), true, 1, "holding"},
		{"holding before cordoned", "online", custodyCount(2), true, 0, "holding"},
		{"draining zero", "online", custodyCount(0), true, 1, "draining"},
		{"draining absent", "online", nil, true, 1, "draining"},
		{"cordoned zero", "online", custodyCount(0), true, 0, "cordoned"},
		{"cordoned absent", "online", nil, true, 0, "cordoned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := workerRenderModel(true)
			m.width = 200
			r := workerRow{w: apitypes.WorkerDTO{
				ID: "render", Name: "render-worker", Status: tc.status, Busy: true, ActiveRuns: tc.active,
				RetainingUnpublishedWork: true, CustodyDecisionsNeeded: tc.count,
				StatsCPUPct: fPtr(25), StatsMemBytes: i64(1 << 30), StatsMemLimitBytes: i64(2 << 30),
				StatsDiskDataBytes: i64(20), StatsDiskDataTotalBytes: i64(100),
			}}
			if tc.draining {
				r.w.DrainingSince = &now
			}
			m.workers.rows = []workerRow{r}
			row := stripANSI(m.workerRowLine(r, false, 200, m.workerTableWidths(m.workers.rows, 200), now))
			requireWorkerText(t, row, workerStateGlyph(tc.state)+" "+tc.state)
			if tc.status == "offline" {
				requireWorkerText(t, row, "~25%", "~1.0/2G", "~data")
			}
			detail := stripANSI(m.View().Content)
			requireWorkerText(t, detail, "worker › render-worker  "+workerStateGlyph(tc.state)+" "+tc.state)
			if tc.count != nil && *tc.count > 0 {
				requireWorkerText(t, detail, "unpublished work needs an owner decision")
			} else if strings.Contains(detail, "unpublished work") {
				t.Fatalf("drain became custody attention: %s", detail)
			}
		})
	}
}
