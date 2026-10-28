package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestAccountCooldownNotifications(t *testing.T) {
	rc := healthRun("running", "stalled")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	broadcaster := workersvc.MultiBroadcaster{n}
	// Only durable health/reason transitions reach the notifier. The detector test
	// separately proves identical polls and lease/policy refreshes publish nothing.
	for i, transition := range []struct {
		health, reason string
		nudge          bool
	}{
		{"waiting_account_cooldown", apitypes.AccountCooldownReason, false},
		{"ok", "", false},
		{"stalled", "the agent stopped sending updates", true},
		{"stalled", "an outcome is waiting for delivery", false},
	} {
		fs.rc.Health = transition.health
		broadcaster.PublishHealth(rc.ID, transition.health, transition.reason, transition.nudge)
		select {
		case ev := <-n.healthCh:
			n.handleHealth(context.Background(), ev)
		default:
			t.Fatal("meaningful transition was dropped")
		}
		wantPosts := 0
		if i >= 2 {
			wantPosts = 1
		}
		if len(fp.updateBlocks) != i+1 || len(fp.blocks) != wantPosts {
			t.Fatalf("%s: roots=%d posts=%d, want %d/%d", transition.health, len(fp.updateBlocks), len(fp.blocks), i+1, wantPosts)
		}
		if i <= 1 && strings.Contains(fp.updateBlocks[i].contextText, "stalled") {
			t.Fatalf("prior fault warning survived transition: %+v", fp.updateBlocks[i])
		}
		if i >= 2 && !strings.Contains(fp.updateBlocks[i].contextText, "stalled") {
			t.Fatal("later fault warning missing")
		}
		if len(n.healthCh) != 0 {
			t.Fatal("unexpected duplicate health event")
		}
	}
}
