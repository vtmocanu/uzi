package handler

// Issue #4198 regression proof for GET /api/recovery/holds?user=<other>: a live admin's
// admin_ro token must be served the TARGET user's holds. Before the change the handler
// ignored ?user= and answered with the caller's own holds. Written against symbols present
// on both the base and the candidate; the only candidate-only line is the AdminRunDebug flag.

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/apitypes"
	"github.com/vtmocanu/uzi/api/internal/clitoken"
)

func TestHoldsByUserRegressionLiveDB(t *testing.T) {
	h, router, pool := cliLiveDB(t)
	// Keeps the base (which lacks the flag line below) compiling.
	_ = h
	h.cfg.AdminRunDebug = true // candidate-only line: the flag does not exist on the base

	admin := cliSeedUser(t, pool, true)
	target := cliSeedUser(t, pool, false)
	adminRO := cliMintToken(t, pool, admin, clitoken.ScopeAdminRO)

	conn := rmSeedConn(t, pool, target)
	worker := custodySeedWorker(t, pool, target)
	_, hold1 := custodySeedRunHold(t, pool, target, conn, worker, 8101, "failed", 1)
	_, hold2 := custodySeedRunHold(t, pool, target, conn, worker, 8102, "failed", 1)

	rec := bearerReq(router, http.MethodGet, "/api/recovery/holds?user="+target.String(), adminRO)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin_ro GET holds?user=target = %d, want 200\nbody: %s", rec.Code, rec.Body.String())
	}
	var dto apitypes.RecoveryCustodyHoldsDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode holds: %v (body %s)", err, rec.Body.String())
	}
	got := []string{}
	for _, hd := range dto.Holds {
		got = append(got, hd.ID)
	}
	want := []string{hold1.String(), hold2.String()}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) || (len(got) == 2 && (got[0] != want[0] || got[1] != want[1])) {
		t.Fatalf("hold ids = %v, want the target's holds %v (the caller's own holds were returned instead?)", got, want)
	}
}
