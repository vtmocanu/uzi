package usagepoller

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/anthropic"
)

type weeklyCall struct {
	userID, secretID uuid.UUID
	rev              int64
	pct              int16
	resetsAt         *time.Time
	syncedAt         time.Time
}

type fakeWeeklyWarner struct {
	mu    sync.Mutex
	calls []weeklyCall
}

func (f *fakeWeeklyWarner) ObserveAnthropicWeekly(_ context.Context, userID, secretID uuid.UUID, rev int64, pct *int16, resetsAt *time.Time, syncedAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, weeklyCall{userID: userID, secretID: secretID, rev: rev, pct: *pct, resetsAt: resetsAt, syncedAt: syncedAt})
}

func (f *fakeWeeklyWarner) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// PRD #4123 M3: the weekly warner is called after a written (fenced) upsert, with the instant that
// upsert persisted, whether or not the owner opted in to the early-reset alert.
func TestWeeklyWarnerHookAfterWrittenUpsert(t *testing.T) {
	reset := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	for _, notifyEarly := range []bool{false, true} {
		name := "notifyEarly off"
		if notifyEarly {
			name = "notifyEarly on"
		}
		t.Run(name, func(t *testing.T) {
			u := uuid.New()
			st := newFakeStore(u)
			st.setNotify(u, notifyEarly)
			cl := &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
				return readingWithReset(88, anthropic.SourceUsageEndpoint, &reset), nil
			}}
			e, clk := newEngine(t, st, &fakeOpener{}, cl, true)
			clk.set(time.Date(2026, 7, 15, 12, 0, 0, 987654321, time.UTC))
			w := &fakeWeeklyWarner{}
			e.SetWeeklyWarner(w)

			e.tickAll(context.Background())

			if w.count() != 1 {
				t.Fatalf("hook calls = %d, want 1", w.count())
			}
			got := w.calls[0]
			persisted, ok := st.got(u)
			if !ok {
				t.Fatal("no upsert")
			}
			if !got.syncedAt.Equal(persisted.SyncedAt.Time) || got.syncedAt.Nanosecond()%1000 != 0 {
				t.Fatalf("hook instant = %v, persisted synced_at = %v (want the exact microsecond value)", got.syncedAt, persisted.SyncedAt.Time)
			}
			if got.userID != u || got.secretID != u || got.pct != 88 || got.resetsAt == nil || !got.resetsAt.Equal(reset) || got.rev != persisted.EnablementRev {
				t.Fatalf("hook call = %+v", got)
			}
		})
	}
}

// A fence-refused write and a failed write report nothing to the warner, with the early-reset alert
// on or off.
func TestWeeklyWarnerHookSilentWithoutAWrite(t *testing.T) {
	for _, notifyEarly := range []bool{false, true} {
		for _, tc := range []string{"fence refused (stale revision)", "store error"} {
			t.Run(tc, func(t *testing.T) {
				u := uuid.New()
				st := newFakeStore(u)
				st.setNotify(u, notifyEarly)
				e, _ := newEngine(t, st, &fakeOpener{}, &fakeClient{}, false)
				w := &fakeWeeklyWarner{}
				e.SetWeeklyWarner(w)
				rev := int64(0)
				if tc == "store error" {
					st.upsertErr = errors.New("db down")
				} else {
					rev = 99
				}
				e.observe(context.Background(), u, u, rev, notifyEarly, reading(10, 90, anthropic.SourceUsageEndpoint))
				if w.count() != 0 {
					t.Fatalf("hook calls = %d, want 0", w.count())
				}
			})
		}
	}
}

// No warner wired (the default) changes nothing.
func TestWeeklyWarnerNilIsSilent(t *testing.T) {
	u := uuid.New()
	st := newFakeStore(u)
	e, _ := newEngine(t, st, &fakeOpener{}, &fakeClient{usage: func([]byte) (anthropic.Reading, error) {
		return reading(1, 2, anthropic.SourceUsageEndpoint), nil
	}}, false)
	e.tickAll(context.Background())
	if _, ok := st.got(u); !ok {
		t.Fatal("upsert must still happen")
	}
}
