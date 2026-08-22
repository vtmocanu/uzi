package slacksvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type fakeNotifStore struct {
	rc           store.GetSlackRunContextRow
	rcErr        error
	delivery     pgtype.Text
	deliveryErr  error
	msg          store.SlackRunMessage
	msgErr       error
	upserted     []store.UpsertSlackRunMessageParams
	gateSet      []store.SetSlackRunGateParams
	gateSetGen   []store.SetSlackRunGateGenParams
	planCount    int64
	planCountErr error
	// PRD #88 M3: the latest kind='question' payload and the anchor write recording
	// which question the thread already carries. A nil question with no error models
	// "no row" the way the generated :one query surfaces it.
	question    []byte
	questionErr error
	questionSet []store.SetSlackRunQuestionParams
	// PRD #122 M4: the milestone thread-line dedup writes. Each SetSlackRunMilestoneNotified
	// call is captured so a test can assert the advanced count was recorded (or that a
	// deduped/no-milestone report recorded nothing).
	milestoneSet []store.SetSlackRunMilestoneNotifiedParams
	// PRD #191 M2b: the repo-less chat context the notifier falls back to. chatCtxErr
	// defaults to pgx.ErrNoRows via the method below when no row is staged, modelling a
	// non-chat run.
	chatCtx    store.GetSlackChatContextRow
	chatCtxSet bool
	chatCtxErr error
}

func (f *fakeNotifStore) GetSlackRunContext(context.Context, uuid.UUID) (store.GetSlackRunContextRow, error) {
	return f.rc, f.rcErr
}
func (f *fakeNotifStore) GetSlackChatContext(context.Context, uuid.UUID) (store.GetSlackChatContextRow, error) {
	if f.chatCtxErr != nil {
		return store.GetSlackChatContextRow{}, f.chatCtxErr
	}
	if !f.chatCtxSet {
		return store.GetSlackChatContextRow{}, pgx.ErrNoRows // not a chat run
	}
	return f.chatCtx, nil
}
func (f *fakeNotifStore) GetSlackDeliveryForUser(context.Context, uuid.UUID) (pgtype.Text, error) {
	return f.delivery, f.deliveryErr
}
func (f *fakeNotifStore) GetSlackRunMessage(context.Context, uuid.UUID) (store.SlackRunMessage, error) {
	return f.msg, f.msgErr
}
func (f *fakeNotifStore) UpsertSlackRunMessage(_ context.Context, arg store.UpsertSlackRunMessageParams) (store.SlackRunMessage, error) {
	f.upserted = append(f.upserted, arg)
	return store.SlackRunMessage{RunID: arg.RunID, ChannelID: arg.ChannelID, RootTs: arg.RootTs}, nil
}
func (f *fakeNotifStore) SetSlackRunGate(_ context.Context, arg store.SetSlackRunGateParams) (store.SlackRunMessage, error) {
	f.gateSet = append(f.gateSet, arg)
	return store.SlackRunMessage{RunID: arg.RunID, GateTs: arg.GateTs, GateState: arg.GateState}, nil
}
func (f *fakeNotifStore) SetSlackRunGateGen(_ context.Context, arg store.SetSlackRunGateGenParams) (store.SlackRunMessage, error) {
	f.gateSetGen = append(f.gateSetGen, arg)
	return store.SlackRunMessage{RunID: arg.RunID, GateTs: arg.GateTs, GateState: arg.GateState, GateGeneration: arg.GateGeneration}, nil
}
func (f *fakeNotifStore) CountRunPlanMessages(context.Context, uuid.UUID) (int64, error) {
	return f.planCount, f.planCountErr
}
func (f *fakeNotifStore) GetLatestRunQuestion(context.Context, uuid.UUID) ([]byte, error) {
	if f.questionErr != nil {
		return nil, f.questionErr
	}
	if f.question == nil {
		return nil, pgx.ErrNoRows
	}
	return f.question, nil
}
func (f *fakeNotifStore) SetSlackRunQuestion(_ context.Context, arg store.SetSlackRunQuestionParams) (store.SlackRunMessage, error) {
	f.questionSet = append(f.questionSet, arg)
	return store.SlackRunMessage{RunID: arg.RunID, QuestionID: arg.QuestionID}, nil
}
func (f *fakeNotifStore) SetSlackRunMilestoneNotified(_ context.Context, arg store.SetSlackRunMilestoneNotifiedParams) (store.SlackRunMessage, error) {
	f.milestoneSet = append(f.milestoneSet, arg)
	return store.SlackRunMessage{RunID: arg.RunID, MilestonesNotifiedCompleted: arg.Count}, nil
}

type postCall struct{ channel, thread, text string }
type updateCall struct{ channel, ts, text string }
type blockCall struct {
	channel, thread, fallback string
	sectionText               string
	contextText               string
	actionIDs                 []string
}
type updateBlockCall struct {
	channel, ts, fallback string
	sectionText           string
	contextText           string
	actionIDs             []string
}
type ephemeralCall struct{ channel, user, text string }
type reactionCall struct{ channel, ts, emoji string }

type fakePoster struct {
	dmChannel    string
	posts        []postCall
	updates      []updateCall
	blocks       []blockCall
	updateBlocks []updateBlockCall
	ephemerals   []ephemeralCall
	reactions    []reactionCall
	openErr      error
	postErr      error
	tsSeq        int
	// emailToID maps an email to a Slack id for LookupUserByEmail; a miss returns
	// lookupErr (defaulting to a not-found-style error).
	emailToID map[string]string
	lookupErr error
}

// blockSummary flattens a Block Kit slice to (button action ids, concatenated
// section text) so tests can assert what a message carries without deep matching.
func blockSummary(blks []slack.Block) ([]string, string) {
	ids := []string{}
	section := ""
	for _, b := range blks {
		switch bl := b.(type) {
		case *slack.ActionBlock:
			if bl.Elements != nil {
				for _, el := range bl.Elements.ElementSet {
					if btn, ok := el.(*slack.ButtonBlockElement); ok {
						ids = append(ids, btn.ActionID)
					}
				}
			}
		case *slack.SectionBlock:
			if bl.Text != nil {
				section += bl.Text.Text
			}
		}
	}
	return ids, section
}

// findUpdateBlock returns the first UpdateBlocks call recorded at ts. Since PRD #268
// M2 the run-status root is edited via UpdateBlocks, so the root edit shares
// fp.updateBlocks with the gate edits — a test that wants a specific edit locates it
// by ts rather than by position.
func findUpdateBlock(calls []updateBlockCall, ts string) (updateBlockCall, bool) {
	for _, c := range calls {
		if c.ts == ts {
			return c, true
		}
	}
	return updateBlockCall{}, false
}

