package workersvc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/runkind"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// #4540 M2 characterization: a run whose frozen provider binding is fully populated but carries a key
// mode other than user_key is refused as stale by all three claim fences, before any revision lookup,
// key lookup or key open. Today every non-user_key mode is stale.

// modeFenceStore counts the binding reads an assembleProviderBinding call makes. Every read answers
// "no rows", so a fence that wrongly proceeded would still end in a refusal; the counters (and the
// embedded fakeStore's byIDLookups, the vault ciphertext read) are what prove nothing was reached.
type modeFenceStore struct {
	*fakeStore
	revisionLookups int
	keyLookups      int
}

func (m *modeFenceStore) GetModelProviderRevisionForOwner(context.Context, store.GetModelProviderRevisionForOwnerParams) (store.GetModelProviderRevisionForOwnerRow, error) {
	m.revisionLookups++
	return store.GetModelProviderRevisionForOwnerRow{}, pgx.ErrNoRows
}

func (m *modeFenceStore) GetProviderBindingKeyByID(context.Context, store.GetProviderBindingKeyByIDParams) (store.GetProviderBindingKeyByIDRow, error) {
	m.keyLookups++
	return store.GetProviderBindingKeyByIDRow{}, pgx.ErrNoRows
}

// modeFenceLocker is the in-transaction locker handed to lockedProviderRefusal; it counts every call.
type modeFenceLocker struct{ calls int }

func (l *modeFenceLocker) LockProviderBindingKeyForShareNowait(context.Context, store.LockProviderBindingKeyForShareNowaitParams) (store.LockProviderBindingKeyForShareNowaitRow, error) {
	l.calls++
	return store.LockProviderBindingKeyForShareNowaitRow{}, pgx.ErrNoRows
}

func (l *modeFenceLocker) LockProviderBindingRevisionForKeyShareNowait(context.Context, store.LockProviderBindingRevisionForKeyShareNowaitParams) (store.LockProviderBindingRevisionForKeyShareNowaitRow, error) {
	l.calls++
	return store.LockProviderBindingRevisionForKeyShareNowaitRow{}, pgx.ErrNoRows
}

func TestNonUserKeyModeIsStaleAtEveryClaimFence(t *testing.T) {
	cases := []struct{ name, mode string }{
		{"operator_key (baseline pre-M2 rejection)", "operator_key"},
		{"unknown mode", "bogus_mode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := providerBoundRow(runkind.Issue)
			run.Harness = "claude"
			run.ModelProviderKeyMode = pgtype.Text{String: c.mode, Valid: true}
			// Fully populated apart from the mode: every other field the shape check reads is valid.
			if !run.ModelProviderID.Valid || !run.ModelProviderRevision.Valid || !run.ModelProviderKeySecretID.Valid || !run.ModelProviderKeyRev.Valid {
				t.Fatal("fixture binding is not fully populated")
			}

			if id, ok := frozenProviderKeyID(run); ok {
				t.Errorf("frozenProviderKeyID = (%v, true), want ok=false", id)
			}

			fs := &fakeStore{}
			ms := &modeFenceStore{fakeStore: fs}
			svc := New(ms, newBox(t), testParams())
			svc.p.ProviderAllowedBaseURLs = []string{pvBase}

			locker := &modeFenceLocker{}
			reason, err := svc.lockedProviderRefusal(context.Background(), locker, run)
			if err != nil || reason != holdProviderBindingStale {
				t.Errorf("lockedProviderRefusal = (%q, %v), want (%q, nil)", reason, err, holdProviderBindingStale)
			}
			if locker.calls != 0 {
				t.Errorf("locked re-check made %d lock reads, want 0", locker.calls)
			}

			wkr := store.Worker{ProtocolCapabilities: []string{capability.ClaudeProviderV1}}
			_, err = svc.assembleProviderBinding(context.Background(), wkr, run)
			var refusal *providerClaimRefusal
			if !errors.As(err, &refusal) || refusal.Reason != holdProviderBindingStale || !errors.Is(err, ErrProviderBindingStale) {
				t.Errorf("assembleProviderBinding err = %v, want a *providerClaimRefusal provider_binding_stale", err)
			}
			if ms.revisionLookups != 0 || ms.keyLookups != 0 || len(fs.byIDLookups) != 0 {
				t.Errorf("lookups: revision=%d key=%d keyOpens=%d, want all 0", ms.revisionLookups, ms.keyLookups, len(fs.byIDLookups))
			}
		})
	}
}
