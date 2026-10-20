package slacksvc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func (f *fakeNotifStore) IsCodexEarlyResetCurrent(_ context.Context, p store.IsCodexEarlyResetCurrentParams) (bool, error) {
	f.codexAsked = append(f.codexAsked, p)
	return f.codexCurrent, f.codexErr
}

func codexDeliveryFence() notifysvc.CodexAccountFence {
	return notifysvc.CodexAccountFence{OwnerID: uuid.New(), AccountID: uuid.New(), EnablementSig: "private-alias-signature"}
}

func TestCodexDispatchAuthority(t *testing.T) {
	for _, name := range []string{"initial zero", "nonzero", "multiple aliases one account", "ownership mismatch", "last alias disabled", "disable re-enable", "unlink last alias", "relink other account", "generation changed", "revision changed", "in progress", "quarantine", "reauth", "recovery", "alerts off", "false currentness", "no row", "read error", "global optout after enqueue", "no Slack link"} {
		t.Run(name, func(t *testing.T) {
			f := codexDeliveryFence()
			if name == "nonzero" {
				f.Generation = 8
				f.CredentialRevision = 12
			}
			success := name == "initial zero" || name == "nonzero" || name == "multiple aliases one account"
			fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: true}
			fp := &fakePoster{}
			var logs bytes.Buffer
			n := NewNotifier(fs, fp, fixedBase, slog.New(slog.NewTextHandler(&logs, nil)))
			n.PublishNotification(f.OwnerID, notifysvc.SlackRender{Title: "CODEX WEEKLY LIMIT RESET EARLY", DeliveryTag: notifysvc.DeliveryTagCodexEarlyReset, CodexAccount: &f, DeliveryID: uuid.New()})
			// Change authority while the notification waits in the actual queue.
			fs.codexCurrent = success
			if name == "no row" {
				fs.codexErr = pgx.ErrNoRows
			}
			if name == "read error" {
				fs.codexCurrent = true
				fs.codexErr = errors.New("private-alias-signature provider detail")
			}
			noLink := name == "global optout after enqueue" || name == "no Slack link"
			if noLink {
				fs.deliveryErr = pgx.ErrNoRows
			}
			ev := <-n.notifyCh
			if ev.deliveryID != uuid.Nil {
				t.Fatal("Codex acquired durable identity")
			}
			n.handleNotify(context.Background(), ev)
			wantPosts := 0
			if success {
				wantPosts = 1
			}
			if len(fp.blocks) != wantPosts || len(fs.marked) != 0 {
				t.Fatalf("posts=%d marks=%v", len(fp.blocks), fs.marked)
			}
			want := store.IsCodexEarlyResetCurrentParams{UserID: f.OwnerID, ProviderAccountID: f.AccountID, ObservedGeneration: f.Generation, ObservedCredentialRevision: f.CredentialRevision, EnablementSig: f.EnablementSig}
			if !noLink && !reflect.DeepEqual(fs.codexAsked, []store.IsCodexEarlyResetCurrentParams{want}) {
				t.Fatalf("scope=%+v", fs.codexAsked)
			}
			if noLink && len(fs.codexAsked) != 0 {
				t.Fatal("unlinked should drop")
			}
			if !success && !noLink && !strings.Contains(logs.String(), "slack: dropped Codex notification") {
				t.Fatal(logs.String())
			}
			if strings.Contains(logs.String(), "private-alias-signature") || strings.Contains(logs.String(), "provider detail") {
				t.Fatal("unsafe diagnostic")
			}
		})
	}
}