func (p *fakePoster) OpenDM(context.Context, string) (string, error) {
	if p.dmChannel == "" {
		p.dmChannel = "D1"
	}
	return p.dmChannel, p.openErr
}
func (p *fakePoster) Post(_ context.Context, ch, thread, text string) (string, error) {
	p.posts = append(p.posts, postCall{ch, thread, text})
	p.tsSeq++
	return fmt.Sprintf("ts%d", p.tsSeq), p.postErr
}
func (p *fakePoster) Update(_ context.Context, ch, ts, text string) error {
	p.updates = append(p.updates, updateCall{ch, ts, text})
	return nil
}
func (p *fakePoster) PostBlocks(_ context.Context, ch, thread, fallback string, blks []slack.Block) (string, error) {
	ids, sectionText := blockSummary(blks)
	p.blocks = append(p.blocks, blockCall{channel: ch, thread: thread, fallback: fallback, sectionText: sectionText, contextText: contextText(blks), actionIDs: ids})
	p.tsSeq++
	return fmt.Sprintf("ts%d", p.tsSeq), p.postErr
}
func (p *fakePoster) UpdateBlocks(_ context.Context, ch, ts, fallback string, blks []slack.Block) error {
	ids, sectionText := blockSummary(blks)
	p.updateBlocks = append(p.updateBlocks, updateBlockCall{channel: ch, ts: ts, fallback: fallback, sectionText: sectionText, contextText: contextText(blks), actionIDs: ids})
	return p.postErr
}
func (p *fakePoster) PostEphemeral(_ context.Context, ch, user, text string) error {
	p.ephemerals = append(p.ephemerals, ephemeralCall{channel: ch, user: user, text: text})
	return p.postErr
}
func (p *fakePoster) AddReaction(_ context.Context, ch, ts, emoji string) error {
	p.reactions = append(p.reactions, reactionCall{channel: ch, ts: ts, emoji: emoji})
	return p.postErr
}
func (p *fakePoster) LookupUserByEmail(_ context.Context, email string) (string, error) {
	if id, ok := p.emailToID[email]; ok {
		return id, nil
	}
	if p.lookupErr != nil {
		return "", p.lookupErr
	}
	return "", errors.New("users_not_found")
}

func fixedBase(context.Context) (string, error) { return "https://uzi.example", nil }

func text8(v int64) pgtype.Int8 { return pgtype.Int8{Int64: v, Valid: true} }
func txt(s string) pgtype.Text  { return pgtype.Text{String: s, Valid: true} }

func baseRun(status string) store.GetSlackRunContextRow {
	return store.GetSlackRunContextRow{
		ID: uuid.New(), UserID: uuid.New(), Status: status,
		IssueIid: text8(42), IssueTitle: "Add the feature",
		PathWithNamespace: "grp/repo", WebUrl: "https://gitlab.example/grp/repo",
	}
}

func TestNotifierFirstTransitionPostsRoot(t *testing.T) {
	fs := &fakeNotifStore{rc: baseRun("running"), delivery: txt("U123"), msgErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: fs.rc.ID, status: "running"})

	if len(fp.blocks) != 1 || fp.blocks[0].thread != "" {
		t.Fatalf("want 1 top-level Block Kit post, got %+v", fp.blocks)
	}
	root := fp.blocks[0]
	// The section carries the glyph label + repo#iid + title; the deep link rides the
	// context block, not a trailing text line.
	for _, want := range []string{"Running", "grp/repo#42", "Add the feature"} {
		if !strings.Contains(root.sectionText, want) {
			t.Errorf("root section missing %q in %q", want, root.sectionText)
		}
	}
	for _, want := range []string{"/runs/" + fs.rc.ID.String(), "Open in uzi"} {
		if !strings.Contains(root.contextText, want) {
			t.Errorf("root context missing %q in %q", want, root.contextText)
		}
	}
	if want := "Running · grp/repo#42 — Add the feature"; root.fallback != want {
		t.Errorf("root fallback = %q, want %q", root.fallback, want)
	}
	if len(fs.upserted) != 1 || fs.upserted[0].ChannelID != "D1" || fs.upserted[0].RootTs != "ts1" {
		t.Errorf("anchor not recorded: %+v", fs.upserted)
	}
}

func TestNotifierEditsRootAndThreadsCompleted(t *testing.T) {
	rc := baseRun("completed")
	rc.MrIid = text8(7)
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U123"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})

	root, ok := findUpdateBlock(fp.updateBlocks, "ts1")
	if !ok || !strings.Contains(root.sectionText, "Completed") {
		t.Fatalf("root not edited to Completed: %+v", fp.updateBlocks)
	}
	if !strings.Contains(root.contextText, "MR !7") {
		t.Fatalf("root context missing the MR link: %q", root.contextText)
	}
	// PRD #268 M3: the terminal thread event is now a Block Kit post (family B).
	if len(fp.blocks) != 1 || fp.blocks[0].thread != "ts1" {
		t.Fatalf("want 1 threaded Block Kit event under ts1, got %+v", fp.blocks)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "Completed") {
		t.Errorf("thread event section not Completed: %q", fp.blocks[0].sectionText)
	}
	if !strings.Contains(fp.blocks[0].contextText, "/-/merge_requests/7") {
		t.Errorf("thread event missing MR link: %q", fp.blocks[0].contextText)
	}
}

func TestNotifierForgejoRunSaysPullRequestAndUsesPersistedURL(t *testing.T) {
	// PRD #65 D2/D8: a Forgejo run's DM must read in Forgejo's vocabulary ("PR #7",
	// not "MR !7") and link the worker-persisted mr_web_url directly — the GitLab
	// `/-/merge_requests/` reconstruction is wrong for Forgejo.
	rc := baseRun("completed")
	rc.MrIid = text8(7)
	rc.ForgeType = "forgejo"
	rc.MrWebUrl = txt("https://forge.example/grp/repo/pulls/7")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U123"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})

	root, ok := findUpdateBlock(fp.updateBlocks, "ts1")
	if !ok || !strings.Contains(root.contextText, "PR #7") {
		t.Fatalf("root not edited to Forgejo completed: %+v", fp.updateBlocks)
	}
	if strings.Contains(root.contextText, "MR !7") {
		t.Errorf("Forgejo DM must not use GitLab's MR !N form: %q", root.contextText)
	}
	if len(fp.blocks) != 1 || !strings.Contains(fp.blocks[0].contextText, "/pulls/7") {
		t.Fatalf("thread event must link the persisted mr_web_url: %+v", fp.blocks)
	}
	if strings.Contains(fp.blocks[0].contextText, "/-/merge_requests/") {
		t.Errorf("Forgejo DM must not reconstruct a GitLab MR URL: %q", fp.blocks[0].contextText)
	}
}

// A worker-supplied mr_web_url that is not https must never become a rendered link
// (Go twin of the web's isHttpsUrl guard, PRD #65 D8).
func TestNotifierRejectsNonHTTPSMrWebURL(t *testing.T) {
	rc := baseRun("completed")
	rc.MrIid = text8(7)
	rc.ForgeType = "gitlab"
	rc.MrWebUrl = txt("javascript:alert(1)")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U123"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})

	if len(fp.blocks) != 1 {
		t.Fatalf("want 1 threaded event, got %+v", fp.blocks)
	}
	if strings.Contains(fp.blocks[0].contextText, "javascript:") {
		t.Errorf("a non-https mr_web_url must not be rendered: %q", fp.blocks[0].contextText)
	}
	// It falls back to the GitLab reconstruction (this row's forge is gitlab).
	if !strings.Contains(fp.blocks[0].contextText, "/-/merge_requests/7") {
		t.Errorf("want the GitLab reconstruction fallback: %q", fp.blocks[0].contextText)
	}
}

func TestNotifierDropsUnlinkedOwner(t *testing.T) {
	fs := &fakeNotifStore{rc: baseRun("running"), deliveryErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: fs.rc.ID, status: "running"})
	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("unlinked owner must not receive any Slack call: posts=%v updates=%v", fp.posts, fp.updates)
	}
}

