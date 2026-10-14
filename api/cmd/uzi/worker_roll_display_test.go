package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
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
