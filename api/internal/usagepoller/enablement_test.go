package usagepoller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// PRD #1732 M3a: the poller's enablement fences. The SQL half of each fence (the
// listing and poke resolve drop disabled tokens, the upsert refuses a moved
// revision) is proven against Postgres in the store package's LiveDB tests; these
// prove the engine carries the captured revision end to end and acts on the
// fence's answer.

// A disabled token is polled on neither the tick nor a poke (D1): no Anthropic call,
// no gauge write, whichever way the poke names it.
func TestDisabledTokenPolledOnNeitherTickNorPoke(t *testing.T) {
	on, off := uuid.New(), uuid.New()
	st := newFakeStore(on, off)
	st.transition(off, false)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return reading(5, 5, anthropic.SourceUsageEndpoint), nil
	}}
	e, _ := newEngine(t, st, &fakeOpener{}, cl, true)

	e.tickAll(context.Background())
	if usage, _ := cl.calls(); usage != 1 {
		t.Fatalf("tick made %d Anthropic calls, want 1 (the enabled token only)", usage)
	}
	if _, ok := st.got(off); ok {
		t.Fatal("tick wrote a reading for a disabled token")
	}

	e.pokeUser(context.Background(), off, uuid.Nil) // the default-token poke (a save)
	e.pokeUser(context.Background(), off, off)      // the named-token poke (a re-enable)
	if usage, _ := cl.calls(); usage != 1 {
		t.Fatalf("poking a disabled token made %d Anthropic calls in total, want still 1", usage)
	}
	if _, ok := st.got(off); ok {
		t.Fatal("poke wrote a reading for a disabled token")
	}
}

// PokeSecret reaches the named token, not the owner's default: a re-enabled token is
// usually not the default, and polling the default instead would leave it unread.
func TestPokeSecretResolvesTheNamedToken(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	e, _ := newEngine(t, st, &fakeOpener{}, &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return reading(1, 1, anthropic.SourceUsageEndpoint), nil
	}}, true)
	named := uuid.New()

	e.PokeSecret(u, named)
	e.Poke(u)
	for range 2 {
		p := <-e.poke
		e.pokeUser(context.Background(), p.userID, p.secretID)
	}

	if len(st.resolved) != 2 {
		t.Fatalf("resolved %d pokes, want 2", len(st.resolved))
	}
	if got := st.resolved[0]; got.UserID != u || !got.SecretID.Valid || got.SecretID.Bytes != named {
		t.Fatalf("PokeSecret resolved %+v, want user %s secret %s", got, u, named)
	}
	if got := st.resolved[1]; got.UserID != u || got.SecretID.Valid {
		t.Fatalf("Poke resolved %+v, want user %s and the default (no secret id)", got, u)
	}
}

// A poll that started before a disable and finished after the re-enable writes
// nothing and alerts nothing (D13), on the tick and on the poke. The fixture would
// otherwise fire an early-reset alert: a pending prior reading at the poll's own
// starting revision and a moved reset epoch.
func TestPollAcrossDisableAndReenableWritesAndAlertsNothing(t *testing.T) {
	for _, path := range []string{"tick", "poke"} {
		t.Run(path, func(t *testing.T) {
			tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
			moved := tReset.Add(72 * time.Hour)
			u := uuid.New()
			st := newFakeStore(u)
			st.setNotify(u, true)
			st.setPrev(u, prevRow(u, u, 99, anthropic.SourceUsageEndpoint, tReset, tReset.Add(-72*time.Hour)))
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
				// The owner disables and re-enables the token while Anthropic answers.
				st.transition(u, false)
				st.transition(u, true)
				return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
			}}
			notif := &fakeNotifier{}
			e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
			clk.set(tReset.Add(-10 * time.Hour))
			e.SetNotifier(notif)

			if path == "tick" {
				e.tickAll(context.Background())
			} else {
				e.pokeUser(context.Background(), u, u)
			}

			if usage, _ := cl.calls(); usage != 1 {
				t.Fatalf("Anthropic calls = %d, want 1 (the poll did run)", usage)
			}
			if got, ok := st.got(u); ok {
				t.Fatalf("a poll from before the disable wrote after the re-enable: %+v", got)
			}
			if notif.count() != 0 {
				t.Fatalf("notify calls = %d, want 0 (no alert without the fenced write)", notif.count())
			}
		})
	}
}