// TestNotifierSuppressesSelfImproveRunState: a self_improve run is repo-ful, so
// GetSlackRunContext returns a row (unlike a repo-less judge run, dropped as
// ErrNoRows) — its OWN state transition must still be suppressed from the run-state
// DM path (PRD #46 Decision 6). The owner is linked, so the only reason nothing posts
// is the kind skip.
func TestNotifierSuppressesSelfImproveRunState(t *testing.T) {
	rc := baseRun("completed")
	rc.Kind = "self_improve"
	fs := &fakeNotifStore{rc: rc, delivery: txt("U123")}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "completed"})
	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("a self_improve run's own state must not post to the run-state DM path: posts=%v updates=%v", fp.posts, fp.updates)
	}
}

func TestNotifierPublishStateNeverBlocks(t *testing.T) {
	// No drain goroutine: the queue fills and further calls must drop, not block.
	n := NewNotifier(&fakeNotifStore{}, &fakePoster{}, fixedBase, nil)
	done := make(chan struct{})
	go func() {
		for i := 0; i < notifierQueue+50; i++ {
			n.PublishState(uuid.New(), "running")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("PublishState blocked when the queue was full")
	}
}

// A forge-controlled issue title (and repo path) must not be able to smuggle a
// clickable spoofed link or a mention into the trusted bot DM: the dynamic fields
// are mrkdwn-escaped, while the real Open-in-uzi deep link stays raw.
func TestNotifierEscapesHostileTitleAndPath(t *testing.T) {
	rc := baseRun("running")
	rc.IssueTitle = "Fix bug <https://phishing.example|Open in uzi> ping <@U123>"
	rc.PathWithNamespace = "grp/<@channel>"
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.blocks) != 1 {
		t.Fatalf("want a Block Kit post, got %+v", fp.blocks)
	}
	// The hostile forge/model fields live in the section; they must appear only escaped.
	body := fp.blocks[0].sectionText
	if strings.Contains(body, "<https://phishing.example|Open in uzi>") {
		t.Errorf("raw spoofed link survived into the DM: %q", body)
	}
	if strings.Contains(body, "<@U123>") || strings.Contains(body, "<@channel>") {
		t.Errorf("raw mention survived into the DM: %q", body)
	}
	if !strings.Contains(body, "&lt;https://phishing.example|Open in uzi&gt;") || !strings.Contains(body, "&lt;@U123&gt;") {
		t.Errorf("hostile fields were not mrkdwn-escaped: %q", body)
	}
	// The genuine deep link (trusted base + uuid) must stay raw and clickable in context.
	if !strings.Contains(fp.blocks[0].contextText, "<https://uzi.example/runs/"+rc.ID.String()+"|Open in uzi>") {
		t.Errorf("legit deep link was broken by over-escaping: %q", fp.blocks[0].contextText)
	}
	// The fallback must never carry a raw hostile field either (it is parsed for mrkdwn).
	if strings.Contains(fp.blocks[0].fallback, "<@U123>") || strings.Contains(fp.blocks[0].fallback, "<https://phishing.example|Open in uzi>") {
		t.Errorf("fallback carried a raw hostile field: %q", fp.blocks[0].fallback)
	}
}

// The worker-originated failure reason is untrusted free text with no source-side
// length bound: the notifier escapes it AND caps its length.
func TestNotifierEscapesAndBoundsFailureReason(t *testing.T) {
	rc := baseRun("failed")
	rc.FailureReason = txt("boom <@U9> " + strings.Repeat("x", 600))
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "failed"})

	if len(fp.blocks) != 1 {
		t.Fatalf("want one threaded failure event, got %+v", fp.blocks)
	}
	// The reason rides its own FULL section (never a context element).
	evt := fp.blocks[0].sectionText
	if !strings.Contains(evt, "Failed") {
		t.Errorf("failure event missing the Failed head: %q", evt)
	}
	if strings.Contains(evt, "<@U9>") {
		t.Errorf("raw mention survived in the failure event: %q", evt)
	}
	if !strings.Contains(evt, "&lt;@U9&gt;") {
		t.Errorf("failure reason was not escaped: %q", evt)
	}
	if !strings.Contains(evt, "…") || strings.Contains(evt, strings.Repeat("x", 501)) {
		t.Errorf("failure reason was not length-bounded: %q", evt)
	}
	// The fallback also carries the escaped, bounded reason — never a raw model field.
	if strings.Contains(fp.blocks[0].fallback, "<@U9>") {
		t.Errorf("fallback carried a raw mention: %q", fp.blocks[0].fallback)
	}
}

