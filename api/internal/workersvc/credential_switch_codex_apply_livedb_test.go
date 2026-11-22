package workersvc

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/vtmocanu/uzi/api/internal/capability"
)

const codexApplyTargetAccess = "apply-target-access-fixture"

// Preparation uses the public owner request so pending snapshots are captured
// under the same locks as production; ordinal zero is recorded before requesting.
func codexApplyFixture(t *testing.T, subscription bool) (applyFx, uuid.UUID, uuid.UUID) {
	t.Helper()
	return codexApplyFixtureWithRegistration(t, subscription, false)
}

func codexApplyFixtureWithRegistration(t *testing.T, subscription, register bool) (applyFx, uuid.UUID, uuid.UUID) {
	t.Helper()
	f, run, target, _ := codexUnrequestedFixture(t, false, subscription, register)
	_, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target)
	if err != nil {
		t.Fatal(err)
	}
	return f, run, target
}

// Freeze initial authority and record ordinal zero without making an owner request.
func codexUnrequestedFixture(t *testing.T, oldSubscription, targetSubscription, register bool) (applyFx, uuid.UUID, uuid.UUID, string) {
	t.Helper()
	f := newApplyFx(t)
	if register {
		f.register(t, []string{capability.CodexCredentialSwitchV1, capability.CodexSwitchStoppedV1})
	} else {
		f.env.exec("UPDATE workers SET credential_switch_apply_nonce=snapshot_register_nonce, protocol_capabilities=ARRAY[$2,'codex_switch_stopped_v1']::text[] WHERE id=$1", f.o.workerID, capability.CodexCredentialSwitchV1)
	}
	run, _ := f.run(t, "running")
	var old uuid.UUID
	authMode := codexAuthModeAPIKey
	if oldSubscription {
		old = f.env.seedLinkedSubscription(t, f.o.userID, "apply-old-subscription", codexToken("old-access"), codexToken("old-refresh"))
		authMode = codexAuthModeSubscription
	} else {
		old = f.env.seedStaticAPIKey(t, f.o.userID, "apply-old", "apply-old-key")
	}
	if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, run, old, authMode); err != nil {
		t.Fatal(err)
	}
	plain, hash := mintCodexCapability()
	f.env.exec("UPDATE runs SET codex_claim_epoch=41, codex_cap_hash=$2 WHERE id=$1", run, hash)
	if err := f.svc.recordCodexEpoch(f.env.ctx, mustRun(t, f.env, run)); err != nil {
		t.Fatal(err)
	}
	target := f.env.seedStaticAPIKey(t, f.o.userID, "apply-target", "apply-target-key")
	if targetSubscription {
		target = f.env.seedLinkedSubscription(t, f.o.userID, "apply-subscription", codexApplyTargetAccess, codexToken("target-refresh"))
	}
	return f, run, target, formatCodexCapability(41, plain)
}

