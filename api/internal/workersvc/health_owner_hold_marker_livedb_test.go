package workersvc

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// Issue #4507: the episode-keyed owner-hold marker through the real store queries and the real
// ListActiveRunsForHealth projection. A fake store cannot show that the SQL comparison, the guard on
// MarkRunHealthOwnerHold or the NULL health_since case behave as the detector assumes.
func TestHealthOwnerHoldMarkerLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	t.Cleanup(func() {
		if _, err := e.pool.Exec(e.ctx, "DELETE FROM runs WHERE user_id=$1", e.userID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	const reasonA = "held for worker A: wait for it or cancel the run; only the worker that last ran this Codex run may resume it"
	text := func(s string) pgtype.Text { return pgtype.Text{String: s, Valid: true} }
	ts := func(tm time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: tm, Valid: true} }
	setHealth := func(t *testing.T, id uuid.UUID, since time.Time, mark bool) {
		t.Helper()
		n, err := e.q.SetRunHealth(e.ctx, store.SetRunHealthParams{
			Health: healthWaitingWorker, HealthReason: text(reasonA), HealthSince: ts(since),
			MarkOwnerHold: mark, ID: id, Status: "queued",
		})
		if err != nil || n != 1 {
			t.Fatalf("SetRunHealth n=%d err=%v", n, err)
		}
	}
	active := func(t *testing.T, id uuid.UUID) bool {
		t.Helper()
		return e.healthRowFor(t, id).HealthOwnerHoldActive
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)

	t.Run("SetRunHealth with the mark makes it active", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base, false)
		if active(t, id) {
			t.Fatal("active before any mark")
		}
		setHealth(t, id, base, true)
		if !active(t, id) {
			t.Fatal("not active after SetRunHealth with MarkOwnerHold")
		}
	})

	t.Run("MarkRunHealthOwnerHold marks an unmarked episode", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base, false)
		n, err := e.q.MarkRunHealthOwnerHold(e.ctx, store.MarkRunHealthOwnerHoldParams{
			ID: id, Status: "queued", Health: healthWaitingWorker, HealthReason: text(reasonA), HealthSince: ts(base),
		})
		if err != nil || n != 1 {
			t.Fatalf("mark n=%d err=%v, want 1", n, err)
		}
		if !active(t, id) {
			t.Fatal("not active after MarkRunHealthOwnerHold")
		}
	})

	t.Run("a mark for another episode or reason affects no row", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base, false)
		for name, p := range map[string]store.MarkRunHealthOwnerHoldParams{
			"stale since":      {ID: id, Status: "queued", Health: healthWaitingWorker, HealthReason: text(reasonA), HealthSince: ts(base.Add(-time.Minute))},
			"different reason": {ID: id, Status: "queued", Health: healthWaitingWorker, HealthReason: text("other"), HealthSince: ts(base)},
			"different status": {ID: id, Status: "running", Health: healthWaitingWorker, HealthReason: text(reasonA), HealthSince: ts(base)},
			"null since":       {ID: id, Status: "queued", Health: healthWaitingWorker, HealthReason: text(reasonA)},
		} {
			if n, err := e.q.MarkRunHealthOwnerHold(e.ctx, p); err != nil || n != 0 {
				t.Fatalf("%s: n=%d err=%v, want 0", name, n, err)
			}
		}
		if active(t, id) {
			t.Fatal("active after only rejected marks")
		}
	})

	t.Run("a NULL health_since projects inactive and scans", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base, true)
		e.exec(t, `UPDATE runs SET health_since = NULL WHERE id = $1`, id)
		if active(t, id) {
			t.Fatal("active with NULL health_since")
		}
	})

	t.Run("a new episode without the mark is inactive", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base, true)
		setHealth(t, id, base.Add(time.Minute), false)
		if active(t, id) {
			t.Fatal("marker for the previous episode still active")
		}
	})

	t.Run("a later write for the same episode without the mark keeps it active", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base, true)
		setHealth(t, id, base, false)
		if !active(t, id) {
			t.Fatal("marker lost by a same-episode SetRunHealth without MarkOwnerHold")
		}
	})

	t.Run("a mark for another health or a NULL reason affects no row", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base, false)
		for name, p := range map[string]store.MarkRunHealthOwnerHoldParams{
			"different health": {ID: id, Status: "queued", Health: healthStalled, HealthReason: text(reasonA), HealthSince: ts(base)},
			"null reason":      {ID: id, Status: "queued", Health: healthWaitingWorker, HealthSince: ts(base)},
		} {
			if n, err := e.q.MarkRunHealthOwnerHold(e.ctx, p); err != nil || n != 0 {
				t.Fatalf("%s: n=%d err=%v, want 0", name, n, err)
			}
		}
		if active(t, id) {
			t.Fatal("active after only rejected marks")
		}
	})

	t.Run("a nanosecond health_since round-trips to active", func(t *testing.T) {
		id := e.seedQueuedRun(t, nil, nil)
		setHealth(t, id, base.Add(123*time.Nanosecond), true)
		if !active(t, id) {
			t.Fatal("not active for a sub-microsecond health_since")
		}
	})
}
