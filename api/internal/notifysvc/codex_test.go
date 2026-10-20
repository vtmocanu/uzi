package notifysvc

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func (f *fakeStore) IsCodexEarlyResetCurrent(_ context.Context, p store.IsCodexEarlyResetCurrentParams) (bool, error) {
	f.order = append(f.order, "codex-current")
	f.codexAsked = append(f.codexAsked, p)
	return f.codexCurrent, f.codexErr
}

func codexFence() CodexAccountFence {
	return CodexAccountFence{OwnerID: uuid.New(), AccountID: uuid.New(), EnablementSig: "opaque-alias-signature"}
}

func TestNotifyCodexEarlyResetCreation(t *testing.T) {
	for _, counters := range [][2]int64{{0, 0}, {7, 11}} {
		t.Run(time.Duration(counters[0]).String(), func(t *testing.T) {
			f := codexFence()
			f.Generation, f.CredentialRevision = counters[0], counters[1]
			fs := &fakeStore{codexCurrent: true}
			slk := &fakeSlacker{order: &fs.order}
			observed := time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)
			expected := observed.Add(90 * time.Minute)
			_, err := New(fs, slk, 0, nil).NotifyCodexEarlyReset(context.Background(), f, expected, observed)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fs.order, []string{"codex-current", "insert", "prune", "slack"}) {
				t.Fatal(fs.order)
			}
			want := store.IsCodexEarlyResetCurrentParams{UserID: f.OwnerID, ProviderAccountID: f.AccountID, ObservedGeneration: counters[0], ObservedCredentialRevision: counters[1], EnablementSig: "opaque-alias-signature"}
			if !reflect.DeepEqual(fs.codexAsked, []store.IsCodexEarlyResetCurrentParams{want}) {
				t.Fatalf("scope: %+v", fs.codexAsked)
			}
			if slk.calls != 1 || slk.lastUserID != f.OwnerID || fs.inserted.Kind != KindEarlyLimitReset {
				t.Fatal("expected one account event")
			}
			var payload EarlyResetPayload
			if err := json.Unmarshal(fs.inserted.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload != (EarlyResetPayload{Title: "Codex weekly limit reset early", Expected: "2026-10-18T08:30:00Z", Observed: "2026-10-18T07:00:00Z", HoursEarly: 2}) {
				t.Fatalf("%+v", payload)
			}
			r := slk.lastRender
			if r.Title != "CODEX WEEKLY LIMIT RESET EARLY" || r.Body != "Your weekly Codex limit reopened ahead of schedule." || r.Emoji != "🚨" ||
				r.DeliveryTag != DeliveryTagCodexEarlyReset || r.Credential != nil || r.CodexAccount == nil || *r.CodexAccount != f {
				t.Fatalf("%+v", r)
			}
			if !reflect.DeepEqual(r.Facts, []string{"reset ~2h early", "observed <!date^1792306800^{time}|07:00 UTC>", "expected <!date^1792312200^{time}|08:30 UTC>"}) {
				t.Fatal(r.Facts)
			}
		})
	}
}

func TestNotifyCodexEarlyResetHours(t *testing.T) {
	observed := time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name               string
		expected, observed time.Time
		want               int
	}{
		{"maximum supported reset", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), observed, 69891665},
		{"just below half hour", observed.Add(90 * time.Minute), observed.Add(time.Nanosecond), 1},
		{"exact half hour", observed.Add(90 * time.Minute), observed, 2},
		{"just above half hour", observed.Add(90 * time.Minute), observed.Add(-time.Nanosecond), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{codexCurrent: true}
			_, err := New(fs, nil, 0, nil).NotifyCodexEarlyReset(context.Background(), codexFence(), tc.expected, tc.observed)
			if err != nil {
				t.Fatal(err)
			}
			var payload EarlyResetPayload
			if err := json.Unmarshal(fs.inserted.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.HoursEarly != tc.want {
				t.Fatalf("hours gained = %d, want %d", payload.HoursEarly, tc.want)
			}
		})
	}
}

func TestNotifyCodexEarlyResetMalformed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CodexAccountFence)
	}{
		{"zero fence", func(f *CodexAccountFence) { *f = CodexAccountFence{} }},
		{"nil owner", func(f *CodexAccountFence) { f.OwnerID = uuid.Nil }},
		{"nil account", func(f *CodexAccountFence) { f.AccountID = uuid.Nil }},
		{"empty sig", func(f *CodexAccountFence) { f.EnablementSig = "" }},
		{"blank sig", func(f *CodexAccountFence) { f.EnablementSig = " \t" }},
		{"negative generation", func(f *CodexAccountFence) { f.Generation = -1 }},
		{"negative revision", func(f *CodexAccountFence) { f.CredentialRevision = -1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := codexFence()
			tc.mutate(&f)
			fs := &fakeStore{codexCurrent: true}
			slk := &fakeSlacker{}
			_, err := New(fs, slk, 0, nil).NotifyCodexEarlyReset(context.Background(), f, time.Now(), time.Now())
			if !errors.Is(err, ErrCredentialNotCurrent) || len(fs.order) != 0 || slk.calls != 0 {
				t.Fatalf("err=%v order=%v calls=%d", err, fs.order, slk.calls)
			}
		})
	}
}

// The M1 live SQL tests prove these transitions; here each refusal is returned by
// the actual IsCodexEarlyResetCurrent seam, without duplicating SQL authority logic.
func TestNotifyCodexEarlyResetAuthorityRefusals(t *testing.T) {
	for _, name := range []string{"ownership mismatch", "last alias disabled", "disable re-enable", "unlink last alias", "relink other account", "generation changed", "revision changed", "in progress", "quarantine", "reauth", "recovery", "alerts off", "false currentness", "no row", "read error"} {
		t.Run(name, func(t *testing.T) {
			f := codexFence()
			f.Generation = 4
			f.CredentialRevision = 6
			fs := &fakeStore{}
			if name == "no row" {
				fs.codexErr = pgx.ErrNoRows
			}
			if name == "read error" {
				fs.codexCurrent = true
				fs.codexErr = errors.New("private provider input")
			}
			slk := &fakeSlacker{}
			_, err := New(fs, slk, 0, nil).NotifyCodexEarlyReset(context.Background(), f, time.Now(), time.Now())
			want := store.IsCodexEarlyResetCurrentParams{UserID: f.OwnerID, ProviderAccountID: f.AccountID, ObservedGeneration: 4, ObservedCredentialRevision: 6, EnablementSig: f.EnablementSig}
			if !errors.Is(err, ErrCredentialNotCurrent) || !reflect.DeepEqual(fs.codexAsked, []store.IsCodexEarlyResetCurrentParams{want}) || fs.insertCalled || fs.pruneCalled || slk.calls != 0 {
				t.Fatalf("err=%v store=%+v calls=%d", err, fs, slk.calls)
			}
		})
	}
}

func TestNotifyCodexEarlyResetInsertError(t *testing.T) {
	boom := errors.New("insert failed")
	fs := &fakeStore{codexCurrent: true, insertErr: boom}
	slk := &fakeSlacker{}
	_, err := New(fs, slk, 0, nil).NotifyCodexEarlyReset(context.Background(), codexFence(), time.Now(), time.Now())
	if !errors.Is(err, boom) || slk.calls != 0 || !reflect.DeepEqual(fs.order, []string{"codex-current", "insert"}) {
		t.Fatalf("%v %v calls=%d", err, fs.order, slk.calls)
	}
}
