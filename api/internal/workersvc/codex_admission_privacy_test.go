package workersvc

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestCodexAdmissionUpgradeBlockerPrivacy(t *testing.T) {
	eligible := time.Date(2026, 11, 1, 12, 34, 56, 0, time.UTC)
	for _, class := range []string{"rate_limit", "overload", "ordinary"} {
		t.Run(class, func(t *testing.T) {
			reason, want := "waiting: maintenance", "waiting: maintenance"
			if class != "ordinary" {
				reason = apitypes.CodexAdmissionReason(class, eligible)
				want = "waiting: admission held"
			}
			owner, worker, run := uuid.New(), uuid.New(), uuid.New()
			b := GroupUpgradeBlockers([]store.ListWorkerUpgradeBlockersRow{{
				WorkerID: worker, WorkerOwner: owner, SafeCount: 1,
				RunID:  pgtype.UUID{Bytes: run, Valid: true},
				Status: blockerText("queued"), Kind: blockerText("chat"),
				HealthReason: blockerText(reason), HoldReason: blockerText("credential_disabled"),
			}})[worker]
			got := b.Summary()
			for _, private := range []string{"provider reported", "rate limit", "overload", eligible.Format(time.RFC3339)} {
				if strings.Contains(got, private) {
					t.Errorf("blocker summary leaked %q", private)
				}
			}
			for _, text := range []string{run.String(), "recorded reason " + want, "credential_disabled", "exact fence unavailable", "assignment releases only when the run ends or its assignment is removed"} {
				if !strings.Contains(got, text) {
					t.Errorf("summary missing %q: %s", text, got)
				}
			}
			if b.Owner != owner || b.Count != 1 || b.Unavailable || len(b.Runs) != 1 {
				t.Fatalf("blocker metadata changed: %+v", b)
			}
		})
	}
}
