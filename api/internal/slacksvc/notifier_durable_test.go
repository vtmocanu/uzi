package slacksvc

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// Durable Slack delivery (issue #1675): handleNotify stamps the notification row
// delivered on success and on the terminal no-link outcomes, and nothing else.

func TestHandleNotifyDurableStamping(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name      string
		store     *fakeNotifStore
		poster    *fakePoster
		deliverID uuid.UUID
		wantMark  bool
	}{
		{"posted", &fakeNotifStore{delivery: linked("U1")}, &fakePoster{}, id, true},
		{"unlinked (ErrNoRows)", &fakeNotifStore{deliveryErr: pgx.ErrNoRows}, &fakePoster{}, id, true},
		{"empty link", &fakeNotifStore{delivery: linked("")}, &fakePoster{}, id, true},
		{"resolve error", &fakeNotifStore{deliveryErr: errors.New("db down")}, &fakePoster{}, id, false},
		{"open dm fails", &fakeNotifStore{delivery: linked("U1")}, &fakePoster{openErr: errors.New("slack down")}, id, false},
		{"post fails", &fakeNotifStore{delivery: linked("U1")}, &fakePoster{postErr: errors.New("slack down")}, id, false},
		{"credential fence not current", &fakeNotifStore{delivery: linked("U1")}, &fakePoster{}, id, false},
		{"zero id never stamps", &fakeNotifStore{delivery: linked("U1")}, &fakePoster{}, uuid.Nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := NewNotifier(c.store, c.poster, fixedBase, nil)
			ev := notifyEvent{userID: uuid.New(), title: "halted", deliveryID: c.deliverID}
			if c.name == "credential fence not current" {
				ev.credential = &notifysvc.CredentialFence{SecretID: uuid.New(), Kind: store.KindAnthropicToken}
			}
			n.handleNotify(context.Background(), ev)
			if c.wantMark {
				if len(c.store.marked) != 1 || c.store.marked[0] != id {
					t.Errorf("marked = %v, want [%s]", c.store.marked, id)
				}
				want := store.PruneNotificationsForUserParams{UserID: ev.userID, Keep: notifysvc.DefaultUserCap, MaxAttempts: notifysvc.MaxSlackAttempts}
				if len(c.store.pruned) != 1 || c.store.pruned[0] != want {
					t.Errorf("pruned = %+v, want %+v", c.store.pruned, want)
				}
			} else if len(c.store.pruned) != 0 {
				t.Errorf("pruned unsettled notification: %+v", c.store.pruned)
			} else if len(c.store.marked) != 0 {
				t.Errorf("marked = %v, want none", c.store.marked)
			}
		})
	}
}

func TestHandleNotifyMarkFailureDoesNotPanicOrRepost(t *testing.T) {
	fs := &fakeNotifStore{delivery: linked("U1"), markErr: errors.New("db down")}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.handleNotify(context.Background(), notifyEvent{userID: uuid.New(), title: "t", deliveryID: uuid.New()})
	if len(fp.blocks) != 1 || len(fs.marked) != 1 || len(fs.pruned) != 0 {
		t.Errorf("posts=%d marks=%d, want 1/1", len(fp.blocks), len(fs.marked))
	}
}

// sqlMirror reproduces, in memory, the notifications-table state the durable delivery
// relies on: InsertNotification stores the render, ClaimPendingSlackNotifications returns
// the undelivered rows and bumps their attempts, MarkNotificationSlackDelivered settles one.
// It implements notifysvc.Store and notifysvc.RedeliveryStore.
type sqlMirror struct {
	mu   sync.Mutex
	rows []*mirrorRow
}

type mirrorRow struct {
	id        uuid.UUID
	userID    uuid.UUID
	render    []byte
	attempts  int32
	delivered bool
}

func (m *sqlMirror) InsertNotification(_ context.Context, a store.InsertNotificationParams) (store.Notification, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &mirrorRow{id: uuid.New(), userID: a.UserID, render: a.SlackRender}
	if a.SlackRender != nil {
		r.attempts = 1
	}
	m.rows = append(m.rows, r)
	return store.Notification{ID: r.id, UserID: a.UserID, Kind: a.Kind, Payload: a.Payload}, nil
}
func (m *sqlMirror) PruneNotificationsForUser(context.Context, store.PruneNotificationsForUserParams) (int64, error) {
	return 0, nil
}

