package slacksvc

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func providerHoldRun(t *testing.T) store.GetSlackRunContextRow {
	t.Helper()
	rc := milestoneRun(t, 1, 2, "")
	rc.Kind = "issue"
	rc.Status = "paused"
	rc.HoldReason = txt("provider_unavailable")
	rc.StatusSince = pgtype.Timestamptz{Time: time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC), Valid: true}
	return rc
}

func TestProviderHoldAnnouncementCycles(t *testing.T) {
	for _, firstRoot := range []bool{true, false} {
		name := "existing_thread"
		if firstRoot {
			name = "first_root"
		}
		t.Run(name, func(t *testing.T) {
			rc := providerHoldRun(t)
			fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts0"}}
			if firstRoot {
				fs.msgErr = pgx.ErrNoRows
			}
			fp := &fakePoster{}
			n := NewNotifier(fs, fp, fixedBase, nil)
			// Even an old recovery event must render the fetched hold.
			ev := stateEvent{runID: rc.ID, status: "recovery_wait"}
			n.handle(context.Background(), ev)
			if len(fp.blocks) != 1 || !strings.Contains(fp.blocks[0].sectionText, "Needs your attention") {
				t.Fatalf("fetched hold must be announced once: %+v", fp.blocks)
			}
			if len(fs.limitPauseSet) != 1 || fs.msg.ParkKind.String != "paused" || !fs.msg.LimitPausedAt.Time.Equal(rc.StatusSince.Time) {
				t.Fatalf("successful announcement must stamp hold identity: %+v", fs.limitPauseSet)
			}
			if firstRoot {
				fs.msgErr = nil
				fs.msg.ChannelID, fs.msg.RootTs = "D1", "ts1"
			}
			n.handle(context.Background(), ev)
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})
			updates := 3
			if firstRoot {
				updates = 2
			}
			if len(fp.blocks) != 1 || len(fp.updateBlocks) != updates || len(fs.limitPauseSet) != 1 {
				t.Fatalf("duplicate/late events must only update root: posts=%+v updates=%+v stamps=%+v", fp.blocks, fp.updateBlocks, fs.limitPauseSet)
			}
			fs.rc.Status = "running"
			fs.rc.HoldReason = pgtype.Text{}
			n.handle(context.Background(), ev)
			if len(resumeBlocks(fp.blocks)) != 1 || fs.msg.LimitPausedAt.Valid {
				t.Fatalf("resume must consume hold marker: posts=%+v marker=%+v", fp.blocks, fs.msg)
			}
			n.handle(context.Background(), ev)
			if len(resumeBlocks(fp.blocks)) != 1 {
				t.Fatal("repeated resume announced again")
			}
			before := len(fp.blocks)
			fs.rc = rc
			fs.rc.StatusSince.Time = rc.StatusSince.Time.Add(time.Hour)
			n.handle(context.Background(), ev)
			n.handle(context.Background(), ev)
			if len(fp.blocks) != before+1 || !fs.msg.LimitPausedAt.Time.Equal(fs.rc.StatusSince.Time) {
				t.Fatalf("new hold after Resume must announce once: %+v", fp.blocks)
			}
		})
	}
}

