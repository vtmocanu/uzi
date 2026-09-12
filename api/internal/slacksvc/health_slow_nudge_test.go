package slacksvc

// PRD #1189 M4 — the near-timeout (`slow`) health nudge gains a viewer-local deadline
// line and, unless the extension cap is exhausted or disabled, the exact `uzi run
// extend` command. These tests pin: the <!date^…^{time}|…> form with a UTC fallback;
// the checkpoint clause omitted when checkpoint_tip_at is null; the command omitted at
// cap and when the cap is 0; the fallback string staying the head sentence alone; and —
// the safety property this milestone is judged on — every OTHER flag's blocks staying
// byte-identical to before.

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// slowNow is a fixed clock so the rendered durations are deterministic.
var slowNow = time.Date(2026, 9, 12, 16, 15, 0, 0, time.UTC)

// slowDeadline is 1h05m after slowNow, i.e. 17:20 UTC — the PRD's worked example.
func slowDeadline() *time.Time {
	d := slowNow.Add(time.Hour + 5*time.Minute)
	return &d
}

// slowRow is a running issue run flagged `slow`, with a fresh (9m old) checkpoint and no
// extension used yet. Callers override the fields a given case exercises.
func slowRow() store.GetSlackRunContextRow {
	return store.GetSlackRunContextRow{
		ID:                     uuid.New(),
		Kind:                   "issue",
		Status:                 "running",
		Health:                 healthSlow,
		BudgetExtensionSeconds: 0,
		CheckpointTipAt:        pgtype.Timestamptz{Time: slowNow.Add(-9 * time.Minute), Valid: true},
	}
}

func TestHealthNudgeBlocksSlowNamesTheDeadlineInViewerLocalTime(t *testing.T) {
	rc := slowRow()
	dl := slowDeadline()
	blocks, fallback := healthNudgeBlocks(healthSlow, "", "https://uzi.example", rc, dl, 57600, slowNow)
	_, section := blockSummary(blocks)

	// Slack's <!date^<unix>^{time}|<fallback>> form: the viewer's own locale renders it,
	// and the fallback is the UTC time (D9: no stored time zone).
	wantDate := fmt.Sprintf("<!date^%d^{time}|17:20 UTC>", dl.Unix())
	if !strings.Contains(section, wantDate) {
		t.Fatalf("slow section = %q, want it to carry the viewer-local deadline token %q", section, wantDate)
	}
	if !strings.Contains(section, "Stops at "+wantDate+" (1h 05m left).") {
		t.Fatalf("slow section = %q, want the deadline line with the padded time-left %q", section, "(1h 05m left)")
	}
	if !strings.Contains(section, "Last checkpoint pushed 9m ago.") {
		t.Fatalf("slow section = %q, want the checkpoint-age clause", section)
	}
	// The fallback (a client that can't render blocks) stays the head sentence alone — no
	// deadline token, no command.
	if fallback != healthNudgeHead(healthSlow, "") {
		t.Fatalf("slow fallback = %q, want the head sentence alone", fallback)
	}
	if strings.Contains(fallback, "Stops at") || strings.Contains(fallback, "extend") {
		t.Fatalf("slow fallback = %q, want no deadline/command markup in the OS-notification line", fallback)
	}
}

func TestHealthNudgeBlocksSlowOmitsCheckpointClauseWhenTipAtNull(t *testing.T) {
	rc := slowRow()
	rc.CheckpointTipAt = pgtype.Timestamptz{} // null: run never published a checkpoint
	blocks, _ := healthNudgeBlocks(healthSlow, "", "https://uzi.example", rc, slowDeadline(), 57600, slowNow)
	_, section := blockSummary(blocks)

	if strings.Contains(section, "Last checkpoint") {
		t.Fatalf("slow section = %q, want the checkpoint clause omitted when checkpoint_tip_at is null", section)
	}
	// The deadline line itself must still be there.
	if !strings.Contains(section, "Stops at ") {
		t.Fatalf("slow section = %q, want the deadline line even with no checkpoint", section)
	}
}

func TestHealthNudgeBlocksSlowOmitsDeadlineLineWhenNoWallDeadline(t *testing.T) {
	// A run with no wall deadline never gets the slow flag, but the nil guard must still
	// degrade to the plain head/reason section rather than render a bogus line.
	rc := slowRow()
	blocks, _ := healthNudgeBlocks(healthSlow, "", "https://uzi.example", rc, nil, 57600, slowNow)
	_, section := blockSummary(blocks)
	if strings.Contains(section, "Stops at") {
		t.Fatalf("slow section = %q, want no deadline line when the deadline is nil", section)
	}
}