// Entering awaiting_approval posts the gate message (Approve / Reject / Open)
// in-thread under the root and records gate_ts/gate_state='open' (PRD #25 M4).
func TestNotifierPostsGateOnAwaitingApproval(t *testing.T) {
	rc := baseRun("awaiting_approval")
	fs := &fakeNotifStore{
		rc:        rc,
		delivery:  txt("U1"),
		msg:       store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
		planCount: 1, // one plan version so far → generation 1
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_approval"})

	// No plan_md on the row → no plan-in-thread post, just the gate.
	if len(fp.blocks) != 1 || fp.blocks[0].thread != "ts1" {
		t.Fatalf("gate must post one Block Kit message in-thread under the root: %+v", fp.blocks)
	}
	ids := fp.blocks[0].actionIDs
	if len(ids) != 4 || ids[0] != ActionGateApprove || ids[1] != ActionGateRequestChanges || ids[2] != ActionGateReject || ids[3] != ActionGateOpen {
		t.Fatalf("gate buttons = %v, want [approve request_changes reject open]", ids)
	}
	if len(fs.gateSetGen) != 1 || fs.gateSetGen[0].GateState.String != gateStateOpen ||
		!fs.gateSetGen[0].GateTs.Valid || fs.gateSetGen[0].GateGeneration.Int32 != 1 {
		t.Fatalf("gate anchor not recorded open at generation 1: %+v", fs.gateSetGen)
	}
}

// A transient plan-count error must NOT fabricate a generation (PRD #41): the old
// storedGen+1 fallback would post the current (possibly pre-revision) plan and record a
// generation that later SWALLOWS the genuine re-gate. Skip this event instead — post
// nothing, advance nothing — and let a subsequent state event re-drive with a working
// count. The run is unaffected (Slack is best-effort; the web gate is canonical).
func TestNotifierSkipsGateOnCountError(t *testing.T) {
	rc := baseRun("awaiting_approval")
	rc.PlanMd = txt("## Plan\n1. do a thing")
	fs := &fakeNotifStore{
		rc:           rc,
		delivery:     txt("U1"),
		msg:          store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}, // gate closed
		planCountErr: fmt.Errorf("db hiccup"),
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_approval"})

	if len(fp.blocks) != 0 {
		t.Fatalf("a count error must post no gate/plan Block Kit message: %+v", fp.blocks)
	}
	if len(fs.gateSetGen) != 0 {
		t.Fatalf("a count error must NOT advance/record a generation (no burn): %+v", fs.gateSetGen)
	}
}

// The gate depends on the worker flushing the `plan` run_message BEFORE it re-reports
// awaiting_approval (§343): a correctly-ordered gate always has currentGen>=1. A
// currentGen of 0 means the plan isn't flushed yet, so the notifier waits (posts no gate)
// rather than posting a gate with no plan — a no-op, not a drop.
func TestNotifierWaitsForPlanFlushWhenCountZero(t *testing.T) {
	rc := baseRun("awaiting_approval")
	fs := &fakeNotifStore{
		rc:        rc,
		delivery:  txt("U1"),
		msg:       store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}, // fresh anchor, gen 0
		planCount: 0,                                                                   // plan run_message not flushed yet → currentGen 0
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_approval"})

	if len(fp.blocks) != 0 {
		t.Fatalf("currentGen==0 must not post a gate (wait for the plan flush): %+v", fp.blocks)
	}
	if len(fs.gateSetGen) != 0 {
		t.Fatalf("currentGen==0 must not record a generation: %+v", fs.gateSetGen)
	}
}

// The plan itself is posted into the thread at the gate (PRD #41 Decision 10 — Slack
// gate parity), keyed to the fresh-gate post, alongside the gate buttons.
func TestNotifierPostsPlanInThreadAtGate(t *testing.T) {
	rc := baseRun("awaiting_approval")
	rc.PlanMd = txt("## Plan\n1. do a thing\n2. do another")
	fs := &fakeNotifStore{
		rc:        rc,
		delivery:  txt("U1"),
		msg:       store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
		planCount: 1,
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_approval"})

	// Two block posts under the root: the plan render + the gate.
	if len(fp.blocks) != 2 {
		t.Fatalf("want a plan-in-thread post AND a gate post: %+v", fp.blocks)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "do a thing") {
		t.Fatalf("plan-in-thread must carry the plan body: %q", fp.blocks[0].sectionText)
	}
	if len(fp.blocks[0].actionIDs) != 0 {
		t.Fatalf("the plan post carries no buttons: %+v", fp.blocks[0].actionIDs)
	}
	if len(fp.blocks[1].actionIDs) == 0 {
		t.Fatalf("the second post must be the gate (with buttons): %+v", fp.blocks[1])
	}
}

// When the run detected a repo roster, the gate offers TWO approve buttons — repo
// and own — before Reject/Open (PRD #37 M7). A run with no roster (the test above)
// keeps the single-approve shape.
func TestNotifierGateOffersRepoAndOwnWhenRosterDetected(t *testing.T) {
	rc := baseRun("awaiting_approval")
	rc.RepoAgentNames = []string{"coder", "reviewer", "tester"}
	fs := &fakeNotifStore{
		rc:        rc,
		delivery:  txt("U1"),
		msg:       store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
		planCount: 1,
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_approval"})

	if len(fp.blocks) != 1 {
		t.Fatalf("gate must post one Block Kit message: %+v", fp.blocks)
	}
	ids := fp.blocks[0].actionIDs
	want := []string{ActionGateApproveRepo, ActionGateApproveOwn, ActionGateRequestChanges, ActionGateReject, ActionGateOpen}
	if len(ids) != len(want) {
		t.Fatalf("gate buttons = %v, want %v", ids, want)
	}
	for i, w := range want {
		if ids[i] != w {
			t.Fatalf("gate button %d = %q, want %q (all: %v)", i, ids[i], w, ids)
		}
	}
	// The names ride the body; descriptions never do (there are none in the row).
	if !strings.Contains(fp.blocks[0].sectionText, "coder") {
		t.Fatalf("gate body must list the repo agent names: %q", fp.blocks[0].sectionText)
	}
}

// A redundant awaiting_approval re-broadcast of the SAME plan generation must not get
// a second gate message or re-post the plan (PRD #41 Decision 10e).
func TestNotifierDoesNotDoublePostGate(t *testing.T) {
	rc := baseRun("awaiting_approval")
	rc.PlanMd = txt("## Plan\ndo the thing")
	fs := &fakeNotifStore{
		rc:        rc,
		delivery:  txt("U1"),
		msg:       store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1", GateTs: txt("gate-ts"), GateState: txt(gateStateOpen), GateGeneration: pgtype.Int4{Int32: 1, Valid: true}},
		planCount: 1, // same generation as the stored anchor → redundant
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_approval"})

	if len(fp.blocks) != 0 {
		t.Fatalf("a same-generation re-broadcast must not re-post the gate or plan: %+v", fp.blocks)
	}
	if len(fs.gateSet) != 0 || len(fs.gateSetGen) != 0 {
		t.Fatalf("no gate write expected on a redundant broadcast: gate=%+v gen=%+v", fs.gateSet, fs.gateSetGen)
	}
}

// A NEW plan version (higher generation) while a gate is still open re-parks: it
// supersedes the prior gate button-free, posts a FRESH gate + the new plan, and a
// click on the SUPERSEDED message is refused server-side (see gatekeeper test). PRD
// #41 Decision 10a.
func TestNotifierRepostsGateOnNewPlanGeneration(t *testing.T) {
	rc := baseRun("awaiting_approval")
	rc.PlanMd = txt("## Plan v2\nthe revised approach")
	fs := &fakeNotifStore{
		rc:        rc,
		delivery:  txt("U1"),
		msg:       store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1", GateTs: txt("gate-v1"), GateState: txt(gateStateOpen), GateGeneration: pgtype.Int4{Int32: 1, Valid: true}},
		planCount: 2, // a second plan version arrived → generation 2 > stored 1
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "awaiting_approval"})

	// The prior gate (gate-v1) is edited button-free to a superseded state. (The root
	// itself is also edited via UpdateBlocks now, so locate the gate edit by ts.)
	superseded, ok := findUpdateBlock(fp.updateBlocks, "gate-v1")
	if !ok || len(superseded.actionIDs) != 0 ||
		!strings.Contains(strings.ToLower(superseded.sectionText), "superseded") {
		t.Fatalf("prior gate must be edited button-free to 'superseded': %+v", fp.updateBlocks)
	}
	// A fresh plan-in-thread + a fresh gate are posted.
	if len(fp.blocks) != 2 || !strings.Contains(fp.blocks[0].sectionText, "revised approach") || len(fp.blocks[1].actionIDs) == 0 {
		t.Fatalf("want a fresh plan post + fresh gate for the new version: %+v", fp.blocks)
	}
	// The anchor advances to generation 2 at the new gate ts.
	if len(fs.gateSetGen) != 1 || fs.gateSetGen[0].GateGeneration.Int32 != 2 || !fs.gateSetGen[0].GateTs.Valid {
		t.Fatalf("anchor must advance to generation 2 at the fresh gate: %+v", fs.gateSetGen)
	}
}

// Cross-surface idempotency: when a run leaves awaiting_approval with a gate still
// open (resolved from the web UI / a timeout / the sweeper), the notifier closes
// the Slack gate message and clears the anchor.
func TestNotifierClosesGateWhenResolvedElsewhere(t *testing.T) {
	rc := baseRun("running")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1", GateTs: txt("gate-ts"), GateState: txt(gateStateOpen)},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	// The root is edited via UpdateBlocks too now, so locate the gate close by ts.
	closed, ok := findUpdateBlock(fp.updateBlocks, "gate-ts")
	if !ok || len(closed.actionIDs) != 0 {
		t.Fatalf("gate message must be edited button-free at its gate_ts: %+v", fp.updateBlocks)
	}
	if len(fs.gateSet) != 1 || fs.gateSet[0].GateTs.Valid || fs.gateSet[0].GateState.Valid {
		t.Fatalf("gate anchor must be cleared (both NULL): %+v", fs.gateSet)
	}
}

func TestNotifierScrubsSecretsOutbound(t *testing.T) {
	rc := baseRun("running")
	rc.IssueTitle = "oops xoxb-leak-token embedded"
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})
	if len(fp.blocks) != 1 {
		t.Fatalf("want a Block Kit post")
	}
	if strings.Contains(fp.blocks[0].sectionText, "xoxb-leak-token") {
		t.Errorf("outbound section leaked a token: %q", fp.blocks[0].sectionText)
	}
	if strings.Contains(fp.blocks[0].fallback, "xoxb-leak-token") {
		t.Errorf("fallback leaked a token: %q", fp.blocks[0].fallback)
	}
}

// ── Run-health nudges (PRD #47 M4) ───────────────────────────────────────────

// healthRun is baseRun with a health flag set, for the handleHealth tests.
func healthRun(status, health string) store.GetSlackRunContextRow {
	rc := baseRun(status)
	rc.Health = health
	return rc
}

func TestNotifierHealthFlipsRootNoNudge(t *testing.T) {
	rc := healthRun("running", "stalled")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "stalled", reason: "the agent stopped sending updates", nudge: false})

	root, ok := findUpdateBlock(fp.updateBlocks, "ts1")
	if !ok || !strings.Contains(root.contextText, "⚠️ stalled") {
		t.Fatalf("root not flipped to ⚠️ stalled context flag: %+v", fp.updateBlocks)
	}
	if len(fp.posts) != 0 {
		t.Fatalf("a non-nudge event must not thread a DM: %+v", fp.posts)
	}
}

