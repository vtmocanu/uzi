package workersvc

import (
	"errors"
	"testing"

	"github.com/vtmocanu/uzi/api/internal/capability"
)

func TestCodexRegisteredSwitchRefusesLegacyReleaseLiveDB(t *testing.T) {
	f := newApplyFx(t)
	nonce := f.register(t, []string{capability.CodexCredentialSwitchV1})
	run, _ := f.run(t, "running")
	old := f.env.seedStaticAPIKey(t, f.o.userID, "negotiation-old", "old-test-key")
	target := f.env.seedStaticAPIKey(t, f.o.userID, "negotiation-target", "target-test-key")
	if err := f.svc.FreezeCodexBinding(f.env.ctx, f.o.userID, run, old, codexAuthModeAPIKey); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target); err != nil {
		t.Fatal(err)
	}
	w := f.worker(t)
	if !codexSwitchNegotiated(w) || w.CredentialSwitchApplyNonce.String != nonce {
		t.Fatal("real registration did not negotiate Codex apply")
	}
	for _, state := range []string{"credential_switch", "credential_switch_held"} {
		f.noWrite(t, run, func() {
			_, applied, err := f.report(t, f.worker(t), run, state, f.gen, nonce, nil)
			if applied || !errors.Is(err, ErrCredentialSwitchReleaseRetired) {
				t.Fatalf("%s allowed release/reclaim: applied=%t err=%v", state, applied, err)
			}
		})
	}
	// An older replica can rotate registration without stamping apply. It must
	// neither admit a new Codex request nor turn an existing one into v1 release.
	f.oldReplicaRegister(t, "older-api-registration")
	f.noWrite(t, run, func() {
		if _, err := f.svc.SetRunCredential(f.env.ctx, f.o.userID, run, BindModePinned, &target); err == nil {
			t.Fatal("old replica registration admitted a Codex request")
		}
		if applied, err := f.release(t, run); applied || !errors.Is(err, ErrCredentialSwitchReleaseRetired) {
			t.Fatal("version skew enabled Codex release", err)
		}
	})
}

func TestCodexNegotiationPreservesLegacyClaudeReleaseLiveDB(t *testing.T) {
	f := newApplyFx(t)
	f.register(t, []string{capability.CredentialSwitchV1, capability.CodexCredentialSwitchV1})
	// The worker supports new Codex switching but only legacy Claude switching.
	run := f.heldV1Run(t)
	if applied, err := f.release(t, run); err != nil || !applied {
		t.Fatalf("legacy Claude release changed: applied=%t err=%v", applied, err)
	}
}
