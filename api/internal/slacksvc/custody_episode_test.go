package slacksvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/vtmocanu/uzi/api/internal/notifysvc"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// custodyEpisodeFakeStore is a minimal custodyEpisodeStore for the reconciler unit tests. It
// models the two find-owners reads, the atomic claim (with a pre-burned set and a concurrency
// counter), the re-arm clear, and the frozen aggregate.
type custodyEpisodeFakeStore struct {
	overLimit   []uuid.UUID        // ListOwnersOverCustodyLimit result
	cleared     []uuid.UUID        // ListOwnersWithClearedCustodyEpisode result
	overErr     error              // forces the over-limit list to fail
	clearedErr  error              // forces the cleared list to fail
	claimed     map[uuid.UUID]bool // already-notified (claim returns pgx.ErrNoRows)
	claimErr    error              // forces every claim to fail (non-ErrNoRows)
	aggErr      error              // forces GetCustodyAggregateForOwner to fail
	agg         map[uuid.UUID]store.GetCustodyAggregateForOwnerRow
	claimOrder  []uuid.UUID // every claim ATTEMPT, in order
	clearedCall []uuid.UUID // every ClearCustodyEpisodeNotice call, in order
	limitSeen   []int32     // every custody_hold_limit param passed to the list reads
}

func (f *custodyEpisodeFakeStore) ListOwnersOverCustodyLimit(_ context.Context, limit int32) ([]uuid.UUID, error) {
	f.limitSeen = append(f.limitSeen, limit)
	if f.overErr != nil {
		return nil, f.overErr
	}
	return f.overLimit, nil
}

func (f *custodyEpisodeFakeStore) ListOwnersWithClearedCustodyEpisode(_ context.Context, limit int32) ([]uuid.UUID, error) {
	f.limitSeen = append(f.limitSeen, limit)
	if f.clearedErr != nil {
		return nil, f.clearedErr
	}
	return f.cleared, nil
}

func (f *custodyEpisodeFakeStore) ClaimCustodyEpisodeNotice(_ context.Context, userID uuid.UUID) (uuid.UUID, error) {
	f.claimOrder = append(f.claimOrder, userID)
	if f.claimErr != nil {
		return uuid.Nil, f.claimErr
	}
	if f.claimed[userID] {
		return uuid.Nil, pgx.ErrNoRows
	}
	if f.claimed == nil {
		f.claimed = map[uuid.UUID]bool{}
	}
	f.claimed[userID] = true // burn the slot so a second attempt in the same run sees ErrNoRows
	return userID, nil
}

func (f *custodyEpisodeFakeStore) ClearCustodyEpisodeNotice(_ context.Context, userID uuid.UUID) error {
	f.clearedCall = append(f.clearedCall, userID)
	if f.claimed != nil {
		delete(f.claimed, userID) // re-arm: a later crossing can claim afresh
	}
	return nil
}

func (f *custodyEpisodeFakeStore) GetCustodyAggregateForOwner(_ context.Context, arg store.GetCustodyAggregateForOwnerParams) (store.GetCustodyAggregateForOwnerRow, error) {
	if f.aggErr != nil {
		return store.GetCustodyAggregateForOwnerRow{}, f.aggErr
	}
	if row, ok := f.agg[arg.UserID]; ok {
		return row, nil
	}
	// Default: at the limit with no blocked runs.
	return store.GetCustodyAggregateForOwnerRow{OpenHolds: 8, BlockedRuns: 0}, nil
}

// custodyEpisodeFakeNotifier records every Notify.
type custodyEpisodeFakeNotifier struct {
	notifications []notifysvc.Notification
}

func (n *custodyEpisodeFakeNotifier) Notify(_ context.Context, notif notifysvc.Notification) (store.Notification, error) {
	n.notifications = append(n.notifications, notif)
	return store.Notification{}, nil
}

// custodyEpisodeFakeSettings is a static settings source.
type custodyEpisodeFakeSettings struct {
	enabled bool
	baseURL string
	enabErr error
}

func (s custodyEpisodeFakeSettings) HealthEnabled(context.Context) (bool, error) {
	return s.enabled, s.enabErr
}
func (s custodyEpisodeFakeSettings) PublicBaseURL(context.Context) (string, error) {
	return s.baseURL, nil
}

