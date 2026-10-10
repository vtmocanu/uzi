package store_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/vtmocanu/uzi/api/internal/store"
)

// #2705 M3: ListWaitingWorkerRuns feeds the queue.waiting age alarm. A run held for its returning
// worker (health_reason 'waiting until ...') is expected waiting and must be excluded, while a
// waiting_worker row with a NULL health_reason (a bare NOT LIKE would drop it) and one with an
// unrelated reason stay included. Against a REAL Postgres; skipped unless UZI_TEST_DATABASE_URL
// points at a throwaway one (e2e/run-store-it.sh provides it).
func TestListWaitingWorkerRunsExcludesStaleRequeuePinLiveDB(t *testing.T) {
	fx := newFleetFixture(t)

	insert := func(reason any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, health, health_reason, health_since)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'waiting_worker', $5, now())`,
			id, fx.userID, fx.repoID, fx.nextIID(), reason)
		t.Cleanup(func() {
			if _, err := fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id = $1`, id); err != nil {
				t.Errorf("cleanup run %v: %v", id, err)
			}
		})
		return id
	}
	pin := insert("waiting until 2026-07-12T12:19:00Z for its previous worker w1 to return; another worker may take it after that")
	null := insert(nil)
	other := insert("no worker is online to pick up this run")

	rows, err := fx.q.ListWaitingWorkerRuns(fx.ctx)
	if err != nil {
		t.Fatalf("ListWaitingWorkerRuns: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.RunID] = true
	}
	if got[pin] {
		t.Error("the stale-requeue pin row is listed; want it excluded from the queue.waiting population")
	}
	if !got[null] {
		t.Error("the NULL health_reason row is missing; want it included")
	}
	if !got[other] {
		t.Error("the unrelated-reason row is missing; want it included")
	}
}

// #2705 M3: ListOwnersWaitingNoCapacity feeds the fleet.capacity alarm. health_since is stamped at
// the requeue, so a run held for its returning worker (health_reason 'waiting until ...') must be
// excluded or the alarm would fire inside the grace. A NULL-reason row and an unrelated-reason row
// of an owner with zero usable workers stay included. The fixture user owns no worker, so it has
// zero usable workers; the result is filtered to the fixture's runs.
func TestListOwnersWaitingNoCapacityExcludesStaleRequeuePinLiveDB(t *testing.T) {
	fx := newFleetFixture(t)

	insert := func(reason any) uuid.UUID {
		t.Helper()
		id := uuid.New()
		mustExec(fx.ctx, t, fx.pool,
			`INSERT INTO runs (id, user_id, repo_id, issue_iid, issue_title, issue_description, status, health, health_reason, health_since)
			 VALUES ($1, $2, $3, $4, 't', 'd', 'queued', 'waiting_worker', $5, now())`,
			id, fx.userID, fx.repoID, fx.nextIID(), reason)
		t.Cleanup(func() {
			if _, err := fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id = $1`, id); err != nil {
				t.Errorf("cleanup run %v: %v", id, err)
			}
		})
		return id
	}
	pin := insert("waiting until 2026-07-12T12:19:00Z for its previous worker w1 to return; another worker may take it after that")
	null := insert(nil)
	other := insert("no worker is online to pick up this run")

	rows, err := fx.q.ListOwnersWaitingNoCapacity(fx.ctx, store.ListOwnersWaitingNoCapacityParams{
		RollReason:      "no roll reason in this test",
		HeartbeatCutoff: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	})
	if err != nil {
		t.Fatalf("ListOwnersWaitingNoCapacity: %v", err)
	}
	got := map[uuid.UUID]bool{}
	for _, r := range rows {
		got[r.RunID] = true
	}
	if got[pin] {
		t.Error("the stale-requeue pin row is listed; want it excluded from the fleet.capacity population")
	}
	if !got[null] {
		t.Error("the NULL health_reason row is missing; want it included")
	}
	if !got[other] {
		t.Error("the unrelated-reason row is missing; want it included")
	}
}
