package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// requestStoppedY files a second owner switch (Y) after X was withdrawn and
// returns Y's operation id.
func requestStoppedY(t *testing.T, e *applyRouteEnv) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	sealed, err := e.handler.box.Seal([]byte("harmless-stopped-y-" + id.String()))
	if err != nil {
		t.Fatal(err)
	}
	stoppedRouteExec(t, e, `INSERT INTO user_secrets(id,user_id,kind,label,ciphertext,sealed_with)
 VALUES($1,$2,'openai_api_key',$3,$4,'master')`, id, e.user, "stopped-y-"+id.String(), sealed)
	if _, err := e.handler.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: id, UserID: e.user, Status: "static"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.handler.wsvc.SetRunCredential(ctx, e.user, e.run, workersvc.BindModePinned, &id); err != nil {
		t.Fatal(err)
	}
	var op uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT credential_switch_op_id FROM runs WHERE id=$1 AND credential_switch_requested_at IS NOT NULL`, e.run).Scan(&op); err != nil {
		t.Fatal(err)
	}
	if op == e.op {
		t.Fatal("Y reused X's operation id")
	}
	return op
}

func withdrawStoppedOp(e *applyRouteEnv, w stoppedRouteWire, op uuid.UUID) int {
	rec := e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state",
		fmt.Sprintf(`{"status":"credential_switch_failed","claim_generation":%d,"register_nonce":%q,"op_id":%q}`, w.ClaimGeneration, w.RegisterNonce, op))
	return rec.Code
}

// A stop latched from X's READY settles after X is withdrawn and a later Y is
// requested and withdrawn: neither withdrawal changes claim, incarnation or
// epoch (the latched values the predicate compares). A pending Y refuses it, and
// a delayed withdrawal naming X cannot discharge Y.
func TestWorkerCodexStoppedWithdrawnXYRouteLiveDB(t *testing.T) {
	t.Run("xy_settles", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		y := requestStoppedY(t, e)
		if code := withdrawStoppedOp(e, w, y); code != http.StatusOK {
			t.Fatalf("Y withdraw status=%d", code)
		}
		reportStoppedRoute(t, e, w, true, true)
		reportStoppedRoute(t, e, w, true, false)
	})
	t.Run("y_pending", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		requestStoppedY(t, e)
		reportStoppedRoute(t, e, w, false, false)
	})
	t.Run("stale_x_withdraw", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		requestStoppedY(t, e)
		before := stoppedRouteSnapshot(t, e)
		if code := withdrawStoppedOp(e, w, e.op); code != http.StatusConflict {
			t.Fatalf("delayed X withdraw status=%d; want 409", code)
		}
		assertStoppedSnapshot(t, before, stoppedRouteSnapshot(t, e), false)
		reportStoppedRoute(t, e, w, false, false)
	})
}
