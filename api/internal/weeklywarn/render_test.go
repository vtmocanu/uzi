package weeklywarn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/slacksvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/usagereserve"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

type fakeBlocks map[uuid.UUID]*apitypes.WeeklyLimitDTO

func (f fakeBlocks) ForRuns(_ context.Context, _ uuid.UUID, runs []store.Run) map[uuid.UUID]*apitypes.WeeklyLimitDTO {
	out := map[uuid.UUID]*apitypes.WeeklyLimitDTO{}
	for _, r := range runs {
		if d, ok := f[r.ID]; ok {
			out[r.ID] = d
		}
	}
	return out
}

func renderWarner(runs []store.Run, blocks Blocks, runsErr error) *Warner {
	return renderWarnerTotal(runs, len(runs), blocks, runsErr)
}

// renderWarnerTotal is renderWarner with a claim-predicate total larger than the listed runs.
func renderWarnerTotal(newest []store.Run, total int, blocks Blocks, runsErr error) *Warner {
	return New(Config{Pool: &countingPool{}, Queries: store.New(nil), Publisher: noPublisher{}, Blocks: blocks,
		Runs: func(context.Context, uuid.UUID, uuid.UUID, string) (AffectedRuns, error) {
			return AffectedRuns{Total: total, Newest: newest}, runsErr
		}})
}

func nearAt(pct int, limit int) usagereserve.NearVerdict {
	reset := at(100)
	return usagereserve.NearVerdict{State: usagereserve.NearNear, Pct: pct, EffectiveLimit: limit, ResetsAt: &reset}
}

func TestRenderListsTotalAndAtMostFiveRuns(t *testing.T) {
	owner := uuid.New()
	secret := uuid.New()
	key := "anthropic:" + secret.String()
	var runs []store.Run
	blocks := fakeBlocks{}
	for i := 0; i < 5; i++ {
		r := store.Run{ID: uuid.New(), UserID: owner, Status: "running", Harness: "claude"}
		runs = append(runs, r)
		blocks[r.ID] = &apitypes.WeeklyLimitDTO{CredentialKey: key, CredentialLabel: "subscription", Reason: workersvc.NextClaimNoAlternative,
			Fix: []apitypes.WeeklyLimitFixDTO{{Kind: "set_token", Command: fmt.Sprintf("uzi run set-token %s -- 'other'", r.ID)}}}
	}

	// The source reports the claim's total (8) and its five newest runs.
	w := renderWarnerTotal(runs, 8, blocks, nil)
	c := &credential{userID: owner, key: key, harness: "claude", label: "fallback", fence: func(*notifysvc.SlackRender) {}}
	n := w.build(context.Background(), c, nearAt(92, 100))
	body := n.Slack.Body
	if !strings.Contains(body, "8 runs use this credential") || !strings.Contains(body, "and 3 more") {
		t.Fatalf("total / truncation missing:\n%s", body)
	}
	if got := strings.Count(body, "\n• "); got != 5 {
		t.Fatalf("run lines = %d, want 5:\n%s", got, body)
	}
	if !strings.Contains(body, "**subscription**") || !strings.Contains(body, "92% of its 100% weekly limit") ||
		!strings.Contains(body, "no other token to switch to") || !strings.Contains(body, "`uzi run set-token ") {
		t.Fatalf("body incomplete:\n%s", body)
	}
	if n.Kind != notifysvc.KindWeeklyLimitWarning || n.Slack.Title != "WEEKLY LIMIT NEARLY REACHED" {
		t.Fatalf("kind/title = %q / %q", n.Kind, n.Slack.Title)
	}
	if p, ok := n.Payload.(Payload); !ok || p.Runs != 8 || p.CredentialKey != key {
		t.Fatalf("payload = %#v", n.Payload)
	}
}

