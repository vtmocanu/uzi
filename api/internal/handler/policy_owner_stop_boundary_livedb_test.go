package handler

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestPolicyOwnerStopPresentedHeaderLiveDB(t *testing.T) {
	e := newWallParkExtendHTTPEnv(t, 57600)
	pool, err := store.OpenPool(context.Background(), os.Getenv("UZI_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	e.h.wsvc = workersvc.New(e.race, nil, workersvc.Params{RunTimeout: 16 * time.Hour})
	e.h.wsvc.SetTxBeginner(pool)
	id := e.providerHold(1)
	e.exec("UPDATE runs SET harness='codex',hold_reason='provider_policy_refusal',policy_refusal_count=1,policy_last_outcome_id=$2 WHERE id=$1", id, uuid.New())
	held, err := e.race.GetRunByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	call := func(token string) int {
		return e.post(func(w http.ResponseWriter, r *http.Request) {
			r.Header.Set("If-Match", token)
			e.h.CreateRunInput(w, r)
		}, id, `{"kind":"stop","body":"finish only"}`).Code
	}
	for _, token := range []string{"", "stale"} {
		if code := call(token); code != http.StatusConflict {
			t.Fatalf("token=%q status=%d", token, code)
		}
	}
	if code := call(workersvc.PolicyHoldActionToken(held)); code != http.StatusAccepted {
		t.Fatalf("presented Stop status=%d", code)
	}
	got, err := e.race.GetRunByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "queued" || !got.ScopeCeiling.Valid || got.ScopeCeiling.Int32 != 1 || got.PolicyRefusalCount != 1 {
		t.Fatalf("Stop did not preserve and resume: status=%s ceiling=%v count=%d", got.Status, got.ScopeCeiling, got.PolicyRefusalCount)
	}
}
