package schedsvc

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// newVaultLockReconciler wires a reconciler over the shared M1 test fakes. A nil vault
// means "no local gate" (every user treated as locked); pass a *fakeVault to exercise
// the vlt.Unlocked skip.
func newVaultLockReconciler(st *fakeStore, notif *fakeNotifier, set *fakeSettings, vlt VaultGate) *VaultLockReconciler {
	return NewVaultLockReconciler(st, vlt, notif, set, nil)
}

// TestVaultLockReconcilePersistFirstSend: one eligible locked user drives exactly one
// Notify with Kind vault_locked, a populated SlackRender, and the expected Facts.
func TestVaultLockReconcilePersistFirstSend(t *testing.T) {
	uid := uuid.New()
	st := &fakeStore{vaultNoticeUsers: []store.ListUsersNeedingVaultLockNoticeRow{
		{ID: uid, PendingRuns: 2, PendingSchedules: 1},
	}}
	notif := &fakeNotifier{}
	set := &fakeSettings{publicBaseURL: "https://uzi.example.com"}
	r := newVaultLockReconciler(st, notif, set, nil)

	r.Reconcile(context.Background())

	if len(notif.notifications) != 1 {
		t.Fatalf("Notify called %d times, want 1", len(notif.notifications))
	}
	n := notif.notifications[0]
	if n.UserID != uid {
		t.Errorf("notification UserID = %v, want %v", n.UserID, uid)
	}
	if n.Kind != KindVaultLocked {
		t.Errorf("notification Kind = %q, want %q", n.Kind, KindVaultLocked)
	}
	if n.RunID != nil || n.ReviewID != nil {
		t.Error("vault-lock notice must be user-scoped (no run/review anchor)")
	}
	if n.Slack == nil {
		t.Fatal("SlackRender is nil, want populated")
	}
	if n.Slack.Title == "" || n.Slack.Body == "" || n.Slack.Emoji == "" {
		t.Errorf("SlackRender under-populated: %+v", n.Slack)
	}
	if strings.Contains(strings.ToLower(n.Slack.Body), "restart") {
		t.Errorf("body must be cause-neutral, got %q", n.Slack.Body)
	}
	if n.Slack.Link != "https://uzi.example.com" {
		t.Errorf("Slack.Link = %q, want the base URL", n.Slack.Link)
	}
	if len(n.Slack.Facts) != 2 {
		t.Fatalf("Facts = %v, want 2 (runs + schedules)", n.Slack.Facts)
	}
	if !strings.Contains(n.Slack.Facts[0], "2") || !strings.Contains(n.Slack.Facts[0], "runs") {
		t.Errorf("runs fact = %q, want plural 2 runs", n.Slack.Facts[0])
	}
	if !strings.Contains(n.Slack.Facts[1], "1") || !strings.Contains(n.Slack.Facts[1], "job") ||
		strings.Contains(n.Slack.Facts[1], "jobs") {
		t.Errorf("schedules fact = %q, want singular 1 job", n.Slack.Facts[1])
	}
	// The claim (mark) must be recorded — persist/claim-first before the send.
	if len(st.vaultClaimedOrder) != 1 || st.vaultClaimedOrder[0] != uid {
		t.Errorf("claim order = %v, want [%v]", st.vaultClaimedOrder, uid)
	}
}

// TestVaultLockReconcileAtomicClaimDedup: two users in the list, both claimed once; a
// user already burned (pre-seeded in vaultClaimed) yields pgx.ErrNoRows and is NOT notified.
func TestVaultLockReconcileAtomicClaimDedup(t *testing.T) {
	fresh := uuid.New()
	already := uuid.New()
	st := &fakeStore{
		vaultNoticeUsers: []store.ListUsersNeedingVaultLockNoticeRow{
			{ID: fresh, PendingRuns: 1},
			{ID: already, PendingRuns: 1},
		},
		vaultClaimed: map[uuid.UUID]bool{already: true}, // already marked → claim returns pgx.ErrNoRows
	}
	notif := &fakeNotifier{}
	r := newVaultLockReconciler(st, notif, &fakeSettings{}, nil)

	r.Reconcile(context.Background())

	if len(notif.notifications) != 1 {
		t.Fatalf("Notify called %d times, want 1 (already-claimed user skipped)", len(notif.notifications))
	}
	if notif.notifications[0].UserID != fresh {
		t.Errorf("notified %v, want the fresh user %v", notif.notifications[0].UserID, fresh)
	}
}

// TestVaultLockReconcileUnlockedSkip: a user in the list who is unlocked in this process
// (the seed admin) is skipped — no claim, no Notify.
func TestVaultLockReconcileUnlockedSkip(t *testing.T) {
	seedAdmin := uuid.New()
	locked := uuid.New()
	st := &fakeStore{vaultNoticeUsers: []store.ListUsersNeedingVaultLockNoticeRow{
		{ID: seedAdmin, PendingRuns: 1},
		{ID: locked, PendingSchedules: 1},
	}}
	notif := &fakeNotifier{}
	vlt := &fakeVault{unlockedSet: map[uuid.UUID]bool{seedAdmin: true}} // seedAdmin unlocked, locked not
	r := newVaultLockReconciler(st, notif, &fakeSettings{}, vlt)

	r.Reconcile(context.Background())

	if len(notif.notifications) != 1 || notif.notifications[0].UserID != locked {
		t.Fatalf("notifications = %+v, want exactly the locked user %v", notif.notifications, locked)
	}
	// The skipped seed admin must not have been claimed at all.
	for _, id := range st.vaultClaimedOrder {
		if id == seedAdmin {
			t.Error("unlocked seed admin was claimed — the vlt.Unlocked skip must precede the claim")
		}
	}
}

