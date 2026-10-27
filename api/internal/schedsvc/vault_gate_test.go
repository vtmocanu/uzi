package schedsvc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

// gateStateReader supplies durable state through Vault's public context seam.
// Its observations pin both the owner ID and propagation of the caller's context.
type gateStateReader struct {
	t     *testing.T
	owner uuid.UUID
	state store.GetUserVaultStateRow
	err   error
	calls int
}

type gateContextKey struct{}

func (r *gateStateReader) GetUserVaultState(ctx context.Context, id uuid.UUID) (store.GetUserVaultStateRow, error) {
	r.t.Helper()
	if id != r.owner || ctx.Value(gateContextKey{}) != r {
		r.t.Fatalf("vault lookup lost owner or context: owner=%v", id)
	}
	r.calls++
	return r.state, r.err
}

func actualEmptyVaultGate(t *testing.T, owner uuid.UUID, mode string, err error) (*vault.Vault, context.Context, *gateStateReader) {
	t.Helper()
	reader := &gateStateReader{t: t, owner: owner, state: store.GetUserVaultStateRow{
		VaultMode: mode, VaultEpoch: 3,
	}, err: err}
	ctx := context.WithValue(context.Background(), gateContextKey{}, reader)
	ctx = vault.WithStateReader(ctx, reader)
	v := vault.New(nil, nil, func(context.Context, func(context.Context, vault.Store) error) error {
		t.Fatal("vault gate unexpectedly opened a lifecycle transaction")
		return errors.New("unexpected lifecycle transaction")
	})
	return v, ctx, reader
}

func TestTickSelfImproveActualVaultGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		err     error
		blocked bool
	}{
		{name: "off empty cache fires", mode: store.VaultModeUnprotected},
		{name: "protected empty cache skips", mode: store.VaultModeProtected, blocked: true},
		{name: "state error fails closed", mode: store.VaultModeUnprotected, err: errors.New("state unavailable"), blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness()
			v, ctx, reader := actualEmptyVaultGate(t, h.owner, tc.mode, tc.err)
			h.sched.vault = v
			h.st.due = []store.RunSchedule{h.selfImproveSchedule()}
			if v.Unlocked(h.owner) {
				t.Fatal("fixture must have no cached DEK, including Off mode")
			}

			h.sched.Boot(ctx)

			wantRuns, wantSkipped := 1, 0
			if tc.blocked {
				wantRuns, wantSkipped = 0, 1
			}
			if len(h.runs.selfImprove) != wantRuns || h.fb.f.createCount != wantRuns {
				t.Fatalf("run/forge creates = %d/%d, want %d/%d", len(h.runs.selfImprove), h.fb.f.createCount, wantRuns, wantRuns)
			}
			if got := h.countKind("selfimprove_skipped"); got != wantSkipped {
				t.Fatalf("skip notices = %d, want %d", got, wantSkipped)
			}
			skips := lastFireSkips(t, h)
			if tc.blocked {
				if len(skips) != 1 || skips[0].Reason != string(SkipVaultLocked) {
					t.Fatalf("skips = %+v, want vault_locked", skips)
				}
			} else if len(skips) != 0 {
				t.Fatalf("Off fire recorded skips: %+v", skips)
			}
			if h.st.advanceCalls[0].Status != "active" || len(h.st.statusCalls) != 0 {
				t.Fatalf("cadence did not remain active: advance=%+v status=%+v", h.st.advanceCalls, h.st.statusCalls)
			}
			if reader.calls == 0 {
				t.Fatal("scheduler did not consult durable vault state")
			}
		})
	}
}

func TestTickSelfImproveNilVaultGateFires(t *testing.T) {
	h := newHarness()
	h.sched.vault = nil
	h.st.due = []store.RunSchedule{h.selfImproveSchedule()}

	h.sched.Boot(context.Background())

	if len(h.runs.selfImprove) != 1 || h.fb.f.createCount != 1 || h.countKind("selfimprove_skipped") != 0 {
		t.Fatal("nil scheduler gate must permit the run and forge work without a skip notice")
	}
	if skips := lastFireSkips(t, h); len(skips) != 0 {
		t.Fatalf("nil scheduler gate recorded skips: %+v", skips)
	}
}

func TestVaultLockReconcileActualVaultGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		err        error
		wantNotice bool
	}{
		{name: "off candidate never claimed", mode: store.VaultModeUnprotected},
		{name: "protected locked positive control", mode: store.VaultModeProtected, wantNotice: true},
		{name: "state error fails closed", mode: store.VaultModeUnprotected, err: errors.New("state unavailable"), wantNotice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := uuid.New()
			v, ctx, reader := actualEmptyVaultGate(t, owner, tc.mode, tc.err)
			st := &fakeStore{vaultNoticeUsers: []store.ListUsersNeedingVaultLockNoticeRow{
				{ID: owner, PendingRuns: 1},
			}}
			notifier := &fakeNotifier{}
			rec := newVaultLockReconciler(st, notifier, &fakeSettings{}, v)
			if v.Unlocked(owner) {
				t.Fatal("fixture must have no cached DEK")
			}

			rec.Reconcile(ctx)

			want := 0
			if tc.wantNotice {
				want = 1
			}
			if len(st.vaultClaimedOrder) != want || len(notifier.notifications) != want {
				t.Fatalf("claims/notices = %d/%d, want %d/%d", len(st.vaultClaimedOrder), len(notifier.notifications), want, want)
			}
			if reader.calls == 0 {
				t.Fatal("reconciler did not consult durable vault state")
			}
		})
	}
}

func TestVaultLockReconcileModeAwareGate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gate       *fakeVault
		wantNotice bool
	}{
		{name: "off raw cache absent", gate: &fakeVault{unprotectedMode: true}},
		{name: "protected unlocked", gate: &fakeVault{unlocked: true}},
		{name: "protected locked", gate: &fakeVault{}, wantNotice: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := uuid.New()
			st := &fakeStore{vaultNoticeUsers: []store.ListUsersNeedingVaultLockNoticeRow{
				{ID: owner, PendingSchedules: 1},
			}}
			notifier := &fakeNotifier{}
			rec := newVaultLockReconciler(st, notifier, &fakeSettings{}, tc.gate)

			rec.Reconcile(context.Background())

			want := 0
			if tc.wantNotice {
				want = 1
			}
			if len(st.vaultClaimedOrder) != want || len(notifier.notifications) != want {
				t.Fatalf("claims/notices = %d/%d, want %d/%d", len(st.vaultClaimedOrder), len(notifier.notifications), want, want)
			}
		})
	}
}