func TestProviderHoldPostFailureRetryPreservesMarker(t *testing.T) {
	for _, oldKind := range []string{"", "limit_wait", "paused"} {
		t.Run(oldKind, func(t *testing.T) {
			rc := providerHoldRun(t)
			old := pgtype.Timestamptz{}
			if oldKind != "" {
				old = pgtype.Timestamptz{Time: rc.StatusSince.Time.Add(-time.Hour), Valid: true}
			}
			fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts0", LimitPausedAt: old, ParkKind: txt(oldKind)}}
			fp := &fakePoster{postErr: errors.New("post refused")}
			n := NewNotifier(fs, fp, fixedBase, nil)
			ev := stateEvent{runID: rc.ID, status: "paused"}
			n.handle(context.Background(), ev)
			if len(fs.limitPauseSet) != 0 || fs.msg.LimitPausedAt != old || fs.msg.ParkKind.String != oldKind {
				t.Fatalf("failed post must preserve previous marker: %+v", fs.msg)
			}
			fp.postErr = nil
			n.handle(context.Background(), ev)
			n.handle(context.Background(), ev)
			if len(fp.blocks) != 2 || len(fs.limitPauseSet) != 1 || !fs.msg.LimitPausedAt.Time.Equal(rc.StatusSince.Time) {
				t.Fatalf("failed hold must retry, then dedupe: posts=%+v stamps=%+v", fp.blocks, fs.limitPauseSet)
			}
		})
	}
}

// Nonprovider parks retain the legacy marker even when their thread delivery fails.
func TestNonproviderParkFailedThreadStillRecordsAndResumes(t *testing.T) {
	for _, tc := range []struct {
		name, status, reason string
	}{
		{"owner_pause", "paused", ""},
		{"unknown_hold", "paused", "unknown"},
		{"account_hold", "paused", "codex_account_unavailable"},
		{"usage_limit", "limit_wait", ""},
		{"usage_limit_with_provider_reason", "limit_wait", "provider_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := baseRun(tc.status)
			rc.HoldReason = txt(tc.reason)
			rc.StatusSince = pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true}
			old := pgtype.Timestamptz{Time: rc.StatusSince.Time.Add(-time.Hour), Valid: true}
			fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{
				RunID: rc.ID, ChannelID: "D1", RootTs: "ts0", LimitPausedAt: old, ParkKind: txt("paused"),
			}}
			fp := &fakePoster{postErr: errors.New("post refused")}
			n := NewNotifier(fs, fp, fixedBase, nil)
			for i := 0; i < 2; i++ {
				n.handle(context.Background(), stateEvent{runID: rc.ID, status: tc.status})
				if len(fs.limitPauseSet) != i+1 || fs.msg.LimitPausedAt != rc.StatusSince || fs.msg.ParkKind.String != tc.status {
					t.Fatalf("nonprovider failed thread must record current park on each event: writes=%+v marker=%+v", fs.limitPauseSet, fs.msg)
				}
			}
			if len(fp.blocks) != 2 || fp.blocks[0].thread != "ts0" {
				t.Fatalf("expected failed thread delivery attempts: %+v", fp.blocks)
			}
			fp.postErr = nil
			fs.rc.Status = "running"
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})
			rb := resumeBlocks(fp.blocks)
			if len(rb) != 1 || fs.msg.LimitPausedAt.Valid {
				t.Fatalf("legacy park must resume after failed thread delivery: posts=%+v marker=%+v", fp.blocks, fs.msg)
			}
			if strings.Contains(rb[0].sectionText, "usage limit cleared") != (tc.status == "limit_wait") {
				t.Fatalf("resume must reflect recorded park kind: %s", rb[0].sectionText)
			}
		})
	}
}

func TestNonproviderFirstRootAnchorFailureStillRecords(t *testing.T) {
	for _, status := range []string{"paused", "limit_wait"} {
		t.Run(status, func(t *testing.T) {
			rc := baseRun(status)
			rc.StatusSince = pgtype.Timestamptz{Time: time.Now().Add(-30 * time.Minute), Valid: true}
			fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
			fp := &fakePoster{}
			n := NewNotifier(&providerAnchorFailureStore{fs}, fp, fixedBase, nil)
			n.handle(context.Background(), stateEvent{runID: rc.ID, status: status})
			if len(fp.blocks) != 1 || fp.blocks[0].thread != "" {
				t.Fatalf("expected successful first root: %+v", fp.blocks)
			}
			if len(fs.limitPauseSet) != 1 || fs.msg.LimitPausedAt != rc.StatusSince || fs.msg.ParkKind.String != status {
				t.Fatalf("nonprovider first root must attempt legacy marker despite anchor failure: writes=%+v marker=%+v", fs.limitPauseSet, fs.msg)
			}
		})
	}
}