func (m *sqlMirror) GetSecretEnablement(context.Context, store.GetSecretEnablementParams) (store.GetSecretEnablementRow, error) {
	return store.GetSecretEnablementRow{}, pgx.ErrNoRows
}
func (m *sqlMirror) ClaimPendingSlackNotifications(_ context.Context, a store.ClaimPendingSlackNotificationsParams) ([]store.ClaimPendingSlackNotificationsRow, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.ClaimPendingSlackNotificationsRow
	for _, r := range m.rows {
		if r.render == nil || r.delivered || r.attempts >= a.MaxAttempts || len(out) >= int(a.Lim) {
			continue
		}
		r.attempts++
		out = append(out, store.ClaimPendingSlackNotificationsRow{ID: r.id, UserID: r.userID, SlackRender: r.render, SlackAttempts: r.attempts})
	}
	return out, nil
}
func (m *sqlMirror) markDelivered(id uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.id == id {
			r.delivered = true
		}
	}
}
func (m *sqlMirror) delivered(id uuid.UUID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.id == id {
			return r.delivered
		}
	}
	return false
}

// mirrorNotifStore is the notifier's store whose delivered stamp lands in the mirror.
type mirrorNotifStore struct {
	*fakeNotifStore
	m *sqlMirror
}

func (s mirrorNotifStore) MarkNotificationSlackDelivered(ctx context.Context, id uuid.UUID) error {
	s.m.markDelivered(id)
	return s.fakeNotifStore.MarkNotificationSlackDelivered(ctx, id)
}

// TestDroppedHaltDMIsRedelivered is the issue #1675 acceptance: a durable halt DM whose
// first in-memory enqueue is dropped (notifier queue full) is posted exactly once by the
// redelivery sweep, the row is then marked delivered, and a second sweep finds nothing.
// No timing: the notifier is never Run; the test drains notifyCh by hand.
func TestDroppedHaltDMIsRedelivered(t *testing.T) {
	ctx := context.Background()
	mirror := &sqlMirror{}
	fp := &fakePoster{}
	n := NewNotifier(mirrorNotifStore{&fakeNotifStore{delivery: linked("U1")}, mirror}, fp, fixedBase, nil)

	// Fill the queue so the first delivery is dropped.
	for i := 0; i < cap(n.notifyCh); i++ {
		n.notifyCh <- notifyEvent{title: "filler"}
	}
	svc := notifysvc.New(mirror, n, 0, nil)
	user := uuid.New()
	row, err := svc.Notify(ctx, notifysvc.Notification{
		UserID: user, Kind: "ci_autofix_halted", Payload: notifysvc.CIAutofixPayload{Ref: "main"},
		DurableSlack: true,
		Slack:        &notifysvc.SlackRender{Title: "CI auto-fix stopped", Body: "no progress", Emoji: "🛑"},
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if len(n.notifyCh) != cap(n.notifyCh) {
		t.Fatalf("queue len %d, want it still full (halt DM dropped)", len(n.notifyCh))
	}
	if mirror.delivered(row.ID) || len(fp.blocks) != 0 {
		t.Fatalf("precondition: halt DM must be undelivered and unposted")
	}

	// The queue drains (the fillers go nowhere), then the sweep re-enqueues the halt.
	for len(n.notifyCh) > 0 {
		<-n.notifyCh
	}
	red := notifysvc.NewRedeliverer(mirror, n, nil)
	if got, err := red.Pass(ctx); err != nil || got != 1 {
		t.Fatalf("first Pass = %d, %v; want 1, nil", got, err)
	}
	select {
	case ev := <-n.notifyCh:
		n.handleNotify(ctx, ev)
	default:
		t.Fatal("redelivery enqueued nothing")
	}
	if len(fp.blocks) != 1 {
		t.Fatalf("posts = %d, want exactly 1", len(fp.blocks))
	}
	if !mirror.delivered(row.ID) {
		t.Errorf("row not marked delivered after the post")
	}

	if got, err := red.Pass(ctx); err != nil || got != 0 {
		t.Errorf("second Pass = %d, %v; want 0, nil (delivered row must not be claimed)", got, err)
	}
	if len(n.notifyCh) != 0 || len(fp.blocks) != 1 {
		t.Errorf("second Pass re-enqueued (queue %d, posts %d)", len(n.notifyCh), len(fp.blocks))
	}
}
