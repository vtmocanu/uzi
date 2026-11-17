package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerCodexSwitchPreflightRouteLiveDB(t *testing.T) {
	e := setupApplyRouteLiveDB(t)
	path := "/api/worker/runs/" + e.run.String() + "/credential-switch/codex/preflight"
	for _, body := range []string{
		"{}",
		"{",
		"{\"unknown\":true}",
		"{\"padding\":\"" + strings.Repeat("x", 40<<10) + "\"}",
	} {
		rec := e.do(e.worker, "POST", path, body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("strict bounded preflight status=%d", rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("preflight refusal is cacheable")
		}
	}
	body := strings.TrimSuffix(e.applyBody(e.op), "}") + ",\"target_secret_id\":\"" + e.secret.String() + "\"}"
	for _, tc := range []struct {
		name           string
		stoppedSupport bool
		outcome        string
		reason         string
	}{
		{"missing_stopped_support", false, "not_applied", "worker_upgrade_required"},
		{"stopped_support_wrong_harness", true, "fenced", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.stoppedSupport {
				// Satisfy negotiation and registration so this Claude run is
				// refused by the harness predicate, not another worker fence.
				if _, err := e.pool.Exec(context.Background(), `UPDATE workers SET protocol_capabilities =
protocol_capabilities || $2::text[], credential_switch_apply_nonce = snapshot_register_nonce
WHERE id = $1`, e.worker, []string{capability.CodexSwitchStoppedV1, capability.CodexCredentialSwitchV1}); err != nil {
					t.Fatal(err)
				}
			}
			rec := e.do(e.worker, "POST", path, body)
			var got workersvc.CodexSwitchPreflightResult
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil ||
				got.Outcome != tc.outcome || got.Reason != tc.reason {
				t.Fatalf("unnegotiated preflight status=%d body=%s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), e.plain) || strings.Contains(rec.Body.String(), "ciphertext") {
				t.Fatal("preflight disclosed material")
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("preflight result is cacheable")
			}
		})
	}
}
