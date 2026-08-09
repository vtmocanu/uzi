package notifysvc

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"gitlab.example.com/vtmocanu/uzi/api/internal/store"
)

// fakeStore records the calls Notify makes so the persist-first ordering, the
// prune cap, and the payload marshaling are all observable without a database.
type fakeStore struct {
	order        []string
	inserted     store.InsertNotificationParams
	pruned       store.PruneNotificationsForUserParams
	insertErr    error
	pruneErr     error
	returnedID   uuid.UUID
	insertCalled bool
	pruneCalled  bool
}

func (f *fakeStore) InsertNotification(_ context.Context, arg store.InsertNotificationParams) (store.Notification, error) {
	f.order = append(f.order, "insert")
	f.insertCalled = true
	f.inserted = arg
	if f.insertErr != nil {
		return store.Notification{}, f.insertErr
	}
	f.returnedID = uuid.New()
	return store.Notification{ID: f.returnedID, UserID: arg.UserID, Kind: arg.Kind, Payload: arg.Payload}, nil
}

func (f *fakeStore) PruneNotificationsForUser(_ context.Context, arg store.PruneNotificationsForUserParams) (int64, error) {
	f.order = append(f.order, "prune")
	f.pruneCalled = true
	f.pruned = arg
	return 0, f.pruneErr
}

// fakeSlacker records PublishNotification calls, sharing the store's order slice
// so the insert→prune→slack sequence is asserted end to end.
type fakeSlacker struct {
	order      *[]string
	calls      int
	lastUserID uuid.UUID
	lastRender SlackRender
}

func (f *fakeSlacker) PublishNotification(userID uuid.UUID, r SlackRender) {
	if f.order != nil {
		*f.order = append(*f.order, "slack")
	}
	f.calls++
	f.lastUserID = userID
	f.lastRender = r
}

