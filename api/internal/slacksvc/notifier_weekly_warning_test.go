package slacksvc

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestCodexWeeklyWarningDispatchFence: a Codex weekly warning is posted only while the
// account fence is current, and it is asked through IsCodexWeeklyWarningCurrent (never the
// early-reset query, which also needs the owner's early-reset opt-in).
func TestCodexWeeklyWarningDispatchFence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		current   bool
		err       error
		wantPosts int
	}{
		{"current", true, nil, 1},
		{"stale", false, nil, 0},
		{"no row", false, pgx.ErrNoRows, 0},
		{"read error", true, errors.New("boom"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := codexDeliveryFence()
			f.Generation, f.CredentialRevision = 3, 5
			fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: tc.current, codexErr: tc.err}
			fp := &fakePoster{}
			n := NewNotifier(fs, fp, fixedBase, slog.New(slog.DiscardHandler))
			n.PublishNotification(f.OwnerID, notifysvc.SlackRender{Title: "Weekly limit", DeliveryTag: notifysvc.DeliveryTagCodexWeeklyWarning, CodexAccount: &f, DeliveryID: uuid.New()})
			ev := <-n.notifyCh
			n.handleNotify(context.Background(), ev)
			if len(fp.blocks) != tc.wantPosts {
				t.Fatalf("posts = %d, want %d", len(fp.blocks), tc.wantPosts)
			}
			want := store.IsCodexWeeklyWarningCurrentParams{UserID: f.OwnerID, ProviderAccountID: f.AccountID, ObservedGeneration: 3, ObservedCredentialRevision: 5, EnablementSig: f.EnablementSig}
			if !reflect.DeepEqual(fs.weeklyAsked, []store.IsCodexWeeklyWarningCurrentParams{want}) || len(fs.codexAsked) != 0 {
				t.Fatalf("weekly asked = %+v, early asked = %+v", fs.weeklyAsked, fs.codexAsked)
			}
		})
	}
}

// TestCodexWeeklyWarningDispatchMalformed: a weekly-warning tag without a valid, owner-scoped
// account fence (or with a Claude fence beside it) is dropped without a fence read.
func TestCodexWeeklyWarningDispatchMalformed(t *testing.T) {
	for _, name := range []string{"missing fence", "blank sig", "owner mismatch", "Claude conflict"} {
		t.Run(name, func(t *testing.T) {
			f := codexDeliveryFence()
			user := f.OwnerID
			r := notifysvc.SlackRender{DeliveryTag: notifysvc.DeliveryTagCodexWeeklyWarning, CodexAccount: &f}
			switch name {
			case "missing fence":
				r.CodexAccount = nil
			case "blank sig":
				f.EnablementSig = " "
			case "owner mismatch":
				user = uuid.New()
			case "Claude conflict":
				r.Credential = &notifysvc.CredentialFence{SecretID: uuid.New(), Kind: store.KindAnthropicToken}
			}
			fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: true}
			fp := &fakePoster{}
			n := NewNotifier(fs, fp, fixedBase, slog.New(slog.DiscardHandler))
			n.PublishNotification(user, r)
			n.handleNotify(context.Background(), <-n.notifyCh)
			if len(fp.blocks) != 0 || len(fs.weeklyAsked) != 0 || len(fs.codexAsked) != 0 {
				t.Fatalf("posts=%d weekly=%d early=%d, want all 0", len(fp.blocks), len(fs.weeklyAsked), len(fs.codexAsked))
			}
		})
	}
}