func TestNotifierHealthClearEditsRootBack(t *testing.T) {
	rc := healthRun("running", "ok")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "ok", reason: "", nudge: false})

	root, ok := findUpdateBlock(fp.updateBlocks, "ts1")
	if !ok || strings.Contains(root.contextText, "⚠️") || strings.Contains(root.sectionText, "⚠️") {
		t.Fatalf("root should be edited back without ⚠️: %+v", fp.updateBlocks)
	}
	if len(fp.posts) != 0 {
		t.Fatalf("a clear must not thread a DM: %+v", fp.posts)
	}
}

func TestNotifierHealthNudgeThreadsUnderRoot(t *testing.T) {
	rc := healthRun("running", "stalled")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "stalled", reason: "the agent stopped sending updates", nudge: true})

	if _, ok := findUpdateBlock(fp.updateBlocks, "ts1"); !ok {
		t.Fatalf("root should also be re-rendered: %+v", fp.updateBlocks)
	}
	// PRD #268 M3: the nudge is now a Block Kit post (family E) threaded under the root.
	if len(fp.blocks) != 1 || fp.blocks[0].thread != "ts1" {
		t.Fatalf("nudge not threaded under the root ts1: %+v", fp.blocks)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "quiet") {
		t.Errorf("nudge missing its enum-keyed framing: %q", fp.blocks[0].sectionText)
	}
}

func TestNotifierHealthApprovalIdleThreadsUnderGate(t *testing.T) {
	rc := healthRun("awaiting_approval", "approval_idle")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1", GateTs: txt("gate1")},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "approval_idle", reason: "waiting for the plan to be approved", nudge: true})

	if len(fp.blocks) != 1 || fp.blocks[0].thread != "gate1" {
		t.Fatalf("approval_idle nudge should thread under the gate ts: %+v", fp.blocks)
	}
}

func TestNotifierHealthDropsOptedOutMidRun(t *testing.T) {
	rc := healthRun("running", "stalled")
	fs := &fakeNotifStore{rc: rc, deliveryErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "stalled", reason: "x", nudge: true})

	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("an opted-out owner must get nothing: posts=%+v updates=%+v", fp.posts, fp.updates)
	}
}

func TestNotifierHealthCreatesRootWhenAbsent(t *testing.T) {
	rc := healthRun("queued", "waiting_worker")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "waiting_worker", reason: "no worker is online to pick up this run", nudge: true})

	// PRD #268 M3: both the new top-level root AND the threaded nudge are Block Kit posts
	// now, so fp.blocks carries two — the root (thread "") first, then the nudge under it.
	if len(fp.blocks) != 2 {
		t.Fatalf("want the root + the nudge as two Block Kit posts: %+v", fp.blocks)
	}
	root := fp.blocks[0]
	if root.thread != "" || !strings.Contains(root.contextText, "⚠️ waiting for a worker") {
		t.Fatalf("root not created with the flag: %+v", root)
	}
	nudge := fp.blocks[1]
	if nudge.thread != "ts1" {
		t.Fatalf("nudge not threaded under the new root: %+v", nudge)
	}
	if len(fs.upserted) != 1 {
		t.Fatalf("anchor not recorded for the new root: %+v", fs.upserted)
	}
}

func TestNotifierHealthNudgeScrubsSecrets(t *testing.T) {
	rc := healthRun("running", "stalled")
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msg: store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	// A hostile reason carrying a token must never reach Slack.
	n.handleHealth(context.Background(), healthEvent{runID: rc.ID, health: "stalled", reason: "leaked xoxb-abc123DEF token", nudge: true})

	if len(fp.blocks) != 1 {
		t.Fatalf("want a nudge post: %+v", fp.blocks)
	}
	if strings.Contains(fp.blocks[0].sectionText, "xoxb-abc123DEF") {
		t.Errorf("nudge leaked a token: %q", fp.blocks[0].sectionText)
	}
	if !strings.Contains(fp.blocks[0].sectionText, "[redacted]") {
		t.Errorf("token not scrubbed in nudge: %q", fp.blocks[0].sectionText)
	}
}

func TestNotifierHealthNeverBlocks(t *testing.T) {
	fs := &fakeNotifStore{}
	n := NewNotifier(fs, &fakePoster{}, fixedBase, nil)
	// Overfill the queue well past capacity: PublishHealth must drop, never block.
	for i := 0; i < notifierQueue*2; i++ {
		n.PublishHealth(uuid.New(), "stalled", "x", true)
	}
}

// --- PRD #35: the usage-limit park -----------------------------------------------

func parkedCtx(rateLimitType string, resumesIn time.Duration, count int32) store.GetSlackRunContextRow {
	rc := baseRun("limit_wait")
	if rateLimitType != "" {
		rc.RateLimitType = pgtype.Text{String: rateLimitType, Valid: true}
	}
	if resumesIn != 0 {
		rc.RetryNotBefore = pgtype.Timestamptz{Time: time.Now().Add(resumesIn), Valid: true}
	}
	rc.LimitWaitCount = count
	return rc
}

// TestParkedRunRendersAsPausedNotAsARawStatus is the gap PRD #35 closed and PRD #268
// M2 preserves under Block Kit: the notifier has NO status filter, so a parked run must
// not reach the default arm and leak the literal database string "limit_wait" into a
// user's DM. The root section reads "⏸️ Paused · usage limit"; the window/resume detail
// now rides the terminal thread event's context (limitWaitDetail), asserted below.
func TestParkedRunRendersAsPausedNotAsARawStatus(t *testing.T) {
	blocks, fallback := rootBlocks(parkedCtx("five_hour", 4*time.Hour, 1), "https://uzi.example")
	_, section := blockSummary(blocks)
	all := section + contextText(blocks) + fallback
	if strings.Contains(all, "limit_wait") {
		t.Fatalf("root = %q — the raw status string leaked to a user's DM", all)
	}
	if !strings.Contains(section, "Paused · usage limit") {
		t.Fatalf("root section = %q, want it to name the pause", section)
	}
	if !strings.Contains(fallback, "Paused · usage limit") {
		t.Fatalf("root fallback = %q, want it to name the pause", fallback)
	}
}