func TestRenderEscapesOwnerTextAndDropsUnsafeCommands(t *testing.T) {
	owner, secret := uuid.New(), uuid.New()
	key := "anthropic:" + secret.String()
	hostile := "<!channel> *bold* `tick` & <https://evil.example|click>"
	safe := store.Run{ID: uuid.New(), Status: "running", Harness: "claude"}
	unsafe := store.Run{ID: uuid.New(), Status: "running", Harness: "claude"}
	blocks := fakeBlocks{
		safe.ID: {CredentialKey: key, CredentialLabel: hostile, Reason: workersvc.NextClaimCanSwitch,
			Fix: []apitypes.WeeklyLimitFixDTO{{Kind: "set_token", Command: "uzi run set-token x -- '<!here> & y'"}}},
		unsafe.ID: {CredentialKey: key, CredentialLabel: hostile, Reason: workersvc.NextClaimPinned,
			Fix: []apitypes.WeeklyLimitFixDTO{{Kind: "set_token", TargetLabel: "evil", CommandUnavailable: apitypes.WeeklyFixCommandLabel}}},
	}
	w := renderWarner([]store.Run{safe, unsafe}, blocks, nil)
	c := &credential{userID: owner, key: key, harness: "claude", fence: func(*notifysvc.SlackRender) {}}
	n := w.build(context.Background(), c, nearAt(99, 100))
	// The notifier renders the body with slacksvc.SlackMrkdwnBlock; assert what Slack would receive.
	body, _ := slacksvc.SlackMrkdwnBlock(n.Slack.Body)
	for _, raw := range []string{"<!channel>", "<!here>", "<https://evil.example|click>"} {
		if strings.Contains(body, raw) {
			t.Fatalf("unescaped owner text %q in:\n%s", raw, body)
		}
	}
	if !strings.Contains(body, "&lt;!channel&gt;") || !strings.Contains(body, "&lt;!here&gt; &amp; y") {
		t.Fatalf("owner text not escaped:\n%s", body)
	}
	if strings.Contains(body, "*bold*") || strings.Contains(body, "`tick`") || strings.Contains(body, "_bold_") {
		t.Fatalf("label formatting not neutralised:\n%s", body)
	}
	if !strings.Contains(body, "&lt;!channel&gt; bold tick &amp; https://evil.example|click") {
		t.Fatalf("label not rendered as plain escaped text:\n%s", body)
	}
	if !strings.Contains(body, "rename the token in Settings to get a copyable command") {
		t.Fatalf("an unsafe label must say no command is available:\n%s", body)
	}
	if strings.Count(body, "uzi run set-token") != 1 {
		t.Fatalf("exactly the safe command is rendered:\n%s", body)
	}
}

func TestRenderFencesPerVendorAndAtLimitTitle(t *testing.T) {
	owner := uuid.New()
	fx := notifysvc.CodexAccountFence{OwnerID: owner, AccountID: uuid.New(), Generation: 2, CredentialRevision: 3, EnablementSig: "s"}
	w := renderWarner(nil, nil, nil)
	c := &credential{userID: owner, key: "codex:" + fx.AccountID.String(), harness: "codex", label: "Codex account",
		fence: func(r *notifysvc.SlackRender) {
			f := fx
			r.DeliveryTag, r.CodexAccount = notifysvc.DeliveryTagCodexWeeklyWarning, &f
		}}
	near := nearAt(100, 100)
	near.State = usagereserve.NearAt
	n := w.build(context.Background(), c, near)
	if n.Slack.DeliveryTag != notifysvc.DeliveryTagCodexWeeklyWarning || n.Slack.CodexAccount == nil || *n.Slack.CodexAccount != fx || n.Slack.Credential != nil {
		t.Fatalf("Codex fence = %+v", n.Slack)
	}
	if n.Slack.Title != "WEEKLY LIMIT REACHED" || !strings.Contains(n.Slack.Body, "has reached 100%") || !strings.Contains(n.Slack.Body, "Codex account") {
		t.Fatalf("at-limit render = %+v", n.Slack)
	}
	if strings.Contains(n.Slack.Body, "run uses") || strings.Contains(n.Slack.Body, "runs use") {
		t.Fatalf("no run lines expected without blocks:\n%s", n.Slack.Body)
	}

	sec := notifysvc.CredentialFence{SecretID: uuid.New(), Kind: store.KindAnthropicToken, EnablementRev: 4}
	c2 := &credential{userID: owner, key: "anthropic:" + sec.SecretID.String(), harness: "claude", label: "tok",
		fence: func(r *notifysvc.SlackRender) { f := sec; r.Credential = &f }}
	n2 := w.build(context.Background(), c2, nearAt(90, 100))
	if n2.Slack.Credential == nil || *n2.Slack.Credential != sec || n2.Slack.DeliveryTag != "" || n2.Slack.CodexAccount != nil {
		t.Fatalf("Anthropic fence = %+v", n2.Slack)
	}
}

