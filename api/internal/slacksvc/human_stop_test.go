package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type humanStopCase struct {
	name, status          string
	stamp                 pgtype.Text
	reason                string
	landing               bool
	head, label, fallback string
	thread                bool
}

func TestHumanStopNewBehavior(t *testing.T) {
	for _, tc := range []humanStopCase{
		{"cancel-no-reason", "failed", txt("cancelled"), "", false, "🚫 *Cancelled*", "Cancelled", "Cancelled · grp/repo#42", true},
		{"cancel-genuine-reason", "failed", txt("cancelled"), "agent crashed", true, "🚫 *Cancelled*", "Cancelled", "Cancelled · grp/repo#42", true},
		{"reject", "failed", txt("plan_rejected"), "", false, "🚫 *Stopped (plan rejected)*", "Stopped (plan rejected)", "Stopped (plan rejected) · grp/repo#42", true},
		{"reject-needs-landing", "failed", txt("plan_rejected"), "agent crashed", true, "🚫 *Stopped (plan rejected)*", "Stopped (plan rejected)", "Stopped (plan rejected) · grp/repo#42", true},
		{"sentinel-alone", "failed", pgtype.Text{}, "run cancelled", false, "❌ *Failed*run cancelled", "Failed", "Failed · grp/repo#42 — run cancelled", true},
		{"auto-sentinel", "failed", txt("auto_stopped"), "run cancelled", false, "❌ *Failed*run cancelled", "Failed", "Failed · grp/repo#42 — run cancelled", true},
	} {
		t.Run(tc.name, func(t *testing.T) { assertHumanStopSurfaces(t, tc) })
	}
}

func TestHumanStopRegressionControls(t *testing.T) {
	for _, tc := range []humanStopCase{
		{"raw-cancel-conflict", "cancelled", txt("plan_rejected"), "agent crashed", true, "🚫 *Cancelled*", "Cancelled", "Cancelled · grp/repo#42", true},
		{"raw-cancel-auto", "cancelled", txt("auto_stopped"), "", false, "🚫 *Cancelled*", "Cancelled", "Cancelled · grp/repo#42", true},
		{"raw-cancel-null", "cancelled", pgtype.Text{}, "", false, "🚫 *Cancelled*", "Cancelled", "Cancelled · grp/repo#42", true},
		{"auto", "failed", txt("auto_stopped"), "agent crashed", false, "❌ *Failed*agent crashed", "Failed", "Failed · grp/repo#42 — agent crashed", true},
		{"null", "failed", pgtype.Text{}, "", false, "❌ *Failed*", "Failed", "Failed · grp/repo#42", true},
		{"unknown", "failed", txt("unknown"), "", false, "❌ *Failed*", "Failed", "Failed · grp/repo#42", true},
		{"invalid-cancel", "failed", pgtype.Text{String: "cancelled"}, "", false, "❌ *Failed*", "Failed", "Failed · grp/repo#42", true},
		{"invalid-reject", "failed", pgtype.Text{String: "plan_rejected"}, "", false, "❌ *Failed*", "Failed", "Failed · grp/repo#42", true},
		{"genuine", "failed", pgtype.Text{}, "bad <@U123> & crash", false, "❌ *Failed*bad &lt;@U123&gt; &amp; crash", "Failed", "Failed · grp/repo#42 — bad &lt;@U123&gt; &amp; crash", true},
		{"genuine-needs-landing", "failed", pgtype.Text{}, "agent crashed", true, "❌ *Failed — needs landing*agent crashed", "Failed — needs landing", "Failed (needs landing) · grp/repo#42 — agent crashed", true},
		{"running-cancel", "running", txt("cancelled"), "run cancelled", false, "", "Running", "", false},
		{"queued-reject", "queued", txt("plan_rejected"), "", false, "", "Queued", "", false},
		{"approval-reject", "awaiting_approval", txt("plan_rejected"), "", false, "", "Needs your approval", "", false},
		{"completed-cancel", "completed", txt("cancelled"), "run cancelled", false, "✅ *Completed*", "Completed", "Completed · grp/repo#42", true},
		{"completed-reject", "completed", txt("plan_rejected"), "", false, "✅ *Completed*", "Completed", "Completed · grp/repo#42", true},
	} {
		t.Run(tc.name, func(t *testing.T) { assertHumanStopSurfaces(t, tc) })
	}
}

func assertHumanStopSurfaces(t *testing.T, tc humanStopCase) {
	t.Helper()
	rc := baseRun(tc.status)
	rc.StopKind, rc.FailureReason = tc.stamp, txt(tc.reason)
	if tc.landing {
		rc.FailOrigin = txt("workflow_scope_missing")
		rc.HasPreservedPatch = true
	}
	blocks, fallback, ok := renderThreadBlocks(rc, "https://uzi.example")
	_, section := blockSummary(blocks)
	if ok != tc.thread || section != tc.head || fallback != tc.fallback {
		t.Errorf("thread: ok=%v section=%q fallback=%q; want %v %q %q", ok, section, fallback, tc.thread, tc.head, tc.fallback)
	}
	if tc.thread && (len(blocks) < 2 || !strings.Contains(contextText(blocks), "/runs/"+rc.ID.String())) {
		t.Errorf("thread deep link missing: %q", contextText(blocks))
	}
	root, rootFallback := rootBlocks(rc, "https://uzi.example")
	_, rootSection := blockSummary(root)
	wantRootFallback := tc.label + " · grp/repo#42 — Add the feature"
	if !strings.Contains(rootSection, "*"+tc.label+"*\n`grp/repo#42` · *Add the feature*") || rootFallback != wantRootFallback {
		t.Errorf("root: section=%q fallback=%q; want label %q fallback %q", rootSection, rootFallback, tc.label, wantRootFallback)
	}
	for _, first := range []bool{true, false} {
		fs := &fakeNotifStore{rc: rc, delivery: txt("U123"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "root"}}
		if first {
			fs.msgErr = pgx.ErrNoRows
		}
		fp := &fakePoster{}
		n := NewNotifier(fs, fp, fixedBase, nil)
		n.handle(context.Background(), stateEvent{runID: rc.ID, status: rc.Status})
		if first {
			if len(fp.blocks) != 1 || fp.blocks[0].thread != "" || fp.blocks[0].sectionText != rootSection || fp.blocks[0].fallback != wantRootFallback {
				t.Errorf("first DM root: %+v", fp.blocks)
			}
		} else {
			updated, found := findUpdateBlock(fp.updateBlocks, "root")
			if !found || updated.sectionText != rootSection || updated.fallback != wantRootFallback {
				t.Errorf("DM root update: %+v", fp.updateBlocks)
			}
			if tc.thread {
				if len(fp.blocks) != 1 || fp.blocks[0].thread != "root" || fp.blocks[0].sectionText != tc.head || fp.blocks[0].fallback != tc.fallback {
					t.Errorf("DM thread: %+v", fp.blocks)
				}
			} else if len(fp.blocks) != 0 {
				t.Errorf("unexpected DM thread: %+v", fp.blocks)
			}
		}
	}
}
