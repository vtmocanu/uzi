package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vtmocanu/uzi/api/internal/clitoken"
	mw "github.com/vtmocanu/uzi/api/internal/middleware"
)

func TestWorkerCodexOwnerLiveRequestRouteLiveDB(t *testing.T) {
	for _, status := range []string{"running", "awaiting_approval", "awaiting_input", "awaiting_followup"} {
		t.Run(status, func(t *testing.T) {
			e, w := setupStoppedGuardsRouteWithSeed(t, true)
			ctx := context.Background()
			// Read A and B from the Codex binding and pending target. e.secret
			// is the fixture's ordinary Anthropic override, not target B.
			var current, target uuid.UUID
			var revision int64
			if err := e.pool.QueryRow(ctx, `SELECT r.codex_secret_id,
 r.codex_switch_target_secret_id, a.material_revision
 FROM runs r JOIN codex_credential_state a ON a.user_secret_id=r.codex_switch_target_secret_id
 WHERE r.id=$1`, e.run).Scan(&current, &target, &revision); err != nil {
				t.Fatal(err)
			}
			if current == uuid.Nil || target == uuid.Nil || current == target || target == e.secret {
				t.Fatal("fixture requires distinct current A and Codex target B")
			}
			oldOp := e.op
			// The real worker failure clears X without reporting a stopped pause.
			withdrawStoppedRoute(t, e, w)
			stoppedRouteExec(t, e, "UPDATE runs SET status=$2 WHERE id=$1", e.run, status)

			lim := mw.NewLimiter(100000, time.Minute, nil)
			router := e.handler.Routes(lim, lim, lim, lim, lim, lim, lim, lim, lim, lim, lim)
			token := cliMintToken(t, e.pool, e.user, clitoken.ScopeUser)
			before := stoppedRouteSnapshot(t, e)
			var runBefore map[string]json.RawMessage
			if err := json.Unmarshal(before["run"], &runBefore); err != nil {
				t.Fatal(err)
			}
			encode := func(value any) json.RawMessage {
				t.Helper()
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal("encode expected snapshot value")
				}
				return raw
			}
			for key, want := range map[string]json.RawMessage{
				"status":                         encode(status),
				"codex_secret_id":                encode(current),
				"codex_claim_epoch":              encode(int64(41)),
				"claim_generation":               encode(w.ClaimGeneration),
				"worker_id":                      encode(e.worker),
				"credential_switch_requested_at": encode(nil),
				"credential_switch_generation":   encode(nil),
				"credential_switch_op_id":        encode(nil),
				"codex_switch_target_secret_id":  encode(nil),
			} {
				if !reflect.DeepEqual(runBefore[key], want) {
					t.Fatalf("fixture precondition differs in %s", key)
				}
			}

			rec := bearerReqBody(router, http.MethodPost,
				"/api/runs/"+e.run.String()+"/credential", token,
				fmt.Sprintf(`{"mode":"pinned","secret_id":%q}`, target))
			if rec.Code != http.StatusOK {
				t.Fatalf("owner credential request status=%d, want 200", rec.Code)
			}
			after := stoppedRouteSnapshot(t, e)
			var runAfter map[string]json.RawMessage
			if err := json.Unmarshal(after["run"], &runAfter); err != nil {
				t.Fatal(err)
			}
			var newOp uuid.UUID
			if err := json.Unmarshal(runAfter["credential_switch_op_id"], &newOp); err != nil {
				t.Fatal("new operation ID is not a UUID")
			}
			if newOp == uuid.Nil || newOp == oldOp {
				t.Fatal("owner request did not create a fresh operation distinct from X")
			}
			var requestedAt string
			if err := json.Unmarshal(runAfter["credential_switch_requested_at"], &requestedAt); err != nil {
				t.Fatal("pending request timestamp is not a string")
			}
			if _, err := time.Parse(time.RFC3339Nano, requestedAt); err != nil {
				t.Fatal("pending request timestamp is invalid")
			}

			// Exact writes of StampHeldCodexCredentialSwitch and
			// SetPendingCodexSwitchTarget, plus runs_credential_switch_op_id.
			// Every other run field, including capability, session, binding and
			// ordinary override, retains its pre-request value.
			expectedPending := map[string]json.RawMessage{
				"credential_switch_requested_at":        encode(requestedAt),
				"credential_switch_generation":          encode(w.ClaimGeneration),
				"credential_switch_op_id":               encode(newOp),
				"codex_switch_target_secret_id":         encode(target),
				"codex_switch_target_generation":        encode(w.ClaimGeneration),
				"codex_switch_target_op_id":             encode(newOp),
				"codex_switch_target_auth_mode":         encode("api_key"),
				"codex_switch_target_material_revision": encode(revision),
				"codex_switch_target_account_key":       encode(nil),
				"codex_switch_target_account_revision":  encode(nil),
			}
			for key, want := range expectedPending {
				if !reflect.DeepEqual(runAfter[key], want) {
					t.Fatalf("pending request differs in %s", key)
				}
				runBefore[key] = want
			}
			if len(runBefore) != len(runAfter) {
				t.Fatal("run snapshot field count changed")
			}
			for key, want := range runBefore {
				if !reflect.DeepEqual(runAfter[key], want) {
					t.Fatalf("owner request changed unrelated run field %s", key)
				}
			}
			// Keep the full snapshot comparison: both credential journals,
			// messages, inputs and custody must remain unchanged.
			before["run"] = after["run"]
			if len(before) != len(after) {
				t.Fatal("snapshot section count changed")
			}
			for key, want := range before {
				if !reflect.DeepEqual(after[key], want) {
					t.Fatalf("owner request changed snapshot section %s", key)
				}
			}
		})
	}
}
