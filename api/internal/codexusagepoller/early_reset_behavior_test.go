package codexusagepoller

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// This fake models accepted writes becoming the next comparison baseline. It
// deliberately implements no detection or notification deduplication.
type earlyResetBehaviorStore struct {
	*fakeStore
	baseline  store.GetCodexEarlyResetBaselineRow
	writeTime time.Time
}

func (s *earlyResetBehaviorStore) GetCodexEarlyResetBaseline(_ context.Context, p store.GetCodexEarlyResetBaselineParams) (store.GetCodexEarlyResetBaselineRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.baseline
	if !b.LastSuccessAt.Valid || !b.NotifyEarlyLimitReset ||
		p.UserID != b.UserID || p.ProviderAccountID != b.ProviderAccountID ||
		!b.EnablementSig.Valid || p.EnablementSig != b.EnablementSig.String {
		return store.GetCodexEarlyResetBaselineRow{}, pgx.ErrNoRows
	}
	b.Buckets = append([]byte(nil), b.Buckets...)
	return b, nil
}

func (s *earlyResetBehaviorStore) UpsertCodexAccountRateLimits(ctx context.Context, p store.UpsertCodexAccountRateLimitsParams) (int64, error) {
	n, err := s.fakeStore.UpsertCodexAccountRateLimits(ctx, p)
	if err != nil || n != 1 {
		return n, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseline = store.GetCodexEarlyResetBaselineRow{
		UserID: p.UserID, ProviderAccountID: p.ProviderAccountID,
		Buckets:               append([]byte(nil), p.Buckets...),
		LastSuccessAt:         pgtype.Timestamptz{Time: s.writeTime, Valid: true},
		EnablementSig:         pgtype.Text{String: p.EnablementSig, Valid: true},
		NotifyEarlyLimitReset: true,
	}
	return n, nil
}

type earlyResetBehaviorEvent struct {
	fence              notifysvc.CodexAccountFence
	expected, observed time.Time
}

type earlyResetBehaviorNotifier struct {
	mu     sync.Mutex
	events []earlyResetBehaviorEvent
}

func (n *earlyResetBehaviorNotifier) NotifyCodexEarlyReset(_ context.Context, f notifysvc.CodexAccountFence, expected, observed time.Time) (store.Notification, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, earlyResetBehaviorEvent{f, expected, observed})
	return store.Notification{}, nil
}

func earlyResetBehaviorReading(pct float64, reset time.Time) workersvc.CodexUsageReading {
	weekly, epoch := int64(604800), reset.Unix()
	return workersvc.CodexUsageReading{
		Buckets: []apitypes.CodexRateLimitBucketDTO{{
			ID: "codex", // Main bucket intentionally has no display name.
			Secondary: &apitypes.CodexRateLimitWindowDTO{
				UsedPercent: &pct, LimitWindowSeconds: &weekly, ResetAt: &epoch,
			},
		}},
		ObservedGeneration: 3, ObservedCredentialRevision: 7,
	}
}

