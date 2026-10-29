package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestUpgradeBlockerIdleStallTiming(t *testing.T) {
	for _, tc := range []struct {
		name  string
		retry pgtype.Timestamptz
		want  string
	}{
		{"held", pgtype.Timestamptz{}, "no timer"},
		{"timed", pgtype.Timestamptz{Valid: true, Time: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, "automatic retry not before"},
		{"due", pgtype.Timestamptz{Valid: true, Time: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}, "automatic retry not before"},
		{"infinite", pgtype.Timestamptz{Valid: true, InfinityModifier: pgtype.Infinity}, "retry timing unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action := blockerAction(store.ListWorkerUpgradeBlockersRow{Status: blockerText("recovery_wait"), RecoveryWaitCause: blockerText("codex_idle_stall"), RecoveryRetryNotBefore: tc.retry})
			if !strings.Contains(action, tc.want) || !strings.Contains(action, "codex_idle_stall") {
				t.Fatal(action)
			}
			if tc.name != "infinite" && (!strings.Contains(action, "fresh wall") || !strings.Contains(action, "reset idle-stall count") || !strings.Contains(action, "Cancel")) {
				t.Fatal(action)
			}
		})
	}
}