func TestRenderSurvivesARunListingFailure(t *testing.T) {
	w := renderWarner(nil, fakeBlocks{}, errors.New("db down"))
	c := &credential{userID: uuid.New(), key: "anthropic:x", harness: "claude", label: "tok", fence: func(*notifysvc.SlackRender) {}}
	n := w.build(context.Background(), c, nearAt(90, 100))
	if !strings.Contains(n.Slack.Body, "**tok** is at 90% of its 100% weekly limit.") {
		t.Fatalf("body = %q", n.Slack.Body)
	}
	if len(n.Slack.Facts) != 1 || !strings.Contains(n.Slack.Facts[0], "<!date^") || !strings.Contains(n.Slack.Facts[0], at(100).UTC().Format("2006-01-02 15:04 MST")) {
		t.Fatalf("reset time missing: %q", n.Slack.Facts)
	}
	if strings.Contains(n.Slack.Body, "<!date^") {
		t.Fatalf("the date token must not be in the markdown-walked body: %q", n.Slack.Body)
	}
}

// The reset time is a Fact (trusted markup, never markdown-walked), so the date token reaches Slack
// intact, while a label with an ampersand is escaped exactly once by the real body renderer.
func TestRenderThroughSlackBlockKeepsDateTokenAndEscapesOnce(t *testing.T) {
	d := dm{Label: "R&D", Pct: 92, Limit: 100, ResetsAt: ptrT(at(100)), Total: 1,
		Runs: []runLine{{ID: uuid.New(), Reason: workersvc.NextClaimNoAlternative, Command: "uzi run set-token x -- 'a&b'"}}}
	r := renderSlack(d)
	body, _ := slacksvc.SlackMrkdwnBlock(r.Body)
	if !strings.HasPrefix(body, "*R&amp;D* is at 92%") || strings.Contains(body, "&amp;amp;") {
		t.Fatalf("label not escaped exactly once:\n%s", body)
	}
	if !strings.Contains(body, "`uzi run set-token x -- 'a&amp;b'`") {
		t.Fatalf("command not escaped by the block renderer:\n%s", body)
	}
	want := fmt.Sprintf("Resets <!date^%d^{date_short_pretty} {time}|%s>", at(100).Unix(), at(100).UTC().Format("2006-01-02 15:04 MST"))
	if len(r.Facts) != 1 || r.Facts[0] != want {
		t.Fatalf("facts = %q, want %q", r.Facts, want)
	}
	if strings.Contains(body, "<!date") || strings.Contains(body, "&lt;!date") {
		t.Fatalf("date token leaked into the body:\n%s", body)
	}
}