func TestCodexDispatchMalformed(t *testing.T) {
	for _, name := range []string{"missing fence", "zero fence", "nil owner", "nil account", "blank sig", "negative generation", "negative revision", "owner mismatch", "unknown tag", "unknown tag no fence", "fence without tag", "Claude conflict"} {
		t.Run(name, func(t *testing.T) {
			f := codexDeliveryFence()
			user := f.OwnerID
			r := notifysvc.SlackRender{DeliveryTag: notifysvc.DeliveryTagCodexEarlyReset, CodexAccount: &f, DeliveryID: uuid.New()}
			switch name {
			case "missing fence":
				r.CodexAccount = nil
			case "zero fence":
				f = notifysvc.CodexAccountFence{}
			case "nil owner":
				f.OwnerID = uuid.Nil
			case "nil account":
				f.AccountID = uuid.Nil
			case "blank sig":
				f.EnablementSig = " \t"
			case "negative generation":
				f.Generation = -1
			case "negative revision":
				f.CredentialRevision = -1
			case "owner mismatch":
				user = uuid.New()
			case "unknown tag":
				r.DeliveryTag = "untrusted-provider-input"
			case "unknown tag no fence":
				r.DeliveryTag = "untrusted-provider-input"
				r.CodexAccount = nil
			case "fence without tag":
				r.DeliveryTag = ""
			case "Claude conflict":
				r.Credential = &notifysvc.CredentialFence{SecretID: uuid.New(), Kind: store.KindAnthropicToken}
			}
			fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: true}
			fp := &fakePoster{}
			var logs bytes.Buffer
			n := NewNotifier(fs, fp, fixedBase, slog.New(slog.NewTextHandler(&logs, nil)))
			n.PublishNotification(user, r)
			ev := <-n.notifyCh
			if ev.deliveryID != uuid.Nil {
				t.Fatal("malformed Codex retained durable ID")
			}
			n.handleNotify(context.Background(), ev)
			if len(fp.blocks) != 0 || len(fs.codexAsked) != 0 || len(fs.secretAsked) != 0 || len(fs.marked) != 0 {
				t.Fatal("malformed event delivered/read authority")
			}
			// Pin the complete diagnostic payload without timestamp to prove no caller fields escaped.
			line := logs.String()
			idx := strings.Index(line, "level=WARN")
			if idx < 0 || line[idx:] != "level=WARN msg=\"slack: dropped Codex notification\"\n" {
				t.Fatal(line)
			}
		})
	}
}

func TestCodexQueueCopiesEveryFenceField(t *testing.T) {
	f := codexDeliveryFence()
	f.Generation = 4
	f.CredentialRevision = 9
	original := f
	fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: true}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	n.PublishNotification(f.OwnerID, notifysvc.SlackRender{DeliveryTag: notifysvc.DeliveryTagCodexEarlyReset, CodexAccount: &f})
	f.OwnerID = uuid.New()
	f.AccountID = uuid.New()
	f.Generation = 17
	f.CredentialRevision = 23
	f.EnablementSig = "changed"
	n.handleNotify(context.Background(), <-n.notifyCh)
	want := store.IsCodexEarlyResetCurrentParams{UserID: original.OwnerID, ProviderAccountID: original.AccountID, ObservedGeneration: 4, ObservedCredentialRevision: 9, EnablementSig: "private-alias-signature"}
	if len(fp.blocks) != 1 || !reflect.DeepEqual(fs.codexAsked, []store.IsCodexEarlyResetCurrentParams{want}) {
		t.Fatalf("posts=%d params=%+v", len(fp.blocks), fs.codexAsked)
	}
}