func TestHealthNudgeBlocksSlowShowsExtendCommandWhenCapHasRoom(t *testing.T) {
	rc := slowRow()
	blocks, _ := healthNudgeBlocks(healthSlow, "", "https://uzi.example", rc, slowDeadline(), 57600, slowNow)
	ctx := contextText(blocks)

	want := "Extend: `uzi run extend " + rc.ID.String() + " --by 2h`"
	if !strings.Contains(ctx, want) {
		t.Fatalf("slow context = %q, want the extend command %q", ctx, want)
	}
	// The command is a SECOND context line, after the 🔗 deep link, each its own block.
	if got := countContextBlocks(blocks); got != 2 {
		t.Fatalf("slow blocks carry %d context blocks, want 2 (🔗 link + extend command)", got)
	}
	if !strings.Contains(ctx, "🔗") {
		t.Fatalf("slow context = %q, want the deep link still present alongside the command", ctx)
	}
}

func TestHealthNudgeBlocksSlowOmitsExtendCommandAtCap(t *testing.T) {
	// extension_seconds >= cap: no room left, so the DM never suggests a command that
	// would 409. The boundary (equal) is the omit case.
	rc := slowRow()
	rc.BudgetExtensionSeconds = 7200
	blocks, _ := healthNudgeBlocks(healthSlow, "", "https://uzi.example", rc, slowDeadline(), 7200, slowNow)
	if strings.Contains(contextText(blocks), "uzi run extend") {
		t.Fatalf("slow context carries the extend command though extension (7200) >= cap (7200)")
	}
	if got := countContextBlocks(blocks); got != 1 {
		t.Fatalf("at cap, slow blocks carry %d context blocks, want 1 (🔗 link only)", got)
	}
}

func TestHealthNudgeBlocksSlowOmitsExtendCommandWhenCapDisabled(t *testing.T) {
	// cap == 0 turns extending off everywhere; the DM must not offer it.
	rc := slowRow()
	blocks, _ := healthNudgeBlocks(healthSlow, "", "https://uzi.example", rc, slowDeadline(), 0, slowNow)
	if strings.Contains(contextText(blocks), "uzi run extend") {
		t.Fatalf("slow context carries the extend command though the cap is 0 (disabled)")
	}
	// But the deadline line is still rendered — the deadline is independent of the cap.
	_, section := blockSummary(blocks)
	if !strings.Contains(section, "Stops at ") {
		t.Fatalf("slow section = %q, want the deadline line even with extending disabled", section)
	}
}

// TestHealthNudgeBlocksOtherFlagsAreByteIdenticalToBefore is the milestone's safety pin:
// the two near-timeout extras live entirely inside the `health == healthSlow` guard, so
// feeding slow-path data (a deadline, a checkpoint, a non-zero cap) to any OTHER flag
// must change NOTHING. Comparing the rich-input render against the empty-input render for
// each non-slow flag proves that byte-for-byte (reflect.DeepEqual over the Block Kit),
// and also that no non-slow flag ever grows an extra block.
func TestHealthNudgeBlocksOtherFlagsAreByteIdenticalToBefore(t *testing.T) {
	rc := slowRow()
	others := map[string]string{
		healthStalled:       "",
		healthLooping:       reasonPersistFailing,
		healthWaitingWorker: reasonVerdictUndelivered,
		healthApprovalIdle:  "",
		"":                  "", // unknown/default arm
	}
	for h, reason := range others {
		// Same rc.ID both times so the deep link is identical; only the slow-path inputs
		// differ between the two renders.
		rich, richFb := healthNudgeBlocks(h, reason, "https://uzi.example", rc, slowDeadline(), 57600, slowNow)
		bare, bareFb := healthNudgeBlocks(h, reason, "https://uzi.example", rc, nil, 0, slowNow)
		if !reflect.DeepEqual(rich, bare) {
			t.Errorf("health %q: slow-path inputs changed the blocks (rich=%v bare=%v)", h, rich, bare)
		}
		if richFb != bareFb {
			t.Errorf("health %q: slow-path inputs changed the fallback (%q vs %q)", h, richFb, bareFb)
		}
		if got := countContextBlocks(rich); got != 1 {
			t.Errorf("health %q: %d context blocks, want 1 (🔗 link only) — no flag but slow gets the extend line", h, got)
		}
		if _, section := blockSummary(rich); strings.Contains(section, "Stops at") {
			t.Errorf("health %q: section carries a deadline line reserved for the slow flag: %q", h, section)
		}
	}
}

