package handler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// Only these four fields belong to the stopped settlement wire contract.
type stoppedRouteWire struct {
	Status          string `json:"status"`
	ClaimGeneration int64  `json:"claim_generation"`
	RegisterNonce   string `json:"register_nonce"`
	ExpectedEpoch   int64  `json:"expected_epoch"`
}

func stoppedRouteExec(t *testing.T, e *applyRouteEnv, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

func setupStoppedGuardsRoute(t *testing.T) (*applyRouteEnv, stoppedRouteWire) {
	t.Helper()
	return setupStoppedGuardsRouteWithSeed(t, false)
}

func setupStoppedGuardsRouteWithSeed(t *testing.T, seedPending bool) (*applyRouteEnv, stoppedRouteWire) {
	t.Helper()
	return setupCodexSwitchAccountRoute(t, seedPending, false)
}

func setupCodexSwitchAccountRoute(t *testing.T, seedPending, subscriptionA bool) (*applyRouteEnv, stoppedRouteWire) {
	t.Helper()
	e := setupApplyRouteLiveDB(t)
	ctx := context.Background()
	q, svc := e.handler.q, e.handler.wsvc
	svc.SetVault(vault.NewPostgres(e.handler.box, e.pool))
	// Registration without a snapshot retires existing active claims. Negotiate
	// first, then seed this fixture's active claim under the new incarnation.
	stoppedRouteExec(t, e, `UPDATE runs SET status='queued',worker_id=NULL,
 credential_switch_requested_at=NULL,credential_switch_generation=NULL,
 budget_wall_seconds=3600,interactive=false WHERE id=$1`, e.run)
	rec := e.do(e.worker, http.MethodPost, "/api/worker/register",
		`{"version":"1.0.0","protocol_capabilities":["credential_switch_v1","credential_switch_in_place_v1","input_receipts_v1","codex_credential_switch_v1","codex_switch_stopped_v1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status=%d", rec.Code)
	}
	var nonce, applyNonce string
	if err := e.pool.QueryRow(ctx, `SELECT snapshot_register_nonce,credential_switch_apply_nonce FROM workers WHERE id=$1`, e.worker).Scan(&nonce, &applyNonce); err != nil {
		t.Fatal(err)
	}
	if nonce == "" || nonce != applyNonce {
		t.Fatal("registration did not negotiate apply authority")
	}
	stoppedRouteExec(t, e, `UPDATE runs SET status='running',worker_id=$2,
 started_at=now(),status_since=now() WHERE id=$1`, e.run, e.worker)
	seed := func(label string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		sealed, err := e.handler.box.Seal([]byte("harmless-stopped-fixture-" + id.String()))
		if err != nil {
			t.Fatal(err)
		}
		stoppedRouteExec(t, e, `INSERT INTO user_secrets(id,user_id,kind,label,ciphertext,sealed_with)
 VALUES($1,$2,'openai_api_key',$3,$4,'master')`, id, e.user, label, sealed)
		if _, err := q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{UserSecretID: id, UserID: e.user, Status: "static"}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldLabel := "stopped-old-" + uuid.NewString()
	var old uuid.UUID
	mode := "api_key"
	if subscriptionA {
		old, oldLabel = seedCodexSwitchSubscriptionA(t, e)
		mode = "subscription"
	} else {
		old = seed(oldLabel)
	}
	target := seed("stopped-target-" + uuid.NewString())
	if err := svc.FreezeCodexBinding(ctx, e.user, e.run, old, mode); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte("synthetic stopped capability"))
	stoppedRouteExec(t, e, `UPDATE runs SET codex_claim_epoch=41,codex_cap_hash=$2 WHERE id=$1`, e.run, hash[:])
	if err := q.RecordCodexCredentialEpoch(ctx, store.RecordCodexCredentialEpochParams{
		RunID: e.run, ClaimGeneration: e.gen, SecretID: pgtype.UUID{Bytes: old, Valid: true},
		Label: pgtype.Text{String: oldLabel, Valid: true}, SelectReason: pgtype.Text{String: "codex_binding", Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if seedPending {
		// Seed X independently of the owner request route under test. These
		// queries preserve the pending-only writes and operation-ID trigger.
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT material_revision FROM codex_credential_state
 WHERE user_secret_id=$1 AND user_id=$2`, target, e.user).Scan(&revision); err != nil {
			t.Fatal(err)
		}
		fixtureQ := q.WithTx(tx)
		generation := pgtype.Int8{Int64: e.gen, Valid: true}
		n, err := fixtureQ.StampHeldCodexCredentialSwitch(ctx, store.StampHeldCodexCredentialSwitchParams{
			ID: e.run, UserID: e.user, WorkerID: pgtype.UUID{Bytes: e.worker, Valid: true}, Generation: generation,
		})
		if err != nil || n != 1 {
			t.Fatalf("seed pending switch stamp: rows=%d err=%v", n, err)
		}
		n, err = fixtureQ.SetPendingCodexSwitchTarget(ctx, store.SetPendingCodexSwitchTargetParams{
			ID: e.run, UserID: e.user, SecretID: pgtype.UUID{Bytes: target, Valid: true}, Generation: generation,
			AuthMode: "api_key", MaterialRevision: revision,
		})
		if err != nil || n != 1 {
			t.Fatalf("seed pending switch target: rows=%d err=%v", n, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	} else if _, err := svc.SetRunCredential(ctx, e.user, e.run, workersvc.BindModePinned, &target); err != nil {
		t.Fatal(err)
	}
	if err := e.pool.QueryRow(ctx, `SELECT credential_switch_op_id FROM runs WHERE id=$1`, e.run).Scan(&e.op); err != nil {
		t.Fatal(err)
	}
	rec = e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/credential-switch/codex/preflight",
		fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q,"target_secret_id":%q}`, e.gen, nonce, e.op, target))
	var ready struct {
		Outcome         string `json:"outcome"`
		ExpectedEpoch   int64  `json:"expected_epoch"`
		ClaimGeneration int64  `json:"claim_generation"`
		RegisterNonce   string `json:"register_nonce"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ready) != nil || ready.Outcome != "ready" ||
		ready.ExpectedEpoch != 41 || ready.ClaimGeneration != e.gen || ready.RegisterNonce != nonce {
		t.Fatalf("preflight must be ready with captured authority (status=%d)", rec.Code)
	}
	stoppedRouteExec(t, e, `UPDATE runs SET health='stalled',health_reason='fixture',health_since=now() WHERE id=$1`, e.run)
	return e, stoppedRouteWire{"codex_switch_stopped", ready.ClaimGeneration, ready.RegisterNonce, ready.ExpectedEpoch}
}

