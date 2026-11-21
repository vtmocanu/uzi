package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerCodexStoppedSwitchJournalRouteLiveDB(t *testing.T) {
	e, w := setupStoppedGuardsRoute(t)
	ctx := context.Background()
	var oldSecret, target uuid.UUID
	var opX uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT codex_secret_id,codex_switch_target_secret_id,credential_switch_op_id FROM runs WHERE id=$1`,
		e.run).Scan(&oldSecret, &target, &opX); err != nil {
		t.Fatal(err)
	}
	if opX != e.op || oldSecret == target {
		t.Fatal("fixture lacks distinct old/target credentials and pending X")
	}
	rec := e.do(e.worker, http.MethodPost, e.applyPath(),
		fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`, w.ClaimGeneration, w.RegisterNonce, opX))
	var applied struct {
		Outcome         string `json:"outcome"`
		CapabilityEpoch int64  `json:"capability_epoch"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &applied) != nil ||
		applied.Outcome != "applied" || applied.CapabilityEpoch != 42 {
		t.Fatalf("X apply must return applied epoch 42 (status=%d)", rec.Code)
	}

	// Capture the actual transaction's complete row and verify its frozen B identity.
	var journal []byte
	var valid bool
	if err := e.pool.QueryRow(ctx, `SELECT to_jsonb(s),
 s.claim_generation=$3 AND s.ordinal=1 AND s.codex_claim_epoch=$4
 AND s.worker_id=$5 AND s.secret_id=$6 AND s.secret_id=r.codex_secret_id
 AND s.secret_enablement_rev=u.enablement_rev
 AND s.label IS NOT DISTINCT FROM r.codex_secret_label
 AND s.select_reason='pinned' AND s.applied_at IS NOT NULL
 AND s.codex_auth_mode='api_key' AND s.codex_auth_mode=r.codex_auth_mode
 AND s.codex_material_revision IS NOT NULL AND s.codex_material_revision=r.codex_material_revision
 AND s.codex_account_key IS NOT DISTINCT FROM r.codex_account_key
 AND s.codex_account_id IS NULL
 FROM run_credential_switch_epochs s JOIN runs r ON r.id=s.run_id
 JOIN user_secrets u ON u.id=s.secret_id
 WHERE s.run_id=$1 AND s.op_id=$2`,
		e.run, opX, e.gen, applied.CapabilityEpoch, e.worker, target).Scan(&journal, &valid); err != nil {
		t.Fatal(err)
	}
	if !valid || len(journal) == 0 {
		t.Fatal("X journal lacks the applied epoch, ordinal, or frozen B identity")
	}

	// Service preparation requests Y; worker preflight alone latches stop authority.
	if _, err := e.handler.wsvc.SetRunCredential(ctx, e.user, e.run, workersvc.BindModePinned, &oldSecret); err != nil {
		t.Fatal(err)
	}
	var opY uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT credential_switch_op_id FROM runs WHERE id=$1`, e.run).Scan(&opY); err != nil {
		t.Fatal(err)
	}
	if opY == uuid.Nil || opY == opX {
		t.Fatal("reswitch must create a distinct pending Y")
	}
	e.op = opY
	rec = e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/credential-switch/codex/preflight",
		fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q,"target_secret_id":%q}`,
			w.ClaimGeneration, w.RegisterNonce, opY, oldSecret))
	var ready struct {
		Outcome         string `json:"outcome"`
		ExpectedEpoch   int64  `json:"expected_epoch"`
		ClaimGeneration int64  `json:"claim_generation"`
		RegisterNonce   string `json:"register_nonce"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ready) != nil ||
		ready.Outcome != "ready" || ready.ExpectedEpoch != 42 ||
		ready.ClaimGeneration != w.ClaimGeneration || ready.RegisterNonce != w.RegisterNonce {
		t.Fatalf("Y preflight must return current ready authority at epoch 42 (status=%d)", rec.Code)
	}
	neww := stoppedRouteWire{"codex_switch_stopped", ready.ClaimGeneration, ready.RegisterNonce, ready.ExpectedEpoch}
	var capBefore, capAfter []byte
	var epochAfter int64
	if err := e.pool.QueryRow(ctx, `SELECT codex_cap_hash FROM runs WHERE id=$1`, e.run).Scan(&capBefore); err != nil {
		t.Fatal(err)
	}
	if len(capBefore) == 0 {
		t.Fatal("X apply left no capability")
	}
	withdrawStoppedRoute(t, e, neww)
	if err := e.pool.QueryRow(ctx, `SELECT codex_cap_hash,codex_claim_epoch FROM runs WHERE id=$1`,
		e.run).Scan(&capAfter, &epochAfter); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(capBefore, capAfter) || epochAfter != ready.ExpectedEpoch {
		t.Fatal("Y withdrawal changed capability authority")
	}
	var yExists bool
	if err := e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_credential_switch_epochs WHERE run_id=$1 AND op_id=$2)`,
		e.run, opY).Scan(&yExists); err != nil {
		t.Fatal(err)
	}
	if yExists {
		t.Fatal("historyless Y withdrawal created a durable switch row")
	}

	// Prove the full snapshot contains the exact nonempty applied row before
	// reportStoppedRoute checks all other data and all six permitted run changes.
	beforeStop := stoppedRouteSnapshot(t, e)
	var rows []json.RawMessage
	if err := json.Unmarshal(beforeStop["switch"], &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatal("pre-stop switch snapshot must contain exactly one applied X row")
	}
	var want map[string]json.RawMessage
	if err := json.Unmarshal(journal, &want); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		var got map[string]json.RawMessage
		if err := json.Unmarshal(row, &got); err != nil {
			t.Fatal(err)
		}
		if reflect.DeepEqual(want, got) {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("pre-stop switch snapshot lacks the exact applied X row")
	}
	reportStoppedRoute(t, e, neww, true, true)
	reportStoppedRoute(t, e, neww, true, false)
}