func TestCodexDurableExclusion(t *testing.T) {
	for _, name := range []string{"tag only", "fence only", "full"} {
		t.Run(name, func(t *testing.T) {
			f := codexDeliveryFence()
			r := notifysvc.SlackRender{DeliveryTag: notifysvc.DeliveryTagCodexEarlyReset, CodexAccount: &f, DeliveryID: uuid.New()}
			if name == "tag only" {
				r.CodexAccount = nil
			}
			if name == "fence only" {
				r.DeliveryTag = ""
			}
			mirror := &sqlMirror{}
			fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: true}
			n := NewNotifier(fs, &fakePoster{}, fixedBase, nil)
			_, err := notifysvc.New(mirror, n, 0, nil).Notify(context.Background(), notifysvc.Notification{UserID: f.OwnerID, Kind: notifysvc.KindEarlyLimitReset, Slack: &r, DurableSlack: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(mirror.rows) != 1 || mirror.rows[0].render != nil || mirror.rows[0].attempts != 0 {
				t.Fatal("Codex stored durable render")
			}
			ev := <-n.notifyCh
			if ev.deliveryID != uuid.Nil {
				t.Fatal("Codex retained delivery ID")
			}
			claims, err := mirror.ClaimPendingSlackNotifications(context.Background(), store.ClaimPendingSlackNotificationsParams{MaxAttempts: notifysvc.MaxSlackAttempts, Lim: 10})
			if err != nil || len(claims) != 0 {
				t.Fatalf("claims=%v err=%v", claims, err)
			}
			// The redeliverer has no eligible Codex candidate.
			count, err := notifysvc.NewRedeliverer(mirror, n, nil).Pass(context.Background())
			if err != nil || count != 0 || len(n.notifyCh) != 0 {
				t.Fatalf("redelivery=%d err=%v", count, err)
			}
		})
	}
}

// A real creation-to-queue-to-render check keeps the trusted timestamp markup intact.
func TestCodexCreationAndSlackRendering(t *testing.T) {
	f := codexDeliveryFence()
	fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: true}
	fp := &fakePoster{}
	n := NewNotifier(fs, fp, fixedBase, nil)
	observed := time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)
	_, err := notifysvc.New(&codexCreationStore{sqlMirror: &sqlMirror{}}, n, 0, nil).NotifyCodexEarlyReset(context.Background(), f, observed.Add(2*time.Hour), observed)
	if err != nil {
		t.Fatal(err)
	}
	n.handleNotify(context.Background(), <-n.notifyCh)
	if len(fp.blocks) != 1 {
		t.Fatal("missing post")
	}
	got := fp.blocks[0].sectionText + fp.blocks[0].contextText
	for _, want := range []string{"🚨 *CODEX WEEKLY LIMIT RESET EARLY*", "> Your weekly Codex limit reopened ahead of schedule.", "reset ~2h early", "observed <!date^1792306800^{time}|07:00 UTC>", "expected <!date^1792314000^{time}|09:00 UTC>"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render=%q missing=%q", got, want)
		}
	}
	for _, private := range []string{f.OwnerID.String(), f.AccountID.String(), f.EnablementSig} {
		if strings.Contains(got, private) {
			t.Fatal("private fence rendered")
		}
	}
}

// OpenDM is an outbound operation before the post; revoke authority there to prove
// the query read happens after opening the channel, rather than only at dequeue.
type codexRevokingPoster struct {
	*fakePoster
	store *fakeNotifStore
}

func (p *codexRevokingPoster) OpenDM(ctx context.Context, target string) (string, error) {
	p.store.codexCurrent = false
	return p.fakePoster.OpenDM(ctx, target)
}
func TestCodexRechecksImmediatelyBeforePost(t *testing.T) {
	f := codexDeliveryFence()
	fs := &fakeNotifStore{delivery: linked("U123"), codexCurrent: true}
	fp := &fakePoster{}
	n := NewNotifier(fs, &codexRevokingPoster{fakePoster: fp, store: fs}, fixedBase, nil)
	n.PublishNotification(f.OwnerID, notifysvc.SlackRender{DeliveryTag: notifysvc.DeliveryTagCodexEarlyReset, CodexAccount: &f})
	n.handleNotify(context.Background(), <-n.notifyCh)
	if fp.dmChannel == "" || len(fp.blocks) != 0 || len(fs.codexAsked) != 1 {
		t.Fatalf("channel=%q posts=%d reads=%d", fp.dmChannel, len(fp.blocks), len(fs.codexAsked))
	}
}

type codexCreationStore struct{ *sqlMirror }

func (*codexCreationStore) IsCodexEarlyResetCurrent(context.Context, store.IsCodexEarlyResetCurrentParams) (bool, error) {
	return true, nil
}
