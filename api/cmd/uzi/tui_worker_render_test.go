package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func workerRenderModel(dark bool) tuiModel {
	m := workersScene(dark, "workers-factory")
	m.view = viewWorker
	m.width, m.height = 120, 200
	m.workerDetail = workerDetailState{workerID: "render", admin: m.board.admin}
	m.workers.rows = []workerRow{{w: apitypes.WorkerDTO{ID: "render", Name: "render-worker", Status: "online", Kind: "hosted", AnthropicBindMode: "default"}}}
	return m
}

func requireWorkerText(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestWorkerRenderSectionsAndSources(t *testing.T) {
	for _, dark := range []bool{true, false} {
		m := workerRenderModel(dark)
		w := &m.workers.rows[0].w
		now := time.Now()
		cpu := 12.5
		w.StatsCPUPct, w.StatsSource = &cpu, sp("process")
		w.StatsMemBytes, w.StatsMemLimitBytes = i64(1024), i64(2048)
		w.StatsDiskDataBytes, w.StatsDiskDataTotalBytes = i64(4096), i64(8192)
		w.StatsDiskDataInodes, w.StatsDiskDataTotalInodes = i64(3), i64(7)
		w.StatsDiskDindBytes, w.StatsDiskDindTotalBytes = i64(1024), i64(4096)
		w.StatsDiskDindInodes, w.StatsDiskDindTotalInodes = i64(5), i64(9)
		w.RunDisk = []apitypes.WorkerRunDiskDTO{{RunID: "disk-run", HomeBytes: 8192, CacheBytes: 2048, Truncated: true, SampledAt: now.Add(-2 * time.Minute)}}
		w.ReportedRuns = []apitypes.WorkerReportedRunDTO{
			{RunID: "cached", Phase: "awaiting_input", ClaimGeneration: 17, TerminalPending: true},
			{RunID: "uncached", Phase: "future-phase", ClaimGeneration: 19},
		}
		w.Version, w.UpgradeTarget = sp("v1"), "v2"
		w.UpgradeStatus = "outdated"
		w.Capabilities = []string{"docker", "jvm"}
		w.TemplateDeclared, w.TemplateReported = sp("declared"), sp("reported")
		w.AnthropicBindMode, w.AnthropicSecretLabel = "pinned", sp("work-token")
		w.Ephemeral = true
		lease := now.Add(time.Hour)
		w.EphemeralLeaseExpiresAt = &lease
		m.workers.rows[0].workerOwner = "owner@example.test"
		m.board.runs = []apitypes.RunListItemDTO{{RunDTO: apitypes.RunDTO{ID: "cached", IssueTitle: "cached title", IssueIID: i64(1), Harness: "claude", Status: "running", IsPlanning: true}}}
		m.board.runsAdmin = m.board.admin
		out := stripANSI(m.View().Content)
		last := -1
		for _, section := range []string{"attention", "reported runs", "resources", "configuration"} {
			at := strings.Index(out, section)
			if at <= last {
				t.Fatalf("section order: %s", out)
			}
			last = at
		}
		requireWorkerText(t, out, "cached title", "worker phase awaiting_input · run stage planning · gen 17", "uncached", "worker phase unknown · run stage ? (not cached) · gen 19", "outcome pending",
			"cpu", "12%", "process only", "memory", "1.0 KiB / 2.0 KiB", "50%", "25%", "inodes 43%", "nix", "?", "dind", "inodes 56%", "display only", "largest HOME", "disk-run  ≥8.0 KiB (sampled 2m ago)", "version", "v1", "target v2", "capabilities", "docker jvm", "template", "declared  ≠ reported reported", "token", "pinned · work-token", "kind", "hosted", "ephemeral · lease", "owner", "owner@example.test")
		if strings.Contains(out, "10.0 KiB") || strings.Contains(out, "future-phase") {
			t.Fatal("invented total or raw enum")
		}
		// A token label is not evidence of effective pinning (including ephemeral workers).
		w.AnthropicBindMode = "auto"
		out = stripANSI(m.renderWorker())
		requireWorkerText(t, out, "token         auto")
		if strings.Contains(out, "work-token") {
			t.Fatal("inferred token binding")
		}
		w.AnthropicBindMode = "default"
		requireWorkerText(t, stripANSI(m.renderWorker()), "token         default")
	}
}

func TestWorkerRenderHostileBoundsAndScrolling(t *testing.T) {
	const nasty = "\u202e\x07\x01\r\n\t"
	for _, dark := range []bool{true, false} {
		for _, width := range []int{80, 119, 120} {
			m := workerRenderModel(dark)
			m.width = width
			w := &m.workers.rows[0].w
			w.Status, w.UpgradeStatus = "offline", "upgrade_failed"
			w.Name, w.Version, w.UpgradeTarget = nasty+"name", sp(nasty+"version"), nasty+"target"
			w.UpgradeBlockingContainer, w.UpgradeBlockingReason, w.UpgradeDetail = sp(nasty+"container"), sp(nasty+"reason"), sp(nasty+"detail")
			w.OutboxBlocked = sp(nasty + "outbox")
			w.TemplateDeclared, w.TemplateReported = sp(nasty+"declared"), sp(nasty+"reported")
			w.Capabilities, w.HostedSize = []string{nasty + "cap"}, sp(nasty+"size")
			w.AnthropicBindMode, w.AnthropicSecretLabel = "pinned", sp(nasty+"token")
			m.workers.rows[0].workerOwner = nasty + "owner"
			m.workers.rows[0].pressureText = []string{"data", "nix", "dind"}
			w.DrainingSince, w.Ephemeral, w.EphemeralLeaseExpiresAt = &w.CreatedAt, true, nil
			w.RetainingUnpublishedWork, w.CustodyDecisionsNeeded = true, custodyCount(1)
			for i := 0; i < 12; i++ {
				w.ReportedRuns = append(w.ReportedRuns, apitypes.WorkerReportedRunDTO{RunID: fmt.Sprintf("reported-%02d", i), Phase: "running", TerminalPending: true})
			}
			w.RunDisk = []apitypes.WorkerRunDiskDTO{{RunID: nasty + "disk", SampledAt: time.Now()}}
			m = step(m, tea.ColorProfileMsg{Profile: colorprofile.Ascii})
			out := m.View().Content
			assertNoRawControls(t, "worker", out)
			requireWorkerText(t, stripANSI(out), "name", "reason", "detail", "outbox", "version", "target",
				"declared", "reported", "cap", "size", "token", "owner", "disk", "last-known, stale", "~ ?", "? / ?")
			for _, height := range []int{1, 2, 3, 8, 20} {
				m.height = height
				for _, k := range []string{keyPageDown, keyPageDown, keyPageUp, "j", "j", "k"} {
					next, _ := m.workerKey(k)
					m = next.(tuiModel)
					out := stripANSI(m.View().Content)
					if len(strings.Split(out, "\n")) > height {
						t.Fatalf("height %d overflow:\n%s", height, out)
					}
					for _, line := range strings.Split(out, "\n") {
						if visualWidth(line) > width {
							t.Fatalf("width %d overflow: %q", width, line)
						}
					}
					if height >= 3 && (k == "j" || k == "k") {
						requireWorkerText(t, out, fmt.Sprintf("▌ %d %s", m.workerDetail.cursor+1, clampVisual(m.renderer.Plain(fmt.Sprintf("reported-%02d", m.workerDetail.cursor), width), 8)))
					}
				}
			}
			m.height = 8
			for i := 0; i < 20; i++ {
				next, _ := m.workerKey(keyPageDown)
				m = next.(tuiModel)
			}
			requireWorkerText(t, stripANSI(m.View().Content), "owner", "pgup/pgdn")
			for i := 0; i < 20; i++ {
				next, _ := m.workerKey(keyPageUp)
				m = next.(tuiModel)
			}
			requireWorkerText(t, stripANSI(m.View().Content), "name", "attention")
		}
	}
}

func TestWorkerRenderBoardWorkerCell(t *testing.T) {
	const nasty = "\u202e\x07\x01"
	for _, dark := range []bool{true, false} {
		for _, admin := range []bool{true, false} {
			for _, width := range []int{80, 119, 120} {
				m := workerRenderModel(dark)
				m.view, m.width, m.board.admin = viewBoard, width, admin
				iid := int64(123456789012345)
				r := apitypes.RunListItemDTO{RunDTO: apitypes.RunDTO{ID: "run", Status: "running", IssueIID: &iid, IssueTitle: strings.Repeat("wide界", 80), WorkerName: sp("embedded-wrong"), AnthropicSecretLabel: sp("credential")}, WorkerName: sp(nasty + "outer-worker"), OwnerEmail: sp("owner@example.test")}
				verdict := "pass"
				r.JudgeVerdict = &verdict
				for _, sel := range []bool{true, false} {
					out := m.boardRow(r, sel, boardMarkerCols{fullW: 4, verdictW: 1, countW: 1})
					assertNoRawControls(t, "board worker", out)
					out = stripANSI(out)
					if visualWidth(out) > width {
						t.Fatalf("board overflow: %q", out)
					}
					if strings.Contains(out, "embedded-wrong") {
						t.Fatal("embedded worker name drawn")
					}
					if width >= 120 {
						requireWorkerText(t, out, "outer-worker")
						if visualWidth(out) != 120 || !strings.HasSuffix(out, "outer-worker    ") {
							t.Fatalf("unaligned worker cell: %q", out)
						}
					} else if strings.Contains(out, "outer-worker") {
						t.Fatal("worker cell below 120")
					}
				}
			}
		}
	}
}

func TestWorkerRenderRailNameAndFoldBudget(t *testing.T) {
	decided := false
	for h := 20; h <= 60; h++ {
		m := codexFloorRunAt(t, h, true)
		control := m.effectiveRailFolded(time.Now())
		m.detail.run.WorkerName = sp("\u202e\x07\x01rail-worker")
		out := m.renderLaneRail()
		assertNoRawControls(t, "rail worker", out)
		requireWorkerText(t, stripANSI(out), "worker rail-worker")
		for _, line := range strings.Split(stripANSI(out), "\n") {
			if visualWidth(line) > 26 {
				t.Fatalf("rail overflow %q", line)
			}
		}
		if !control && m.effectiveRailFolded(time.Now()) {
			decided = true
			requireWorkerText(t, stripANSI(m.View().Content), "rail-worker", "alpha", "34%")
		}
	}
	if !decided {
		t.Fatal("worker row never charged to auto-fold budget")
	}
}