// After a re-enable the first fresh poll writes at the new revision, and a reading
// from before the disable is no basis for an alert (D13): the same fixture that
// fires at an unchanged revision stays silent here.
func TestPreDisableReadingIsNoAlertBasisAfterReenable(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true)
	st.setPrev(u, prevRow(u, u, 99, anthropic.SourceUsageEndpoint, tReset, tReset.Add(-72*time.Hour)))
	st.transition(u, false)
	st.transition(u, true)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.tickAll(context.Background())

	got, ok := st.got(u)
	if !ok || got.EnablementRev != 2 {
		t.Fatalf("fresh reading = %+v (written=%v), want one written at revision 2", got, ok)
	}
	if notif.count() != 0 {
		t.Fatalf("notify calls = %d, want 0 (a pre-disable reading is no comparison basis)", notif.count())
	}
}

// The alert carries the credential and the revision its fenced write landed at, so
// the notifier can re-check both before delivery (D13).
func TestEarlyResetCarriesSecretAndRevision(t *testing.T) {
	tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	moved := tReset.Add(72 * time.Hour)
	u := uuid.New()
	st := newFakeStore(u)
	st.setNotify(u, true)
	st.transition(u, false)
	st.transition(u, true)
	st.transition(u, false)
	st.transition(u, true) // revision 4
	row := prevRow(u, u, 99, anthropic.SourceUsageEndpoint, tReset, tReset.Add(-72*time.Hour))
	row.EnablementRev = 4
	st.setPrev(u, row)
	cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
	}}
	notif := &fakeNotifier{}
	e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
	clk.set(tReset.Add(-10 * time.Hour))
	e.SetNotifier(notif)

	e.tickAll(context.Background())

	call, ok := notif.last()
	if !ok {
		t.Fatal("no alert at an unchanged revision")
	}
	if call.userID != u || call.secretID != u || call.rev != 4 {
		t.Fatalf("alert = user %s secret %s rev %d, want user %s secret %s rev 4", call.userID, call.secretID, call.rev, u, u)
	}
}

// refusingNotifier answers every alert with err, like a notifier whose re-check refused.
type refusingNotifier struct{ err error }

func (n refusingNotifier) NotifyEarlyReset(context.Context, uuid.UUID, uuid.UUID, int64, time.Time, time.Time) (store.Notification, error) {
	return store.Notification{}, n.err
}

// levelCounter is a slog handler that counts records per level.
type levelCounter struct {
	mu     sync.Mutex
	counts map[slog.Level]int
}

func (h *levelCounter) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelCounter) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.counts[r.Level]++
	return nil
}
func (h *levelCounter) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelCounter) WithGroup(string) slog.Handler      { return h }

func (h *levelCounter) count(l slog.Level) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[l]
}

// An alert the notifier refused because the credential stopped being current between
// the fenced write and delivery (D13) is an expected race, logged below Error; any
// other notifier failure is still an Error.
func TestRefusedEarlyResetAlertIsNotAnError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		wantError int
	}{
		{"credential no longer current", fmt.Errorf("notify: %w", notifysvc.ErrCredentialNotCurrent), 0},
		{"delivery failure", errors.New("db down"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tReset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
			moved := tReset.Add(72 * time.Hour)
			u := uuid.New()
			st := newFakeStore(u)
			st.setNotify(u, true)
			st.setPrev(u, prevRow(u, u, 99, anthropic.SourceUsageEndpoint, tReset, tReset.Add(-72*time.Hour)))
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
				return readingWithReset(10, anthropic.SourceUsageEndpoint, &moved), nil
			}}
			e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
			clk.set(tReset.Add(-10 * time.Hour))
			logs := &levelCounter{counts: map[slog.Level]int{}}
			e.logger = slog.New(logs)
			e.SetNotifier(refusingNotifier{err: tc.err})

			e.tickAll(context.Background())

			if got := logs.count(slog.LevelError); got != tc.wantError {
				t.Fatalf("error-level records = %d, want %d", got, tc.wantError)
			}
			if tc.wantError == 0 && logs.count(slog.LevelInfo) == 0 {
				t.Fatal("the dropped alert left no trace in the log")
			}
		})
	}
}