func TestNotifyPersistsThenPrunesThenSlack(t *testing.T) {
	fs := &fakeStore{}
	slk := &fakeSlacker{order: &fs.order}
	svc := New(fs, slk, 0, nil)

	user := uuid.New()
	run := uuid.New()
	if _, err := svc.Notify(context.Background(), Notification{
		UserID:  user,
		Kind:    "judge_review",
		Payload: map[string]any{"verdict": "ok"},
		RunID:   &run,
		Slack:   &SlackRender{Title: "judge review ready", Body: "verdict: ok", Link: "https://uzi.example/runs/1", Emoji: "🔎", Facts: []string{"Verdict ✅ *ok*", "1 recommendation"}},
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// The load-bearing invariant: the durable row is written before Slack is ever
	// attempted, and the prune rides after the persist too.
	got := fs.order
	if len(got) != 3 || got[0] != "insert" || got[1] != "prune" || got[2] != "slack" {
		t.Fatalf("call order = %v, want [insert prune slack]", got)
	}
	if fs.inserted.UserID != user || fs.inserted.Kind != "judge_review" {
		t.Fatalf("inserted = %+v, want user/kind to match", fs.inserted)
	}
	if !fs.inserted.RunID.Valid || uuid.UUID(fs.inserted.RunID.Bytes) != run {
		t.Fatalf("inserted run anchor = %+v, want %s", fs.inserted.RunID, run)
	}
	if fs.inserted.ReviewID.Valid {
		t.Fatalf("review anchor should be NULL when unset, got %+v", fs.inserted.ReviewID)
	}
	var payload map[string]any
	if err := json.Unmarshal(fs.inserted.Payload, &payload); err != nil {
		t.Fatalf("payload not valid json: %v (%s)", err, fs.inserted.Payload)
	}
	if payload["verdict"] != "ok" {
		t.Fatalf("payload = %v, want verdict=ok", payload)
	}
	if slk.calls != 1 || slk.lastUserID != user || slk.lastRender.Body != "verdict: ok" {
		t.Fatalf("slack call = %d user=%s body=%q, want one call to the owner", slk.calls, slk.lastUserID, slk.lastRender.Body)
	}
	if slk.lastRender.Emoji != "🔎" || len(slk.lastRender.Facts) != 2 || slk.lastRender.Facts[0] != "Verdict ✅ *ok*" {
		t.Fatalf("slack render = %+v, want the emoji + facts passed through as the struct", slk.lastRender)
	}
}

func TestNotifyNilPayloadMarshalsEmptyObject(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, nil, 0, nil)
	if _, err := svc.Notify(context.Background(), Notification{UserID: uuid.New(), Kind: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if string(fs.inserted.Payload) != "{}" {
		t.Fatalf("nil payload persisted as %q, want {}", fs.inserted.Payload)
	}
}

func TestNotifyWithoutSlackRenderSkipsDelivery(t *testing.T) {
	fs := &fakeStore{}
	slk := &fakeSlacker{}
	svc := New(fs, slk, 0, nil)
	// Slacker is wired, but no SlackRender ⇒ inbox-only, no DM.
	if _, err := svc.Notify(context.Background(), Notification{UserID: uuid.New(), Kind: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if slk.calls != 0 {
		t.Fatalf("slack called %d times, want 0 when no SlackRender", slk.calls)
	}
	if !fs.insertCalled || !fs.pruneCalled {
		t.Fatalf("persist+prune must still run: insert=%v prune=%v", fs.insertCalled, fs.pruneCalled)
	}
}

func TestNotifyNilSlackerIsInboxOnly(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, nil, 0, nil)
	if _, err := svc.Notify(context.Background(), Notification{
		UserID: uuid.New(), Kind: "x", Slack: &SlackRender{Title: "t", Body: "b"},
	}); err != nil {
		t.Fatalf("Notify with nil slacker: %v", err)
	}
	if !fs.insertCalled {
		t.Fatalf("row must still persist with no Slacker wired")
	}
}

func TestNotifyInsertErrorSkipsPruneAndSlack(t *testing.T) {
	fs := &fakeStore{insertErr: errors.New("boom")}
	slk := &fakeSlacker{}
	svc := New(fs, slk, 0, nil)
	_, err := svc.Notify(context.Background(), Notification{
		UserID: uuid.New(), Kind: "x", Slack: &SlackRender{Body: "b"},
	})
	if err == nil {
		t.Fatalf("Notify should surface the persist error")
	}
	// Persist-first: nothing runs after a failed durable write.
	if fs.pruneCalled {
		t.Fatalf("prune must not run when persist fails")
	}
	if slk.calls != 0 {
		t.Fatalf("slack must not fire when persist fails")
	}
}

func TestNotifyPruneFailureIsNonFatal(t *testing.T) {
	fs := &fakeStore{pruneErr: errors.New("prune boom")}
	slk := &fakeSlacker{}
	svc := New(fs, slk, 0, nil)
	row, err := svc.Notify(context.Background(), Notification{
		UserID: uuid.New(), Kind: "x", Slack: &SlackRender{Body: "b"},
	})
	if err != nil {
		t.Fatalf("a prune failure must not fail Notify: %v", err)
	}
	if row.ID == (uuid.UUID{}) {
		t.Fatalf("the persisted row should still be returned")
	}
	if slk.calls != 1 {
		t.Fatalf("Slack should still fire after a best-effort prune failure, calls=%d", slk.calls)
	}
}

func TestNotifyPruneUsesConfiguredCap(t *testing.T) {
	fs := &fakeStore{}
	svc := New(fs, nil, 5, nil)
	if _, err := svc.Notify(context.Background(), Notification{UserID: uuid.New(), Kind: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if fs.pruned.Keep != 5 {
		t.Fatalf("prune keep = %d, want the configured cap 5", fs.pruned.Keep)
	}

	fs2 := &fakeStore{}
	def := New(fs2, nil, 0, nil) // non-positive ⇒ DefaultUserCap
	if _, err := def.Notify(context.Background(), Notification{UserID: uuid.New(), Kind: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if fs2.pruned.Keep != DefaultUserCap {
		t.Fatalf("prune keep = %d, want DefaultUserCap %d", fs2.pruned.Keep, DefaultUserCap)
	}
}
