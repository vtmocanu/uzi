package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// TestNextClaimSnapshotLoadsTheWarningMargin: the margin rides the one reserve-settings read, with and
// without a reserve threshold, and reports the owner's value (0 included).
func TestNextClaimSnapshotLoadsTheWarningMargin(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  store.GetUserUsageReserveRow
		want int
		thr  *int16
	}{
		{"no reserve, default margin", store.GetUserUsageReserveRow{WeeklyWarningMarginPts: 15}, 15, nil},
		{"no reserve, margin off", store.GetUserUsageReserveRow{WeeklyWarningMarginPts: 0}, 0, nil},
		{"reserve and margin", store.GetUserUsageReserveRow{WeeklyReservePct: pgtype.Int2{Int16: 70, Valid: true}, WeeklyWarningMarginPts: 50}, 50, i16p(70)},
	} {
		e := newNCEnv(t, ncParams())
		e.setTokens(ncTok{id: ncTokA, label: "tok-a", pooled: true, weekly: 10})
		e.f.reserve = tc.row
		snap := e.snapshot(t)
		if snap.MarginPts != tc.want {
			t.Errorf("%s: MarginPts = %d, want %d", tc.name, snap.MarginPts, tc.want)
		}
		got := snap.ReserveThreshold()
		if (got == nil) != (tc.thr == nil) || (got != nil && *got != *tc.thr) {
			t.Errorf("%s: ReserveThreshold = %v, want %v", tc.name, got, tc.thr)
		}
	}
}