func withdrawStoppedRoute(t *testing.T, e *applyRouteEnv, w stoppedRouteWire) {
	t.Helper()
	rec := e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state",
		fmt.Sprintf(`{"status":"credential_switch_failed","claim_generation":%d,"register_nonce":%q,"op_id":%q}`, w.ClaimGeneration, w.RegisterNonce, e.op))
	if rec.Code != http.StatusOK {
		t.Fatalf("withdraw status=%d", rec.Code)
	}
	var cleared bool
	if err := e.pool.QueryRow(context.Background(), `SELECT credential_switch_requested_at IS NULL AND credential_switch_generation IS NULL FROM runs WHERE id=$1`, e.run).Scan(&cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared {
		t.Fatal("withdraw left pending switch authority")
	}
}

// PostgreSQL canonicalizes every object; arrays retain explicit primary-coordinate order.
func stoppedRouteSnapshot(t *testing.T, e *applyRouteEnv) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	err := e.pool.QueryRow(context.Background(), `SELECT jsonb_build_object(
 'run',(SELECT to_jsonb(r) FROM runs r WHERE id=$1),
 'base',(SELECT coalesce(jsonb_agg(to_jsonb(b) ORDER BY claim_generation),'[]'::jsonb) FROM run_credential_epochs b WHERE run_id=$1),
 'switch',(SELECT coalesce(jsonb_agg(to_jsonb(s) ORDER BY claim_generation,ordinal),'[]'::jsonb) FROM run_credential_switch_epochs s WHERE run_id=$1),
 'messages',(SELECT coalesce(jsonb_agg(to_jsonb(m) ORDER BY seq),'[]'::jsonb) FROM run_messages m WHERE run_id=$1),
 'inputs',(SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY id),'[]'::jsonb) FROM run_user_inputs i WHERE run_id=$1),
 'custody',(SELECT coalesce(jsonb_agg(to_jsonb(c) ORDER BY id),'[]'::jsonb) FROM recovery_custody_holds c WHERE run_id=$1))`, e.run).Scan(&raw)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]json.RawMessage
	if err = json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertStoppedSnapshot(t *testing.T, before, after map[string]json.RawMessage, first bool) {
	t.Helper()
	if first {
		var b, a map[string]json.RawMessage
		if err := json.Unmarshal(before["run"], &b); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(after["run"], &a); err != nil {
			t.Fatal(err)
		}
		if string(a["status"]) != `"paused"` || string(a["hold_reason"]) != "null" {
			t.Fatal("stop did not store paused with NULL hold")
		}
		if string(a["health"]) != `"ok"` || string(a["health_reason"]) != "null" || string(a["health_since"]) != "null" {
			t.Fatal("stop did not reset unhealthy state")
		}
		for _, key := range []string{"status", "status_since", "updated_at", "health", "health_reason", "health_since"} {
			delete(b, key)
			delete(a, key)
		}
		if !reflect.DeepEqual(b, a) {
			t.Fatal("first stop changed unrelated run fields")
		}
		delete(before, "run")
		delete(after, "run")
	}
	for key, b := range before {
		if !reflect.DeepEqual(b, after[key]) {
			t.Fatalf("snapshot changed in %s", key)
		}
	}
}