func TestCodexEarlyResetBehavior(t *testing.T) {
	for _, arm := range []string{"forward_epoch_over_1h", "unchanged_epoch_20_to_0"} {
		for _, path := range []string{"Boot", "tick", "poke"} {
			t.Run(arm+"/"+path, func(t *testing.T) {
				ctx := context.Background()
				now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
				expected := now.Add(12 * time.Hour) // Still pending, strictly more than 8h early.
				nextReset, nextPct := expected, 0.0
				if arm == "forward_epoch_over_1h" {
					nextReset, nextPct = now.Add(7*24*time.Hour), 20
				}
				user, account := uuid.New(), uuid.New()
				// One canonical account representing two linked aliases, as returned by
				// the real deduplicating listing query (covered by M1 SQL tests).
				sig := "11111111-1111-4111-8111-111111111111:2,22222222-2222-4222-8222-222222222222:5"
				st := &earlyResetBehaviorStore{fakeStore: newFakeStore(), writeTime: now.Add(-5 * time.Minute)}
				row := linkedRow(user, account, 3, 7, "idle", false, false)
				row.EnablementSig = sig
				st.toPoll = []store.ListLinkedCodexAccountsToPollRow{row}
				st.forUser[user] = []store.ListLinkedCodexAccountsForUserRow{{
					UserID: user, ProviderAccountID: account, Generation: 3,
					CredentialRevision: 7, CoordState: "idle", EnablementSig: sig,
				}}
				first := earlyResetBehaviorReading(20, expected)
				second := earlyResetBehaviorReading(nextPct, nextReset)
				col := &fakeCollector{results: map[uuid.UUID]collectResult{account: {reading: first}}}
				notifications := &earlyResetBehaviorNotifier{}
				e := New(st, col, &fakeReconciler{}, 5*time.Minute, quietLogger())
				e.now = func() time.Time { return now.Add(-5 * time.Minute) }
				e.SetNotifier(notifications)

				poke := func() {
					e.Poke(user)
					select {
					case queued := <-e.poke:
						if queued != user {
							t.Fatalf("queued user = %s, want %s", queued, user)
						}
						e.pokeUser(ctx, queued)
					default:
						t.Fatal("poke was not queued")
					}
				}
				pass := func() {
					switch path {
					case "Boot":
						e.Boot(ctx)
					case "tick":
						e.tickAll(ctx)
					case "poke":
						poke()
					}
				}
				// Check accepted collection, write authority, and usable persisted
				// baseline BEFORE any missing-alert assertion.
				assertAccepted := func(want int, reading workersvc.CodexUsageReading, at time.Time) {
					t.Helper()
					if len(col.calls) != want || len(st.upserts) != want || len(st.failures) != 0 {
						t.Fatalf("collections/writes/failures = %d/%d/%d, want %d/%d/0", len(col.calls), len(st.upserts), len(st.failures), want, want)
					}
					for i, id := range col.calls {
						up := st.upserts[i]
						if id != account || up.UserID != user || up.ProviderAccountID != account ||
							up.ObservedGeneration != 3 || up.ObservedCredentialRevision != 7 ||
							up.EnablementSig != sig || up.AttemptStatus != attemptStatusOK {
							t.Fatalf("collection/write %d has wrong account or accepted authority: %s / %+v", i, id, up)
						}
					}
					b, err := st.GetCodexEarlyResetBaseline(ctx, store.GetCodexEarlyResetBaselineParams{
						UserID: user, ProviderAccountID: account, EnablementSig: sig,
					})
					if err != nil || b.UserID != user || b.ProviderAccountID != account ||
						!b.LastSuccessAt.Valid || !b.LastSuccessAt.Time.Equal(at) ||
						!b.EnablementSig.Valid || b.EnablementSig.String != sig || !b.NotifyEarlyLimitReset {
						t.Fatalf("accepted baseline missing comparison provenance: %+v, err=%v", b, err)
					}
					var buckets []apitypes.CodexRateLimitBucketDTO
					if err := json.Unmarshal(b.Buckets, &buckets); err != nil || !reflect.DeepEqual(buckets, reading.Buckets) {
						t.Fatalf("accepted baseline buckets = %s, want %+v (err=%v)", b.Buckets, reading.Buckets, err)
					}
				}
				pass()
				assertAccepted(1, first, now.Add(-5*time.Minute))
				if len(notifications.events) != 0 {
					t.Fatalf("first reading got %d notifications, want 0", len(notifications.events))
				}

				col.results[account] = collectResult{reading: second}
				st.writeTime = now
				e.now = func() time.Time { return now }
				pass()
				assertAccepted(2, second, now)
				if len(notifications.events) != 1 {
					t.Fatalf("got %d notifications, want 1", len(notifications.events))
				}
				wantFence := notifysvc.CodexAccountFence{
					OwnerID: user, AccountID: account, Generation: 3, CredentialRevision: 7, EnablementSig: sig,
				}
				event := notifications.events[0]
				if event.fence != wantFence || !event.expected.Equal(expected) || !event.observed.Equal(now) {
					t.Fatalf("account event = %+v, want fence %+v, expected %s, observed %s", event, wantFence, expected, now)
				}
				// Replaying the accepted reading through both common paths must
				// not turn the same account edge into multiple notifications.
				e.tickAll(ctx)
				poke()
				assertAccepted(4, second, now)
				if len(notifications.events) != 1 {
					t.Fatalf("repeat poll/poke got %d notifications, want 1", len(notifications.events))
				}
			})
		}
	}
}