func TestProviderHoldIdentityIncludesParkKind(t *testing.T) {
	rc := providerHoldRun(t)
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{
		RunID: rc.ID, ChannelID: "D1", RootTs: "ts0", LimitPausedAt: rc.StatusSince, ParkKind: txt("limit_wait"),
	}}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
	if len(fp.blocks) != 1 || len(fs.limitPauseSet) != 1 || fs.msg.ParkKind.String != "paused" {
		t.Fatalf("equal timestamp on another park kind must not suppress hold: posts=%+v marker=%+v", fp.blocks, fs.msg)
	}
}

type providerAnchorFailureStore struct{ *fakeNotifStore }

func (f *providerAnchorFailureStore) UpsertSlackRunMessage(context.Context, store.UpsertSlackRunMessageParams) (store.SlackRunMessage, error) {
	return store.SlackRunMessage{}, errors.New("anchor write refused")
}

func TestProviderHoldFirstRootFailuresDoNotStamp(t *testing.T) {
	for _, anchorFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "post", true: "anchor"}[anchorFailure], func(t *testing.T) {
			rc := providerHoldRun(t)
			fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
			fp := &fakePoster{}
			if anchorFailure {
				n := NewNotifier(&providerAnchorFailureStore{fs}, fp, fixedBase, nil)
				n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
			} else {
				fp.postErr = errors.New("post refused")
				n := NewNotifier(fs, fp, fixedBase, nil)
				n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
				if len(fs.upserted) != 0 {
					t.Fatal("failed root wrote anchor")
				}
				fp.postErr = nil
				n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
				if len(fs.limitPauseSet) != 1 {
					t.Fatal("successful root retry must stamp")
				}
				return
			}
			if len(fs.limitPauseSet) != 0 {
				t.Fatal("failed anchor must not stamp announcement")
			}
		})
	}
}

func TestProviderRecoveryProjectionAndNoThreadPerPark(t *testing.T) {
	rc := baseRun("recovery_wait")
	rc.RecoveryWaitCause = txt("provider_outage")
	rc.ProviderParkCount = 3
	rc.ProviderParkStreakStartedAt = pgtype.Timestamptz{Time: time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC), Valid: true}
	rc.RecoveryRetryNotBefore = pgtype.Timestamptz{Time: time.Date(2026, 10, 12, 11, 0, 0, 0, time.UTC), Valid: true}
	rc.RetryNotBefore = pgtype.Timestamptz{Time: time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC), Valid: true}
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts0"}}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	for i := 0; i < 3; i++ {
		n.handle(context.Background(), stateEvent{runID: rc.ID, status: "paused"})
		fs.rc.ProviderParkCount++
	}
	if len(fp.blocks) != 0 || len(fs.limitPauseSet) != 0 || len(fp.updateBlocks) != 3 {
		t.Fatalf("auto retries must only update root: %+v", fp)
	}
	root := fp.updateBlocks[0]
	for _, want := range []string{"provider outage", "provider parks: 3", "streak started 2026-10-11T10:00:00Z", "next retry 2026-10-12T11:00:00Z"} {
		if !strings.Contains(root.sectionText+root.contextText, want) {
			t.Fatalf("missing projected %q: %+v", want, root)
		}
	}
	if strings.Contains(root.contextText, "2026-10-13") || strings.Contains(root.contextText, "hold") {
		t.Fatalf("wrong retry field or future hold promise: %+v", root)
	}
	rc.RecoveryWaitCause = pgtype.Text{}
	blocks, fallback := rootBlocks(rc, "")
	if strings.Contains(fallback+contextText(blocks), "provider") {
		t.Fatal("missing typed cause inferred from counters")
	}
	rc.RecoveryWaitCause = txt("<@U1>")
	blocks, fallback = rootBlocks(rc, "")
	if strings.Contains(fallback+contextText(blocks), "provider") || strings.Contains(contextText(blocks), "<@U1>") {
		t.Fatal("unknown cause must stay generic")
	}
	rc.RecoveryWaitCause = txt("provider_outage")
	rc.ProviderParkCount = 0
	rc.ProviderParkStreakStartedAt, rc.RecoveryRetryNotBefore = pgtype.Timestamptz{}, pgtype.Timestamptz{}
	blocks, fallback = rootBlocks(rc, "")
	if !strings.Contains(fallback, "provider outage") || strings.Contains(contextText(blocks), "next retry") || strings.Contains(contextText(blocks), "streak started") || strings.Contains(contextText(blocks), "provider parks") {
		t.Fatal("absent projection must not fabricate detail")
	}
}

