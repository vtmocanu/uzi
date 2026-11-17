package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/workersvc"
)

func TestWorkerCodexSwitchReleaseFencesRouteLiveDB(t *testing.T) {
	for _, tc := range []struct {
		name string
		want error
	}{
		{"old_epoch_new_secret", workersvc.ErrCodexCapabilityEpoch},
		{"current_epoch_old_secret", workersvc.ErrCodexCapabilityMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, w := setupStoppedGuardsRoute(t)
			ctx := context.Background()
			var old, target uuid.UUID
			if err := e.pool.QueryRow(ctx, `SELECT codex_secret_id,codex_switch_target_secret_id
 FROM runs WHERE id=$1`, e.run).Scan(&old, &target); err != nil {
				t.Fatal(err)
			}
			if old == uuid.Nil || target == uuid.Nil || old == target {
				t.Fatal("fixture must bind distinct A and pending B credentials")
			}
			oldPlain := "harmless-stopped-fixture-" + old.String()
			targetPlain := "harmless-stopped-fixture-" + target.String()
			const oldSecret = "synthetic stopped capability"
			oldHash := sha256.Sum256([]byte(oldSecret))
			var initialValid bool
			if err := e.pool.QueryRow(ctx, `SELECT codex_claim_epoch=41 AND codex_cap_hash=$2
 AND claim_generation=$3 AND worker_id=$4 FROM runs WHERE id=$1`,
				e.run, oldHash[:], w.ClaimGeneration, e.worker).Scan(&initialValid); err != nil {
				t.Fatal(err)
			}
			if !initialValid {
				t.Fatal("fixture lacks original A authority")
			}

			rec := e.do(e.worker, http.MethodPost, e.applyPath(),
				fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`,
					w.ClaimGeneration, w.RegisterNonce, e.op))
			var applied struct {
				Outcome         string `json:"outcome"`
				CapabilityEpoch int64  `json:"capability_epoch"`
				Codex           *struct {
					Capability string `json:"capability"`
				} `json:"codex"`
			}
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &applied) != nil ||
				applied.Outcome != "applied" || applied.CapabilityEpoch != 42 || applied.Codex == nil {
				t.Fatalf("X apply must return applied epoch 42 (status=%d)", rec.Code)
			}
			capability := applied.Codex.Capability
			head, secret, found := strings.Cut(capability, ".")
			epoch, parseErr := strconv.ParseInt(head, 10, 64)
			if !found || parseErr != nil || epoch != 42 || secret == "" {
				t.Fatal("B capability must parse with epoch 42 and a nonempty secret")
			}
			hash := sha256.Sum256([]byte(secret))
			if bytes.Equal(hash[:], oldHash[:]) {
				t.Fatal("B capability must rotate the secret hash")
			}
			checkBinding := func() {
				t.Helper()
				var valid bool
				if err := e.pool.QueryRow(ctx, `SELECT codex_secret_id=$2 AND codex_auth_mode='api_key'
 AND codex_claim_epoch=42 AND codex_cap_hash=$3
 AND claim_generation=$4 AND worker_id=$5
 AND codex_material_revision IS NOT NULL
 AND EXISTS (SELECT 1 FROM run_credential_switch_epochs s
 WHERE s.run_id=r.id AND s.op_id=$6 AND s.secret_id=r.codex_secret_id
 AND s.claim_generation=r.claim_generation AND s.worker_id=r.worker_id
 AND s.codex_claim_epoch=r.codex_claim_epoch AND s.codex_auth_mode=r.codex_auth_mode
 AND s.label IS NOT DISTINCT FROM r.codex_secret_label
 AND s.codex_material_revision=r.codex_material_revision
 AND s.codex_account_key IS NOT DISTINCT FROM r.codex_account_key)
 FROM runs r WHERE id=$1`, e.run, target, hash[:], w.ClaimGeneration, e.worker, e.op).Scan(&valid); err != nil {
					t.Fatal(err)
				}
				if !valid {
					t.Fatal("release changed frozen B binding or claim/capability authority")
				}
			}
			checkBinding()
			worker, err := e.handler.q.GetWorkerByID(ctx, e.worker)
			if err != nil {
				t.Fatal(err)
			}
			path := "/api/worker/runs/" + e.run.String() + "/codex/release"
			releaseBody := func(cap string) string {
				return fmt.Sprintf(`{"capability":%q}`, cap)
			}
			checkSuccess := func() {
				t.Helper()
				rec := e.do(e.worker, http.MethodPost, path, releaseBody(capability))
				var response struct {
					AuthMode    string `json:"auth_mode"`
					AccessToken string `json:"access_token"`
				}
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil ||
					response.AuthMode != "api_key" || response.AccessToken != targetPlain ||
					rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("B release must return exact B api_key with no-store (status=%d)", rec.Code)
				}
				checkBinding()
			}
			checkRefused := func(cap string, want error) {
				t.Helper()
				rec := e.do(e.worker, http.MethodPost, path, releaseBody(cap))
				// writeCodexError intentionally collapses both capability sentinels to 403.
				if rec.Code != http.StatusForbidden {
					t.Fatalf("capability refusal status=%d; want 403", rec.Code)
				}
				var response map[string]json.RawMessage
				if json.Unmarshal(rec.Body.Bytes(), &response) != nil {
					t.Fatal("capability refusal must return JSON")
				}
				if _, present := response["access_token"]; present {
					t.Fatal("capability refusal disclosed an access_token field")
				}
				if strings.Contains(rec.Body.String(), oldPlain) || strings.Contains(rec.Body.String(), targetPlain) {
					t.Fatal("capability refusal disclosed credential plaintext")
				}
				// Exercise real store-backed release to distinguish causes hidden by the HTTP mapping.
				_, err := e.handler.wsvc.ReleaseCodexCredential(ctx, worker, e.run, cap)
				if !errors.Is(err, want) {
					t.Fatal("store-backed release refused for the wrong capability fence")
				}
				checkBinding()
				checkSuccess()
			}
			checkSuccess()
			bad := "41." + secret
			if tc.name == "current_epoch_old_secret" {
				bad = "42." + oldSecret
			}
			checkRefused(bad, tc.want)
			checkRefused("41."+oldSecret, workersvc.ErrCodexCapabilityEpoch)
		})
	}
}