func newCustodyEpisodeReconciler(st *custodyEpisodeFakeStore, notif *custodyEpisodeFakeNotifier, set custodyEpisodeFakeSettings) *CustodyEpisodeReconciler {
	return NewCustodyEpisodeReconciler(st, notif, set, 8, nil)
}

// TestCustodyEpisodeCrossingSendsOneDM: one owner over the limit drives exactly one Notify with
// Kind custody_episode, a populated SlackRender carrying the aggregate facts, the deep link, and
// the fixed discard command — and the claim is recorded before the send.
func TestCustodyEpisodeCrossingSendsOneDM(t *testing.T) {
	uid := uuid.New()
	st := &custodyEpisodeFakeStore{
		overLimit: []uuid.UUID{uid},
		agg:       map[uuid.UUID]store.GetCustodyAggregateForOwnerRow{uid: {OpenHolds: 8, BlockedRuns: 3}},
	}
	notif := &custodyEpisodeFakeNotifier{}
	r := newCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: true, baseURL: "https://uzi.example.com"})

	r.Reconcile(context.Background())

	if len(notif.notifications) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notif.notifications))
	}
	n := notif.notifications[0]
	if n.UserID != uid {
		t.Errorf("notification UserID = %v, want %v", n.UserID, uid)
	}
	if n.Kind != KindCustodyEpisode {
		t.Errorf("notification Kind = %q, want %q", n.Kind, KindCustodyEpisode)
	}
	if n.RunID != nil || n.ReviewID != nil {
		t.Error("custody episode DM must be owner-scoped (no run/review anchor)")
	}
	if n.Slack == nil {
		t.Fatal("SlackRender is nil, want populated")
	}
	if n.Slack.Title == "" || n.Slack.Body == "" || n.Slack.Emoji == "" {
		t.Errorf("SlackRender under-populated: %+v", n.Slack)
	}
	if n.Slack.Link != "https://uzi.example.com/workers" {
		t.Errorf("Slack.Link = %q, want the /workers custody surface deep link", n.Slack.Link)
	}
	// Facts: open holds, blocked runs, and the exact discard command chip.
	if len(n.Slack.Facts) != 3 {
		t.Fatalf("Facts = %v, want 3 (holds + runs + command)", n.Slack.Facts)
	}
	if !strings.Contains(n.Slack.Facts[0], "8") || !strings.Contains(n.Slack.Facts[0], "sources") {
		t.Errorf("holds fact = %q, want plural 8 sources", n.Slack.Facts[0])
	}
	if !strings.Contains(n.Slack.Facts[1], "3") || !strings.Contains(n.Slack.Facts[1], "runs") {
		t.Errorf("runs fact = %q, want plural 3 runs", n.Slack.Facts[1])
	}
	if !strings.Contains(n.Slack.Facts[2], custodyDiscardCommand) {
		t.Errorf("command fact = %q, want the exact discard command %q", n.Slack.Facts[2], custodyDiscardCommand)
	}
	// The claim (mark) must have been recorded — claim-first before the send.
	if len(st.claimOrder) != 1 || st.claimOrder[0] != uid {
		t.Errorf("claim order = %v, want [%v]", st.claimOrder, uid)
	}
}

// TestCustodyEpisodeSecondReconcileSilent: a second reconcile in the SAME episode (the notice is
// already claimed) sends nothing — the at-most-once dedup.
func TestCustodyEpisodeSecondReconcileSilent(t *testing.T) {
	uid := uuid.New()
	st := &custodyEpisodeFakeStore{overLimit: []uuid.UUID{uid}}
	notif := &custodyEpisodeFakeNotifier{}
	r := newCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: true, baseURL: "https://uzi.example.com"})

	r.Reconcile(context.Background())
	r.Reconcile(context.Background()) // same episode: the mark from the first tick excludes it

	if len(notif.notifications) != 1 {
		t.Fatalf("Notify called %d times across two ticks, want 1 (episode dedup)", len(notif.notifications))
	}
}