// A park is the ONE non-terminal transition that threads an event, and the reason is
// mechanical rather than editorial: the root line is EDITED, and a Slack edit raises
// no notification, so without this a user is never told their run paused.
func TestParkThreadsAnEventWhileResumeDoesNot(t *testing.T) {
	if _, _, ok := renderThreadBlocks(parkedCtx("seven_day", time.Hour, 1), "https://uzi.example"); !ok {
		t.Fatal("a park threaded nothing; the edited root raises no Slack notification, so " +
			"the user would never learn the run paused")
	}
	// The promotion back to queued must NOT post — resuming is a return to normal and
	// the edited root already shows it. A run that parks five times would otherwise
	// produce ten posts.
	if _, _, ok := renderThreadBlocks(baseRun("queued"), "https://uzi.example"); ok {
		t.Fatal("the resume threaded an event; only the park is worth interrupting for")
	}
}

// Every part is omitted rather than defaulted when unknown, matching the server's own
// failure-reason composition: the detail must never claim a fact uzi does not have. The
// park head is the section (⏸️ Paused · usage limit); the detail suffix is the context.
func TestLimitWaitDetailOmitsWhatItDoesNotKnow(t *testing.T) {
	// A park with no window and no stamp on the first pause has no detail at all: the
	// section carries the head, and the context is just the deep link.
	if bare := limitWaitDetail(parkedCtx("", 0, 1)); bare != "" {
		t.Fatalf("a park with no window and no stamp had detail %q; it must be empty rather "+
			"than inventing a window or a time", bare)
	}
	blocks, _, ok := renderThreadBlocks(parkedCtx("", 0, 1), "https://uzi.example")
	if !ok {
		t.Fatal("a bare park must still thread the head + link")
	}
	_, section := blockSummary(blocks)
	if !strings.Contains(section, "Paused · usage limit") {
		t.Fatalf("park section = %q, want the pause head", section)
	}
	if strings.Contains(limitWaitDetail(parkedCtx("five_hour", time.Hour, 1)), "(pause") {
		t.Fatal("a pause counter on the FIRST park is noise — the counter is the signal that a run is burning its retry budget")
	}
	if repeat := limitWaitDetail(parkedCtx("five_hour", time.Hour, 3)); !strings.Contains(repeat, "(pause 3)") {
		t.Fatalf("%q — from the second park on, the rising count is the warning that this "+
			"run may be about to fail for good", repeat)
	}
	// The reader-local resume token is preserved verbatim.
	if d := limitWaitDetail(parkedCtx("five_hour", time.Hour, 1)); !strings.Contains(d, "resumes <!date^") {
		t.Fatalf("detail = %q, want the <!date^…> reader-local resume token preserved", d)
	}
}

// The enum is allowlisted server-side and 00091's CHECK backstops it, so this can
// only fire for a writer that bypassed both — a backfill, an admin tool, a later
// refactor. That is exactly the population the CHECK exists for, so the renderer
// escapes rather than trusting.
func TestLimitWaitDetailEscapesTheWindowField(t *testing.T) {
	got := limitWaitDetail(parkedCtx("five_hour<https://evil|click>", time.Hour, 1))
	if strings.Contains(got, "<https://evil|click>") {
		t.Fatalf("unescaped mrkdwn reached the DM: %q", got)
	}
}

// --- PRD #268 M3: the Block Kit terminal thread events (family B) -----------------

// Each terminal transition renders its canonical glyph + label section, the deep link,
// and a fallback built from fixed labels + the escaped repo#iid — never a raw field.
func TestRenderThreadBlocksShapesPerEvent(t *testing.T) {
	failedCancelled := baseRun("failed")
	failedCancelled.FailureReason = txt("run cancelled")

	failedReason := baseRun("failed")
	failedReason.FailureReason = txt("the agent crashed")

	completedMR := baseRun("completed")
	completedMR.MrIid = text8(7)

	for _, tc := range []struct {
		name         string
		rc           store.GetSlackRunContextRow
		wantSection  string
		wantFallback string
	}{
		{"completed", completedMR, "✅ *Completed*", "Completed · grp/repo#42"},
		{"failed", failedReason, "❌ *Failed*", "Failed · grp/repo#42 — the agent crashed"},
		{"failed-run-cancelled", failedCancelled, "🚫 *Cancelled*", "Cancelled · grp/repo#42"},
		{"cancelled", baseRun("cancelled"), "🚫 *Cancelled*", "Cancelled · grp/repo#42"},
		{"limit_wait", parkedCtx("five_hour", time.Hour, 1), "⏸️ *Paused · usage limit*", "Paused · usage limit · grp/repo#42"},
	} {
		blocks, fallback, ok := renderThreadBlocks(tc.rc, "https://uzi.example")
		if !ok {
			t.Errorf("%s: ok=false, want a threaded event", tc.name)
			continue
		}
		_, section := blockSummary(blocks)
		if !strings.Contains(section, tc.wantSection) {
			t.Errorf("%s: section = %q, want it to contain %q", tc.name, section, tc.wantSection)
		}
		if fallback != tc.wantFallback {
			t.Errorf("%s: fallback = %q, want %q", tc.name, fallback, tc.wantFallback)
		}
		// Every event carries the run deep link in a context element.
		if !strings.Contains(contextText(blocks), "Open in uzi") {
			t.Errorf("%s: context missing the deep link: %q", tc.name, contextText(blocks))
		}
	}
}

// A non-terminal, non-park transition threads nothing (ok=false).
func TestRenderThreadBlocksSkipsRunningAndQueued(t *testing.T) {
	for _, status := range []string{"queued", "running", "claimed", "awaiting_approval"} {
		if _, _, ok := renderThreadBlocks(baseRun(status), "https://uzi.example"); ok {
			t.Errorf("%s must not thread a terminal event", status)
		}
	}
}

// The failed reason is untrusted free text: it rides its OWN section (never a context
// element), scrubbed and escaped, and the fallback carries the escaped reason too.
func TestRenderThreadBlocksFailedReasonIsAnEscapedSection(t *testing.T) {
	rc := baseRun("failed")
	rc.FailureReason = txt("boom <@U9> leaked sk-ant-abc123DEF")
	blocks, fallback, ok := renderThreadBlocks(rc, "https://uzi.example")
	if !ok {
		t.Fatal("failed must thread an event")
	}
	_, section := blockSummary(blocks)
	if strings.Contains(section, "<@U9>") || !strings.Contains(section, "&lt;@U9&gt;") {
		t.Errorf("reason not escaped in its section: %q", section)
	}
	if strings.Contains(section, "sk-ant-abc123DEF") || strings.Contains(fallback, "sk-ant-abc123DEF") {
		t.Errorf("reason not scrubbed: section=%q fallback=%q", section, fallback)
	}
}

