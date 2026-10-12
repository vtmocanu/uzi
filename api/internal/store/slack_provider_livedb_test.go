package store_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// TestGetSlackRunContextProviderParkLiveDB executes the provider projection using
// the existing fixture, which skips without UZI_TEST_DATABASE_URL.
func TestGetSlackRunContextProviderParkLiveDB(t *testing.T) {
	fx := newWPFixture(t)
	statusSince := time.Date(2026, 10, 1, 10, 0, 0, 123456000, time.UTC)
	streakStarted := statusSince.Add(-2 * time.Hour)
	providerRetry := statusSince.Add(7 * time.Minute)
	usageRetry := statusSince.Add(19 * time.Minute)
	for _, tc := range []struct {
		name, status, cause, holdReason string
		providerRetry                   bool
	}{
		{"provider outage", "recovery_wait", "provider_outage", "", true},
		{"provider unavailable hold", "paused", "provider_outage", "provider_unavailable", false},
		{"generic recovery wait with retained streak", "recovery_wait", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := fx.run(wpRun{status: tc.status, holdReason: tc.holdReason})
			var retry any
			if tc.providerRetry {
				retry = providerRetry
			}
			mustExec(fx.ctx, t, fx.pool, `UPDATE runs SET
    recovery_wait_cause = $2, recovery_retry_not_before = $3,
    provider_park_count = $4, provider_park_streak_started_at = $5,
    status_since = $6, retry_not_before = $7
    WHERE id = $1`, id, nullText(tc.cause), retry, int32(3), streakStarted, statusSince, usageRetry)
			row, err := fx.q.GetSlackRunContext(fx.ctx, id)
			if err != nil {
				t.Fatalf("GetSlackRunContext: %v", err)
			}
			if row.Status != tc.status {
				t.Errorf("status = %q, want %q", row.Status, tc.status)
			}
			if row.RecoveryWaitCause.Valid != (tc.cause != "") || row.RecoveryWaitCause.String != tc.cause {
				t.Errorf("recovery_wait_cause = %+v, want %q (valid=%t)", row.RecoveryWaitCause, tc.cause, tc.cause != "")
			}
			if row.HoldReason.Valid != (tc.holdReason != "") || row.HoldReason.String != tc.holdReason {
				t.Errorf("hold_reason = %+v, want %q (valid=%t)", row.HoldReason, tc.holdReason, tc.holdReason != "")
			}
			if row.ProviderParkCount != 3 {
				t.Errorf("provider_park_count = %d, want 3", row.ProviderParkCount)
			}
			for _, stamp := range []struct {
				name  string
				got   pgtype.Timestamptz
				want  time.Time
				valid bool
			}{
				{"recovery_retry_not_before", row.RecoveryRetryNotBefore, providerRetry, tc.providerRetry},
				{"retry_not_before", row.RetryNotBefore, usageRetry, true},
				{"provider_park_streak_started_at", row.ProviderParkStreakStartedAt, streakStarted, true},
				{"status_since", row.StatusSince, statusSince, true},
			} {
				if stamp.got.Valid != stamp.valid || (stamp.valid &&
					(stamp.got.InfinityModifier != pgtype.Finite || !stamp.got.Time.Equal(stamp.want))) {
					t.Errorf("%s = %+v, want %s (valid=%t)", stamp.name, stamp.got, stamp.want.Format(time.RFC3339Nano), stamp.valid)
				}
			}
		})
	}
}
