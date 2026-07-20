package slacksvc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/slack-go/slack"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
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
}

func (f *fakeNotifStore) GetSlackRunContext(context.Context, uuid.UUID) (store.GetSlackRunContextRow, error) {
	return f.rc, f.rcErr
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

type postCall struct{ channel, thread, text string }
type updateCall struct{ channel, ts, text string }
type blockCall struct {
	channel, thread, fallback string
	sectionText               string
	actionIDs                 []string
}
type updateBlockCall struct {
	channel, ts, fallback string
	sectionText           string
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
	p.blocks = append(p.blocks, blockCall{channel: ch, thread: thread, fallback: fallback, sectionText: sectionText, actionIDs: ids})
	p.tsSeq++
	return fmt.Sprintf("ts%d", p.tsSeq), p.postErr
}
func (p *fakePoster) UpdateBlocks(_ context.Context, ch, ts, fallback string, blks []slack.Block) error {
	ids, sectionText := blockSummary(blks)
	p.updateBlocks = append(p.updateBlocks, updateBlockCall{channel: ch, ts: ts, fallback: fallback, sectionText: sectionText, actionIDs: ids})
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

	if len(fp.posts) != 1 || fp.posts[0].thread != "" {
		t.Fatalf("want 1 top-level post, got %+v", fp.posts)
	}
	body := fp.posts[0].text
	for _, want := range []string{"grp/repo#42", "Add the feature", "running", "/runs/" + fs.rc.ID.String(), "Open in uzi"} {
		if !strings.Contains(body, want) {
			t.Errorf("root missing %q in %q", want, body)
		}
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

	if len(fp.updates) != 1 || fp.updates[0].ts != "ts1" || !strings.Contains(fp.updates[0].text, "MR !7") {
		t.Fatalf("root not edited to completed: %+v", fp.updates)
	}
	if len(fp.posts) != 1 || fp.posts[0].thread != "ts1" {
		t.Fatalf("want 1 threaded event under ts1, got %+v", fp.posts)
	}
	if !strings.Contains(fp.posts[0].text, "/-/merge_requests/7") {
		t.Errorf("thread event missing MR link: %q", fp.posts[0].text)
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

	if len(fp.updates) != 1 || !strings.Contains(fp.updates[0].text, "PR #7") {
		t.Fatalf("root not edited to Forgejo completed: %+v", fp.updates)
	}
	if strings.Contains(fp.updates[0].text, "MR !7") {
		t.Errorf("Forgejo DM must not use GitLab's MR !N form: %q", fp.updates[0].text)
	}
	if len(fp.posts) != 1 || !strings.Contains(fp.posts[0].text, "/pulls/7") {
		t.Fatalf("thread event must link the persisted mr_web_url: %+v", fp.posts)
	}
	if strings.Contains(fp.posts[0].text, "/-/merge_requests/") {
		t.Errorf("Forgejo DM must not reconstruct a GitLab MR URL: %q", fp.posts[0].text)
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

	if len(fp.posts) != 1 {
		t.Fatalf("want 1 threaded event, got %+v", fp.posts)
	}
	if strings.Contains(fp.posts[0].text, "javascript:") {
		t.Errorf("a non-https mr_web_url must not be rendered: %q", fp.posts[0].text)
	}
	// It falls back to the GitLab reconstruction (this row's forge is gitlab).
	if !strings.Contains(fp.posts[0].text, "/-/merge_requests/7") {
		t.Errorf("want the GitLab reconstruction fallback: %q", fp.posts[0].text)
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

	if len(fp.posts) != 1 {
		t.Fatalf("want a post, got %+v", fp.posts)
	}
	body := fp.posts[0].text
	// The hostile markup must appear only in escaped form.
	if strings.Contains(body, "<https://phishing.example|Open in uzi>") {
		t.Errorf("raw spoofed link survived into the DM: %q", body)
	}
	if strings.Contains(body, "<@U123>") || strings.Contains(body, "<@channel>") {
		t.Errorf("raw mention survived into the DM: %q", body)
	}
	if !strings.Contains(body, "&lt;https://phishing.example|Open in uzi&gt;") || !strings.Contains(body, "&lt;@U123&gt;") {
		t.Errorf("hostile fields were not mrkdwn-escaped: %q", body)
	}
	// The genuine deep link (trusted base + uuid) must stay raw and clickable.
	if !strings.Contains(body, "<https://uzi.example/runs/"+rc.ID.String()+"|Open in uzi>") {
		t.Errorf("legit deep link was broken by over-escaping: %q", body)
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

	if len(fp.posts) != 1 {
		t.Fatalf("want one threaded failure event, got %+v", fp.posts)
	}
	evt := fp.posts[0].text
	if strings.Contains(evt, "<@U9>") {
		t.Errorf("raw mention survived in the failure event: %q", evt)
	}
	if !strings.Contains(evt, "&lt;@U9&gt;") {
		t.Errorf("failure reason was not escaped: %q", evt)
	}
	if !strings.Contains(evt, "…") || strings.Contains(evt, strings.Repeat("x", 501)) {
		t.Errorf("failure reason was not length-bounded: %q", evt)
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

	// The prior gate (gate-v1) is edited button-free to a superseded state.
	if len(fp.updateBlocks) != 1 || fp.updateBlocks[0].ts != "gate-v1" || len(fp.updateBlocks[0].actionIDs) != 0 ||
		!strings.Contains(strings.ToLower(fp.updateBlocks[0].sectionText), "superseded") {
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

	if len(fp.updateBlocks) != 1 || fp.updateBlocks[0].ts != "gate-ts" || len(fp.updateBlocks[0].actionIDs) != 0 {
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
	if len(fp.posts) != 1 {
		t.Fatalf("want a post")
	}
	if strings.Contains(fp.posts[0].text, "xoxb-leak-token") {
		t.Errorf("outbound text leaked a token: %q", fp.posts[0].text)
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

	if len(fp.updates) != 1 || !strings.Contains(fp.updates[0].text, "⚠ stalled") {
		t.Fatalf("root not flipped to ⚠ stalled: %+v", fp.updates)
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

	if len(fp.updates) != 1 || strings.Contains(fp.updates[0].text, "⚠") {
		t.Fatalf("root should be edited back without ⚠: %+v", fp.updates)
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

	if len(fp.updates) != 1 {
		t.Fatalf("root should also be re-rendered: %+v", fp.updates)
	}
	if len(fp.posts) != 1 || fp.posts[0].thread != "ts1" {
		t.Fatalf("nudge not threaded under the root ts1: %+v", fp.posts)
	}
	if !strings.Contains(fp.posts[0].text, "quiet") {
		t.Errorf("nudge missing its enum-keyed framing: %q", fp.posts[0].text)
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

	if len(fp.posts) != 1 || fp.posts[0].thread != "gate1" {
		t.Fatalf("approval_idle nudge should thread under the gate ts: %+v", fp.posts)
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

	// Post 1 is the new top-level root (carrying the flag); post 2 is the nudge under it.
	if len(fp.posts) != 2 {
		t.Fatalf("want a root post + a threaded nudge, got %+v", fp.posts)
	}
	if fp.posts[0].thread != "" || !strings.Contains(fp.posts[0].text, "⚠ waiting for a worker") {
		t.Fatalf("root not created with the flag: %+v", fp.posts[0])
	}
	if fp.posts[1].thread != "ts1" {
		t.Fatalf("nudge not threaded under the new root: %+v", fp.posts[1])
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

	if len(fp.posts) != 1 {
		t.Fatalf("want a nudge post: %+v", fp.posts)
	}
	if strings.Contains(fp.posts[0].text, "xoxb-abc123DEF") {
		t.Errorf("nudge leaked a token: %q", fp.posts[0].text)
	}
	if !strings.Contains(fp.posts[0].text, "[redacted]") {
		t.Errorf("token not scrubbed in nudge: %q", fp.posts[0].text)
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
