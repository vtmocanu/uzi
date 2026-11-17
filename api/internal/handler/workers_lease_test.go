package handler

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/settings"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestEphemeralLeaseExpiry(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: now.Add(d), Valid: true}
	}
	none := pgtype.Timestamptz{}
	cases := []struct {
		name     string
		since    pgtype.Timestamptz
		draining pgtype.Timestamptz
		lease    time.Duration
		want     *time.Time
	}{
		{"no lease_since", none, none, 2 * time.Hour, nil},
		{"live lease", ts(-30 * time.Minute), none, 2 * time.Hour, ptrTime(now.Add(90 * time.Minute))},
		{"expired lease", ts(-3 * time.Hour), none, 2 * time.Hour, nil},
		{"expires exactly now", ts(-2 * time.Hour), none, 2 * time.Hour, nil},
		{"draining ends lease", ts(-30 * time.Minute), ts(-time.Minute), 2 * time.Hour, nil},
		{"lease disabled", ts(-time.Minute), none, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ephemeralLeaseExpiry(tc.since, tc.draining, tc.lease, now)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %v, want nil", *got)
			case tc.want != nil && (got == nil || !got.Equal(*tc.want)):
				t.Fatalf("got %v, want %v", got, *tc.want)
			}
		})
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// The lease expiry reaches the worker DTO's JSON only while the lease is live, and a
// cordon (draining_since) removes it.
func TestWorkerDTOEphemeralLeaseJSON(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	h := &Handler{now: func() time.Time { return now }}
	since := pgtype.Timestamptz{Time: now.Add(-30 * time.Minute), Valid: true}
	drain := pgtype.Timestamptz{Time: now.Add(-time.Minute), Valid: true}

	live := workerDTOFromWorker(store.Worker{LeaseSince: since}, 0, false, "", "", "", now, now)
	h.overlayEphemeralLease(&live, since, pgtype.Timestamptz{}, 2*time.Hour)
	b, _ := json.Marshal(live)
	if !strings.Contains(string(b), `"ephemeral_lease_expires_at":"2026-10-01T13:30:00Z"`) {
		t.Fatalf("live lease missing from JSON: %s", b)
	}

	cordoned := workerDTOFromWorker(store.Worker{LeaseSince: since}, 0, false, "", "", "", now, now)
	h.overlayEphemeralLease(&cordoned, since, drain, 2*time.Hour)
	b, _ = json.Marshal(cordoned)
	if strings.Contains(string(b), "ephemeral_lease_expires_at") {
		t.Fatalf("cordoned worker must omit the lease: %s", b)
	}
}

// The expiry a worker response shows follows the ephemeral_lease setting in force when the
// response is built (PRD #4212), not a boot snapshot: one handler, the setting edited between reads.
func TestWorkerEphemeralLeaseDisplayFollowsSettingChange(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	st := &settingsStore{rows: []store.AppSetting{{Key: settings.KeyEphemeralLease, Value: "2h"}}}
	sc := settings.New(st, time.Hour)
	h := &Handler{settings: sc, now: func() time.Time { return now }}
	since := pgtype.Timestamptz{Time: now.Add(-30 * time.Minute), Valid: true}
	expiry := func() *time.Time {
		t.Helper()
		dto := workerDTOFromWorker(store.Worker{LeaseSince: since}, 0, false, "", "", "", now, now)
		h.overlayEphemeralLease(&dto, since, pgtype.Timestamptz{}, h.currentEphemeralLease(context.Background()))
		return dto.EphemeralLeaseExpiresAt
	}
	edit := func(rows ...store.AppSetting) {
		st.rows, st.err = rows, nil
		sc.Invalidate()
	}

	if got := expiry(); got == nil || !got.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("lease 2h: expiry = %v, want %v", got, now.Add(90*time.Minute))
	}
	edit(store.AppSetting{Key: settings.KeyEphemeralLease, Value: "1h"})
	if got := expiry(); got == nil || !got.Equal(now.Add(30*time.Minute)) {
		t.Fatalf("lease 1h: expiry = %v, want %v", got, now.Add(30*time.Minute))
	}
	edit(store.AppSetting{Key: settings.KeyEphemeralLease, Value: "0s"})
	if got := expiry(); got != nil {
		t.Fatalf("lease 0: expiry = %v, want none", *got)
	}
	edit() // reset: the default lease (2h) applies again
	if got := expiry(); got == nil || !got.Equal(now.Add(90*time.Minute)) {
		t.Fatalf("lease reset: expiry = %v, want %v", got, now.Add(90*time.Minute))
	}
	// A failed read shows no expiry rather than failing the response or guessing a lease.
	st.rows, st.err = nil, errors.New("settings read failed")
	sc.Invalidate()
	if got := expiry(); got != nil {
		t.Fatalf("settings read error: expiry = %v, want none", *got)
	}
}