func reportStoppedRoute(t *testing.T, e *applyRouteEnv, w stoppedRouteWire, accept, first bool) {
	t.Helper()
	before := stoppedRouteSnapshot(t, e)
	body, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state", string(body))
	var response struct {
		Applied     bool                       `json:"applied"`
		Disposition string                     `json:"disposition"`
		Run         map[string]json.RawMessage `json:"run"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("state status=%d invalid JSON", rec.Code)
	}
	if accept {
		if rec.Code != http.StatusOK || !response.Applied || response.Disposition != "codex_switch_stopped" {
			t.Fatalf("stop acceptance mismatch: status=%d applied=%t", rec.Code, response.Applied)
		}
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || len(envelope) != 3 || rec.Body.Len() > 16*1024 {
			t.Fatal("stopped acknowledgement is not a bounded minimal envelope")
		}
		if len(response.Run) != 4 || string(response.Run["id"]) != fmt.Sprintf("%q", e.run.String()) ||
			string(response.Run["status"]) != `"paused"` ||
			string(response.Run["claim_generation"]) != fmt.Sprint(w.ClaimGeneration) || string(response.Run["hold_reason"]) != "null" {
			t.Fatal("stopped acknowledgement does not identify the committed paused run")
		}
	} else if rec.Code != http.StatusConflict || response.Applied {
		t.Fatalf("refusal mismatch: status=%d applied=%t", rec.Code, response.Applied)
	}
	assertStoppedSnapshot(t, before, stoppedRouteSnapshot(t, e), first)
}

func TestWorkerCodexStoppedGuardsRouteLiveDB(t *testing.T) {
	t.Run("fresh_and_replay", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		withdrawStoppedRoute(t, e, w)
		reportStoppedRoute(t, e, w, true, true)
		reportStoppedRoute(t, e, w, true, false)
	})
	for _, status := range []string{"running", "paused"} {
		for _, variant := range []string{"stale", "mismatched"} {
			t.Run("epoch_"+status+"_"+variant, func(t *testing.T) {
				e, w := setupStoppedGuardsRoute(t)
				withdrawStoppedRoute(t, e, w)
				if status == "paused" {
					reportStoppedRoute(t, e, w, true, true)
				}
				if variant == "stale" {
					stoppedRouteExec(t, e, "UPDATE runs SET codex_claim_epoch=42 WHERE id=$1", e.run)
				} else {
					w.ExpectedEpoch = 42
				}
				reportStoppedRoute(t, e, w, false, false)
			})
		}
		for _, kind := range []string{"pause", "pause_cancel", "cancel", "stop", "scope", "revise_plan", "reject_plan", "resume"} {
			t.Run("pending_"+status+"_"+kind, func(t *testing.T) {
				e, w := setupStoppedGuardsRoute(t)
				withdrawStoppedRoute(t, e, w)
				if status == "paused" {
					reportStoppedRoute(t, e, w, true, true)
				}
				// Exactly one input is the competing control: no pause flags or cutoff.
				stoppedRouteExec(t, e, "INSERT INTO run_user_inputs(run_id,kind,body) VALUES($1,$2,'competing control')", e.run, kind)
				reportStoppedRoute(t, e, w, false, false)
			})
		}
	}
	t.Run("epoch_applied_successor", func(t *testing.T) {
		e, w := setupStoppedGuardsRoute(t)
		rec := e.do(e.worker, http.MethodPost, e.applyPath(), fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`, w.ClaimGeneration, w.RegisterNonce, e.op))
		var applied struct {
			Outcome         string `json:"outcome"`
			CapabilityEpoch int64  `json:"capability_epoch"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &applied) != nil || applied.Outcome != "applied" || applied.CapabilityEpoch != 42 {
			t.Fatalf("successor apply status=%d; expected applied epoch 42", rec.Code)
		}
		reportStoppedRoute(t, e, w, false, false)
	})
	for _, scenario := range []string{"expired_budget", "replay_original_clock", "replay_no_write"} {
		t.Run("paused_"+scenario, func(t *testing.T) {
			e, w := setupStoppedGuardsRoute(t)
			withdrawStoppedRoute(t, e, w)
			reportStoppedRoute(t, e, w, true, true)
			switch scenario {
			case "expired_budget":
				stoppedRouteExec(t, e, `UPDATE runs SET started_at=now()-interval '1 hour',status_since=now()-interval '30 minutes',budget_wall_seconds=60,budget_paused_seconds=0 WHERE id=$1`, e.run)
			case "replay_original_clock":
				stoppedRouteExec(t, e, `UPDATE runs SET started_at=now()-interval '1 hour',status_since=now()-interval '3590 seconds',budget_wall_seconds=60,budget_paused_seconds=0 WHERE id=$1`, e.run)
			case "replay_no_write":
				stoppedRouteExec(t, e, `UPDATE runs SET started_at=now()-interval '30 seconds',status_since=now()-interval '20 seconds',budget_wall_seconds=3600,budget_paused_seconds=0 WHERE id=$1`, e.run)
			}
			reportStoppedRoute(t, e, w, scenario != "expired_budget", false)
		})
	}
}