// TestVaultLockReconcileEmptyBaseOmitsLink: an empty PublicBaseURL yields no deep link,
// but the notification still sends (Notify still called).
func TestVaultLockReconcileEmptyBaseOmitsLink(t *testing.T) {
	uid := uuid.New()
	st := &fakeStore{vaultNoticeUsers: []store.ListUsersNeedingVaultLockNoticeRow{
		{ID: uid, PendingRuns: 1},
	}}
	notif := &fakeNotifier{}
	r := newVaultLockReconciler(st, notif, &fakeSettings{publicBaseURL: ""}, nil)

	r.Reconcile(context.Background())

	if len(notif.notifications) != 1 {
		t.Fatalf("Notify called %d times, want 1 (still sends without a base URL)", len(notif.notifications))
	}
	if link := notif.notifications[0].Slack.Link; link != "" {
		t.Errorf("Slack.Link = %q, want empty (no base URL)", link)
	}
}

// TestVaultLockReconcileListErrorNoOp: a list error is best-effort — logged, no panic,
// no Notify.
func TestVaultLockReconcileListErrorNoOp(t *testing.T) {
	st := &fakeStore{vaultNoticeUsersErr: context.DeadlineExceeded}
	notif := &fakeNotifier{}
	r := newVaultLockReconciler(st, notif, &fakeSettings{}, nil)

	r.Reconcile(context.Background())

	if len(notif.notifications) != 0 {
		t.Errorf("Notify called %d times, want 0 on a list error", len(notif.notifications))
	}
}

// TestVaultLockReconcileClaimErrorContinues: a non-ErrNoRows claim error on one user is
// logged and skipped without aborting the loop — the next user still proceeds.
func TestVaultLockReconcileClaimErrorContinues(t *testing.T) {
	u1, u2 := uuid.New(), uuid.New()
	st := &fakeStore{
		vaultNoticeUsers: []store.ListUsersNeedingVaultLockNoticeRow{
			{ID: u1, PendingRuns: 1},
			{ID: u2, PendingRuns: 1},
		},
		vaultClaimErr: context.DeadlineExceeded,
	}
	notif := &fakeNotifier{}
	r := newVaultLockReconciler(st, notif, &fakeSettings{}, nil)

	r.Reconcile(context.Background()) // must not panic

	if len(notif.notifications) != 0 {
		t.Errorf("Notify called %d times, want 0 when the claim errors", len(notif.notifications))
	}
	// Both users' claims must have been ATTEMPTED: the first user's claim error
	// is logged and skipped with continue (not return), so the loop reaches the
	// second user. A return in place of continue would leave this at 1.
	if len(st.vaultClaimedOrder) != 2 {
		t.Errorf("claim attempts = %d, want 2 (the loop must continue past a failed claim)", len(st.vaultClaimedOrder))
	}
}

// ── buildVaultLockNotification (pure) ─────────────────────────────────────────

func TestBuildVaultLockNotificationBasePresent(t *testing.T) {
	uid := uuid.New()
	n := buildVaultLockNotification("https://uzi.example.com/", uid, 3, 0)
	if n.Kind != KindVaultLocked {
		t.Errorf("Kind = %q, want %q", n.Kind, KindVaultLocked)
	}
	if n.Slack.Link != "https://uzi.example.com" {
		t.Errorf("Link = %q, want trimmed base URL", n.Slack.Link)
	}
	// pendingSchedules == 0 → its Fact is omitted; only the runs Fact remains.
	if len(n.Slack.Facts) != 1 {
		t.Fatalf("Facts = %v, want 1 (0-count schedule fact omitted)", n.Slack.Facts)
	}
	if !strings.Contains(n.Slack.Facts[0], "3") || !strings.Contains(n.Slack.Facts[0], "runs") {
		t.Errorf("runs fact = %q, want plural 3 runs", n.Slack.Facts[0])
	}
}

func TestBuildVaultLockNotificationBaseEmpty(t *testing.T) {
	n := buildVaultLockNotification("   ", uuid.New(), 1, 1)
	if n.Slack.Link != "" {
		t.Errorf("Link = %q, want empty for a blank base URL", n.Slack.Link)
	}
	if len(n.Slack.Facts) != 2 {
		t.Fatalf("Facts = %v, want 2", n.Slack.Facts)
	}
	// Singular wording for both (the count is mrkdwn-bolded, e.g. "*1* queued run").
	if !strings.Contains(n.Slack.Facts[0], "queued run") || strings.Contains(n.Slack.Facts[0], "runs") {
		t.Errorf("runs fact = %q, want singular 'queued run'", n.Slack.Facts[0])
	}
	if !strings.Contains(n.Slack.Facts[1], "scheduled job") || strings.Contains(n.Slack.Facts[1], "jobs") {
		t.Errorf("schedules fact = %q, want singular 'scheduled job'", n.Slack.Facts[1])
	}
}
