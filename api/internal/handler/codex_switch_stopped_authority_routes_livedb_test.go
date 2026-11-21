package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestWorkerCodexStoppedAuthorityRouteLiveDB(t *testing.T) {
	cases := []struct {
		name    string
		sql     string
		noAudit bool
	}{
		{name: "registration_nonce_mismatch"},
		{name: "claim_generation_mismatch_no_reconciliation", noAudit: true},
		{name: "claim_released_no_reconciliation", sql: "claim_released_at=now()", noAudit: true},
		{name: "missing_frozen_secret", sql: "codex_secret_id=NULL"},
		{name: "missing_capability_hash", sql: "codex_cap_hash=NULL"},
		{name: "switch_requested_marker_only", sql: "credential_switch_requested_at=now(),credential_switch_generation=NULL"},
		{name: "switch_generation_marker_only", sql: "credential_switch_generation=$2,credential_switch_requested_at=NULL"},
		{name: "ordinary_hold_reason", sql: "hold_reason='completion_blocked'"},
		{name: "pause_requested_marker_only", sql: "pause_requested_at=now(),pause_mode=NULL"},
		{name: "pause_mode_marker_only", sql: "pause_mode='now',pause_requested_at=NULL"},
		{name: "stop_kind_marker_only", sql: "stop_kind='cancelled',stop_reason=NULL"},
		{name: "stop_reason_marker_only", sql: "stop_reason='fixture',stop_kind=NULL"},
		{name: "reserve_hold_marker_only", sql: "reserve_hold_reason='fixture'"},
		{name: "status_claimed", sql: "status='claimed'"},
	}
	for _, status := range []string{"running", "paused"} {
		for _, tc := range cases {
			t.Run(status+"_"+tc.name, func(t *testing.T) {
				e, w := setupStoppedGuardsRoute(t)
				withdrawStoppedRoute(t, e, w)
				if status == "paused" {
					reportStoppedRoute(t, e, w, true, true)
				}
				if tc.noAudit {
					var absent bool
					if err := e.pool.QueryRow(context.Background(),
						"SELECT NOT EXISTS (SELECT 1 FROM run_milestone_reconciliations WHERE run_id=$1)",
						e.run).Scan(&absent); err != nil {
						t.Fatal(err)
					}
					if !absent {
						t.Fatal("fixture must have no milestone reconciliation")
					}
				}
				switch tc.name {
				case "registration_nonce_mismatch":
					w.RegisterNonce = "different-current-incarnation"
				case "claim_generation_mismatch_no_reconciliation":
					w.ClaimGeneration++
				case "switch_generation_marker_only":
					stoppedRouteExec(t, e, "UPDATE runs SET "+tc.sql+" WHERE id=$1", e.run, e.gen)
				default:
					stoppedRouteExec(t, e, "UPDATE runs SET "+tc.sql+" WHERE id=$1", e.run)
				}
				reportStoppedRoute(t, e, w, false, false)
			})
		}
	}
}

func TestWorkerCodexStoppedWireAuthorityRouteLiveDB(t *testing.T) {
	for _, status := range []string{"running", "paused"} {
		for _, name := range []string{"extra_known_typed_member", "register_nonce_alias"} {
			t.Run(status+"_"+name, func(t *testing.T) {
				e, w := setupStoppedGuardsRoute(t)
				withdrawStoppedRoute(t, e, w)
				if status == "paused" {
					reportStoppedRoute(t, e, w, true, true)
				}
				fields := map[string]any{
					"status": w.Status, "claim_generation": w.ClaimGeneration,
					"register_nonce": w.RegisterNonce, "expected_epoch": w.ExpectedEpoch,
				}
				switch name {
				case "extra_known_typed_member":
					// StateRequest.MessagesThroughSeq is known to the typed decoder.
					fields["messages_through_seq"] = 0
				case "register_nonce_alias":
					delete(fields, "register_nonce")
					fields["Register_Nonce"] = w.RegisterNonce
				}
				body, err := json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if len(body) > 1024 {
					t.Fatal("wire fixture exceeds small payload bound")
				}
				before := stoppedRouteSnapshot(t, e)
				rec := e.do(e.worker, http.MethodPost, "/api/worker/runs/"+e.run.String()+"/state", string(body))
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("wire refusal status=%d; want 400", rec.Code)
				}
				assertStoppedSnapshot(t, before, stoppedRouteSnapshot(t, e), false)
			})
		}
	}
}
