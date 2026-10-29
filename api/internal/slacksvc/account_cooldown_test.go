package slacksvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestAccountCooldownNotifications(t *testing.T) {
	for _, initial := range []string{"ok", "stalled", "unknown"} {
		for _, stateCooldown := range []bool{false, true} {
			t.Run(initial+map[bool]string{false: "/health", true: "/state"}[stateCooldown], func(t *testing.T) {
				rc := healthRun("running", initial)
				fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
				fp := &fakePoster{dmChannel: "D1"}
				n := NewNotifier(fs, fp, fixedBase, nil)
				ctx := context.Background()
				if initial == "unknown" {
					fs.msgErr = nil
					fs.msg = store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}
				} else {
					// The real state path creates the initial healthy or faulted root.
					n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
					if len(fp.blocks) != 1 {
						t.Fatalf("initial root: %+v", fp.blocks)
					}
					fs.msgErr = nil
					fs.msg = store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}
				}
				posts := len(fp.blocks)
				fs.rc.Health = "waiting_account_cooldown"
				if stateCooldown {
					n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
				} else {
					n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
				}
				want := 0
				if initial != "ok" {
					want = 1
				}
				if len(fp.updateBlocks) != want || len(fp.blocks) != posts {
					t.Fatalf("cooldown: roots=%+v posts=%+v", fp.updateBlocks, fp.blocks)
				}
				if want == 1 && strings.Contains(fp.updateBlocks[0].contextText, "stalled") {
					t.Fatal("fault warning survived")
				}
				fs.rc.Health = "ok"
				n.handleHealth(ctx, healthEvent{runID: rc.ID, health: "ok"})
				if len(fp.updateBlocks) != want {
					t.Fatal("cooldown exit rewrote identical root")
				}
				fs.rc.Health = "waiting_account_cooldown"
				n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
				fs.rc.Health = "stalled"
				n.handleHealth(ctx, healthEvent{runID: rc.ID, health: "stalled", reason: "independent fault", nudge: true})
				if len(fp.updateBlocks) != want+1 || len(fp.blocks) != posts+1 ||
					!strings.Contains(fp.updateBlocks[want].contextText, "stalled") || fp.blocks[posts].thread != "ts1" {
					t.Fatalf("direct fault: roots=%+v posts=%+v", fp.updateBlocks, fp.blocks)
				}
			})
		}
	}
}

func TestAccountCooldownRootRetries(t *testing.T) {
	ctx := context.Background()
	rc := healthRun("running", "stalled")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
	fp := &fakePoster{dmChannel: "D1", postErr: errors.New("temporary Slack failure")}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
	fp.postErr = nil
	n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
	if len(fp.blocks) != 2 {
		t.Fatal("failed initial post was not retried")
	}
	fs.msgErr = nil
	fs.msg = store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts2"}
	fs.rc.Health = "waiting_account_cooldown"
	fp.postErr = errors.New("temporary update failure")
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	fp.postErr = nil
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	if len(fp.updateBlocks) != 2 {
		t.Fatalf("retry then dedup: %+v", fp.updateBlocks)
	}
	// A different Slack root identity cannot reuse the successful render.
	fs.msg.RootTs = "replacement"
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	fs.msg.ChannelID = "D2"
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	if len(fp.updateBlocks) != 4 {
		t.Fatal("different Slack identity was suppressed")
	}
	// Terminal events release the cache; an unknown root must be refreshed.
	fs.rc.Status = "completed"
	n.handle(ctx, stateEvent{runID: rc.ID, status: "completed"})
	before := len(fp.updateBlocks)
	fs.rc.Status = "running"
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	if len(fp.updateBlocks) != before+1 {
		t.Fatal("terminal render remained cached")
	}
}

func TestAccountCooldownColdTerminalRootCleanup(t *testing.T) {
	for _, healthPath := range []bool{false, true} {
		t.Run(map[bool]string{false: "state", true: "health"}[healthPath], func(t *testing.T) {
			rc := healthRun("completed", "ok")
			fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
			fp := &fakePoster{dmChannel: "D1"}
			n := NewNotifier(fs, fp, fixedBase, nil)
			if healthPath {
				n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "ok"})
			} else {
				n.handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})
			}
			if len(fp.blocks) == 0 {
				t.Fatal("terminal root was not rendered")
			}
			if len(n.rootRenders) != 0 {
				t.Fatal("first terminal render retained root cache")
			}
		})
	}
}

func TestAccountCooldownHealthRootThenState(t *testing.T) {
	ctx := context.Background()
	rc := healthRun("running", "ok")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: "ok"})
	if len(fp.blocks) != 1 {
		t.Fatal("health path did not create root")
	}
	fs.msgErr = nil
	fs.msg = store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}
	fs.rc.Health = "waiting_account_cooldown"
	n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
	fs.rc.Health = "ok"
	n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
	if len(fp.updateBlocks) != 0 {
		t.Fatal("state path rewrote identical cooldown root")
	}
	// Noncooldown repeats keep their existing update behavior.
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: "ok"})
	n.handle(ctx, stateEvent{runID: rc.ID, status: "running"})
	if len(fp.updateBlocks) != 2 {
		t.Fatal("normal root updates were suppressed")
	}
	// Health reason changes that do not change a cooldown render stay quiet.
	fs.rc.Health = "waiting_account_cooldown"
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	n.handleHealth(ctx, healthEvent{runID: rc.ID, health: fs.rc.Health})
	if len(fp.updateBlocks) != 2 {
		t.Fatal("identical cooldown refresh wrote root")
	}
}
