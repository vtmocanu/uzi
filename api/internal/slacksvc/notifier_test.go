package slacksvc

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

type fakeNotifStore struct {
	rc          store.GetSlackRunContextRow
	rcErr       error
	delivery    pgtype.Text
	deliveryErr error
	msg         store.SlackRunMessage
	msgErr      error
	upserted    []store.UpsertSlackRunMessageParams
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

type postCall struct{ channel, thread, text string }
type updateCall struct{ channel, ts, text string }

type fakePoster struct {
	dmChannel string
	posts     []postCall
	updates   []updateCall
	openErr   error
	tsSeq     int
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
	return fmt.Sprintf("ts%d", p.tsSeq), nil
}
func (p *fakePoster) Update(_ context.Context, ch, ts, text string) error {
	p.updates = append(p.updates, updateCall{ch, ts, text})
	return nil
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

func TestNotifierDropsUnlinkedOwner(t *testing.T) {
	fs := &fakeNotifStore{rc: baseRun("running"), deliveryErr: pgx.ErrNoRows}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handle(context.Background(), stateEvent{runID: fs.rc.ID, status: "running"})
	if len(fp.posts) != 0 || len(fp.updates) != 0 {
		t.Fatalf("unlinked owner must not receive any Slack call: posts=%v updates=%v", fp.posts, fp.updates)
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
