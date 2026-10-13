package slacksvc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestNotifierRetentionCapAndOrder(t *testing.T) {
	for _, tc := range []struct {
		name string
		cap  int
		opts bool
		keep int32
	}{
		{"default", 0, false, notifysvc.DefaultUserCap}, {"custom", 7, true, 7},
		{"zero", 0, true, notifysvc.DefaultUserCap}, {"negative", -1, true, notifysvc.DefaultUserCap},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeNotifStore{delivery: linked("U1")}
			var opts []NotifierOption
			if tc.opts {
				opts = append(opts, WithNotificationUserCap(tc.cap))
			}
			n := NewNotifier(fs, &fakePoster{}, fixedBase, nil, opts...)
			tc.cap = 99
			user := uuid.New()
			n.handleNotify(context.Background(), notifyEvent{userID: user, deliveryID: uuid.New()})
			if !reflect.DeepEqual(fs.settlementOrder, []string{"stamp", "prune"}) {
				t.Fatalf("order = %v", fs.settlementOrder)
			}
			want := []store.PruneNotificationsForUserParams{{UserID: user, Keep: tc.keep, MaxAttempts: notifysvc.MaxSlackAttempts}}
			if !reflect.DeepEqual(fs.pruned, want) {
				t.Fatalf("prunes = %+v, want %+v", fs.pruned, want)
			}
		})
	}
}

func TestNotifierRetentionFailureRedacted(t *testing.T) {
	secret := "glpat-" + "abcdefghijklmnopqrst"
	fs := &fakeNotifStore{delivery: linked("U1"), pruneErr: errors.New("database " + secret)}
	fp := &fakePoster{}
	var logs bytes.Buffer
	n := NewNotifier(fs, fp, fixedBase, slog.New(slog.NewTextHandler(&logs, nil)))
	n.handleNotify(context.Background(), notifyEvent{userID: uuid.New(), deliveryID: uuid.New()})
	if len(fs.marked) != 1 || len(fs.pruned) != 1 || len(fp.blocks) != 1 {
		t.Fatal("prune failure undid settlement or reposted")
	}
	if strings.Contains(logs.String(), secret) || !strings.Contains(logs.String(), "prune delivered notification") || !strings.Contains(logs.String(), "[redacted]") {
		t.Fatalf("prune log not redacted: %s", logs.String())
	}
}
