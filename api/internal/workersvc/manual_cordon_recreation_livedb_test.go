package workersvc

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestManualCordonServiceRecreationLiveDB(t *testing.T) {
	e := setupInterlockLiveDB(t)
	worker := e.seedWorker(t, nil)
	firstService := e.permitService(t)
	initial, _, err := firstService.Register(e.ctx, store.Worker{ID: worker, UserID: e.userID}, "v1", "base", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "Operator", pgtype.Text{String: "rotation", Valid: true}, true)
	if err != nil {
		t.Fatal(err)
	}
	e.exec(t, "UPDATE workers SET kind='hosted',hosted_size='m',template_declared='base' WHERE id=$1", worker)
	if _, err = e.q.CordonHostedWorkerImageRoll(e.ctx, worker); err != nil {
		t.Fatal(err)
	}
	// A new Service, its locks and query object all read the same durable DB.
	e.q = store.New(e.pool)
	recreated := e.permitService(t)
	if recreated == firstService {
		t.Fatal("service was reused")
	}
	prior, err := e.q.GetWorkerByID(e.ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	registered, nonce, err := recreated.Register(e.ctx, prior, "v2", "base", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := e.q.GetWorkerByID(e.ctx, worker)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []store.Worker{registered, persisted} {
		if !got.ManualCordonedAt.Valid || !got.ManualCordonedAt.Time.Equal(first.ManualCordonedAt.Time) || got.ManualCordonedBy != first.ManualCordonedBy || got.ManualCordonedByLabel != first.ManualCordonedByLabel || got.ManualCordonReason != first.ManualCordonReason || got.DrainingSince.Valid || got.DrainingReason.Valid || store.WorkerAcceptsNewIntake(got) {
			t.Fatal("recreated registration lost manual intent or failed to clear image roll")
		}
	}
	if nonce == "" || nonce == initial.SnapshotRegisterNonce.String {
		t.Fatal("registration did not create a fresh incarnation")
	}
	if _, err = store.ChangeWorkerManualCordon(e.ctx, e.pool, e.userID, worker, "Operator", pgtype.Text{}, false); err != nil {
		t.Fatal(err)
	}
	persisted, err = e.q.GetWorkerByID(e.ctx, worker)
	if err != nil || !store.WorkerAcceptsNewIntake(persisted) {
		t.Fatalf("release after recreation: %v", err)
	}
}
