package slacksvc

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
)

func weeklyBlock(runID uuid.UUID, command string) *apitypes.WeeklyLimitDTO {
	return &apitypes.WeeklyLimitDTO{
		Reason: "pinned", CredentialLabel: "work", Tier: apitypes.WeeklyLimitTierWarning,
		Fix: []apitypes.WeeklyLimitFixDTO{{Kind: "set_token", TargetID: uuid.NewString(), Command: command}},
	}
}

func TestSevenDayParkAppendsReasonAndCommand(t *testing.T) {
	rc := parkedCtx("seven_day", 2*time.Hour, 1)
	cmd := "uzi run set-token " + rc.ID.String() + " -- spare"
	wl := weeklyBlock(rc.ID, cmd)
	blocks, fallback, ok := renderThreadBlocks(rc, "https://uzi.example", wl)
	if !ok {
		t.Fatal("park must thread")
	}
	ctx := contextText(blocks)
	for _, want := range []string{"this run is pinned to work", cmd} {
		if !strings.Contains(ctx, want) || !strings.Contains(fallback, want) {
			t.Errorf("want %q in both the block and the fallback:\n block: %s\n fallback: %s", want, ctx, fallback)
		}
	}
	// Without a block the same park is exactly the old rendering.
	plainBlocks, plainFallback, _ := renderThreadBlocks(rc, "https://uzi.example", nil)
	if strings.Contains(contextText(plainBlocks), "set-token") || strings.Contains(plainFallback, "set-token") {
		t.Fatal("a park without a block grew a fix")
	}
	if !strings.HasPrefix(fallback, plainFallback) {
		t.Fatalf("fallback %q does not extend the plain %q", fallback, plainFallback)
	}
}

func TestOnlyWeeklyParksAskForABlock(t *testing.T) {
	calls := 0
	n := &Notifier{}
	WithWeeklyLimit(func(context.Context, uuid.UUID) *apitypes.WeeklyLimitDTO { calls++; return nil })(n)
	for _, rc := range []string{"five_hour", "seven_day_opus", "seven_day_sonnet", ""} {
		if got := n.weeklyLimitFor(context.Background(), parkedCtx(rc, time.Hour, 1)); got != nil {
			t.Errorf("%q: block %+v", rc, got)
		}
	}
	if got := n.weeklyLimitFor(context.Background(), baseRun("paused")); got != nil {
		t.Errorf("ordinary pause: block %+v", got)
	}
	if calls != 0 {
		t.Fatalf("the source was consulted %d times for parks that never carry a block", calls)
	}
	n.weeklyLimitFor(context.Background(), parkedCtx("seven_day", time.Hour, 1))
	reserve := baseRun("paused")
	reserve.HoldReason = txt("weekly_reserve")
	n.weeklyLimitFor(context.Background(), reserve)
	if calls != 2 {
		t.Fatalf("source calls = %d; want one per seven_day park and per reserve pause", calls)
	}
	if (&Notifier{}).weeklyLimitFor(context.Background(), parkedCtx("seven_day", time.Hour, 1)) != nil {
		t.Fatal("an unwired notifier produced a block")
	}
}

func TestFiveHourParkIsByteIdenticalWithOrWithoutTheSource(t *testing.T) {
	rc := parkedCtx("five_hour", time.Hour, 2)
	n := &Notifier{}
	WithWeeklyLimit(func(context.Context, uuid.UUID) *apitypes.WeeklyLimitDTO {
		t.Fatal("five_hour park asked for a weekly block")
		return nil
	})(n)
	b1, f1, _ := renderThreadBlocks(rc, "https://uzi.example", n.weeklyLimitFor(context.Background(), rc))
	b2, f2, _ := renderThreadBlocks(rc, "https://uzi.example", nil)
	if f1 != f2 || contextText(b1) != contextText(b2) {
		t.Fatalf("five_hour output changed: %q vs %q", f1, f2)
	}
}

func TestWeeklyReservePauseAddsReasonAndAnotherFixNotTheResumeTwice(t *testing.T) {
	rc := providerHoldRun(t)
	rc.HoldReason = txt("weekly_reserve")
	resume := "uzi run resume " + rc.ID.String()
	set := "uzi run set-token " + rc.ID.String() + " -- spare"
	wl := &apitypes.WeeklyLimitDTO{Reason: "can_switch", AlternativeLabel: "spare", Fix: []apitypes.WeeklyLimitFixDTO{
		{Kind: "resume_reserve", Command: resume}, {Kind: "edit_reserve"}, {Kind: "set_token", Command: set},
	}}
	blocks, fallback, ok := renderThreadBlocks(rc, "", wl)
	if !ok {
		t.Fatal("reserve park must thread")
	}
	all := contextText(blocks) + fallback
	if !strings.Contains(all, "this run can move to spare") || !strings.Contains(all, set) {
		t.Fatalf("missing reason or fix: %s", all)
	}
	if got := strings.Count(contextText(blocks), resume); got != 1 {
		t.Fatalf("resume command shown %d times in the block; want once", got)
	}
}

