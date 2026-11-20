package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func postStoppedFields(t *testing.T, e *applyRouteEnv, worker uuid.UUID, fields map[string]any) (int, bool) {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state", string(body))
	var response struct {
		Applied bool `json:"applied"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &response)
	return rec.Code, response.Applied
}

func stoppedFields(w stoppedRouteWire) map[string]any {
	return map[string]any{"status": w.Status, "claim_generation": w.ClaimGeneration,
		"register_nonce": w.RegisterNonce, "expected_epoch": w.ExpectedEpoch}
}

// The worker latches claim_generation, expected_epoch and register_nonce from
// preflight. Out-of-bounds values are refused as malformed (400) before any
// lookup; the largest safe integer and a 128-byte nonce pass the bounds and are
// refused by the predicate instead (409).
func TestWorkerCodexStoppedAuthorityBoundsRouteLiveDB(t *testing.T) {
	const maxSafe = int64(9007199254740991)
	cases := []struct {
		name   string
		mutate func(map[string]any)
		want   int
	}{
		{"generation_zero", func(f map[string]any) { f["claim_generation"] = 0 }, http.StatusBadRequest},
		{"generation_negative", func(f map[string]any) { f["claim_generation"] = -1 }, http.StatusBadRequest},
		{"generation_unsafe", func(f map[string]any) { f["claim_generation"] = maxSafe + 1 }, http.StatusBadRequest},
		{"generation_max_safe", func(f map[string]any) { f["claim_generation"] = maxSafe }, http.StatusConflict},
		{"epoch_zero", func(f map[string]any) { f["expected_epoch"] = 0 }, http.StatusBadRequest},
		{"epoch_negative", func(f map[string]any) { f["expected_epoch"] = -1 }, http.StatusBadRequest},
		{"epoch_unsafe", func(f map[string]any) { f["expected_epoch"] = maxSafe + 1 }, http.StatusBadRequest},
		{"epoch_max_safe", func(f map[string]any) { f["expected_epoch"] = maxSafe }, http.StatusConflict},
		{"nonce_empty", func(f map[string]any) { f["register_nonce"] = "" }, http.StatusBadRequest},
		{"nonce_over_128", func(f map[string]any) { f["register_nonce"] = strings.Repeat("n", 129) }, http.StatusBadRequest},
		{"nonce_128", func(f map[string]any) { f["register_nonce"] = strings.Repeat("n", 128) }, http.StatusConflict},
		{"nonce_nul", func(f map[string]any) { f["register_nonce"] = f["register_nonce"].(string) + "\x00" }, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, w := setupStoppedGuardsRoute(t)
			withdrawStoppedRoute(t, e, w)
			fields := stoppedFields(w)
			tc.mutate(fields)
			before := stoppedRouteSnapshot(t, e)
			code, applied := postStoppedFields(t, e, e.worker, fields)
			if code != tc.want || applied {
				t.Fatalf("bounds mismatch: status=%d applied=%t; want %d", code, applied, tc.want)
			}
			assertStoppedSnapshot(t, before, stoppedRouteSnapshot(t, e), false)
		})
	}
}

// Ownership, the reconciliation-path generation fence, statuses outside the
// live set and binding ownership, each on a running run after withdrawal.
func TestWorkerCodexStoppedOwnershipRouteLiveDB(t *testing.T) {
	t.Run("reconciled_generation_mismatch", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		insertStoppedCutoff(t, e, "stop")
		w.ClaimGeneration++
		reportStoppedRoute(t, e, w, false, false)
	})
	t.Run("reconciled_claim_released", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		insertStoppedCutoff(t, e, "stop")
		stoppedRouteExec(t, e, "UPDATE runs SET claim_released_at=now() WHERE id=$1", e.run)
		reportStoppedRoute(t, e, w, false, false)
	})
	t.Run("foreign_worker", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		exec := func(sql string, args ...any) { stoppedRouteExec(t, e, sql, args...) }
		other := e.seedWorker(t, exec, []string{"codex_switch_stopped_v1"})
		exec("UPDATE workers SET snapshot_register_nonce='foreign-incarnation', last_heartbeat_at=now() WHERE id=$1", other)
		w.RegisterNonce = "foreign-incarnation"
		before := stoppedRouteSnapshot(t, e)
		code, applied := postStoppedFields(t, e, other, stoppedFields(w))
		if applied || (code != http.StatusNotFound && code != http.StatusConflict) {
			t.Fatalf("foreign worker refusal mismatch: status=%d applied=%t", code, applied)
		}
		assertStoppedSnapshot(t, before, stoppedRouteSnapshot(t, e), false)
	})
	for _, status := range []string{"queued", "limit_wait", "pool_wait", "recovery_wait", "completed", "failed", "cancelled"} {
		t.Run("status_"+status, func(t *testing.T) {
			e, w := setupStoppedGuardsRoute(t)
			withdrawStoppedRoute(t, e, w)
			stoppedRouteExec(t, e, "UPDATE runs SET status=$2 WHERE id=$1", e.run, status)
			reportStoppedRoute(t, e, w, false, false)
		})
	}
	// The predicate checks binding presence only; ownership is the composite
	// (user_id, codex_secret_id) foreign key, so a foreign binding cannot be stored.
	t.Run("foreign_binding", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		foreignUser, foreignSecret := uuid.New(), uuid.New()
		stoppedRouteExec(t, e, "INSERT INTO users (id, email, password_hash, vault_mode) VALUES ($1, $2, 'x', 'unprotected')",
			foreignUser, "stopped-foreign-"+foreignUser.String()+"@e2e")
		stoppedRouteExec(t, e, "INSERT INTO user_secrets(id,user_id,kind,label,ciphertext,sealed_with) VALUES($1,$2,'openai_api_key','foreign',$3,'master')",
			foreignSecret, foreignUser, []byte("x"))
		_, err := e.pool.Exec(context.Background(), "UPDATE runs SET codex_secret_id=$2 WHERE id=$1", e.run, foreignSecret)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "runs_codex_secret_fk" {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		reportStoppedRoute(t, e, w, false, false)
	})
}
