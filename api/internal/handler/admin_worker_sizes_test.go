package handler

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestEvaluateAdminSizesGate(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	adv := func(version int32, ago time.Duration, poll int32) store.GetControllerAdminSizesAdvertisementRow {
		return store.GetControllerAdminSizesAdvertisementRow{
			AdminSizesVersion:      pgtype.Int4{Int32: version, Valid: true},
			AdminSizesAdvertisedAt: pgtype.Timestamptz{Time: now.Add(-ago), Valid: true},
			PollIntervalSeconds:    pgtype.Int4{Int32: poll, Valid: true},
		}
	}
	cases := []struct {
		name string
		row  store.GetControllerAdminSizesAdvertisementRow
		err  error
		want string // "" = editable
	}{
		{"no row", store.GetControllerAdminSizesAdvertisementRow{}, pgx.ErrNoRows, readOnlyNoReport},
		{"row without an advertisement", store.GetControllerAdminSizesAdvertisementRow{PollIntervalSeconds: pgtype.Int4{Int32: 3, Valid: true}}, nil, readOnlyNoReport},
		{"fresh v1", adv(1, time.Second, 3), nil, ""},
		{"fresh v2", adv(2, time.Second, 3), nil, ""},
		{"fresh v0", adv(0, time.Second, 3), nil, readOnlyUnsupported},
		{"stale v1", adv(1, 7*time.Second, 3), nil, readOnlyStale},
		{"stale v0 reads as stale", adv(0, time.Minute, 3), nil, readOnlyStale},
		{"just inside the window", adv(1, 5*time.Second, 3), nil, ""},
		{"exactly 2x poll is stale", adv(1, 6*time.Second, 3), nil, readOnlyStale},
		{"clamped: a huge advertised poll does not keep a report fresh forever", adv(1, 601*time.Second, 100000), nil, readOnlyStale},
		{"a read failure is not editable", store.GetControllerAdminSizesAdvertisementRow{}, errors.New("boom"), readOnlyNoReport},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := evaluateAdminSizesGate(tc.row, tc.err, now)
			if tc.want == "" {
				if !g.Editable || g.Code != "" || g.Reason != "" {
					t.Fatalf("gate = %+v, want editable", g)
				}
				return
			}
			if g.Editable || g.Code != tc.want || g.Reason == "" {
				t.Fatalf("gate = %+v, want read-only %s", g, tc.want)
			}
		})
	}
}