// Emoji-presentation across the thread events, same guard as the root.
func TestRenderThreadBlocksUseEmojiPresentation(t *testing.T) {
	completedMR := baseRun("completed")
	completedMR.MrIid = text8(7)
	for _, rc := range []store.GetSlackRunContextRow{
		completedMR,
		baseRun("cancelled"),
		parkedCtx("five_hour", time.Hour, 2),
	} {
		blocks, fallback, ok := renderThreadBlocks(rc, "https://uzi.example")
		if !ok {
			t.Fatalf("%s: want a threaded event", rc.Status)
		}
		_, section := blockSummary(blocks)
		assertEmojiPresentation(t, section+contextText(blocks)+fallback)
	}
}

// --- PRD #122 M4: Slack milestone progress ---------------------------------------

func mjson(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal milestone jsonb: %v", err)
	}
	return b
}

// milestoneRun is a running run with a frozen list of `total` milestones (ids m1..mN,
// titles "Milestone N"), `done` of them completed (m1..mDone), and an optional
// in-progress id.
func milestoneRun(t *testing.T, done, total int, inProgress string) store.GetSlackRunContextRow {
	t.Helper()
	rc := baseRun("running")
	frozen := make([]apitypes.Milestone, total)
	for i := 0; i < total; i++ {
		frozen[i] = apitypes.Milestone{ID: fmt.Sprintf("m%d", i+1), Title: fmt.Sprintf("Milestone %d", i+1)}
	}
	completed := make([]string, done)
	for i := 0; i < done; i++ {
		completed[i] = fmt.Sprintf("m%d", i+1)
	}
	rc.MilestonesFrozen = mjson(t, frozen)
	rc.MilestonesCompleted = mjson(t, completed)
	if inProgress != "" {
		rc.MilestonesInProgress = mjson(t, []string{inProgress})
	}
	return rc
}

// The root status line of a milestone-structured run carries the compact `3/7` counter,
// re-rendered in place on the existing anchor like every other state edit.
func TestNotifierRootCarriesMilestoneCounter(t *testing.T) {
	rc := milestoneRun(t, 3, 7, "")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	root, ok := findUpdateBlock(fp.updateBlocks, "ts1")
	if !ok || !strings.Contains(root.sectionText, "Running") || !strings.Contains(root.contextText, "🧩 3/7 milestones") {
		t.Fatalf("root must carry Running + the `🧩 3/7 milestones` context counter: %+v", fp.updateBlocks)
	}
}

// A count-advancing `running` report posts exactly ONE thread line (with the in-progress
// title) and records the new notified count.
func TestNotifierPostsMilestoneLineOnAdvance(t *testing.T) {
	rc := milestoneRun(t, 3, 7, "m4")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg: store.SlackRunMessage{
			RunID: rc.ID, ChannelID: "D1", RootTs: "ts1",
			MilestonesNotifiedCompleted: pgtype.Int4{Int32: 2, Valid: true}, // last posted 2/7
		},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.posts) != 1 || fp.posts[0].thread != "ts1" {
		t.Fatalf("want exactly one milestone thread line under the root: %+v", fp.posts)
	}
	if !strings.Contains(fp.posts[0].text, "🧩 3/7 · working Milestone 4") {
		t.Fatalf("milestone line = %q, want `🧩 3/7 · working Milestone 4`", fp.posts[0].text)
	}
	if len(fs.milestoneSet) != 1 || fs.milestoneSet[0].Count.Int32 != 3 || !fs.milestoneSet[0].Count.Valid {
		t.Fatalf("advanced notified count not recorded as 3: %+v", fs.milestoneSet)
	}
}

// The initial root post (the ErrNoRows first-transition branch) must NEVER carry a
// milestone progress line, even when the run is already mid-progress when Slack is
// first linked. handleMilestone lives only in the existing-message branch; this pins
// that so a future edit that hoists the call above the switch is caught. Without this
// test every other milestone case (all take the existing-message branch) still passes.
func TestNotifierFirstPostCarriesNoMilestoneLine(t *testing.T) {
	rc := milestoneRun(t, 3, 7, "m4") // already 3/7 done at first link
	fs := &fakeNotifStore{rc: rc, delivery: txt("U1"), msgErr: pgx.ErrNoRows}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.blocks) != 1 || fp.blocks[0].thread != "" {
		t.Fatalf("want exactly one top-level root block, no threaded line: %+v", fp.blocks)
	}
	if len(fp.posts) != 0 {
		t.Fatalf("first post must not carry a milestone advance thread line: %+v", fp.posts)
	}
	if len(fs.milestoneSet) != 0 {
		t.Fatalf("first post must not record a notified count: %+v", fs.milestoneSet)
	}
	// The root itself still shows the compact counter — that is the context counter, not a line.
	if !strings.Contains(fp.blocks[0].contextText, "3/7") {
		t.Fatalf("root should still carry the `3/7` context counter: %q", fp.blocks[0].contextText)
	}
}

// When nothing is in progress, the thread line drops the ` · working …` suffix.
func TestNotifierMilestoneLineDropsWorkingSuffixWhenIdle(t *testing.T) {
	rc := milestoneRun(t, 1, 7, "") // no in-progress id
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"}, // NULL notified → 0
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.posts) != 1 || fp.posts[0].text != "🧩 1/7" {
		t.Fatalf("want a bare `🧩 1/7` line with no working suffix: %+v", fp.posts)
	}
}

// A repeated `running` report at the SAME completed count posts NO thread line — dedup is
// on the new notified-count column, not on status.
func TestNotifierDoesNotRepostMilestoneOnUnchangedCount(t *testing.T) {
	rc := milestoneRun(t, 3, 7, "m4")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg: store.SlackRunMessage{
			RunID: rc.ID, ChannelID: "D1", RootTs: "ts1",
			MilestonesNotifiedCompleted: pgtype.Int4{Int32: 3, Valid: true}, // already posted 3/7
		},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.posts) != 0 {
		t.Fatalf("an unchanged count must not re-post a milestone line: %+v", fp.posts)
	}
	if len(fs.milestoneSet) != 0 {
		t.Fatalf("an unchanged count must not record a new notified count: %+v", fs.milestoneSet)
	}
	// The root is still re-edited and keeps the counter.
	root, ok := findUpdateBlock(fp.updateBlocks, "ts1")
	if !ok || !strings.Contains(root.contextText, "3/7") {
		t.Fatalf("root should still carry the counter on a re-broadcast: %+v", fp.updateBlocks)
	}
}

// A `+2` jump in one turn (1/7 → 3/7) posts ONE line and does not lose the jump: the
// count moves straight to 3, not to 2.
func TestNotifierMilestonePlusTwoPostsOneLine(t *testing.T) {
	rc := milestoneRun(t, 3, 7, "m4")
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg: store.SlackRunMessage{
			RunID: rc.ID, ChannelID: "D1", RootTs: "ts1",
			MilestonesNotifiedCompleted: pgtype.Int4{Int32: 1, Valid: true}, // last posted 1/7
		},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.posts) != 1 || !strings.Contains(fp.posts[0].text, "🧩 3/7") {
		t.Fatalf("a +2 jump must post exactly one line at the new count 3/7: %+v", fp.posts)
	}
	if len(fs.milestoneSet) != 1 || fs.milestoneSet[0].Count.Int32 != 3 {
		t.Fatalf("the jump must record the new count 3, not lose it: %+v", fs.milestoneSet)
	}
}