func TestHostileLabelIsEscapedInSlackBlocksAndFallback(t *testing.T) {
	rc := parkedCtx("seven_day", time.Hour, 1)
	// Assembled at runtime; a label may carry mrkdwn control characters and shell syntax.
	hostile := "<!channel>" + "&" + "$" + "(id)" + "`x`"
	cmd := "uzi run set-token " + rc.ID.String() + " -- '" + hostile + "'"
	wl := weeklyBlock(rc.ID, cmd)
	wl.AlternativeLabel = hostile
	wl.Reason = "headroom_outside_pool"
	blocks, fallback, _ := renderThreadBlocks(rc, "", wl)
	for name, text := range map[string]string{"block": contextText(blocks), "fallback": fallback} {
		if strings.Contains(text, "<!channel>") {
			t.Errorf("%s carries a live broadcast mention: %s", name, text)
		}
		if !strings.Contains(text, "&lt;!channel&gt;&amp;") {
			t.Errorf("%s did not escape the label: %s", name, text)
		}
	}
}

// A server-withheld command (the label is not command-safe) leaves the park line with a rename hint,
// in the block and the fallback, on both the limit_wait park and the reserve pause; a shown command
// carries no hint.
func TestWithheldCommandAddsARenameHint(t *testing.T) {
	const hint = "rename the token in Settings to get a copyable command"
	withheld := func() *apitypes.WeeklyLimitDTO {
		return &apitypes.WeeklyLimitDTO{Reason: "pinned", CredentialLabel: "work", Fix: []apitypes.WeeklyLimitFixDTO{
			{Kind: "set_token", TargetLabel: "odd", CommandUnavailable: apitypes.WeeklyFixCommandLabel},
		}}
	}
	rc := parkedCtx("seven_day", 2*time.Hour, 1)
	blocks, fallback, ok := renderThreadBlocks(rc, "", withheld())
	if !ok {
		t.Fatal("park must thread")
	}
	if !strings.Contains(contextText(blocks), hint) || !strings.Contains(fallback, hint) || strings.Contains(fallback, "set-token") {
		t.Fatalf("limit_wait park:\n block: %s\n fallback: %s", contextText(blocks), fallback)
	}

	reserve := providerHoldRun(t)
	reserve.HoldReason = txt("weekly_reserve")
	resume := "uzi run resume " + reserve.ID.String()
	wl := withheld()
	wl.Fix = append([]apitypes.WeeklyLimitFixDTO{{Kind: "resume_reserve", Command: resume}}, wl.Fix...)
	blocks, fallback, ok = renderThreadBlocks(reserve, "", wl)
	if !ok {
		t.Fatal("reserve park must thread")
	}
	if !strings.Contains(contextText(blocks), hint) || !strings.Contains(fallback, hint) {
		t.Fatalf("reserve pause (resume shown elsewhere):\n block: %s\n fallback: %s", contextText(blocks), fallback)
	}

	// A command that is shown makes the hint redundant.
	cmd := "uzi run set-token " + rc.ID.String() + " -- spare"
	wl = withheld()
	wl.Fix = append(wl.Fix, apitypes.WeeklyLimitFixDTO{Kind: "set_token", Command: cmd})
	blocks, fallback, _ = renderThreadBlocks(rc, "", wl)
	if strings.Contains(contextText(blocks), hint) || strings.Contains(fallback, hint) || !strings.Contains(fallback, cmd) {
		t.Fatalf("a shown command must suppress the hint:\n block: %s\n fallback: %s", contextText(blocks), fallback)
	}
}

func TestCommandIsInACodeSpanSoAMarkedUpLabelStaysLiteral(t *testing.T) {
	rc := parkedCtx("seven_day", 2*time.Hour, 1)
	cmd := "uzi run set-token " + rc.ID.String() + " -- _prod_"
	blocks, fallback, ok := renderThreadBlocks(rc, "", weeklyBlock(rc.ID, cmd))
	if !ok {
		t.Fatal("park must thread")
	}
	want := "`" + cmd + "`"
	if !strings.Contains(contextText(blocks), want) || !strings.Contains(fallback, want) {
		t.Fatalf("command not in a code span:\n block: %s\n fallback: %s", contextText(blocks), fallback)
	}
}
