package store_test

import (
	"testing"

	"github.com/google/uuid"
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
		t.Cleanup(func() { _, _ = fx.pool.Exec(fx.ctx, `DELETE FROM runs WHERE id = $1`, id) })
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
