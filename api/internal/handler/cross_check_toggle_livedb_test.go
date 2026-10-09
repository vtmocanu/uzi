package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/capability"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

// PRD #2460: enabling plan cross-check warns per direction. A Claude lead needs a Codex
// checker worker (codex_harness_v1); a Codex lead needs a Claude checker worker
// (cross_check_codex_lead_v1). Each warning names the family with no capable worker.
func TestSetCrossCheckWarnsPerCheckerDirectionLiveDB(t *testing.T) {
	const codexWarning = "No online worker can run Codex plan cross-checks; enable ephemeral workers or upgrade a worker."
	const claudeWarning = "No online worker can run Claude plan cross-checks for Codex-lead runs; enable ephemeral workers or upgrade a worker."
	for _, tc := range []struct {
		name    string
		workers [][]string
		want    string
	}{
		{"a worker covers both directions", [][]string{{capability.CrossCheckV1, capability.CodexHarnessV1, capability.CrossCheckCodexLeadV1}}, ""},
		{"two workers cover one direction each", [][]string{{capability.CrossCheckV1, capability.CodexHarnessV1}, {capability.CrossCheckV1, capability.CrossCheckCodexLeadV1}}, ""},
		{"only a worker released before the Claude checker", [][]string{{capability.CrossCheckV1, capability.CodexHarnessV1}}, claudeWarning},
		{"no Codex-capable worker", [][]string{{capability.CrossCheckV1, capability.CrossCheckCodexLeadV1}}, codexWarning},
		{"no cross-check worker at all", [][]string{{capability.CodexHarnessV1, capability.CrossCheckCodexLeadV1}}, codexWarning},
		{"no worker", nil, codexWarning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pool := secretsCRUDHandler(t)
			h.wsvc = workersvc.New(h.q, nil, workersvc.Params{})
			user := mkSecretUser(t, pool)
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
			}
			t.Cleanup(func() { exec(`DELETE FROM users WHERE id=$1`, user) })
			// Both families usable: the consent endpoint refuses otherwise.
			exec(`INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with)
				VALUES ($1,$2,'anthropic_token','a',true,'\x01','master')`, uuid.New(), user)
			codexSecret := uuid.New()
			exec(`INSERT INTO user_secrets (id,user_id,kind,label,is_default,ciphertext,sealed_with)
				VALUES ($1,$2,'openai_api_key','c',true,'\x01','master')`, codexSecret, user)
			if _, err := h.q.InsertCodexCredentialState(context.Background(), store.InsertCodexCredentialStateParams{
				UserSecretID: codexSecret, UserID: user, Status: "static"}); err != nil {
				t.Fatal(err)
			}
			for _, caps := range tc.workers {
				id := uuid.New()
				exec(`INSERT INTO workers (id,user_id,name,token_hash,status,protocol_capabilities) VALUES ($1,$2,$3,$4,'online',$5)`,
					id, user, "w-"+id.String(), id[:], caps)
			}
			rec := httptest.NewRecorder()
			h.SetCrossCheck(rec, userReq(http.MethodPut, "/api/me/cross-check", `{"plan":true}`, user, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var body struct {
				Warning string `json:"warning"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Warning != tc.want {
				t.Fatalf("warning = %q, want %q", body.Warning, tc.want)
			}
		})
	}
}
