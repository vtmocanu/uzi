package store_test

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestManualUncordonAuditAtomicityLiveDB(t *testing.T) {
	for _, failure := range []string{"event", "before-commit", "lost-commit-response"} {
		t.Run(failure, func(t *testing.T) {
			f := newRollAffinityFixture(t)
			w := f.worker("uncordon-atomic", 0, false)
			first := manualChange(t, f, w, true)
			p := &manualFaultPool{pool: f.pool, eventFailure: failure == "event", commitBefore: failure == "before-commit", commitAfter: failure == "lost-commit-response"}
			_, err := store.ChangeWorkerManualCordon(f.ctx, p, f.userID, w, "Release operator", pgtype.Text{String: "must not replace previous reason", Valid: true}, false)
			if !errors.Is(err, errCordonFault) {
				t.Fatalf("fault not reported: %v", err)
			}
			committed := failure == "lost-commit-response"
			expected := 1
			if committed {
				expected = 2
			}
			if manualEventCount(t, f, w) != expected || manualRead(t, f, w).ManualCordonedAt.Valid == committed {
				t.Fatal("uncordon state/event atomicity broken")
			}
			if !committed {
				kept := manualRead(t, f, w)
				if !kept.ManualCordonedAt.Time.Equal(first.ManualCordonedAt.Time) || kept.ManualCordonReason != first.ManualCordonReason {
					t.Fatal("failed uncordon changed original intent")
				}
			}
			for range 2 {
				manualChange(t, f, w, false)
			}
			if manualEventCount(t, f, w) != 2 || manualRead(t, f, w).ManualCordonedAt.Valid {
				t.Fatal("retry/repeat duplicated transition or retained cordon")
			}
			var reason string
			if err := f.pool.QueryRow(f.ctx, "SELECT reason FROM worker_cordon_events WHERE worker_id=$1 AND action='uncordon'", w).Scan(&reason); err != nil {
				t.Fatal(err)
			}
			if reason != "rotation" {
				t.Fatalf("uncordon reason=%q want previous rotation", reason)
			}
		})
	}
}

func TestManualCordonAuditDeletionSnapshotLiveDB(t *testing.T) {
	f := newRollAffinityFixture(t)
	w := f.worker("audit-snapshot", 0, false)
	first := manualChange(t, f, w, true)
	mustExec(f.ctx, t, f.pool, "UPDATE users SET display_name='Renamed operator' WHERE id=$1", f.userID)
	if got := manualRead(t, f, w); got.ManualCordonedByLabel != first.ManualCordonedByLabel {
		t.Fatal("rename rewrote worker actor snapshot")
	}
	manualChange(t, f, w, false)
	// Compare the full event rows, including snapshot IDs, labels, reason and time.
	snapshot := func() string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(f.ctx, "SELECT jsonb_agg(to_jsonb(e) ORDER BY occurred_at,id)::text FROM worker_cordon_events e WHERE worker_id=$1", w).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	var label string
	if err := f.pool.QueryRow(f.ctx, "SELECT actor_label FROM worker_cordon_events WHERE worker_id=$1 AND action='cordon'", w).Scan(&label); err != nil {
		t.Fatal(err)
	}
	if label != "Operator" {
		t.Fatalf("rename changed event snapshot: %q", label)
	}
	before := snapshot()
	f.deleteWorker(w)
	mustExec(f.ctx, t, f.pool, "DELETE FROM users WHERE id=$1", f.userID)
	if manualEventCount(t, f, w) != 2 || snapshot() != before {
		t.Fatal("worker/account deletion changed audit history")
	}
}
