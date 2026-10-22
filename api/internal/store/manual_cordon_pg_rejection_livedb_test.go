package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

// A real PostgreSQL CHECK rejection, rather than the Exec wrapper used by the
// uncertain-commit tests, must roll the transition back with its event.
func TestManualCordonPostgresAuditRejectionLiveDB(t *testing.T) {
	for _, cordon := range []bool{true, false} {
		t.Run(fmt.Sprintf("cordon=%v", cordon), func(t *testing.T) {
			f := newRollAffinityFixture(t)
			w := f.worker("pg-audit-rejection", 0, false)
			before := 0
			if !cordon {
				manualChange(t, f, w, true)
				before = 1
			}
			constraint := "m1_reject_" + fmt.Sprintf("%x", w[:])
			// The worker UUID is generated locally; no trigger or audit read API is added.
			sql := fmt.Sprintf("ALTER TABLE worker_cordon_events ADD CONSTRAINT %s CHECK (worker_id <> '%s'::uuid) NOT VALID", constraint, w)
			if _, err := f.pool.Exec(f.ctx, sql); err != nil {
				t.Fatal(err)
			}
			drop := func() {
				t.Helper()
				if _, err := f.pool.Exec(f.ctx, "ALTER TABLE worker_cordon_events DROP CONSTRAINT IF EXISTS "+constraint); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(drop)
			_, err := store.ChangeWorkerManualCordon(f.ctx, f.pool, f.userID, w, "Retry", pgtype.Text{String: "replacement", Valid: true}, cordon)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || pgerr.Code != "23514" || pgerr.ConstraintName != constraint {
				t.Fatalf("want actual PostgreSQL audit CHECK rejection, got %v", err)
			}
			if got := manualRead(t, f, w).ManualCordonedAt.Valid; got == cordon {
				t.Fatal("audit insert rejection committed worker transition")
			}
			if got := manualEventCount(t, f, w); got != before {
				t.Fatalf("rejected transition events=%d want=%d", got, before)
			}
			drop()
			manualChange(t, f, w, cordon)
			manualChange(t, f, w, cordon)
			if got := manualEventCount(t, f, w); got != before+1 {
				t.Fatalf("retry/no-op events=%d want=%d", got, before+1)
			}
			if got := manualRead(t, f, w).ManualCordonedAt.Valid; got != cordon {
				t.Fatal("retry did not establish requested state")
			}
		})
	}
}
