package store_test

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/vtmocanu/uzi/api/internal/store"
)

func TestManualCordonProjectionQueryBoundAndParity(t *testing.T) {
	raw, err := os.ReadFile("queries/runtime.sql")
	if err != nil {
		t.Fatal(err)
	}
	query := func(name string) string {
		_, body, ok := strings.Cut(string(raw), "-- name: "+name+" ")
		if !ok {
			t.Fatalf("missing %s", name)
		}
		start := strings.Index(body, "SELECT w.*,")
		if start < 0 {
			t.Fatalf("missing projection %s", name)
		}
		body = body[start:]
		return body[:strings.Index(body, ";")+1]
	}
	list := query("ListWorkersByUser")
	single := query("GetWorkerProjectionForOwner")
	want := strings.Replace(list, "WHERE w.user_id = @user_id\nORDER BY w.created_at ASC;", "WHERE w.user_id = @user_id AND w.id = @worker_id;", 1)
	if single != want {
		t.Fatal("single worker projection differs from full list diagnostics or lacks bounded owner tuple")
	}
}

func TestManualCordonProjectionOwnerLiveDB(t *testing.T) {
	f := newRollAffinityFixture(t)
	id := f.worker("projection", 0, false)
	run := f.pinnedRun(id, 0)
	mustExec(f.ctx, t, f.pool, "UPDATE runs SET status='running' WHERE id=$1", run)
	manualChange(t, f, id, true)
	row, err := f.q.GetWorkerProjectionForOwner(f.ctx, store.GetWorkerProjectionForOwnerParams{UserID: f.userID, WorkerID: id})
	if err != nil {
		t.Fatal(err)
	}
	if !row.Busy || row.ActiveRuns != 1 || !row.ManualCordonedAt.Valid {
		t.Fatalf("lost activity or manual metadata: %+v", row)
	}
	rows, err := f.q.ListWorkersByUser(f.ctx, f.userID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, listed := range rows {
		if listed.ID == id {
			found = true
			if !reflect.DeepEqual(store.ListWorkersByUserRow(row), listed) {
				t.Fatal("single projection lost list diagnostics")
			}
		}
	}
	if !found {
		t.Fatal("list control missing")
	}
	_, err = f.q.GetWorkerProjectionForOwner(f.ctx, store.GetWorkerProjectionForOwnerParams{UserID: uuid.New(), WorkerID: id})
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("foreign owner read: %v", err)
	}
}
