package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/vtmocanu/uzi/api/internal/pgconv"
	"github.com/vtmocanu/uzi/api/internal/store"
	"github.com/vtmocanu/uzi/api/internal/vault"
)

func seedCodexSwitchSubscriptionA(t *testing.T, e *applyRouteEnv) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	login := fmt.Sprintf(`{"access_token":%q,"refresh_token":%q}`, "route-access-"+uuid.NewString(), "route-refresh-"+uuid.NewString())
	sealed, err := e.handler.box.Seal([]byte(login))
	if err != nil {
		t.Fatal(err)
	}
	label := "switch-subscription-" + uuid.NewString()
	secret, err := e.handler.q.InsertCodexSecret(ctx, store.InsertCodexSecretParams{
		UserID: e.user, Kind: store.KindCodexAuth, Label: label, WantDefault: true,
		Ciphertext: sealed, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err := e.handler.q.InsertCodexProviderAccount(ctx, store.InsertCodexProviderAccountParams{
		UserID: e.user, ProviderUserID: "provider-" + uuid.NewString(), WorkspaceAccountID: "account-" + uuid.NewString(),
		SealedLogin: sealed, SealedWith: store.SealedWithMaster,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.handler.q.InsertCodexCredentialState(ctx, store.InsertCodexCredentialStateParams{
		UserSecretID: secret.ID, UserID: e.user, Status: "staging",
	}); err != nil {
		t.Fatal(err)
	}
	n, err := e.handler.q.LinkCodexCredentialState(ctx, store.LinkCodexCredentialStateParams{
		ProviderAccountID: pgconv.UUID(account.ID), UserSecretID: secret.ID, UserID: e.user, MaterialRevision: 0,
	})
	if err != nil || n != 1 {
		t.Fatalf("link subscription A: rows=%d err=%v", n, err)
	}
	e.handler.wsvc.SetVault(vault.NewPostgres(e.handler.box, e.pool))
	return secret.ID, label
}

func codexApplyRefreshSnapshot(t *testing.T, e *applyRouteEnv, account uuid.UUID) map[string]json.RawMessage {
	t.Helper()
	out := stoppedRouteSnapshot(t, e)
	var a, intents []byte
	if err := e.pool.QueryRow(context.Background(), `SELECT
 (SELECT to_jsonb(a) FROM codex_provider_account a WHERE id=$1 AND user_id=$2),
 (SELECT coalesce(jsonb_agg(to_jsonb(i) ORDER BY operation_id),'[]'::jsonb)
 FROM codex_refresh_intent i WHERE provider_account_id=$1 AND user_id=$2)`,
		account, e.user).Scan(&a, &intents); err != nil {
		t.Fatal(err)
	}
	out["account"] = json.RawMessage(a)
	out["intents"] = json.RawMessage(intents)
	return out
}

func TestWorkerCodexApplyRefreshFencesRouteLiveDB(t *testing.T) {
	for _, fence := range []string{"a_coordination_lease", "a_unresolved_intent"} {
		t.Run(fence, func(t *testing.T) {
			e, w := setupCodexSwitchAccountRoute(t, true, true)
			ctx := context.Background()
			var current, target, account uuid.UUID
			var oldHash []byte
			var generation int64
			if err := e.pool.QueryRow(ctx, `SELECT r.codex_secret_id,r.codex_switch_target_secret_id,r.codex_cap_hash,
 a.id,a.generation FROM runs r JOIN codex_credential_state c ON c.user_secret_id=r.codex_secret_id
 JOIN codex_provider_account a ON a.id=c.provider_account_id AND a.user_id=c.user_id
 WHERE r.id=$1 AND c.user_id=$2`, e.run, e.user).Scan(&current, &target, &oldHash, &account, &generation); err != nil {
				t.Fatal(err)
			}
			if current == uuid.Nil || target == uuid.Nil || current == target || len(oldHash) != 32 || account == uuid.Nil {
				t.Fatal("fixture requires subscription A, distinct B and capability hash")
			}
			op := uuid.New()
			lease := fence == "a_coordination_lease"
			// Valid synthetic coordination/intent states isolate the two apply fences.
			// This does not claim worker refresh admission creates a lease without an
			// intent, or exercise actual refresh admission or provider I/O.
			// Exec commits each injection before HTTP apply; no locks are held across it.
			if lease {
				stoppedRouteExec(t, e, `UPDATE codex_provider_account SET coord_state='in_progress',
 coord_operation_id=$2,lease_deadline=now()+interval '5 minutes' WHERE id=$1`, account, op)
			} else {
				stoppedRouteExec(t, e, `INSERT INTO codex_refresh_intent
 (operation_id,user_id,provider_account_id,from_generation,state) VALUES($1,$2,$3,$4,'rotating')`,
					op, e.user, account, generation)
			}
			var isolated bool
			if err := e.pool.QueryRow(ctx, `SELECT
 r.status='running' AND r.hold_reason IS NULL AND r.harness='codex'
 AND r.kind='issue' AND r.review_target_run_id IS NULL AND r.claim_released_at IS NULL
 AND r.worker_id=$2 AND r.claim_generation=$3
 AND r.codex_secret_id=$4 AND r.codex_claim_epoch=41 AND r.codex_cap_hash=$5
 AND r.codex_auth_mode='subscription'
 AND r.credential_switch_requested_at IS NOT NULL AND r.credential_switch_generation=$3
 AND r.credential_switch_op_id=$6 AND r.codex_switch_target_secret_id=$7
 AND r.codex_switch_target_generation=$3 AND r.codex_switch_target_op_id=$6
 AND r.codex_switch_target_auth_mode='api_key'
 AND r.codex_switch_target_material_revision=b.material_revision AND b.status='static'
 AND b.user_id=r.user_id AND b.provider_account_id IS NULL
 AND c.user_id=r.user_id AND c.provider_account_id=a.id
 AND a.user_id=r.user_id AND a.id=$8 AND a.generation=$9
 AND c.material_revision=r.codex_material_revision
 AND r.codex_account_key::jsonb=jsonb_build_array(a.provider_user_id,a.workspace_account_id)
 AND r.codex_account_revision=a.credential_revision
 AND s.kind='codex_auth' AND s.user_id=r.user_id AND s.label=r.codex_secret_label
 AND s.ciphertext=a.sealed_login AND s.sealed_with=a.sealed_with
 AND a.provider_user_id<>'' AND a.workspace_account_id<>''
 AND NOT a.reauth_required AND a.recovery_sealed IS NULL
 AND a.recovery_generation IS NULL AND a.recovery_sealed_with IS NULL
 AND CASE WHEN $10::boolean THEN
 a.coord_state='in_progress' AND a.coord_operation_id=$11 AND a.lease_deadline>now()
 ELSE a.coord_state='idle' AND a.coord_operation_id IS NULL AND a.lease_deadline IS NULL END
 AND (SELECT count(*) FROM codex_refresh_intent i WHERE i.provider_account_id=a.id
 AND i.user_id=r.user_id AND i.state='rotating')=CASE WHEN $10::boolean THEN 0 ELSE 1 END
 AND ($10::boolean OR EXISTS(SELECT 1 FROM codex_refresh_intent i
 WHERE i.operation_id=$11 AND i.user_id=r.user_id AND i.provider_account_id=a.id
 AND i.from_generation=a.generation AND i.state='rotating'))
 AND EXISTS(SELECT 1 FROM workers w WHERE w.id=r.worker_id AND w.user_id=r.user_id
 AND w.snapshot_register_nonce=$12 AND w.credential_switch_apply_nonce=$12)
 AND (SELECT count(*) FROM run_credential_epochs x WHERE x.run_id=r.id
 AND x.claim_generation=r.claim_generation AND x.secret_id=$4 AND x.label=s.label)=1
 AND NOT EXISTS(SELECT 1 FROM run_credential_switch_epochs x WHERE x.run_id=r.id)
 FROM runs r JOIN codex_credential_state c ON c.user_secret_id=r.codex_secret_id
 JOIN user_secrets s ON s.id=c.user_secret_id
 JOIN codex_provider_account a ON a.id=c.provider_account_id
 JOIN codex_credential_state b ON b.user_secret_id=r.codex_switch_target_secret_id
 WHERE r.id=$1`, e.run, e.worker, w.ClaimGeneration, current, oldHash, e.op, target, account, generation, lease, op, w.RegisterNonce).Scan(&isolated); err != nil {
				t.Fatal(err)
			}
			if !isolated {
				t.Fatal("fixture does not isolate A refresh fence with READY X at epoch 41")
			}
			before := codexApplyRefreshSnapshot(t, e, account)
			body := fmt.Sprintf(`{"claim_generation":%d,"register_nonce":%q,"op_id":%q}`, w.ClaimGeneration, w.RegisterNonce, e.op)
			rec := e.do(e.worker, http.MethodPost, e.applyPath(), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("fenced apply status=%d", rec.Code)
			}
			var wire map[string]json.RawMessage
			if json.Unmarshal(rec.Body.Bytes(), &wire) != nil {
				t.Fatal("apply response is not a JSON object")
			}
			var outcome string
			if json.Unmarshal(wire["outcome"], &outcome) != nil {
				t.Fatal("apply lacks outcome")
			}
			// Assert the decision first so a fence bypass fails here, before auth fields or snapshots.
			if outcome != "not_applied" {
				t.Fatalf("refresh-fenced apply outcome=%.64q, want not_applied", outcome)
			}
			var reason string
			if json.Unmarshal(wire["reason"], &reason) != nil || reason != "credential_busy" {
				t.Fatal("apply must return credential_busy")
			}
			if len(wire) != 2 {
				t.Fatal("refusal must contain exactly outcome and reason")
			}
			after := codexApplyRefreshSnapshot(t, e, account)
			sections := []string{"run", "base", "switch", "messages", "inputs", "custody", "presentations", "account", "intents"}
			if len(before) != 9 || len(after) != 9 {
				t.Fatal("snapshot must contain nine sections")
			}
			for _, section := range sections {
				b, bok := before[section]
				a, aok := after[section]
				if !bok || !aok || !reflect.DeepEqual(b, a) {
					t.Fatalf("refused apply changed section %s", section)
				}
			}
			if lease {
				stoppedRouteExec(t, e, `UPDATE codex_provider_account SET coord_state='idle',
 coord_operation_id=NULL,lease_deadline=NULL WHERE id=$1`, account)
			} else {
				stoppedRouteExec(t, e, `UPDATE codex_refresh_intent SET state='reconciled' WHERE operation_id=$1`, op)
			}
			rec = e.do(e.worker, http.MethodPost, e.applyPath(), body)
			if rec.Code != http.StatusOK {
				t.Fatalf("resolved apply status=%d", rec.Code)
			}
			wire = nil
			if json.Unmarshal(rec.Body.Bytes(), &wire) != nil {
				t.Fatal("resolved apply response is not JSON")
			}
			if json.Unmarshal(wire["outcome"], &outcome) != nil || outcome != "applied" {
				t.Fatal("same X must apply after clearing blocker")
			}
			var epoch, ordinal int64
			if json.Unmarshal(wire["capability_epoch"], &epoch) != nil || epoch != 42 ||
				json.Unmarshal(wire["ordinal"], &ordinal) != nil || ordinal != 1 {
				t.Fatal("resolved apply must return epoch 42 ordinal 1")
			}
			var valid bool
			if err := e.pool.QueryRow(ctx, `SELECT codex_secret_id=$2 AND codex_auth_mode='api_key'
 AND codex_claim_epoch=42 AND octet_length(codex_cap_hash)=32 AND codex_cap_hash<>$3
 AND claim_generation=$4 AND worker_id=$5 AND claim_released_at IS NULL
 AND credential_switch_requested_at IS NULL AND credential_switch_generation IS NULL
 AND credential_switch_op_id IS NULL AND codex_switch_target_secret_id IS NULL
 AND (SELECT count(*) FROM run_credential_switch_epochs s WHERE s.run_id=r.id)=1
 AND EXISTS(SELECT 1 FROM run_credential_switch_epochs s WHERE s.run_id=r.id
 AND s.op_id=$6 AND s.ordinal=1 AND s.secret_id=r.codex_secret_id
 AND s.worker_id=r.worker_id AND s.claim_generation=r.claim_generation
 AND s.codex_claim_epoch=r.codex_claim_epoch AND s.codex_auth_mode=r.codex_auth_mode
 AND s.label IS NOT DISTINCT FROM r.codex_secret_label
 AND s.codex_material_revision=r.codex_material_revision
 AND s.codex_account_key IS NOT DISTINCT FROM r.codex_account_key)
 FROM runs r WHERE id=$1`, e.run, target, oldHash, w.ClaimGeneration, e.worker, e.op).Scan(&valid); err != nil {
				t.Fatal(err)
			}
			if !valid {
				t.Fatal("resolved X must journal B at epoch 42 ordinal 1 with current authority and rotated hash")
			}
		})
	}
}
