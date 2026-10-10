package notifysvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func durableNotification(user uuid.UUID) Notification {
	return Notification{
		UserID:       user,
		Kind:         "ci_autofix_halted",
		Payload:      CIAutofixPayload{Ref: "main"},
		DurableSlack: true,
		Slack: &SlackRender{Title: "halted", Body: "b", Link: "https://uzi.example/x", LinkLabel: "Open",
			Emoji: "🛑", Facts: []string{"one", "two"}},
	}
}

func TestNotifyDurableStoresRenderAndStampsDeliveryID(t *testing.T) {
	fs := &fakeStore{}
	slk := &fakeSlacker{order: &fs.order}
	svc := New(fs, slk, 0, nil)

	n := durableNotification(uuid.New())
	n.Slack.DeliveryID = uuid.New() // callers never set it; Notify overwrites
	if _, err := svc.Notify(context.Background(), n); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(fs.inserted.SlackRender, &got); err != nil {
		t.Fatalf("stored render is not JSON (%q): %v", fs.inserted.SlackRender, err)
	}
	want := map[string]any{"title": "halted", "body": "b", "link": "https://uzi.example/x",
		"link_label": "Open", "emoji": "🛑", "facts": []any{"one", "two"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("stored render = %v, want %v", got, want)
	}
	if slk.lastRender.DeliveryID != fs.returnedID {
		t.Errorf("published DeliveryID = %s, want the inserted row id %s", slk.lastRender.DeliveryID, fs.returnedID)
	}
	if fs.pruned.MaxAttempts != MaxSlackAttempts {
		t.Errorf("prune MaxAttempts = %d, want %d", fs.pruned.MaxAttempts, MaxSlackAttempts)
	}
}

func TestNotifyNonDurableStoresNoRender(t *testing.T) {
	user := uuid.New()
	cred := &CredentialFence{SecretID: uuid.New(), Kind: store.KindAnthropicToken}
	cases := map[string]struct {
		mut      func(*Notification)
		nilSlack bool
	}{
		"opt-out":  {mut: func(n *Notification) { n.DurableSlack = false }},
		"fenced":   {mut: func(n *Notification) { n.Slack.Credential = cred }},
		"no slack": {mut: func(n *Notification) {}, nilSlack: true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			fs := &fakeStore{}
			slk := &fakeSlacker{order: &fs.order}
			var s Slacker = slk
			if c.nilSlack {
				s = nil
			}
			n := durableNotification(user)
			c.mut(&n)
			if _, err := New(fs, s, 0, nil).Notify(context.Background(), n); err != nil {
				t.Fatalf("Notify: %v", err)
			}
			if fs.inserted.SlackRender != nil {
				t.Errorf("stored render %q, want none", fs.inserted.SlackRender)
			}
			if !c.nilSlack && slk.lastRender.DeliveryID != uuid.Nil {
				t.Errorf("DeliveryID = %s, want zero", slk.lastRender.DeliveryID)
			}
		})
	}
}

type fakeClaimer struct {
	rows     []store.ClaimPendingSlackNotificationsRow
	err      error
	params   []store.ClaimPendingSlackNotificationsParams
	pruned   []store.PruneNotificationsForUserParams
	pruneErr error
	onPrune  func()
}

func (f *fakeClaimer) ClaimPendingSlackNotifications(_ context.Context, a store.ClaimPendingSlackNotificationsParams) ([]store.ClaimPendingSlackNotificationsRow, error) {
	f.params = append(f.params, a)
	return f.rows, f.err
}

func (f *fakeClaimer) PruneNotificationsForUser(_ context.Context, a store.PruneNotificationsForUserParams) (int64, error) {
	f.pruned = append(f.pruned, a)
	if f.onPrune != nil {
		f.onPrune()
	}
	return 0, f.pruneErr
}

type recordingSlacker struct {
	users   []uuid.UUID
	renders []SlackRender
}

func (r *recordingSlacker) PublishNotification(u uuid.UUID, s SlackRender) {
	r.users = append(r.users, u)
	r.renders = append(r.renders, s)
}

func TestRedelivererPublishesClaimedRowsAndSkipsCorrupt(t *testing.T) {
	good := store.ClaimPendingSlackNotificationsRow{ID: uuid.New(), UserID: uuid.New(), SlackAttempts: 2,
		SlackRender: []byte(`{"title":"t","body":"b","link":"l","link_label":"ll","emoji":"e","facts":["f"]}`)}
	bad := store.ClaimPendingSlackNotificationsRow{ID: uuid.New(), UserID: uuid.New(), SlackAttempts: 2,
		SlackRender: []byte(`{not json`)}
	last := store.ClaimPendingSlackNotificationsRow{ID: uuid.New(), UserID: uuid.New(), SlackAttempts: MaxSlackAttempts,
		SlackRender: []byte(`{"title":"last"}`)}
	fc := &fakeClaimer{rows: []store.ClaimPendingSlackNotificationsRow{bad, good, last}}
	slk := &recordingSlacker{}

	n, err := NewRedeliverer(fc, slk, nil).Pass(context.Background())
	if err != nil {
		t.Fatalf("Pass: %v", err)
	}
	if n != 2 || len(slk.renders) != 2 {
		t.Fatalf("published %d (recorded %d), want 2", n, len(slk.renders))
	}
	want := SlackRender{Title: "t", Body: "b", Link: "l", LinkLabel: "ll", Emoji: "e", Facts: []string{"f"}, DeliveryID: good.ID}
	if !reflect.DeepEqual(slk.renders[0], want) || slk.users[0] != good.UserID {
		t.Errorf("first publish = %+v for %s, want %+v for %s", slk.renders[0], slk.users[0], want, good.UserID)
	}
	if slk.renders[1].DeliveryID != last.ID {
		t.Errorf("last-attempt row not still published with its id")
	}
	p := fc.params[0]
	if p.MaxAttempts != MaxSlackAttempts || p.RetryAfterSecs != int32(SlackRetryAfter.Seconds()) || p.Lim <= 0 {
		t.Errorf("claim params = %+v", p)
	}
}

func TestRedelivererNilSlackClaimsNothing(t *testing.T) {
	fc := &fakeClaimer{}
	n, err := NewRedeliverer(fc, nil, nil).Pass(context.Background())
	if n != 0 || err != nil || len(fc.params) != 0 {
		t.Errorf("Pass = %d, %v, claims=%d; want 0, nil, 0", n, err, len(fc.params))
	}
}

func TestRedelivererClaimErrorSurfaces(t *testing.T) {
	boom := errors.New("boom")
	_, err := NewRedeliverer(&fakeClaimer{err: boom}, &recordingSlacker{}, nil).Pass(context.Background())
	if !errors.Is(err, boom) {
		t.Errorf("err = %v, want wrapping boom", err)
	}
}