// The wait statement comes only from the block's Wait field; an unknown reason says the DM cannot tell
// whether the run can switch and never claims the run waits.
func TestRenderWaitStatementComesOnlyFromWaitField(t *testing.T) {
	owner, secret := uuid.New(), uuid.New()
	key := "anthropic:" + secret.String()
	mk := func(reason, wait string) string {
		r := store.Run{ID: uuid.New(), Status: "running", Harness: "claude", IssueTitle: "t"}
		blocks := fakeBlocks{r.ID: {CredentialKey: key, Reason: reason, Wait: wait}}
		c := &credential{userID: owner, key: key, harness: "claude", label: "tok", fence: func(*notifysvc.SlackRender) {}}
		return renderWarner([]store.Run{r}, blocks, nil).build(context.Background(), c, nearAt(92, 100)).Slack.Body
	}
	body := mk(workersvc.NextClaimUnknown, workersvc.WaitFails)
	if strings.Contains(body, "wait for the reset") || !strings.Contains(body, "can't tell whether it can switch") || !strings.Contains(body, "will fail at the limit") {
		t.Fatalf("unknown reason + fails:\n%s", body)
	}
	if body := mk(workersvc.NextClaimNoAlternative, workersvc.WaitWaits); !strings.Contains(body, "no other token to switch to; will wait for the reset") {
		t.Fatalf("waits:\n%s", body)
	}
	if body := mk(workersvc.NextClaimNoAlternative, workersvc.WaitReservePause); !strings.Contains(body, "will pause at the reserve") || strings.Contains(body, "wait for the reset") {
		t.Fatalf("reserve_pause:\n%s", body)
	}
	if body := mk(workersvc.NextClaimNoAlternative, workersvc.WaitUnknown); strings.Contains(body, "wait for the reset") || strings.Contains(body, "will fail") {
		t.Fatalf("unknown wait states nothing:\n%s", body)
	}
}

// A counted run the block builder dropped (or put on another credential) is still listed by id and title.
func TestRenderListsARunWithoutABlock(t *testing.T) {
	owner, secret := uuid.New(), uuid.New()
	key := "anthropic:" + secret.String()
	bare := store.Run{ID: uuid.New(), Status: "running", Harness: "claude", IssueTitle: "Fix *the* login"}
	foreign := store.Run{ID: uuid.New(), Status: "running", Harness: "claude", IssueTitle: "other"}
	blocks := fakeBlocks{foreign.ID: {CredentialKey: "anthropic:" + uuid.NewString(), CredentialLabel: "someone else", Reason: workersvc.NextClaimPinned}}
	c := &credential{userID: owner, key: key, harness: "claude", label: "tok", fence: func(*notifysvc.SlackRender) {}}
	body := renderWarner([]store.Run{bare, foreign}, blocks, nil).build(context.Background(), c, nearAt(92, 100)).Slack.Body
	if !strings.Contains(body, "2 runs use this credential") || !strings.Contains(body, "`"+bare.ID.String()[:8]+"` Fix the login") ||
		!strings.Contains(body, "`"+foreign.ID.String()[:8]+"` other") {
		t.Fatalf("runs without a block not listed:\n%s", body)
	}
	if strings.Contains(body, "someone else") || strings.Contains(body, "pinned") {
		t.Fatalf("another credential's block leaked:\n%s", body)
	}
}

func TestRenderLabelCannotFormLinksOrFakeLines(t *testing.T) {
	for _, label := range []string{"[click](https://evil.example)", "a\n• `deadbeef` fake line", "x\\", "[[]]", " \t "} {
		r := renderSlack(dm{Label: label, Pct: 91, Limit: 100})
		body, _ := slacksvc.SlackMrkdwnBlock(r.Body)
		if strings.Contains(body, "[") || strings.Contains(body, "](") || strings.Contains(body, "<http") || strings.Contains(body, "\n") {
			t.Fatalf("label %q rendered as markup:\n%s", label, body)
		}
	}
	if r := renderSlack(dm{Label: "[]", Pct: 91, Limit: 100}); !strings.HasPrefix(r.Body, "A credential is at") {
		t.Fatalf("an empty stripped label falls back: %q", r.Body)
	}
}
