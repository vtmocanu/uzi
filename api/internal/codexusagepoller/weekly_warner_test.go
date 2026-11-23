package codexusagepoller

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

type weeklyCall struct {
	fence         notifysvc.CodexAccountFence
	buckets       []apitypes.CodexRateLimitBucketDTO
	lastSuccessAt time.Time
}

type fakeWeeklyWarner struct {
	mu    sync.Mutex
	calls []weeklyCall
}

func (f *fakeWeeklyWarner) ObserveCodexWeekly(_ context.Context, fence notifysvc.CodexAccountFence, buckets []apitypes.CodexRateLimitBucketDTO, lastSuccessAt time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, weeklyCall{fence, buckets, lastSuccessAt})
}

// PRD #4123 M3: the weekly warner is called after a written upsert with the account fence the write
// observed, the reading's buckets and the exact last_success_at the upsert returned (microsecond
// precision), with no early-reset notifier wired.
func TestWeeklyWarnerHookAfterWrittenUpsert(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	row := linkedRow(userID, acct, 2, 5, "idle", false, false)
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{row}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {reading: sampleReading(2, 5)}}}
	e := newEngineFor(st, col, &fakeReconciler{orderRef: st})
	w := &fakeWeeklyWarner{}
	e.SetWeeklyWarner(w)

	e.Boot(context.Background())

	if len(w.calls) != 1 {
		t.Fatalf("hook calls = %d, want 1", len(w.calls))
	}
	got := w.calls[0]
	if !got.lastSuccessAt.Equal(fakeLastSuccess) || got.lastSuccessAt.Nanosecond()%1000 != 0 {
		t.Fatalf("hook instant = %v, want the upsert's returned last_success_at %v", got.lastSuccessAt, fakeLastSuccess)
	}
	want := notifysvc.CodexAccountFence{OwnerID: userID, AccountID: acct, Generation: 2, CredentialRevision: 5, EnablementSig: row.EnablementSig}
	if got.fence != want {
		t.Fatalf("hook fence = %+v, want %+v", got.fence, want)
	}
	if len(got.buckets) != 1 || got.buckets[0].ID != "codex" {
		t.Fatalf("hook buckets = %+v", got.buckets)
	}
}

// A write the fence refused (the store returns no row) reports nothing to the warner.
func TestWeeklyWarnerHookSilentWhenTheFenceRefusesTheWrite(t *testing.T) {
	userID, acct := uuid.New(), uuid.New()
	st := newFakeStore()
	st.upsertRows = 0
	st.toPoll = []store.ListLinkedCodexAccountsToPollRow{linkedRow(userID, acct, 2, 5, "idle", false, false)}
	col := &fakeCollector{results: map[uuid.UUID]collectResult{acct: {reading: sampleReading(2, 5)}}}
	e := newEngineFor(st, col, &fakeReconciler{orderRef: st})
	w := &fakeWeeklyWarner{}
	e.SetWeeklyWarner(w)

	e.Boot(context.Background())

	if len(w.calls) != 0 {
		t.Fatalf("hook calls = %d, want 0 after a refused write", len(w.calls))
	}
}