// An unlinked / opted-out run (ErrNoRows on delivery) gets nothing — no milestone line,
// no notified write — and is otherwise unaffected.
func TestNotifierMilestoneUnlinkedGetsNothing(t *testing.T) {
	rc := milestoneRun(t, 3, 7, "m4")
	fs := &fakeNotifStore{rc: rc, deliveryErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("an unlinked owner must receive no Slack call: posts=%+v updates=%+v", fp.posts, fp.updates)
	}
	if len(fs.milestoneSet) != 0 {
		t.Fatalf("an unlinked owner must record no notified count: %+v", fs.milestoneSet)
	}
}

// A run with NO milestones behaves exactly as today: no counter on the root line and no
// milestone thread line.
func TestNotifierNoMilestonesBehavesAsToday(t *testing.T) {
	rc := baseRun("running") // no milestone columns set
	fs := &fakeNotifStore{
		rc:       rc,
		delivery: txt("U1"),
		msg:      store.SlackRunMessage{RunID: rc.ID, ChannelID: "D1", RootTs: "ts1"},
	}
	fp := &fakePoster{dmChannel: "D1"}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: rc.ID, status: "running"})

	root, ok := findUpdateBlock(fp.updateBlocks, "ts1")
	if !ok || regexp.MustCompile(`[0-9]+/[0-9]+`).MatchString(root.contextText) {
		t.Fatalf("a no-milestone run's root must carry no milestone counter: %+v", fp.updateBlocks)
	}
	if len(fp.posts) != 0 {
		t.Fatalf("a no-milestone run must post no milestone thread line: %+v", fp.posts)
	}
	if len(fs.milestoneSet) != 0 {
		t.Fatalf("a no-milestone run must record no notified count: %+v", fs.milestoneSet)
	}
}

// TestForgeMrVocabulary pins the Go twins of web/src/lib/forgeNoun.ts (PRD #65 D2,
// #238 D2): a Forgejo or GitHub run's DM reads "PR #N", a GitLab (or unknown/absent)
// run "MR !N". Both PR-forges must be named explicitly so a missing github arm never
// silently renders GitLab's form.
func TestForgeMrVocabulary(t *testing.T) {
	for _, tc := range []struct {
		forge        string
		abbrev, sref string
	}{
		{"github", "PR", "#"},
		{"forgejo", "PR", "#"},
		{"gitlab", "MR", "!"},
		{"", "MR", "!"},
		{"something_else", "MR", "!"},
	} {
		if got := forgeMrAbbrev(tc.forge); got != tc.abbrev {
			t.Errorf("forgeMrAbbrev(%q) = %q, want %q", tc.forge, got, tc.abbrev)
		}
		if got := forgeMrRef(tc.forge); got != tc.sref {
			t.Errorf("forgeMrRef(%q) = %q, want %q", tc.forge, got, tc.sref)
		}
	}
}

// --- PRD #268 M2: the Block Kit root glyphs -------------------------------------

// statusGlyph is the canonical (emoji, label) per status on the root line. This pins
// the exact pair for every arm, including the `failed`+"run cancelled" sentinel that
// reads as Cancelled, so a later edit that swaps a glyph or reword is caught.
func TestStatusGlyph(t *testing.T) {
	failedCancelled := baseRun("failed")
	failedCancelled.FailureReason = txt("run cancelled")

	for _, tc := range []struct {
		name         string
		rc           store.GetSlackRunContextRow
		emoji, label string
	}{
		{"queued", baseRun("queued"), "⏳", "Queued"},
		{"claimed", baseRun("claimed"), "▶️", "Running"},
		{"running", baseRun("running"), "▶️", "Running"},
		{"awaiting_approval", baseRun("awaiting_approval"), "⏸️", "Needs your approval"},
		{"awaiting_input", baseRun("awaiting_input"), "❓", "Needs your answer"},
		// PRD #517: an interactive task parked awaiting the user's next follow-up. Reddening
		// mutation: remove the awaiting_followup case from statusGlyph → the default arm
		// returns ("", "awaiting_followup"), the raw enum with no emoji, so this row fails.
		// Distinct emoji + label from awaiting_input's ❓ "Needs your answer".
		{"awaiting_followup", baseRun("awaiting_followup"), "💬", "Awaiting your follow-up"},
		{"limit_wait", baseRun("limit_wait"), "⏸️", "Paused · usage limit"},
		{"completed", baseRun("completed"), "✅", "Completed"},
		{"failed", baseRun("failed"), "❌", "Failed"},
		{"cancelled", baseRun("cancelled"), "🚫", "Cancelled"},
		{"failed-run-cancelled", failedCancelled, "🚫", "Cancelled"},
	} {
		emoji, label := statusGlyph(tc.rc)
		if emoji != tc.emoji || label != tc.label {
			t.Errorf("statusGlyph(%s) = (%q, %q), want (%q, %q)", tc.name, emoji, label, tc.emoji, tc.label)
		}
	}
}

// A run with no context elements (no milestones, no MR, non-flagged health, no deep
// link because base is empty) omits the context block entirely — Slack rejects an
// empty one — so rootBlocks returns exactly the single section block.
func TestRootBlocksOmitsEmptyContext(t *testing.T) {
	blocks, _ := rootBlocks(baseRun("running"), "")
	if len(blocks) != 1 {
		t.Fatalf("want exactly one block (context omitted when empty), got %d: %+v", len(blocks), blocks)
	}
	if _, ok := blocks[0].(*slack.SectionBlock); !ok {
		t.Fatalf("the single block must be a *slack.SectionBlock, got %T", blocks[0])
	}
}

// assertEmojiPresentation fails if s carries a bare monochrome glyph: the retired
// ✓ (U+2713, now fully replaced by 🧩), or a ▶ / ⏸ / ⚠ rune not immediately followed
// by the emoji-presentation variation selector U+FE0F. A strings.Contains check
// cannot express this — emoji presentation is a base rune + U+FE0F, so the scan must
// look at the NEXT rune, not the substring.
func assertEmojiPresentation(t *testing.T, s string) {
	t.Helper()
	runes := []rune(s)
	for i, r := range runes {
		if r == '✓' {
			t.Errorf("found bare U+2713 ✓ (should be fully replaced by 🧩): %q", s)
			continue
		}
		if r == '▶' || r == '⏸' || r == '⚠' {
			if i+1 >= len(runes) || runes[i+1] != '️' {
				t.Errorf("found monochrome %q not followed by U+FE0F variation selector: %q", string(r), s)
			}
		}
	}
}

// Every glyph the root emits is emoji-presentation (base rune + U+FE0F), never a bare
// monochrome codepoint, so the DM reads consistently beside the full-color ✅ ❌ 🚫.
// Exercises a running run (▶️), a completed run WITH an MR (✅ + 🔀), and a
// health-flagged run (⚠️) across the section, context and fallback strings.
func TestRootBlocksUseEmojiPresentation(t *testing.T) {
	completedMR := baseRun("completed")
	completedMR.MrIid = text8(7)

	for _, rc := range []store.GetSlackRunContextRow{
		baseRun("running"),
		completedMR,
		healthRun("running", "stalled"),
	} {
		blocks, fallback := rootBlocks(rc, "https://uzi.example")
		_, section := blockSummary(blocks)
		assertEmojiPresentation(t, section+contextText(blocks)+fallback)
	}
}