func TestProviderHoldTypedCopyAndStopEligibility(t *testing.T) {
	for _, name := range []string{"eligible", "no_completed", "no_milestones", "task", "interactive", "missing_reason", "unknown_reason"} {
		t.Run(name, func(t *testing.T) {
			rc := providerHoldRun(t)
			switch name {
			case "no_completed":
				rc.MilestonesCompleted = []byte(`["stale"]`)
			case "no_milestones":
				rc.MilestonesFrozen = nil
			case "task":
				rc.Kind = "task"
			case "interactive":
				rc.Interactive = true
			case "missing_reason":
				rc.HoldReason = pgtype.Text{}
			case "unknown_reason":
				rc.HoldReason = txt("<@U1>")
			}
			blocks, fallback := rootBlocks(rc, "")
			thread, tfallback, ok := renderThreadBlocks(rc, "")
			if !ok {
				t.Fatal("hold must render thread")
			}
			copy := fallback + contextText(blocks) + tfallback + contextText(thread)
			if name == "missing_reason" || name == "unknown_reason" {
				if strings.Contains(copy, "provider") || strings.Contains(copy, "<@U1>") {
					t.Fatal("untyped hold inferred provider cause or leaked reason")
				}
				return
			}
			_, rootSection := blockSummary(blocks)
			_, threadSection := blockSummary(thread)
			if strings.Contains(rootSection, "‖ *Paused*") || strings.Contains(threadSection, "‖ *Paused*") {
				t.Fatalf("provider hold must not use the ordinary pause header: root=%q thread=%q", rootSection, threadSection)
			}
			if strings.HasPrefix(fallback, "Paused · ") || strings.HasPrefix(tfallback, "Paused · ") {
				t.Fatalf("provider hold must not use the ordinary pause fallback: root=%q thread=%q", fallback, tfallback)
			}
			for _, want := range []string{"Needs your attention", "provider unavailable", "Resume", "Cancel", "worker or a verified recovery capture"} {
				if !strings.Contains(copy, want) {
					t.Fatalf("missing %q: %s", want, copy)
				}
			}
			if name == "eligible" {
				if !strings.Contains(copy, "scope ceiling") || !strings.Contains(copy, "then Resume") || strings.Contains(copy, "Stop unavailable") {
					t.Fatalf("eligible Stop must describe two steps: %s", copy)
				}
			} else if !strings.Contains(copy, "Stop unavailable") {
				t.Fatalf("unavailable Stop needs reason: %s", copy)
			}
			if name == "no_completed" && !strings.Contains(copy, "no completed milestones") {
				t.Fatal("missing completed-work reason")
			}
			if strings.Contains(copy, "Completed ·") || strings.Contains(copy, "Paused by you") {
				t.Fatalf("provider hold must not imply completion or owner pause: %s", copy)
			}
			if (name == "task") == strings.Contains(copy, "issue claim remains held") {
				t.Fatalf("issue claim wording must be issue-only: %s", copy)
			}
		})
	}
}