// TestCustodyEpisodeAlreadyClaimedSkips: an owner whose episode is already claimed (pre-burned)
// is NOT notified, while a fresh over-limit owner is.
func TestCustodyEpisodeAlreadyClaimedSkips(t *testing.T) {
	fresh := uuid.New()
	already := uuid.New()
	st := &custodyEpisodeFakeStore{
		overLimit: []uuid.UUID{fresh, already},
		claimed:   map[uuid.UUID]bool{already: true}, // already marked → claim returns pgx.ErrNoRows
	}
	notif := &custodyEpisodeFakeNotifier{}
	r := newCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: true})

	r.Reconcile(context.Background())

	if len(notif.notifications) != 1 {
		t.Fatalf("Notify called %d times, want 1 (already-claimed owner skipped)", len(notif.notifications))
	}
	if notif.notifications[0].UserID != fresh {
		t.Errorf("notified %v, want the fresh owner %v", notif.notifications[0].UserID, fresh)
	}
}

// TestCustodyEpisodeReArmThenReCross: an owner who dropped below the limit is cleared, so a later
// re-crossing (same reconciler, next tick) notifies afresh. Drives the full episode cycle.
func TestCustodyEpisodeReArmThenReCross(t *testing.T) {
	uid := uuid.New()
	st := &custodyEpisodeFakeStore{
		overLimit: []uuid.UUID{uid},
		agg:       map[uuid.UUID]store.GetCustodyAggregateForOwnerRow{uid: {OpenHolds: 8, BlockedRuns: 1}},
	}
	notif := &custodyEpisodeFakeNotifier{}
	r := newCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: true, baseURL: "https://uzi.example.com"})

	// Tick 1: crossing → one DM, notice claimed.
	r.Reconcile(context.Background())
	if len(notif.notifications) != 1 {
		t.Fatalf("after tick 1: Notify = %d, want 1", len(notif.notifications))
	}

	// The owner drops below the limit: no longer over, but now in the cleared set.
	st.overLimit = nil
	st.cleared = []uuid.UUID{uid}

	// Tick 2: the episode closes → the notice is cleared (re-armed), no DM.
	r.Reconcile(context.Background())
	if len(notif.notifications) != 1 {
		t.Fatalf("after tick 2 (drop below): Notify = %d, want still 1", len(notif.notifications))
	}
	if len(st.clearedCall) != 1 || st.clearedCall[0] != uid {
		t.Fatalf("clear calls = %v, want [%v] (episode closed → re-arm)", st.clearedCall, uid)
	}

	// The owner re-crosses: over the limit again, cleared set empty.
	st.overLimit = []uuid.UUID{uid}
	st.cleared = nil

	// Tick 3: a fresh crossing after re-arm → a NEW DM.
	r.Reconcile(context.Background())
	if len(notif.notifications) != 2 {
		t.Fatalf("after tick 3 (re-cross): Notify = %d, want 2 (re-armed episode notifies afresh)", len(notif.notifications))
	}
}

// TestCustodyEpisodeDisabledIsNoop: HealthEnabled=false suppresses everything — no claim, no
// clear, no send — reusing the health enablement gate (no separate enable flag).
func TestCustodyEpisodeDisabledIsNoop(t *testing.T) {
	uid := uuid.New()
	st := &custodyEpisodeFakeStore{overLimit: []uuid.UUID{uid}, cleared: []uuid.UUID{uid}}
	notif := &custodyEpisodeFakeNotifier{}
	r := newCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: false})

	r.Reconcile(context.Background())

	if len(notif.notifications) != 0 {
		t.Errorf("Notify called %d times, want 0 when health notifications are disabled", len(notif.notifications))
	}
	if len(st.claimOrder) != 0 {
		t.Errorf("claim attempts = %d, want 0 when disabled (the enablement gate precedes any DB read)", len(st.claimOrder))
	}
	if len(st.limitSeen) != 0 {
		t.Errorf("list reads = %d, want 0 when disabled (no work at all)", len(st.limitSeen))
	}
}

// TestCustodyEpisodeAggregateErrorReArms: a GetCustodyAggregateForOwner failure AFTER a successful
// claim re-arms the notice (clears it) so a later tick retries, rather than burning the episode's
// one DM on a transient read error.
func TestCustodyEpisodeAggregateErrorReArms(t *testing.T) {
	uid := uuid.New()
	st := &custodyEpisodeFakeStore{overLimit: []uuid.UUID{uid}, aggErr: context.DeadlineExceeded}
	notif := &custodyEpisodeFakeNotifier{}
	r := newCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: true})

	r.Reconcile(context.Background())

	if len(notif.notifications) != 0 {
		t.Fatalf("Notify called %d times, want 0 on an aggregate read error", len(notif.notifications))
	}
	if len(st.claimOrder) != 1 {
		t.Fatalf("claim attempts = %d, want 1", len(st.claimOrder))
	}
	// The just-claimed slot must have been re-armed so the next tick retries.
	if len(st.clearedCall) != 1 || st.clearedCall[0] != uid {
		t.Fatalf("clear calls = %v, want [%v] (re-arm after aggregate error)", st.clearedCall, uid)
	}
}