func TestNearTimeoutLeftFormatsPerThePRDLiteral(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{time.Hour + 5*time.Minute, "1h 05m"},
		{2*time.Hour + 13*time.Minute, "2h 13m"},
		{9 * time.Minute, "9m"},
		{0, "0m"},
		{-time.Minute, "0m"}, // defensive clamp at/past the deadline
	}
	for _, c := range cases {
		if got := nearTimeoutLeft(c.d); got != c.want {
			t.Errorf("nearTimeoutLeft(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

// TestNotifierSlowNudgeWiresDeadlineAndExtendEndToEnd drives the whole handleHealth path
// with the two collaborators wired via NewNotifier's options (WithRunTimeout,
// WithExtensionCap): the DM's section carries the run's deadline (RunDeadline computed
// from n.runTimeout + the rc budget fields) and the context carries the exact extend
// command (the cap reader reports room). It is the caller-side counterpart to the
// healthNudgeBlocks unit tests above, and it is what keeps the options reachable.
func TestNotifierSlowNudgeWiresDeadlineAndExtendEndToEnd(t *testing.T) {
	rc := baseRun("running")
	rc.Kind = "issue"
	rc.Health = healthSlow
	// Started 6h55m ago with an 8h budget → the deadline is ~1h05m out, well inside the
	// slow window. Real-clock arithmetic runs in notifier_health.go, so the assertions
	// below check structure, not the to-the-minute label (the unit tests pin that).
	rc.StartedAt = pgtype.Timestamptz{Time: time.Now().Add(-(6*time.Hour + 55*time.Minute)), Valid: true}
	rc.BudgetWallSeconds = pgtype.Int4{Int32: int32((8 * time.Hour).Seconds()), Valid: true}
	rc.CheckpointTipAt = pgtype.Timestamptz{Time: time.Now().Add(-9 * time.Minute), Valid: true}

	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil,
		WithRunTimeout(2*time.Hour),
		WithExtensionCap(func(context.Context) (int, error) { return 57600, nil }))
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: healthSlow, reason: "", nudge: true})

	if len(fp.blocks) != 1 {
		t.Fatalf("want one threaded nudge post: %+v", fp.blocks)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "Stops at <!date^") {
		t.Fatalf("nudge section missing the viewer-local deadline: %q", fp.blocks[0].sectionText)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "Last checkpoint pushed") {
		t.Fatalf("nudge section missing the checkpoint-age clause: %q", fp.blocks[0].sectionText)
	}
	wantCmd := "uzi run extend " + rc.ID.String() + " --by 2h"
	if !strings.Contains(fp.blocks[0].contextText, wantCmd) {
		t.Fatalf("nudge context missing the extend command %q: %q", wantCmd, fp.blocks[0].contextText)
	}
}

// TestNotifierSlowNudgeOmitsExtendWhenCapReaderReportsDisabled proves the caller honors a
// disabled cap (0) reader: the deadline still renders, the command does not.
func TestNotifierSlowNudgeOmitsExtendWhenCapReaderReportsDisabled(t *testing.T) {
	rc := baseRun("running")
	rc.Kind = "issue"
	rc.Health = healthSlow
	rc.StartedAt = pgtype.Timestamptz{Time: time.Now().Add(-(6*time.Hour + 55*time.Minute)), Valid: true}
	rc.BudgetWallSeconds = pgtype.Int4{Int32: int32((8 * time.Hour).Seconds()), Valid: true}

	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil,
		WithRunTimeout(2*time.Hour),
		WithExtensionCap(func(context.Context) (int, error) { return 0, nil }))
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: healthSlow, reason: "", nudge: true})

	if len(fp.blocks) != 1 {
		t.Fatalf("want one threaded nudge post: %+v", fp.blocks)
	}
	if strings.Contains(fp.blocks[0].contextText, "uzi run extend") {
		t.Fatalf("nudge offered the extend command though the cap reader reports disabled: %q", fp.blocks[0].contextText)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "Stops at <!date^") {
		t.Fatalf("nudge section missing the deadline (independent of the cap): %q", fp.blocks[0].sectionText)
	}
}

// countContextBlocks returns how many Block Kit context blocks a message carries — the
// 🔗 deep link and the extend command are each their own context line.
func countContextBlocks(blocks []slack.Block) int {
	n := 0
	for _, b := range blocks {
		if _, ok := b.(*slack.ContextBlock); ok {
			n++
		}
	}
	return n
}