func TestCodexTransactionalApplyBundleLiveDB(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprint(subscription), func(t *testing.T) {
			f, run, target := codexApplyFixture(t, subscription)
			oldPlain, oldHash := mintCodexCapability()
			f.env.exec("UPDATE runs SET codex_cap_hash=$2 WHERE id=$1", run, oldHash)
			oldCapability := formatCodexCapability(41, oldPlain)
			op := f.opOf(t, run)
			res := f.apply(t, run, f.req(op))
			if res.Outcome != ApplyOutcomeApplied || res.Codex == nil || res.Token != "" {
				t.Fatalf("incomplete result: %v", res)
			}
			if _, err := f.svc.AuthorizeCodexCredentialOp(f.env.ctx, f.worker(t), run, oldCapability, ScopeReleaseAccessToken); err == nil {
				t.Fatal("old capability still authorized")
			}
			authority, err := f.svc.AuthorizeCodexCredentialOp(f.env.ctx, f.worker(t), run, res.Codex.Capability, ScopeReleaseAccessToken)
			if err != nil {
				t.Fatal("replacement capability cannot authorize credential release", err)
			}
			if authority.SecretID != target || authority.AuthMode != res.Codex.AuthMode || authority.Scope != ScopeReleaseAccessToken {
				t.Fatal("replacement capability resolved a different authority")
			}
			if res.Ordinal != 1 || res.MessageSeq != 6 || res.CapabilityEpoch != 42 {
				t.Fatal("incorrect replacement coordinates")
			}
			expected := "apply-target-key"
			if subscription {
				expected = codexApplyTargetAccess
			}
			if res.Codex.AccessToken != expected || res.Codex.Capability == "" ||
				(res.Codex.Generation != nil) != subscription ||
				(res.Codex.ChatGPTAccountID != "") != subscription {
				t.Fatal("incorrect auth bundle")
			}
			for _, printed := range []string{fmt.Sprint(res), fmt.Sprintf("%+v", res), fmt.Sprintf("%#v", res), res.LogValue().String()} {
				if strings.Contains(printed, expected) || strings.Contains(printed, res.Codex.Capability) {
					t.Fatal("plaintext logged")
				}
			}
			wire, err := json.Marshal(res)
			if err != nil || !strings.Contains(string(wire), expected) {
				t.Fatal("auth response missing material", err)
			}
			var epoch pgtype.Int8
			var ordinal int32
			var secret uuid.UUID
			var persisted string
			if err := f.env.pool.QueryRow(f.env.ctx, "SELECT codex_claim_epoch,ordinal,secret_id,to_jsonb(e)::text FROM run_credential_switch_epochs e WHERE op_id=$1", op).Scan(&epoch, &ordinal, &secret, &persisted); err != nil {
				t.Fatal(err)
			}
			if !epoch.Valid {
				t.Fatal("committed switch journal missing capability epoch metadata")
			}
			if epoch.Int64 != 42 || ordinal != 1 || secret != target {
				t.Fatal("journal used ordinal as capability epoch")
			}
			if strings.Contains(persisted, expected) || strings.Contains(persisted, res.Codex.Capability) {
				t.Fatal("journal contains auth bytes")
			}
			persistedRun := f.snap(t, run)
			if strings.Contains(persistedRun, expected) || strings.Contains(persistedRun, res.Codex.Capability) {
				t.Fatal("run or feed persisted auth bytes")
			}
			f.noWrite(t, run, func() {
				replay := f.apply(t, run, f.req(op))
				if replay.Outcome != ApplyOutcomeRefusedCommitted || replay.Codex != nil {
					t.Fatal("replay reminted")
				}
			})
			// A second owner request produces ordinal two while the mint epoch becomes 43.
			next := f.env.seedStaticAPIKey(t, f.o.userID, "apply-next", "apply-next-key")
			if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &next); err != nil {
				t.Fatal(err)
			}
			second := f.apply(t, run, f.req(f.opOf(t, run)))
			if second.Outcome != ApplyOutcomeApplied || second.Ordinal != 2 || second.CapabilityEpoch != 43 || second.MessageSeq != 7 {
				t.Fatalf("second switch: %v", second)
			}
			r := mustRun(t, f.env, run)
			if r.CodexAccountKey.Valid || r.CodexAccountRevision.Valid {
				t.Fatal("API key retained subscription authority")
			}
		})
	}
}

func TestCodexApplyTargetMovedAfterPreflightLiveDB(t *testing.T) {
	for _, subscription := range []bool{false, true} {
		t.Run(fmt.Sprint(subscription), func(t *testing.T) {
			f, run, target := codexApplyFixture(t, subscription)
			req := f.req(f.opOf(t, run))
			ready, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), run, CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: req, TargetSecretID: target})
			if err != nil || ready.Outcome != "ready" {
				t.Fatal("preflight not ready", ready, err)
			}
			if subscription {
				f.env.exec("UPDATE codex_provider_account SET credential_revision=credential_revision+1 WHERE id=(SELECT provider_account_id FROM codex_credential_state WHERE user_secret_id=$1)", target)
			} else {
				f.env.exec("UPDATE codex_credential_state SET material_revision=material_revision+1 WHERE user_secret_id=$1", target)
			}
			moved, err := f.svc.PreflightCodexCredentialSwitch(f.env.ctx, f.worker(t), run, CodexSwitchPreflightRequest{CredentialSwitchApplyRequest: req, TargetSecretID: target})
			if err != nil || moved.Outcome != ApplyOutcomeNotApplied || moved.Reason != "target_moved" {
				t.Fatal("preflight accepted moved target", moved, err)
			}
			f.noWrite(t, run, func() {
				got := f.apply(t, run, req)
				if got.Outcome != ApplyOutcomeNotApplied || got.Reason != "target_moved" {
					t.Fatalf("moved target accepted: %v", got)
				}
			})
		})
	}
}