// TestCustodyEpisodeListErrorNoOp: an over-limit list error is best-effort — logged, no panic, no
// Notify.
func TestCustodyEpisodeListErrorNoOp(t *testing.T) {
	st := &custodyEpisodeFakeStore{overErr: context.DeadlineExceeded}
	notif := &custodyEpisodeFakeNotifier{}
	r := newCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: true})

	r.Reconcile(context.Background()) // must not panic

	if len(notif.notifications) != 0 {
		t.Errorf("Notify called %d times, want 0 on a list error", len(notif.notifications))
	}
}

// TestCustodyEpisodeNonPositiveLimitNoop: a non-positive limit disables the admission gate, so the
// reconciler does no work at all (no list reads, no sends).
func TestCustodyEpisodeNonPositiveLimitNoop(t *testing.T) {
	st := &custodyEpisodeFakeStore{overLimit: []uuid.UUID{uuid.New()}}
	notif := &custodyEpisodeFakeNotifier{}
	r := NewCustodyEpisodeReconciler(st, notif, custodyEpisodeFakeSettings{enabled: true}, 0, nil)

	r.Reconcile(context.Background())

	if len(st.limitSeen) != 0 {
		t.Errorf("list reads = %d, want 0 with a non-positive limit", len(st.limitSeen))
	}
	if len(notif.notifications) != 0 {
		t.Errorf("Notify called %d times, want 0 with a non-positive limit", len(notif.notifications))
	}
}

// ── buildCustodyEpisodeNotification (pure) ─────────────────────────────────────

func TestBuildCustodyEpisodeNotificationFactsAndCommand(t *testing.T) {
	uid := uuid.New()
	n := buildCustodyEpisodeNotification("https://uzi.example.com/", uid, 8, 1)
	if n.Kind != KindCustodyEpisode {
		t.Errorf("Kind = %q, want %q", n.Kind, KindCustodyEpisode)
	}
	if n.Slack.Link != "https://uzi.example.com/workers" {
		t.Errorf("Link = %q, want trimmed base + /workers", n.Slack.Link)
	}
	if len(n.Slack.Facts) != 3 {
		t.Fatalf("Facts = %v, want 3", n.Slack.Facts)
	}
	// blocked_runs == 1 → singular "blocked run".
	if !strings.Contains(n.Slack.Facts[1], "blocked run") || strings.Contains(n.Slack.Facts[1], "blocked runs") {
		t.Errorf("runs fact = %q, want singular 'blocked run'", n.Slack.Facts[1])
	}
	// The command fact carries the EXACT fixed string with literal placeholders, in a code chip.
	if n.Slack.Facts[2] != "Discard held work: `uzi run discard <run-id> --hold <hold-id> --yes`" {
		t.Errorf("command fact = %q, want the exact fixed discard command chip", n.Slack.Facts[2])
	}
	// FACTS-ONLY: no free text carrying a run/hold uuid or any interpolated value.
	for _, f := range n.Slack.Facts {
		if strings.Contains(f, uid.String()) {
			t.Errorf("fact %q leaks the owner uuid — the DM must carry FACTS ONLY, not identifiers", f)
		}
	}
}

func TestBuildCustodyEpisodeNotificationEmptyBaseOmitsLink(t *testing.T) {
	n := buildCustodyEpisodeNotification("   ", uuid.New(), 9, 4)
	if n.Slack.Link != "" {
		t.Errorf("Link = %q, want empty for a blank base URL", n.Slack.Link)
	}
	// Plural on both counts.
	if !strings.Contains(n.Slack.Facts[0], "9") || !strings.Contains(n.Slack.Facts[0], "sources") {
		t.Errorf("holds fact = %q, want plural 9 sources", n.Slack.Facts[0])
	}
	if !strings.Contains(n.Slack.Facts[1], "4") || !strings.Contains(n.Slack.Facts[1], "blocked runs") {
		t.Errorf("runs fact = %q, want plural 4 blocked runs", n.Slack.Facts[1])
	}
}
